package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// stub is a provider whose answers the test sets.
type stub struct {
	provider.BaseProvider
	records []*endpoint.Endpoint
	err     error
	applied *plan.Changes
}

func (s *stub) Records(context.Context) ([]*endpoint.Endpoint, error) { return s.records, s.err }

func (s *stub) ApplyChanges(_ context.Context, changes *plan.Changes) error {
	s.applied = changes
	return s.err
}

func (s *stub) GetDomainFilter() endpoint.DomainFilterInterface {
	return endpoint.NewDomainFilter([]string{"example.com"})
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Accept", MediaType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestNegotiate(t *testing.T) {
	w := call(t, Handler(&stub{}, discard), http.MethodGet, "/", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != MediaType {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var filter endpoint.DomainFilter
	if err := json.Unmarshal(w.Body.Bytes(), &filter); err != nil {
		t.Fatal(err)
	}
	if !filter.Match("www.example.com") || filter.Match("www.example.org") {
		t.Errorf("negotiated filter %s", w.Body.String())
	}
}

func TestRecordsAndChanges(t *testing.T) {
	s := &stub{records: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", 60, "192.0.2.1")}}
	h := Handler(s, discard)

	w := call(t, h, http.MethodGet, "/records", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != MediaType {
		t.Fatalf("GET /records: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if got := strings.TrimSpace(w.Body.String()); got != `[{"dnsName":"www.example.com","targets":["192.0.2.1"],"recordType":"A","recordTTL":60}]` {
		t.Errorf("GET /records: %s", got)
	}

	w = call(t, h, http.MethodPost, "/records", `{"create":[{"dnsName":"a.example.com","targets":["192.0.2.2"],"recordType":"A"}]}`)
	if w.Code != http.StatusNoContent || s.applied == nil || len(s.applied.Create) != 1 || s.applied.Create[0].DNSName != "a.example.com" {
		t.Errorf("POST /records: %d %+v", w.Code, s.applied)
	}

	w = call(t, h, http.MethodPost, "/adjustendpoints", `[{"dnsName":"a.example.com","targets":["192.0.2.2"],"recordType":"A"}]`)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != MediaType || !strings.Contains(w.Body.String(), "a.example.com") {
		t.Errorf("POST /adjustendpoints: %d %s", w.Code, w.Body.String())
	}
}

func TestErrors(t *testing.T) {
	s := &stub{err: errors.New("origincloud: SERVICE_UNAVAILABLE")}
	h := Handler(s, discard)
	// A provider's failure is a 500, which external-dns retries; a 4xx
	// would stop it.
	if w := call(t, h, http.MethodGet, "/records", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("GET /records: %d", w.Code)
	}
	if w := call(t, h, http.MethodPost, "/records", `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("POST /records: %d", w.Code)
	}
	if w := call(t, h, http.MethodPost, "/records", `{not json`); w.Code != http.StatusBadRequest {
		t.Errorf("POST /records, not JSON: %d", w.Code)
	}
	if w := call(t, h, http.MethodDelete, "/records", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /records: %d", w.Code)
	}
	if w := call(t, h, http.MethodGet, "/nothing", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET /nothing: %d", w.Code)
	}
}

func TestHealth(t *testing.T) {
	ready := false
	h := HealthHandler(func() bool { return ready })
	if w := call(t, h, http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("/healthz: %d", w.Code)
	}
	if w := call(t, h, http.MethodGet, "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before ready: %d", w.Code)
	}
	ready = true
	if w := call(t, h, http.MethodGet, "/readyz", ""); w.Code != http.StatusOK {
		t.Errorf("/readyz once ready: %d", w.Code)
	}
}
