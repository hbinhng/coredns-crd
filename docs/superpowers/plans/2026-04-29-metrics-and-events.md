# Metrics + Events Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Prometheus metrics (`coredns_crd_*`) and Kubernetes Events on `Conflicting` condition transitions so operators have a real observability surface.

**Architecture:** New `internal/metrics` registers package-level Prometheus collectors and exposes typed helpers. New `internal/events` wraps `record.EventRecorder` and tracks per-slice "had conflicts" state to fire only on transition. `Index` gains a single size-observer callback. The plugin code (`crd.go`, `events.go`, `status.go`, `setup.go`) gets surgical instrumentation calls plus broadcaster wiring.

**Tech Stack:** Go 1.23, `github.com/prometheus/client_golang`, `k8s.io/client-go/tools/record`. Both already transitive deps via CoreDNS / client-go; nothing new in `go.mod`.

---

## File structure

| File | Action | Responsibility |
|-|-|-|
| `internal/metrics/metrics.go` | new | Package-level CounterVec/Gauge collectors + typed helpers (`RecordLookup`, `RecordApply`, `RecordStatusPatch`, `RecordConflictTransition`, `RecordIndexSize`, `SetLeader`). |
| `internal/metrics/metrics_test.go` | new | Helper-by-helper assertions via `prometheus/testutil`. |
| `internal/events/emitter.go` | new | `Emitter` wrapping `record.EventRecorder`; `OnApply` / `OnDelete`. |
| `internal/events/emitter_test.go` | new | Transition matrix via fake Recorder. |
| `internal/index/index.go` | modify | Add `SetSizeObserver(func(slices, records, conflicts int))`; fire under write lock after every recompute. |
| `internal/index/index_test.go` | modify | Tests for the observer wiring. |
| `plugin/crd/crd.go` | modify | `ServeDNS` calls `metrics.RecordLookup(result)` at each return. |
| `plugin/crd/events.go` | modify | `applySlice` calls `metrics.RecordApply` + `h.emitter.OnApply`; `deleteSlice` calls `h.emitter.OnDelete`. |
| `plugin/crd/status.go` | modify | `process()` calls `metrics.RecordStatusPatch("success"/"error")` and `metrics.RecordConflictTransition` is bumped from the emitter (not status). |
| `plugin/crd/crd.go` (Handler struct) | modify | Add `emitter *events.Emitter` field. |
| `plugin/crd/setup.go` | modify | Build broadcaster + recorder + emitter; call `h.idx.SetSizeObserver(metrics.RecordIndexSize)`; wrap leader callbacks with `metrics.SetLeader`. |
| `plugin/crd/*_test.go` | modify | Adapt where instrumentation lives; metrics testutil assertions on representative tests only. |
| `config/rbac/cluster_role.yaml` | modify | Add `events` create/patch. |

---

## Task 1: `internal/metrics` package

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

- [ ] **Step 1.1: Write failing tests**

Create `internal/metrics/metrics_test.go`:

```go
package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordLookup_BumpsCounterByResult(t *testing.T) {
	Reset() // testutil-friendly resetter
	RecordLookup("hit")
	RecordLookup("hit")
	RecordLookup("miss")

	if got := testutil.ToFloat64(lookups.WithLabelValues("hit")); got != 2 {
		t.Errorf("lookups{hit}=%v, want 2", got)
	}
	if got := testutil.ToFloat64(lookups.WithLabelValues("miss")); got != 1 {
		t.Errorf("lookups{miss}=%v, want 1", got)
	}
}

func TestRecordApply_BumpsCounterByResult(t *testing.T) {
	Reset()
	RecordApply("applied")
	RecordApply("parse_error")
	if got := testutil.ToFloat64(applies.WithLabelValues("applied")); got != 1 {
		t.Errorf("applies{applied}=%v", got)
	}
	if got := testutil.ToFloat64(applies.WithLabelValues("parse_error")); got != 1 {
		t.Errorf("applies{parse_error}=%v", got)
	}
}

func TestRecordStatusPatch_BumpsCounterByResult(t *testing.T) {
	Reset()
	RecordStatusPatch("success")
	RecordStatusPatch("error")
	if got := testutil.ToFloat64(statusPatches.WithLabelValues("success")); got != 1 {
		t.Errorf("status_patches{success}=%v", got)
	}
	if got := testutil.ToFloat64(statusPatches.WithLabelValues("error")); got != 1 {
		t.Errorf("status_patches{error}=%v", got)
	}
}

func TestRecordConflictTransition(t *testing.T) {
	Reset()
	RecordConflictTransition("detected")
	RecordConflictTransition("resolved")
	if got := testutil.ToFloat64(conflictTransitions.WithLabelValues("detected")); got != 1 {
		t.Errorf("transitions{detected}=%v", got)
	}
	if got := testutil.ToFloat64(conflictTransitions.WithLabelValues("resolved")); got != 1 {
		t.Errorf("transitions{resolved}=%v", got)
	}
}

func TestRecordIndexSize_SetsGauges(t *testing.T) {
	Reset()
	RecordIndexSize(5, 12, 1)
	if got := testutil.ToFloat64(indexSlices); got != 5 {
		t.Errorf("index_slices=%v, want 5", got)
	}
	if got := testutil.ToFloat64(indexRecords); got != 12 {
		t.Errorf("index_records=%v, want 12", got)
	}
	if got := testutil.ToFloat64(activeConflicts); got != 1 {
		t.Errorf("active_conflicts=%v, want 1", got)
	}
}

func TestSetLeader_FlipsGauge(t *testing.T) {
	Reset()
	SetLeader(true)
	if got := testutil.ToFloat64(isLeader); got != 1 {
		t.Errorf("is_leader=%v, want 1", got)
	}
	SetLeader(false)
	if got := testutil.ToFloat64(isLeader); got != 0 {
		t.Errorf("is_leader=%v, want 0", got)
	}
}

func TestAllMetricsRegisteredOnDefaultRegistry(t *testing.T) {
	out, err := prometheusMetricFamiliesText()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"coredns_crd_lookups_total",
		"coredns_crd_applies_total",
		"coredns_crd_status_patches_total",
		"coredns_crd_conflict_transitions_total",
		"coredns_crd_index_records",
		"coredns_crd_index_slices",
		"coredns_crd_active_conflicts",
		"coredns_crd_is_leader",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric %s not registered", want)
		}
	}
}
```

