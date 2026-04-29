package index

import (
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
)

// ---------- helpers ----------

func ptr[T any](v T) *T { return &v }

func mkSlice(ns, name, uid string, creation time.Time, gen int64, defTTL *int32, entries ...apiv1.DNSEntry) *apiv1.DNSSlice {
	return &apiv1.DNSSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              name,
			UID:               types.UID(uid),
			Generation:        gen,
			CreationTimestamp: metav1.NewTime(creation),
		},
		Spec: apiv1.DNSSliceSpec{
			DefaultTTL: defTTL,
			Entries:    entries,
		},
	}
}

func a(name, ip string, ttl *int32) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "A", TTL: ttl, A: &apiv1.ARecord{Address: ip}}
}
func aaaa(name, ip string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "AAAA", AAAA: &apiv1.AAAARecord{Address: ip}}
}
func cname(name, target string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "CNAME", CNAME: &apiv1.CNAMERecord{Target: target}}
}
func txt(name string, vals ...string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "TXT", TXT: &apiv1.TXTRecord{Values: vals}}
}
func srv(name string, prio, weight, port int32, target string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: "SRV", SRV: &apiv1.SRVRecord{
		Priority: prio, Weight: weight, Port: port, Target: target,
	}}
}
func raw(name, typ, rdata string) apiv1.DNSEntry {
	return apiv1.DNSEntry{Name: name, Type: typ, Raw: &apiv1.RawRecord{RData: rdata}}
}

func t0() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// ---------- parseSlice / buildRR ----------

func TestParseSlice_AllRecordTypes(t *testing.T) {
	s := mkSlice("ns", "n", "uid", t0(), 1, nil,
		a("a.example.com.", "10.0.0.1", nil),
		aaaa("a6.example.com.", "2001:db8::1"),
		cname("alias.example.com.", "a.example.com."),
		txt("t.example.com.", "v=spf1 -all"),
		srv("_http._tcp.example.com.", 10, 100, 80, "a.example.com."),
		raw("c.example.com.", "CAA", `0 issue "letsencrypt.org"`),
	)
	sc, errs := parseSlice(s)
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}
	if got := len(sc.rrs); got != 6 {
		t.Fatalf("expected 6 record keys, got %d", got)
	}
	for k, rrs := range sc.rrs {
		if len(rrs) != 1 {
			t.Errorf("key %v: expected 1 RR, got %d", k, len(rrs))
		}
		if rrs[0].Header().Ttl != DefaultTTL {
			t.Errorf("key %v: expected default TTL %d, got %d", k, DefaultTTL, rrs[0].Header().Ttl)
		}
	}
}

func TestParseSlice_TTLDefaults(t *testing.T) {
	t.Run("uses spec.defaultTTL when set", func(t *testing.T) {
		s := mkSlice("ns", "n", "uid", t0(), 1, ptr(int32(60)),
			a("foo.example.com.", "1.2.3.4", nil),
		)
		sc, _ := parseSlice(s)
		for _, rrs := range sc.rrs {
			if rrs[0].Header().Ttl != 60 {
				t.Errorf("got TTL %d, want 60", rrs[0].Header().Ttl)
			}
		}
	})
	t.Run("per-entry TTL overrides default", func(t *testing.T) {
		s := mkSlice("ns", "n", "uid", t0(), 1, ptr(int32(60)),
			a("foo.example.com.", "1.2.3.4", ptr(int32(15))),
		)
		sc, _ := parseSlice(s)
		for _, rrs := range sc.rrs {
			if rrs[0].Header().Ttl != 15 {
				t.Errorf("got TTL %d, want 15", rrs[0].Header().Ttl)
			}
		}
	})
	t.Run("zero CreationTimestamp leaves creationUnixNano at 0", func(t *testing.T) {
		s := &apiv1.DNSSlice{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "n", UID: "uid"},
			Spec:       apiv1.DNSSliceSpec{Entries: []apiv1.DNSEntry{a("foo.example.com.", "1.2.3.4", nil)}},
		}
		sc, _ := parseSlice(s)
		if sc.creationUnixNano != 0 {
			t.Errorf("expected creationUnixNano=0 for zero timestamp, got %d", sc.creationUnixNano)
		}
	})
}

