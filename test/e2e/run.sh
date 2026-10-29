#!/usr/bin/env bash
# End-to-end test on kind: the official ExternalDNS Helm chart with this
# webhook as its sidecar, against the fake Hostinger API. Verifies that
# Services become DNS records (including an apex record and a CNAME), that a
# steady state makes no writes, that updates and deletes are applied with
# their ownership records, and that a record ExternalDNS does not own is
# left alone.
#
# Usage: test/e2e/run.sh            (creates kind cluster "hostinger-e2e")
#   KEEP_CLUSTER=1  keep the cluster afterwards
#   SKIP_BUILD=1    reuse already-loaded images
#   KIND_NODE_IMAGE kindest/node image to test another Kubernetes version
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
E2E="$ROOT/test/e2e"
CLUSTER="${KIND_CLUSTER:-hostinger-e2e}"
KIND="${KIND:-kind}"
IMAGE_REPO="external-dns-hostinger-webhook"
IMAGE_TAG="e2e"
EXTERNAL_DNS_CHART_VERSION="${EXTERNAL_DNS_CHART_VERSION:-$(cat "$ROOT/hack/external-dns-chart.version")}"
TIMEOUT="${TIMEOUT:-300s}"
ZONE_URL="/api/v1/namespaces/e2e/services/http:fake-hostinger:80/proxy/debug/zones/example.com"
WRITES_URL="/api/v1/namespaces/e2e/services/http:fake-hostinger:80/proxy/debug/writes"

log() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

dump() {
  log "Failure diagnostics"
  kubectl -n external-dns get pods -o wide || true
  kubectl -n external-dns logs deploy/external-dns -c external-dns --tail=100 || true
  kubectl -n external-dns logs deploy/external-dns -c webhook --tail=100 || true
  kubectl -n e2e logs deploy/fake-hostinger --tail=100 || true
  kubectl get --raw "$ZONE_URL" | jq . || true
}

cleanup() {
  local rc=$?
  [[ $rc -ne 0 ]] && dump
  if [[ -z "${KEEP_CLUSTER:-}" ]]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  exit $rc
}
trap cleanup EXIT

wait_for() { # wait_for <description> <timeout seconds> <command...>
  local what=$1 secs=$2; shift 2
  for ((i = 0; i < secs; i += 5)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 5
  done
  echo "timed out waiting for $what" >&2
  return 1
}

zone() { kubectl get --raw "$ZONE_URL"; }
writes() { kubectl get --raw "$WRITES_URL"; }

# has_record <name> <type> <content> [ttl]: the set exists and contains content.
has_record() {
  zone | jq -e --arg n "$1" --arg t "$2" --arg c "$3" --arg ttl "${4:-}" '
    any(.[]; .name == $n and .type == $t and any(.records[]; .content == $c)
             and ($ttl == "" or .ttl == ($ttl | tonumber)))'
}

# no_set <name> <type>: the zone was read and has no set with that name and type.
no_set() {
  zone | jq -e --arg n "$1" --arg t "$2" 'any(.[]; .name == $n and .type == $t) | not'
}

# owned <txt name>: the ownership TXT record for owner "e2e" exists.
owned() {
  zone | jq -e --arg n "$1" '
    any(.[]; .name == $n and .type == "TXT" and any(.records[]; .content | contains("external-dns/owner=e2e")))'
}

log "Creating kind cluster $CLUSTER"
"$KIND" get clusters | grep -qx "$CLUSTER" || "$KIND" create cluster --name "$CLUSTER" --wait 120s ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"}
kubectl config use-context "kind-$CLUSTER" >/dev/null

if [[ -z "${SKIP_BUILD:-}" ]]; then
  log "Building images"
  docker build -q -t "$IMAGE_REPO:$IMAGE_TAG" --build-arg VERSION=e2e "$ROOT"
  docker build -q -t fake-hostinger:e2e -f "$E2E/Dockerfile.fake" "$ROOT"
fi
"$KIND" load docker-image --name "$CLUSTER" "$IMAGE_REPO:$IMAGE_TAG" fake-hostinger:e2e

log "Deploying fake Hostinger API"
kubectl apply -f "$E2E/manifests/fake-hostinger.yaml"
kubectl -n e2e rollout status deploy/fake-hostinger --timeout="$TIMEOUT"

log "Creating source Services"
kubectl apply -f "$E2E/manifests/services.yaml"

log "Installing ExternalDNS chart $EXTERNAL_DNS_CHART_VERSION with the webhook sidecar"
helm upgrade --install external-dns external-dns \
  --repo https://kubernetes-sigs.github.io/external-dns/ --version "$EXTERNAL_DNS_CHART_VERSION" \
  --namespace external-dns --create-namespace --wait --timeout "$TIMEOUT" \
  -f "$E2E/values.yaml" \
  --set provider.webhook.image.repository="$IMAGE_REPO" \
  --set provider.webhook.image.tag="$IMAGE_TAG"

log "Waiting for records to be created"
wait_for "web A record" 120 has_record web A 10.96.200.10 300
wait_for "apex A record" 60 has_record @ A 10.96.200.11
wait_for "alias CNAME record" 60 has_record alias CNAME target.example.net.
for txt in extdns-a.web extdns-a extdns-cname.alias; do
  wait_for "ownership record $txt" 60 owned "$txt"
done
has_record manual A 192.0.2.1 300

# With nothing changed, several sync loops (interval 10s) must not write.
log "Checking the steady state makes no writes"
before="$(writes)"
sleep 40
after="$(writes)"
if [[ "$before" != "$after" ]]; then
  echo "steady state made $((after - before)) write calls" >&2
  exit 1
fi

log "Updating the TTL of web.example.com"
kubectl -n e2e annotate service web external-dns.alpha.kubernetes.io/ttl=600 --overwrite
wait_for "web TTL update" 120 has_record web A 10.96.200.10 600

log "Deleting alias.example.com"
kubectl -n e2e delete service alias --wait=true
wait_for "alias CNAME deletion" 120 no_set alias CNAME
wait_for "alias ownership record deletion" 60 no_set extdns-cname.alias TXT

log "Checking unmanaged records and the webhook's health"
has_record manual A 192.0.2.1 300
has_record @ A 10.96.200.11
restarts="$(kubectl -n external-dns get pods -l app.kubernetes.io/name=external-dns \
  -o jsonpath='{range .items[*].status.containerStatuses[*]}{.restartCount}{"\n"}{end}' | sort -n | tail -1)"
if [[ "$restarts" != "0" ]]; then
  echo "containers restarted $restarts times" >&2
  exit 1
fi

log "E2E passed"
