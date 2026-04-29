package crd

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hbinhng/coredns-crd/internal/metrics"
)

// ---------- stubs ----------

type fakeWriter struct {
	msg    *dns.Msg
	err    error
	writes int // number of WriteMsg calls — pins the "exactly one write" contract
}

func (f *fakeWriter) LocalAddr() net.Addr  { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (f *fakeWriter) RemoteAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (f *fakeWriter) WriteMsg(m *dns.Msg) error {
	f.msg = m
	f.writes++
	return f.err
}
func (f *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeWriter) Close() error                { return nil }
func (f *fakeWriter) TsigStatus() error           { return nil }
func (f *fakeWriter) TsigTimersOnly(bool)         {}
func (f *fakeWriter) Hijack()                     {}

type fakeNext struct {
	called bool
	rcode  int
	err    error
}

func (n *fakeNext) Name() string { return "next" }
func (n *fakeNext) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	n.called = true
	return n.rcode, n.err
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

// ---------- ServeDNS ----------

func TestServeDNS_Hit_AReturnsAnswer(t *testing.T) {
	metrics.ResetForTest()
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		aRecord("foo.example.com.", "1.2.3.4"),
	))

	q := query("foo.example.com.", dns.TypeA)
	q.Id = 0x4242
	w := &fakeWriter{}
	rcode, err := h.ServeDNS(context.Background(), w, q)
	if err != nil {
		t.Fatalf("ServeDNS error: %v", err)
	}
	if rcode != dns.RcodeSuccess {
		t.Errorf("rcode=%d, want %d", rcode, dns.RcodeSuccess)
	}
	if w.writes != 1 {
		t.Errorf("expected exactly 1 WriteMsg call, got %d", w.writes)
	}
	if w.msg == nil || len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer RR, got %v", w.msg)
	}
	if w.msg.Id != 0x4242 {
		t.Errorf("Id mirror: got 0x%x want 0x4242", w.msg.Id)
	}
	if len(w.msg.Question) != 1 || w.msg.Question[0].Name != "foo.example.com." {
		t.Errorf("Question section not mirrored: %+v", w.msg.Question)
	}
	rr, ok := w.msg.Answer[0].(*dns.A)
	if !ok || rr.A.String() != "1.2.3.4" {
		t.Errorf("answer mismatch: %v", w.msg.Answer[0])
	}
	if w.msg.Authoritative {
		t.Errorf("AA bit set, want clear (non-authoritative plugin)")
	}
	if got := testutil.ToFloat64(metrics.LookupsForTest("hit")); got != 1 {
		t.Errorf("metrics.lookups{hit}=%v, want 1", got)
	}
}

func TestServeDNS_Hit_RoundRobinMultipleA(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		aRecord("foo.example.com.", "1.2.3.4"),
		aRecord("foo.example.com.", "5.6.7.8"),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("foo.example.com.", dns.TypeA))
	if rcode != dns.RcodeSuccess {
		t.Fatalf("rcode=%d", rcode)
	}
	if len(w.msg.Answer) != 2 {
		t.Fatalf("expected 2 answers (round-robin), got %d", len(w.msg.Answer))
	}
	got := map[string]bool{}
	for _, ans := range w.msg.Answer {
		if rr, ok := ans.(*dns.A); ok {
			got[rr.A.String()] = true
		}
	}
	if !got["1.2.3.4"] || !got["5.6.7.8"] {
		t.Errorf("missing addresses in round-robin answer: %v", got)
	}
}

func TestServeDNS_Hit_AAAAReturnsAnswer(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		aaaaRecord("foo.example.com.", "2001:db8::1"),
	))

	w := &fakeWriter{}
	rcode, err := h.ServeDNS(context.Background(), w, query("foo.example.com.", dns.TypeAAAA))
	if err != nil || rcode != dns.RcodeSuccess {
		t.Fatalf("rcode=%d err=%v", rcode, err)
	}
	if rr, ok := w.msg.Answer[0].(*dns.AAAA); !ok || rr.AAAA.String() != "2001:db8::1" {
		t.Errorf("answer mismatch: %v", w.msg.Answer[0])
	}
}

