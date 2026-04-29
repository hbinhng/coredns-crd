package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordLookup_BumpsCounterByResult(t *testing.T) {
	ResetForTest()
	RecordLookup("hit")
	RecordLookup("hit")
	RecordLookup("miss")

	if got := testutil.ToFloat64(lookups.WithLabelValues("hit")); got != 2 {
		t.Errorf("lookups{hit}=%v, want 2", got)
	}
	if got := testutil.ToFloat64(lookups.WithLabelValues("miss")); got != 1 {
		t.Errorf("lookups{miss}=%v, want 1", got)
	}
}

func TestRecordApply_BumpsCounterByResult(t *testing.T) {
	ResetForTest()
	RecordApply("applied")
	RecordApply("parse_error")
	if got := testutil.ToFloat64(applies.WithLabelValues("applied")); got != 1 {
		t.Errorf("applies{applied}=%v", got)
	}
	if got := testutil.ToFloat64(applies.WithLabelValues("parse_error")); got != 1 {
		t.Errorf("applies{parse_error}=%v", got)
	}
}

func TestRecordStatusPatch_BumpsCounterByResult(t *testing.T) {
	ResetForTest()
	RecordStatusPatch("success")
	RecordStatusPatch("error")
	if got := testutil.ToFloat64(statusPatches.WithLabelValues("success")); got != 1 {
		t.Errorf("status_patches{success}=%v", got)
	}
	if got := testutil.ToFloat64(statusPatches.WithLabelValues("error")); got != 1 {
		t.Errorf("status_patches{error}=%v", got)
	}
}

func TestRecordConflictTransition(t *testing.T) {
	ResetForTest()
	RecordConflictTransition("detected")
	RecordConflictTransition("resolved")
	if got := testutil.ToFloat64(conflictTransitions.WithLabelValues("detected")); got != 1 {
		t.Errorf("transitions{detected}=%v", got)
	}
	if got := testutil.ToFloat64(conflictTransitions.WithLabelValues("resolved")); got != 1 {
		t.Errorf("transitions{resolved}=%v", got)
	}
}

func TestRecordIndexSize_SetsGauges(t *testing.T) {
	ResetForTest()
	RecordIndexSize(5, 12, 1)
	if got := testutil.ToFloat64(indexSlices); got != 5 {
		t.Errorf("index_slices=%v, want 5", got)
	}
	if got := testutil.ToFloat64(indexRecords); got != 12 {
		t.Errorf("index_records=%v, want 12", got)
	}
	if got := testutil.ToFloat64(activeConflicts); got != 1 {
		t.Errorf("active_conflicts=%v, want 1", got)
	}
}

func TestSetLeader_FlipsGauge(t *testing.T) {
	ResetForTest()
	SetLeader(true)
	if got := testutil.ToFloat64(isLeader); got != 1 {
		t.Errorf("is_leader=%v, want 1", got)
	}
	SetLeader(false)
	if got := testutil.ToFloat64(isLeader); got != 0 {
		t.Errorf("is_leader=%v, want 0", got)
	}
}

func TestForTestHelpers_ReturnUnderlyingCounters(t *testing.T) {
	ResetForTest()
	LookupsForTest("hit").Inc()
	AppliesForTest("applied").Inc()
	StatusPatchesForTest("success").Inc()

	if got := testutil.ToFloat64(LookupsForTest("hit")); got != 1 {
		t.Errorf("LookupsForTest hit=%v, want 1", got)
	}
	if got := testutil.ToFloat64(AppliesForTest("applied")); got != 1 {
		t.Errorf("AppliesForTest applied=%v, want 1", got)
	}
	if got := testutil.ToFloat64(StatusPatchesForTest("success")); got != 1 {
		t.Errorf("StatusPatchesForTest success=%v, want 1", got)
	}
}

func TestAllMetricsRegisteredOnDefaultRegistry(t *testing.T) {
	out, err := metricFamiliesText()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"coredns_crd_lookups_total",
		"coredns_crd_applies_total",
		"coredns_crd_status_patches_total",
		"coredns_crd_conflict_transitions_total",
		"coredns_crd_index_records",
		"coredns_crd_index_slices",
		"coredns_crd_active_conflicts",
		"coredns_crd_is_leader",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric %s not registered", want)
		}
	}
}
