package crd

import (
	"context"
	"fmt"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/fall"
	clog "github.com/coredns/coredns/plugin/pkg/log"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
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
	Kubeconfig   string
	ResyncPeriod time.Duration
	Fall         fall.F
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

	h := New(cfg)
	h.statusUpdater = NewStatusUpdater(dyn)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, cfg.ResyncPeriod)
	informer := factory.ForResource(dnsSliceGVR).Informer()
	if _, err := informer.AddEventHandler(h.eventHandler()); err != nil {
		return plugin.Error(pluginName, fmt.Errorf("register event handler: %w", err))
	}

	c.OnStartup(func() error {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.statusUpdater.Run(ctx)
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
			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}
	}
	return cfg, nil
}

func loadRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
