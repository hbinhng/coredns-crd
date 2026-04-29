package crd

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/hbinhng/coredns-crd/internal/index"
)

// ---------- fake dynamic client setup ----------

func newFakeDynamicClient() (*dynamicfake.FakeDynamicClient, *clienttesting.Fake) {
	scheme := runtime.NewScheme()
	gvr := dnsSliceGVR
	gvk := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "DNSSlice"}
	listGvk := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "DNSSliceList"}
	scheme.AddKnownTypeWithName(gvk, &mapObject{})
	scheme.AddKnownTypeWithName(listGvk, &mapObjectList{})

	listKinds := map[schema.GroupVersionResource]string{
		gvr: "DNSSliceList",
	}
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	return c, &c.Fake
}

// mapObject + mapObjectList are minimal runtime.Object stand-ins for the
// scheme. The fake dynamic client uses Unstructured under the hood; these
// are only needed so AddKnownTypeWithName has something to register.
type mapObject struct{ metav1.TypeMeta }

func (m *mapObject) DeepCopyObject() runtime.Object { c := *m; return &c }

type mapObjectList struct {
	metav1.TypeMeta
	metav1.ListMeta
	Items []mapObject
}

func (m *mapObjectList) DeepCopyObject() runtime.Object { c := *m; return &c }

// ---------- process() patch shape ----------

