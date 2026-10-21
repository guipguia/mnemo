#!/usr/bin/env bash
# The "broken ConfigMap" incident, for trying mnemo (and recording the README demo).
#
# Argo CD deploys a checkout service; later someone hot-fixes it by hand. Run it against a
# cluster mnemo watches, then open mnemo and find out what happened.
#
#   KUBECONFIG=... demo/incident.sh            # deploy, roll out, then the hand-edits
#   KUBECONFIG=... STAGE=gitops demo/incident.sh   # only the GitOps part (a healthy cluster)
#
# Changes are applied with --field-manager=argocd-controller, which is exactly how Argo CD's
# server-side apply appears in managedFields, so mnemo attributes them to GitOps.
set -euo pipefail
NS=${NS:-shop}
PAUSE=${PAUSE:-3}
STAGE=${STAGE:-all}

say() { printf '\n\033[1m▶ %s\033[0m\n' "$*"; }
gitops() { kubectl apply --server-side --force-conflicts --field-manager=argocd-controller -f - >/dev/null; }

manifests() { # $1 = image tag
  cat <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: $NS}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: checkout-config
  namespace: $NS
  labels: {argocd.argoproj.io/instance: shop}
data:
  PAYMENT_PROVIDER: stripe
  PAYMENT_TIMEOUT: 30s
  RETRIES: "3"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: $NS
  labels: {argocd.argoproj.io/instance: shop}
spec:
  replicas: 3
  selector: {matchLabels: {app: checkout}}
  template:
    metadata: {labels: {app: checkout}}
    spec:
      containers:
        - name: checkout
          image: nginx:$1
          envFrom: [{configMapRef: {name: checkout-config}}]
EOF
}

say "Argo CD deploys checkout"
manifests 1.27 | gitops
sleep "$PAUSE"

say "Argo CD rolls out a new version"
manifests 1.28 | gitops
sleep "$PAUSE"

[ "$STAGE" = gitops ] && exit 0

say "Someone hot-fixes prod by hand: shorter payment timeout, restart, scale down"
kubectl -n "$NS" patch configmap checkout-config --type merge -p '{"data":{"PAYMENT_TIMEOUT":"3s"}}' >/dev/null
sleep 1
kubectl -n "$NS" rollout restart deployment/checkout >/dev/null
sleep 1
kubectl -n "$NS" scale deployment/checkout --replicas=1 >/dev/null

say "Done. Open mnemo: what changed, and who changed it?"