func TestServeDNS_Hit_TXT(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		txtRecord("foo.example.com.", "v=spf1 -all", `value with "quotes" and \backslash`),
	))
	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("foo.example.com.", dns.TypeTXT))
	if rcode != dns.RcodeSuccess {
		t.Errorf("rcode=%d", rcode)
	}
	rr, ok := w.msg.Answer[0].(*dns.TXT)
	if !ok || len(rr.Txt) != 2 {
		t.Fatalf("answer mismatch: %v", w.msg.Answer[0])
	}
	if rr.Txt[0] != "v=spf1 -all" || rr.Txt[1] != `value with "quotes" and \backslash` {
		t.Errorf("TXT round-trip lost characters: %#v", rr.Txt)
	}
}

func TestServeDNS_CNAMEFallback_AQuery(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		cnameRecord("alias.example.com.", "real.example.com."),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("alias.example.com.", dns.TypeA))
	if rcode != dns.RcodeSuccess {
		t.Fatalf("rcode=%d, want success (CNAME fallback)", rcode)
	}
	if rr, ok := w.msg.Answer[0].(*dns.CNAME); !ok || rr.Target != "real.example.com." {
		t.Errorf("expected CNAME RR, got %v", w.msg.Answer[0])
	}
}

func TestServeDNS_CNAMEFallback_AAAAQuery(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		cnameRecord("alias.example.com.", "real.example.com."),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("alias.example.com.", dns.TypeAAAA))
	if rcode != dns.RcodeSuccess {
		t.Fatalf("rcode=%d", rcode)
	}
	if _, ok := w.msg.Answer[0].(*dns.CNAME); !ok {
		t.Errorf("expected CNAME for AAAA-on-CNAME-name, got %v", w.msg.Answer[0])
	}
}

func TestServeDNS_CNAMEDoesNotFallbackForOtherTypes(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		cnameRecord("alias.example.com.", "real.example.com."),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("alias.example.com.", dns.TypeTXT))
	if rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN for TXT-on-CNAME-only name, got %d", rcode)
	}
}

// Pins single-step CNAME behavior: even when the index also has the A record
// for the CNAME's target, ServeDNS returns ONLY the CNAME and lets the
// resolver re-query. This is RFC-compliant and intentional (no in-plugin chase).
func TestServeDNS_CNAMESingleStep_DoesNotChase(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		cnameRecord("alias.example.com.", "real.example.com."),
		aRecord("real.example.com.", "10.0.0.1"),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("alias.example.com.", dns.TypeA))
	if rcode != dns.RcodeSuccess || len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 answer (CNAME only), got rcode=%d answers=%d", rcode, len(w.msg.Answer))
	}
	if _, ok := w.msg.Answer[0].(*dns.CNAME); !ok {
		t.Errorf("expected CNAME RR (not chased to A), got %v", w.msg.Answer[0])
	}
}

func TestServeDNS_Miss_WithFallthrough_CallsNext(t *testing.T) {
	h, _ := newHandler(t)
	h.Fall = fall.F{}
	h.Fall.SetZonesFromArgs(nil)

	next := &fakeNext{rcode: dns.RcodeSuccess}
	h.Next = next

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("nope.example.com.", dns.TypeA))
	if !next.called {
		t.Errorf("expected fallthrough to call next plugin")
	}
	if rcode != dns.RcodeSuccess {
		t.Errorf("expected to propagate next plugin's rcode, got %d", rcode)
	}
	// Critical: the plugin must NOT write its own NXDOMAIN before delegating.
	if w.writes != 0 {
		t.Errorf("plugin wrote %d times before fallthrough; must be 0", w.writes)
	}
}

