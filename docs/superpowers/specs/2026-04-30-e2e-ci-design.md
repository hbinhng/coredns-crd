# KinD-based E2E in CI

**Sub-project F** in the production-readiness push.
**Date**: 2026-04-30.
**Status**: Approved (autonomous run; user delegated supervision).

## Problem

The integration tests we ran throughout this session — install the
chart, dig the FQDN, scrape /metrics, create a conflicting slice, kill
the leader and watch failover — were all manual against a single k3s
box. There's no automated regression net for the user-facing flow. If
sub-project A's leader-election wiring breaks tomorrow, only a human
running the same `kubectl` ceremony would catch it.

The plugin's `setup()` function remains at 0% Go-level coverage. The
unit tests cover everything `setup()` orchestrates (parseConfig,
loadRESTConfig, buildLeaderElection, leaderCallbacks, the index, the
emitter, the status updater) but never exercise the orchestration
itself. A bug in the wiring — wrong order, missed lifecycle hook,
broken context propagation — would slip through unit CI today.

## Goal

Add a GitHub Actions workflow that, on every PR and push to main,
spins up a KinD cluster, helm-installs the chart, and reproduces the
manual e2e checks as automated assertions. The workflow doubles as
exercise of `setup()` end-to-end without requiring Go coverage
instrumentation.

## Non-goals

- Go-level coverage instrumentation of the running plugin. Doable
  (`-cover` + `GOCOVERDIR` + `kubectl cp` + `covdata textfmt` merge)
  but adds machinery worth deferring until production telemetry tells
  us which `setup()` paths matter.
- Multi-node KinD. Single-node is sufficient — topology spread is a
  soft constraint, leader election works either way.
- Chart value-matrix testing (chart-testing `ct` tool).
- Soak, chaos, load. Sub-project E (deferred to v1.1).

## Design

### Workflow file: `.github/workflows/e2e.yml`

Separate from `ci.yml` because:
- Different trigger filter (skip docs-only changes).
- Different runtime budget (~5 min vs ~30s).
- Different failure profile (e2e flakes are real and need re-runs;
  unit tests should not flake).

```yaml
name: E2E

on:
  push:
    branches: [main]
    paths-ignore: ['docs/**', '*.md', 'LICENSE']
  pull_request:
    paths-ignore: ['docs/**', '*.md', 'LICENSE']

permissions:
  contents: read

concurrency:
  group: e2e-${{ github.ref }}
  cancel-in-progress: true

jobs:
  e2e:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
          cache: true
      - uses: docker/setup-buildx-action@v3
      - uses: helm/kind-action@v1
        with:
          version: v0.24.0
          cluster_name: coredns-crd-e2e
      - name: Build + load image into KinD
        run: |
          docker build -t coredns-crd:e2e .
          kind load docker-image coredns-crd:e2e --name coredns-crd-e2e
      - uses: azure/setup-helm@v4
        with:
          version: v3.15.4
      - name: Run e2e
        run: bash test/e2e/run.sh
      - name: Diagnostics on failure
        if: failure()
        run: |
          kubectl get pods -A -o wide || true
          kubectl -n kube-system describe pods -l k8s-app=kube-dns || true
          kubectl -n kube-system logs -l k8s-app=kube-dns --tail=100 || true
          kubectl get events -A --sort-by='.lastTimestamp' | tail -50 || true
```

The `failure()` diagnostics block is the difference between "test
failed, who knows why" and "test failed, here's the cluster state."
Cheap and load-bearing for triage.

### Test script: `test/e2e/run.sh`

Single bash script. Inline-script approach beats inline-YAML steps
because contributors can run it locally:

```bash
bash test/e2e/run.sh
```

against any current-context cluster (a KinD/k3s/minikube install in
their dotfiles).

The script implements five scenarios sequentially:

#### Scenario 1: Install

```bash
helm install coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set image.repository=coredns-crd \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set fullnameOverride=kube-dns

kubectl -n kube-system rollout status deployment/kube-dns --timeout=120s

# Lease holder must be set within 60s of rollout
for i in {1..60}; do
  HOLDER=$(kubectl -n kube-system get lease coredns-crd-leader \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "$HOLDER" ]] && break
  sleep 1
done
[[ -n "$HOLDER" ]] || { echo "lease never acquired"; exit 1; }
```

#### Scenario 2: Resolution

```bash
kubectl apply -f config/example/dnsslice.yaml
# Wait for the slice's Ready condition (informer + reconcile cycle)
kubectl wait --for=condition=Ready dnsslice/web --timeout=30s

DNS_IP=$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')

# Run dig from a transient pod
kubectl run dig --image=alpine:3.20 --restart=Never \
  --command -- sh -c "
    apk add --no-cache bind-tools >/dev/null 2>&1
    dig +short @$DNS_IP web.example.com A
    dig +short @$DNS_IP web6.example.com AAAA
    dig +short @$DNS_IP alias.example.com CNAME
    dig +short @$DNS_IP example.com TXT
    dig +short @$DNS_IP _http._tcp.example.com SRV
  " 2>&1 | tee /tmp/dig.out

kubectl wait --for=condition=Ready pod/dig --timeout=60s
kubectl logs dig > /tmp/dig.log
kubectl delete pod dig --wait=false

grep -q '10.0.0.1'                /tmp/dig.log
grep -q '10.0.0.2'                /tmp/dig.log
grep -q '2001:db8::1'             /tmp/dig.log
grep -q 'web.example.com.'        /tmp/dig.log   # CNAME target
grep -q 'v=spf1 -all'             /tmp/dig.log
grep -q '10 100 80'               /tmp/dig.log   # SRV
```

