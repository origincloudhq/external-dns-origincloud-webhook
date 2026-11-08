package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// maxBatchChanges is the most changes one batch change makes.
const maxBatchChanges = 1000

// zonePlan is what a plan asks of one zone, by record set: the record sets
// to delete, and those to hold the values given, as updates (the plan
// knows them) or creations (it doesn't).
type zonePlan struct {
	zone      zone
	deletions map[recordKey]bool
	updates   map[recordKey]*recordSet
	creations map[recordKey]*recordSet
}

func (zp *zonePlan) empty() bool {
	return len(zp.deletions) == 0 && len(zp.updates) == 0 && len(zp.creations) == 0
}

// planByZone sorts the plan's changes into the zones that hold their
// names, in the zones' order, merging endpoints of one name and type into
// one record set. UpdateOld is not needed: an update replaces the record
// set's TTL and values with UpdateNew's. A change the API can't make, of a
// name the domain filter doesn't match, or of a name no zone holds, is
// skipped with a warning.
func (p *Provider) planByZone(zones []zone, changes *plan.Changes) []*zonePlan {
	plans := map[string]*zonePlan{}
	add := func(ep *endpoint.Endpoint, kind string) {
		if reason := p.unsupported(ep); reason != "" {
			p.log.Warn("skipping change", "change", kind, "dnsName", ep.DNSName, "type", ep.RecordType, "reason", reason)
			return
		}
		dnsName := normalizeName(ep.DNSName)
		if !p.domainFilter.Match(dnsName) {
			p.log.Warn("skipping change", "change", kind, "dnsName", dnsName, "type", ep.RecordType, "reason", "the domain filter doesn't match the name")
			return
		}
		z, ok := zoneFor(zones, dnsName)
		if !ok {
			p.log.Warn("skipping change", "change", kind, "dnsName", dnsName, "type", ep.RecordType, "reason", "no zone managed holds the name")
			return
		}
		zp := plans[z.name]
		if zp == nil {
			zp = &zonePlan{
				zone:      z,
				deletions: map[recordKey]bool{},
				updates:   map[recordKey]*recordSet{},
				creations: map[recordKey]*recordSet{},
			}
			plans[z.name] = zp
		}
		key := recordKey{dnsName: dnsName, recordType: ep.RecordType}
		var sets map[recordKey]*recordSet
		switch kind {
		case "delete":
			zp.deletions[key] = true
			return
		case "update":
			sets = zp.updates
		default:
			sets = zp.creations
		}
		s := sets[key]
		if s == nil {
			s = &recordSet{key: key}
			sets[key] = s
		}
		s.merge(ep)
	}
	for _, ep := range changes.Delete {
		add(ep, "delete")
	}
	for _, ep := range changes.UpdateNew {
		add(ep, "update")
	}
	for _, ep := range changes.Create {
		add(ep, "create")
	}
	out := make([]*zonePlan, 0, len(plans))
	for _, z := range zones {
		if zp := plans[z.name]; zp != nil && !zp.empty() {
			out = append(out, zp)
		}
	}
	return out
}

// change is one change of a batch, with the name it is about, so that the
// changes of one name stay in one batch.
type change struct {
	dnsName    string
	recordType string
	deletion   string
	update     *dnsv1beta1.RecordSetUpdate
	creation   *dnsv1beta1.RecordSet
}

func (c change) String() string {
	switch {
	case c.deletion != "":
		return fmt.Sprintf("delete %s %s (%s)", c.dnsName, c.recordType, c.deletion)
	case c.update != nil:
		rs := c.update.GetRecordSet()
		return fmt.Sprintf("update %s %s ttl=%s %q (%s)", c.dnsName, c.recordType, ttlString(rs), rs.GetBasic().GetValues(), rs.GetName())
	default:
		rs := c.creation
		return fmt.Sprintf("create %s %s ttl=%s %q", c.dnsName, c.recordType, ttlString(rs), rs.GetBasic().GetValues())
	}
}

func ttlString(rs *dnsv1beta1.RecordSet) string {
	if rs.GetTtl() == nil {
		return "default"
	}
	return rs.GetTtl().AsDuration().String()
}