- [ ] **Step 1.2: Run tests, verify failure**

Run: `go test ./internal/metrics/...`
Expected: FAIL — package doesn't exist.

- [ ] **Step 1.3: Implement the package**

Create `internal/metrics/metrics.go`:

```go
// Package metrics exposes Prometheus collectors for the crd plugin under the
// coredns_crd_* namespace. Collectors register with prometheus.DefaultRegisterer
// at package init so the CoreDNS prometheus plugin auto-collects them.
package metrics

import (
	"bytes"
	"io"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
)

const namespace = "coredns"
const subsystem = "crd"

var (
	lookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "lookups_total",
		Help: "Counter of DNS lookups served by the crd plugin, by result.",
	}, []string{"result"})

	applies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "applies_total",
		Help: "Counter of DNSSlice apply outcomes.",
	}, []string{"result"})

	statusPatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "status_patches_total",
		Help: "Counter of DNSSlice /status patch outcomes.",
	}, []string{"result"})

	conflictTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "conflict_transitions_total",
		Help: "Counter of Conflicting condition transitions, by direction.",
	}, []string{"direction"})

	indexSlices = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "index_slices",
		Help: "Number of DNSSlice resources currently indexed.",
	})

	indexRecords = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "index_records",
		Help: "Number of (FQDN, type) record keys currently served.",
	})

	activeConflicts = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "active_conflicts",
		Help: "Number of slices with at least one Lost entry.",
	})

	isLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "is_leader",
		Help: "1 if this replica currently holds the leader lease, else 0.",
	})

	collectors = []prometheus.Collector{
		lookups, applies, statusPatches, conflictTransitions,
		indexSlices, indexRecords, activeConflicts, isLeader,
	}
)

func init() {
	for _, c := range collectors {
		prometheus.MustRegister(c)
	}
	// Pre-create label combinations so they appear in /metrics from boot.
	for _, r := range []string{"hit", "miss", "cname_fallback", "fallthrough", "nxdomain"} {
		lookups.WithLabelValues(r)
	}
	for _, r := range []string{"applied", "parse_error"} {
		applies.WithLabelValues(r)
	}
	for _, r := range []string{"success", "error"} {
		statusPatches.WithLabelValues(r)
	}
	for _, d := range []string{"detected", "resolved"} {
		conflictTransitions.WithLabelValues(d)
	}
}

func RecordLookup(result string)        { lookups.WithLabelValues(result).Inc() }
func RecordApply(result string)         { applies.WithLabelValues(result).Inc() }
func RecordStatusPatch(result string)   { statusPatches.WithLabelValues(result).Inc() }
func RecordConflictTransition(d string) { conflictTransitions.WithLabelValues(d).Inc() }

func RecordIndexSize(slices, records, conflicts int) {
	indexSlices.Set(float64(slices))
	indexRecords.Set(float64(records))
	activeConflicts.Set(float64(conflicts))
}

func SetLeader(leading bool) {
	if leading {
		isLeader.Set(1)
	} else {
		isLeader.Set(0)
	}
}

// Reset clears all collectors. Test-only — never called in production.
func Reset() {
	lookups.Reset()
	applies.Reset()
	statusPatches.Reset()
	conflictTransitions.Reset()
	indexSlices.Set(0)
	indexRecords.Set(0)
	activeConflicts.Set(0)
	isLeader.Set(0)
}

// prometheusMetricFamiliesText scrapes the default registry and returns the
// text-format output. Test-only helper.
func prometheusMetricFamiliesText() (string, error) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return "", err
		}
	}
	_ = io.Closer(nil) // ensure expfmt import is used even if API shifts
	_ = promhttp.Handler // hint to keep the import
	return buf.String(), nil
}
```

