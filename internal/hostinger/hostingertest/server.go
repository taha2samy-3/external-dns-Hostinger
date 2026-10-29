// Package hostingertest provides an in-memory fake of the Hostinger DNS zone
// and domain portfolio APIs, so the provider can be tested without
// credentials.
package hostingertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
)

const (
	zonePrefix    = "/api/dns/v1/zones/"
	portfolioPath = "/api/domains/v1/portfolio"
)

// Fake is an in-memory Hostinger API. It implements http.Handler.
type Fake struct {
	Token string

	mu    sync.Mutex
	zones map[string][]hostinger.RecordSet
	calls map[string]int
}

// New returns a fake API for the given zones (domain names without trailing
// dot). Requests must carry "Authorization: Bearer <token>".
func New(token string, zones ...string) *Fake {
	f := &Fake{Token: token, zones: map[string][]hostinger.RecordSet{}, calls: map[string]int{}}
	for _, z := range zones {
		f.zones[strings.ToLower(z)] = nil
	}
	return f
}

// Server is a Fake listening on a local httptest server.
type Server struct {
	*Fake
	*httptest.Server
}

// NewServer starts a Fake on a random local port.
func NewServer(token string, zones ...string) *Server {
	f := New(token, zones...)
	return &Server{Fake: f, Server: httptest.NewServer(f)}
}

// Seed stores sets in zone as if they had been created in hPanel.
func (s *Fake) Seed(zone string, sets ...hostinger.RecordSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zones[zone] = append(s.zones[zone], sets...)
}

// Get returns the set called name of type typ in zone, as stored.
func (s *Fake) Get(zone, name, typ string) (hostinger.RecordSet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := indexOf(s.zones[zone], name, typ)
	if idx < 0 {
		return hostinger.RecordSet{}, false
	}
	return s.zones[zone][idx], true
}

// Calls returns how many zone requests were made with the given HTTP method.
func (s *Fake) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if r.URL.Path == portfolioPath && r.Method == http.MethodGet {
		var out []map[string]string
		for z := range s.zones {
			out = append(out, map[string]string{"domain": z, "type": "domain", "status": "active"})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	zone, ok := strings.CutPrefix(r.URL.Path, zonePrefix)
	if !ok || strings.Contains(zone, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found"})
		return
	}
	s.calls[r.Method]++

	sets, exists := s.zones[zone]
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Zone not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Like the real API, TXT contents come back wrapped in quotes.
		out := make([]hostinger.RecordSet, 0, len(sets))
		for _, rs := range sets {
			cp := rs
			cp.Records = nil
			for _, rec := range rs.Records {
				if rs.Type == "TXT" {
					rec.Content = `"` + rec.Content + `"`
				}
				cp.Records = append(cp.Records, rec)
			}
			out = append(out, cp)
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPut:
		var req struct {
			Overwrite *bool                 `json:"overwrite"`
			Zone      []hostinger.RecordSet `json:"zone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Zone) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid body"})
			return
		}
		overwrite := req.Overwrite == nil || *req.Overwrite // API default is true
		for _, in := range req.Zone {
			if in.Type == "TXT" {
				for i := range in.Records {
					in.Records[i].Content = strings.Trim(in.Records[i].Content, `"`)
				}
			}
			idx := indexOf(sets, in.Name, in.Type)
			switch {
			case idx < 0:
				sets = append(sets, in)
			case overwrite:
				sets[idx] = in
			default:
				if in.TTL != 0 {
					sets[idx].TTL = in.TTL
				}
				sets[idx].Records = append(sets[idx].Records, in.Records...)
			}
		}
		s.zones[zone] = sets
		writeJSON(w, http.StatusOK, map[string]string{})

	case http.MethodDelete:
		var req struct {
			Filters []hostinger.DeleteFilter `json:"filters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Filters) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid body"})
			return
		}
		for _, f := range req.Filters {
			sets = slices.DeleteFunc(sets, func(rs hostinger.RecordSet) bool {
				return rs.Name == f.Name && rs.Type == f.Type
			})
		}
		s.zones[zone] = sets
		writeJSON(w, http.StatusOK, map[string]string{})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
	}
}

func indexOf(sets []hostinger.RecordSet, name, typ string) int {
	for i, rs := range sets {
		if rs.Name == name && rs.Type == typ {
			return i
		}
	}
	return -1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
