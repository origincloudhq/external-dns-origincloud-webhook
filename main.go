// Command external-dns-origincloud-webhook is an external-dns webhook
// provider for Origin Cloud DNS. It runs next to external-dns, usually as
// a sidecar in its pod, and is configured from the environment; see the
// README.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.origincloud.eu/sdk/core"
	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/config"
	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/provider"
	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/webhook"
)

// version is set at build time, -X main.version=1.2.3.
var version = "dev"

const (
	// shutdownTimeout bounds how long a call under way may take to finish
	// once the process is asked to stop: less than the 30 seconds
	// Kubernetes waits by default.
	shutdownTimeout = 20 * time.Second

	// checkInterval is the longest wait between attempts to list the
	// zones at startup, until the first succeeds.
	checkInterval = 30 * time.Second
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := core.NewClient(ctx, core.WithUserAgent("external-dns-origincloud-webhook/"+version))
	if err != nil {
		return err
	}
	p := provider.New(provider.NewDNS(client), provider.Options{
		Project:      cfg.Project,
		ZoneIDs:      cfg.ZoneIDs,
		Visibility:   visibility(cfg.Visibility),
		DomainFilter: domainFilter(cfg),
		DryRun:       cfg.DryRun,
	}, log)
	log.Info("starting", "version", version, "project", cfg.Project, "dryRun", cfg.DryRun,
		"webhookAddress", cfg.WebhookAddress, "healthAddress", cfg.HealthAddress)

	go checkUntilReady(ctx, p, log)

	webhookServer := newServer(webhook.Handler(p, log), log)
	healthServer := newServer(webhook.HealthHandler(p.Ready), log)
	errs := make(chan error, 2)
	serve := func(s *http.Server, address string) {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			errs <- err
			return
		}
		if err := s.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}
	go serve(webhookServer, cfg.WebhookAddress)
	go serve(healthServer, cfg.HealthAddress)

	select {
	case err := <-errs:
		stop()
		shutdown(log, webhookServer, healthServer)
		return err
	case <-ctx.Done():
		log.Info("stopping: finishing the calls under way")
		shutdown(log, webhookServer, healthServer)
		return nil
	}
}

// checkUntilReady lists the zones until it succeeds, so that a wrong
// project or credentials show in the log at once, and the zones managed
// are logged.
func checkUntilReady(ctx context.Context, p *provider.Provider, log *slog.Logger) {
	wait := time.Second
	for {
		err := p.Check(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Error("listing the hosted zones", "error", err, "retryIn", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, checkInterval)
	}
}

func newServer(handler http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// shutdown stops the servers taking new calls, and waits for the calls
// under way, until shutdownTimeout.
func shutdown(log *slog.Logger, servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			log.Warn("stopping the server", "error", err)
		}
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func visibility(v config.Visibility) dnsv1beta1.HostedZone_Visibility {
	switch v {
	case config.VisibilityPublic:
		return dnsv1beta1.HostedZone_PUBLIC
	case config.VisibilityPrivate:
		return dnsv1beta1.HostedZone_PRIVATE
	}
	return dnsv1beta1.HostedZone_VISIBILITY_UNSPECIFIED
}

func domainFilter(cfg config.Config) *endpoint.DomainFilter {
	if cfg.RegexDomainFilter != nil {
		return endpoint.NewRegexDomainFilter(cfg.RegexDomainFilter, cfg.RegexDomainExclusion)
	}
	return endpoint.NewDomainFilterWithExclusions(cfg.DomainFilter, cfg.ExcludeDomains)
}
