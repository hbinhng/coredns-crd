// Package metrics exposes Prometheus collectors for the crd plugin under the
// coredns_crd_* namespace. Collectors register with prometheus.DefaultRegisterer
// at package init so the CoreDNS prometheus plugin auto-collects them.
package metrics

import (
	"bytes"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

const (
	namespace = "coredns"
	subsystem = "crd"
)

var (
	lookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "lookups_total",
		Help: "Counter of DNS lookups served by the crd plugin, by result.",
	}, []string{"result"})

	applies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "applies_total",
		Help: "Counter of DNSSlice apply outcomes.",
	}, []string{"result"})

	statusPatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "status_patches_total",
		Help: "Counter of DNSSlice /status patch outcomes.",
	}, []string{"result"})

	conflictTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "conflict_transitions_total",
		Help: "Counter of Conflicting condition transitions, by direction.",
	}, []string{"direction"})

	indexSlices = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "index_slices",
		Help: "Number of DNSSlice resources currently indexed.",
	})

	indexRecords = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "index_records",
		Help: "Number of (FQDN, type) record values currently served.",
	})

	activeConflicts = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "active_conflicts",
		Help: "Number of slices with at least one Lost entry.",
	})

	isLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystem, Name: "is_leader",
		Help: "1 if this replica currently holds the leader lease, else 0.",
	})

	collectors = []prometheus.Collector{
		lookups, applies, statusPatches, conflictTransitions,
		indexSlices, indexRecords, activeConflicts, isLeader,
	}
)

func init() {
	for _, c := range collectors {
		prometheus.MustRegister(c)
	}
	primeLabels()
}

// primeLabels pre-creates every (counter, label-value) combination so all
// known series are visible at zero from boot — Prometheus best practice that
// avoids "absent" alerts on cold start. Called from init() and from Reset().
func primeLabels() {
	for _, r := range []string{"hit", "miss", "cname_fallback", "fallthrough", "nxdomain"} {
		lookups.WithLabelValues(r)
	}
	for _, r := range []string{"applied", "parse_error"} {
		applies.WithLabelValues(r)
	}
	for _, r := range []string{"success", "error"} {
		statusPatches.WithLabelValues(r)
	}
	for _, d := range []string{"detected", "resolved"} {
		conflictTransitions.WithLabelValues(d)
	}
}

// RecordLookup increments lookups_total by result.
// result ∈ {hit, miss, cname_fallback, fallthrough, nxdomain}.
func RecordLookup(result string) { lookups.WithLabelValues(result).Inc() }

// RecordApply increments applies_total by result.
// result ∈ {applied, parse_error}.
func RecordApply(result string) { applies.WithLabelValues(result).Inc() }

// RecordStatusPatch increments status_patches_total by result.
// result ∈ {success, error}.
func RecordStatusPatch(result string) { statusPatches.WithLabelValues(result).Inc() }

// RecordConflictTransition increments conflict_transitions_total by direction.
// direction ∈ {detected, resolved}.
func RecordConflictTransition(direction string) {
	conflictTransitions.WithLabelValues(direction).Inc()
}

// RecordIndexSize sets the three Index gauges atomically (relative to the
// caller, which is expected to hold the Index write lock).
func RecordIndexSize(slices, records, conflicts int) {
	indexSlices.Set(float64(slices))
	indexRecords.Set(float64(records))
	activeConflicts.Set(float64(conflicts))
}

// SetLeader flips the is_leader gauge (1 = leading, 0 = follower).
func SetLeader(leading bool) {
	if leading {
		isLeader.Set(1)
	} else {
		isLeader.Set(0)
	}
}

// LookupsForTest exposes the underlying counter for a given result label so
// other packages' tests can assert via prometheus/testutil. Not for use in
// production code paths.
func LookupsForTest(result string) prometheus.Counter {
	return lookups.WithLabelValues(result)
}

// AppliesForTest exposes the applies counter for a given result label.
// Test-only.
func AppliesForTest(result string) prometheus.Counter {
	return applies.WithLabelValues(result)
}

// StatusPatchesForTest exposes the status_patches counter for a given result
// label. Test-only.
func StatusPatchesForTest(result string) prometheus.Counter {
	return statusPatches.WithLabelValues(result)
}

// ResetForTest clears all collectors and re-primes label series. Test-only;
// the name carries the intent because Go can't enforce cross-package
// "test-only export". Production code must never call this.
func ResetForTest() {
	lookups.Reset()
	applies.Reset()
	statusPatches.Reset()
	conflictTransitions.Reset()
	indexSlices.Set(0)
	indexRecords.Set(0)
	activeConflicts.Set(0)
	isLeader.Set(0)
	primeLabels()
}

// metricFamiliesText scrapes the default registry into the prom text format.
// Test-only helper.
func metricFamiliesText() (string, error) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}
