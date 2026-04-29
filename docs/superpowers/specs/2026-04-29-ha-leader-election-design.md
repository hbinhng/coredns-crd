# HA + Leader Election for Status Writes

**Sub-project A** in the production-readiness push.
**Date**: 2026-04-29.
**Status**: Approved (autonomous run; user delegated supervision).

## Problem

`coredns-crd` ships as a Deployment that today runs `replicas: 1`. Cluster
DNS as a single-pod system is a SPoF: a pod restart, eviction, or node drain
takes down resolution for the whole cluster. The fix is `replicas: ≥2` with
node anti-affinity and a PodDisruptionBudget.

Running multiple replicas exposes a correctness issue: every pod's informer
fires events for every DNSSlice, every pod's `statusUpdater.Enqueue`
records the event in its local pending map, and every pod's `Run` loop
calls `Patch` on `/status`. Two pods racing on the same patch will produce
overwrites, lastTransitionTime churn, and observedGeneration ping-pong.
DNS resolution itself is unaffected (each pod has its own in-memory Index
and serves from it), but the status surface — the user's only window into
the system — becomes untrustworthy.

## Goal

Run N replicas safely. Exactly one replica writes `/status` patches at any
time. Leader transition is bounded (~15s worst case) and DNS resolution is
unaffected throughout.

## Non-goals

- Sharding the work across replicas. Status writes are low volume; a single
  leader is sufficient and dramatically simpler.
- Active-active write coordination. Optimistic concurrency on
  `MergePatchType` is awkward and the API traffic scales with replica count.
- Replacing the workqueue. Per-pod queues are kept; only the leader drains.

## Design

### Architecture

Every replica continues to run:

- The dynamic informer over `DNSSlice` resources.
- `internal/index.Index` updated from informer events.
- `Handler.ServeDNS` answering DNS queries from the Index.
- `statusUpdater.Enqueue` recording events into the per-pod pending map.

What changes:

- A new component `internal/leader` runs alongside the plugin and elects
  one replica as the leader via a `coordination.k8s.io/v1.Lease`.
- `statusUpdater.Run` consults a leader predicate before each Patch. When
  the predicate returns false, the queue cycles (`Get` → `Done`) without
  calling `Patch`; pending entries remain in the side map for whoever
  becomes leader.
- On `OnStartedLeading`, the plugin calls `Handler.reconcileAll`, which
  iterates the Index's known slices and enqueues a fresh status snapshot
  for each. This guarantees the new leader's status writes converge to
  current state without depending on whatever the previous leader had
  already done.
- On `OnStoppedLeading`, the predicate flips to false; in-flight `Patch`
  finishes, then drains stop.

### Components

#### `internal/leader/leader.go` (new)

Thin wrapper around `k8s.io/client-go/tools/leaderelection`.

```go
package leader

type Config struct {
    Client          kubernetes.Interface
    LeaseNamespace  string
    LeaseName       string
    Identity        string
    LeaseDuration   time.Duration   // default 15s
    RenewDeadline   time.Duration   // default 10s
    RetryPeriod     time.Duration   // default 2s
    OnStartedLeading func(ctx context.Context)
    OnStoppedLeading func()
    OnNewLeader      func(identity string)
}

type Elector struct { /* ... */ }

func New(cfg Config) (*Elector, error)
func (e *Elector) Run(ctx context.Context) error
func (e *Elector) IsLeader() bool
```

The wrapper exists to (a) narrow the surface area we test against and
(b) give us a stable seam for the in-process predicate the status updater
needs. Without it, status would have to import `client-go/leaderelection`
and tests would need to mock the LeaderElector struct.

#### `plugin/crd/status.go` (modified)

`statusUpdater` gains an `isLeader func() bool` field, set via constructor.

`Run` loop becomes:

```go
for {
    key, shutdown := s.queue.Get()
    if shutdown { return }
    if !s.isLeader() {
        s.queue.Done(key)         // pending stays in side map
        continue
    }
    ev, ok := s.take(key)
    // ... existing process/restore/forget flow
}
```

Existing behavior preserved when `isLeader` returns true. Default for
testing without leader election: a closure returning true (i.e. always
leader, single-replica semantics).

#### `plugin/crd/crd.go` (modified)

Add `(h *Handler) reconcileAll()`:

```go
func (h *Handler) reconcileAll() {
    snaps := h.idx.AllSnapshots()  // new method on Index
    for _, s := range snaps {
        h.statusUpdater.Enqueue(s.Namespace, s.Name, s.Generation, s.Result)
    }
}
```

#### `internal/index/index.go` (modified)

Add `(i *Index) AllSnapshots() []SliceStatus` — read-locked iteration over
`i.slices` calling `i.snapshot` for each.

#### `plugin/crd/setup.go` (modified)

Wire leader election:

1. Read `POD_NAMESPACE` (downward API) — fall back to `kube-system`.
2. Build `kubernetes.Interface` from the same REST config.
3. Construct `leader.New(...)` with callbacks.
4. In `OnStartedLeading`, call `h.reconcileAll()` then return (the elector
   blocks the goroutine until lease is lost; that's how leaderelection
   semantics work — the callback runs as long as you hold the lease).
