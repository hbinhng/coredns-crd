# HA + Leader Election Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run `coredns-crd` with `replicas ≥ 2` safely; exactly one replica writes `/status` patches at a time via a `coordination.k8s.io/v1.Lease`.

**Architecture:** Every pod keeps its informer + Index (so DNS resolution works on every replica). A new `internal/leader` package wraps `client-go/tools/leaderelection` and exposes `IsLeader() bool`. `statusUpdater.Run` consults the predicate before each Patch; non-leaders cycle the queue without writing. On lease acquisition, the leader does a one-shot reconcile sweep over all known slices to publish current state.

**Tech Stack:** Go 1.23, `k8s.io/client-go/tools/leaderelection`, `k8s.io/client-go/kubernetes`, `k8s.io/api/coordination/v1`. No new third-party deps beyond client-go (already a transitive dep).

---

## File structure

| File | Action | Responsibility |
|-|-|-|
| `internal/leader/leader.go` | new | Wrapper over client-go leader election. `Config`, `Elector`, `New()`, `Run()`, `IsLeader()`. |
| `internal/leader/leader_test.go` | new | Validate Config, IsLeader-reflects-state, smoke. |
| `internal/index/index.go` | modify | Add `(i *Index) AllSnapshots() []SliceStatus`. |
| `internal/index/index_test.go` | modify | Tests for `AllSnapshots`. |
| `plugin/crd/status.go` | modify | `statusUpdater` gains `isLeader func() bool`; `Run` gates each iteration. |
| `plugin/crd/status_test.go` | modify | Existing callers updated; new gating tests. |
| `plugin/crd/crd.go` | modify | Add `(h *Handler) reconcileAll()`. |
| `plugin/crd/crd_test.go` | modify | Test `reconcileAll`. |
| `plugin/crd/events_test.go` | modify | `newHandler` helper passes `func() bool { return true }`. |
| `plugin/crd/setup.go` | modify | Parse `leader_election { ... }`, build typed `kubernetes.Interface`, construct elector, wire callbacks. |
| `plugin/crd/setup_test.go` | modify | Tests for new Corefile block parsing. |
| `config/rbac/lease_role.yaml` | new | Role: `coordination.k8s.io/leases` get/create/update/patch. |
| `config/rbac/lease_role_binding.yaml` | new | RoleBinding to `coredns-crd` ServiceAccount. |
| `deploy/coredns-deployment.yaml` | modify | `replicas: 2`, `POD_NAMESPACE` downward API, topology spread constraint. |

---

## Task 1: `Index.AllSnapshots()`

**Files:**
- Modify: `internal/index/index.go`
- Test: `internal/index/index_test.go`

- [ ] **Step 1.1: Write failing tests**

Add to `internal/index/index_test.go` (after `TestSnapshot_UnknownSlice`):

```go
func TestAllSnapshots_Empty(t *testing.T) {
	idx := New()
	if got := idx.AllSnapshots(); len(got) != 0 {
		t.Errorf("expected empty for fresh index, got %v", got)
	}
}

func TestAllSnapshots_ReturnsEverySlice(t *testing.T) {
	idx := New()
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil, a("a.example.com.", "1.1.1.1", nil)))
	idx.Upsert(mkSlice("ns", "b", "u2", t0().Add(time.Hour), 2, nil, a("b.example.com.", "2.2.2.2", nil)))

	got := idx.AllSnapshots()
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots, got %d: %+v", len(got), got)
	}
	byName := map[string]SliceStatus{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["a"].Generation != 1 || len(byName["a"].Result.Won) != 1 {
		t.Errorf("snapshot a wrong: %+v", byName["a"])
	}
	if byName["b"].Generation != 2 || len(byName["b"].Result.Won) != 1 {
		t.Errorf("snapshot b wrong: %+v", byName["b"])
	}
}

func TestAllSnapshots_ReflectsLWWState(t *testing.T) {
	idx := New()
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	idx.Upsert(older)
	idx.Upsert(newer)

	snaps := idx.AllSnapshots()
	byName := map[string]SliceStatus{}
	for _, s := range snaps {
		byName[s.Name] = s
	}
	if got := byName["older"].Result.Won; len(got) != 1 || got[0] != "foo.example.com. A" {
		t.Errorf("older should have Won=[foo. A], got %v", got)
	}
	if got := byName["newer"].Result.Lost; len(got) != 1 || got[0] != "foo.example.com. A" {
		t.Errorf("newer should have Lost=[foo. A], got %v", got)
	}
}
```

