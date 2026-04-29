package index

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/miekg/dns"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
)

const DefaultTTL uint32 = 300

type sliceKey = string // "namespace/name"

type recordKey struct {
	fqdn string
	typ  uint16
}

type sliceCandidates struct {
	creationUnixNano int64
	uid              string
	generation       int64
	parseErrs        []string
	rrs              map[recordKey][]dns.RR
}

// Index is a thread-safe FQDN→RR index with last-write-wins arbitration.
// Winner = slice with oldest creationTimestamp; UID breaks ties for determinism.
type Index struct {
	mu       sync.RWMutex
	slices   map[sliceKey]sliceCandidates
	lookup   map[recordKey][]dns.RR
	winners  map[recordKey]sliceKey
	observer func(slices, records, conflicts int)
}

func New() *Index {
	return &Index{
		slices:  map[sliceKey]sliceCandidates{},
		lookup:  map[recordKey][]dns.RR{},
		winners: map[recordKey]sliceKey{},
	}
}

// SetSizeObserver installs a callback fired after every Index mutation with
// (slices, records, conflicts). `conflicts` is the number of slices with at
// least one Lost record key, not the total number of contested keys.
//
// The callback runs synchronously under the Index write lock — it must be
// cheap and non-reentrant (calling back into Index would deadlock).
//
// Pass nil to clear. Single observer per Index.
func (i *Index) SetSizeObserver(fn func(slices, records, conflicts int)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.observer = fn
}

// fireObserver computes current sizes under the held write lock and invokes
// the observer. Caller must hold i.mu (write lock). O(slices * keys-per-slice);
// trivial at current scale (50 keys/slice cap, sub-ms for thousands of slices).
func (i *Index) fireObserver() {
	if i.observer == nil {
		return
	}
	records := 0
	for _, rrs := range i.lookup {
		records += len(rrs)
	}
	conflicts := 0
	for sk, sc := range i.slices {
		for k := range sc.rrs {
			if i.winners[k] != sk {
				conflicts++
				break
			}
		}
	}
	i.observer(len(i.slices), records, conflicts)
}

// UpsertResult is the per-slice arbitration outcome, suitable for status emission.
type UpsertResult struct {
	Won         []string          // sorted "fqdn TYPE"
	Lost        []string          // sorted "fqdn TYPE"
	LostTo      map[string]string // "fqdn TYPE" → winner sliceKey
	ParseErrors []string
}

// SliceStatus carries a sibling slice's current outcome for status emission.
type SliceStatus struct {
	Namespace  string
	Name       string
	Generation int64
	Result     UpsertResult
}

func (i *Index) Upsert(slice *apiv1.DNSSlice) (UpsertResult, []SliceStatus) {
	i.mu.Lock()
	defer i.mu.Unlock()

	key := slice.Namespace + "/" + slice.Name
	parsed, parseErrs := parseSlice(slice)
	parsed.generation = slice.Generation
	parsed.parseErrs = parseErrs

	affected := keysOf(parsed.rrs)
	if prev, ok := i.slices[key]; ok {
		for k := range prev.rrs {
			affected[k] = struct{}{}
		}
	}

	pre := i.snapshotSiblings(key, affected)

	i.slices[key] = parsed
	for k := range affected {
		i.recompute(k)
	}
	i.fireObserver()

	primary, _ := i.snapshot(key)
	siblings := i.diffSiblings(pre)
	return primary, siblings
}

func (i *Index) Delete(namespace, name string) []SliceStatus {
	i.mu.Lock()
	defer i.mu.Unlock()

	key := namespace + "/" + name
	prev, ok := i.slices[key]
	if !ok {
		return nil
	}
	affected := keysOf(prev.rrs)
	pre := i.snapshotSiblings(key, affected)
	delete(i.slices, key)
	for k := range affected {
		i.recompute(k)
	}
	i.fireObserver()
	return i.diffSiblings(pre)
}

func (i *Index) Lookup(qname string, qtype uint16) []dns.RR {
	k := recordKey{fqdn: strings.ToLower(dns.Fqdn(qname)), typ: qtype}
	i.mu.RLock()
	rrs := i.lookup[k]
	i.mu.RUnlock()
	if len(rrs) == 0 {
		return nil
	}
	out := make([]dns.RR, len(rrs))
	copy(out, rrs)
	return out
}