// zoneChanges turns one zone's plan into the changes to make, against the
// zone's record sets as they are now:
//   - A deletion of a record set that is gone already is done.
//   - A record set both deleted and written in the plan, such as one whose
//     owner changes, is updated in place.
//   - An update of a record set that is gone creates it again.
//   - A creation of a record set that exists is skipped with a warning: the
//     plan didn't see it, so it isn't external-dns's to replace.
//   - An alias is never changed or deleted.
func (p *Provider) zoneChanges(ctx context.Context, zp *zonePlan) ([]change, error) {
	records, err := p.dns.ListRecordSets(ctx, zp.zone.name)
	if err != nil {
		return nil, fmt.Errorf("listing the record sets: %w", err)
	}
	existing := map[recordKey]*dnsv1beta1.RecordSet{}
	for _, rs := range records {
		existing[recordKey{dnsName: normalizeName(rs.GetDnsName()), recordType: rs.GetType().String()}] = rs
	}

	var changes []change
	for _, key := range sortedKeys(zp.deletions) {
		if zp.updates[key] != nil || zp.creations[key] != nil {
			continue
		}
		rs := existing[key]
		switch {
		case rs == nil:
			p.log.Debug("record set to delete is gone already", "zone", zp.zone.name, "recordSet", key.String())
		case rs.GetAlias() != nil:
			p.log.Warn("not deleting an alias", "zone", zp.zone.name, "recordSet", rs.GetName(), "dnsName", key.dnsName)
		default:
			changes = append(changes, change{dnsName: key.dnsName, recordType: key.recordType, deletion: rs.GetName()})
		}
	}
	writes := map[recordKey]*recordSet{}
	for key, s := range zp.creations {
		writes[key] = s
	}
	for key, s := range zp.updates {
		writes[key] = s
	}
	for _, key := range sortedKeys(writes) {
		s := writes[key]
		known := zp.updates[key] != nil || zp.deletions[key]
		rs := existing[key]
		switch {
		case rs == nil:
			changes = append(changes, change{dnsName: key.dnsName, recordType: key.recordType, creation: s.apiRecordSet("")})
		case rs.GetAlias() != nil:
			p.log.Warn("not replacing an alias", "zone", zp.zone.name, "recordSet", rs.GetName(), "dnsName", key.dnsName, "type", key.recordType)
		case !known:
			p.log.Warn("not creating a record set that exists already", "zone", zp.zone.name, "recordSet", rs.GetName(), "dnsName", key.dnsName, "type", key.recordType)
		default:
			changes = append(changes, change{dnsName: key.dnsName, recordType: key.recordType, update: &dnsv1beta1.RecordSetUpdate{
				RecordSet: s.apiRecordSet(rs.GetName()),
				// Naming ttl without one returns it to the default.
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl", "basic"}},
			}})
		}
	}
	return changes, nil
}

// applyBatches makes one zone's changes, in as few batch changes as hold
// them.
func (p *Provider) applyBatches(ctx context.Context, zp *zonePlan, changes []change) error {
	for _, batch := range batches(changes, maxBatchChanges) {
		req := &dnsv1beta1.BatchChangeRecordSetsRequest{Parent: zp.zone.name}
		for _, c := range batch {
			switch {
			case c.deletion != "":
				req.Deletions = append(req.Deletions, c.deletion)
			case c.update != nil:
				req.Updates = append(req.Updates, c.update)
			default:
				req.Creations = append(req.Creations, c.creation)
			}
		}
		if p.dryRun {
			for _, c := range batch {
				p.log.Info("dry run: not changing", "zone", zp.zone.name, "change", c.String())
			}
			continue
		}
		if err := p.dns.BatchChangeRecordSets(ctx, req); err != nil {
			return err
		}
		for _, c := range batch {
			p.log.Info("changed", "zone", zp.zone.name, "change", c.String())
		}
	}
	return nil
}

// batches splits changes into batches of at most size changes, keeping
// the changes of one name together, as a CNAME that takes an A record
// set's place has to be in one batch with the A record set's deletion. A
// name with more changes than size, which external-dns never asks for,
// gets batches of its own.
func batches(changes []change, size int) [][]change {
	slices.SortStableFunc(changes, func(a, b change) int { return strings.Compare(a.dnsName, b.dnsName) })
	var out [][]change
	var current []change
	for i := 0; i < len(changes); {
		j := i
		for j < len(changes) && changes[j].dnsName == changes[i].dnsName {
			j++
		}
		group := changes[i:j]
		if len(current) > 0 && len(current)+len(group) > size {
			out = append(out, current)
			current = nil
		}
		for len(group) > size {
			out = append(out, group[:size])
			group = group[size:]
		}
		current = append(current, group...)
		i = j
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

func sortedKeys[V any](m map[recordKey]V) []recordKey {
	keys := make([]recordKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b recordKey) int {
		if c := strings.Compare(a.dnsName, b.dnsName); c != 0 {
			return c
		}
		return strings.Compare(a.recordType, b.recordType)
	})
	return keys
}