- [ ] **Step 1.2: Run tests, verify they fail**

```
go test -run TestAllSnapshots ./internal/index/...
```

Expected: FAIL — `idx.AllSnapshots undefined`.

- [ ] **Step 1.3: Implement `AllSnapshots`**

Add to `internal/index/index.go` (after `Lookup`):

```go
// AllSnapshots returns the current outcome for every slice in the index.
// The result is sorted by namespace, then name, for deterministic output.
func (i *Index) AllSnapshots() []SliceStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]SliceStatus, 0, len(i.slices))
	for sk, sc := range i.slices {
		snap, _ := i.snapshot(sk)
		ns, name := splitKey(sk)
		out = append(out, SliceStatus{
			Namespace:  ns,
			Name:       name,
			Generation: sc.generation,
			Result:     snap,
		})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Namespace != out[b].Namespace {
			return out[a].Namespace < out[b].Namespace
		}
		return out[a].Name < out[b].Name
	})
	return out
}
```

- [ ] **Step 1.4: Run tests, verify pass + coverage**

```
go test -race -coverprofile=/tmp/cov.out ./internal/index/...
go tool cover -func=/tmp/cov.out | grep -v 100.0%
```

Expected: PASS, total 100% (no entries in the second command output).

- [ ] **Step 1.5: Commit**

```
git add internal/index/index.go internal/index/index_test.go
git commit -m "feat(index): AllSnapshots iterator for leader-acquired reconcile sweep"
```

---

## Task 2: `internal/leader` package

**Files:**
- Create: `internal/leader/leader.go`
- Create: `internal/leader/leader_test.go`

- [ ] **Step 2.1: Write failing tests**

Create `internal/leader/leader_test.go`:

```go
package leader

import (
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func validConfig() Config {
	return Config{
		Client:         fake.NewSimpleClientset(),
		LeaseNamespace: "kube-system",
		LeaseName:      "coredns-crd-leader",
		Identity:       "test-pod",
	}
}

func TestNew_RejectsZeroClient(t *testing.T) {
	cfg := validConfig()
	cfg.Client = nil
	if _, err := New(cfg); err == nil {
		t.Errorf("expected error on nil client")
	}
}

func TestNew_RejectsEmptyLeaseName(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseName = ""
	if _, err := New(cfg); err == nil {
		t.Errorf("expected error on empty lease name")
	}
}

func TestNew_RejectsEmptyNamespace(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseNamespace = ""
	if _, err := New(cfg); err == nil {
		t.Errorf("expected error on empty namespace")
	}
}

func TestNew_RejectsEmptyIdentity(t *testing.T) {
	cfg := validConfig()
	cfg.Identity = ""
	if _, err := New(cfg); err == nil {
		t.Errorf("expected error on empty identity")
	}
}

func TestNew_FillsDefaults(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.cfg.LeaseDuration != 15*time.Second {
		t.Errorf("default LeaseDuration: got %v", e.cfg.LeaseDuration)
	}
	if e.cfg.RenewDeadline != 10*time.Second {
		t.Errorf("default RenewDeadline: got %v", e.cfg.RenewDeadline)
	}
	if e.cfg.RetryPeriod != 2*time.Second {
		t.Errorf("default RetryPeriod: got %v", e.cfg.RetryPeriod)
	}
}

func TestNew_RejectsInconsistentDurations(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseDuration = 5 * time.Second
	cfg.RenewDeadline = 10 * time.Second
	if _, err := New(cfg); err == nil || !errors.Is(err, ErrBadDurations) {
		t.Errorf("expected ErrBadDurations, got %v", err)
	}
}

func TestIsLeader_FalseBeforeRun(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e.IsLeader() {
		t.Errorf("IsLeader must be false before Run")
	}
}
```

