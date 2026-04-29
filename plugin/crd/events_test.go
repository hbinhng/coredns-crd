package crd

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/pkg/fall"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/index"
)

// ---------- fakes ----------

type recordedEnqueue struct {
	Namespace, Name string
	Generation      int64
	Result          index.UpsertResult
}

type fakeStatusUpdater struct {
	mu       sync.Mutex
	calls    []recordedEnqueue
	runDone  chan struct{}
	runCount int
}

func newFakeStatusUpdater() *fakeStatusUpdater {
	return &fakeStatusUpdater{runDone: make(chan struct{})}
}

func (f *fakeStatusUpdater) Enqueue(ns, name string, gen int64, r index.UpsertResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedEnqueue{Namespace: ns, Name: name, Generation: gen, Result: r})
}

func (f *fakeStatusUpdater) Run(ctx context.Context) {
	f.mu.Lock()
	f.runCount++
	f.mu.Unlock()
	<-ctx.Done()
	close(f.runDone)
}

func (f *fakeStatusUpdater) Calls() []recordedEnqueue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedEnqueue, len(f.calls))
	copy(out, f.calls)
	return out
}

func newHandler(t *testing.T) (*Handler, *fakeStatusUpdater) {
	t.Helper()
	cfg := &config{Fall: fall.F{}}
	h := New(cfg)
	su := newFakeStatusUpdater()
	h.statusUpdater = su
	return h, su
}

// ---------- helpers ----------

func toUnstructured(t *testing.T, slice *apiv1.DNSSlice) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(slice)
	if err != nil {
		t.Fatalf("toUnstructured: %v", err)
	}
	return &unstructured.Unstructured{Object: m}
}

func mkSlice(ns, name, uid string, creation time.Time, gen int64, entries ...apiv1.DNSEntry) *apiv1.DNSSlice {
	t := metav1.NewTime(creation)
	return &apiv1.DNSSlice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "dns.coredns-crd.io/v1alpha1",
			Kind:       "DNSSlice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			UID:               types.UID(uid),
			Generation:        gen,
			CreationTimestamp: t,
		},
		Spec: apiv1.DNSSliceSpec{Entries: entries},
	}
}

func aRecord(name, ip string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "A", A: &apiv1.ARecord{Address: ip}}
}

func aaaaRecord(name, ip string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "AAAA", AAAA: &apiv1.AAAARecord{Address: ip}}
}

func cnameRecord(name, target string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "CNAME", CNAME: &apiv1.CNAMERecord{Target: target}}
}

func txtRecord(name string, vals ...string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "TXT", TXT: &apiv1.TXTRecord{Values: vals}}
}

// ---------- toTyped ----------

func TestToTyped_Roundtrip(t *testing.T) {
	want := mkSlice("ns", "n", "u1", time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC), 3,
		aRecord("foo.example.com.", "1.2.3.4"),
	)
	got, err := toTyped(toUnstructured(t, want))
	if err != nil {
		t.Fatalf("toTyped: %v", err)
	}
	if got.Namespace != want.Namespace || got.Name != want.Name {
		t.Errorf("identity mismatch: got %s/%s want %s/%s", got.Namespace, got.Name, want.Namespace, want.Name)
	}
	if got.Generation != want.Generation {
		t.Errorf("Generation: got %d want %d", got.Generation, want.Generation)
	}
	if len(got.Spec.Entries) != 1 || got.Spec.Entries[0].A == nil || got.Spec.Entries[0].A.Address != "1.2.3.4" {
		t.Errorf("Spec.Entries roundtrip failed: %+v", got.Spec.Entries)
	}
}

func TestToTyped_RejectsNonUnstructured(t *testing.T) {
	_, err := toTyped("not-an-unstructured")
	if err == nil {
		t.Fatal("expected error on non-unstructured input")
	}
}

// ---------- applySlice / deleteSlice ----------

func TestApplySlice_EnqueuesPrimaryAndSiblings(t *testing.T) {
	h, su := newHandler(t)

	older := mkSlice("ns", "older", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.1.1.1"))
	newer := mkSlice("ns", "newer", "u2", time.Unix(3600, 0), 1, aRecord("foo.example.com.", "2.2.2.2"))
	h.applySlice(newer)
	h.applySlice(older)

	calls := su.Calls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 enqueues, got %d: %+v", len(calls), calls)
	}
	// Contract: each apply enqueues primary FIRST, then siblings in the
	// order Index returned them (sorted by ns/name).
	if calls[0].Name != "newer" || len(calls[0].Result.Won) != 1 {
		t.Errorf("first apply: expected newer as winning primary, got %+v", calls[0])
	}
	if calls[1].Name != "older" || len(calls[1].Result.Won) != 1 {
		t.Errorf("second apply primary: expected older as new winner, got %+v", calls[1])
	}
	if calls[2].Name != "newer" || len(calls[2].Result.Lost) != 1 {
		t.Errorf("second apply sibling: expected newer demoted to loser, got %+v", calls[2])
	}
}

