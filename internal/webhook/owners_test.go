package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.origincloud.eu/sdk/core"
	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	externaldnswebhook "sigs.k8s.io/external-dns/provider/webhook"
	"sigs.k8s.io/external-dns/registry"
	"sigs.k8s.io/external-dns/registry/txt"

	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/fakedns"
	"github.com/origincloudhq/external-dns-origincloud-webhook/internal/provider"
)

// cluster is one external-dns, as it runs against the webhook: its own
// webhook client and TXT registry, talking to a webhook of its own over
// HTTP, all webhooks sharing one zone.
type cluster struct {
	owner    string
	registry registry.Registry
	desired  []*endpoint.Endpoint
}

func newCluster(t *testing.T, owner string, dns provider.DNS, opts provider.Options, log *slog.Logger) *cluster {
	t.Helper()
	server := httptest.NewServer(Handler(provider.New(dns, opts, log), log))
	t.Cleanup(server.Close)

	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = server.URL
	cfg.TXTOwnerID = owner
	client, err := externaldnswebhook.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := txt.New(cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{owner: owner, registry: reg}
}

// sync is one reconciliation, as external-dns's controller makes it with
// --policy=sync.
func (c *cluster) sync(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	current, err := c.registry.Records(ctx)
	if err != nil {
		t.Fatalf("%s: Records: %v", c.owner, err)
	}
	var desired []*endpoint.Endpoint
	for _, ep := range c.desired {
		desired = append(desired, ep.DeepCopy())
	}
	desired, err = c.registry.AdjustEndpoints(desired)
	if err != nil {
		t.Fatalf("%s: AdjustEndpoints: %v", c.owner, err)
	}
	p := (&plan.Plan{
		Policies:       []plan.Policy{&plan.SyncPolicy{}},
		Current:        current,
		Desired:        desired,
		DomainFilter:   endpoint.MatchAllDomainFilters{c.registry.GetDomainFilter()},
		ManagedRecords: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME, endpoint.RecordTypeTXT},
		OwnerID:        c.owner,
	}).Calculate()
	if p.Changes.HasChanges() {
		if err := c.registry.ApplyChanges(ctx, p.Changes); err != nil {
			t.Fatalf("%s: ApplyChanges: %v", c.owner, err)
		}
	}
}

// TestOwnersShareAZone runs two external-dns instances, with the owner ids
// cluster-a and cluster-b, against one zone of the fake API.
func TestOwnersShareAZone(t *testing.T) {
	dns := fakedns.New()
	dns.AddZone("projects/p-1", "z-1", "example.com", dnsv1beta1.HostedZone_PUBLIC, dnsv1beta1.HostedZone_ACTIVE)
	ownersShareAZone(t, dns, "projects/p-1", "example.com", discard)
}

// TestLiveOwnersShareAZone runs the same against the live API, with the
// credentials the SDK finds, below a domain of a zone of a project:
// EXTERNAL_DNS_LIVE_PROJECT and EXTERNAL_DNS_LIVE_DOMAIN, such as
// my-project and example.com. It creates and deletes records below a name
// of its own, owners-{time}.{domain}, and skips without them.
func TestLiveOwnersShareAZone(t *testing.T) {
	project, domain := os.Getenv("EXTERNAL_DNS_LIVE_PROJECT"), os.Getenv("EXTERNAL_DNS_LIVE_DOMAIN")
	if project == "" || domain == "" {
		t.Skip("EXTERNAL_DNS_LIVE_PROJECT and EXTERNAL_DNS_LIVE_DOMAIN are not set")
	}
	client, err := core.NewClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ownersShareAZone(t, provider.NewDNS(client), "projects/"+strings.TrimPrefix(project, "projects/"),
		fmt.Sprintf("owners-%d.%s", time.Now().Unix(), domain),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

// ownersShareAZone checks that two external-dns instances sharing a zone
// keep to their own records, below domain: each creates its records with
// their ownership records; neither changes or deletes the other's, even
// when it asks for the same name; and each deletes its own when they are
// no longer wanted.
func ownersShareAZone(t *testing.T, dns provider.DNS, project, domain string, log *slog.Logger) {
	opts := provider.Options{Project: project, DomainFilter: endpoint.NewDomainFilter([]string{domain})}
	a := newCluster(t, "cluster-a", dns, opts, log)
	b := newCluster(t, "cluster-b", dns, opts, log)
	name := func(label string) string { return label + "." + domain }
	zone := provider.New(dns, opts, discard)
	records := func() []string {
		t.Helper()
		endpoints, err := zone.Records(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ep := range endpoints {
			targets := slices.Clone([]string(ep.Targets))
			slices.Sort(targets)
			out = append(out, strings.TrimSuffix(ep.DNSName, "."+domain)+" "+ep.RecordType+" "+strings.Join(targets, "|"))
		}
		slices.Sort(out)
		return out
	}
	expect := func(when string, want ...string) {
		t.Helper()
		if got := records(); !slices.Equal(got, want) {
			t.Fatalf("%s:\n%s\nwant:\n%s", when, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	t.Cleanup(func() {
		a.desired, b.desired = nil, nil
		a.sync(t)
		b.sync(t)
	})

	a.desired = []*endpoint.Endpoint{
		endpoint.NewEndpoint(name("app"), "A", "192.0.2.1").WithLabel(endpoint.ResourceLabelKey, "service/default/app"),
		endpoint.NewEndpoint(name("shared"), "A", "192.0.2.10").WithLabel(endpoint.ResourceLabelKey, "ingress/default/shared"),
	}
	b.desired = []*endpoint.Endpoint{
		endpoint.NewEndpoint(name("web"), "CNAME", "lb.example.net").WithLabel(endpoint.ResourceLabelKey, "ingress/default/web"),
		// The same name as one of a's, with another address.
		endpoint.NewEndpoint(name("shared"), "A", "192.0.2.20").WithLabel(endpoint.ResourceLabelKey, "ingress/default/shared"),
	}
	for range 2 {
		a.sync(t)
		b.sync(t)
	}
	expect("after both synced",
		"a-app TXT heritage=external-dns,external-dns/owner=cluster-a,external-dns/resource=service/default/app",
		"a-shared TXT heritage=external-dns,external-dns/owner=cluster-a,external-dns/resource=ingress/default/shared",
		"app A 192.0.2.1",
		"cname-web TXT heritage=external-dns,external-dns/owner=cluster-b,external-dns/resource=ingress/default/web",
		"shared A 192.0.2.10",
		"web CNAME lb.example.net",
	)

	// Once a wants nothing, it deletes only its own records.
	a.desired = nil
	a.sync(t)
	expect("after a deleted its records",
		"cname-web TXT heritage=external-dns,external-dns/owner=cluster-b,external-dns/resource=ingress/default/web",
		"web CNAME lb.example.net",
	)

	// b takes the name a gave up.
	b.sync(t)
	expect("after b took the name",
		"a-shared TXT heritage=external-dns,external-dns/owner=cluster-b,external-dns/resource=ingress/default/shared",
		"cname-web TXT heritage=external-dns,external-dns/owner=cluster-b,external-dns/resource=ingress/default/web",
		"shared A 192.0.2.20",
		"web CNAME lb.example.net",
	)

	b.desired = nil
	b.sync(t)
	expect("after b deleted its records")
}
