// Package leader is a thin wrapper around client-go leader election. It
// narrows the upstream surface to a single seam (IsLeader) the rest of the
// plugin consults before performing leader-only work.
package leader

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// ErrBadDurations is returned when timing parameters violate the invariant
// LeaseDuration > RenewDeadline > RetryPeriod, which would prevent the
// elector from holding the lease cleanly.
var ErrBadDurations = errors.New("LeaseDuration must be > RenewDeadline > RetryPeriod")

type Config struct {
	Client         kubernetes.Interface
	LeaseNamespace string
	LeaseName      string
	// Identity must be unique across competing replicas. Pod name from the
	// downward API is the conventional choice and is what kube-controller-
	// manager and friends use.
	Identity string

	// Defaults applied when zero-valued: 15s / 10s / 2s.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration

	OnStartedLeading func(ctx context.Context)
	OnStoppedLeading func()
	OnNewLeader      func(identity string)
}

type Elector struct {
	cfg     Config
	leading atomic.Bool
	elector *leaderelection.LeaderElector
}

func New(cfg Config) (*Elector, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("client is required")
	}
	if cfg.LeaseNamespace == "" {
		return nil, fmt.Errorf("lease namespace is required")
	}
	if cfg.LeaseName == "" {
		return nil, fmt.Errorf("lease name is required")
	}
	if cfg.Identity == "" {
		return nil, fmt.Errorf("identity is required")
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = 10 * time.Second
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = 2 * time.Second
	}
	if cfg.LeaseDuration <= cfg.RenewDeadline || cfg.RenewDeadline <= cfg.RetryPeriod {
		return nil, ErrBadDurations
	}

	e := &Elector{cfg: cfg}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Namespace: cfg.LeaseNamespace, Name: cfg.LeaseName},
		Client:    cfg.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: e.onStartedLeading,
			OnStoppedLeading: e.onStoppedLeading,
			OnNewLeader:      e.onNewLeader,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build LeaderElector: %w", err)
	}
	e.elector = le
	return e, nil
}

func (e *Elector) onStartedLeading(ctx context.Context) {
	e.leading.Store(true)
	if e.cfg.OnStartedLeading != nil {
		e.cfg.OnStartedLeading(ctx)
	}
}

func (e *Elector) onStoppedLeading() {
	e.leading.Store(false)
	if e.cfg.OnStoppedLeading != nil {
		e.cfg.OnStoppedLeading()
	}
}

func (e *Elector) onNewLeader(id string) {
	if e.cfg.OnNewLeader != nil {
		e.cfg.OnNewLeader(id)
	}
}

// Run blocks until ctx is cancelled. Upstream elector.Run owns its own
// retry/backoff and only returns on ctx cancel.
func (e *Elector) Run(ctx context.Context) {
	e.elector.Run(ctx)
}

// IsLeader reports whether this process currently holds the lease.
//
// The result is updated by upstream callbacks (OnStartedLeading /
// OnStoppedLeading), so there is a small window after the lease is
// actually lost on the API server during which this still returns true,
// until the upstream library invokes our callback. Acceptable given lease
// durations dwarf patch latency on the call sites.
func (e *Elector) IsLeader() bool {
	return e.leading.Load()
}