func TestApplySlice_ParseErrorsSurfaceWithoutWonEntries(t *testing.T) {
	h, su := newHandler(t)
	bad := mkSlice("ns", "bad", "u1", time.Unix(0, 0), 1,
		apiv1.DNSEntry{Name: "x.example.com.", Type: "A"}, // missing .a
	)
	h.applySlice(bad)

	calls := su.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 enqueue, got %d", len(calls))
	}
	if len(calls[0].Result.ParseErrors) != 1 {
		t.Errorf("expected 1 ParseError, got %v", calls[0].Result.ParseErrors)
	}
	if len(calls[0].Result.Won) != 0 {
		t.Errorf("expected no Won entries, got %v", calls[0].Result.Won)
	}
}

func TestApplySlice_NoStatusUpdater_NoOp(t *testing.T) {
	cfg := &config{}
	h := New(cfg) // statusUpdater intentionally nil
	// Must not panic when statusUpdater is nil.
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4")))
}

func TestDeleteSlice_PromotesAndEnqueuesSibling(t *testing.T) {
	h, su := newHandler(t)
	older := mkSlice("ns", "older", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.1.1.1"))
	newer := mkSlice("ns", "newer", "u2", time.Unix(3600, 0), 1, aRecord("foo.example.com.", "2.2.2.2"))
	h.applySlice(older)
	h.applySlice(newer)

	before := len(su.Calls())
	h.deleteSlice(older)

	calls := su.Calls()[before:]
	if len(calls) != 1 {
		t.Fatalf("expected 1 enqueue on delete (the promoted sibling), got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "newer" || len(calls[0].Result.Won) != 1 {
		t.Errorf("expected newer to be promoted, got %+v", calls[0])
	}
}

func TestDeleteSlice_NoSiblings_NoEnqueue(t *testing.T) {
	h, su := newHandler(t)
	s := mkSlice("ns", "lone", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.1.1.1"))
	h.applySlice(s)
	before := len(su.Calls())
	h.deleteSlice(s)
	if got := len(su.Calls()) - before; got != 0 {
		t.Errorf("expected 0 sibling enqueues, got %d", got)
	}
}

func TestDeleteSlice_NoStatusUpdater_NoOp(t *testing.T) {
	cfg := &config{}
	h := New(cfg)
	h.deleteSlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4")))
}

// ---------- eventHandler ----------

func TestEventHandler_AddDispatchesToApplySlice(t *testing.T) {
	h, su := newHandler(t)
	eh := h.eventHandler()
	slice := mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4"))
	eh.OnAdd(toUnstructured(t, slice), false)

	calls := su.Calls()
	if len(calls) != 1 || calls[0].Name != "n" {
		t.Errorf("expected 1 enqueue for the added slice, got %+v", calls)
	}
}

func TestEventHandler_UpdateDispatchesToApplySlice(t *testing.T) {
	h, su := newHandler(t)
	eh := h.eventHandler()
	old := mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4"))
	new := mkSlice("ns", "n", "u1", time.Unix(0, 0), 2, aRecord("foo.example.com.", "5.6.7.8"))
	eh.OnUpdate(toUnstructured(t, old), toUnstructured(t, new))

	calls := su.Calls()
	if len(calls) != 1 || calls[0].Generation != 2 {
		t.Errorf("expected 1 enqueue with generation 2, got %+v", calls)
	}
}

func TestEventHandler_DeleteDispatchesToDeleteSlice(t *testing.T) {
	h, su := newHandler(t)
	eh := h.eventHandler()

	older := mkSlice("ns", "older", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.1.1.1"))
	newer := mkSlice("ns", "newer", "u2", time.Unix(3600, 0), 1, aRecord("foo.example.com.", "2.2.2.2"))
	h.applySlice(older)
	h.applySlice(newer)

	before := len(su.Calls())
	eh.OnDelete(toUnstructured(t, older))

	calls := su.Calls()[before:]
	if len(calls) != 1 || calls[0].Name != "newer" {
		t.Errorf("expected promotion of newer, got %+v", calls)
	}
}

func TestEventHandler_DeleteUnwrapsTombstone(t *testing.T) {
	// Use a sibling so the unwrap branch's effect is observable: removing
	// `older` via tombstone must promote `newer` and enqueue exactly 1 update.
	h, su := newHandler(t)
	eh := h.eventHandler()

	older := mkSlice("ns", "older", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.1.1.1"))
	newer := mkSlice("ns", "newer", "u2", time.Unix(3600, 0), 1, aRecord("foo.example.com.", "2.2.2.2"))
	h.applySlice(older)
	h.applySlice(newer)
	before := len(su.Calls())

	tombstone := cache.DeletedFinalStateUnknown{
		Key: "ns/older",
		Obj: toUnstructured(t, older),
	}
	eh.OnDelete(tombstone)

	calls := su.Calls()[before:]
	if len(calls) != 1 || calls[0].Name != "newer" {
		t.Fatalf("expected tombstone unwrap to promote newer, got %+v", calls)
	}
}

func TestEventHandler_DeleteTombstoneWithGarbageObj(t *testing.T) {
	h, su := newHandler(t)
	eh := h.eventHandler()

	before := len(su.Calls())
	eh.OnDelete(cache.DeletedFinalStateUnknown{Key: "ns/x", Obj: "garbage"})
	if got := len(su.Calls()) - before; got != 0 {
		t.Errorf("garbage tombstone Obj should be ignored, got %d enqueues", got)
	}
}

func TestEventHandler_BadObjectsAreLoggedAndIgnored(t *testing.T) {
	h, su := newHandler(t)
	eh := h.eventHandler()

	// Each handler should swallow conversion errors and not crash.
	eh.OnAdd("not-an-object", false)
	eh.OnUpdate("nope", 42)
	eh.OnDelete(struct{}{})

	if got := len(su.Calls()); got != 0 {
		t.Errorf("expected 0 enqueues for bad inputs, got %d", got)
	}
}

func TestEventHandler_MalformedUnstructured(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]interface{}
	}{
		{
			"entries is a string instead of array",
			map[string]interface{}{"spec": map[string]interface{}{"entries": "not-a-list"}},
		},
		{
			"a field is a string instead of object",
			map[string]interface{}{
				"spec": map[string]interface{}{
					"entries": []interface{}{
						map[string]interface{}{
							"name": "x.example.com.",
							"type": "A",
							"a":    "1.2.3.4", // should be {address: "1.2.3.4"}
						},
					},
				},
			},
		},
		{
			"ttl is a string instead of number",
			map[string]interface{}{
				"spec": map[string]interface{}{
					"entries": []interface{}{
						map[string]interface{}{
							"name": "x.example.com.",
							"type": "A",
							"ttl":  "thirty",
						},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, su := newHandler(t)
			eh := h.eventHandler()
			eh.OnAdd(&unstructured.Unstructured{Object: tc.obj}, false)
			if got := len(su.Calls()); got != 0 {
				t.Errorf("expected 0 enqueues for malformed object, got %d", got)
			}
		})
	}
}


// ---------- defensive equality helpers used in assertions ----------

func TestEventHandler_UpdateRefiresOnSameGenerationDifferentResourceVersion(t *testing.T) {
	// Informers refire OnUpdate whenever resourceVersion changes (e.g. status
	// subresource patches). The plugin must reapply unconditionally so the
	// in-memory index reflects the latest spec — even if Generation didn't move.
	h, su := newHandler(t)
	eh := h.eventHandler()

	old := mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4"))
	old.ResourceVersion = "100"
	new := mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4"))
	new.ResourceVersion = "101"

	eh.OnUpdate(toUnstructured(t, old), toUnstructured(t, new))

	if got := len(su.Calls()); got != 1 {
		t.Errorf("expected 1 enqueue on RV-only update, got %d", got)
	}
}

func TestEventHandler_ConcurrentApplyIsRaceFree(t *testing.T) {
	// Informers serialize per-key in real life, but events.go uses no per-call
	// locking — correctness comes from Index's lock. Run -race over many
	// concurrent applies on disjoint slices to pin that.
	h, _ := newHandler(t)
	eh := h.eventHandler()

	const goroutines = 8
	const iterations = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				name := "n" + string(rune('A'+gid))
				slice := mkSlice("ns", name, "uid"+name, time.Unix(int64(i), 0), int64(i+1),
					aRecord("foo.example.com.", "1.2.3.4"))
				eh.OnAdd(toUnstructured(t, slice), false)
			}
		}(g)
	}
	wg.Wait()
}

// TestFake_CallsReturnsCopy is a sanity test on the fakeStatusUpdater helper,
// not on production code. Catches regressions if the helper is later changed.
func TestFake_CallsReturnsCopy(t *testing.T) {
	su := newFakeStatusUpdater()
	su.Enqueue("ns", "n", 1, index.UpsertResult{Won: []string{"x A"}})
	first := su.Calls()
	first[0].Name = "MUTATED"
	again := su.Calls()
	if reflect.DeepEqual(first, again) {
		t.Errorf("Calls() must return a copy; mutation leaked: %+v", again)
	}
}