// AllSnapshots returns the current outcome for every slice in the index,
// sorted by namespace then name.
func (i *Index) AllSnapshots() []SliceStatus {
	i.mu.RLock()
	out := make([]SliceStatus, 0, len(i.slices))
	for sk := range i.slices {
		snap, _ := i.snapshot(sk)
		out = append(out, i.statusFor(sk, snap))
	}
	i.mu.RUnlock()
	sortSliceStatuses(out)
	return out
}

// statusFor assembles a SliceStatus for sk with the supplied result.
// Caller must hold i.mu (read lock is sufficient).
func (i *Index) statusFor(sk sliceKey, result UpsertResult) SliceStatus {
	ns, name := splitKey(sk)
	return SliceStatus{
		Namespace:  ns,
		Name:       name,
		Generation: i.slices[sk].generation,
		Result:     result,
	}
}

func sortSliceStatuses(s []SliceStatus) {
	sort.Slice(s, func(a, b int) bool {
		if s[a].Namespace != s[b].Namespace {
			return s[a].Namespace < s[b].Namespace
		}
		return s[a].Name < s[b].Name
	})
}

func (i *Index) recompute(k recordKey) {
	winner, rrs := i.resolveWinner(k)
	if len(rrs) == 0 {
		delete(i.lookup, k)
		delete(i.winners, k)
		return
	}
	i.lookup[k] = rrs
	i.winners[k] = winner
}

func (i *Index) resolveWinner(k recordKey) (sliceKey, []dns.RR) {
	var winner sliceKey
	var winnerData *sliceCandidates
	for sk, sc := range i.slices {
		if _, ok := sc.rrs[k]; !ok {
			continue
		}
		if winnerData == nil ||
			sc.creationUnixNano < winnerData.creationUnixNano ||
			(sc.creationUnixNano == winnerData.creationUnixNano && sc.uid < winnerData.uid) {
			winner = sk
			cur := sc
			winnerData = &cur
		}
	}
	if winnerData == nil {
		return "", nil
	}
	return winner, winnerData.rrs[k]
}

// snapshot returns the current outcome for the named slice. Caller must hold i.mu.
func (i *Index) snapshot(key sliceKey) (UpsertResult, bool) {
	sc, ok := i.slices[key]
	if !ok {
		return UpsertResult{}, false
	}
	res := UpsertResult{
		LostTo:      map[string]string{},
		ParseErrors: append([]string(nil), sc.parseErrs...),
	}
	for k := range sc.rrs {
		ks := keyString(k)
		if i.winners[k] == key {
			res.Won = append(res.Won, ks)
		} else {
			res.Lost = append(res.Lost, ks)
			res.LostTo[ks] = i.winners[k]
		}
	}
	sort.Strings(res.Won)
	sort.Strings(res.Lost)
	return res, true
}

// snapshotSiblings returns pre-mutation snapshots for every slice OTHER than
// `excludeKey` whose candidates intersect `affected`. Caller must hold i.mu.
func (i *Index) snapshotSiblings(excludeKey sliceKey, affected map[recordKey]struct{}) map[sliceKey]UpsertResult {
	out := map[sliceKey]UpsertResult{}
	for sk, sc := range i.slices {
		if sk == excludeKey {
			continue
		}
		for k := range sc.rrs {
			if _, hit := affected[k]; hit {
				snap, _ := i.snapshot(sk)
				out[sk] = snap
				break
			}
		}
	}
	return out
}

// diffSiblings returns SliceStatus entries for siblings whose ownership snapshot
// changed relative to pre. Caller must hold i.mu.
func (i *Index) diffSiblings(pre map[sliceKey]UpsertResult) []SliceStatus {
	var out []SliceStatus
	for sk, before := range pre {
		after, _ := i.snapshot(sk) // sk is guaranteed present: pre only holds slices that survived
		if !statusChanged(before, after) {
			continue
		}
		out = append(out, i.statusFor(sk, after))
	}
	sortSliceStatuses(out)
	return out
}

func statusChanged(a, b UpsertResult) bool {
	return !slices.Equal(a.Won, b.Won) ||
		!slices.Equal(a.Lost, b.Lost) ||
		!maps.Equal(a.LostTo, b.LostTo)
}

