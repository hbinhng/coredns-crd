package crd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/workqueue"

	"github.com/hbinhng/coredns-crd/internal/index"
)

type statusEvent struct {
	namespace  string
	name       string
	generation int64
	result     index.UpsertResult
}

type StatusUpdater interface {
	Enqueue(namespace, name string, generation int64, result index.UpsertResult)
	Run(ctx context.Context)
}

// statusUpdater dedupes per-slice status writes via a queue keyed by namespace/name,
// with the latest payload held in a side map. Run consults isLeader before each
// patch so non-leader replicas leave pending entries intact for whoever is.
type statusUpdater struct {
	client   dynamic.Interface
	isLeader func() bool
	queue    workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]statusEvent
}

// NewStatusUpdater builds a StatusUpdater. isLeader gates whether Run actually
// drains the queue; pass nil for the always-leader behavior used by single-
// replica deployments and tests.
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

// Enqueue overwrites the pending payload for the slice and adds the key to
// the workqueue. The informer guarantees per-key serialization so concurrent
// Enqueue for the same key is not expected; if a future caller needs to
// race-enqueue, add a monotonic guard on `generation` here.
func (s *statusUpdater) Enqueue(namespace, name string, generation int64, result index.UpsertResult) {
	key := namespace + "/" + name
	s.mu.Lock()
	s.pending[key] = statusEvent{
		namespace:  namespace,
		name:       name,
		generation: generation,
		result:     result,
	}
	s.mu.Unlock()
	s.queue.Add(key)
}

func (s *statusUpdater) take(key string) (statusEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.pending[key]
	if ok {
		delete(s.pending, key)
	}
	return ev, ok
}

func (s *statusUpdater) restore(key string, ev statusEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, has := s.pending[key]; !has {
		s.pending[key] = ev
	}
}

// Run drains the workqueue, patching /status for the slice referenced by each
// key. The loop is gated by isLeader: when false, keys are Done'd without
// patching and pending entries remain in the side map.
//
// Leadership acquisition does NOT auto-drain. The caller is expected to
// trigger a reconcile sweep on OnStartedLeading (Handler.reconcileAll
// re-enqueues every known slice), which wakes the queue and unblocks
// the side-map entries that were Done'd while we were not leader.
//
// If a leadership flip happens mid-process (after isLeader returned true
// but before Patch returns), the in-flight write completes; the new leader
// will overwrite it on its own reconcile sweep. Status patches are merge
// patches and idempotent enough that this is safe.
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
			// Not leader: leave the pending entry intact for whoever is, but
			// Forget any rate-limiter state so a future leader doesn't inherit
			// backoff accumulated under our previous leadership window.
			s.queue.Forget(key)
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

func (s *statusUpdater) process(ctx context.Context, ev statusEvent) error {
	now := metav1.Now()

	ready := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "Applied",
		Message:            fmt.Sprintf("%d entries indexed", len(ev.result.Won)),
		ObservedGeneration: ev.generation,
	}
	if len(ev.result.ParseErrors) > 0 {
		ready.Status = metav1.ConditionFalse
		ready.Reason = "ParseError"
		ready.Message = strings.Join(ev.result.ParseErrors, "; ")
	}

	conflicting := metav1.Condition{
		Type:               "Conflicting",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "NoConflict",
		Message:            "all entries owned",
		ObservedGeneration: ev.generation,
	}
	if len(ev.result.Lost) > 0 {
		conflicting.Status = metav1.ConditionTrue
		conflicting.Reason = "OutbidByOlder"
		msgs := make([]string, 0, len(ev.result.Lost))
		for _, k := range ev.result.Lost {
			msgs = append(msgs, fmt.Sprintf("%s owned by %s", k, ev.result.LostTo[k]))
		}
		conflicting.Message = strings.Join(msgs, "; ")
	}

	patch := map[string]any{
		"status": map[string]any{
			"observedGeneration": ev.generation,
			"conditions":         []metav1.Condition{ready, conflicting},
			"conflictedEntries":  ev.result.Lost,
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = s.client.Resource(dnsSliceGVR).
		Namespace(ev.namespace).
		Patch(ctx, ev.name, types.MergePatchType, data, metav1.PatchOptions{}, "status")
	return err
}
