package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/fakedns"
)

const project = "projects/p-1"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func basic(dnsName string, t dnsv1beta1.RecordSet_Type, ttl int64, values ...string) *dnsv1beta1.RecordSet {
	rs := &dnsv1beta1.RecordSet{
		DnsName: dnsName,
		Type:    t,
		Data:    &dnsv1beta1.RecordSet_Basic{Basic: &dnsv1beta1.BasicRecordData{Values: values}},
	}
	if ttl > 0 {
		rs.Ttl = durationpb.New(time.Duration(ttl) * time.Second)
	}
	return rs
}

func alias(dnsName, floatingIP string) *dnsv1beta1.RecordSet {
	return &dnsv1beta1.RecordSet{
		DnsName: dnsName,
		Type:    dnsv1beta1.RecordSet_A,
		Data:    &dnsv1beta1.RecordSet_Alias{Alias: &dnsv1beta1.AliasRecordData{FloatingIp: floatingIP}},
	}
}

func publicZone(d *fakedns.DNS, id, dnsName string) string {
	return d.AddZone(project, id, dnsName, dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
}

// show writes endpoints one per line, sorted, for comparison.
func show(endpoints []*endpoint.Endpoint) string {
	lines := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		targets := slices.Clone([]string(ep.Targets))
		slices.Sort(targets)
		lines = append(lines, ep.DNSName+" "+ep.RecordType+" "+strconv.FormatInt(int64(ep.RecordTTL), 10)+" "+strings.Join(targets, "|"))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// showZone writes a zone's record sets one per line.
func showZone(d *fakedns.DNS, zone string) string {
	var lines []string
	for _, rs := range d.RecordSets(zone) {
		values := slices.Clone(rs.GetBasic().GetValues())
		slices.Sort(values)
		line := rs.GetDnsName() + " " + rs.GetType().String() + " " + rs.GetTtl().AsDuration().String() + " " + strings.Join(values, "|")
		if rs.GetAlias() != nil {
			line = rs.GetDnsName() + " " + rs.GetType().String() + " alias " + rs.GetAlias().GetFloatingIp()
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func TestRecords(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	d.AddRecordSet(zone, basic("www.example.com", dnsv1beta1.RecordSet_A, 60, "192.0.2.1", "192.0.2.2"))
	d.AddRecordSet(zone, basic("example.com", dnsv1beta1.RecordSet_TXT, 300, "v=spf1 mx -all", "google-site-verification=abc"))
	d.AddRecordSet(zone, basic("example.com", dnsv1beta1.RecordSet_MX, 300, "10 mail.example.com"))
	d.AddRecordSet(zone, basic("example.com", dnsv1beta1.RecordSet_CAA, 300, "0 issue letsencrypt.org"))
	d.AddRecordSet(zone, alias("app.example.com", "projects/p-1/floatingIps/fip-1"))
	other := d.AddZone("projects/p-2", "z-2", "example.org", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
	d.AddRecordSet(other, basic("www.example.org", dnsv1beta1.RecordSet_A, 60, "192.0.2.9"))

	p := New(d, Options{Project: project}, discard)
	got, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// One endpoint per record set, every value a target; the alias, the
	// CAA record set and the other project's zone aren't there.
	want := strings.Join([]string{
		"example.com MX 300 10 mail.example.com",
		"example.com TXT 300 google-site-verification=abc|v=spf1 mx -all",
		"www.example.com A 60 192.0.2.1|192.0.2.2",
	}, "\n")
	if show(got) != want {
		t.Errorf("Records:\n%s\nwant:\n%s", show(got), want)
	}
	if !p.Ready() {
		t.Error("not ready after listing the zones")
	}
}

func TestZoneSelection(t *testing.T) {
	d := fakedns.New()
	d.AddZone(project, "z-com", "example.com", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
	d.AddZone(project, "z-org", "example.org", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
	d.AddZone(project, "z-corp", "corp.example", dnsv1beta1.HostedZone_PRIVATE, dnsv1beta1.HostedZone_ACTIVE)
	d.AddZone(project, "z-new", "new.example", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_PENDING_VERIFICATION)

	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{"every zone", Options{}, []string{"z-corp", "z-com", "z-org", "z-new"}},
		{"domain filter", Options{DomainFilter: endpoint.NewDomainFilter([]string{"example.com"})}, []string{"z-com"}},
		{"domain below a zone", Options{DomainFilter: endpoint.NewDomainFilter([]string{"k8s.example.org"})}, []string{"z-org"}},
		{"excluded domain", Options{DomainFilter: endpoint.NewDomainFilterWithExclusions(nil, []string{"example.org"})}, []string{"z-corp", "z-com", "z-new"}},
		{"zone ids", Options{ZoneIDs: []string{"z-org", "z-new"}}, []string{"z-org", "z-new"}},
		{"zone ids and domain filter", Options{ZoneIDs: []string{"z-org", "z-com"}, DomainFilter: endpoint.NewDomainFilter([]string{"example.com"})}, []string{"z-com"}},
		{"private", Options{Visibility: dnsv1beta1.HostedZone_PRIVATE}, []string{"z-corp"}},
		{"public", Options{Visibility: dnsv1beta1.HostedZone_PUBLIC}, []string{"z-com", "z-org", "z-new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Project = project
			zones, err := New(d, tt.opts, discard).zones(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, z := range zones {
				got = append(got, z.name[strings.LastIndex(z.name, "/")+1:])
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("zones = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZonesOfOneName(t *testing.T) {
	d := fakedns.New()
	d.AddZone(project, "pending-old", "example.com", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_PENDING_VERIFICATION)
	d.AddZone(project, "active", "example.com", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
	d.AddZone(project, "pending-new", "example.com", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_PENDING_VERIFICATION)
	d.AddZone(project, "pending-a", "example.net", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_PENDING_VERIFICATION)
	d.AddZone(project, "pending-b", "example.net", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_PENDING_VERIFICATION)

	zones, err := New(d, Options{Project: project}, discard).zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The active zone over the pending ones, and of pending ones the
	// oldest.
	if len(zones) != 2 || zones[0].name != project+"/hostedZones/active" || zones[1].name != project+"/hostedZones/pending-a" {
		t.Errorf("zones = %+v", zones)
	}

	// A public and a private zone of one name, both active: refused unless
	// the visibility tells them apart.
	d.AddZone(project, "private", "example.com", dnsv1beta1.HostedZone_PRIVATE, dnsv1beta1.HostedZone_ACTIVE)
	if _, err := New(d, Options{Project: project}, discard).zones(context.Background()); err == nil || !strings.Contains(err.Error(), "ORIGINCLOUD_ZONE_VISIBILITY") {
		t.Errorf("two active zones of one name: err = %v", err)
	}
	zones, err = New(d, Options{Project: project, Visibility: dnsv1beta1.HostedZone_PRIVATE}, discard).zones(context.Background())
	if err != nil || len(zones) != 1 || zones[0].name != project+"/hostedZones/private" {
		t.Errorf("private zones = %+v, %v", zones, err)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	p := New(fakedns.New(), Options{Project: project}, discard)
	in := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("WWW.Example.com.", "A", 10, "192.0.2.1", "192.0.2.1"),
		endpoint.NewEndpointWithTTL("v6.example.com", "AAAA", 999999999, "2001:DB8:0:0::10"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "Target.Example.NET."),
		endpoint.NewEndpoint("example.com", "TXT", `"v=spf1 mx -all"`, "plain text", `"split " "string"`, `"say \"hi\""`),
		endpoint.NewEndpoint("example.com", "MX", "10  Mail.Example.com."),
		endpoint.NewEndpoint("_sip._tcp.example.com", "SRV", "10 5 5060 SIP.example.com."),
		endpoint.NewEndpoint("example.com", "NS", "ns1.example.net"),
		endpoint.NewEndpoint("weighted.example.com", "A", "192.0.2.3").WithSetIdentifier("eu"),
	}
	got, err := p.AdjustEndpoints(in)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"_sip._tcp.example.com SRV 0 10 5 5060 sip.example.com",
		"alias.example.com CNAME 0 target.example.net",
		`example.com MX 0 10 mail.example.com`,
		`example.com TXT 0 plain text|say "hi"|split string|v=spf1 mx -all`,
		"v6.example.com AAAA 604800 2001:db8::10",
		"www.example.com A 30 192.0.2.1",
	}, "\n")
	if show(got) != want {
		t.Errorf("AdjustEndpoints:\n%s\nwant:\n%s", show(got), want)
	}
}

func TestTXTText(t *testing.T) {
	long := strings.Repeat("k", 300)
	tests := []struct{ in, want string }{
		{`"heritage=external-dns,external-dns/owner=a"`, "heritage=external-dns,external-dns/owner=a"},
		{"heritage=external-dns,external-dns/owner=a", "heritage=external-dns,external-dns/owner=a"},
		{`"` + long[:255] + `" "` + long[255:] + `"`, long},
		{`"unterminated`, `"unterminated`},
		{`"a"b`, `"a"b`},
		{`"a" b`, `"a" b`},
		{`""`, ""},
		{`"back\\slash"`, `back\slash`},
		{`text with "quotes" inside`, `text with "quotes" inside`},
	}
	for _, tt := range tests {
		if got := txtText(tt.in); got != tt.want {
			t.Errorf("txtText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplyChanges(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	sub := publicZone(d, "z-2", "sub.example.com")
	d.AddRecordSet(zone, basic("old.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.10"))
	d.AddRecordSet(zone, basic("moved.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.11"))
	d.AddRecordSet(zone, basic("change.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.12"))
	d.AddRecordSet(zone, basic("taken.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.13"))
	d.AddRecordSet(zone, alias("fip.example.com", "projects/p-1/floatingIps/fip-1"))
	d.AddRecordSet(sub, basic("x.sub.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.20"))

	p := New(d, Options{Project: project}, discard)
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			// Two endpoints of one name and type: one record set.
			endpoint.NewEndpointWithTTL("new.example.com", "A", 60, "192.0.2.1"),
			endpoint.NewEndpoint("new.example.com", "A", "192.0.2.2"),
			endpoint.NewEndpoint("new.example.com", "TXT", `"heritage=external-dns,external-dns/owner=a"`),
			// Deleted and created in one plan: updated in place.
			endpoint.NewEndpoint("moved.example.com", "A", "192.0.2.111"),
			// Exists, but the plan didn't know: left as it is.
			endpoint.NewEndpoint("taken.example.com", "A", "192.0.2.99"),
			// An alias is never replaced.
			endpoint.NewEndpoint("fip.example.com", "A", "192.0.2.98"),
			// The zone below holds it.
			endpoint.NewEndpoint("y.sub.example.com", "A", "192.0.2.21"),
			// No zone holds it.
			endpoint.NewEndpoint("www.example.net", "A", "192.0.2.30"),
		},
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("change.example.com", "A", "192.0.2.12")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("change.example.com", "A", 120, "192.0.2.112", "192.0.2.113")},
		Delete: []*endpoint.Endpoint{
			endpoint.NewEndpoint("old.example.com", "A", "192.0.2.10"),
			endpoint.NewEndpoint("moved.example.com", "A", "192.0.2.11"),
			endpoint.NewEndpoint("gone.example.com", "A", "192.0.2.14"),
			endpoint.NewEndpoint("x.sub.example.com", "A", "192.0.2.20"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"change.example.com A 2m0s 192.0.2.112|192.0.2.113",
		"fip.example.com A alias projects/p-1/floatingIps/fip-1",
		"moved.example.com A 5m0s 192.0.2.111",
		"new.example.com A 1m0s 192.0.2.1|192.0.2.2",
		"new.example.com TXT 5m0s heritage=external-dns,external-dns/owner=a",
		"taken.example.com A 5m0s 192.0.2.13",
	}, "\n")
	if got := showZone(d, zone); got != want {
		t.Errorf("example.com:\n%s\nwant:\n%s", got, want)
	}
	if got, want := showZone(d, sub), "y.sub.example.com A 5m0s 192.0.2.21"; got != want {
		t.Errorf("sub.example.com:\n%s\nwant:\n%s", got, want)
	}
	// One batch per zone.
	if len(d.Batches) != 2 {
		t.Fatalf("%d batches, want 2", len(d.Batches))
	}
	if b := d.Batches[0]; b.GetParent() != zone || len(b.GetDeletions()) != 1 || len(b.GetUpdates()) != 2 || len(b.GetCreations()) != 2 {
		t.Errorf("example.com's batch: %v", b)
	}
}

func TestApplyChangesCNAMEReplacesA(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	d.AddRecordSet(zone, basic("www.example.com", dnsv1beta1.RecordSet_A, 300, "192.0.2.1"))

	p := New(d, Options{Project: project}, discard)
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "CNAME", "lb.example.net")},
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := showZone(d, zone), "www.example.com CNAME 5m0s lb.example.net"; got != want {
		t.Errorf("zone:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyChangesDryRun(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	p := New(d, Options{Project: project, DryRun: true}, discard)
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Batches) != 0 || len(d.RecordSets(zone)) != 0 {
		t.Errorf("a dry run changed the zone: %d batches", len(d.Batches))
	}
}

func TestApplyChangesDomainFilter(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	p := New(d, Options{Project: project, DomainFilter: endpoint.NewDomainFilter([]string{"k8s.example.com"})}, discard)
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.k8s.example.com", "A", "192.0.2.1"),
			endpoint.NewEndpoint("www.example.com", "A", "192.0.2.2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := showZone(d, zone), "app.k8s.example.com A 5m0s 192.0.2.1"; got != want {
		t.Errorf("zone:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyChangesFailingZone(t *testing.T) {
	d := fakedns.New()
	publicZone(d, "z-1", "example.com")
	d.FailBatch = errors.New("SERVICE_UNAVAILABLE")
	p := New(d, Options{Project: project}, discard)
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
	})
	if err == nil || !strings.Contains(err.Error(), "example.com") || !strings.Contains(err.Error(), "SERVICE_UNAVAILABLE") {
		t.Errorf("err = %v", err)
	}
}

func TestTXTValuesRoundTrip(t *testing.T) {
	d := fakedns.New()
	zone := publicZone(d, "z-1", "example.com")
	p := New(d, Options{Project: project}, discard)
	long := strings.Repeat("0123456789", 300) // 3000 characters
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			endpoint.NewEndpoint("example.com", "TXT", `"v=spf1 mx -all"`, `"`+long[:255]+`" "`+long[255:]+`"`, "plain"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	values := d.RecordSets(zone)[0].GetBasic().GetValues()
	if len(values) != 3 || values[0] != "v=spf1 mx -all" || values[1] != long || values[2] != "plain" {
		t.Fatalf("stored %q", values)
	}
	records, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !slices.Equal([]string(records[0].Targets), values) {
		t.Errorf("Records = %v", records)
	}
}

func TestBatches(t *testing.T) {
	var changes []change
	for i := range 5 {
		changes = append(changes, change{dnsName: "a", deletion: "a"}, change{dnsName: string(rune('b' + i)), deletion: "x"})
	}
	// a has five changes, b to f one each.
	got := batches(changes, 6)
	if len(got) != 2 || len(got[0]) != 6 || len(got[1]) != 4 {
		t.Fatalf("batches of %v", lens(got))
	}
	for _, c := range got[1] {
		if c.dnsName == "a" {
			t.Error("a's changes were split across batches")
		}
	}
	if got := batches(changes, 3); len(got) != 4 {
		// No batch holds all five of a's changes: they are split.
		t.Errorf("batches of %v", lens(got))
	}
}

func lens(b [][]change) []int {
	var out []int
	for _, c := range b {
		out = append(out, len(c))
	}
	return out
}