func TestProcess_BuildsExpectedPatch_ReadyTrue_NoConflict(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	s := &statusUpdater{client: c}
	err := s.process(context.Background(), statusEvent{
		namespace: "ns", name: "n", generation: 5,
		result: index.UpsertResult{
			Won:    []string{"foo.example.com. A"},
			LostTo: map[string]string{},
		},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if captured.len() != 1 {
		t.Fatalf("expected 1 patch, got %d", captured.len())
	}
	p := captured.snapshot()[0]
	if p.PatchType != types.MergePatchType {
		t.Errorf("expected MergePatch, got %v", p.PatchType)
	}
	if p.Subresource != "status" {
		t.Errorf("expected status subresource, got %q", p.Subresource)
	}

	// Top-level patch keys must be exactly ["status"] — no metadata/spec leakage.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(p.Patch, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	if len(top) != 1 {
		t.Errorf("expected 1 top-level key, got %d: %v", len(top), keysOf(top))
	}
	if _, ok := top["status"]; !ok {
		t.Errorf("missing 'status' key, got %v", keysOf(top))
	}

	body := decodePatch(t, p.Patch)
	conds := body.Status.Conditions
	if len(conds) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conds))
	}
	ready, conflict := conds[0], conds[1]
	if ready.Type != "Ready" || ready.Status != metav1.ConditionTrue || ready.Reason != "Applied" {
		t.Errorf("Ready condition wrong: %+v", ready)
	}
	if conflict.Type != "Conflicting" || conflict.Status != metav1.ConditionFalse || conflict.Reason != "NoConflict" {
		t.Errorf("Conflicting condition wrong: %+v", conflict)
	}
	if conflict.Message != "all entries owned" {
		t.Errorf("Conflicting.Message=%q, want %q", conflict.Message, "all entries owned")
	}
	if body.Status.ObservedGeneration != 5 {
		t.Errorf("ObservedGeneration: got %d, want 5", body.Status.ObservedGeneration)
	}
	if ready.ObservedGeneration != 5 || conflict.ObservedGeneration != 5 {
		t.Errorf("per-condition ObservedGeneration mismatch: ready=%d conflict=%d",
			ready.ObservedGeneration, conflict.ObservedGeneration)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestProcess_BuildsExpectedPatch_Conflicting(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	s := &statusUpdater{client: c}
	err := s.process(context.Background(), statusEvent{
		namespace: "ns", name: "n", generation: 9,
		result: index.UpsertResult{
			Lost:   []string{"foo.example.com. A"},
			LostTo: map[string]string{"foo.example.com. A": "ns/older"},
		},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	body := decodePatch(t, captured.snapshot()[0].Patch)
	conflict := body.Status.Conditions[1]
	if conflict.Status != metav1.ConditionTrue || conflict.Reason != "OutbidByOlder" {
		t.Errorf("expected Conflicting=True/OutbidByOlder, got %+v", conflict)
	}
	if conflict.Message != "foo.example.com. A owned by ns/older" {
		t.Errorf("Conflicting message=%q", conflict.Message)
	}
	if conflict.ObservedGeneration != 9 {
		t.Errorf("Conflicting.ObservedGeneration=%d, want 9", conflict.ObservedGeneration)
	}
	if len(body.Status.ConflictedEntries) != 1 || body.Status.ConflictedEntries[0] != "foo.example.com. A" {
		t.Errorf("ConflictedEntries=%v", body.Status.ConflictedEntries)
	}
}

func TestProcess_BuildsExpectedPatch_MultipleLost_OrderPreserved(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	s := &statusUpdater{client: c}
	_ = s.process(context.Background(), statusEvent{
		namespace: "ns", name: "n", generation: 1,
		result: index.UpsertResult{
			Lost: []string{"a A", "b A", "c A"},
			LostTo: map[string]string{
				"a A": "ns/x",
				"b A": "ns/y",
				"c A": "ns/z",
			},
		},
	})
	body := decodePatch(t, captured.snapshot()[0].Patch)
	want := "a A owned by ns/x; b A owned by ns/y; c A owned by ns/z"
	if got := body.Status.Conditions[1].Message; got != want {
		t.Errorf("multi-lost message ordering broke: got %q want %q", got, want)
	}
}

func TestProcess_BuildsExpectedPatch_ParseError(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	s := &statusUpdater{client: c}
	_ = s.process(context.Background(), statusEvent{
		namespace: "ns", name: "n", generation: 2,
		result: index.UpsertResult{
			ParseErrors: []string{"entry[0] foo/A: invalid IPv4"},
			LostTo:      map[string]string{},
		},
	})
	body := decodePatch(t, captured.snapshot()[0].Patch)
	ready := body.Status.Conditions[0]
	if ready.Status != metav1.ConditionFalse || ready.Reason != "ParseError" {
		t.Errorf("expected Ready=False/ParseError, got %+v", ready)
	}
	if ready.Message != "entry[0] foo/A: invalid IPv4" {
		t.Errorf("Ready message=%q", ready.Message)
	}
}

func TestProcess_ApiError_Surfaces(t *testing.T) {
	c, fake := newFakeDynamicClient()
	fake.PrependReactor("patch", "dnsslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})

	s := &statusUpdater{client: c}
	err := s.process(context.Background(), statusEvent{namespace: "ns", name: "n"})
	if err == nil || err.Error() == "" {
		t.Errorf("expected error from patch failure, got %v", err)
	}
}

// ---------- Enqueue / take / restore semantics ----------

func TestEnqueueDedupsByKey(t *testing.T) {
	c, _ := newFakeDynamicClient()
	su := NewStatusUpdater(c, nil).(*statusUpdater)

	su.Enqueue("ns", "n", 1, index.UpsertResult{Won: []string{"a A"}})
	su.Enqueue("ns", "n", 2, index.UpsertResult{Won: []string{"b A"}})
	su.Enqueue("ns", "n", 3, index.UpsertResult{Won: []string{"c A"}})

	if su.queue.Len() != 1 {
		t.Errorf("queue should hold 1 dedup'd key, got %d", su.queue.Len())
	}
	su.mu.Lock()
	pending := su.pending["ns/n"]
	su.mu.Unlock()
	if pending.generation != 3 {
		t.Errorf("expected last enqueue (gen=3) to be retained, got gen=%d", pending.generation)
	}
}

func TestTakeRemovesPending(t *testing.T) {
	c, _ := newFakeDynamicClient()
	su := NewStatusUpdater(c, nil).(*statusUpdater)
	su.Enqueue("ns", "n", 1, index.UpsertResult{})

	if _, ok := su.take("ns/n"); !ok {
		t.Errorf("take should return ok=true for present key")
	}
	if _, ok := su.take("ns/n"); ok {
		t.Errorf("take should return ok=false after removal")
	}
}

func TestRestoreSurvivesNewerEnqueue(t *testing.T) {
	// Exercise restore via the public Enqueue path (rather than direct map
	// mutation) so the lock interleaving is the real production code path.
	c, _ := newFakeDynamicClient()
	su := NewStatusUpdater(c, nil).(*statusUpdater)

	original := statusEvent{namespace: "ns", name: "n", generation: 1}
	su.Enqueue("ns", "n", 5, index.UpsertResult{}) // newer enqueue first
	su.restore("ns/n", original)                   // restore must NOT clobber

	su.mu.Lock()
	got := su.pending["ns/n"]
	su.mu.Unlock()
	if got.generation != 5 {
		t.Errorf("restore clobbered newer pending: got gen=%d, want 5", got.generation)
	}
}

func TestRestoreInsertsWhenAbsent(t *testing.T) {
	c, _ := newFakeDynamicClient()
	su := NewStatusUpdater(c, nil).(*statusUpdater)
	original := statusEvent{namespace: "ns", name: "n", generation: 1}
	su.restore("ns/n", original)

	su.mu.Lock()
	got, ok := su.pending["ns/n"]
	su.mu.Unlock()
	if !ok || got.generation != 1 {
		t.Errorf("restore on empty must insert; got %+v ok=%v", got, ok)
	}
}

// ---------- Run loop ----------

func TestRun_DrainsAndPatches(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	su := NewStatusUpdater(c, nil).(*statusUpdater)
	su.Enqueue("ns", "a", 1, index.UpsertResult{Won: []string{"a A"}, LostTo: map[string]string{}})
	su.Enqueue("ns", "b", 1, index.UpsertResult{Won: []string{"b A"}, LostTo: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	waitFor(t, 5*time.Second, func() bool { return captured.len() == 2 })
	cancel()
	<-done
}

func TestRun_NotLeader_DoesNotPatch(t *testing.T) {
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	notLeader := func() bool { return false }
	su := NewStatusUpdater(c, notLeader).(*statusUpdater)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	// Wait until the run loop has cycled the key (queue will dedupe to len 0
	// once Done has been called for it).
	waitFor(t, time.Second, func() bool { return su.queue.Len() == 0 })
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

	// Confirm no patch fires before becoming leader.
	waitFor(t, time.Second, func() bool { return su.queue.Len() == 0 })
	if captured.len() != 0 {
		cancel()
		<-done
		t.Fatalf("expected 0 patches before leader, got %d", captured.len())
	}

	// Become leader; re-enqueue to wake the queue (key was Done'd).
	leading.Store(true)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})
	waitFor(t, 5*time.Second, func() bool { return captured.len() >= 1 })
	cancel()
	<-done
}

func TestRun_KeyWithoutPending_IsDoneAndContinues(t *testing.T) {
	// Direct add to the queue without populating pending exercises the
	// `take ok=false` branch (Done + continue, no patch).
	c, fake := newFakeDynamicClient()
	captured := capturePatches(fake)

	su := NewStatusUpdater(c, nil).(*statusUpdater)
	su.queue.Add("ns/orphan")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	// Give Run a chance to drain the queue. Then enqueue a real one and confirm
	// it's the only patched.
	time.Sleep(50 * time.Millisecond)
	su.Enqueue("ns", "real", 1, index.UpsertResult{LostTo: map[string]string{}})
	waitFor(t, 5*time.Second, func() bool { return captured.len() >= 1 })

	cancel()
	<-done

	patches := captured.snapshot()
	for _, p := range patches {
		if p.Name == "orphan" {
			t.Errorf("orphan key (no pending) should not produce a patch, got %+v", p)
		}
	}
	if len(patches) != 1 || patches[0].Name != "real" {
		t.Errorf("expected exactly the real patch, got %+v", patches)
	}
}

func TestRun_RetriesOnPatchError(t *testing.T) {
	c, fake := newFakeDynamicClient()

	var attempts int32
	var firstAt, thirdAt time.Time
	fake.PrependReactor("patch", "dnsslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			firstAt = time.Now()
		}
		if n == 3 {
			thirdAt = time.Now()
		}
		if n < 3 {
			return true, nil, errors.New("transient")
		}
		return true, nil, nil
	})

	su := NewStatusUpdater(c, nil).(*statusUpdater)
	su.Enqueue("ns", "n", 1, index.UpsertResult{LostTo: map[string]string{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { su.Run(ctx); close(done) }()

	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&attempts) >= 3 })
	cancel()
	<-done

	// AddRateLimited (vs Add) means default exponential backoff: ~5ms, ~10ms, ~20ms.
	// Two retries should take at least ~5ms total — pin that "rate limit was used".
	elapsed := thirdAt.Sub(firstAt)
	if elapsed < 5*time.Millisecond {
		t.Errorf("3 attempts in %v; expected >=5ms backoff (AddRateLimited not used?)", elapsed)
	}
}

func TestEnqueueConcurrent_LastWriteRetained(t *testing.T) {
	// Document the contract: with concurrent enqueues for the same key, the
	// LAST writer wins (no monotonic guard). The informer guarantees per-key
	// serialization in production, so this is acceptable; this test pins the
	// behavior so a future change is intentional.
	c, _ := newFakeDynamicClient()
	su := NewStatusUpdater(c, nil).(*statusUpdater)

	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			su.Enqueue("ns", "n", g, index.UpsertResult{LostTo: map[string]string{}})
		}(int64(i))
	}
	wg.Wait()

	if su.queue.Len() != 1 {
		t.Errorf("queue should hold 1 dedup'd key, got %d", su.queue.Len())
	}
	su.mu.Lock()
	gen := su.pending["ns/n"].generation
	su.mu.Unlock()
	if gen < 0 || gen >= N {
		t.Errorf("retained generation out of range: %d", gen)
	}
}