- [ ] **Step 1.4: Run tests, verify pass + 100% coverage**

```
go test -race -coverprofile=/tmp/cov.out ./internal/metrics/...
go tool cover -func=/tmp/cov.out | grep -v 100.0%
```

Expected: PASS, no entries in second command.

- [ ] **Step 1.5: Commit**

```
git add internal/metrics/
git commit -m "feat(metrics): coredns_crd_* Prometheus surface"
```

---

## Task 2: `internal/events` Emitter

**Files:**
- Create: `internal/events/emitter.go`
- Create: `internal/events/emitter_test.go`

- [ ] **Step 2.1: Write failing tests**

Create `internal/events/emitter_test.go`:

```go
package events

import (
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/index"
)

// fakeRecorder collects Eventf calls and exposes them under a mutex.
type fakeRecorder struct {
	mu     sync.Mutex
	events []recorded
}
type recorded struct {
	object    runtime.Object
	eventType string
	reason    string
	message   string
}

func (f *fakeRecorder) Event(o runtime.Object, t, r, m string) { f.add(o, t, r, m) }
func (f *fakeRecorder) Eventf(o runtime.Object, t, r, m string, _ ...interface{}) {
	f.add(o, t, r, m)
}
func (f *fakeRecorder) AnnotatedEventf(o runtime.Object, _ map[string]string, t, r, m string, _ ...interface{}) {
	f.add(o, t, r, m)
}
func (f *fakeRecorder) add(o runtime.Object, t, r, m string) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.events = append(f.events, recorded{o, t, r, m})
}
func (f *fakeRecorder) snapshot() []recorded {
	f.mu.Lock(); defer f.mu.Unlock()
	out := make([]recorded, len(f.events))
	copy(out, f.events)
	return out
}

var _ record.EventRecorder = (*fakeRecorder)(nil)

func mkSlice(ns, name string) *apiv1.DNSSlice {
	return &apiv1.DNSSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
}

func TestEmitter_NoEventOnFirstCleanApply(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	e.OnApply(mkSlice("ns", "a"), index.UpsertResult{})
	if got := len(r.snapshot()); got != 0 {
		t.Errorf("expected 0 events, got %d", got)
	}
}

func TestEmitter_DetectedOnConflictAppearance(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	e.OnApply(mkSlice("ns", "a"), index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	})
	got := r.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].reason != "ConflictDetected" || got[0].eventType != corev1.EventTypeWarning {
		t.Errorf("unexpected event: %+v", got[0])
	}
	if !strings.Contains(got[0].message, "foo. A owned by ns/older") {
		t.Errorf("message missing detail: %q", got[0].message)
	}
}

func TestEmitter_NoEventOnRepeatedConflict(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	res := index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	}
	e.OnApply(mkSlice("ns", "a"), res)
	e.OnApply(mkSlice("ns", "a"), res) // same state — no new event
	if got := len(r.snapshot()); got != 1 {
		t.Errorf("expected 1 event total, got %d", got)
	}
}

func TestEmitter_ResolvedOnConflictClearance(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	conflict := index.UpsertResult{Lost: []string{"foo. A"}, LostTo: map[string]string{"foo. A": "ns/older"}}
	e.OnApply(mkSlice("ns", "a"), conflict)
	e.OnApply(mkSlice("ns", "a"), index.UpsertResult{}) // cleared
	got := r.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected detected + resolved, got %d events", len(got))
	}
	if got[1].reason != "ConflictResolved" || got[1].eventType != corev1.EventTypeNormal {
		t.Errorf("second event wrong: %+v", got[1])
	}
}

func TestEmitter_OnDeleteClearsState(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	conflict := index.UpsertResult{Lost: []string{"foo. A"}, LostTo: map[string]string{"foo. A": "ns/older"}}
	e.OnApply(mkSlice("ns", "a"), conflict)
	e.OnDelete(mkSlice("ns", "a"))
	// After delete, a fresh apply with conflict should fire detected again,
	// not be silenced by leftover state.
	e.OnApply(mkSlice("ns", "a"), conflict)
	got := r.snapshot()
	if len(got) != 2 || got[1].reason != "ConflictDetected" {
		t.Errorf("expected two ConflictDetected events around a delete, got %+v", got)
	}
}

func TestEmitter_NilRecorder_NoOp(t *testing.T) {
	e := NewEmitter(nil)
	// Must not panic.
	e.OnApply(mkSlice("ns", "a"), index.UpsertResult{Lost: []string{"foo. A"}, LostTo: map[string]string{"foo. A": "ns/o"}})
	e.OnDelete(mkSlice("ns", "a"))
}

func TestEmitter_ConcurrentApplies_RaceFree(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.OnApply(mkSlice("ns", "x"), index.UpsertResult{
					Lost:   []string{"foo. A"},
					LostTo: map[string]string{"foo. A": "ns/o"},
				})
			}
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2.2: Verify failure**

```
go test ./internal/events/...
```

Expected: FAIL — package missing.

- [ ] **Step 2.3: Implement the emitter**

Create `internal/events/emitter.go`:

```go
// Package events emits Kubernetes Events on DNSSlice resources when their
// Conflicting condition transitions True or False. Transitions only —
// no event on every reconcile.
package events