#### Scenario 3: Metrics

```bash
LEADER_IP=$(kubectl -n kube-system get pods -l app.kubernetes.io/name=coredns-crd \
  -o jsonpath='{.items[0].status.podIP}')

kubectl run metrics --image=curlimages/curl:8.10.1 --restart=Never \
  --command -- curl -s "http://$LEADER_IP:9153/metrics"
kubectl wait --for=condition=Ready pod/metrics --timeout=30s || true
kubectl logs metrics > /tmp/metrics.out
kubectl delete pod metrics --wait=false

grep -q '^coredns_crd_lookups_total'             /tmp/metrics.out
grep -q '^coredns_crd_index_records'             /tmp/metrics.out
grep -q '^coredns_crd_is_leader'                 /tmp/metrics.out
grep -q '^coredns_crd_status_patches_total'      /tmp/metrics.out
grep -q '^coredns_crd_active_conflicts'          /tmp/metrics.out
```

#### Scenario 4: Conflict

```bash
kubectl apply -f test/e2e/conflict-slice.yaml

# Wait for the loser slice's Conflicting condition to flip True (≤10s)
for i in {1..10}; do
  COND=$(kubectl get dnsslice e2e-loser \
    -o jsonpath='{.status.conditions[?(@.type=="Conflicting")].status}' 2>/dev/null)
  [[ "$COND" == "True" ]] && break
  sleep 1
done
[[ "$COND" == "True" ]] || { echo "loser never marked Conflicting"; exit 1; }

# Confirm a ConflictDetected Event exists for the loser
kubectl get events --field-selector \
  involvedObject.name=e2e-loser \
  -o jsonpath='{.items[*].reason}' | grep -q ConflictDetected
```

#### Scenario 5: Failover

```bash
LEADER_BEFORE=$(kubectl -n kube-system get lease coredns-crd-leader \
  -o jsonpath='{.spec.holderIdentity}')
kubectl -n kube-system delete pod "$LEADER_BEFORE" \
  --grace-period=0 --force >/dev/null 2>&1

# Surviving pod must claim the lease within 30s
for i in {1..30}; do
  NEW=$(kubectl -n kube-system get lease coredns-crd-leader \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "$NEW" && "$NEW" != "$LEADER_BEFORE" ]] && break
  sleep 1
done
[[ "$NEW" != "$LEADER_BEFORE" ]] || { echo "lease never failed over"; exit 1; }

# DNS still resolves
kubectl run dig2 --image=alpine:3.20 --restart=Never \
  --command -- sh -c "apk add --no-cache bind-tools >/dev/null 2>&1 && dig +short @$DNS_IP web.example.com A"
kubectl wait --for=condition=Ready pod/dig2 --timeout=30s
kubectl logs dig2 | grep -q '10.0.0.'
kubectl delete pod dig2 --wait=false
```

### Conflict slice fixture: `test/e2e/conflict-slice.yaml`

```yaml
apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: e2e-loser
  namespace: default
spec:
  entries:
    - name: web.example.com.
      type: A
      a:
        address: 9.9.9.9
```

(Contests `web.example.com. A` with the example slice's older
arrival, becoming the loser by LWW.)

### Local reproducibility

`test/e2e/README.md`:

```markdown
# E2E

Run the same tests CI runs, against any current-context cluster.

Prereqs:
- `kubectl` configured against a target cluster (KinD, k3s, anything).
- The image `coredns-crd:e2e` loaded into the cluster's runtime:

      docker build -t coredns-crd:e2e .
      kind load docker-image coredns-crd:e2e --name <kind-cluster-name>
      # OR for k3s: docker save coredns-crd:e2e | sudo k3s ctr images import -

Run:

    bash test/e2e/run.sh

Cleanup happens automatically (each scenario tears down its own
helper pods); the final state is a healthy chart install. To reset:

    helm uninstall coredns-crd -n kube-system
    kubectl delete dnsslice --all
    kubectl delete -f deploy/helm/coredns-crd/crds/dnsslice.yaml
```

### Failure modes

| Mode | Behavior |
|-|-|
| KinD action fails to bring up cluster | Workflow fails before Run e2e step; diagnostics step has no cluster to query. |
| Image build fails | Workflow fails at the build step. |
| Helm install fails | Diagnostics step dumps pods + events (Run e2e exits non-zero, `if: failure()` fires). |
| One assertion fails | bash exits with that line's status; diagnostics step runs. |
| Flake in failover (>30s) | Run fails; rerun the workflow. If chronic, raise timeout. |
| `coredns_crd_*` series missing post-rollout | Likely metrics package init order regression; diagnostics shows pod logs. |

## Acceptance criteria

1. `.github/workflows/e2e.yml` runs on every PR and push to main and
   completes in <8 minutes (target ~5).
2. All five scenarios pass on a fresh KinD cluster (validated by at
   least one CI run with the workflow on a sample branch).
3. `bash test/e2e/run.sh` works locally against an existing
   KinD/k3s/minikube cluster with image `coredns-crd:e2e` available.
4. Diagnostics step provides useful triage on failure (pods, events,
   logs).
5. Existing CI gates (unit tests, helm lint, kubeconform) remain green.
