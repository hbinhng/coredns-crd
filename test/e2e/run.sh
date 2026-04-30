#!/usr/bin/env bash
# End-to-end test for coredns-crd. Runs against the current kubectl context.
# Image coredns-crd:e2e must be loaded into the cluster before invocation.
set -euo pipefail

phase() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }

# Strict cleanup on exit (success or failure) so a re-run starts clean.
# Pre-cleanup at start in case a previous SIGKILL'd run left residue.
cleanup() {
  kubectl delete pod dig dig2 metrics --wait=true --timeout=15s --ignore-not-found 2>/dev/null || true
  kubectl delete dnsslice e2e-loser --wait=true --timeout=15s --ignore-not-found 2>/dev/null || true
}
trap cleanup EXIT
cleanup

phase "Pre-clean: remove prior chart install + stock CoreDNS"
# Idempotency: uninstall any prior coredns-crd release before reinstalling.
if helm status coredns-crd -n kube-system >/dev/null 2>&1; then
  echo "uninstalling prior coredns-crd release"
  helm uninstall coredns-crd -n kube-system --wait
fi

# Stock CoreDNS marker: a Deployment named `coredns` in kube-system.
# Our chart's Deployment is named `kube-dns` (fullnameOverride), so it
# never matches. Capture and preserve the existing kube-dns ClusterIP so
# pods that already use ClusterFirst keep resolving after the swap.
EXISTING_DNS_IP=""
if kubectl -n kube-system get deployment coredns >/dev/null 2>&1; then
  EXISTING_DNS_IP=$(kubectl -n kube-system get svc kube-dns \
    -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
  echo "stock CoreDNS present; removing (preserving ClusterIP $EXISTING_DNS_IP)"
  kubectl -n kube-system delete deployment coredns --wait=true --ignore-not-found
  kubectl -n kube-system delete service kube-dns --wait=true --ignore-not-found
  kubectl -n kube-system delete configmap coredns --wait=true --ignore-not-found
else
  echo "no stock CoreDNS Deployment; nothing to remove"
fi

phase "Scenario 1: Install"
HELM_ARGS=(
  --namespace kube-system
  --set image.repository=coredns-crd
  --set image.tag=e2e
  --set image.pullPolicy=IfNotPresent
  --set fullnameOverride=kube-dns
)
[[ -n "$EXISTING_DNS_IP" ]] && HELM_ARGS+=(--set service.clusterIP="$EXISTING_DNS_IP")
helm install coredns-crd deploy/helm/coredns-crd "${HELM_ARGS[@]}"

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
[[ -n "$DNS_IP" ]] || { echo "kube-dns Service has no ClusterIP"; exit 1; }
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
PHASE=""
for i in $(seq 1 30); do
  PHASE=$(kubectl get pod dig -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
[[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] || { echo "dig pod stuck in $PHASE"; exit 1; }
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

phase "Scenario 3: Metrics"
LEADER_IP=$(kubectl -n kube-system get pods -l app.kubernetes.io/name=coredns-crd \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].status.podIP}')
echo "scraping http://$LEADER_IP:9153/metrics"

kubectl run metrics --image=curlimages/curl:8.10.1 --restart=Never \
  --command -- curl -s "http://$LEADER_IP:9153/metrics"
PHASE=""
for i in $(seq 1 30); do
  PHASE=$(kubectl get pod metrics -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
[[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] || { echo "metrics pod stuck in $PHASE"; exit 1; }
METRICS_OUT=$(kubectl logs metrics 2>/dev/null)
kubectl delete pod metrics --wait=false 2>/dev/null || true

for series in \
  '^coredns_crd_lookups_total' \
  '^coredns_crd_index_records' \
  '^coredns_crd_index_slices' \
  '^coredns_crd_active_conflicts' \
  '^coredns_crd_is_leader' \
  '^coredns_crd_status_patches_total' \
  '^coredns_crd_applies_total' \
  '^coredns_crd_conflict_transitions_total'; do
  grep -q "$series" <<<"$METRICS_OUT" || { echo "missing series: $series"; exit 1; }
done
echo "all 8 coredns_crd_* series present"

phase "Scenario 4: Conflict"
kubectl apply -f test/e2e/conflict-slice.yaml

# Wait for the loser slice's Conflicting condition to flip True (≤10s).
COND=""
for i in $(seq 1 10); do
  COND=$(kubectl get dnsslice e2e-loser \
    -o jsonpath='{.status.conditions[?(@.type=="Conflicting")].status}' 2>/dev/null || true)
  [[ "$COND" == "True" ]] && break
  sleep 1
done
if [[ "$COND" != "True" ]]; then
  echo "loser never marked Conflicting (got '$COND')"
  exit 1
fi
echo "loser correctly marked Conflicting=True"

# A ConflictDetected Event must exist for the loser.
EVENTS=$(kubectl get events --field-selector involvedObject.name=e2e-loser \
  -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}')
grep -q ConflictDetected <<<"$EVENTS" || {
  echo "ConflictDetected Event missing"
  echo "events:"
  echo "$EVENTS"
  exit 1
}
echo "ConflictDetected Event observed"

phase "Scenario 5: Failover"
LEADER_BEFORE=$(kubectl -n kube-system get lease coredns-crd-leader \
  -o jsonpath='{.spec.holderIdentity}')
echo "killing leader $LEADER_BEFORE"
kubectl -n kube-system delete pod "$LEADER_BEFORE" \
  --grace-period=0 --force >/dev/null 2>&1 || true

# A surviving pod must claim the lease within 30s.
NEW=""
for i in $(seq 1 30); do
  NEW=$(kubectl -n kube-system get lease coredns-crd-leader \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "$NEW" && "$NEW" != "$LEADER_BEFORE" ]] && break
  sleep 1
done
if [[ "$NEW" == "$LEADER_BEFORE" || -z "$NEW" ]]; then
  echo "lease never failed over (held by '$LEADER_BEFORE' -> '$NEW')"
  exit 1
fi
echo "lease failed over: $LEADER_BEFORE -> $NEW"

# DNS still resolves. Re-derive DNS_IP defensively.
DNS_IP=$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')
[[ -n "$DNS_IP" ]] || { echo "kube-dns Service has no ClusterIP"; exit 1; }
kubectl run dig2 --image=alpine:3.20 --restart=Never \
  --overrides='{"spec":{"dnsPolicy":"Default"}}' \
  --command -- sh -ec "
  apk add --no-cache bind-tools
  dig +short @$DNS_IP web.example.com A
"
PHASE=""
for i in $(seq 1 30); do
  PHASE=$(kubectl get pod dig2 -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
[[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] || { echo "dig2 pod stuck in $PHASE"; exit 1; }
DIG2_OUT=$(kubectl logs dig2 2>/dev/null)
kubectl delete pod dig2 --wait=false 2>/dev/null || true
grep -q '10.0.0.' <<<"$DIG2_OUT" || {
  echo "DNS broken after failover; got: $DIG2_OUT"
  exit 1
}
echo "DNS still resolving after failover"

phase "Scenario 6: Standalone-mode side-by-side install"
# Install the chart a second time in its own namespace using the
# values-standalone.yaml overlay. Verifies that:
#  (a) the overlay produces a Corefile without the kubernetes plugin
#      and without the forward block,
#  (b) the standalone install does not collide with the in-place
#      cluster-DNS install (different release name, different Lease,
#      different Service name),
#  (c) a pod with dnsConfig.nameservers pointing at the standalone
#      ClusterIP can resolve a DNSSlice that the standalone install
#      owns.

kubectl create namespace coredns-crd-standalone
helm install coredns-crd-standalone deploy/helm/coredns-crd \
  --namespace coredns-crd-standalone \
  -f deploy/helm/coredns-crd/values-standalone.yaml \
  --set image.repository=coredns-crd \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set replicaCount=1 \
  --set leaderElection.enabled=false \
  --set podDisruptionBudget.enabled=false \
  --set topologySpreadConstraints.enabled=false \
  --set priorityClassName=""

# replicaCount=1 + leader-election off keeps the test simple: one pod,
# no Lease churn. PDB off because PDB requires replicas>1. Topology
# constraints off because we're scheduling a single replica. priorityClassName
# emptied because system-cluster-critical requires kube-system or a
# ResourceQuota allowance — neither true in a tenant namespace.
# Tolerations are left as-is (they only widen scheduling, never narrow).

kubectl -n coredns-crd-standalone rollout status \
  deployment/coredns-crd-standalone --timeout=120s

STANDALONE_DNS_IP=$(kubectl -n coredns-crd-standalone get svc \
  coredns-crd-standalone -o jsonpath='{.spec.clusterIP}')
[[ -n "$STANDALONE_DNS_IP" ]] || { echo "standalone Service has no ClusterIP"; exit 1; }
echo "standalone DNS Service IP: $STANDALONE_DNS_IP"

# Apply a DNSSlice into the standalone namespace.
cat <<EOF | kubectl apply -f -
apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: standalone-test
  namespace: coredns-crd-standalone
spec:
  entries:
    - fqdn: standalone.example.test
      type: A
      a: 10.42.42.42
EOF
kubectl -n coredns-crd-standalone wait --for=condition=Ready \
  dnsslice/standalone-test --timeout=30s

# Resolve via a dig pod whose dnsConfig points at the standalone DNS.
# Uses jessie-dnsutils (dig pre-installed; avoids apk-on-restricted-PSA
# issues we hit during enigma multi-node validation).
kubectl run dig-standalone -n coredns-crd-standalone \
  --image=registry.k8s.io/e2e-test-images/jessie-dnsutils:1.7 \
  --restart=Never \
  --overrides="$(cat <<EOF
{
  "spec": {
    "dnsPolicy": "None",
    "dnsConfig": {"nameservers": ["$STANDALONE_DNS_IP"]},
    "containers": [{
      "name": "dig-standalone",
      "image": "registry.k8s.io/e2e-test-images/jessie-dnsutils:1.7",
      "command": ["sh", "-c", "dig +short standalone.example.test A; dig +short kubernetes.default.svc.cluster.local A"]
    }]
  }
}
EOF
)" \
  --command -- sh -c 'true'

PHASE=""
for i in $(seq 1 30); do
  PHASE=$(kubectl -n coredns-crd-standalone get pod dig-standalone \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
DIG_OUT=$(kubectl -n coredns-crd-standalone logs dig-standalone 2>/dev/null)
echo "$DIG_OUT"
kubectl -n coredns-crd-standalone delete pod dig-standalone --wait=false 2>/dev/null || true

# Standalone DNS resolves the declared record:
grep -q '10.42.42.42' <<<"$DIG_OUT" || { echo "standalone DNS did not resolve standalone.example.test"; exit 1; }
# Standalone DNS does NOT resolve cluster.local (kubernetes plugin is
# disabled). The second `dig` should produce empty output.
grep -q '^[0-9]' <<<"$(echo "$DIG_OUT" | tail -1)" && {
  echo "standalone DNS unexpectedly resolved cluster.local — kubernetes plugin should be disabled"
  exit 1
} || true

# Cleanup standalone install.
helm uninstall coredns-crd-standalone -n coredns-crd-standalone --wait
kubectl delete namespace coredns-crd-standalone --wait=false
echo "Scenario 6 PASS"

phase "ALL SCENARIOS PASSED"