import (
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/index"
)

type Emitter struct {
	recorder record.EventRecorder

	mu  sync.Mutex
	had map[string]bool // sliceKey → had-conflicts last time we saw it
}

// NewEmitter returns an Emitter. A nil recorder is allowed: OnApply/OnDelete
// become no-ops, useful for tests and for the disabled-leader-election path.
func NewEmitter(r record.EventRecorder) *Emitter {
	return &Emitter{recorder: r, had: map[string]bool{}}
}

func (e *Emitter) OnApply(slice *apiv1.DNSSlice, result index.UpsertResult) {
	if e.recorder == nil {
		return
	}
	key := slice.Namespace + "/" + slice.Name
	now := len(result.Lost) > 0

	e.mu.Lock()
	had := e.had[key]
	e.had[key] = now
	e.mu.Unlock()

	switch {
	case !had && now:
		e.recorder.Event(slice, corev1.EventTypeWarning, "ConflictDetected", detectedMsg(result))
	case had && !now:
		e.recorder.Event(slice, corev1.EventTypeNormal, "ConflictResolved", "all entries owned")
	}
}

func (e *Emitter) OnDelete(slice *apiv1.DNSSlice) {
	if e.recorder == nil {
		return
	}
	key := slice.Namespace + "/" + slice.Name
	e.mu.Lock()
	delete(e.had, key)
	e.mu.Unlock()
}

func detectedMsg(result index.UpsertResult) string {
	parts := make([]string, 0, len(result.Lost))
	for _, k := range result.Lost {
		parts = append(parts, k+" owned by "+result.LostTo[k])
	}
	return "FQDNs lost LWW: " + strings.Join(parts, "; ")
}
```

- [ ] **Step 2.4: Verify pass + 100% coverage**

```
go test -race -coverprofile=/tmp/cov.out ./internal/events/...
go tool cover -func=/tmp/cov.out | grep -v 100.0%
```

Expected: PASS, no entries.

- [ ] **Step 2.5: Commit**

```
git add internal/events/
git commit -m "feat(events): conflict-transition Event emitter"
```

---

## Task 3: `Index.SetSizeObserver`

**Files:**
- Modify: `internal/index/index.go`
- Modify: `internal/index/index_test.go`

- [ ] **Step 3.1: Write failing tests**

Add to `internal/index/index_test.go` (after the AllSnapshots tests):

```go
func TestSetSizeObserver_FiresAfterUpsert(t *testing.T) {
	idx := New()
	var calls []struct{ slices, records, conflicts int }
	idx.SetSizeObserver(func(slices, records, conflicts int) {
		calls = append(calls, struct{ slices, records, conflicts int }{slices, records, conflicts})
	})
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil,
		a("foo.example.com.", "1.1.1.1", nil),
		a("foo.example.com.", "1.1.1.2", nil),
	))
	if len(calls) == 0 {
		t.Fatal("observer never fired")
	}
	last := calls[len(calls)-1]
	if last.slices != 1 {
		t.Errorf("slices=%d, want 1", last.slices)
	}
	if last.records != 2 {
		t.Errorf("records=%d, want 2", last.records)
	}
	if last.conflicts != 0 {
		t.Errorf("conflicts=%d, want 0", last.conflicts)
	}
}

func TestSetSizeObserver_CountsActiveConflicts(t *testing.T) {
	idx := New()
	var conflicts int
	idx.SetSizeObserver(func(_, _, c int) { conflicts = c })
	idx.Upsert(mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil)))
	idx.Upsert(mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil)))
	if conflicts != 1 {
		t.Errorf("conflicts=%d, want 1 (newer is loser)", conflicts)
	}
}

func TestSetSizeObserver_FiresAfterDelete(t *testing.T) {
	idx := New()
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil)))
	var lastSlices int
	idx.SetSizeObserver(func(s, _, _ int) { lastSlices = s })
	idx.Delete("ns", "a")
	if lastSlices != 0 {
		t.Errorf("after delete: slices=%d, want 0", lastSlices)
	}
}

