// Package fakedns is an in-memory DNS API for tests: hosted zones and
// their record sets, with batch changes checked as the API checks them.
package fakedns

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// DNS is the fake API. Its zero value is not usable; call New.
type DNS struct {
	mu      sync.Mutex
	zones   []*dnsv1beta1.HostedZone
	records map[string][]*dnsv1beta1.RecordSet // by zone name
	nextID  int

	// Batches holds every batch change asked for, accepted or not.
	Batches []*dnsv1beta1.BatchChangeRecordSetsRequest
	// FailBatch, when set, fails every batch change with it.
	FailBatch error
}

// New returns an empty fake.
func New() *DNS {
	return &DNS{records: map[string][]*dnsv1beta1.RecordSet{}}
}

// AddZone adds a hosted zone and returns its name.
func (d *DNS) AddZone(project, id, dnsName string, visibility dnsv1beta1.HostedZone_Visibility, state dnsv1beta1.HostedZone_State) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := project + "/hostedZones/" + id
	d.zones = append(d.zones, &dnsv1beta1.HostedZone{Name: name, DnsName: dnsName, Visibility: visibility, State: state})
	return name
}

// AddRecordSet adds a record set to a zone as it is, without checks, and
// returns its name.
func (d *DNS) AddRecordSet(zone string, rs *dnsv1beta1.RecordSet) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	rs = proto.Clone(rs).(*dnsv1beta1.RecordSet)
	rs.Name = d.newName(zone)
	d.records[zone] = append(d.records[zone], rs)
	return rs.Name
}

// RecordSets returns a copy of a zone's record sets, by name, then type.
func (d *DNS) RecordSets(zone string) []*dnsv1beta1.RecordSet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneSorted(d.records[zone])
}

func (d *DNS) newName(zone string) string {
	d.nextID++
	return fmt.Sprintf("%s/recordSets/rs-%04d", zone, d.nextID)
}

// ListHostedZones implements provider.DNS.
func (d *DNS) ListHostedZones(_ context.Context, project string) ([]*dnsv1beta1.HostedZone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*dnsv1beta1.HostedZone
	for _, z := range d.zones {
		if strings.HasPrefix(z.GetName(), project+"/") {
			out = append(out, proto.Clone(z).(*dnsv1beta1.HostedZone))
		}
	}
	return out, nil
}

// ListRecordSets implements provider.DNS.
func (d *DNS) ListRecordSets(_ context.Context, zone string) ([]*dnsv1beta1.RecordSet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.zone(zone) == nil {
		return nil, fmt.Errorf("NOT_FOUND: zone %s", zone)
	}
	return cloneSorted(d.records[zone]), nil
}

func (d *DNS) zone(name string) *dnsv1beta1.HostedZone {
	for _, z := range d.zones {
		if z.GetName() == name {
			return z
		}
	}
	return nil
}

