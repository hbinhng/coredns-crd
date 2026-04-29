#!/usr/bin/env bash
# Runs dnsperf against the cluster DNS Service for a fixed duration and
# dumps the results plus per-pod CPU/memory snapshots.
#
# Usage:
#   bash test/load/run.sh <duration-seconds> [client-count]
#
# Default client-count = 4 (parallel request streams). dnsperf's -c is
# the number of in-flight queries, not threads; matches our 4-vCPU box.
#
# Reads the query list at test/load/queries.txt produced by seed.sh.

set -euo pipefail

DURATION=${1:-60}
CLIENTS=${2:-4}

DNS_IP=$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')
[[ -n "$DNS_IP" ]] || { echo "kube-dns Service has no ClusterIP"; exit 1; }
echo "DNS=$DNS_IP duration=${DURATION}s clients=$CLIENTS"

# Snapshot pre-test resource usage.
echo "=== pre-test pod state ==="
kubectl top pod -n kube-system -l app.kubernetes.io/name=coredns-crd 2>/dev/null || \
  echo "(metrics-server unavailable)"

# Mount the query list via a ConfigMap so the dnsperf pod can read it.
kubectl delete configmap dnsperf-queries --ignore-not-found >/dev/null 2>&1
kubectl create configmap dnsperf-queries --from-file=queries.txt=test/load/queries.txt >/dev/null

kubectl delete pod dnsperf --ignore-not-found --wait=true >/dev/null 2>&1 || true
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: dnsperf
spec:
  restartPolicy: Never
  dnsPolicy: Default
  containers:
    - name: dnsperf
      image: dnsperf:local
      imagePullPolicy: IfNotPresent
      args:
        - "-s"
        - "$DNS_IP"
        - "-d"
        - "/queries/queries.txt"
        - "-l"
        - "$DURATION"
        - "-c"
        - "$CLIENTS"
        - "-Q"
        - "100000"
      volumeMounts:
        - name: queries
          mountPath: /queries
          readOnly: true
  volumes:
    - name: queries
      configMap:
        name: dnsperf-queries
EOF

# Wait for completion (Pending→Running→Succeeded/Failed).
echo "=== running dnsperf ==="
PHASE=""
deadline=$((SECONDS + DURATION + 60))
while (( SECONDS < deadline )); do
  PHASE=$(kubectl get pod dnsperf -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 2
done

echo "=== mid-test pod state ==="
kubectl top pod -n kube-system -l app.kubernetes.io/name=coredns-crd 2>/dev/null || true

echo "=== dnsperf output ==="
kubectl logs dnsperf

echo "=== post-test cleanup ==="
kubectl delete pod dnsperf --wait=false --ignore-not-found >/dev/null
kubectl delete configmap dnsperf-queries --ignore-not-found >/dev/null
