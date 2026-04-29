package crd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/fall"
	clog "github.com/coredns/coredns/plugin/pkg/log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	apiv1 "github.com/hbinhng/coredns-crd/api/v1alpha1"
	"github.com/hbinhng/coredns-crd/internal/events"
	"github.com/hbinhng/coredns-crd/internal/leader"
	"github.com/hbinhng/coredns-crd/internal/metrics"
)

const pluginName = "crd"

var log = clog.NewWithPlugin(pluginName)

var dnsSliceGVR = schema.GroupVersionResource{
	Group:    "dns.coredns-crd.io",
	Version:  "v1alpha1",
	Resource: "dnsslices",
}

func init() {
	plugin.Register(pluginName, setup)
}

type config struct {
	Kubeconfig     string
	ResyncPeriod   time.Duration
	Fall           fall.F
	LeaderElection LeaderElectionConfig
}

// LeaderElectionConfig controls the optional client-go leader election. When
// Disabled is true the plugin behaves as if every replica is leader (existing
// single-replica semantics). Empty Namespace/LeaseName fall back at runtime
// to POD_NAMESPACE or "kube-system" and "coredns-crd-leader" respectively.
type LeaderElectionConfig struct {
	Disabled  bool
	Namespace string
	LeaseName string
}

func setup(c *caddy.Controller) error {
	cfg, err := parseConfig(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	rc, err := loadRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return plugin.Error(pluginName, fmt.Errorf("kubernetes config: %w", err))
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return plugin.Error(pluginName, fmt.Errorf("dynamic client: %w", err))
	}
	clientset, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return plugin.Error(pluginName, fmt.Errorf("typed clientset: %w", err))
	}

	h := New(cfg)

	// Build EventBroadcaster + Recorder once for the lifetime of this pod;
	// hand the recorder to the emitter that fires Events on conflict
	// transitions.
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		return plugin.Error(pluginName, fmt.Errorf("register scheme: %w", err))
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "coredns-crd"})
	h.emitter = events.NewEmitter(recorder)

	// Wire the Index size observer for the metric gauges.
	h.idx.SetSizeObserver(metrics.RecordIndexSize)

	isLeader, elector, err := buildLeaderElection(cfg.LeaderElection, clientset, h)
	if err != nil {
		return plugin.Error(pluginName, err)
	}
	h.statusUpdater = NewStatusUpdater(dyn, isLeader)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, cfg.ResyncPeriod)
	informer := factory.ForResource(dnsSliceGVR).Informer()
	if _, err := informer.AddEventHandler(h.eventHandler()); err != nil {
		return plugin.Error(pluginName, fmt.Errorf("register event handler: %w", err))
	}

	c.OnStartup(func() error {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.statusUpdater.Run(ctx)
		if elector != nil {
			go elector.Run(ctx)
		}
		factory.Start(ctx.Done())
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return fmt.Errorf("crd: failed to sync DNSSlice cache")
		}
		log.Info("DNSSlice cache synced")
		return nil
	})
	c.OnShutdown(func() error {
		if h.cancel != nil {
			h.cancel()
		}
		broadcaster.Shutdown()
		return nil
	})

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		h.Next = next
		return h
	})
	return nil
}

func parseConfig(c *caddy.Controller) (*config, error) {
	cfg := &config{ResyncPeriod: 30 * time.Minute}
	for c.Next() {
		// crd { ... }
		args := c.RemainingArgs()
		if len(args) > 0 {
			return nil, c.Errf("unexpected args on plugin line: %v", args)
		}
		for c.NextBlock() {
			switch c.Val() {
			case "kubeconfig":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				cfg.Kubeconfig = c.Val()
			case "resync":
				if !c.NextArg() {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(c.Val())
				if err != nil {
					return nil, c.Errf("invalid resync duration: %v", err)
				}
				cfg.ResyncPeriod = d
			case "fallthrough":
				cfg.Fall.SetZonesFromArgs(c.RemainingArgs())
			case "leader_election":
				if err := parseLeaderElection(c, &cfg.LeaderElection); err != nil {
					return nil, err
				}
			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}
	}
	return cfg, nil
}

// parseLeaderElection consumes a `leader_election { ... }` sub-block.
// caddy's Dispenser only tracks one level of NextBlock nesting, so we
// step the cursor manually across the inner braces.
func parseLeaderElection(c *caddy.Controller, le *LeaderElectionConfig) error {
	if !c.NextArg() || c.Val() != "{" {
		return c.Errf("expected %q after leader_election", "{")
	}
	for c.Next() {
		v := c.Val()
		if v == "}" {
			if le.Disabled && (le.Namespace != "" || le.LeaseName != "") {
				return c.Errf("leader_election disable cannot coexist with namespace or lease_name")
			}
			return nil
		}
		switch v {
		case "disable":
			le.Disabled = true
		case "namespace":
			if !c.NextArg() {
				return c.ArgErr()
			}
			le.Namespace = c.Val()
		case "lease_name":
			if !c.NextArg() {
				return c.ArgErr()
			}
			le.LeaseName = c.Val()
		default:
			return c.Errf("unknown leader_election property %q", v)
		}
	}
	return c.EOFErr()
}

// buildLeaderElection returns the predicate the statusUpdater consults plus
// (optionally) the Elector to be Run by setup. When leader election is
// disabled the predicate is constant-true and the Elector is nil.
func buildLeaderElection(cfg LeaderElectionConfig, clientset kubernetes.Interface, h *Handler) (func() bool, *leader.Elector, error) {
	if cfg.Disabled {
		log.Warning("leader election disabled; every replica will write status (race-prone)")
		metrics.SetLeader(true) // disabled mode = everyone is "leader"
		return alwaysLeader, nil, nil
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		ns = "kube-system"
	}
	name := cfg.LeaseName
	if name == "" {
		name = "coredns-crd-leader"
	}
	identity, _ := os.Hostname()
	if identity == "" {
		identity = "coredns-crd"
	}
	onStarted, onStopped, onNew := leaderCallbacks(h)
	wrappedStarted := func(ctx context.Context) {
		metrics.SetLeader(true)
		onStarted(ctx)
	}
	wrappedStopped := func() {
		onStopped()
		metrics.SetLeader(false)
	}
	elector, err := leader.New(leader.Config{
		Client:           clientset,
		LeaseNamespace:   ns,
		LeaseName:        name,
		Identity:         identity,
		OnStartedLeading: wrappedStarted,
		OnStoppedLeading: wrappedStopped,
		OnNewLeader:      onNew,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build leader elector: %w", err)
	}
	return elector.IsLeader, elector, nil
}

func alwaysLeader() bool { return true }

// leaderCallbacks returns the three callback closures wired into the elector.
// Extracted as a separate function so each closure is unit-testable without
// running real leader election.
//
// Note on ordering: OnStartedLeading may fire before the informer cache has
// finished syncing on a cold start. reconcileAll then iterates an empty
// Index and publishes nothing — the subsequent informer Add events trigger
// applySlice → status enqueues anyway, so the only cost is a wasted reconcile
// pass. Not a correctness issue.
func leaderCallbacks(h *Handler) (
	onStarted func(context.Context),
	onStopped func(),
	onNew func(string),
) {
	onStarted = func(ctx context.Context) {
		log.Info("acquired leadership; reconciling all DNSSlices")
		h.reconcileAll()
	}
	onStopped = func() {
		log.Info("lost leadership; status writes paused")
	}
	onNew = func(id string) {
		log.Infof("current leader: %s", id)
	}
	return
}

func loadRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
