package provider

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
)

const token = "test-token"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTestProvider(t *testing.T, cfg Config, zones ...string) (*Provider, *hostingertest.Server) {
	t.Helper()
	srv := hostingertest.NewServer(token, zones...)
	t.Cleanup(srv.Close)
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 3600
	}
	p, err := New(context.Background(), hostinger.NewClient(srv.URL, token, "test"), cfg, discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, srv
}

func TestNewDiscoversZones(t *testing.T) {
	p, _ := newTestProvider(t, Config{}, "b.com", "a.com")
	if !slices.Equal(p.zones, []string{"a.com", "b.com"}) {
		t.Fatalf("zones = %v", p.zones)
	}
	if !p.GetDomainFilter().Match("www.a.com") || p.GetDomainFilter().Match("c.com") {
		t.Fatal("domain filter does not match the discovered zones")
	}
}

func TestNewRejectsUnknownZone(t *testing.T) {
	srv := hostingertest.NewServer(token, "example.com")
	defer srv.Close()
	_, err := New(context.Background(), hostinger.NewClient(srv.URL, token, "test"),
		Config{Zones: []string{"other.com"}}, discard)
	if err == nil {
		t.Fatal("expected an error for a zone the token cannot manage")
	}
}

func TestRecords(t *testing.T) {
	p, srv := newTestProvider(t, Config{}, "example.com")
	srv.Seed("example.com",
		hostinger.RecordSet{Name: "@", Type: "A", TTL: 300, Records: []hostinger.Record{{Content: "1.2.3.4"}}},
		hostinger.RecordSet{Name: "@", Type: "SOA", Records: []hostinger.Record{{Content: "ns1.dns-parking.com. ..."}}},
		hostinger.RecordSet{Name: "@", Type: "ALIAS", Records: []hostinger.Record{{Content: "lb.example.net."}}},
		hostinger.RecordSet{Name: "www", Type: "CNAME", TTL: 60, Records: []hostinger.Record{{Content: "example.com."}}},
		hostinger.RecordSet{Name: "@", Type: "MX", Records: []hostinger.Record{{Content: "10 mx1.example.net."}}},
		hostinger.RecordSet{Name: "txt", Type: "TXT", Records: []hostinger.Record{{Content: "hello"}}},
		// Duplicate sets are merged.
		hostinger.RecordSet{Name: "@", Type: "A", TTL: 300, Records: []hostinger.Record{{Content: "5.6.7.8"}}},
	)

	got, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("example.com", "A", 300, "1.2.3.4", "5.6.7.8"),
		endpoint.NewEndpointWithTTL("www.example.com", "CNAME", 60, "example.com"),
		endpoint.NewEndpoint("example.com", "MX", "10 mx1.example.net"),
		endpoint.NewEndpoint("txt.example.com", "TXT", `"hello"`),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].DNSName != want[i].DNSName || got[i].RecordType != want[i].RecordType ||
			got[i].RecordTTL != want[i].RecordTTL || !got[i].Targets.Same(want[i].Targets) {
			t.Errorf("endpoint %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestApplyChanges(t *testing.T) {
	p, srv := newTestProvider(t, Config{DefaultTTL: 1800}, "example.com", "sub.example.com")
	srv.Seed("example.com",
		hostinger.RecordSet{Name: "old", Type: "A", TTL: 300, Records: []hostinger.Record{{Content: "9.9.9.9"}}},
		hostinger.RecordSet{Name: "move", Type: "A", TTL: 300, Records: []hostinger.Record{{Content: "9.9.9.9"}}},
	)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{
			endpoint.NewEndpoint("example.com", "A", "1.1.1.1"),
			endpoint.NewEndpointWithTTL("www.example.com", "CNAME", 120, "example.com"),
			endpoint.NewEndpoint("example.com", "TXT", `"heritage=external-dns,external-dns/owner=default"`),
			endpoint.NewEndpoint("deep.sub.example.com", "A", "2.2.2.2"),
			endpoint.NewEndpoint("move.example.com", "CNAME", "target.example.net"),
			endpoint.NewEndpoint("unmanaged.org", "A", "3.3.3.3"),
		},
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("old.example.com", "A", "9.9.9.9")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("old.example.com", "A", "8.8.8.8")},
		Delete:    []*endpoint.Endpoint{endpoint.NewEndpoint("move.example.com", "A", "9.9.9.9")},
	})
	if err != nil {
		t.Fatal(err)
	}

	check := func(zone, name, typ string, ttl int, contents ...string) {
		t.Helper()
		rs, ok := srv.Get(zone, name, typ)
		if !ok {
			t.Fatalf("%s %s in %s not found", typ, name, zone)
		}
		var got []string
		for _, r := range rs.Records {
			got = append(got, r.Content)
		}
		if rs.TTL != ttl || !slices.Equal(got, contents) {
			t.Errorf("%s %s in %s = ttl %d %v, want ttl %d %v", typ, name, zone, rs.TTL, got, ttl, contents)
		}
	}
	check("example.com", "@", "A", 1800, "1.1.1.1")
	check("example.com", "www", "CNAME", 120, "example.com.")
	check("example.com", "@", "TXT", 1800, "heritage=external-dns,external-dns/owner=default")
	check("sub.example.com", "deep", "A", 1800, "2.2.2.2")
	check("example.com", "old", "A", 1800, "8.8.8.8")
	check("example.com", "move", "CNAME", 1800, "target.example.net.")
	if _, ok := srv.Get("example.com", "move", "A"); ok {
		t.Error("deleted record still exists")
	}
	if _, ok := srv.Get("example.com", "deep.sub", "A"); ok {
		t.Error("record was written to the parent zone instead of the most specific one")
	}
}

