// Package provider is an external-dns provider for Origin Cloud DNS: it
// reads the record sets of a project's hosted zones as external-dns's
// endpoints, and makes external-dns's changes with one batch change per
// zone.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

// Options select the zones and names the provider manages.
type Options struct {
	// Project is the project whose zones are managed, projects/{project}.
	Project string
	// ZoneIDs, when not empty, are the only zones managed, by id.
	ZoneIDs []string
	// Visibility, when set, limits the zones managed to public or to
	// private ones.
	Visibility dnsv1beta1.HostedZone_Visibility
	// DomainFilter selects the names managed, and the zones holding them.
	// nil manages every name.
	DomainFilter *endpoint.DomainFilter
	// DryRun logs the changes instead of making them.
	DryRun bool
}

// Provider implements external-dns's provider.Provider over the DNS API.
type Provider struct {
	dns          DNS
	project      string
	zoneIDs      []string
	visibility   dnsv1beta1.HostedZone_Visibility
	domainFilter *endpoint.DomainFilter
	dryRun       bool
	log          *slog.Logger

	ready atomic.Bool
}

var _ provider.Provider = (*Provider)(nil)

// New returns a provider for the zones opts select.
func New(dns DNS, opts Options, log *slog.Logger) *Provider {
	filter := opts.DomainFilter
	if filter == nil {
		filter = endpoint.NewDomainFilter(nil)
	}
	return &Provider{
		dns:          dns,
		project:      opts.Project,
		zoneIDs:      slices.Clone(opts.ZoneIDs),
		visibility:   opts.Visibility,
		domainFilter: filter,
		dryRun:       opts.DryRun,
		log:          log,
	}
}

// Ready reports whether the provider has listed the project's zones at
// least once, which proves its credentials and project.
func (p *Provider) Ready() bool { return p.ready.Load() }

// Check lists the zones the provider manages, and logs them.
func (p *Provider) Check(ctx context.Context) error {
	zones, err := p.zones(ctx)
	if err != nil {
		return err
	}
	for _, z := range zones {
		p.log.Info("managing zone", "zone", z.name, "dnsName", z.dnsName, "state", z.state.String())
	}
	if len(zones) == 0 {
		p.log.Warn("no zone of the project matches the filters", "project", p.project)
	}
	return nil
}

// GetDomainFilter returns the domains the provider manages, which the
// webhook hands external-dns when it negotiates.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// Records returns every record set of the zones managed, as endpoints, that
// the domain filter matches. Aliases aren't returned: their address is the
// platform's to follow, and the provider never changes them.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	zones, err := p.zones(ctx)
	if err != nil {
		return nil, err
	}
	var endpoints []*endpoint.Endpoint
	for _, z := range zones {
		records, err := p.dns.ListRecordSets(ctx, z.name)
		if err != nil {
			return nil, fmt.Errorf("listing the record sets of %s: %w", z.name, err)
		}
		for _, rs := range records {
			ep, ok := endpointOf(rs)
			if !ok {
				p.log.Debug("skipping record set", "recordSet", rs.GetName(), "dnsName", rs.GetDnsName(), "type", rs.GetType().String(), "alias", rs.GetAlias() != nil)
				continue
			}
			if !p.domainFilter.Match(ep.DNSName) {
				continue
			}
			endpoints = append(endpoints, ep)
		}
	}
	return endpoints, nil
}

// AdjustEndpoints writes the endpoints external-dns wants as Records
// returns them, so that the plan compares like with like: names and
// targets normalized, TXT values unquoted, TTLs within the range a record
// set takes. Endpoints the API can't hold are dropped with a warning:
// those of a record type it doesn't have, and those with a set identifier,
// as record sets have no routing policies.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	adjusted := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if reason := p.unsupported(ep); reason != "" {
			p.log.Warn("ignoring endpoint", "dnsName", ep.DNSName, "type", ep.RecordType, "reason", reason)
			continue
		}
		ep.DNSName = normalizeName(ep.DNSName)
		ep.Targets = normalizeTargets(ep.RecordType, ep.Targets)
		if ep.RecordTTL.IsConfigured() {
			ep.RecordTTL = clampTTL(ep.RecordTTL)
		}
		adjusted = append(adjusted, ep)
	}
	return adjusted, nil
}

// unsupported says why the API can't hold ep, or "" when it can.
func (p *Provider) unsupported(ep *endpoint.Endpoint) string {
	if _, ok := recordTypes[ep.RecordType]; !ok {
		return "Origin Cloud DNS has no record sets of this type"
	}
	if ep.SetIdentifier != "" {
		return "Origin Cloud DNS has no routing policies: a set identifier is not supported"
	}
	return ""
}

// ApplyChanges makes the plan's changes, one batch change per zone, all or
// none within a zone. A zone whose batch fails doesn't stop the others; the
// error names every zone that failed.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	zones, err := p.zones(ctx)
	if err != nil {
		return err
	}
	type zoneWork struct {
		plan    *zonePlan
		changes []change
	}
	var errs []error
	zoneFailed := func(zp *zonePlan, err error) {
		errs = append(errs, fmt.Errorf("zone %s (%s): %w", zp.zone.dnsName, zp.zone.name, err))
	}
	// Every zone's record sets are read before any zone is changed, so that
	// no read has to wait for a write of the same call to be readable.
	var work []zoneWork
	for _, zp := range p.planByZone(zones, changes) {
		zoneChanges, err := p.zoneChanges(ctx, zp)
		if err != nil {
			zoneFailed(zp, err)
			continue
		}
		work = append(work, zoneWork{plan: zp, changes: zoneChanges})
	}
	for _, w := range work {
		if err := p.applyBatches(ctx, w.plan, w.changes); err != nil {
			zoneFailed(w.plan, err)
		}
	}
	return errors.Join(errs...)
}
