// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
	"github.com/votdev/node-disk-sentinel/pkg/controller"
	"github.com/votdev/node-disk-sentinel/pkg/discovery"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
	"github.com/votdev/node-disk-sentinel/pkg/utils"
)

var (
	version = "dev"
	scheme  = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

type options struct {
	printVersion    bool
	nodeName        string
	metricsAddr     string
	metricsEnabled  bool
	udevDataDir     string
	pollInterval    time.Duration
	eventDebounce   time.Duration
	smartctlTimeout time.Duration
	excludeDisks    []string
}

func (o *options) bindFlags() {
	flag.BoolVar(&o.printVersion, "version", false, "Print binary version and exit")
	flag.StringVar(&o.nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes Node name where this monitor runs")
	flag.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to")
	flag.BoolVar(&o.metricsEnabled, "metrics-enabled", true, "Expose Prometheus metrics endpoint")
	// udevDataDir points to the host's systemd-udev runtime database directory.
	// In the DaemonSet container, the host's /run/udev/data is mounted here read-only.
	flag.StringVar(&o.udevDataDir, "udev-data-dir", "/run/udev/data", "Path to host udev database directory")
	flag.DurationVar(&o.pollInterval, "poll-interval", 10*time.Minute, "Interval between SMART and disk discovery polling cycles")
	// eventDebounce buffers rapid cascades of udev events (e.g. partition creation, rule execution)
	// so the daemon reconciles only once after the device activity settles.
	flag.DurationVar(&o.eventDebounce, "event-debounce", time.Second, "Quiet period before reconciling a burst of udev add/change events")
	flag.DurationVar(&o.smartctlTimeout, "smartctl-timeout", 30*time.Second, "Timeout for each smartctl command execution")
	flag.Func("exclude-disk", "Exclude disks matching comma-separated exact key=value pairs (node, name, vendor, model, serial, wwn, bus); may be repeated", func(value string) error {
		o.excludeDisks = append(o.excludeDisks, value)
		return nil
	})
}

func (o *options) excludeRules() ([]discovery.ExcludeRule, error) {
	rules := make([]discovery.ExcludeRule, 0, len(o.excludeDisks))
	for _, value := range o.excludeDisks {
		rule, err := discovery.ParseExcludeRule(value)
		if err != nil {
			return nil, fmt.Errorf("invalid --exclude-disk: %w", err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func (o *options) validate() error {
	if o.nodeName == "" {
		return fmt.Errorf("--node-name or the NODE_NAME environment variable must be set")
	}
	if o.pollInterval <= 0 {
		return fmt.Errorf("--poll-interval must be greater than zero, got %s", o.pollInterval)
	}
	if o.eventDebounce < 0 {
		return fmt.Errorf("--event-debounce must not be negative, got %s", o.eventDebounce)
	}
	if o.smartctlTimeout <= 0 {
		return fmt.Errorf("--smartctl-timeout must be greater than zero, got %s", o.smartctlTimeout)
	}
	return nil
}

func main() {
	klog.InitFlags(nil)

	opts := &options{}
	opts.bindFlags()
	flag.Parse()

	if opts.printVersion {
		fmt.Printf("node-disk-sentinel version %s\n", version)
		os.Exit(0)
	}

	// Route controller-runtime and client-go logging through klog.
	ctrl.SetLogger(klog.Background())

	if err := run(opts); err != nil {
		klog.ErrorS(err, "Node Disk Sentinel terminated")
		klog.Flush()
		os.Exit(1)
	}

	klog.InfoS("Node Disk Sentinel monitor stopped gracefully")
	klog.Flush()
}

func newServerMux(metricsEnabled bool, readyFunc func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	if metricsEnabled {
		mux.Handle("/metrics", promhttp.Handler())
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if readyFunc != nil && !readyFunc() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func run(opts *options) error {
	klog.InfoS("Starting Node Disk Sentinel", "version", version, "node", opts.nodeName)

	if err := opts.validate(); err != nil {
		return err
	}
	excludeRules, err := opts.excludeRules()
	if err != nil {
		return err
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Scope caches to this node to eliminate cluster-wide watch fan-out:
	// - PhysicalDisk: watched only where node-disk-sentinel.org/node-name == opts.nodeName
	// - Node: watched only for metadata.name == opts.nodeName.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&v1alpha1.PhysicalDisk{}: {
					Label: labels.SelectorFromSet(labels.Set{
						utils.LabelNodeName: utils.MustFormatValue(opts.nodeName),
					}),
				},
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", opts.nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create controller-runtime manager: %w", err)
	}

	// The label/field-scoped manager cache above only exists to keep the
	// Reconcile-triggering watch cheap. DiskMonitor's own reads and writes
	// (local hardware discovery, listing this node's disks, self-healing the
	// node label) must not go through that same scoped cache: a PhysicalDisk
	// that is missing or has a stale nodeName label would simply be invisible
	// to a cache-backed Get/List, causing duplicate-create errors instead of
	// self-healing. A direct client always sees the true API server state.
	rawClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("failed to create direct Kubernetes client: %w", err)
	}

	recorder := mgr.GetEventRecorderFor(utils.AppName)

	monitor := controller.NewDiskMonitor(
		rawClient,
		recorder,
		&smartmontools.ExecRunner{BinaryPath: "smartctl", Timeout: opts.smartctlTimeout},
		controller.MonitorOptions{
			NodeName:      opts.nodeName,
			UdevDataDir:   opts.udevDataDir,
			PollInterval:  opts.pollInterval,
			EventDebounce: opts.eventDebounce,
			ExcludeRules:  excludeRules,
		},
	)

	mux := newServerMux(opts.metricsEnabled, monitor.Ready)

	server := &http.Server{
		Addr:              opts.metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		klog.InfoS("Starting HTTP server", "addr", opts.metricsAddr, "metricsEnabled", opts.metricsEnabled)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "HTTP server encountered an error")
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.ErrorS(err, "HTTP server shutdown failed")
		}
	}()

	// Register DiskMonitor as Reconciler for PhysicalDisk spec changes.
	if err := monitor.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to setup PhysicalDisk reconciler: %w", err)
	}

	// Register DiskMonitor as Runnable for local udev listener and periodic hardware scans.
	if err := mgr.Add(monitor); err != nil {
		return fmt.Errorf("failed to add DiskMonitor runnable: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	klog.InfoS("Starting Node Disk Sentinel manager", "node", opts.nodeName, "pollInterval", opts.pollInterval)
	return mgr.Start(ctx)
}
