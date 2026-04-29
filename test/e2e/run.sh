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

phase "Scenario 2: Resolution"
kubectl apply -f config/example/dnsslice.yaml
# Wait for the slice's Ready condition (informer + reconcile cycle).
kubectl wait --for=condition=Ready dnsslice/web --timeout=30s

DNS_IP=$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')
echo "DNS Service IP: $DNS_IP"

# dnsPolicy: Default uses the node's resolver for apk; dig commands target
# $DNS_IP explicitly, so cluster DNS is still being tested.
kubectl run dig --image=alpine:3.20 --restart=Never \
  --overrides='{"spec":{"dnsPolicy":"Default"}}' \
  --command -- sh -ec "
  apk add --no-cache bind-tools
  echo == A ==      ; dig +short @$DNS_IP web.example.com A
  echo == AAAA ==   ; dig +short @$DNS_IP web6.example.com AAAA
  echo == CNAME ==  ; dig +short @$DNS_IP alias.example.com CNAME
  echo == TXT ==    ; dig +short @$DNS_IP example.com TXT
  echo == SRV ==    ; dig +short @$DNS_IP _http._tcp.example.com SRV
"
for i in $(seq 1 30); do
  PHASE=$(kubectl get pod dig -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
DIG_OUT=$(kubectl logs dig 2>/dev/null)
echo "$DIG_OUT"
kubectl delete pod dig --wait=false 2>/dev/null || true

grep -q '10.0.0.1'         <<<"$DIG_OUT" || { echo "missing A 10.0.0.1"; exit 1; }
grep -q '10.0.0.2'         <<<"$DIG_OUT" || { echo "missing A 10.0.0.2"; exit 1; }
grep -q '2001:db8::1'      <<<"$DIG_OUT" || { echo "missing AAAA"; exit 1; }
grep -q 'web.example.com.' <<<"$DIG_OUT" || { echo "missing CNAME target"; exit 1; }
grep -q 'v=spf1 -all'      <<<"$DIG_OUT" || { echo "missing TXT"; exit 1; }
grep -q '10 100 80'        <<<"$DIG_OUT" || { echo "missing SRV"; exit 1; }
echo "all five record types resolved correctly"