func splitKey(sk sliceKey) (string, string) {
	ns, name, ok := strings.Cut(sk, "/")
	if !ok {
		return "", sk
	}
	return ns, name
}

func keysOf(m map[recordKey][]dns.RR) map[recordKey]struct{} {
	out := make(map[recordKey]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func keyString(k recordKey) string {
	return fmt.Sprintf("%s %s", k.fqdn, dns.TypeToString[k.typ])
}

func parseSlice(slice *apiv1.DNSSlice) (sliceCandidates, []string) {
	sc := sliceCandidates{
		uid: string(slice.UID),
		rrs: map[recordKey][]dns.RR{},
	}
	if !slice.CreationTimestamp.IsZero() {
		sc.creationUnixNano = slice.CreationTimestamp.UnixNano()
	}

	ttlDefault := DefaultTTL
	if slice.Spec.DefaultTTL != nil {
		ttlDefault = uint32(*slice.Spec.DefaultTTL)
	}

	var errs []string
	for idx, e := range slice.Spec.Entries {
		ttl := ttlDefault
		if e.TTL != nil {
			ttl = uint32(*e.TTL)
		}
		name := strings.ToLower(dns.Fqdn(e.Name))
		rr, typ, err := buildRR(name, ttl, e)
		if err != nil {
			errs = append(errs, fmt.Sprintf("entry[%d] %s/%s: %v", idx, e.Name, e.Type, err))
			continue
		}
		k := recordKey{fqdn: name, typ: typ}
		sc.rrs[k] = append(sc.rrs[k], rr)
	}
	return sc, errs
}

func buildRR(name string, ttl uint32, e apiv1.DNSEntry) (dns.RR, uint16, error) {
	hdr := dns.RR_Header{Name: name, Class: dns.ClassINET, Ttl: ttl}
	switch e.Type {
	case "A":
		if e.A == nil {
			return nil, 0, fmt.Errorf("type=A requires .a")
		}
		ip := net.ParseIP(e.A.Address)
		if ip == nil || ip.To4() == nil {
			return nil, 0, fmt.Errorf("invalid IPv4 %q", e.A.Address)
		}
		hdr.Rrtype = dns.TypeA
		return &dns.A{Hdr: hdr, A: ip.To4()}, dns.TypeA, nil
	case "AAAA":
		if e.AAAA == nil {
			return nil, 0, fmt.Errorf("type=AAAA requires .aaaa")
		}
		ip := net.ParseIP(e.AAAA.Address)
		if ip == nil || ip.To4() != nil {
			return nil, 0, fmt.Errorf("invalid IPv6 %q", e.AAAA.Address)
		}
		hdr.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: hdr, AAAA: ip}, dns.TypeAAAA, nil
	case "CNAME":
		if e.CNAME == nil {
			return nil, 0, fmt.Errorf("type=CNAME requires .cname")
		}
		hdr.Rrtype = dns.TypeCNAME
		return &dns.CNAME{Hdr: hdr, Target: dns.Fqdn(e.CNAME.Target)}, dns.TypeCNAME, nil
	case "TXT":
		if e.TXT == nil {
			return nil, 0, fmt.Errorf("type=TXT requires .txt")
		}
		hdr.Rrtype = dns.TypeTXT
		vals := make([]string, len(e.TXT.Values))
		copy(vals, e.TXT.Values)
		return &dns.TXT{Hdr: hdr, Txt: vals}, dns.TypeTXT, nil
	case "SRV":
		if e.SRV == nil {
			return nil, 0, fmt.Errorf("type=SRV requires .srv")
		}
		hdr.Rrtype = dns.TypeSRV
		return &dns.SRV{
			Hdr:      hdr,
			Priority: uint16(e.SRV.Priority),
			Weight:   uint16(e.SRV.Weight),
			Port:     uint16(e.SRV.Port),
			Target:   dns.Fqdn(e.SRV.Target),
		}, dns.TypeSRV, nil
	default:
		if e.Raw == nil {
			return nil, 0, fmt.Errorf("unknown type %q without .raw", e.Type)
		}
		line := fmt.Sprintf("%s %d IN %s %s", name, ttl, e.Type, e.Raw.RData)
		rr, err := dns.NewRR(line)
		if err != nil {
			return nil, 0, fmt.Errorf("parse raw rdata: %w", err)
		}
		return rr, rr.Header().Rrtype, nil
	}
}