func TestParseSlice_TTLEdgeValues(t *testing.T) {
	cases := []struct {
		name string
		ttl  *int32
		want uint32
	}{
		{"zero", ptr(int32(0)), 0},
		{"max int32", ptr(int32(math.MaxInt32)), uint32(math.MaxInt32)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mkSlice("ns", "n", "uid", t0(), 1, nil,
				a("foo.example.com.", "1.2.3.4", tc.ttl),
			)
			sc, _ := parseSlice(s)
			for _, rrs := range sc.rrs {
				if rrs[0].Header().Ttl != tc.want {
					t.Errorf("got TTL %d, want %d", rrs[0].Header().Ttl, tc.want)
				}
			}
		})
	}
}

func TestParseSlice_EmptyEntries(t *testing.T) {
	s := mkSlice("ns", "n", "uid", t0(), 1, nil)
	sc, errs := parseSlice(s)
	if len(sc.rrs) != 0 {
		t.Errorf("expected 0 rrs, got %v", sc.rrs)
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %v", errs)
	}
	// And the index accepts and serves nothing.
	idx := New()
	primary, siblings := idx.Upsert(s)
	if len(primary.Won) != 0 || len(primary.Lost) != 0 || len(siblings) != 0 {
		t.Errorf("empty entries should produce empty result, got %+v sibs=%v", primary, siblings)
	}
}

func TestParseSlice_FQDNNormalization(t *testing.T) {
	s := mkSlice("ns", "n", "uid", t0(), 1, nil,
		a("FOO.Example.COM", "1.2.3.4", nil), // no trailing dot, mixed case
	)
	sc, _ := parseSlice(s)
	want := recordKey{fqdn: "foo.example.com.", typ: dns.TypeA}
	if _, ok := sc.rrs[want]; !ok {
		t.Fatalf("expected normalized key %v, got %v", want, sc.rrs)
	}
}

func TestParseSlice_MultipleRRsAtSameKey(t *testing.T) {
	s := mkSlice("ns", "n", "uid", t0(), 1, nil,
		a("foo.example.com.", "10.0.0.1", nil),
		a("foo.example.com.", "10.0.0.2", nil),
	)
	sc, _ := parseSlice(s)
	k := recordKey{fqdn: "foo.example.com.", typ: dns.TypeA}
	if len(sc.rrs[k]) != 2 {
		t.Fatalf("expected 2 RRs for round-robin, got %d", len(sc.rrs[k]))
	}
}

func TestParseSlice_Errors(t *testing.T) {
	cases := []struct {
		name  string
		entry apiv1.DNSEntry
		want  string
	}{
		{"A missing .a", apiv1.DNSEntry{Name: "x.example.com.", Type: "A"}, "type=A requires .a"},
		{"AAAA missing .aaaa", apiv1.DNSEntry{Name: "x.example.com.", Type: "AAAA"}, "type=AAAA requires .aaaa"},
		{"CNAME missing .cname", apiv1.DNSEntry{Name: "x.example.com.", Type: "CNAME"}, "type=CNAME requires .cname"},
		{"TXT missing .txt", apiv1.DNSEntry{Name: "x.example.com.", Type: "TXT"}, "type=TXT requires .txt"},
		{"SRV missing .srv", apiv1.DNSEntry{Name: "x.example.com.", Type: "SRV"}, "type=SRV requires .srv"},
		{"unknown type without raw", apiv1.DNSEntry{Name: "x.example.com.", Type: "CAA"}, `unknown type "CAA" without .raw`},
		{"invalid IPv4", a("x.example.com.", "999.bad", nil), `invalid IPv4 "999.bad"`},
		{"IPv6 in A field", a("x.example.com.", "2001:db8::1", nil), `invalid IPv4 "2001:db8::1"`},
		{"invalid IPv6", aaaa("x.example.com.", "not-an-ip"), `invalid IPv6 "not-an-ip"`},
		{"IPv4 in AAAA field", aaaa("x.example.com.", "1.2.3.4"), `invalid IPv6 "1.2.3.4"`},
		{"raw with bad rdata", raw("x.example.com.", "CAA", "this is not valid CAA rdata"), "parse raw rdata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mkSlice("ns", "n", "uid", t0(), 1, nil, tc.entry)
			sc, errs := parseSlice(s)
			if len(sc.rrs) != 0 {
				t.Errorf("expected no RRs on parse error, got %v", sc.rrs)
			}
			if len(errs) != 1 {
				t.Fatalf("expected 1 parse error, got %d: %v", len(errs), errs)
			}
			if !strings.Contains(errs[0], tc.want) {
				t.Errorf("error %q does not contain %q", errs[0], tc.want)
			}
		})
	}
}

