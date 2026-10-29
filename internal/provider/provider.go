// Package provider implements an ExternalDNS provider backed by the Hostinger
// DNS zone API.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
)

// maxConcurrentZones bounds how many zones are read from Hostinger at once.
const maxConcurrentZones = 4

// supportedTypes are the record types both ExternalDNS and Hostinger know.
// SOA and Hostinger's ALIAS are left out on purpose.
var supportedTypes = map[string]bool{
	endpoint.RecordTypeA:     true,
	endpoint.RecordTypeAAAA:  true,
	endpoint.RecordTypeCNAME: true,
	endpoint.RecordTypeTXT:   true,
	endpoint.RecordTypeMX:    true,
	endpoint.RecordTypeSRV:   true,
	endpoint.RecordTypeNS:    true,
	"CAA":                    true,
}

// API is the subset of the Hostinger client the provider needs.
type API interface {
	ListDomains(ctx context.Context) ([]hostinger.Domain, error)
	GetRecords(ctx context.Context, domain string) ([]hostinger.RecordSet, error)
	UpdateRecords(ctx context.Context, domain string, overwrite bool, sets []hostinger.RecordSet) error
	DeleteRecords(ctx context.Context, domain string, filters []hostinger.DeleteFilter) error
}

// Config configures a Provider.
type Config struct {
	// Zones to manage. When empty, every active domain in the account's
	// Hostinger portfolio is managed.
	Zones []string
	// DefaultTTL is used for endpoints that do not set a TTL.
	DefaultTTL int
	// DryRun logs changes instead of applying them.
	DryRun bool
	// RequestTimeout bounds each call from ExternalDNS. Keep it below
	// ExternalDNS' webhook read+write timeout (15s by default).
	RequestTimeout time.Duration
	// ZoneRefreshInterval is how often the portfolio is re-read when zones
	// are discovered rather than configured.
	ZoneRefreshInterval time.Duration
}

// Provider is an ExternalDNS provider for Hostinger DNS.
type Provider struct {
	provider.BaseProvider

	api API
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	zones       []string
	zonesLoaded time.Time
}

// New returns a Provider and loads its zones, failing if none are usable.
func New(ctx context.Context, api API, cfg Config, log *slog.Logger) (*Provider, error) {
	p := &Provider{api: api, cfg: cfg, log: log}
	for _, z := range cfg.Zones {
		if z = normalize(z); z != "" {
			p.zones = append(p.zones, z)
		}
	}
	if len(p.zones) == 0 {
		if err := p.discoverZones(ctx); err != nil {
			return nil, err
		}
	}
	if len(p.zones) == 0 {
		return nil, errors.New("no zones to manage: set DOMAIN_FILTER or add an active domain to the Hostinger account")
	}
	// Fail fast on a bad token or a zone the token cannot manage.
	for _, z := range p.zones {
		if _, err := api.GetRecords(ctx, z); err != nil {
			return nil, fmt.Errorf("reading zone %s: %w", z, err)
		}
	}
	slices.Sort(p.zones)
	log.Info("managing zones", "zones", p.zones)
	return p, nil
}

func (p *Provider) discoverZones(ctx context.Context) error {
	domains, err := p.api.ListDomains(ctx)
	if err != nil {
		return fmt.Errorf("listing Hostinger domains: %w", err)
	}
	var zones []string
	for _, d := range domains {
		if d.Domain != nil && d.Status == "active" {
			zones = append(zones, normalize(*d.Domain))
		}
	}
	slices.Sort(zones)
	p.mu.Lock()
	p.zones, p.zonesLoaded = zones, time.Now()
	p.mu.Unlock()
	return nil
}

