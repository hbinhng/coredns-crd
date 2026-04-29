// Package events emits Kubernetes Events on DNSSlice resources when their
// Conflicting condition transitions True or False — transitions only, never
// on every reconcile.
package events

import (
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/index"
)

// Emitter wraps a record.EventRecorder and tracks per-slice "had conflicts
// last time" state so it only fires Events on real transitions.
//
// State is keyed by namespace/name and pruned by OnDelete. If a Delete event
// is ever dropped (informer watch gap during controller restart), the entry
// leaks until the slice key is reused — bounded by cluster slice count, not
// a practical concern.
type Emitter struct {
	recorder record.EventRecorder

	mu  sync.Mutex
	had map[string]bool
}

// NewEmitter returns an Emitter. A nil recorder is allowed: OnApply/OnDelete
// become no-ops, useful for tests and any code path that wants to disable
// Event emission without conditional checks.
func NewEmitter(r record.EventRecorder) *Emitter {
	return &Emitter{recorder: r, had: map[string]bool{}}
}

// OnApply fires a transition Event if the slice's conflict state flipped,
// and returns the direction it fired ("detected", "resolved", or "" for
// none) so callers can bump correlated metrics.
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

// OnDelete clears tracked state for the slice so a future re-apply with
// the same identity is treated as fresh.
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
