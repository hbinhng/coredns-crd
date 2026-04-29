# Prometheus Metrics + Conflict Events

**Sub-project B** in the production-readiness push.
**Date**: 2026-04-29.
**Status**: Approved (autonomous run; user delegated supervision).

## Problem

Two related observability gaps from the production-readiness assessment:

1. **No metrics surface.** The plugin emits zero Prometheus metrics. Operators
   running `coredns-crd` as cluster DNS have no way to ask "is the plugin
   healthy?", "how many lookups per second?", "is anything conflicting?", or
   "are status patches succeeding?" without grepping logs. Standard CoreDNS
   plugins (cache, kubernetes, forward, hosts) all expose `coredns_<plugin>_*`
   counters/gauges and the `prometheus` plugin auto-scrapes them.

2. **Conflicts surface only in `kubectl get`.** When a `DNSSlice` loses LWW
   arbitration, the slice's `.status.conditions[Conflicting]` flips to True.
   That's correct but invisible to anyone not running `kubectl get
   dnsslice`. Operationally, conflicts deserve a Kubernetes Event so
   `kubectl describe` shows a timeline and any Event-watching tool (Argo
   CD, Datadog, etc.) picks them up automatically.

## Goal

Ship a complete observability surface in one push:

- Prometheus metrics for lookups, applies, status patches, conflict
  transitions, index size, active conflicts, and leader state.
- Kubernetes Events on `DNSSlice` resources when their `Conflicting`
  condition transitions True or False (not on every reconcile).

## Non-goals

- Latency histograms (no SLO defined yet; premature).
- Per-record-type breakdowns of index size (single number is enough at
  this scale).
- An EventBroadcaster shared across multiple plugins (this plugin owns
  its broadcaster lifecycle; standard pattern).
- Alert rule manifests (one PrometheusRule with `active_conflicts > 0`
  belongs in the Helm chart in sub-project C, not here).

## Design

### Metrics

All metrics use the `coredns_crd_` prefix and are registered with the
package-level `prometheus.DefaultRegisterer`. CoreDNS's `prometheus`
plugin auto-collects from this registry, so wiring is free.

| Metric | Type | Labels | When updated |
|-|-|-|-|
| `lookups_total` | CounterVec | `result` | Once per `ServeDNS` call. `result` ∈ {hit, miss, cname_fallback, fallthrough, nxdomain}. |
| `applies_total` | CounterVec | `result` | Once per `applySlice`. `result` ∈ {applied, parse_error}. |
| `status_patches_total` | CounterVec | `result` | Once per `statusUpdater.process()` outcome. `result` ∈ {success, error}. |
| `conflict_transitions_total` | CounterVec | `direction` | Once per Event emission. `direction` ∈ {detected, resolved}. |
| `index_records` | Gauge | (none) | After every Index mutation, via `SetSizeObserver` callback. |
| `index_slices` | Gauge | (none) | Same callback. |
| `active_conflicts` | Gauge | (none) | Same callback (counts slices with non-empty Lost). |
| `is_leader` | Gauge | (none) | 0 or 1; flipped by leader callbacks. |

Conventions:
- No `server` label. We run one server per pod; the `prometheus` plugin's
  existing labels (`server`, `zone`) provide adjacent context.
- All counters initialized at zero so they appear in `/metrics` from boot
  (Prometheus best practice — avoids "absent" alerts on cold start).
- Counters use `*WithLabelValues(...)` not `*With(prometheus.Labels{...})`
  to avoid runtime allocation on the hot path.

The `internal/metrics` package exposes typed helpers
(`RecordLookup(result string)`, `RecordApply(result string)`,
`SetLeader(bool)`, `RecordIndexSize(slices, records, conflicts int)`,
…) so call sites stay terse and the prometheus types don't leak.

### Events

A new `internal/events.Emitter` wraps `record.EventRecorder` and tracks
per-slice "had conflicts last time we saw it" in a small
`map[sliceKey]bool` guarded by a `sync.Mutex`. Pseudocode:

```go
func (e *Emitter) OnApply(slice *DNSSlice, result UpsertResult) {
    e.mu.Lock(); defer e.mu.Unlock()
    had := e.hadConflicts[key(slice)]
    now := len(result.Lost) > 0
    e.hadConflicts[key(slice)] = now
    switch {
    case !had && now: e.fire(slice, "ConflictDetected", "Warning", detectedMsg(result))
    case had && !now: e.fire(slice, "ConflictResolved", "Normal", "all entries owned")
    }
}

func (e *Emitter) OnDelete(slice *DNSSlice) { delete(e.hadConflicts, key(slice)) }
```

Events fire only on transition (false→true or true→false). Re-applies
that don't change conflict state are silent.

Event message format:
- `ConflictDetected`: `"FQDNs lost LWW: foo.example.com. A owned by ns/older; ..."` (semicolon-joined per lost key).
- `ConflictResolved`: `"all entries owned"` (matches the condition message).

`Reason` is the keyword (`ConflictDetected` / `ConflictResolved`).
`Type` is `Warning` for detected, `Normal` for resolved. `involvedObject`
is the `DNSSlice`. The recorder is constructed with `Component:
"coredns-crd"`; Kubernetes API server dedupes Events with same
`(involvedObject, reason, message)` within ~10 minutes by incrementing a
`count` field, so we don't try to dedupe on the client side.

### Architecture

```
internal/metrics/
  metrics.go        # package-level prometheus collectors + helpers
  metrics_test.go   # registry round-trip, helper-by-helper

