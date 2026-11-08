package provider

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
)

// zone is a hosted zone the provider manages.
type zone struct {
	// name is the zone's resource name.
	name    string
	dnsName string
	state   dnsv1beta1.HostedZone_State
}

// contains reports whether dnsName is the zone's name or below it.
func (z zone) contains(dnsName string) bool {
	return dnsName == z.dnsName || strings.HasSuffix(dnsName, "."+z.dnsName)
}

// zones returns the zones of the project that the zone id filter, the
// visibility and the domain filter select, by name.
//
// A zone is managed whatever its state: the records of a zone that is
// pending verification or suspended are kept, and answered once it is
// active. A project may hold several zones of one name, such as a public
// and a private one, or pending zones next to an active one; an active
// zone is taken over the others, and of the rest the oldest. Two active
// zones of one name are refused: the visibility or the zone id filter has
// to tell them apart.
func (p *Provider) zones(ctx context.Context) ([]zone, error) {
	all, err := p.dns.ListHostedZones(ctx, p.project)
	if err != nil {
		return nil, fmt.Errorf("listing the hosted zones of %s: %w", p.project, err)
	}
	byName := map[string]*dnsv1beta1.HostedZone{}
	for _, hz := range all {
		if !p.selects(hz) {
			continue
		}
		dnsName := normalizeName(hz.GetDnsName())
		taken, ok := byName[dnsName]
		switch {
		case !ok:
			byName[dnsName] = hz
		case isActive(taken) && isActive(hz):
			return nil, fmt.Errorf("%s and %s are both active zones of %s: set ORIGINCLOUD_ZONE_VISIBILITY or ORIGINCLOUD_ZONE_IDS to choose one",
				taken.GetName(), hz.GetName(), dnsName)
		case isActive(hz):
			byName[dnsName] = hz
		}
	}
	zones := make([]zone, 0, len(byName))
	for dnsName, hz := range byName {
		zones = append(zones, zone{name: hz.GetName(), dnsName: dnsName, state: hz.GetState()})
	}
	slices.SortFunc(zones, func(a, b zone) int { return strings.Compare(a.dnsName, b.dnsName) })
	p.ready.Store(true)
	return zones, nil
}

func isActive(hz *dnsv1beta1.HostedZone) bool {
	return hz.GetState() == dnsv1beta1.HostedZone_ACTIVE
}

// selects reports whether the filters select the zone. The domain filter
// selects a zone it matches, or one that holds a domain it matches, as
// sub.example.com is held by example.com.
func (p *Provider) selects(hz *dnsv1beta1.HostedZone) bool {
	if len(p.zoneIDs) > 0 && !slices.Contains(p.zoneIDs, path.Base(hz.GetName())) {
		return false
	}
	if p.visibility != dnsv1beta1.HostedZone_VISIBILITY_UNSPECIFIED && hz.GetVisibility() != p.visibility {
		return false
	}
	dnsName := normalizeName(hz.GetDnsName())
	return p.domainFilter.Match(dnsName) || p.domainFilter.MatchParent(dnsName)
}

// zoneFor returns the zone that holds dnsName: of the zones whose name it
// is or is below, the one with the longest name. false when none holds it.
func zoneFor(zones []zone, dnsName string) (zone, bool) {
	var found zone
	ok := false
	for _, z := range zones {
		if z.contains(dnsName) && len(z.dnsName) > len(found.dnsName) {
			found, ok = z, true
		}
	}
	return found, ok
}