// currentZones returns the managed zones, re-reading the portfolio when they
// were discovered and the refresh interval has passed. A failed refresh keeps
// the previous list.
func (p *Provider) currentZones(ctx context.Context) []string {
	p.mu.Lock()
	stale := len(p.cfg.Zones) == 0 && p.cfg.ZoneRefreshInterval > 0 &&
		time.Since(p.zonesLoaded) > p.cfg.ZoneRefreshInterval
	p.mu.Unlock()
	if stale {
		if err := p.discoverZones(ctx); err != nil {
			p.log.Warn("refreshing zones failed, keeping previous list", "error", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.zones)
}

// GetDomainFilter returns the managed zones. ExternalDNS reads it once, when
// it starts.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	p.mu.Lock()
	defer p.mu.Unlock()
	return endpoint.NewDomainFilter(p.zones)
}

// Records returns every supported record in the managed zones.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	zones := p.currentZones(ctx)
	results := make([][]*endpoint.Endpoint, len(zones))
	errs := make([]error, len(zones))
	sem := make(chan struct{}, maxConcurrentZones)
	var wg sync.WaitGroup
	for i, zone := range zones {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			sets, err := p.api.GetRecords(ctx, zone)
			if err != nil {
				errs[i] = fmt.Errorf("reading zone %s: %w", zone, err)
				return
			}
			results[i] = toEndpoints(zone, sets)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return slices.Concat(results...), nil
}

// toEndpoints converts the record sets of zone, merging sets that share a
// name and type.
func toEndpoints(zone string, sets []hostinger.RecordSet) []*endpoint.Endpoint {
	type key struct{ name, typ string }
	byKey := map[key]*endpoint.Endpoint{}
	var out []*endpoint.Endpoint
	for _, rs := range sets {
		typ := strings.ToUpper(rs.Type)
		if !supportedTypes[typ] {
			continue
		}
		name := fqdn(zone, rs.Name)
		k := key{name, typ}
		ep, ok := byKey[k]
		if !ok {
			ep = endpoint.NewEndpointWithTTL(name, typ, endpoint.TTL(rs.TTL))
			byKey[k] = ep
			out = append(out, ep)
		}
		for _, r := range rs.Records {
			ep.Targets = append(ep.Targets, fromContent(typ, r.Content))
		}
	}
	return out
}

// AdjustEndpoints canonicalizes desired endpoints into the form Records
// returns, so unchanged records do not show up as updates.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	out := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if !supportedTypes[ep.RecordType] {
			p.log.Warn("skipping unsupported record type", "name", ep.DNSName, "type", ep.RecordType)
			continue
		}
		if ep.SetIdentifier != "" {
			p.log.Warn("set identifiers are not supported, ignoring", "name", ep.DNSName, "setIdentifier", ep.SetIdentifier)
		}
		for i, t := range ep.Targets {
			ep.Targets[i] = fromContent(ep.RecordType, t)
		}
		out = append(out, ep)
	}
	return out, nil
}

type zoneChanges struct {
	deletes []hostinger.DeleteFilter
	upserts []hostinger.RecordSet
}

