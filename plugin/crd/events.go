package crd

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

func (h *Handler) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			slice, err := toTyped(obj)
			if err != nil {
				log.Errorf("convert add: %v", err)
				return
			}
			h.applySlice(slice)
		},
		UpdateFunc: func(_, obj interface{}) {
			slice, err := toTyped(obj)
			if err != nil {
				log.Errorf("convert update: %v", err)
				return
			}
			h.applySlice(slice)
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			slice, err := toTyped(obj)
			if err != nil {
				log.Errorf("convert delete: %v", err)
				return
			}
			h.deleteSlice(slice)
		},
	}
}

func toTyped(obj interface{}) (*apiv1.DNSSlice, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	out := &apiv1.DNSSlice{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Handler) applySlice(slice *apiv1.DNSSlice) {
	primary, siblings := h.idx.Upsert(slice)
	log.Infof("applied DNSSlice %s/%s: won=%d lost=%d errors=%d siblings=%d",
		slice.Namespace, slice.Name,
		len(primary.Won), len(primary.Lost), len(primary.ParseErrors), len(siblings))

	parsed := len(primary.Won) + len(primary.Lost)
	switch {
	case len(primary.ParseErrors) > 0 && parsed > 0:
		metrics.RecordApply("partial")
	case len(primary.ParseErrors) > 0:
		metrics.RecordApply("parse_error")
	default:
		metrics.RecordApply("applied")
	}
	if dir := h.emitter.OnApply(slice, primary); dir != "" {
		metrics.RecordConflictTransition(dir)
	}

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