func TestServeDNS_FallthroughZoneMatching(t *testing.T) {
	cases := []struct {
		name       string
		zone       string
		qname      string
		shouldFall bool
	}{
		{"exact zone match", "only.this.zone.", "only.this.zone.", true},
		{"subdomain of zone", "only.this.zone.", "sub.only.this.zone.", true},
		{"unrelated zone", "only.this.zone.", "outside.example.com.", false},
		{"sibling zone (not subdomain)", "this.zone.", "other.zone.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHandler(t)
			h.Fall = fall.F{}
			h.Fall.SetZonesFromArgs([]string{tc.zone})
			next := &fakeNext{rcode: dns.RcodeSuccess}
			h.Next = next
			w := &fakeWriter{}
			h.ServeDNS(context.Background(), w, query(tc.qname, dns.TypeA))
			if next.called != tc.shouldFall {
				t.Errorf("fallthrough(%q in zone %q): got %v, want %v",
					tc.qname, tc.zone, next.called, tc.shouldFall)
			}
		})
	}
}


func TestServeDNS_Miss_FallthroughZoneNotMatching_NXDOMAIN(t *testing.T) {
	h, _ := newHandler(t)
	h.Fall = fall.F{}
	h.Fall.SetZonesFromArgs([]string{"only.this.zone."})

	next := &fakeNext{}
	h.Next = next

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("outside.example.com.", dns.TypeA))
	if next.called {
		t.Errorf("must NOT fall through for non-matching zone")
	}
	if rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN, got %d", rcode)
	}
	if w.msg == nil || w.msg.Rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN response written, got %+v", w.msg)
	}
}

func TestServeDNS_Miss_NoFallthrough_NXDOMAIN(t *testing.T) {
	h, _ := newHandler(t)

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("nope.example.com.", dns.TypeA))
	if rcode != dns.RcodeNameError {
		t.Errorf("expected NXDOMAIN, got %d", rcode)
	}
}

func TestServeDNS_WriteError_PropagatesServerFailure(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		aRecord("foo.example.com.", "1.2.3.4"),
	))

	w := &fakeWriter{err: errors.New("network blew up")}
	rcode, err := h.ServeDNS(context.Background(), w, query("foo.example.com.", dns.TypeA))
	if err == nil {
		t.Errorf("expected error to be propagated")
	}
	if rcode != dns.RcodeServerFailure {
		t.Errorf("rcode=%d, want SERVFAIL", rcode)
	}
}

func TestServeDNS_QueryNameLowercasing(t *testing.T) {
	h, _ := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1,
		aRecord("foo.example.com.", "1.2.3.4"),
	))

	w := &fakeWriter{}
	rcode, _ := h.ServeDNS(context.Background(), w, query("FOO.EXAMPLE.COM.", dns.TypeA))
	if rcode != dns.RcodeSuccess || len(w.msg.Answer) != 1 {
		t.Errorf("uppercase query should resolve, got rcode=%d msg=%+v", rcode, w.msg)
	}
}

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
	gen := map[string]int64{}
	for _, c := range calls {
		gen[c.Name] = c.Generation
	}
	if gen["a"] != 1 || gen["b"] != 2 {
		t.Errorf("reconcileAll generations wrong: %v", gen)
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
	// Sanity: the index actually has the slice — otherwise the no-panic
	// assertion below is vacuous.
	if got := h.idx.AllSnapshots(); len(got) != 1 {
		t.Fatalf("setup: expected index to have 1 slice, got %d", len(got))
	}
	h.reconcileAll() // must not panic
}

func TestReconcileAll_ConcurrentWithApplySlice(t *testing.T) {
	// Locks contract: AllSnapshots reads with RLock; applySlice writes with
	// Lock. Iterating reconcileAll concurrently with apply-storms must be
	// race-free. -race amplifies the assertion; the value asserts no panic
	// + a non-empty index at the end.
	h, _ := newHandler(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				h.applySlice(mkSlice("ns", "n"+string(rune('a'+i%4)), "u",
					time.Unix(int64(i), 0), int64(i), aRecord("foo.example.com.", "1.2.3.4")))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.reconcileAll()
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if len(h.idx.AllSnapshots()) == 0 {
		t.Errorf("expected non-empty index after concurrent apply storm")
	}
}

func TestHandlerName(t *testing.T) {
	h, _ := newHandler(t)
	if h.Name() != "crd" {
		t.Errorf("Name()=%q, want %q", h.Name(), "crd")
	}
}
