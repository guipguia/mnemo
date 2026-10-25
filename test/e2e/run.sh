#!/usr/bin/env bash
# End-to-end test: install the chart on a real cluster, replay demo/incident.sh, and check
# through the API that mnemo recorded what happened, including across a hub outage.
#
#   KUBECONFIG=<throwaway cluster> IMAGE=mnemo:e2e test/e2e/run.sh
#
# IMAGE must already be loadable by the cluster (e.g. `kind load docker-image`).
# AUDIT=1 also tests user attribution: the cluster must come from test/e2e/kind.yaml.
# Needs kubectl, helm, curl, jq (and openssl with AUDIT=1). Uses namespaces mnemo-e2e and shop.
set -euo pipefail
cd "$(dirname "$0")/../.."

IMAGE=${IMAGE:?set IMAGE, e.g. mnemo:e2e}
NS=mnemo-e2e
PORT=${PORT:-18200}
API=http://127.0.0.1:$PORT/api/v1

fail() { echo "FAIL: $*" >&2; kubectl -n "$NS" logs deploy/mnemo --tail=50 >&2 || true; exit 1; }
pass() { echo "ok   $*"; }

# retry CMD... until it succeeds or 60s pass: the agent records asynchronously.
retry() { for _ in $(seq 60); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; "$@"; }

port_forward() {
  kubectl -n "$NS" port-forward svc/mnemo "$PORT:80" >/dev/null 2>&1 &
  PF=$!
  retry curl -sf "http://127.0.0.1:$PORT/healthz" || fail "hub not reachable"
}
trap 'kill ${PF:-} 2>/dev/null || true' EXIT

# changes ARGS: query the change feed (newest first) with extra query args.
changes() { curl -sf "$API/changes?limit=1000&$1"; }

# expect DESCRIPTION JQ_FILTER QUERY: some change in the feed matches the filter.
expect() {
  local what=$1 filter=$2 query=${3:-}
  check() { changes "$query" | jq -e "any(.[]; $filter)" >/dev/null; }
  retry check || fail "$what: no change matching $filter"
  pass "$what"
}

echo "== install"
helm upgrade --install mnemo charts/mnemo -n "$NS" --create-namespace --wait --timeout 4m \
  --set agent.cluster=e2e \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=Never \
  >/dev/null
port_forward
curl -sf "http://127.0.0.1:$PORT/" | grep -q "<title>mnemo</title>" || fail "UI not served"
pass "UI served"
expect "baseline recorded on install" '.type == "baseline"' "namespace=kube-system"

echo "== incident"
PAUSE=2 demo/incident.sh >/dev/null
Q="namespace=shop"
expect "GitOps create attributed to argocd-controller" \
  '.kind == "ConfigMap" and .name == "checkout-config" and .type == "added" and .author == "argocd-controller" and .source == "gitops"' "$Q"
expect "GitOps rollout attributed, image path recorded" \
  '.kind == "Deployment" and .author == "argocd-controller" and .paths == ["/spec/template/spec/containers/0/image"]' "$Q"
expect "hand-edit attributed to kubectl-patch and flagged as drift" \
  '.name == "checkout-config" and .author == "kubectl-patch" and .source == "manual" and .drift and .paths == ["/data/PAYMENT_TIMEOUT"]' "$Q"
expect "scale via /scale attributed to kubectl, flagged as drift" \
  '.kind == "Deployment" and .author == "kubectl" and .drift and .paths == ["/spec/replicas"]' "$Q"

patched=$(changes "$Q&kind=ConfigMap" | jq -r '[.[] | select(.author == "kubectl-patch")][0].time')
before=$(date -u -d "$patched - 1 second" +%Y-%m-%dT%H:%M:%SZ)
timeout=$(curl -sf "$API/at?cluster=e2e&kind=ConfigMap&namespace=shop&name=checkout-config&time=$before" | jq -r .object.data.PAYMENT_TIMEOUT)
[ "$timeout" = 30s ] || fail "state before the hand-edit: PAYMENT_TIMEOUT=$timeout, want 30s"
pass "state at a time before the hand-edit shows PAYMENT_TIMEOUT=30s"

echo "== hub outage"
total=$(changes "$Q" | jq length)
kubectl -n "$NS" scale deploy/mnemo --replicas=0 >/dev/null
kubectl -n "$NS" wait --for=delete pod -l app.kubernetes.io/name=mnemo --timeout=2m >/dev/null
kill "$PF" 2>/dev/null || true
kubectl -n shop patch configmap checkout-config --type merge -p '{"data":{"RETRIES":"0"}}' >/dev/null
offline_at=$(date -u +%s)
sleep 5
kubectl -n "$NS" scale deploy/mnemo --replicas=1 >/dev/null
kubectl -n "$NS" rollout status deploy/mnemo --timeout=2m >/dev/null
port_forward
expect "change made while the hub was down is recorded after restart" \
  '.name == "checkout-config" and .paths == ["/data/RETRIES"] and .author == "kubectl-patch"' "$Q"
recorded=$(changes "$Q&kind=ConfigMap" | jq -r '[.[] | select(.paths == ["/data/RETRIES"])][0].time')
skew=$(( $(date -u -d "$recorded" +%s) - offline_at ))
[ "${skew#-}" -le 2 ] || fail "offline change recorded ${skew}s off from when it happened"
pass "...at the time it happened, not when the hub came back (${skew}s)"
[ "$(changes "$Q" | jq length)" -gt "$total" ] || fail "history lost across the restart"
pass "history kept across the restart (PVC)"

if [ "${AUDIT:-}" = 1 ]; then
  echo "== audit attribution"
  kubectl -n "$NS" create secret generic mnemo-audit --from-literal=token=e2e-audit-token \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  tls=$(mktemp -d)
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj /CN=mnemo-audit \
    -addext subjectAltName=IP:10.96.200.200 -keyout "$tls/key" -out "$tls/crt" 2>/dev/null
  kubectl -n "$NS" create secret tls mnemo-audit-tls --cert "$tls/crt" --key "$tls/key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kill "$PF" 2>/dev/null || true
  helm upgrade mnemo charts/mnemo -n "$NS" --reuse-values --wait --timeout 4m \
    --set agent.audit.enabled=true --set agent.audit.existingSecret=mnemo-audit \
    --set agent.audit.tlsSecret=mnemo-audit-tls \
    --set agent.audit.service.clusterIP=10.96.200.200 >/dev/null
  port_forward
  logs_say() { kubectl -n "$NS" logs deploy/mnemo | grep -q "$1"; }
  retry logs_say 'audit=:8443' || fail "agent not receiving audit events"
  # --as: how an access proxy acting for a person shows up in the audit log.
  as() { kubectl --as "$1" --as-group system:masters -n shop "${@:2}" >/dev/null; }
  as alice@example.com patch configmap checkout-config --type merge -p '{"data":{"OWNER":"alice"}}'
  expect "hand-edit attributed to the person, not the field manager" \
    '.name == "checkout-config" and .paths == ["/data/OWNER"] and .author == "alice@example.com" and .source == "manual" and .drift' "$Q"
  as bob@example.com delete configmap checkout-config
  expect "delete attributed to the person who deleted it" \
    '.name == "checkout-config" and .type == "deleted" and .author == "bob@example.com" and .source == "manual"' "$Q"
fi

echo "PASS"
