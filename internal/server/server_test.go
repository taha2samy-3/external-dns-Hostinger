package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/external-dns/controller"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider/webhook"
	"sigs.k8s.io/external-dns/registry/txt"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/provider"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/server"
)

const token = "test-token"

type staticSource struct{ endpoints []*endpoint.Endpoint }

func (s *staticSource) Endpoints(context.Context) ([]*endpoint.Endpoint, error) {
	// The controller may modify endpoints, so hand out copies.
	out := make([]*endpoint.Endpoint, 0, len(s.endpoints))
	for _, ep := range s.endpoints {
		out = append(out, endpoint.NewEndpointWithTTL(ep.DNSName, ep.RecordType, ep.RecordTTL, ep.Targets...))
	}
	return out, nil
}

func (s *staticSource) AddEventHandler(context.Context, func()) {}

// TestExternalDNSSync drives the webhook with ExternalDNS' own webhook
// client, TXT registry and controller, against a fake Hostinger API.
func TestExternalDNSSync(t *testing.T) {
	ctx := context.Background()

	fake := hostingertest.NewServer(token, "example.com")
	defer fake.Close()
	manual := hostinger.RecordSet{Name: "manual", Type: "A", TTL: 300, Records: []hostinger.Record{{Content: "7.7.7.7"}}}
	fake.Seed("example.com", manual)

	p, err := provider.New(ctx, hostinger.NewClient(fake.URL, token, "test"),
		provider.Config{DefaultTTL: 3600}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hook := httptest.NewServer(server.Handler(p, 0))
	defer hook.Close()

	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = hook.URL
	cfg.TXTOwnerID = "test"
	cfg.ManagedDNSRecordTypes = []string{"A", "AAAA", "CNAME", "TXT", "MX"}
	wp, err := webhook.New(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("negotiating with webhook: %v", err)
	}
	if !wp.GetDomainFilter().Match("www.example.com") {
		t.Fatal("negotiated domain filter does not include example.com")
	}
	reg, err := txt.New(cfg, wp)
	if err != nil {
		t.Fatal(err)
	}

	src := &staticSource{endpoints: []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", "A", "1.2.3.4"),
		endpoint.NewEndpointWithTTL("www.example.com", "CNAME", 300, "example.com"),
		endpoint.NewEndpoint("app.example.com", "A", "10.0.0.1", "10.0.0.2"),
		endpoint.NewEndpoint("txt.example.com", "TXT", "some value"),
	}}
	ctrl := &controller.Controller{
		Source:             src,
		Registry:           reg,
		Policy:             &plan.SyncPolicy{},
		DomainFilter:       endpoint.NewDomainFilter(nil),
		ManagedRecordTypes: cfg.ManagedDNSRecordTypes,
	}

	writes := func() int { return fake.Calls(http.MethodPut) + fake.Calls(http.MethodDelete) }
	sync := func(name string) {
		t.Helper()
		if err := ctrl.RunOnce(ctx); err != nil {
			t.Fatalf("%s: RunOnce: %v", name, err)
		}
	}
	contents := func(name, typ string) []string {
		rs, ok := fake.Get("example.com", name, typ)
		if !ok {
			return nil
		}
		var out []string
		for _, r := range rs.Records {
			out = append(out, r.Content)
		}
		slices.Sort(out)
		return out
	}
	expect := func(name, typ string, want ...string) {
		t.Helper()
		if got := contents(name, typ); !slices.Equal(got, want) {
			t.Errorf("%s %s = %v, want %v", typ, name, got, want)
		}
	}
	ownershipTXT := func(name string) bool {
		rs, _ := fake.Get("example.com", name, "TXT")
		for _, r := range rs.Records {
			if strings.Contains(r.Content, "external-dns/owner=test") {
				return true
			}
		}
		return false
	}

	// First sync creates the records and their ownership TXT records.
	sync("initial")
	expect("@", "A", "1.2.3.4")
	expect("www", "CNAME", "example.com.")
	expect("app", "A", "10.0.0.1", "10.0.0.2")
	expect("txt", "TXT", "some value")
	expect("manual", "A", "7.7.7.7")
	if !ownershipTXT("a-app") || !ownershipTXT("cname-www") {
		t.Fatal("ownership TXT records were not created")
	}

	// A second sync with nothing changed must not write anything.
	before := writes()
	sync("steady state")
	if n := writes() - before; n != 0 {
		t.Fatalf("steady-state sync made %d write calls; records are not round-tripping cleanly", n)
	}

	// Changing and removing source endpoints updates and deletes records
	// but leaves records ExternalDNS does not own alone.
	src.endpoints = []*endpoint.Endpoint{
		endpoint.NewEndpoint("example.com", "A", "5.6.7.8"),
		endpoint.NewEndpointWithTTL("www.example.com", "CNAME", 300, "example.com"),
		endpoint.NewEndpoint("txt.example.com", "TXT", "some value"),
	}
	sync("update")
	expect("@", "A", "5.6.7.8")
	expect("app", "A")
	if ownershipTXT("a-app") {
		t.Error("ownership TXT record of deleted endpoint was left behind")
	}
	expect("manual", "A", "7.7.7.7")

	before = writes()
	sync("steady state after update")
	if n := writes() - before; n != 0 {
		t.Fatalf("steady-state sync after update made %d write calls", n)
	}
}