internal/events/
  emitter.go        # EventEmitter (Recorder + per-slice state map)
  emitter_test.go   # transition matrix via fake Recorder

internal/index/
  index.go          # add (i *Index) SetSizeObserver(func(slices, records, conflicts int))
                    # observer fired under write lock after recompute
  index_test.go     # observer wiring + value correctness

plugin/crd/
  crd.go            # ServeDNS calls metrics.RecordLookup
  events.go         # applySlice calls metrics.RecordApply + emitter.OnApply
                    # deleteSlice calls emitter.OnDelete
  status.go         # process() calls metrics.RecordStatusPatch
  setup.go          # builds broadcaster, recorder, emitter
                    # wraps leader callbacks with metrics.SetLeader
                    # wires Index size observer to metrics.RecordIndexSize
  *_test.go         # adapted where instrumentation lives, tests stay focused

config/rbac/
  cluster_role.yaml # add events create/patch
```

### Lifecycle wiring in setup.go

```go
// Build broadcaster + recorder once (shared across the pod's lifetime).
scheme := runtime.NewScheme()
_ = apiv1.AddToScheme(scheme)
broadcaster := record.NewBroadcaster()
broadcaster.StartStructuredLogging(0)
broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
    Interface: clientset.CoreV1().Events(""),
})
recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "coredns-crd"})
h.emitter = events.NewEmitter(recorder)

// Index size observer.
h.idx.SetSizeObserver(metrics.RecordIndexSize)

// Wrap the leader callbacks so we update the is_leader gauge.
onStarted, onStopped, onNew := leaderCallbacks(h)
wrappedStarted := func(ctx context.Context) {
    metrics.SetLeader(true)
    onStarted(ctx)
}
wrappedStopped := func() {
    onStopped()
    metrics.SetLeader(false)
}

// Shutdown.
c.OnShutdown(func() error {
    if h.cancel != nil { h.cancel() }
    broadcaster.Shutdown()  // flushes pending events
    return nil
})
```

When leader election is `disable`d, `metrics.SetLeader(true)` is called
once in `buildLeaderElection` (matching the always-leader semantics) so
the gauge stays at 1.

### RBAC

The existing `coredns-crd` ClusterRole gains:

```yaml
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
```

Cluster-scoped (not Role) because Events are emitted into the namespace
of the involved object — DNSSlice can live anywhere.

### Failure modes

| Mode | Behavior |
|-|-|
| Recorder backend (API server) unreachable | client-go's broadcaster retries internally; Events queue in memory then drop on overflow. Plugin keeps running. |
| Metrics registration collision | `init()`-time panic with clear message; the test suite catches this. |
| Index observer not set | All Index mutations skip the observer call; gauges stay at zero. Documented behavior, observed in tests. |
| Slice deleted while emitter has stale state | `OnDelete` clears the entry; no leak. |
| Recorder nil (test mode) | Emitter no-ops gracefully — same nil-check pattern as `statusUpdater`. |

### Testing

**Unit tests added (target 100% on new code):**

- `internal/metrics`:
  - Register-once-per-package-init guarantee.
  - Each helper writes the expected counter/gauge.
  - Snapshot-style assertions using `prometheus/client_model` testutil.
- `internal/events`:
  - Full transition matrix (table-driven).
  - Concurrent OnApply for the same key is safe (mutex coverage).
  - OnDelete clears state.
  - Nil recorder no-ops.
- `internal/index`:
  - SetSizeObserver fires after Upsert/Delete with correct counts.
  - No fire when observer is nil.
- `plugin/crd`:
  - Existing tests adapted; new instrumentation tests where they're not
    already implicit (e.g., metrics.RecordLookup is called for hit/miss
    paths via existing ServeDNS tests; we add a counter snapshot
    assertion on one representative test rather than touching all).

**E2E (manual today, CI in sub-project F):**

- After deploy, `dig` a known FQDN; `curl :9153/metrics | grep
  coredns_crd_lookups_total` shows >0.
- Apply two conflicting DNSSlices; `kubectl describe dnsslice <loser>`
  shows `ConflictDetected` Event; resolve by deleting winner;
  `ConflictResolved` Event appears.

## Acceptance criteria

1. `go test -race ./...` passes; new packages at 100% line coverage.
2. After deploying with `prometheus` plugin chained, `/metrics` shows
   all 8 `coredns_crd_*` series at boot (zero values OK).
3. Issuing 100 lookups bumps `coredns_crd_lookups_total{result="hit"}`
   by ~100 (race-tolerant; allow ±5%).
4. Creating a conflicting `DNSSlice` produces exactly one
   `ConflictDetected` Event on the loser within 5 seconds; deleting the
   winner produces exactly one `ConflictResolved`.
5. With `replicas: 2`, both pods emit Events for their respective
   processing. Kubernetes-side dedup (count++) handles duplicates;
   `kubectl describe` shows a single Event with `count: 2` (or so).
6. Existing unit tests continue to pass without modification.