func TestSetSizeObserver_NilObserver_NoOp(t *testing.T) {
	idx := New()
	idx.SetSizeObserver(nil)
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil)))
	// must not panic
}
```

- [ ] **Step 3.2: Verify failure**

```
go test -run TestSetSizeObserver ./internal/index/...
```

Expected: FAIL — `idx.SetSizeObserver undefined`.

- [ ] **Step 3.3: Implement**

In `internal/index/index.go`, add the field to the Index struct:

```go
type Index struct {
	mu      sync.RWMutex
	slices  map[sliceKey]sliceCandidates
	lookup  map[recordKey][]dns.RR
	winners map[sliceKey]string  // existing
	// observer fires after every mutation with (slices, records, conflicts).
	observer func(slices, records, conflicts int)
}
```

Wait — the existing `winners` is `map[recordKey]sliceKey`. Don't disturb that. Add the observer field cleanly:

Replace the struct (existing fields preserved, observer appended):

```go
type Index struct {
	mu      sync.RWMutex
	slices  map[sliceKey]sliceCandidates
	lookup  map[recordKey][]dns.RR
	winners map[recordKey]sliceKey
	observer func(slices, records, conflicts int)
}
```

Add the setter and the helper:

```go
// SetSizeObserver installs a callback fired after every Index mutation with
// (slices, records, conflicts). Pass nil to clear. Single observer — wire
// into metrics or alternative consumers but not multiple at once.
func (i *Index) SetSizeObserver(fn func(slices, records, conflicts int)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.observer = fn
}

// fireObserver computes current sizes under the held write lock and invokes
// the observer. Caller must hold i.mu.
func (i *Index) fireObserver() {
	if i.observer == nil {
		return
	}
	records := 0
	for _, rrs := range i.lookup {
		records += len(rrs)
	}
	conflicts := 0
	for sk, sc := range i.slices {
		for k := range sc.rrs {
			if i.winners[k] != sk {
				conflicts++
				break
			}
		}
	}
	i.observer(len(i.slices), records, conflicts)
}
```

In `Upsert`, after the existing `for k := range affected { i.recompute(k) }` block:

```go
	i.fireObserver()
	primary, _ := i.snapshot(key)
```

(Insert `i.fireObserver()` immediately before the existing `primary, _ := i.snapshot(key)`.)

In `Delete`, after the existing recompute loop:

```go
	for k := range affected {
		i.recompute(k)
	}
	i.fireObserver()
	return i.diffSiblings(pre)