// BatchChangeRecordSets implements provider.DNS, refusing a batch as the
// API does: a change it would refuse on its own, a record set named twice,
// or a zone the changes would leave with two record sets of one name and
// type, or a CNAME that isn't alone at its name.
func (d *DNS) BatchChangeRecordSets(_ context.Context, req *dnsv1beta1.BatchChangeRecordSetsRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Batches = append(d.Batches, proto.Clone(req).(*dnsv1beta1.BatchChangeRecordSetsRequest))
	if d.FailBatch != nil {
		return d.FailBatch
	}
	z := d.zone(req.GetParent())
	if z == nil {
		return fmt.Errorf("NOT_FOUND: zone %s", req.GetParent())
	}
	n := len(req.GetDeletions()) + len(req.GetUpdates()) + len(req.GetCreations())
	if n < 1 || n > 1000 {
		return fmt.Errorf("BAD_REQUEST: a batch makes 1 to 1000 changes, got %d", n)
	}

	records := cloneSorted(d.records[req.GetParent()])
	index := func(name string) int {
		return slices.IndexFunc(records, func(rs *dnsv1beta1.RecordSet) bool { return rs.GetName() == name })
	}
	named := map[string]bool{}
	for _, name := range req.GetDeletions() {
		if named[name] {
			return fmt.Errorf("BAD_REQUEST: %s is named twice", name)
		}
		named[name] = true
		i := index(name)
		if i < 0 {
			return fmt.Errorf("NOT_FOUND: deletion %s", name)
		}
		records = slices.Delete(records, i, i+1)
	}
	for _, u := range req.GetUpdates() {
		name := u.GetRecordSet().GetName()
		if named[name] {
			return fmt.Errorf("BAD_REQUEST: %s is named twice", name)
		}
		named[name] = true
		i := index(name)
		if i < 0 {
			return fmt.Errorf("NOT_FOUND: update %s", name)
		}
		if t := u.GetRecordSet().GetType(); t != dnsv1beta1.RecordSet_TYPE_UNSPECIFIED && t != records[i].GetType() {
			return fmt.Errorf("BAD_REQUEST: update %s: the type is immutable", name)
		}
		rs := records[i]
		for _, path := range u.GetUpdateMask().GetPaths() {
			switch path {
			case "ttl":
				rs.Ttl = u.GetRecordSet().GetTtl()
			case "basic":
				rs.Data = &dnsv1beta1.RecordSet_Basic{Basic: u.GetRecordSet().GetBasic()}
			default:
				return fmt.Errorf("BAD_REQUEST: update %s: unknown field %q", name, path)
			}
		}
		if err := check(z, rs); err != nil {
			return fmt.Errorf("BAD_REQUEST: update %s: %w", name, err)
		}
	}
	for i, c := range req.GetCreations() {
		rs := proto.Clone(c).(*dnsv1beta1.RecordSet)
		if err := check(z, rs); err != nil {
			return fmt.Errorf("BAD_REQUEST: creation %d: %w", i, err)
		}
		rs.Name = d.newName(req.GetParent())
		records = append(records, rs)
	}

	seen := map[string]bool{}
	for _, rs := range records {
		key := rs.GetDnsName() + " " + rs.GetType().String()
		if seen[key] {
			return fmt.Errorf("CONFLICT: two record sets of %s", key)
		}
		seen[key] = true
	}
	for _, rs := range records {
		if rs.GetType() != dnsv1beta1.RecordSet_CNAME {
			continue
		}
		for _, other := range records {
			if other != rs && other.GetDnsName() == rs.GetDnsName() {
				return fmt.Errorf("CONFLICT: the CNAME %s isn't alone at its name", rs.GetDnsName())
			}
		}
	}

	for _, rs := range records {
		if rs.Ttl == nil {
			rs.Ttl = durationpb.New(300 * time.Second)
		}
	}
	d.records[req.GetParent()] = records
	return nil
}

// check refuses a record set as the API would on its own.
func check(z *dnsv1beta1.HostedZone, rs *dnsv1beta1.RecordSet) error {
	name := rs.GetDnsName()
	if name != strings.ToLower(name) || strings.HasSuffix(name, ".") {
		return fmt.Errorf("the name %q isn't written as the API answers it", name)
	}
	if name != z.GetDnsName() && !strings.HasSuffix(name, "."+z.GetDnsName()) {
		return fmt.Errorf("the name %s is outside the zone %s", name, z.GetDnsName())
	}
	if rs.GetType() == dnsv1beta1.RecordSet_TYPE_UNSPECIFIED {
		return fmt.Errorf("%s has no type", name)
	}
	if ttl := rs.GetTtl(); ttl != nil && (ttl.AsDuration() < 30*time.Second || ttl.AsDuration() > 7*24*time.Hour) {
		return fmt.Errorf("the TTL %s is out of range", ttl.AsDuration())
	}
	values := rs.GetBasic().GetValues()
	if len(values) < 1 || len(values) > 100 {
		return fmt.Errorf("%s has %d values", name, len(values))
	}
	for i, v := range values {
		if slices.Contains(values[:i], v) {
			return fmt.Errorf("%s holds %q twice", name, v)
		}
		if rs.GetType() == dnsv1beta1.RecordSet_TXT {
			if len(v) > 4000 {
				return fmt.Errorf("a TXT value of %d characters", len(v))
			}
			for _, c := range v {
				if c < ' ' || c > '~' {
					return fmt.Errorf("a TXT value holds %q", c)
				}
			}
		}
	}
	if rs.GetType() == dnsv1beta1.RecordSet_CNAME && len(values) != 1 {
		return fmt.Errorf("the CNAME %s holds %d names", name, len(values))
	}
	return nil
}

func cloneSorted(records []*dnsv1beta1.RecordSet) []*dnsv1beta1.RecordSet {
	out := make([]*dnsv1beta1.RecordSet, 0, len(records))
	for _, rs := range records {
		out = append(out, proto.Clone(rs).(*dnsv1beta1.RecordSet))
	}
	slices.SortFunc(out, func(a, b *dnsv1beta1.RecordSet) int {
		if c := strings.Compare(a.GetDnsName(), b.GetDnsName()); c != 0 {
			return c
		}
		return int(a.GetType() - b.GetType())
	})
	return out
}
