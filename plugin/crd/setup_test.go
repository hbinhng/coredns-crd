package crd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coredns/caddy"
	"k8s.io/client-go/kubernetes/fake"
)

// ---------- parseConfig ----------

func parseConfigFromInput(t *testing.T, input string) (*config, error) {
	t.Helper()
	c := caddy.NewTestController("dns", input)
	return parseConfig(c)
}

func TestParseConfig_Defaults(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResyncPeriod != 30*time.Minute {
		t.Errorf("default ResyncPeriod=%v, want 30m", cfg.ResyncPeriod)
	}
	if cfg.Kubeconfig != "" {
		t.Errorf("default Kubeconfig=%q, want empty", cfg.Kubeconfig)
	}
	if len(cfg.Fall.Zones) != 0 {
		t.Errorf("default Fall.Zones=%v, want empty", cfg.Fall.Zones)
	}
}

func TestParseConfig_AllProperties(t *testing.T) {
	input := `crd {
		kubeconfig /tmp/kubeconfig
		resync 5m
		fallthrough .
	}`
	cfg, err := parseConfigFromInput(t, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Kubeconfig != "/tmp/kubeconfig" {
		t.Errorf("Kubeconfig=%q", cfg.Kubeconfig)
	}
	if cfg.ResyncPeriod != 5*time.Minute {
		t.Errorf("ResyncPeriod=%v", cfg.ResyncPeriod)
	}
	if len(cfg.Fall.Zones) != 1 || cfg.Fall.Zones[0] != "." {
		t.Errorf("Fall.Zones=%v, want [.]", cfg.Fall.Zones)
	}
}

func TestParseConfig_FallthroughWithSpecificZone(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		fallthrough example.com.
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Fall.Zones) != 1 || cfg.Fall.Zones[0] != "example.com." {
		t.Errorf("Fall.Zones=%v", cfg.Fall.Zones)
	}
}

func TestParseConfig_FallthroughMultipleZones(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		fallthrough a.example. b.example.
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Fall.Zones) != 2 {
		t.Fatalf("Fall.Zones=%v, want 2 entries", cfg.Fall.Zones)
	}
	if cfg.Fall.Zones[0] != "a.example." || cfg.Fall.Zones[1] != "b.example." {
		t.Errorf("Fall.Zones order: got %v", cfg.Fall.Zones)
	}
}

func TestParseConfig_EmptyBlock(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
}`)
	if err != nil {
		t.Fatalf("empty block should parse as defaults: %v", err)
	}
	if cfg.ResyncPeriod != 30*time.Minute {
		t.Errorf("ResyncPeriod=%v", cfg.ResyncPeriod)
	}
}

func TestParseConfig_PropertyOrderIndependent(t *testing.T) {
	a, err := parseConfigFromInput(t, `crd {
		resync 1m
		kubeconfig /tmp/x
	}`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseConfigFromInput(t, `crd {
		kubeconfig /tmp/x
		resync 1m
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kubeconfig != b.Kubeconfig || a.ResyncPeriod != b.ResyncPeriod {
		t.Errorf("property order changed parse: a=%+v b=%+v", a, b)
	}
}

func TestParseConfig_CaseSensitivePropertyNames(t *testing.T) {
	if _, err := parseConfigFromInput(t, `crd {
		Kubeconfig /tmp/x
	}`); err == nil {
		t.Errorf("expected unknown property for capitalized 'Kubeconfig'")
	}
}

func TestParseConfig_MultipleBlocks_LastWinsScalars(t *testing.T) {
	// Pins current behavior: when multiple `crd { ... }` blocks appear in the
	// same server, scalars (kubeconfig, resync) are last-wins; fallthrough
	// zones are overwritten on each occurrence.
	cfg, err := parseConfigFromInput(t, `crd {
		resync 1m
		kubeconfig /tmp/a
	}
	crd {
		resync 2m
	}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResyncPeriod != 2*time.Minute {
		t.Errorf("expected last block's resync (2m), got %v", cfg.ResyncPeriod)
	}
	if cfg.Kubeconfig != "/tmp/a" {
		t.Errorf("expected kubeconfig from first block to persist, got %q", cfg.Kubeconfig)
	}
}

func TestParseConfig_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			"unexpected args on plugin line",
			`crd extra-arg`,
			"unexpected args",
		},
		{
			"unknown property",
			`crd {
				bogus value
			}`,
			"unknown property",
		},
		{
			"invalid resync duration",
			`crd {
				resync not-a-duration
			}`,
			"invalid resync duration",
		},
		{
			"missing kubeconfig argument",
			`crd {
				kubeconfig
			}`,
			"Wrong argument count",
		},
		{
			"missing resync argument",
			`crd {
				resync
			}`,
			"Wrong argument count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfigFromInput(t, tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// ---------- loadRESTConfig ----------

func TestLoadRESTConfig_FromKubeconfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: dummy-token
current-context: test
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	rc, err := loadRESTConfig(path)
	if err != nil {
		t.Fatalf("loadRESTConfig: %v", err)
	}
	if rc.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host=%q", rc.Host)
	}
	if rc.BearerToken != "dummy-token" {
		t.Errorf("BearerToken not propagated: %+v", rc)
	}
}

func TestLoadRESTConfig_BadKubeconfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte("not yaml at all: ::"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadRESTConfig(path); err == nil {
		t.Fatalf("expected error parsing bad kubeconfig")
	}
}

func TestLoadRESTConfig_InClusterFallback(t *testing.T) {
	// With kubeconfig=="" and not running inside a pod, InClusterConfig
	// returns ErrNotInCluster — exercises the second branch.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	os.Unsetenv("KUBERNETES_SERVICE_PORT")

	if _, err := loadRESTConfig(""); err == nil {
		t.Errorf("expected ErrNotInCluster outside a pod")
	}
}

// ---------- leader_election parsing ----------

func TestParseConfig_LeaderElection_Defaults(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaderElection.Disabled {
		t.Errorf("leader election should be enabled by default")
	}
	if cfg.LeaderElection.Namespace != "" {
		t.Errorf("Namespace default should be empty (resolved at runtime), got %q", cfg.LeaderElection.Namespace)
	}
	if cfg.LeaderElection.LeaseName != "" {
		t.Errorf("LeaseName default should be empty (resolved at runtime), got %q", cfg.LeaderElection.LeaseName)
	}
}

func TestParseConfig_LeaderElection_AllProperties(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		leader_election {
			namespace foo
			lease_name bar
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LeaderElection.Namespace != "foo" || cfg.LeaderElection.LeaseName != "bar" {
		t.Errorf("got %+v", cfg.LeaderElection)
	}
	if cfg.LeaderElection.Disabled {
		t.Errorf("Disabled should be false")
	}
}

func TestParseConfig_LeaderElection_EmptyBlock(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		leader_election {
		}
	}`)
	if err != nil {
		t.Fatalf("empty leader_election block should parse cleanly: %v", err)
	}
	if cfg.LeaderElection.Disabled {
		t.Errorf("Disabled should be false on empty block")
	}
}