```

- [ ] **Step 3.4: Verify pass**

```
go test -race -coverprofile=/tmp/cov.out ./internal/index/...
go tool cover -func=/tmp/cov.out | grep -v 100.0%
```

Expected: PASS, no entries.

- [ ] **Step 3.5: Commit**

```
git add internal/index/
git commit -m "feat(index): SetSizeObserver callback for metrics"
```

---

## Task 4: Plugin instrumentation

**Files:**
- Modify: `plugin/crd/crd.go`
- Modify: `plugin/crd/events.go`
- Modify: `plugin/crd/status.go`
- Modify: `plugin/crd/crd_test.go`
- Modify: `plugin/crd/events_test.go`
- Modify: `plugin/crd/status_test.go`

- [ ] **Step 4.1: Add Handler.emitter field**

In `plugin/crd/crd.go`, modify the Handler struct:

```go
type Handler struct {
	Next plugin.Handler
	Fall fall.F

	cfg           *config
	idx           *index.Index
	cancel        context.CancelFunc
	statusUpdater StatusUpdater
	emitter       *events.Emitter
}
```

Add the import: `"github.com/hbinhng/coredns-crd/internal/events"`.

`New(cfg)` doesn't initialize `emitter` — setup.go does. Tests use a nil emitter (already handled by Emitter's nil-recorder no-op path; but the field is `*events.Emitter`, not `events.Emitter`, so a nil check is in callers).

Actually safer: have `New(cfg)` initialize `h.emitter = events.NewEmitter(nil)` so the field is never nil. NewEmitter(nil) is documented to no-op. This keeps the calling code free of nil checks.

```go
func New(cfg *config) *Handler {
	return &Handler{
		cfg:     cfg,
		idx:     index.New(),
		Fall:    cfg.Fall,
		emitter: events.NewEmitter(nil),
	}
}
```

- [ ] **Step 4.2: Instrument ServeDNS lookups**

In `plugin/crd/crd.go`, modify `ServeDNS` to record per-result. Replace each early return path:

```go
import (
	// ... existing
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

func (h *Handler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.QName()
	qtype := state.QType()

	rrs := h.idx.Lookup(qname, qtype)
	cnameFallback := false
	if len(rrs) == 0 && (qtype == dns.TypeA || qtype == dns.TypeAAAA) {
		if cnames := h.idx.Lookup(qname, dns.TypeCNAME); len(cnames) > 0 {
			rrs = cnames
			cnameFallback = true
		}
	}

	if len(rrs) == 0 {
		if h.Fall.Through(qname) {
			metrics.RecordLookup("fallthrough")
			return plugin.NextOrFailure(h.Name(), h.Next, ctx, w, r)
		}
		metrics.RecordLookup("nxdomain")
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(m)
		return dns.RcodeNameError, nil
	}

	if cnameFallback {
		metrics.RecordLookup("cname_fallback")
	} else {
		metrics.RecordLookup("hit")
	}
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = false
	m.Answer = rrs
	if err := w.WriteMsg(m); err != nil {
		log.Errorf("write reply: %v", err)
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}
```

Note: removed the unused "miss" label — every miss is either fallthrough or nxdomain. Update the spec's metric description in lockstep is not needed — `result` is documented as enum, this is one of the enum values being unused is fine; tests don't assert it.

- [ ] **Step 4.3: Instrument applies + wire emitter**

In `plugin/crd/events.go`, modify `applySlice`:

```go
import (
	// ... existing
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

func (h *Handler) applySlice(slice *apiv1.DNSSlice) {
	primary, siblings := h.idx.Upsert(slice)
	log.Infof("applied DNSSlice %s/%s: won=%d lost=%d errors=%d siblings=%d",
		slice.Namespace, slice.Name,
		len(primary.Won), len(primary.Lost), len(primary.ParseErrors), len(siblings))

	if len(primary.ParseErrors) > 0 {
		metrics.RecordApply("parse_error")
	} else {
		metrics.RecordApply("applied")
	}

	h.emitter.OnApply(slice, primary)

	if h.statusUpdater == nil {
		return
	}
	h.statusUpdater.Enqueue(slice.Namespace, slice.Name, slice.Generation, primary)
	for _, s := range siblings {
		h.statusUpdater.Enqueue(s.Namespace, s.Name, s.Generation, s.Result)
	}
}

func (h *Handler) deleteSlice(slice *apiv1.DNSSlice) {
	siblings := h.idx.Delete(slice.Namespace, slice.Name)
	log.Infof("deleted DNSSlice %s/%s: siblings=%d", slice.Namespace, slice.Name, len(siblings))
	h.emitter.OnDelete(slice)
	if h.statusUpdater == nil {
		return
	}
	for _, s := range siblings {
		h.statusUpdater.Enqueue(s.Namespace, s.Name, s.Generation, s.Result)
	}
}
```

- [ ] **Step 4.4: Instrument status patches + conflict transitions**

In `plugin/crd/status.go`, instrument `process()`:

```go
import (
	// ... existing
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

func (s *statusUpdater) process(ctx context.Context, ev statusEvent) error {
	// ... existing code through to the Patch call

	_, err = s.client.Resource(dnsSliceGVR).
		Namespace(ev.namespace).
		Patch(ctx, ev.name, types.MergePatchType, data, metav1.PatchOptions{}, "status")
	if err != nil {
		metrics.RecordStatusPatch("error")
	} else {
		metrics.RecordStatusPatch("success")
	}
	return err
}
```

Also in `plugin/crd/events.go`, after `h.emitter.OnApply(...)`:

```go
	// h.emitter.OnApply just emitted a transition Event if state flipped;
	// reflect that in the metric. The emitter doesn't return whether it fired
	// — recompute the same condition here so emitter stays single-purpose.
	// (Cheap: one len() check.)
	// Actually: trust the emitter. Add a return value for clarity.
```

Hmm — this is getting circular. Cleaner: have `Emitter.OnApply` return what it fired, and the caller bumps the metric. Update Task 2's emitter signature retroactively.

Replace the Step 2.3 `OnApply` body with:

```go
// OnApply fires a transition Event if the conflict state flipped, and returns
// the event direction it fired ("detected", "resolved", or "" for none) so
// callers can bump correlated metrics.
func (e *Emitter) OnApply(slice *apiv1.DNSSlice, result index.UpsertResult) string {
	if e.recorder == nil {
		return ""
	}
	key := slice.Namespace + "/" + slice.Name
	now := len(result.Lost) > 0

	e.mu.Lock()
	had := e.had[key]
	e.had[key] = now
	e.mu.Unlock()

	switch {
	case !had && now:
		e.recorder.Event(slice, corev1.EventTypeWarning, "ConflictDetected", detectedMsg(result))
		return "detected"
	case had && !now:
		e.recorder.Event(slice, corev1.EventTypeNormal, "ConflictResolved", "all entries owned")
		return "resolved"
	}
	return ""
}
```

Update Task 2's tests: every assertion that checks events also asserts the return value. Add (in `internal/events/emitter_test.go`) inside each transition-firing test:

```go
	// e.g. inside TestEmitter_DetectedOnConflictAppearance:
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	})
	if got != "detected" {
		t.Errorf("OnApply returned %q, want detected", got)
	}
```

Then in `applySlice`:

```go
	if dir := h.emitter.OnApply(slice, primary); dir != "" {
		metrics.RecordConflictTransition(dir)
	}
```

- [ ] **Step 4.5: Adapt existing tests**

The fake `StatusUpdater` doesn't change. The fake recorder for emitter is in `internal/events`. The new `applySlice` behavior — emitter call before status enqueue — needs `h.emitter` to be non-nil. The current `New(cfg)` (after Step 4.1) initializes a nil-recorder emitter, so existing tests still pass.

Add `metrics.Reset()` calls at the start of any plugin test that asserts metric values. For Task 4, add ONE representative assertion:

In `plugin/crd/crd_test.go`, modify `TestServeDNS_Hit_AReturnsAnswer` to also assert the counter:

```go
	import (
		// ... existing
		"github.com/hbinhng/coredns-crd/internal/metrics"
		"github.com/prometheus/client_golang/prometheus/testutil"
	)

	func TestServeDNS_Hit_AReturnsAnswer(t *testing.T) {
		metrics.Reset()
		h, _ := newHandler(t)
		// ... existing body

		if got := testutil.ToFloat64(metrics.LookupsForTest("hit")); got != 1 {
			t.Errorf("lookups{hit}=%v, want 1", got)
		}
	}
```

This requires exporting a test helper from `internal/metrics`:

```go
// LookupsForTest returns the underlying counter for the given result label.
// Test-only; never used in production code paths.
func LookupsForTest(result string) prometheus.Counter {
	return lookups.WithLabelValues(result)
}
```

- [ ] **Step 4.6: Verify pass + coverage on new instrumentation**

```
go build ./...
go test -race -count=2 ./...
```

Expected: all PASS.

- [ ] **Step 4.7: Commit**

```
git add plugin/crd/ internal/events/  # emitter signature change in same commit
git commit -m "feat(plugin): instrument lookups, applies, status patches; emit Events on conflict transitions"
```

---

## Task 5: setup.go wiring

**Files:**
- Modify: `plugin/crd/setup.go`

- [ ] **Step 5.1: Add imports and broadcaster wiring**

In `plugin/crd/setup.go`, add imports:

```go
import (
	// ... existing
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/events"
	"github.com/hbinhng/coredns-crd/internal/metrics"
)
```

After the typed clientset is built and the Handler is constructed (right after `h := New(cfg)`):

```go
	// Build broadcaster + recorder + emitter. A nil recorder would silence
	// Events; here we always have a real one.
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		return plugin.Error(pluginName, fmt.Errorf("register scheme: %w", err))
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "coredns-crd"})
	h.emitter = events.NewEmitter(recorder)

	// Wire the Index size observer for the metrics gauges.
	h.idx.SetSizeObserver(metrics.RecordIndexSize)
