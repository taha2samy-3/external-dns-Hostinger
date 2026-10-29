// Command fakehostinger runs the in-memory Hostinger API inside the e2e
// cluster, so ExternalDNS can be tested end to end without a real Hostinger
// account.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger/hostingertest"
)

func main() {
	httpAddr := flag.String("http", ":8080", "API listen address")
	token := flag.String("token", "e2e-token", "accepted API bearer token")
	zones := flag.String("zones", "example.com", "comma-separated zones to serve")
	flag.Parse()

	fake := hostingertest.New(*token, strings.Split(*zones, ",")...)
	// A record created by hand in hPanel, which ExternalDNS must never touch.
	for _, z := range strings.Split(*zones, ",") {
		fake.Seed(z, hostinger.RecordSet{Name: "manual", Type: "A", TTL: 300,
			Records: []hostinger.Record{{Content: "192.0.2.1"}}})
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", fake)
	// Unauthenticated read-only views, reachable through the API server's
	// service proxy (which strips Authorization headers).
	mux.HandleFunc("GET /debug/zones/{zone}", func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.URL.Path = "/api/dns/v1/zones/" + r.PathValue("zone")
		req.Header.Set("Authorization", "Bearer "+*token)
		fake.ServeHTTP(w, req)
	})
	mux.HandleFunc("GET /debug/writes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fake.Calls(http.MethodPut) + fake.Calls(http.MethodDelete))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Printf("fake Hostinger API on %s, zones %s", *httpAddr, *zones)
	srv := &http.Server{Addr: *httpAddr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/healthz") && !strings.HasPrefix(r.URL.Path, "/debug/") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