5. Pass `elector.IsLeader` to `NewStatusUpdater`.
6. Run elector + statusUpdater + informer factory under the same context.

### Configurability

New Corefile block:

```
crd {
    leader_election {
        namespace <ns>      # default: $POD_NAMESPACE or "kube-system"
        lease_name <name>   # default: "coredns-crd-leader"
        disable             # opt-out, every replica is "leader"
    }
}
```

When `disable` is present, the elector is not started; the predicate is a
constant `true`. This preserves the existing single-replica behavior for
local development and minimizes regression risk for users who don't run HA.

### RBAC

New Role + RoleBinding (not ClusterRole — leases are namespaced and we
should be tightly scoped):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: coredns-crd-leader
  namespace: kube-system  # match deploy namespace
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create", "update", "patch"]
```

Bound to the existing `coredns-crd` ServiceAccount.

### Deployment changes

- `replicas: 2` (start), with `topologySpreadConstraints` across nodes (or
  `podAntiAffinity` if running ≥2-node cluster).
- `POD_NAMESPACE` injected via downward API.
- The PDB lives in sub-project D, not here, but the deployment must be
  ready for it (>=2 replicas with proper labels).

## Behavior on transitions

| Event | What happens |
|-|-|
| Cold start, N replicas | Each pod starts informer, populates Index, serves DNS. One pod wins lease (typically <2s), fires `OnStartedLeading`, reconciles all status. |
| Leader pod dies | Lease expires after RenewDeadline (~10s). Surviving pod's `RetryPeriod` poll wins. `OnStartedLeading` re-reconciles from its own Index. Status pause window: ≤15s. |
| Leader pod evicted (graceful) | `ReleaseOnCancel: true` causes immediate lease release on SIGTERM. New leader wins on next RetryPeriod tick (~2s). |
| Network partition | Old leader's `RenewDeadline` fails; it stops thinking it's leader. New pod elects after `LeaseDuration`. Both pods serve DNS throughout. |
| All pods die | DNS down until restart. Leader election does not change this; the SPoF is gone (replicas ≥2) but the cluster could still lose every node. |
| `disable` configured | Every pod thinks it's leader. Status patches race (acceptable: opt-in). |

## Testing

### Unit tests added

- `internal/leader/leader_test.go`: smoke test that `New` rejects bad config
  (zero LeaseDuration, missing Client, etc.) and that `IsLeader` reflects
  the elector's internal state. Most behavior is upstream `client-go`; we
  don't re-test that.

- `plugin/crd/status_test.go` additions:
  - `TestRun_NotLeader_DoesNotPatch` — predicate returns false; Enqueue +
    Run cycle a key; assert 0 patches captured; assert pending entry still
    present.
  - `TestRun_LeaderToggle_DrainsAfterAcquire` — predicate flips false → true
    mid-run; assert pending entries get patched after the flip.

- `plugin/crd/crd_test.go` additions:
  - `TestReconcileAll_EnqueuesEverySlice` — apply 3 slices via `applySlice`,
    drain Calls, then call `h.reconcileAll()`, assert exactly 3 new Calls
    matching the slice keys.

- `internal/index/index_test.go` additions:
  - `TestAllSnapshots_ReturnsEverySlice` — populate 3 slices, assert
    AllSnapshots returns all three with their current snapshots.
  - `TestAllSnapshots_EmptyIndex_ReturnsNil` — edge case.

### Unit test gates

100% line coverage on new code in `internal/leader`, `internal/index`
additions, and the modified Run/reconcileAll paths.

### Integration test

Deferred to sub-project F (CI integration test). The KinD-based test
will: deploy `replicas: 2`, scrape both pods' logs to confirm exactly one
holds the lease, kill the leader pod, assert status writes resume on the
survivor within 30s.

## Failure modes / mitigations

| Mode | Mitigation |
|-|-|
| Lease RBAC missing | Elector returns error on first `Run`; setup.go logs and exits. Pod CrashLoops, signaling misconfigured RBAC. |
| `POD_NAMESPACE` env unset and no Corefile override | Default to `kube-system`. Logged at startup. |
| Lease object exists with stale lease | `leaderelection` handles this — waits for `LeaseDuration` to expire then claims. |
| Two pods believe they're leader simultaneously (clock skew, partition) | Brief window of duplicate writes. K8s API server is the serializer; the second patch lands fine. Cosmetic, not a correctness issue. |
| `disable` set in production | Logged warning at startup. Behavior reverts to today (race-prone status writes). User opt-in. |

## Out of scope (explicit)

- Sharding status writes across replicas (premature; volume is low).
- Custom lease backends (etcd lease, Redis). Use the standard k8s API.
- Active-active without coordination. Already rejected.
- Lease metrics — covered by sub-project B.

## Acceptance criteria

1. `go test -race ./...` passes with the new tests; all new code at 100%
   line coverage.
2. With `replicas: 2`, exactly one Lease holder at any time observable via
   `kubectl get lease -n kube-system coredns-crd-leader -o yaml`.
3. Killing the leader pod causes status writes to resume within 30s on the
   survivor (validated in CI integration test, sub-project F).
4. `disable` flag preserves existing single-replica behavior bit-for-bit.
5. Existing unit tests continue to pass without modification (the `IsLeader`
   default for tests is `func() bool { return true }`).