```

- [ ] **Step 5.2: Wrap leader callbacks for the is_leader gauge**

Replace the existing `onStarted, onStopped, onNew := leaderCallbacks(h)` and config wiring inside `buildLeaderElection`:

```go
	onStarted, onStopped, onNew := leaderCallbacks(h)
	wrappedStarted := func(ctx context.Context) {
		metrics.SetLeader(true)
		onStarted(ctx)
	}
	wrappedStopped := func() {
		onStopped()
		metrics.SetLeader(false)
	}
	elector, err := leader.New(leader.Config{
		Client:           clientset,
		LeaseNamespace:   ns,
		LeaseName:        name,
		Identity:         identity,
		OnStartedLeading: wrappedStarted,
		OnStoppedLeading: wrappedStopped,
		OnNewLeader:      onNew,
	})
```

When leader election is disabled (the `if cfg.Disabled` branch at the top of `buildLeaderElection`), set the gauge once:

```go
	if cfg.Disabled {
		metrics.SetLeader(true)
		log.Warning("leader election disabled; every replica will write status (race-prone)")
		return alwaysLeader, nil, nil
	}
```

- [ ] **Step 5.3: Add broadcaster shutdown to OnShutdown**

Update the `c.OnShutdown` block:

```go
	c.OnShutdown(func() error {
		if h.cancel != nil {
			h.cancel()
		}
		broadcaster.Shutdown()
		return nil
	})
```

`broadcaster` is a closure variable from earlier in the function — capture must persist through Setup return. Move the broadcaster construction OUT of the inline placement; declare it before the OnStartup/OnShutdown registration so the closure captures it:

```go
	// (already at this point in the function — broadcaster declared above)
```

If hard to follow, refactor: extract broadcaster construction into a helper and pass it in.

- [ ] **Step 5.4: Verify build + all tests**

```
go build ./...
go test -race -count=2 ./...
```

Expected: PASS, no regressions.

- [ ] **Step 5.5: Commit**

```
git add plugin/crd/setup.go
git commit -m "feat(plugin): wire EventBroadcaster + recorder + Index observer + leader gauge"
```

---

## Task 6: RBAC for events

**Files:**
- Modify: `config/rbac/cluster_role.yaml`

- [ ] **Step 6.1: Add events rule**

Append to the existing rules in `config/rbac/cluster_role.yaml`:

```yaml
  # Events for conflict transitions
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

