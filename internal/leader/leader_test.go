package leader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func validConfig() Config {
	return Config{
		Client:         fake.NewSimpleClientset(),
		LeaseNamespace: "kube-system",
		LeaseName:      "coredns-crd-leader",
		Identity:       "test-pod",
	}
}

func TestNew_RejectsRequiredFieldsMissing(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"nil client", func(c *Config) { c.Client = nil }},
		{"empty namespace", func(c *Config) { c.LeaseNamespace = "" }},
		{"empty lease name", func(c *Config) { c.LeaseName = "" }},
		{"empty identity", func(c *Config) { c.Identity = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestNew_FillsDurationDefaults(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.cfg.LeaseDuration != 15*time.Second {
		t.Errorf("default LeaseDuration: got %v", e.cfg.LeaseDuration)
	}
	if e.cfg.RenewDeadline != 10*time.Second {
		t.Errorf("default RenewDeadline: got %v", e.cfg.RenewDeadline)
	}
	if e.cfg.RetryPeriod != 2*time.Second {
		t.Errorf("default RetryPeriod: got %v", e.cfg.RetryPeriod)
	}
}

func TestNew_RejectsInconsistentDurations(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseDuration = 5 * time.Second
	cfg.RenewDeadline = 10 * time.Second
	cfg.RetryPeriod = 2 * time.Second
	_, err := New(cfg)
	if !errors.Is(err, ErrBadDurations) {
		t.Errorf("expected ErrBadDurations, got %v", err)
	}
}

func TestNew_UpstreamRejectsRenewTooCloseToRetry(t *testing.T) {
	// Upstream leaderelection requires RenewDeadline > RetryPeriod * JitterFactor
	// (JitterFactor=1.2), stricter than our own RenewDeadline > RetryPeriod
	// pre-check. Verifies our wrapper surfaces the upstream error.
	cfg := validConfig()
	cfg.LeaseDuration = 3 * time.Second
	cfg.RenewDeadline = 2 * time.Second
	cfg.RetryPeriod = 1700 * time.Millisecond
	_, err := New(cfg)
	if err == nil {
		t.Fatalf("expected upstream rejection")
	}
	if errors.Is(err, ErrBadDurations) {
		t.Errorf("expected upstream error (not ErrBadDurations), got %v", err)
	}
}

func TestNew_RejectsRenewLessThanRetry(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseDuration = 15 * time.Second
	cfg.RenewDeadline = 1 * time.Second
	cfg.RetryPeriod = 2 * time.Second
	_, err := New(cfg)
	if !errors.Is(err, ErrBadDurations) {
		t.Errorf("expected ErrBadDurations, got %v", err)
	}
}

func TestIsLeader_FalseBeforeRun(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if e.IsLeader() {
		t.Errorf("IsLeader must be false before Run")
	}
}

func TestOnStartedLeading_SetsLeadingAndFiresCallback(t *testing.T) {
	var called atomic.Bool
	cfg := validConfig()
	cfg.OnStartedLeading = func(context.Context) { called.Store(true) }
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.onStartedLeading(context.Background())
	if !e.IsLeader() {
		t.Errorf("IsLeader should be true after onStartedLeading")
	}
	if !called.Load() {
		t.Errorf("user callback OnStartedLeading not invoked")
	}
}

func TestOnStartedLeading_NilUserCallback_StillSetsLeading(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	e.onStartedLeading(context.Background())
	if !e.IsLeader() {
		t.Errorf("IsLeader should be true even when user callback is nil")
	}
}

func TestOnStoppedLeading_ClearsLeadingAndFiresCallback(t *testing.T) {
	var called atomic.Bool
	cfg := validConfig()
	cfg.OnStoppedLeading = func() { called.Store(true) }
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.leading.Store(true)
	e.onStoppedLeading()
	if e.IsLeader() {
		t.Errorf("IsLeader should be false after onStoppedLeading")
	}
	if !called.Load() {
		t.Errorf("user callback OnStoppedLeading not invoked")
	}
}

func TestOnStoppedLeading_NilUserCallback_StillClearsLeading(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	e.leading.Store(true)
	e.onStoppedLeading()
	if e.IsLeader() {
		t.Errorf("IsLeader should be false")
	}
}

func TestOnNewLeader_FiresCallback(t *testing.T) {
	var got string
	cfg := validConfig()
	cfg.OnNewLeader = func(id string) { got = id }
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.onNewLeader("other-pod")
	if got != "other-pod" {
		t.Errorf("OnNewLeader received %q, want %q", got, "other-pod")
	}
}

func TestOnNewLeader_NilUserCallback_NoOp(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	e.onNewLeader("other-pod") // must not panic
}

func TestRun_ExitsOnContextCancel(t *testing.T) {
	cfg := validConfig()
	cfg.LeaseDuration = 200 * time.Millisecond
	cfg.RenewDeadline = 100 * time.Millisecond
	cfg.RetryPeriod = 50 * time.Millisecond
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not exit after context cancel")
	}
}

func TestRun_AlreadyCancelledContext_ReturnsImmediately(t *testing.T) {
	e, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.Run(ctx) // must return immediately, not hang
}
