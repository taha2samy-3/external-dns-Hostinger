#!/usr/bin/env bash
# Prints the part of Hostinger's OpenAPI spec this webhook depends on (the DNS
# zone endpoints, the domain portfolio and their schemas) in a stable form. CI
# diffs it against hack/hostinger-api.json to detect upstream API changes.
set -euo pipefail
SPEC_URL="${SPEC_URL:-https://developers.hostinger.com/openapi/openapi.json}"
curl -fsSL --retry 3 "$SPEC_URL" | jq -S '{
  paths: (.paths | with_entries(select(.key | startswith("/api/dns/v1/zones/{domain}") or . == "/api/domains/v1/portfolio"))
                 | map_values(map_values(del(.description, .summary, .tags, ."x-codeSamples")))),
  schemas: (.components.schemas | with_entries(select(.key | test("^DNS\\.V1\\.Zone\\.|^Domains\\.V1\\.Domain\\.Domain(Resource|Collection)$")))),
  security: .components.securitySchemes
}'
