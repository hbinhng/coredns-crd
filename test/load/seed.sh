#!/usr/bin/env bash
# Seeds N DNSSlices each carrying 50 A records under example.com.
# Generates the matching dnsperf query-list at /tmp/queries.txt
# (also written to test/load/queries.txt for inspection).
#
# Usage:
#   bash test/load/seed.sh <N>
#
# Cleanup:
#   kubectl delete dnsslice -l load=true

set -euo pipefail

N=${1:?usage: seed.sh <slice-count>}

# Cleanup any prior load slices.
kubectl delete dnsslice -l load=true --ignore-not-found --wait=true >/dev/null 2>&1 || true

ENTRIES_PER_SLICE=50
total_records=$((N * ENTRIES_PER_SLICE))
echo "seeding $N slices x $ENTRIES_PER_SLICE entries = $total_records A records"

QUERIES_FILE=test/load/queries.txt
: > "$QUERIES_FILE"

for s in $(seq 1 "$N"); do
  manifest=$(printf 'apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: load-%04d
  namespace: default
  labels:
    load: "true"
spec:
  entries:
' "$s")
  for e in $(seq 1 "$ENTRIES_PER_SLICE"); do
    name="r${e}.s${s}.example.com."
    # Spread IPs across 10.0.0.0/8 so they're plausible
    ip="10.$(( (s >> 8) & 255 )).$(( s & 255 )).$e"
    manifest="${manifest}
    - name: ${name}
      type: A
      a:
        address: ${ip}"
    echo "${name} A" >> "$QUERIES_FILE"
  done
  echo "${manifest}" | kubectl apply -f - >/dev/null
done

# Wait for last slice's status to settle before declaring "ready".
kubectl wait --for=condition=Ready "dnsslice/load-$(printf '%04d' "$N")" --timeout=30s >/dev/null
echo "seeded; query list at $QUERIES_FILE ($(wc -l <"$QUERIES_FILE") queries)"
