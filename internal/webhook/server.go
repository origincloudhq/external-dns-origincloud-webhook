// Package webhook serves external-dns's webhook provider API for a
// provider, and the health endpoints the kubelet probes.
package webhook

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// MediaType is the webhook API's media type and version, which every
// answer is sent as.
const MediaType = "application/external.dns.webhook+json;version=1"

// maxBodySize caps a request's body, as external-dns caps its own by
// default.
const maxBodySize = 32 << 20

// Handler serves the webhook API:
//   - GET / negotiates, answering with the provider's domain filter;
//   - GET /records answers with the provider's records;
//   - POST /records applies a plan's changes;
//   - POST /adjustendpoints adjusts the endpoints external-dns wants.
//
// A call the provider fails is answered 500, which external-dns retries
// and counts as a soft error. A 4xx would stop external-dns, so only a
// request it can't decode is answered with one.
func Handler(p provider.Provider, log *slog.Logger) http.Handler {
	h := &handler{provider: p, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.negotiate)
	mux.HandleFunc("GET /records", h.records)
	mux.HandleFunc("POST /records", h.applyChanges)
	mux.HandleFunc("POST /adjustendpoints", h.adjustEndpoints)
	return mux
}

type handler struct {
	provider provider.Provider
	log      *slog.Logger
}

func (h *handler) negotiate(w http.ResponseWriter, _ *http.Request) {
	h.reply(w, http.StatusOK, h.provider.GetDomainFilter())
}

func (h *handler) records(w http.ResponseWriter, r *http.Request) {
	records, err := h.provider.Records(r.Context())
	if err != nil {
		h.fail(w, "reading the records", err)
		return
	}
	if records == nil {
		records = []*endpoint.Endpoint{}
	}
	h.reply(w, http.StatusOK, records)
}

func (h *handler) applyChanges(w http.ResponseWriter, r *http.Request) {
	var changes plan.Changes
	if !h.decode(w, r, &changes) {
		return
	}
	h.log.Debug("applying changes", "create", len(changes.Create), "updateNew", len(changes.UpdateNew), "delete", len(changes.Delete))
	if err := h.provider.ApplyChanges(r.Context(), &changes); err != nil {
		h.fail(w, "applying the changes", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) adjustEndpoints(w http.ResponseWriter, r *http.Request) {
	var endpoints []*endpoint.Endpoint
	if !h.decode(w, r, &endpoints) {
		return
	}
	adjusted, err := h.provider.AdjustEndpoints(endpoints)
	if err != nil {
		h.fail(w, "adjusting the endpoints", err)
		return
	}
	if adjusted == nil {
		adjusted = []*endpoint.Endpoint{}
	}
	h.reply(w, http.StatusOK, adjusted)
}

func (h *handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize)).Decode(v)
	if err == nil {
		return true
	}
	status := http.StatusBadRequest
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		status = http.StatusRequestEntityTooLarge
	}
	h.log.Error("decoding the request", "path", r.URL.Path, "error", err)
	http.Error(w, "decoding the request: "+err.Error(), status)
	return false
}

func (h *handler) reply(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.fail(w, "encoding the answer", err)
		return
	}
	w.Header().Set("Content-Type", MediaType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) fail(w http.ResponseWriter, doing string, err error) {
	h.log.Error(doing, "error", err)
	http.Error(w, doing+": "+err.Error(), http.StatusInternalServerError)
}

// HealthHandler serves /healthz, answered while the process serves, and
// /readyz, answered once ready reports true and 503 until then.
func HealthHandler(ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready: the hosted zones haven't been listed yet", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
