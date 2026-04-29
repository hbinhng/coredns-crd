#!/usr/bin/env bash
# End-to-end test for coredns-crd. Runs against the current kubectl context.
# Image coredns-crd:e2e must be loaded into the cluster before invocation.
set -euo pipefail

phase() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }

# Strict cleanup on exit (success or failure) so a re-run starts clean.
cleanup() {
  kubectl delete pod dig dig2 metrics --wait=false --ignore-not-found 2>/dev/null || true
  kubectl delete dnsslice e2e-loser --wait=false --ignore-not-found 2>/dev/null || true
}
trap cleanup EXIT

phase "Scenario 1: Install"
helm install coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set image.repository=coredns-crd \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set fullnameOverride=kube-dns

kubectl -n kube-system rollout status deployment/kube-dns --timeout=120s

# Lease holder must be set within 60s of rollout.
HOLDER=""
for i in $(seq 1 60); do
  HOLDER=$(kubectl -n kube-system get lease coredns-crd-leader \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "$HOLDER" ]] && break
  sleep 1
done
if [[ -z "$HOLDER" ]]; then
  echo "lease never acquired"
  exit 1
fi
echo "lease holder: $HOLDER"