// ApplyChanges applies changes zone by zone. Hostinger stores records as sets
// keyed by name and type, the same granularity as ExternalDNS endpoints, so
// creates and updates replace the whole set and deletes remove it.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	zones := p.currentZones(ctx)
	byZone := map[string]*zoneChanges{}
	forZone := func(ep *endpoint.Endpoint) (string, *zoneChanges, bool) {
		zone, name, ok := splitName(zones, ep.DNSName)
		if !ok {
			p.log.Warn("no managed zone for record, skipping", "name", ep.DNSName, "type", ep.RecordType)
			return "", nil, false
		}
		zc := byZone[zone]
		if zc == nil {
			zc = &zoneChanges{}
			byZone[zone] = zc
		}
		return name, zc, true
	}

	for _, ep := range changes.Delete {
		if name, zc, ok := forZone(ep); ok {
			zc.deletes = append(zc.deletes, hostinger.DeleteFilter{Name: name, Type: ep.RecordType})
		}
	}
	for _, ep := range slices.Concat(changes.Create, changes.UpdateNew) {
		if name, zc, ok := forZone(ep); ok {
			zc.upserts = append(zc.upserts, p.toRecordSet(name, ep))
		}
	}

	var errs []error
	for _, zone := range slices.Sorted(maps.Keys(byZone)) {
		zc := byZone[zone]
		if p.cfg.DryRun {
			p.log.Info("dry run: would apply changes", "zone", zone, "delete", zc.deletes, "upsert", zc.upserts)
			continue
		}
		// Deletes go first so a record can change type (e.g. A to CNAME) in
		// one batch.
		if len(zc.deletes) > 0 {
			if err := p.api.DeleteRecords(ctx, zone, zc.deletes); err != nil {
				errs = append(errs, fmt.Errorf("deleting records in %s: %w", zone, err))
				continue
			}
			p.log.Info("deleted records", "zone", zone, "records", zc.deletes)
		}
		if len(zc.upserts) > 0 {
			if err := p.api.UpdateRecords(ctx, zone, true, zc.upserts); err != nil {
				errs = append(errs, fmt.Errorf("updating records in %s: %w", zone, err))
				continue
			}
			p.log.Info("updated records", "zone", zone, "records", zc.upserts)
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) toRecordSet(name string, ep *endpoint.Endpoint) hostinger.RecordSet {
	ttl := p.cfg.DefaultTTL
	if ep.RecordTTL.IsConfigured() {
		ttl = int(ep.RecordTTL)
	}
	rs := hostinger.RecordSet{Name: name, Type: ep.RecordType, TTL: ttl}
	for _, t := range ep.Targets {
		rs.Records = append(rs.Records, hostinger.Record{Content: toContent(ep.RecordType, t)})
	}
	return rs
}

func (p *Provider) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.cfg.RequestTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.cfg.RequestTimeout)
}

// fromContent converts Hostinger record content to an ExternalDNS target:
// host names lose their trailing dot and TXT values are quoted, matching
// what the ExternalDNS TXT registry produces.
func fromContent(typ, content string) string {
	switch typ {
	case endpoint.RecordTypeCNAME, endpoint.RecordTypeNS, endpoint.RecordTypeMX, endpoint.RecordTypeSRV:
		return strings.TrimSuffix(content, ".")
	case endpoint.RecordTypeTXT:
		if len(content) >= 2 && strings.HasPrefix(content, `"`) && strings.HasSuffix(content, `"`) {
			return content
		}
		return `"` + content + `"`
	}
	return content
}

// toContent converts an ExternalDNS target to Hostinger record content. Host
// names are made fully qualified so Hostinger does not append the zone, and
// TXT values are sent unquoted because Hostinger adds the quotes itself.
func toContent(typ, target string) string {
	switch typ {
	case endpoint.RecordTypeCNAME, endpoint.RecordTypeNS, endpoint.RecordTypeMX, endpoint.RecordTypeSRV:
		// The host name is the last field: "10 mail.example.com" for MX,
		// "0 5 5060 sip.example.com" for SRV.
		if !strings.HasSuffix(target, ".") {
			return target + "."
		}
	case endpoint.RecordTypeTXT:
		if len(target) >= 2 && strings.HasPrefix(target, `"`) && strings.HasSuffix(target, `"`) {
			return target[1 : len(target)-1]
		}
	}
	return target
}

// fqdn returns the full name of a Hostinger record name in zone.
func fqdn(zone, name string) string {
	name = normalize(name)
	switch {
	case name == "" || name == "@" || name == zone:
		return zone
	case strings.HasSuffix(name, "."+zone):
		return name
	}
	return name + "." + zone
}

// splitName finds the most specific zone containing dnsName and returns the
// record name relative to it, using "@" for the zone apex.
func splitName(zones []string, dnsName string) (zone, name string, ok bool) {
	dnsName = normalize(dnsName)
	for _, z := range zones {
		if len(z) <= len(zone) {
			continue
		}
		if dnsName == z {
			zone, name, ok = z, "@", true
		} else if n, found := strings.CutSuffix(dnsName, "."+z); found {
			zone, name, ok = z, n, true
		}
	}
	return zone, name, ok
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}