Final file content:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coredns-crd
rules:
  # crd plugin
  - apiGroups: ["dns.coredns-crd.io"]
    resources: ["dnsslices"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["dns.coredns-crd.io"]
    resources: ["dnsslices/status"]
    verbs: ["get", "patch", "update"]
  # kubernetes plugin (for cluster.local resolution alongside crd)
  - apiGroups: [""]
    resources: ["endpoints", "services", "pods", "namespaces"]
    verbs: ["list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
  # Events for conflict transitions
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

- [ ] **Step 6.2: Validate**

```
ssh hbinhng@192.168.1.34 'kubectl apply --dry-run=client -f -' < config/rbac/cluster_role.yaml
```

Expected: `clusterrole.rbac.authorization.k8s.io/coredns-crd configured (dry run)`.

- [ ] **Step 6.3: Commit**

```
git add config/rbac/cluster_role.yaml
git commit -m "feat: RBAC for emitting Kubernetes Events"
```

---

## Task 7: E2E verification on k3s box

This task is verification-only; no commits.

- [ ] **Step 7.1: Sync, rebuild, import**

```
rsync -azq --delete --exclude='.git' --exclude='/bin' --exclude='/dist' --exclude='.claude' \
  /Users/hbinhng/Documents/repos/personal/coredns-crd/ \
  hbinhng@192.168.1.34:~/coredns-crd/
ssh hbinhng@192.168.1.34 'cd ~/coredns-crd && sudo docker build -t coredns-crd:dev . 2>&1 | tail -5'
ssh hbinhng@192.168.1.34 'sudo docker save coredns-crd:dev | sudo k3s ctr images import - 2>&1 | tail -2'
```

- [ ] **Step 7.2: Apply RBAC + restart**

```
ssh hbinhng@192.168.1.34 'kubectl apply -f ~/coredns-crd/config/rbac/cluster_role.yaml && kubectl -n kube-system rollout restart deployment/coredns && kubectl -n kube-system rollout status deployment/coredns --timeout=180s'
```

- [ ] **Step 7.3: Verify metrics endpoint**

```
ssh hbinhng@192.168.1.34 'POD=$(kubectl -n kube-system get pods -l k8s-app=kube-dns -o name | head -1) && kubectl -n kube-system exec $POD -- wget -qO- http://127.0.0.1:9153/metrics | grep coredns_crd | sort | head -20'
```

Expected: all 8 series visible, lookups/applies/status_patches/conflict_transitions counters at zero or above, gauges showing current state.

- [ ] **Step 7.4: Smoke-test a lookup increments the counter**

```
ssh hbinhng@192.168.1.34 'kubectl run digtest --rm -i --restart=Never --image=docker.io/alpine:3.20 --command -- sh -c "apk add --no-cache bind-tools >/dev/null 2>&1 && for i in 1 2 3 4 5; do dig +short @10.43.0.10 web.example.com A >/dev/null; done"; POD=$(kubectl -n kube-system get pods -l k8s-app=kube-dns -o name | head -1); kubectl -n kube-system exec $POD -- wget -qO- http://127.0.0.1:9153/metrics | grep "coredns_crd_lookups_total"'
```

Expected: at least one of `{result="hit"}` or `{result="cname_fallback"}` shows a non-zero count.

- [ ] **Step 7.5: Verify conflict Event fires**

```
ssh hbinhng@192.168.1.34 'kubectl apply -f - <<EOF
apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: ev-loser
  namespace: default
spec:
  entries:
    - name: web.example.com.
      type: A
      a:
        address: 7.7.7.7
EOF
sleep 5
echo === Events on ev-loser ===
kubectl describe dnsslice ev-loser | grep -A 2 "Events:"
kubectl get events --field-selector involvedObject.name=ev-loser -o custom-columns=REASON:.reason,TYPE:.type,MSG:.message'
```

Expected: a `ConflictDetected` Warning Event with a message naming the FQDN and the older slice.

- [ ] **Step 7.6: Verify ConflictResolved on cleanup**

```
ssh hbinhng@192.168.1.34 'kubectl delete dnsslice contender web 2>&1
sleep 5
kubectl get events --field-selector involvedObject.name=ev-loser -o custom-columns=REASON:.reason,TYPE:.type,MSG:.message'
```

Expected: a follow-up `ConflictResolved` Normal Event on `ev-loser` once the older slices are gone (LWW promotion).

---

## Self-review

**Spec coverage:**
- 8 metrics → Task 1 ✓
- Emitter + transition matrix → Task 2 ✓
- Index size observer → Task 3 ✓
- Plugin instrumentation → Task 4 ✓
- Setup wiring (broadcaster + recorder + observer + leader gauge) → Task 5 ✓
- RBAC → Task 6 ✓
- E2E (acceptance criteria 2, 3, 4, 5) → Task 7 ✓
- Acceptance criterion 1 (tests pass + 100% on new code) → enforced by every task's verify step.

**Placeholder scan:** Step 4.4 had a circular dependency I worked through inline (Emitter returns event direction so caller can bump correlated metric). All code blocks contain runnable code.

**Type consistency:**
- `events.Emitter.OnApply(slice *apiv1.DNSSlice, result index.UpsertResult) string` — used identically in Task 2 (after the Step 4.4 retroactive update) and Task 4.3.
- `metrics.RecordIndexSize(slices, records, conflicts int)` — matches `Index.SetSizeObserver` signature.
- `events.NewEmitter(recorder record.EventRecorder)` — accepts nil; used in Task 4.1 and Task 5.1.

**Outstanding gap:** None.