- [ ] **Step 2.2: Run tests, verify failure**

```
go test ./internal/leader/...
```

Expected: FAIL — package doesn't exist yet.

- [ ] **Step 2.3: Implement the leader package**

Create `internal/leader/leader.go`:

```go
// Package leader is a thin wrapper around client-go leader election.
// It narrows the upstream surface to a single seam (IsLeader) that the
// rest of the plugin consults before performing leader-only work.
package leader

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// ErrBadDurations is returned when LeaseDuration <= RenewDeadline, which
// would prevent the elector from ever holding the lease cleanly.
var ErrBadDurations = errors.New("LeaseDuration must be > RenewDeadline > RetryPeriod")

type Config struct {
	Client         kubernetes.Interface
	LeaseNamespace string
	LeaseName      string
	Identity       string

	// Defaults applied when zero-valued: 15s / 10s / 2s.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration

	OnStartedLeading func(ctx context.Context)
	OnStoppedLeading func()
	OnNewLeader      func(identity string)
}

type Elector struct {
	cfg      Config
	leading  atomic.Bool
	elector  *leaderelection.LeaderElector
}

func New(cfg Config) (*Elector, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("Client is required")
	}
	if cfg.LeaseNamespace == "" {
		return nil, fmt.Errorf("LeaseNamespace is required")
	}
	if cfg.LeaseName == "" {
		return nil, fmt.Errorf("LeaseName is required")
	}
	if cfg.Identity == "" {
		return nil, fmt.Errorf("Identity is required")
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = 10 * time.Second
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = 2 * time.Second
	}
	if !(cfg.LeaseDuration > cfg.RenewDeadline && cfg.RenewDeadline > cfg.RetryPeriod) {
		return nil, ErrBadDurations
	}

	e := &Elector{cfg: cfg}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1Object(cfg.LeaseNamespace, cfg.LeaseName),
		Client:    cfg.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				e.leading.Store(true)
				if cfg.OnStartedLeading != nil {
					cfg.OnStartedLeading(ctx)
				}
			},
			OnStoppedLeading: func() {
				e.leading.Store(false)
				if cfg.OnStoppedLeading != nil {
					cfg.OnStoppedLeading()
				}
			},
			OnNewLeader: func(id string) {
				if cfg.OnNewLeader != nil {
					cfg.OnNewLeader(id)
				}
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build LeaderElector: %w", err)
	}
	e.elector = le
	return e, nil
}

// Run blocks until ctx is cancelled. It loops the leader election in case
// of transient failures (matches the upstream pattern in the leaderelection
// godoc example).
func (e *Elector) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		e.elector.Run(ctx)
		// Run() returns when leadership is lost or ctx done.
		if ctx.Err() != nil {
			return nil
		}
		// Otherwise loop and try again.
	}
}

// IsLeader reports whether this process currently holds the lease.
func (e *Elector) IsLeader() bool {
	return e.leading.Load()
}
```

Add a tiny helper file `internal/leader/lease_meta.go`:

```go
package leader

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func metav1Object(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}
```

