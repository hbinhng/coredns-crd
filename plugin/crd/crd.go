package crd

import (
	"context"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"

	"github.com/hbinhng/coredns-crd/internal/events"
	"github.com/hbinhng/coredns-crd/internal/index"
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

type Handler struct {
	Next plugin.Handler
	Fall fall.F

	cfg           *config
	idx           *index.Index
	cancel        context.CancelFunc
	statusUpdater StatusUpdater
	emitter       *events.Emitter
}

func New(cfg *config) *Handler {
	return &Handler{
		cfg:     cfg,
		idx:     index.New(),
		Fall:    cfg.Fall,
		emitter: events.NewEmitter(nil), // no-op until setup.go wires a recorder
	}
}

func (h *Handler) Name() string { return pluginName }

// reconcileAll enqueues a fresh status snapshot for every slice in the index.
// Wired to OnStartedLeading in setup.go: when this pod becomes leader, the
// published /status surface converges to the current Index state regardless
// of whatever the previous leader did or didn't write.
func (h *Handler) reconcileAll() {
	if h.statusUpdater == nil {
		return
	}
	snaps := h.idx.AllSnapshots()
	for _, s := range snaps {
		h.statusUpdater.Enqueue(s.Namespace, s.Name, s.Generation, s.Result)
	}
	log.Infof("reconcileAll: enqueued %d slices", len(snaps))
}

func (h *Handler) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.QName()
	qtype := state.QType()

	rrs := h.idx.Lookup(qname, qtype)
	cnameFallback := false
	if len(rrs) == 0 && (qtype == dns.TypeA || qtype == dns.TypeAAAA) {
		// RFC 1034: a CNAME for the queried name should be returned for A/AAAA queries.
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
