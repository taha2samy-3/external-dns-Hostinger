<p align="center">
  <img src="assets/icon.svg" width="128" alt="">
</p>

# external-dns-hostinger-webhook

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns)
[webhook provider](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/)
for domains hosted on [Hostinger DNS](https://www.hostinger.com). ExternalDNS
creates, updates and deletes DNS records for your `Service`, `Ingress` and
Gateway API resources through the [Hostinger API](https://developers.hostinger.com).

- Runs as a sidecar of ExternalDNS and listens on `localhost` only.
- Uses ExternalDNS' own webhook handlers, so the wire format matches ExternalDNS.
- Discovers zones from your Hostinger domain portfolio, or uses `DOMAIN_FILTER`.
- Supports `A`, `AAAA`, `CNAME`, `TXT`, `MX`, `SRV`, `NS` and `CAA` records.
- Tested end to end against ExternalDNS' controller, TXT registry and webhook client.

> This is a community project, not affiliated with or endorsed by Hostinger or
> the ExternalDNS project.

## Requirements

- Kubernetes 1.27+
- ExternalDNS 0.15+ (Helm chart 1.15+)
- A Hostinger API token: hPanel → Profile → **API**

## Install

1. Store the API token:

   ```sh
   kubectl create namespace external-dns
   kubectl -n external-dns create secret generic hostinger-credentials \
     --from-literal=api-token='<HOSTINGER_API_TOKEN>'
   ```

2. Install ExternalDNS with the webhook as a sidecar, using
   [deploy/external-dns-values.yaml](deploy/external-dns-values.yaml) as a starting point:

   ```sh
   helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
   helm upgrade --install external-dns external-dns/external-dns \
     --namespace external-dns -f deploy/external-dns-values.yaml
   ```

### Apex records need a TXT prefix

ExternalDNS' TXT registry names its ownership record `a-example.com` for an
`A` record at `example.com`. That name lies outside the zone, so ExternalDNS
can create apex records but never update or delete them. Set a prefix that
contains the record type and ends in a dot, as the
[ExternalDNS docs](https://kubernetes-sigs.github.io/external-dns/latest/docs/registry/txt/)
recommend:

```yaml
txtPrefix: "extdns-%{record_type}."
```

## Configuration

The webhook is configured through environment variables.

| Variable | Default | Description |
|---|---|---|
| `HOSTINGER_API_TOKEN` | *(required)* | Hostinger API token. |
| `DOMAIN_FILTER` | *(all active domains)* | Comma-separated zones to manage. |
| `DEFAULT_TTL` | `3600` | TTL for records that do not set one. |
| `DRY_RUN` | `false` | Log changes instead of applying them. |
| `REQUEST_TIMEOUT` | `12s` | Deadline for each call from ExternalDNS. Keep it below ExternalDNS' `--webhook-provider-read-timeout` + `--webhook-provider-write-timeout` (15s by default). |
| `ZONE_REFRESH_INTERVAL` | `1h` | How often the domain portfolio is re-read when `DOMAIN_FILTER` is unset. |
| `WEBHOOK_ADDR` | `127.0.0.1:8888` | Listen address of the webhook API. |
| `HEALTH_ADDR` | `0.0.0.0:8080` | Listen address of `/healthz`. |
| `MAX_BODY_SIZE` | `33554432` | Maximum request body size in bytes. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |
| `HOSTINGER_API_URL` | `https://developers.hostinger.com` | API endpoint, for testing. |

ExternalDNS reads the zone list once, at startup. Restart ExternalDNS after
adding a domain to your Hostinger account.

## How records are mapped

Hostinger stores records as sets keyed by name and type, the same unit as an
ExternalDNS endpoint. Creates and updates replace the whole set
(`PUT /api/dns/v1/zones/{domain}` with `overwrite: true`) and deletes remove it.
Deletes are applied before creates, so a record can change type in one sync.

- Host names in `CNAME`, `NS`, `MX` and `SRV` content are sent fully qualified
  (`target.example.net.`) and returned without the trailing dot.
- `TXT` values are sent unquoted. Hostinger returns them quoted, which is the
  form ExternalDNS uses.
- `SOA` and Hostinger's `ALIAS` records are never reported to ExternalDNS, so they
  are never changed.
- Set identifiers (weighted or geo routing) are not supported.

## Development

```sh
make test    # unit and end-to-end tests against a fake Hostinger API
make build   # binary in _out/webhook
make image   # container image
```

## License

[Apache 2.0](LICENSE)
