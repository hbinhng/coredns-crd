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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recorded{o, t, r, m})
}
func (f *fakeRecorder) snapshot() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{})
	if got != "" {
		t.Errorf("OnApply returned %q, want empty", got)
	}
	if n := len(r.snapshot()); n != 0 {
		t.Errorf("expected 0 events, got %d", n)
	}
}

func TestEmitter_DetectedOnConflictAppearance(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	})
	if got != "detected" {
		t.Errorf("OnApply returned %q, want detected", got)
	}
	events := r.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].reason != "ConflictDetected" || events[0].eventType != corev1.EventTypeWarning {
		t.Errorf("unexpected event: %+v", events[0])
	}
	if !strings.Contains(events[0].message, "foo. A owned by ns/older") {
		t.Errorf("message missing detail: %q", events[0].message)
	}
}

func TestEmitter_NoEventOnRepeatedConflict(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	res := index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	}
	first := e.OnApply(mkSlice("ns", "a"), res)
	second := e.OnApply(mkSlice("ns", "a"), res)
	if first != "detected" {
		t.Errorf("first apply: got %q, want detected", first)
	}
	if second != "" {
		t.Errorf("second apply (no transition): got %q, want empty", second)
	}
	if n := len(r.snapshot()); n != 1 {
		t.Errorf("expected 1 event total, got %d", n)
	}
}

func TestEmitter_ResolvedOnConflictClearance(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	conflict := index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	}
	e.OnApply(mkSlice("ns", "a"), conflict)
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{})
	if got != "resolved" {
		t.Errorf("clearance: got %q, want resolved", got)
	}
	events := r.snapshot()
	if len(events) != 2 {
		t.Fatalf("expected detected + resolved, got %d events", len(events))
	}
	if events[1].reason != "ConflictResolved" || events[1].eventType != corev1.EventTypeNormal {
		t.Errorf("second event wrong: %+v", events[1])
	}
}

func TestEmitter_NoEventOnRepeatedClean(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	e.OnApply(mkSlice("ns", "a"), index.UpsertResult{})
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{})
	if got != "" {
		t.Errorf("clean→clean: got %q, want empty", got)
	}
	if n := len(r.snapshot()); n != 0 {
		t.Errorf("expected 0 events, got %d", n)
	}
}

func TestEmitter_OnDeleteClearsState(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	conflict := index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	}
	e.OnApply(mkSlice("ns", "a"), conflict)
	e.OnDelete(mkSlice("ns", "a"))
	// After delete, a fresh apply with conflict should fire detected again,
	// not be silenced by leftover state.
	got := e.OnApply(mkSlice("ns", "a"), conflict)
	if got != "detected" {
		t.Errorf("post-delete re-apply: got %q, want detected", got)
	}
	events := r.snapshot()
	if len(events) != 2 || events[1].reason != "ConflictDetected" {
		t.Errorf("expected two ConflictDetected around a delete, got %+v", events)
	}
}

func TestEmitter_KeysAreNamespaceScoped(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	conflict := index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/older"},
	}
	// Same name across two namespaces must not share state.
	e.OnApply(mkSlice("ns1", "shared"), conflict)
	e.OnApply(mkSlice("ns2", "shared"), conflict)
	if n := len(r.snapshot()); n != 2 {
		t.Errorf("expected 2 events (one per namespace), got %d", n)
	}
}

func TestEmitter_OnDeleteUnknownKey_NoOp(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	e.OnDelete(mkSlice("ns", "ghost")) // must not panic
	if n := len(r.snapshot()); n != 0 {
		t.Errorf("OnDelete must not emit events, got %d", n)
	}
}

func TestEmitter_NilRecorder_NoOp(t *testing.T) {
	e := NewEmitter(nil)
	got := e.OnApply(mkSlice("ns", "a"), index.UpsertResult{
		Lost:   []string{"foo. A"},
		LostTo: map[string]string{"foo. A": "ns/o"},
	})
	if got != "" {
		t.Errorf("nil recorder: OnApply returned %q, want empty", got)
	}
	e.OnDelete(mkSlice("ns", "a")) // must not panic
}

func TestEmitter_ConcurrentApplies_RaceFree(t *testing.T) {
	r := &fakeRecorder{}
	e := NewEmitter(r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.OnApply(mkSlice("ns", "x"), index.UpsertResult{
					Lost:   []string{"foo. A"},
					LostTo: map[string]string{"foo. A": "ns/o"},
				})
			}
		}()
	}
	wg.Wait()
}

func TestDetectedMsg_FormatsAllLostKeys(t *testing.T) {
	msg := detectedMsg(index.UpsertResult{
		Lost: []string{"a A", "b A"},
		LostTo: map[string]string{
			"a A": "ns/x",
			"b A": "ns/y",
		},
	})
	want := "FQDNs lost LWW: a A owned by ns/x; b A owned by ns/y"
	if msg != want {
		t.Errorf("detectedMsg=%q, want %q", msg, want)
	}
}