func TestApplyChangesDryRun(t *testing.T) {
	p, srv := newTestProvider(t, Config{DryRun: true}, "example.com")
	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("example.com", "A", "1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.Calls(http.MethodPut) + srv.Calls(http.MethodDelete); n != 0 {
		t.Fatalf("dry run made %d write calls", n)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	p, _ := newTestProvider(t, Config{}, "example.com")
	got, err := p.AdjustEndpoints([]*endpoint.Endpoint{
		endpoint.NewEndpoint("a.example.com", "CNAME", "target.example.net."),
		endpoint.NewEndpoint("b.example.com", "TXT", "plain"),
		endpoint.NewEndpoint("c.example.com", "PTR", "x.example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unsupported type was not dropped: %v", got)
	}
	if got[0].Targets[0] != "target.example.net" || got[1].Targets[0] != `"plain"` {
		t.Errorf("targets not canonicalized: %v", got)
	}
}

func TestContentRoundTrip(t *testing.T) {
	for _, tc := range []struct{ typ, target, content string }{
		{"A", "1.2.3.4", "1.2.3.4"},
		{"CNAME", "example.com", "example.com."},
		{"NS", "ns1.example.com", "ns1.example.com."},
		{"MX", "10 mail.example.com", "10 mail.example.com."},
		{"SRV", "0 5 5060 sip.example.com", "0 5 5060 sip.example.com."},
		{"TXT", `"v=spf1 -all"`, "v=spf1 -all"},
		{"CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
	} {
		if got := toContent(tc.typ, tc.target); got != tc.content {
			t.Errorf("toContent(%s, %q) = %q, want %q", tc.typ, tc.target, got, tc.content)
		}
		// Hostinger returns TXT contents quoted.
		returned := tc.content
		if tc.typ == "TXT" {
			returned = `"` + returned + `"`
		}
		if got := fromContent(tc.typ, returned); got != tc.target {
			t.Errorf("fromContent(%s, %q) = %q, want %q", tc.typ, returned, got, tc.target)
		}
	}
}

func TestSplitName(t *testing.T) {
	zones := []string{"example.com", "sub.example.com"}
	for _, tc := range []struct {
		in, zone, name string
		ok             bool
	}{
		{"example.com", "example.com", "@", true},
		{"WWW.Example.com.", "example.com", "www", true},
		{"*.example.com", "example.com", "*", true},
		{"a.sub.example.com", "sub.example.com", "a", true},
		{"sub.example.com", "sub.example.com", "@", true},
		{"notexample.com", "", "", false},
	} {
		zone, name, ok := splitName(zones, tc.in)
		if zone != tc.zone || name != tc.name || ok != tc.ok {
			t.Errorf("splitName(%q) = %q, %q, %v; want %q, %q, %v", tc.in, zone, name, ok, tc.zone, tc.name, tc.ok)
		}
	}
}