func TestParseConfig_LeaderElection_Disable(t *testing.T) {
	cfg, err := parseConfigFromInput(t, `crd {
		leader_election {
			disable
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LeaderElection.Disabled {
		t.Errorf("expected Disabled=true")
	}
}

func TestParseConfig_LeaderElection_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			"unknown property",
			`crd { leader_election { bogus value } }`,
			"unknown leader_election property",
		},
		{
			"missing opening brace",
			`crd {
				leader_election
			}`,
			`expected "{" after leader_election`,
		},
		{
			"truncated block (no closing brace)",
			`crd {
				leader_election {
					disable`,
			"Unexpected EOF",
		},
		{
			"disable conflicts with namespace",
			`crd {
				leader_election {
					disable
					namespace foo
				}
			}`,
			"disable cannot coexist",
		},
		{
			"disable conflicts with lease_name",
			`crd {
				leader_election {
					lease_name foo
					disable
				}
			}`,
			"disable cannot coexist",
		},
		{
			"namespace missing arg",
			`crd {
				leader_election {
					namespace
				}
			}`,
			"Wrong argument count",
		},
		{
			"lease_name missing arg",
			`crd {
				leader_election {
					lease_name
				}
			}`,
			"Wrong argument count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfigFromInput(t, tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// ---------- buildLeaderElection ----------

func TestBuildLeaderElection_Disabled(t *testing.T) {
	pred, elector, err := buildLeaderElection(
		LeaderElectionConfig{Disabled: true},
		fake.NewSimpleClientset(),
		New(&config{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elector != nil {
		t.Errorf("expected nil elector when disabled, got %v", elector)
	}
	if !pred() {
		t.Errorf("predicate should be constant-true when disabled")
	}
}

func TestBuildLeaderElection_DefaultsFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "from-env")
	pred, elector, err := buildLeaderElection(
		LeaderElectionConfig{},
		fake.NewSimpleClientset(),
		New(&config{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elector == nil {
		t.Fatal("expected non-nil elector")
	}
	if pred == nil {
		t.Error("expected non-nil predicate")
	}
	// Predicate must be the elector's IsLeader (false before Run).
	if pred() {
		t.Errorf("predicate should be false before Run")
	}
}

func TestBuildLeaderElection_DefaultsToKubeSystem(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	os.Unsetenv("POD_NAMESPACE")
	_, elector, err := buildLeaderElection(
		LeaderElectionConfig{},
		fake.NewSimpleClientset(),
		New(&config{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elector == nil {
		t.Fatal("expected non-nil elector")
	}
}

func TestBuildLeaderElection_ExplicitValues(t *testing.T) {
	_, elector, err := buildLeaderElection(
		LeaderElectionConfig{Namespace: "ns", LeaseName: "name"},
		fake.NewSimpleClientset(),
		New(&config{}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elector == nil {
		t.Fatal("expected non-nil elector")
	}
}

func TestLeaderCallbacks_StartedTriggersReconcile(t *testing.T) {
	h, su := newHandler(t)
	h.applySlice(mkSlice("ns", "n", "u1", time.Unix(0, 0), 1, aRecord("foo.example.com.", "1.2.3.4")))
	before := len(su.Calls())

	onStarted, _, _ := leaderCallbacks(h)
	onStarted(nil)

	if got := len(su.Calls()) - before; got != 1 {
		t.Errorf("OnStartedLeading should trigger reconcileAll; got %d enqueues", got)
	}
}

func TestLeaderCallbacks_StoppedDoesNotPanic(t *testing.T) {
	h, _ := newHandler(t)
	_, onStopped, _ := leaderCallbacks(h)
	onStopped() // logs only; must not panic
}

func TestLeaderCallbacks_NewLeaderDoesNotPanic(t *testing.T) {
	h, _ := newHandler(t)
	_, _, onNew := leaderCallbacks(h)
	onNew("some-pod") // logs only; must not panic
}