func TestParseSlice_PartialErrors(t *testing.T) {
	// Mix valid and invalid; valid ones survive, invalid ones report.
	s := mkSlice("ns", "n", "uid", t0(), 1, nil,
		a("good.example.com.", "1.2.3.4", nil),
		a("bad.example.com.", "999.999.999.999", nil),
	)
	sc, errs := parseSlice(s)
	if len(sc.rrs) != 1 {
		t.Fatalf("expected 1 valid RR, got %d", len(sc.rrs))
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 parse error, got %d", len(errs))
	}
}

func TestBuildRR_RecordContents(t *testing.T) {
	cases := []struct {
		name    string
		entry   apiv1.DNSEntry
		wantStr string // miekg canonical string suffix to match
	}{
		{"A", a("x.example.com.", "10.0.0.1", nil), "A\t10.0.0.1"},
		{"AAAA", aaaa("x.example.com.", "2001:db8::1"), "AAAA\t2001:db8::1"},
		{"CNAME with trailing dot", cname("x.example.com.", "y.example.com."), "CNAME\ty.example.com."},
		{"CNAME without trailing dot is auto-FQDN'd", cname("x.example.com.", "y.example.com"), "CNAME\ty.example.com."},
		{"TXT", txt("x.example.com.", "hello", "world"), `TXT	"hello" "world"`},
		{"SRV", srv("_http._tcp.x.example.com.", 10, 20, 80, "y.example.com."), "SRV\t10 20 80 y.example.com."},
		{"raw CAA", raw("x.example.com.", "CAA", `0 issue "letsencrypt.org"`), `CAA	0 issue "letsencrypt.org"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, _, err := buildRR("x.example.com.", 300, tc.entry)
			if err != nil {
				t.Fatalf("buildRR error: %v", err)
			}
			if !strings.Contains(rr.String(), tc.wantStr) {
				t.Errorf("rr.String()=%q, want substring %q", rr.String(), tc.wantStr)
			}
		})
	}
}

// ---------- Upsert / Delete / Lookup ----------

func TestUpsert_FreshInsertAllWon(t *testing.T) {
	idx := New()
	s := mkSlice("ns", "a", "uid-a", t0(), 1, nil,
		a("foo.example.com.", "1.2.3.4", nil),
		a("foo.example.com.", "1.2.3.5", nil),
		txt("foo.example.com.", "x"),
	)
	primary, siblings := idx.Upsert(s)
	if len(siblings) != 0 {
		t.Errorf("expected 0 siblings, got %d", len(siblings))
	}
	if !reflect.DeepEqual(primary.Won, []string{"foo.example.com. A", "foo.example.com. TXT"}) {
		t.Errorf("Won=%v", primary.Won)
	}
	if len(primary.Lost) != 0 {
		t.Errorf("Lost=%v, want empty", primary.Lost)
	}
	if len(primary.ParseErrors) != 0 {
		t.Errorf("ParseErrors=%v", primary.ParseErrors)
	}
	// Two A RRs at the same key both serve.
	rrs := idx.Lookup("foo.example.com.", dns.TypeA)
	if len(rrs) != 2 {
		t.Errorf("expected 2 A RRs, got %d", len(rrs))
	}
}

func TestUpsert_LWW_OlderWins(t *testing.T) {
	idx := New()
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))

	primaryOlder, sibsOlder := idx.Upsert(older)
	if len(primaryOlder.Won) != 1 || len(sibsOlder) != 0 {
		t.Fatalf("older insert: won=%v sibs=%d", primaryOlder.Won, len(sibsOlder))
	}
	primaryNewer, sibsNewer := idx.Upsert(newer)
	if len(primaryNewer.Won) != 0 {
		t.Errorf("newer should win nothing, got %v", primaryNewer.Won)
	}
	if !reflect.DeepEqual(primaryNewer.Lost, []string{"foo.example.com. A"}) {
		t.Errorf("newer Lost=%v", primaryNewer.Lost)
	}
	if primaryNewer.LostTo["foo.example.com. A"] != "ns/older" {
		t.Errorf("LostTo=%v", primaryNewer.LostTo)
	}
	if len(sibsNewer) != 0 {
		t.Errorf("older's snapshot didn't change, expected 0 siblings, got %v", sibsNewer)
	}
	rrs := idx.Lookup("foo.example.com.", dns.TypeA)
	if len(rrs) != 1 || rrs[0].(*dns.A).A.String() != "1.1.1.1" {
		t.Errorf("expected older's RR to win, got %v", rrs)
	}
}

func TestUpsert_OlderArrivesAfter_NewerLoses(t *testing.T) {
	idx := New()
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))

	idx.Upsert(newer) // initially wins by default

	primary, siblings := idx.Upsert(older)
	if !reflect.DeepEqual(primary.Won, []string{"foo.example.com. A"}) {
		t.Errorf("older should win, got Won=%v Lost=%v", primary.Won, primary.Lost)
	}
	if len(siblings) != 1 {
		t.Fatalf("expected 1 sibling status, got %d", len(siblings))
	}
	sb := siblings[0]
	if sb.Namespace != "ns" || sb.Name != "newer" {
		t.Errorf("unexpected sibling: %+v", sb)
	}
	if !reflect.DeepEqual(sb.Result.Lost, []string{"foo.example.com. A"}) {
		t.Errorf("sibling Lost=%v", sb.Result.Lost)
	}
	if sb.Result.LostTo["foo.example.com. A"] != "ns/older" {
		t.Errorf("sibling LostTo=%v", sb.Result.LostTo)
	}
}

func TestUpsert_UIDTiebreak(t *testing.T) {
	when := t0()
	t.Run("higher UID first, lower UID wins on arrival", func(t *testing.T) {
		idx := New()
		a1 := mkSlice("ns", "a1", "uid-a", when, 1, nil, a("foo.example.com.", "1.1.1.1", nil))
		a2 := mkSlice("ns", "a2", "uid-b", when, 1, nil, a("foo.example.com.", "2.2.2.2", nil))
		idx.Upsert(a2)
		primary, siblings := idx.Upsert(a1)
		if len(primary.Won) != 1 {
			t.Errorf("uid-a should win on tiebreak, got Won=%v", primary.Won)
		}
		if len(siblings) != 1 || siblings[0].Name != "a2" {
			t.Errorf("expected a2 to be the loser sibling, got %+v", siblings)
		}
	})
	t.Run("lower UID first, higher UID loses on arrival", func(t *testing.T) {
		idx := New()
		a1 := mkSlice("ns", "a1", "uid-a", when, 1, nil, a("foo.example.com.", "1.1.1.1", nil))
		a2 := mkSlice("ns", "a2", "uid-b", when, 1, nil, a("foo.example.com.", "2.2.2.2", nil))
		idx.Upsert(a1)
		primary, siblings := idx.Upsert(a2)
		if len(primary.Won) != 0 {
			t.Errorf("uid-b should lose to incumbent, got Won=%v", primary.Won)
		}
		if !reflect.DeepEqual(primary.Lost, []string{"foo.example.com. A"}) {
			t.Errorf("uid-b Lost=%v", primary.Lost)
		}
		if primary.LostTo["foo.example.com. A"] != "ns/a1" {
			t.Errorf("LostTo=%v", primary.LostTo)
		}
		if len(siblings) != 0 {
			t.Errorf("incumbent's snapshot didn't change, want 0 siblings, got %v", siblings)
		}
	})
}

func TestUpsert_ThreeWayCycle_WinLoseWin(t *testing.T) {
	// mid wins → oldest arrives, mid demoted → oldest deleted, mid re-promoted.
	idx := New()
	mid := mkSlice("ns", "mid", "u-mid", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	oldest := mkSlice("ns", "oldest", "u-old", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))

	primary, _ := idx.Upsert(mid)
	if len(primary.Won) != 1 {
		t.Fatalf("mid should win as first arrival, got %v", primary.Won)
	}

	_, siblings := idx.Upsert(oldest)
	if len(siblings) != 1 || siblings[0].Name != "mid" || len(siblings[0].Result.Lost) != 1 {
		t.Fatalf("mid should be demoted on oldest arrival, siblings=%+v", siblings)
	}

	siblings = idx.Delete("ns", "oldest")
	if len(siblings) != 1 || siblings[0].Name != "mid" {
		t.Fatalf("mid should be re-promoted on oldest delete, siblings=%+v", siblings)
	}
	if !reflect.DeepEqual(siblings[0].Result.Won, []string{"foo.example.com. A"}) {
		t.Errorf("re-promoted Won=%v", siblings[0].Result.Won)
	}
	if rrs := idx.Lookup("foo.example.com.", dns.TypeA); len(rrs) != 1 || rrs[0].(*dns.A).A.String() != "2.2.2.2" {
		t.Errorf("expected mid serving after oldest delete, got %v", rrs)
	}
}

func TestUpsert_GenerationPropagatesToSibling(t *testing.T) {
	idx := New()
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 7 /*generation*/, nil, a("foo.example.com.", "2.2.2.2", nil))
	idx.Upsert(newer)
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	_, siblings := idx.Upsert(older)
	if len(siblings) != 1 {
		t.Fatalf("setup")
	}
	if siblings[0].Generation != 7 {
		t.Errorf("sibling Generation=%d, want 7 (the cached generation of `newer`)", siblings[0].Generation)
	}
}

func TestUpsert_RRDataChange_DoesNotEmitSiblings(t *testing.T) {
	// When a winner re-applies with the same key set but different RR data,
	// no sibling's Won/Lost/LostTo changes — so no sibling status must be emitted.
	idx := New()
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "9.9.9.9", nil))
	idx.Upsert(older)
	idx.Upsert(newer)

	older2 := mkSlice("ns", "older", "u1", t0(), 2, nil, a("foo.example.com.", "5.5.5.5", nil)) // same key, different IP
	_, siblings := idx.Upsert(older2)
	if len(siblings) != 0 {
		t.Errorf("RR-data-only change must not emit siblings, got %+v", siblings)
	}
	// Sanity: served IP updated to newer winner data.
	rrs := idx.Lookup("foo.example.com.", dns.TypeA)
	if len(rrs) != 1 || rrs[0].(*dns.A).A.String() != "5.5.5.5" {
		t.Errorf("expected updated winner RR served, got %v", rrs)
	}
}

func TestUpsert_ReplaceSlice_PromotesLoser(t *testing.T) {
	idx := New()
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	idx.Upsert(older)
	idx.Upsert(newer) // newer is loser

	// Replace older's spec, removing foo.
	older2 := mkSlice("ns", "older", "u1", t0(), 2, nil, a("bar.example.com.", "9.9.9.9", nil))
	primary, siblings := idx.Upsert(older2)
	if !reflect.DeepEqual(primary.Won, []string{"bar.example.com. A"}) {
		t.Errorf("older2 should win bar, got Won=%v", primary.Won)
	}
	if len(siblings) != 1 || siblings[0].Name != "newer" {
		t.Fatalf("expected newer to be promoted as sibling, got %+v", siblings)
	}
	if !reflect.DeepEqual(siblings[0].Result.Won, []string{"foo.example.com. A"}) {
		t.Errorf("newer should now win foo, Won=%v", siblings[0].Result.Won)
	}
	rrs := idx.Lookup("foo.example.com.", dns.TypeA)
	if len(rrs) != 1 || rrs[0].(*dns.A).A.String() != "2.2.2.2" {
		t.Errorf("expected newer's RR after older relinquished, got %v", rrs)
	}
}

func TestUpsert_LostToTransitionEmitsSibling(t *testing.T) {
	idx := New()
	mid := mkSlice("ns", "mid", "u-mid", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	loser := mkSlice("ns", "loser", "u-loser", t0().Add(2*time.Hour), 1, nil, a("foo.example.com.", "3.3.3.3", nil))
	idx.Upsert(mid)
	idx.Upsert(loser) // loser's LostTo points to mid

	// Now an even-older slice wins, mid becomes a loser too, loser's LostTo flips to oldest.
	oldest := mkSlice("ns", "oldest", "u-old", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	primary, siblings := idx.Upsert(oldest)
	if !reflect.DeepEqual(primary.Won, []string{"foo.example.com. A"}) {
		t.Fatalf("oldest should win, got %v", primary.Won)
	}
	bySlice := map[string]SliceStatus{}
	for _, s := range siblings {
		bySlice[s.Name] = s
	}
	if got, ok := bySlice["mid"]; !ok || got.Result.LostTo["foo.example.com. A"] != "ns/oldest" {
		t.Errorf("mid sibling not updated correctly: %+v", got)
	}
	if got, ok := bySlice["loser"]; !ok || got.Result.LostTo["foo.example.com. A"] != "ns/oldest" {
		t.Errorf("loser sibling LostTo not flipped to oldest: %+v", got)
	}
}

func TestUpsert_NoOverlap_NoSiblingsEmitted(t *testing.T) {
	idx := New()
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil, a("a.example.com.", "1.1.1.1", nil)))
	_, siblings := idx.Upsert(mkSlice("ns", "b", "u2", t0().Add(time.Hour), 1, nil, a("b.example.com.", "2.2.2.2", nil)))
	if len(siblings) != 0 {
		t.Errorf("expected 0 siblings on disjoint upsert, got %v", siblings)
	}
}

func TestUpsert_ParseErrorsSurfaceInPrimaryAndPersistOnSnapshot(t *testing.T) {
	idx := New()
	s := mkSlice("ns", "a", "u1", t0(), 1, nil,
		a("good.example.com.", "1.2.3.4", nil),
		a("bad.example.com.", "999.bad", nil),
	)
	primary, _ := idx.Upsert(s)
	if len(primary.ParseErrors) != 1 {
		t.Errorf("expected 1 parse error, got %v", primary.ParseErrors)
	}
	if !reflect.DeepEqual(primary.Won, []string{"good.example.com. A"}) {
		t.Errorf("Won=%v", primary.Won)
	}
}

func TestDelete_PromotesLoser(t *testing.T) {
	idx := New()
	older := mkSlice("ns", "older", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil))
	newer := mkSlice("ns", "newer", "u2", t0().Add(time.Hour), 1, nil, a("foo.example.com.", "2.2.2.2", nil))
	idx.Upsert(older)
	idx.Upsert(newer)

	siblings := idx.Delete("ns", "older")
	if len(siblings) != 1 || siblings[0].Name != "newer" {
		t.Fatalf("expected newer to be promoted, got %+v", siblings)
	}
	if !reflect.DeepEqual(siblings[0].Result.Won, []string{"foo.example.com. A"}) {
		t.Errorf("promoted Won=%v", siblings[0].Result.Won)
	}
	rrs := idx.Lookup("foo.example.com.", dns.TypeA)
	if len(rrs) != 1 || rrs[0].(*dns.A).A.String() != "2.2.2.2" {
		t.Errorf("expected newer to serve after delete, got %v", rrs)
	}
}

func TestDelete_LastOwner_RemovesFromLookup(t *testing.T) {
	idx := New()
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil)))
	siblings := idx.Delete("ns", "a")
	if len(siblings) != 0 {
		t.Errorf("expected 0 siblings, got %v", siblings)
	}
	if rrs := idx.Lookup("foo.example.com.", dns.TypeA); rrs != nil {
		t.Errorf("expected miss after deleting last owner, got %v", rrs)
	}
}

func TestDelete_UnknownSlice_NoOp(t *testing.T) {
	idx := New()
	if siblings := idx.Delete("ns", "ghost"); siblings != nil {
		t.Errorf("expected nil for unknown delete, got %v", siblings)
	}
}

// ---------- Lookup ----------

func TestLookup(t *testing.T) {
	idx := New()
	idx.Upsert(mkSlice("ns", "a", "u1", t0(), 1, nil,
		a("foo.example.com.", "1.2.3.4", nil),
		cname("alias.example.com.", "foo.example.com."),
	))

	t.Run("case-insensitive", func(t *testing.T) {
		if rrs := idx.Lookup("FOO.EXAMPLE.COM.", dns.TypeA); len(rrs) != 1 {
			t.Errorf("case-insensitive miss, got %v", rrs)
		}
	})
	t.Run("trailing dot normalization", func(t *testing.T) {
		if rrs := idx.Lookup("foo.example.com", dns.TypeA); len(rrs) != 1 {
			t.Errorf("missing dot miss, got %v", rrs)
		}
	})
	t.Run("miss returns nil", func(t *testing.T) {
		if rrs := idx.Lookup("nothere.example.com.", dns.TypeA); rrs != nil {
			t.Errorf("expected nil, got %v", rrs)
		}
	})
	t.Run("slice header is a copy (RR pointers are still shared)", func(t *testing.T) {
		// Documented contract: Lookup returns a fresh slice header so callers
		// reordering or replacing elements don't corrupt the index. The pointed-to
		// dns.RR values are NOT deep-copied; mutating fields on those structs
		// would corrupt the index. CoreDNS plugins typically don't mutate RRs
		// they're handed, so this is by design (cheaper hot path).
		rrs := idx.Lookup("foo.example.com.", dns.TypeA)
		if len(rrs) == 0 {
			t.Fatal("setup")
		}
		rrs[0] = nil // clobber the returned slice's element
		again := idx.Lookup("foo.example.com.", dns.TypeA)
		if again[0] == nil {
			t.Errorf("index returned a shared slice header; mutations leaked back")
		}
	})
	t.Run("CNAME at name", func(t *testing.T) {
		rrs := idx.Lookup("alias.example.com.", dns.TypeCNAME)
		if len(rrs) != 1 {
			t.Errorf("expected 1 CNAME, got %v", rrs)
		}
	})
}

// ---------- snapshots ----------

func TestSnapshot_SortedAndStable(t *testing.T) {
	idx := New()
	s := mkSlice("ns", "a", "u1", t0(), 1, nil,
		a("z.example.com.", "1.1.1.1", nil),
		a("a.example.com.", "1.1.1.2", nil),
		txt("m.example.com.", "v"),
	)
	primary, _ := idx.Upsert(s)
	want := []string{"a.example.com. A", "m.example.com. TXT", "z.example.com. A"}
	if !reflect.DeepEqual(primary.Won, want) {
		t.Errorf("Won not sorted: got %v want %v", primary.Won, want)
	}
}

// ---------- helpers tested directly to nail coverage ----------

func TestSplitKey(t *testing.T) {
	cases := []struct {
		in       string
		ns, name string
	}{
		{"ns/foo", "ns", "foo"},
		{"only-name", "", "only-name"},
		{"ns/with/slashes", "ns", "with/slashes"},
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ns, name := splitKey(tc.in)
			if ns != tc.ns || name != tc.name {
				t.Errorf("splitKey(%q)=(%q,%q), want (%q,%q)", tc.in, ns, name, tc.ns, tc.name)
			}
		})
	}
}

func TestStatusChanged(t *testing.T) {
	base := UpsertResult{Won: []string{"x A"}, Lost: []string{}, LostTo: map[string]string{}}
	same := UpsertResult{Won: []string{"x A"}, Lost: []string{}, LostTo: map[string]string{}}
	if statusChanged(base, same) {
		t.Errorf("identical snapshots flagged changed")
	}
	wonDiff := UpsertResult{Won: []string{"y A"}, Lost: []string{}, LostTo: map[string]string{}}
	if !statusChanged(base, wonDiff) {
		t.Errorf("Won diff not detected")
	}
	lostDiff := UpsertResult{Won: []string{"x A"}, Lost: []string{"z TXT"}, LostTo: map[string]string{"z TXT": "ns/o"}}
	if !statusChanged(base, lostDiff) {
		t.Errorf("Lost diff not detected")
	}
	lostToDiff := UpsertResult{Won: []string{"x A"}, Lost: []string{"z TXT"}, LostTo: map[string]string{"z TXT": "ns/p"}}
	if !statusChanged(lostDiff, lostToDiff) {
		t.Errorf("LostTo diff not detected")
	}
}

func TestSnapshot_UnknownSlice(t *testing.T) {
	idx := New()
	if _, ok := idx.snapshot("ns/ghost"); ok {
		t.Errorf("expected ok=false for unknown slice")
	}
}

func TestUpsert_SiblingsSortedByNamespaceThenName(t *testing.T) {
	idx := New()
	// Three slices in three namespaces all losing to one new oldest entry.
	t1 := t0().Add(time.Hour)
	idx.Upsert(mkSlice("zeta", "z1", "uz", t1, 1, nil, a("foo.example.com.", "9.9.9.9", nil)))
	idx.Upsert(mkSlice("alpha", "a1", "ua", t1, 1, nil, a("foo.example.com.", "8.8.8.8", nil)))
	idx.Upsert(mkSlice("beta", "b1", "ub", t1, 1, nil, a("foo.example.com.", "7.7.7.7", nil)))

	_, siblings := idx.Upsert(mkSlice("oldest-ns", "o", "u-o", t0(), 1, nil, a("foo.example.com.", "1.1.1.1", nil)))
	if len(siblings) != 3 {
		t.Fatalf("expected 3 siblings, got %d", len(siblings))
	}
	wantOrder := []string{"alpha/a1", "beta/b1", "zeta/z1"}
	gotOrder := make([]string, len(siblings))
	for i, s := range siblings {
		gotOrder[i] = s.Namespace + "/" + s.Name
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("siblings not sorted: got %v want %v", gotOrder, wantOrder)
	}
}

// ---------- concurrency smoke test ----------

func TestConcurrent_ReadersAndMultipleWriters(t *testing.T) {
	// Real contention: 2 writers fighting over the same key (alternating LWW
	// owner) plus 4 readers asserting served records are well-formed throughout.
	idx := New()
	const iterations = 500
	const readers = 4
	const writers = 2

	// Seed so readers see a non-nil RR from the very first lookup.
	idx.Upsert(mkSlice("ns", "seed", "u-seed", t0(), 1, nil, a("contended.example.com.", "1.1.1.1", nil)))

	var writersWG, readersWG sync.WaitGroup
	stop := make(chan struct{})
	failures := make(chan string, 16)

	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					rrs := idx.Lookup("contended.example.com.", dns.TypeA)
					if len(rrs) != 1 {
						failures <- "lookup got len != 1"
						return
					}
					if rrs[0].(*dns.A).A == nil {
						failures <- "lookup got RR with nil IP"
						return
					}
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		writerID := w
		go func() {
			defer writersWG.Done()
			name := "writer" + string(rune('A'+writerID))
			uid := "uid-" + string(rune('A'+writerID))
			creation := t0().Add(time.Duration(writerID) * time.Second)
			for i := 0; i < iterations; i++ {
				s := mkSlice("ns", name, uid, creation, int64(i), nil,
					a("contended.example.com.", "10.0.0.1", nil))
				idx.Upsert(s)
			}
		}()
	}

	writersWG.Wait()
	close(stop)
	readersWG.Wait()
	close(failures)

	for f := range failures {
		t.Error(f)
	}
	if rrs := idx.Lookup("contended.example.com.", dns.TypeA); len(rrs) != 1 {
		t.Errorf("post-concurrent lookup got %v", rrs)
	}
}

