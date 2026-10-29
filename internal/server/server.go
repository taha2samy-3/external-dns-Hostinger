// Package server wires a provider into the ExternalDNS webhook HTTP API.
package server

import (
	"net/http"

	"sigs.k8s.io/external-dns/provider"
	"sigs.k8s.io/external-dns/provider/webhook/api"
)

// Handler serves the webhook API for p. It reuses the upstream ExternalDNS
// handlers so the wire format stays in lockstep with ExternalDNS.
func Handler(p provider.Provider, maxBodySize int64) http.Handler {
	hooks := api.WebhookServer{Provider: p, MaxBodySize: maxBodySize}
	mux := http.NewServeMux()
	mux.HandleFunc("/", hooks.NegotiateHandler)
	mux.HandleFunc(api.UrlRecords, hooks.RecordsHandler)
	mux.HandleFunc(api.UrlAdjustEndpoints, hooks.AdjustEndpointsHandler)
	return mux
}