(Keeps the import out of `leader.go`'s body for readability.)

- [ ] **Step 2.4: Run tests, verify pass**

```
go test -race -coverprofile=/tmp/cov.out ./internal/leader/...
go tool cover -func=/tmp/cov.out
```

Expected: PASS. Coverage on `New` and `IsLeader` should be 100%; `Run` is covered only smoke-wise (it blocks on the elector loop) — accept this gap.

- [ ] **Step 2.5: Commit**

```
git add internal/leader/
git commit -m "feat(leader): client-go leader election wrapper"
```

---

## Task 3: Gate `statusUpdater.Run` with `isLeader` predicate

**Files:**
- Modify: `plugin/crd/status.go`
- Modify: `plugin/crd/status_test.go`
- Modify: `plugin/crd/events_test.go` (helper update)
- Modify: `plugin/crd/setup.go` (caller update)

- [ ] **Step 3.1: Update `StatusUpdater` constructor signature**

Replace the constructor and struct in `plugin/crd/status.go`:

```go
// statusUpdater dedupes per-slice status writes via a queue keyed by namespace/name,
// with the latest payload held in a side map. Run() consults isLeader before
// patching; non-leaders Get/Done-cycle the queue without calling the API.
type statusUpdater struct {
	client   dynamic.Interface
	isLeader func() bool
	queue    workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]statusEvent
}

func NewStatusUpdater(client dynamic.Interface, isLeader func() bool) StatusUpdater {
	if isLeader == nil {
		isLeader = func() bool { return true }
	}
	return &statusUpdater{
		client:   client,
		isLeader: isLeader,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		pending: map[string]statusEvent{},
	}
}
```

- [ ] **Step 3.2: Update `Run` loop with the gate**

In `plugin/crd/status.go`, replace the body of `Run`:

```go
func (s *statusUpdater) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.queue.ShutDown()
	}()
	for {
		key, shutdown := s.queue.Get()
		if shutdown {
			return
		}
		if !s.isLeader() {
			// Not leader: leave pending entry intact for whoever is.
			s.queue.Done(key)
			continue
		}
		ev, ok := s.take(key)
		if !ok {
			s.queue.Done(key)
			continue
		}
		if err := s.process(ctx, ev); err != nil {
			log.Errorf("status update %s: %v", key, err)
			s.restore(key, ev)
			s.queue.AddRateLimited(key)
		} else {
			s.queue.Forget(key)
		}
		s.queue.Done(key)
	}
}
```

- [ ] **Step 3.3: Update existing callers in tests**

In `plugin/crd/status_test.go`, replace every `NewStatusUpdater(c)` with `NewStatusUpdater(c, nil)` (pass nil so the default `func() bool { return true }` applies, preserving existing behavior).

```bash
sed -i '' 's/NewStatusUpdater(c)/NewStatusUpdater(c, nil)/g' plugin/crd/status_test.go
```

In `plugin/crd/setup.go`, update the caller (line that says `h.statusUpdater = NewStatusUpdater(dyn)`):

```go
h.statusUpdater = NewStatusUpdater(dyn, nil)
```

(This is a placeholder — Task 6 replaces `nil` with the elector's `IsLeader`.)

- [ ] **Step 3.4: Add new gating tests**

Add to `plugin/crd/status_test.go` (after `TestRun_DrainsAndPatches`):

```go
func TestRun_NotLeader_DoesNotPatch(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	notLeader := func() bool { return false }
	su := NewStatusUpdater(c, notLeader).(*statusUpdater)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	// Give the run loop time to cycle the key.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if captured.len() != 0 {
		t.Errorf("non-leader must not patch; got %d", captured.len())
	}
	su.mu.Lock()
	pending := len(su.pending)
	su.mu.Unlock()
	if pending != 1 {
		t.Errorf("non-leader must leave pending intact; got %d entries", pending)
	}
}

func TestRun_LeaderToggle_DrainsAfterAcquire(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	var leading atomic.Bool
	su := NewStatusUpdater(c, leading.Load).(*statusUpdater)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	if captured.len() != 0 {
		cancel()
		<-done
		t.Fatal("setup: expected no patches before becoming leader")
	}

	// Acquire leadership; re-enqueue to wake the queue (key was Done'd).
	leading.Store(true)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})

	waitFor(t, 5*time.Second, func() bool { return captured.len() >= 1 })
	cancel()
	<-done
}
```

- [ ] **Step 3.5: Run all tests**

```
go test -race ./...
```

Expected: PASS, all tests including new ones.

- [ ] **Step 3.6: Commit**

```
git add plugin/crd/status.go plugin/crd/status_test.go plugin/crd/setup.go
git commit -m "feat(plugin): gate status writes on isLeader predicate"
```

---

## Task 4: `Handler.reconcileAll()`

**Files:**
- Modify: `plugin/crd/crd.go`
- Modify: `plugin/crd/crd_test.go`

- [ ] **Step 4.1: Write failing test**

Add to `plugin/crd/crd_test.go`:

```go
func TestReconcileAll_EnqueuesEverySlice(t *testing.T) {
	h, su := newHandler(t)
	h.applySlice(mkSlice("ns", "a", "u1", time.Unix(0, 0), 1, aRecord("a.example.com.", "1.1.1.1")))
	h.applySlice(mkSlice("ns", "b", "u2", time.Unix(3600, 0), 2, aRecord("b.example.com.", "2.2.2.2")))
	before := len(su.Calls())

	h.reconcileAll()

	calls := su.Calls()[before:]
	if len(calls) != 2 {
		t.Fatalf("expected 2 enqueues from reconcileAll, got %d", len(calls))
	}
	names := map[string]int64{}
	for _, c := range calls {
		names[c.Name] = c.Generation
	}
	if names["a"] != 1 || names["b"] != 2 {
		t.Errorf("reconcileAll generations wrong: %v", names)
	}
}

func TestReconcileAll_EmptyIndex_NoEnqueues(t *testing.T) {
	h, su := newHandler(t)
	h.reconcileAll()
	if got := len(su.Calls()); got != 0 {
		t.Errorf("empty index reconcile should enqueue 0, got %d", got)
	}
}

func TestReconcileAll_NilStatusUpdater_NoOp(t *testing.T) {
	cfg := &config{}
	h := New(cfg)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4")))
	// must not panic
	h.reconcileAll()
}
```

- [ ] **Step 4.2: Verify failure**

```
go test -run TestReconcileAll ./plugin/crd/...
```

Expected: FAIL — `h.reconcileAll undefined`.

- [ ] **Step 4.3: Implement `reconcileAll`**

Add to `plugin/crd/crd.go`:

```go
// reconcileAll enqueues a fresh status snapshot for every slice in the index.
// Called by setup() when the pod becomes leader, ensuring published status
// matches current Index state regardless of whatever the previous leader
// did or didn't write.
func (h *Handler) reconcileAll() {
	if h.statusUpdater == nil {
		return
	}
	for _, s := range h.idx.AllSnapshots() {
		h.statusUpdater.Enqueue(s.Namespace, s.Name, s.Generation, s.Result)
	}
}
```

- [ ] **Step 4.4: Verify pass + coverage**

```
go test -race -coverprofile=/tmp/cov.out ./plugin/crd/...
go tool cover -func=/tmp/cov.out | grep -E '(crd\.go|reconcileAll)' | grep -v 100.0%
```

Expected: PASS; second command produces no output (reconcileAll at 100%).

- [ ] **Step 4.5: Commit**

```
git add plugin/crd/crd.go plugin/crd/crd_test.go
git commit -m "feat(plugin): reconcileAll snapshot sweep for leader acquisition"
```

---

## Task 5: Parse `leader_election` Corefile block

**Files:**
- Modify: `plugin/crd/setup.go`
- Modify: `plugin/crd/setup_test.go`

- [ ] **Step 5.1: Add LeaderElection fields to config struct**

In `plugin/crd/setup.go`, replace the `config` struct:

```go
type config struct {
	Kubeconfig     string
	ResyncPeriod   time.Duration
	Fall           fall.F
	LeaderElection LeaderElectionConfig
}

type LeaderElectionConfig struct {
	Disabled  bool
	Namespace string
	LeaseName string
}
```

- [ ] **Step 5.2: Add parsing for the new block**

In `plugin/crd/setup.go`, update `parseConfig` — add a case in the inner switch:

```go
case "leader_election":
    if err := parseLeaderElection(c, &cfg.LeaderElection); err != nil {
        return nil, err
    }
```

Add the helper at the bottom of the file:

```go
func parseLeaderElection(c *caddy.Controller, le *LeaderElectionConfig) error {
	args := c.RemainingArgs()
	if len(args) > 0 {
		return c.Errf("unexpected args on leader_election line: %v", args)
	}
	for c.NextBlock() {
		switch c.Val() {
		case "disable":
			le.Disabled = true
		case "namespace":
			if !c.NextArg() {
				return c.ArgErr()
			}
			le.Namespace = c.Val()
		case "lease_name":
			if !c.NextArg() {
				return c.ArgErr()
			}
			le.LeaseName = c.Val()
		default:
			return c.Errf("unknown leader_election property %q", c.Val())
		}
	}
	return nil
}
```

- [ ] **Step 5.3: Add tests**

Add to `plugin/crd/setup_test.go`:

```go
func TestParseConfig_LeaderElection_Defaults(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaderElection.Disabled {
		t.Errorf("leader election should be enabled by default")
	}
	if cfg.LeaderElection.Namespace != "" {
		t.Errorf("Namespace default should be empty (resolved at runtime), got %q", cfg.LeaderElection.Namespace)
	}
	if cfg.LeaderElection.LeaseName != "" {
		t.Errorf("LeaseName default should be empty (resolved at runtime), got %q", cfg.LeaderElection.LeaseName)
	}
}

func TestParseConfig_LeaderElection_AllProperties(t *testing.T) {
	input := `crd {
		leader_election {
			namespace foo
			lease_name bar
		}
	}`
	cfg, err := parseConfigFromInput(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaderElection.Namespace != "foo" || cfg.LeaderElection.LeaseName != "bar" {
		t.Errorf("got %+v", cfg.LeaderElection)
	}
	if cfg.LeaderElection.Disabled {
		t.Errorf("Disabled should be false")
	}
}

func TestParseConfig_LeaderElection_Disable(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		leader_election {
			disable
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LeaderElection.Disabled {
		t.Errorf("expected Disabled=true")
	}
}

func TestParseConfig_LeaderElection_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			"unknown property",
			`crd { leader_election { bogus value } }`,
			"unknown leader_election property",
		},
		{
			"namespace missing arg",
			`crd { leader_election { namespace } }`,
			"Wrong argument count",
		},
		{
			"lease_name missing arg",
			`crd { leader_election { lease_name } }`,
			"Wrong argument count",
		},
		{
			"unexpected args on plugin line",
			`crd { leader_election extra { disable } }`,
			"unexpected args",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfigFromInput(t, tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
```

- [ ] **Step 5.4: Run tests, verify pass + coverage**

```
go test -race -coverprofile=/tmp/cov.out ./plugin/crd/...
go tool cover -func=/tmp/cov.out | grep parseLeaderElection
```

Expected: PASS; parseLeaderElection at 100%.

- [ ] **Step 5.5: Commit**

```
git add plugin/crd/setup.go plugin/crd/setup_test.go
git commit -m "feat(plugin): parse leader_election Corefile block"
```

---

## Task 6: Wire leader election in `setup()`

**Files:**
- Modify: `plugin/crd/setup.go`

- [ ] **Step 6.1: Add imports**

In `plugin/crd/setup.go`, add to the import block:

```go
import (
	// ... existing imports ...
	"os"

	"k8s.io/client-go/kubernetes"

	"github.com/hbinhng/coredns-crd/internal/leader"
)
```

- [ ] **Step 6.2: Build typed clientset and elector in `setup`**

In `plugin/crd/setup.go`, replace the body of `setup()` after the `dyn, err := dynamic.NewForConfig(rc)` line:

```go
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return plugin.Error(pluginName, fmt.Errorf("dynamic client: %w", err))
	}
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return plugin.Error(pluginName, fmt.Errorf("typed clientset: %w", err))
	}

	h := New(cfg)

	var isLeader func() bool
	var elector *leader.Elector
	if cfg.LeaderElection.Disabled {
		isLeader = func() bool { return true }
	} else {
		ns := cfg.LeaderElection.Namespace
		if ns == "" {
			ns = os.Getenv("POD_NAMESPACE")
		}
		if ns == "" {
			ns = "kube-system"
		}
		name := cfg.LeaderElection.LeaseName
		if name == "" {
			name = "coredns-crd-leader"
		}
		identity, _ := os.Hostname()
		if identity == "" {
			identity = "coredns-crd"
		}
		elector, err = leader.New(leader.Config{
			Client:         clientset,
			LeaseNamespace: ns,
			LeaseName:      name,
			Identity:       identity,
			OnStartedLeading: func(ctx context.Context) {
				log.Infof("acquired leadership; reconciling all DNSSlices")
				h.reconcileAll()
			},
			OnStoppedLeading: func() {
				log.Infof("lost leadership; status writes paused")
			},
			OnNewLeader: func(id string) {
				log.Infof("current leader: %s", id)
			},
		})
		if err != nil {
			return plugin.Error(pluginName, fmt.Errorf("leader elector: %w", err))
		}
		isLeader = elector.IsLeader
	}

	h.statusUpdater = NewStatusUpdater(dyn, isLeader)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, cfg.ResyncPeriod)
	informer := factory.ForResource(dnsSliceGVR).Informer()
	if _, err := informer.AddEventHandler(h.eventHandler()); err != nil {
		return plugin.Error(pluginName, fmt.Errorf("register event handler: %w", err))
	}

	c.OnStartup(func() error {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.statusUpdater.Run(ctx)
		if elector != nil {
			go func() {
				if err := elector.Run(ctx); err != nil {
					log.Errorf("leader elector exited: %v", err)
				}
			}()
		}
		factory.Start(ctx.Done())
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return fmt.Errorf("crd: failed to sync DNSSlice cache")
		}
		log.Info("DNSSlice cache synced")
		return nil
	})
	c.OnShutdown(func() error {
		if h.cancel != nil {
			h.cancel()
		}
		return nil
	})

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		h.Next = next
		return h
	})
	return nil
}
```

- [ ] **Step 6.3: Verify build + existing tests**

```
go build ./...
go test -race ./...
```

Expected: PASS. (Nothing in unit tests exercises `setup()` itself; this verifies no regressions.)

- [ ] **Step 6.4: Commit**

```
git add plugin/crd/setup.go
git commit -m "feat(plugin): wire client-go leader election into setup"
```

---

## Task 7: Lease RBAC

**Files:**
- Create: `config/rbac/lease_role.yaml`
- Create: `config/rbac/lease_role_binding.yaml`

- [ ] **Step 7.1: Create the Role**

Create `config/rbac/lease_role.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: coredns-crd-leader
  namespace: kube-system
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update", "patch"]
```

- [ ] **Step 7.2: Create the RoleBinding**

Create `config/rbac/lease_role_binding.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: coredns-crd-leader
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: coredns-crd-leader
subjects:
  - kind: ServiceAccount
    name: coredns-crd
    namespace: kube-system
```

- [ ] **Step 7.3: Validate manifests**

```
kubectl apply --dry-run=client -f config/rbac/lease_role.yaml
kubectl apply --dry-run=client -f config/rbac/lease_role_binding.yaml
```

Expected: Both report `created (dry run)` with no errors.

- [ ] **Step 7.4: Commit**

```
git add config/rbac/lease_role.yaml config/rbac/lease_role_binding.yaml
git commit -m "feat: RBAC for coordination.k8s.io/leases (leader election)"
```

---

## Task 8: Deployment updates

**Files:**
- Modify: `deploy/coredns-deployment.yaml`

- [ ] **Step 8.1: Update Deployment**

In `deploy/coredns-deployment.yaml`:

1. Change `replicas: 1` to `replicas: 2`.
2. Add `topologySpreadConstraints` to the pod spec (above `tolerations:`):

   ```yaml
       topologySpreadConstraints:
         - maxSkew: 1
           topologyKey: kubernetes.io/hostname
           whenUnsatisfiable: ScheduleAnyway
           labelSelector:
             matchLabels:
               k8s-app: kube-dns
   ```

3. Add an `env:` block to the container (below `args:`):

   ```yaml
           env:
             - name: POD_NAMESPACE
               valueFrom:
                 fieldRef:
                   fieldPath: metadata.namespace
   ```

- [ ] **Step 8.2: Validate manifest**

```
kubectl apply --dry-run=client -f deploy/coredns-deployment.yaml
```

Expected: All resources `(dry run)` with no errors.

- [ ] **Step 8.3: Commit**

```
git add deploy/coredns-deployment.yaml
git commit -m "feat(deploy): replicas:2 with topology spread + POD_NAMESPACE downward API"
```

---

## Task 9: E2E validation on the k3s box

This task is verification-only; it produces no commits.

- [ ] **Step 9.1: Sync repo to remote**

```
rsync -azq --delete --exclude='.git' --exclude='/bin' --exclude='/dist' \
  /Users/hbinhng/Documents/repos/personal/coredns-crd/ \
  hbinhng@192.168.1.34:~/coredns-crd/
```

- [ ] **Step 9.2: Rebuild image (cached layers, fast)**

```
ssh hbinhng@192.168.1.34 'cd ~/coredns-crd && sudo docker build -t coredns-crd:dev . 2>&1 | tail -5'
```

Expected: `Successfully tagged coredns-crd:dev`.

- [ ] **Step 9.3: Re-import into k3s containerd**

```
ssh hbinhng@192.168.1.34 'sudo docker save coredns-crd:dev | sudo k3s ctr images import - 2>&1 | tail -2'
```

- [ ] **Step 9.4: Apply manifests**

```
ssh hbinhng@192.168.1.34 'kubectl apply -f ~/coredns-crd/config/rbac/ -f ~/coredns-crd/deploy/coredns-deployment.yaml && kubectl -n kube-system rollout restart deployment/coredns && kubectl -n kube-system rollout status deployment/coredns --timeout=120s'
```

Expected: rollout succeeds, both pods Ready.

- [ ] **Step 9.5: Verify exactly one Lease holder**

```
ssh hbinhng@192.168.1.34 'kubectl -n kube-system get lease coredns-crd-leader -o jsonpath="{.spec.holderIdentity}"; echo'
```

Expected: a single pod name.

- [ ] **Step 9.6: Kill the leader, verify failover**

```
ssh hbinhng@192.168.1.34 'LEADER=$(kubectl -n kube-system get lease coredns-crd-leader -o jsonpath="{.spec.holderIdentity}") && echo "killing $LEADER" && kubectl -n kube-system delete pod "$LEADER" --grace-period=0 --force && sleep 20 && echo "new holder:" && kubectl -n kube-system get lease coredns-crd-leader -o jsonpath="{.spec.holderIdentity}{\"\\n\"}"'
```

Expected: a different pod name; both pods serve DNS throughout (no resolution drop).

- [ ] **Step 9.7: Resolution sanity post-failover**

```
ssh hbinhng@192.168.1.34 'kubectl apply -f ~/coredns-crd/config/example/dnsslice.yaml; sleep 5; kubectl run digtest --rm -i --restart=Never --image=docker.io/alpine:3.20 --command -- sh -c "apk add --no-cache bind-tools >/dev/null 2>&1 && dig +short @10.43.0.10 web.example.com A"'
```

Expected: `10.0.0.1` and `10.0.0.2`.

---

## Self-review

**Spec coverage:** every section of the design spec is implemented:
- Architecture (every replica informer + Index, leader-only writes) → Tasks 1–6.
- `internal/leader` package → Task 2.
- `statusUpdater` gating → Task 3.
- `reconcileAll` → Task 4.
- Configurability (`leader_election { ... }`) → Task 5.
- Setup wiring → Task 6.
- RBAC → Task 7.
- Deployment changes → Task 8.
- Acceptance criteria #1 (tests) → Tasks 1–6 each gate on tests.
- AC #2 / #3 / #4 / #5 → Task 9 (manual verification today; CI in sub-project F).

**Placeholder scan:** every code step shows the actual code; no "TBD" or "as appropriate".

**Type consistency:** `Index.AllSnapshots` returns `[]SliceStatus` (already defined in `index.go`); `reconcileAll` consumes it; `LeaderElectionConfig` fields used in setup match the parsed values.

**Outstanding gap:** test for `internal/leader.Run` blocking-loop coverage left as a known small gap (upstream client-go behavior). Documented in Task 2 step 2.4.