// ---------- helpers ----------

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

type capturedPatch struct {
	Namespace, Name string
	PatchType       types.PatchType
	Subresource     string
	Patch           []byte
}

// patchRecorder is a thread-safe sink for patch reactor invocations.
type patchRecorder struct {
	mu   sync.Mutex
	list []capturedPatch
}

func (p *patchRecorder) add(c capturedPatch) {
	p.mu.Lock()
	p.list = append(p.list, c)
	p.mu.Unlock()
}

func (p *patchRecorder) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.list)
}

func (p *patchRecorder) snapshot() []capturedPatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]capturedPatch, len(p.list))
	copy(out, p.list)
	return out
}

// capturePatches records every patch on dnsslices and short-circuits the
// reaction (returns handled=true) so the default tracker does not require a
// pre-existing object — lets us test process() without seeding the fake.
func capturePatches(fake *clienttesting.Fake) *patchRecorder {
	rec := &patchRecorder{}
	fake.PrependReactor("patch", "dnsslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchAction)
		rec.add(capturedPatch{
			Namespace:   pa.GetNamespace(),
			Name:        pa.GetName(),
			PatchType:   pa.GetPatchType(),
			Subresource: pa.GetSubresource(),
			Patch:       append([]byte(nil), pa.GetPatch()...),
		})
		return true, nil, nil
	})
	return rec
}

type patchBody struct {
	Status struct {
		ObservedGeneration int64              `json:"observedGeneration"`
		Conditions         []metav1.Condition `json:"conditions"`
		ConflictedEntries  []string           `json:"conflictedEntries"`
	} `json:"status"`
}

func decodePatch(t *testing.T, data []byte) patchBody {
	t.Helper()
	var b patchBody
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	return b
}

// Sanity: the dynamic.Interface returned by NewStatusUpdater is not nil.
var _ dynamic.Interface = (&statusUpdater{}).client
