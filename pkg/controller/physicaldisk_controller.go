// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
	"github.com/votdev/node-disk-sentinel/pkg/assessment"
	"github.com/votdev/node-disk-sentinel/pkg/discovery"
	"github.com/votdev/node-disk-sentinel/pkg/kernellog"
	"github.com/votdev/node-disk-sentinel/pkg/metrics"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
	"github.com/votdev/node-disk-sentinel/pkg/utils"
)

const (
	defaultPollInterval  = 10 * time.Minute
	defaultEventDebounce = time.Second
	defaultKmsgDebounce  = 5 * time.Second
)

// MonitorOptions contains configuration options for the local disk monitor and reconciler.
type MonitorOptions struct {
	NodeName      string
	UdevDataDir   string
	PollInterval  time.Duration
	EventDebounce time.Duration
	KmsgDebounce  time.Duration
	ExcludeRules  []discovery.ExcludeRule
	KmsgEnabled   bool
	KmsgPath      string
}

// pendingKernelError buffers a not-yet-flushed kernel-reported storage error
// for a single device, coalescing repeated occurrences between reconcile cycles.
type pendingKernelError struct {
	err   *kernellog.KernelError
	count int64
}

// DiskMonitor coordinates local udev disk discovery, periodic inventory scans,
// and acts as a controller-runtime Reconciler for PhysicalDisk spec changes.
type DiskMonitor struct {
	client.Client
	Recorder    record.EventRecorder
	SmartRunner smartmontools.Runner
	KmsgReader  kernellog.Reader
	Options     MonitorOptions

	mu      sync.RWMutex
	nodeRef *corev1.Node
	ready   atomic.Bool

	// knownDevices tracks kernel device names (e.g. "sdb") currently backed by
	// a monitored PhysicalDisk on this node. It is consulted by the kmsg
	// listener's filter so that log lines for excluded, virtual, or foreign
	// devices are dropped before ever reaching the reconcile path.
	knownDevices map[string]struct{}

	// pendingKernelErrors buffers kernel storage errors detected since the
	// last reconcile of a given device. It is drained by reconcileDisk during
	// the next (debounced) reconcile, so that an arbitrarily large burst of
	// kernel log lines results in at most one API read/write per device per
	// debounce window instead of one per line.
	pendingKernelErrors map[string]*pendingKernelError
}

// NewDiskMonitor creates a new DiskMonitor instance.
func NewDiskMonitor(c client.Client, recorder record.EventRecorder, runner smartmontools.Runner, opts MonitorOptions) *DiskMonitor {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.EventDebounce <= 0 {
		opts.EventDebounce = defaultEventDebounce
	}
	if opts.KmsgDebounce <= 0 {
		opts.KmsgDebounce = defaultKmsgDebounce
	}
	return &DiskMonitor{
		Client:              c,
		Recorder:            recorder,
		SmartRunner:         runner,
		Options:             opts,
		knownDevices:        make(map[string]struct{}),
		pendingKernelErrors: make(map[string]*pendingKernelError),
	}
}

// ignoreNonUpdateEvents prevents redundant reconciliations for resources created
// by the local inventory loop. GenerationChangedPredicate handles update events.
var ignoreNonUpdateEvents = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool {
		return false
	},
	DeleteFunc: func(event.DeleteEvent) bool {
		return false
	},
	GenericFunc: func(event.GenericEvent) bool {
		return false
	},
}

var physicalDiskSpecUpdatePredicate = predicate.And(
	predicate.GenerationChangedPredicate{},
	ignoreNonUpdateEvents,
)

// SetupWithManager registers the DiskMonitor as a controller-runtime Reconciler
// for PhysicalDisk resources, watching only user-initiated spec changes.
func (m *DiskMonitor) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PhysicalDisk{}, builder.WithPredicates(physicalDiskSpecUpdatePredicate)).
		Complete(m)
}

// Reconcile handles Kubernetes events for PhysicalDisk resources.
// Triggered when a user updates spec (e.g. extraCmdArgs).
func (m *DiskMonitor) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var physicalDisk v1alpha1.PhysicalDisk
	if err := m.Get(ctx, req.NamespacedName, &physicalDisk); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only reconcile disks belonging to this node.
	if physicalDisk.Spec.NodeName != m.Options.NodeName {
		return ctrl.Result{}, nil
	}

	klog.InfoS("Reconciling PhysicalDisk on spec change", "disk", physicalDisk.Name, "node", physicalDisk.Spec.NodeName)

	// Reconcile this specific disk using current hardware inventory.
	diskInfos, err := discovery.DiscoverDisks(m.Options.UdevDataDir, m.Options.NodeName, m.Options.ExcludeRules...)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to discover disks: %w", err)
	}

	for _, diskInfo := range diskInfos {
		expectedName := discovery.GenerateCRName(m.Options.NodeName, &diskInfo)
		if expectedName == physicalDisk.Name {
			if err := m.reconcileDisk(ctx, diskInfo); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if deleted, err := m.deleteIfExcluded(ctx, &physicalDisk); err != nil || deleted {
		return ctrl.Result{}, err
	}

	// Disk was not found in current hardware inventory; mark as missing if not already.
	if err := m.markDiskMissing(ctx, &physicalDisk, "Hardware device not found during reconcile"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// Start implements manager.Runnable, running local udev event listener and
// periodic polling loop alongside the controller-runtime manager.
func (m *DiskMonitor) Start(ctx context.Context) error {
	// Cache Node reference for ownerReference injection.
	var node corev1.Node
	if err := m.Get(ctx, client.ObjectKey{Name: m.Options.NodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node %s for ownerReference: %w", m.Options.NodeName, err)
	}
	m.mu.Lock()
	m.nodeRef = &node
	m.mu.Unlock()

	ticker := time.NewTicker(m.Options.PollInterval)
	defer ticker.Stop()

	// Initial hardware inventory scan. This covers disks that existed before the
	// pod started; netlink only delivers future multicast packets and is not a
	// replayable history. There is an unavoidable small race between this scan and
	// binding the listener below, which the recurring full inventory scan repairs.
	if err := m.ReconcileAll(ctx); err != nil {
		klog.ErrorS(err, "Initial hardware reconcile failed")
	}
	m.ready.Store(true)

	events, stopListener := m.startEventListener(ctx)
	defer stopListener()

	kmsgEvents, stopKmsgListener := m.startKmsgListener(ctx)
	defer stopKmsgListener()

	var (
		ueventDebounce  *time.Timer
		ueventDebounced <-chan time.Time
		kmsgDebounce    *time.Timer
		kmsgDebounced   <-chan time.Time
	)
	defer func() {
		if ueventDebounce != nil {
			ueventDebounce.Stop()
		}
		if kmsgDebounce != nil {
			kmsgDebounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			m.ready.Store(false)
			return nil

		case <-ticker.C:
			// Netlink multicast is an optimization, not a durable event log. The kernel
			// may drop queued packets during a hotplug storm, and this process can start
			// after the hardware already exists. A full periodic scan is therefore the
			// authoritative repair path for missed add, change, and remove notifications.
			if err := m.ReconcileAll(ctx); err != nil {
				klog.ErrorS(err, "Periodic reconcile failed")
			}

		case kErr, ok := <-kmsgEvents:
			if !ok {
				kmsgEvents = nil
				continue
			}
			// BUFFERING KERNEL ERROR STORMS:
			// A failing drive can emit hundreds of kernel log lines per second -
			// far more than a typical udev hotplug burst. recordKernelError only
			// updates an in-memory buffer (no API calls); the buffered error is
			// drained and applied to the PhysicalDisk exactly once per debounce
			// window by reconcileDisk, reusing the same single-writer path as
			// every other reconcile trigger.
			//
			// Kernel storage errors use a dedicated, longer quiet period (default: 5s)
			// than udev hotplug events (default: 1s). This gives the kernel time to
			// finish SCSI error recovery (command aborts, link/bus resets, retries)
			// and allows drive firmware to update its internal bad-sector reallocation
			// tables before NDS invokes smartctl.
			m.recordKernelError(kErr)
			klog.V(4).InfoS("Queueing reconcile for kernel storage error",
				"device", kErr.Device, "rule", kErr.RuleID, "sector", kErr.Sector, "op", kErr.Op)
			if kmsgDebounce == nil {
				kmsgDebounce = time.NewTimer(m.Options.KmsgDebounce)
				kmsgDebounced = kmsgDebounce.C
			} else {
				kmsgDebounce.Reset(m.Options.KmsgDebounce)
			}

		case uevent, ok := <-events:
			if !ok {
				// A nil channel is disabled in a select. This prevents a stopped listener
				// from causing a busy loop while retaining the periodic recovery path.
				events = nil
				continue
			}
			switch uevent.Action {
			case discovery.ActionRemove, discovery.ActionOffline:
				// IMMEDIATE ACTION ON REMOVAL:
				// When a drive is detached or pulled from a hot-swap bay, issuing further
				// smartctl or sysfs reads will block or fail with EIO.
				// We do NOT debounce removals; we immediately mark the PhysicalDisk CR
				// status as missing and delete exported Prometheus metrics so Kubernetes
				// controllers and storage operators are alerted without delay.
				m.handleDeviceRemoval(ctx, uevent)
			case discovery.ActionAdd, discovery.ActionChange, discovery.ActionOnline:
				// DEBOUNCING HARDWARE EVENT STORMS:
				// When a storage device is inserted, Linux triggers a cascade of uevents:
				//   1. Whole-disk registration ("add" for sda).
				//   2. Kernel partition scanning ("add" for sda1, sda2...).
				//   3. udev helper rules execution ("change" after blkid / ata_id / scsi_id).
				//   4. Filesystem or volume manager probes ("change").
				// Reconciling synchronously on each individual event would hammer the node
				// with 5-10 redundant, expensive smartctl queries and API server updates.
				// Debouncing collapses the entire flurry into a single reconcile run
				// once the quiet period (default: 1s) elapses.
				klog.V(4).InfoS("Queueing reconcile for block device uevent",
					"action", uevent.Action, "device", uevent.KernelName())
				if ueventDebounce == nil {
					ueventDebounce = time.NewTimer(m.Options.EventDebounce)
					ueventDebounced = ueventDebounce.C
				} else {
					ueventDebounce.Reset(m.Options.EventDebounce)
				}
			case discovery.ActionRescan:
				// ENOBUFS means one or more events were lost. Reconcile immediately and
				// discard a pending debounce timer: its scan is now redundant and could
				// otherwise run shortly after this full recovery reconciliation.
				if ueventDebounce != nil {
					ueventDebounce.Stop()
					ueventDebounce, ueventDebounced = nil, nil
				}
				klog.InfoS("Triggering immediate reconcile due to udev buffer overrun")
				if err := m.ReconcileAll(ctx); err != nil {
					klog.ErrorS(err, "Reconcile after udev buffer overrun failed")
				}
			}

		case <-ueventDebounced:
			ueventDebounce, ueventDebounced = nil, nil
			if err := m.ReconcileAll(ctx); err != nil {
				klog.ErrorS(err, "Reconcile after udev event burst failed")
			}

		case <-kmsgDebounced:
			kmsgDebounce, kmsgDebounced = nil, nil
			if err := m.ReconcileAll(ctx); err != nil {
				klog.ErrorS(err, "Reconcile after kernel error burst failed")
			}
		}
	}
}

// startEventListener subscribes to udev events. If the socket is unavailable
// (for example in a container without hostNetwork or without access to the host
// netlink namespace), the monitor logs the failure and gracefully retries periodically.
// If the listener encounters an unexpected error, a supervisor loop automatically
// reconnects to ensure event monitoring is not permanently lost.
func (m *DiskMonitor) startEventListener(ctx context.Context) (<-chan *discovery.UEvent, func()) {
	events := make(chan *discovery.UEvent, 32)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(events)

		for {
			listener, err := discovery.NewUEventListener()
			if err != nil {
				klog.ErrorS(err, "Failed to start udev netlink listener; retrying in 10s")
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}

			klog.InfoS("Udev netlink event listener active")
			if err := listener.Listen(ctx, events); err != nil {
				if ctx.Err() != nil {
					return
				}
				klog.ErrorS(err, "Udev netlink listener stopped with error; reconnecting in 5s")
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			} else {
				if ctx.Err() != nil {
					// Clean shutdown via context.
					return
				}
				klog.Warning("Udev netlink listener stopped unexpectedly; reconnecting in 5s")
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return events, func() { <-done }
}

// startKmsgListener subscribes to kernel log messages for block device I/O errors.
func (m *DiskMonitor) startKmsgListener(ctx context.Context) (<-chan *kernellog.KernelError, func()) {
	if !m.Options.KmsgEnabled {
		return nil, func() {}
	}

	events := make(chan *kernellog.KernelError, 64)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(events)

		for {
			reader := m.KmsgReader
			if reader == nil {
				var err error
				reader, err = kernellog.NewKmsgReader(m.Options.KmsgPath)
				if err != nil {
					klog.ErrorS(err, "Failed to start kernel message listener; retrying in 30s", "path", m.Options.KmsgPath)
					select {
					case <-time.After(30 * time.Second):
						continue
					case <-ctx.Done():
						return
					}
				}
			}

			klog.InfoS("Kernel log message listener active", "path", m.Options.KmsgPath)
			monitor := kernellog.NewMonitor(reader, m.isKnownDevice)
			if err := monitor.Listen(ctx, events); err != nil {
				if ctx.Err() != nil {
					return
				}
				klog.ErrorS(err, "Kernel log listener stopped with error; reconnecting in 10s")
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			} else {
				if ctx.Err() != nil {
					return
				}
				klog.Warning("Kernel log listener stopped unexpectedly; reconnecting in 10s")
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return events, func() { <-done }
}

// isKnownDevice reports whether dev currently backs a monitored PhysicalDisk
// on this node. Used as the kmsg listener's filter so that log lines for
// excluded, virtual, or foreign devices never reach the reconcile path.
func (m *DiskMonitor) isKnownDevice(dev string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.knownDevices[dev]
	return ok
}

// rememberDevice marks dev as currently backed by a monitored PhysicalDisk.
func (m *DiskMonitor) rememberDevice(dev string) {
	if dev == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.knownDevices[dev] = struct{}{}
}

// forgetDevice removes dev from the known-device index and drops any not-yet-
// flushed kernel error buffered for it, e.g. when the disk is removed or excluded.
func (m *DiskMonitor) forgetDevice(dev string) {
	if dev == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.knownDevices, dev)
	delete(m.pendingKernelErrors, dev)
}

// recordKernelError buffers a detected kernel storage error in memory. It performs
// no API calls: an arbitrarily large burst of kernel log lines for the same device
// (a failing drive can emit hundreds per second) results in a single buffered entry
// that mergePendingKernelError later drains exactly once per debounced reconcile.
func (m *DiskMonitor) recordKernelError(kErr *kernellog.KernelError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pendingKernelErrors[kErr.Device]; ok {
		p.err = kErr
		p.count++
	} else {
		m.pendingKernelErrors[kErr.Device] = &pendingKernelError{err: kErr, count: 1}
	}
}

// mergePendingKernelError drains any kernel error buffered for disk's device and,
// if present, attaches/updates the corresponding Finding and increments the
// kernel error metric. It does not touch disk.Status.Health directly; the health
// merge (prioritizing confirmed SMART findings, never auto-healing a kernel error
// away) happens uniformly in applyCollectionSuccess/applyCollectionFailure based
// on the resulting Findings, exactly like every other diagnostic source.
func (m *DiskMonitor) mergePendingKernelError(disk *v1alpha1.PhysicalDisk) {
	device := disk.Status.Info.Name

	m.mu.Lock()
	p, ok := m.pendingKernelErrors[device]
	if ok {
		delete(m.pendingKernelErrors, device)
	}
	m.mu.Unlock()
	if !ok {
		return
	}

	kErr := p.err
	metrics.KernelErrors.WithLabelValues(m.Options.NodeName, disk.Name, disk.Status.Info.Path, string(kErr.Op)).Add(float64(p.count))

	findingID := kErr.RuleID
	if findingID == "" {
		findingID = kernellog.FindingIDPrefix + "ERROR"
	}
	findingMsg := fmt.Sprintf("%s on dev %s (op %s, sector %d)", kErr.Description, kErr.Device, kErr.Op, kErr.Sector)
	if p.count > 1 {
		findingMsg = fmt.Sprintf("%s [x%d since last check]", findingMsg, p.count)
	}

	findingUpdated := false
	for j := range disk.Status.Findings {
		if disk.Status.Findings[j].ID == findingID {
			disk.Status.Findings[j].RawValue = kErr.Sector
			disk.Status.Findings[j].Message = findingMsg
			findingUpdated = true
			break
		}
	}
	if !findingUpdated {
		disk.Status.Findings = append(disk.Status.Findings, v1alpha1.Finding{
			ID:            findingID,
			AttributeName: "KernelStorage",
			RawValue:      kErr.Sector,
			Message:       findingMsg,
		})
	}
}

// handleDeviceRemoval flags the disks of a removed device as missing.
func (m *DiskMonitor) handleDeviceRemoval(ctx context.Context, uevent *discovery.UEvent) {
	var diskList v1alpha1.PhysicalDiskList
	if err := m.List(ctx, &diskList, client.MatchingLabels{
		utils.LabelNodeName: utils.MustFormatValue(m.Options.NodeName),
	}); err != nil {
		klog.ErrorS(err, "Failed to list physical disks for removal event")
		return
	}

	// Match by the kernel name saved at the last inventory scan. We cannot use the
	// preferred by-id symlink here: udev removes that symlink as part of processing a
	// removal event, while DEVPATH still retains the kernel name (see KernelName).
	devName := uevent.KernelName()
	for i := range diskList.Items {
		disk := &diskList.Items[i]
		if disk.Spec.NodeName != m.Options.NodeName || disk.Status.Info.Name != devName {
			continue
		}

		if err := m.markDiskMissing(ctx, disk, fmt.Sprintf("Device was removed from the node (udev action %q)", uevent.Action)); err != nil {
			klog.ErrorS(err, "Failed to mark removed disk as missing", "disk", disk.Name)
		}
	}
}

func (m *DiskMonitor) markDiskMissing(ctx context.Context, disk *v1alpha1.PhysicalDisk, message string) error {
	// Readings of a detached disk must not keep being exported.
	metrics.DeleteDeviceHealthMetrics(m.Options.NodeName, disk.Name)
	metrics.CollectionSuccess.WithLabelValues(m.Options.NodeName, disk.Name, disk.Status.Info.Path).Set(0)
	// Stop accepting kmsg lines for this device and drop any buffered error.
	m.forgetDevice(disk.Status.Info.Name)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestDisk v1alpha1.PhysicalDisk
		if err := m.Get(ctx, client.ObjectKey{Name: disk.Name}, &latestDisk); err != nil {
			return err
		}

		changed := meta.SetStatusCondition(&latestDisk.Status.Conditions, metav1.Condition{
			Type:    v1alpha1.ConditionDataCollected,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.ReasonDiskMissing,
			Message: message,
		})
		if !changed {
			return nil
		}

		klog.InfoS("Marking disk as missing", "disk", disk.Name, "path", disk.Status.Info.Path, "reason", message)
		m.recordEvent(&latestDisk, corev1.EventTypeWarning, v1alpha1.ReasonDiskMissing, message)

		return m.Status().Update(ctx, &latestDisk)
	})
	if err != nil {
		return fmt.Errorf("failed to mark disk %s as missing: %w", disk.Name, err)
	}
	return nil
}

// ReconcileAll discovers local disks and updates their corresponding PhysicalDisk CRs.
func (m *DiskMonitor) ReconcileAll(ctx context.Context) error {
	diskInfos, err := discovery.DiscoverDisks(m.Options.UdevDataDir, m.Options.NodeName, m.Options.ExcludeRules...)
	if err != nil {
		return fmt.Errorf("failed to discover disks: %w", err)
	}

	klog.InfoS("Discovered physical disks on node", "node", m.Options.NodeName, "count", len(diskInfos))

	for _, diskInfo := range diskInfos {
		if err := m.reconcileDisk(ctx, diskInfo); err != nil {
			klog.ErrorS(err, "Failed to reconcile disk", "device", diskInfo.Name, "path", diskInfo.Path)
		}
	}

	// A periodic scan must also detect removals because netlink events can be
	// missed while the process is restarting or, in Kind, are not forwarded.
	var diskList v1alpha1.PhysicalDiskList
	if err := m.List(ctx, &diskList, client.MatchingLabels{
		utils.LabelNodeName: utils.MustFormatValue(m.Options.NodeName),
	}); err != nil {
		return fmt.Errorf("failed to list physical disks: %w", err)
	}

	discoveredNames := make(map[string]struct{}, len(diskInfos))
	for _, diskInfo := range diskInfos {
		discoveredNames[discovery.GenerateCRName(m.Options.NodeName, &diskInfo)] = struct{}{}
	}

	for i := range diskList.Items {
		disk := &diskList.Items[i]
		if disk.Spec.NodeName != m.Options.NodeName {
			continue
		}
		if deleted, err := m.deleteIfExcluded(ctx, disk); err != nil {
			return err
		} else if deleted {
			continue
		}
		if _, exists := discoveredNames[disk.Name]; !exists {
			if err := m.markDiskMissing(ctx, disk, "Hardware device not found during inventory scan"); err != nil {
				return err
			}
		}
	}

	return nil
}

// deleteIfExcluded deletes a PhysicalDisk and drops its metrics if it matches an exclusion rule.
// Returns (true, nil) if the disk was excluded and deleted, or (false, nil) if not excluded.
func (m *DiskMonitor) deleteIfExcluded(ctx context.Context, disk *v1alpha1.PhysicalDisk) (bool, error) {
	rule, excluded := discovery.FindMatchingRule(m.Options.NodeName, disk.Status.Info, m.Options.ExcludeRules)
	if !excluded {
		return false, nil
	}
	if err := m.Delete(ctx, disk); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("failed to delete excluded PhysicalDisk %s: %w", disk.Name, err)
	}
	metrics.DeleteAllDiskMetrics(m.Options.NodeName, disk.Name)
	m.forgetDevice(disk.Status.Info.Name)
	klog.InfoS("Deleted excluded physical disk",
		"disk", disk.Name, "path", disk.Status.Info.Path, "rule", rule.String())
	return true, nil
}

func (m *DiskMonitor) reconcileDisk(ctx context.Context, diskInfo v1alpha1.DiskInfo) error {
	name := discovery.GenerateCRName(m.Options.NodeName, &diskInfo)

	var physicalDisk v1alpha1.PhysicalDisk
	err := m.Get(ctx, client.ObjectKey{Name: name}, &physicalDisk)
	nodeLabelValue := utils.MustFormatValue(m.Options.NodeName)

	switch {
	case apierrors.IsNotFound(err):
		physicalDisk = v1alpha1.PhysicalDisk{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					utils.LabelNodeName: nodeLabelValue,
				},
				OwnerReferences: m.ownerReferences(),
			},
			Spec: v1alpha1.PhysicalDiskSpec{NodeName: m.Options.NodeName},
		}
		if err := m.Create(ctx, &physicalDisk); err != nil {
			return fmt.Errorf("failed to create PhysicalDisk %s: %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("failed to get PhysicalDisk %s: %w", name, err)
	}

	// Ensure label is present on existing disks.
	if physicalDisk.Labels == nil || physicalDisk.Labels[utils.LabelNodeName] != nodeLabelValue {
		if physicalDisk.Labels == nil {
			physicalDisk.Labels = make(map[string]string)
		}
		physicalDisk.Labels[utils.LabelNodeName] = nodeLabelValue
		if err := m.Update(ctx, &physicalDisk); err != nil && !apierrors.IsConflict(err) {
			klog.V(2).InfoS("Failed to ensure node label on disk", "disk", name, "error", err)
		}
	}

	physicalDisk.Status.Info = diskInfo

	// Mark this device as known so the kmsg listener's filter accepts kernel
	// log lines for it, and drain any kernel error buffered since the last
	// reconcile of this disk (see recordKernelError/mergePendingKernelError).
	m.rememberDevice(diskInfo.Name)
	m.mergePendingKernelError(&physicalDisk)

	var extraCmdArgs string
	if physicalDisk.Spec.Smartmontools != nil && physicalDisk.Spec.Smartmontools.Smartctl != nil {
		extraCmdArgs = physicalDisk.Spec.Smartmontools.Smartctl.ExtraCmdArgs
	}

	smartData, collectErr := m.SmartRunner.Collect(ctx, diskInfo.Path, extraCmdArgs)
	if collectErr != nil {
		m.applyCollectionFailure(&physicalDisk, diskInfo, collectErr)
	} else {
		if smartData.FirmwareVersion != "" {
			diskInfo.FirmwareVersion = smartData.FirmwareVersion
			physicalDisk.Status.Info.FirmwareVersion = smartData.FirmwareVersion
		}
		m.applyCollectionSuccess(&physicalDisk, diskInfo, smartData)
	}

	var latestDisk v1alpha1.PhysicalDisk
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := m.Get(ctx, client.ObjectKey{Name: name}, &latestDisk); err != nil {
			return err
		}
		latestDisk.Status.Info = physicalDisk.Status.Info
		latestDisk.Status.Health = physicalDisk.Status.Health
		latestDisk.Status.Findings = physicalDisk.Status.Findings
		if physicalDisk.Status.LastDataCollectedTime != nil {
			latestDisk.Status.LastDataCollectedTime = physicalDisk.Status.LastDataCollectedTime
		}
		if physicalDisk.Status.Telemetry != nil {
			latestDisk.Status.Telemetry = physicalDisk.Status.Telemetry
		}
		for _, c := range physicalDisk.Status.Conditions {
			meta.SetStatusCondition(&latestDisk.Status.Conditions, c)
		}
		return m.Status().Update(ctx, &latestDisk)
	})
	if err != nil {
		return fmt.Errorf("failed to update status of %s: %w", name, err)
	}

	physicalDisk.Status = latestDisk.Status

	klog.InfoS("Reconciled physical disk",
		"disk", name, "status", physicalDisk.Status.Health, "path", diskInfo.Path)
	return nil
}

// Ready reports whether the monitor has completed its initial hardware inventory scan.
func (m *DiskMonitor) Ready() bool {
	return m.ready.Load()
}

func (m *DiskMonitor) ownerReferences() []metav1.OwnerReference {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.nodeRef == nil {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       m.nodeRef.Name,
		UID:        m.nodeRef.UID,
		Controller: new(false),
	}}
}

// hasKernelFinding reports whether findings contains any kernel-log-derived
// diagnostic entry (see kernellog.FindingIDPrefix).
func hasKernelFinding(findings []v1alpha1.Finding) bool {
	for _, f := range findings {
		if strings.HasPrefix(f.ID, kernellog.FindingIDPrefix) {
			return true
		}
	}
	return false
}

func (m *DiskMonitor) applyCollectionFailure(disk *v1alpha1.PhysicalDisk, diskInfo v1alpha1.DiskInfo, collectErr error) {
	metrics.CollectionSuccess.WithLabelValues(m.Options.NodeName, disk.Name, diskInfo.Path).Set(0)

	var reason, message string
	switch {
	case errors.Is(collectErr, smartmontools.ErrDeviceInStandby):
		reason = v1alpha1.ReasonSkippedStandby
		message = "Disk is in SLEEP or STANDBY mode; collection was skipped to preserve the power state"
	case errors.Is(collectErr, smartmontools.ErrDeviceNotFound):
		reason = v1alpha1.ReasonDiskMissing
		message = fmt.Sprintf("Device is missing or unreadable: %v", collectErr)
		klog.InfoS("Disk marked as missing due to collection failure", "disk", disk.Name, "path", diskInfo.Path, "error", collectErr)
	case errors.Is(collectErr, smartmontools.ErrSmartUnsupported):
		reason = v1alpha1.ReasonSmartUnsupported
		message = "SMART is unavailable or disabled on this device"
	default:
		reason = v1alpha1.ReasonFailed
		message = fmt.Sprintf("smartctl execution failed: %v", collectErr)
	}

	changed := meta.SetStatusCondition(&disk.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.ConditionDataCollected,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	if changed {
		m.recordEvent(disk, corev1.EventTypeWarning, reason, message)
	}

	// A failed collection says nothing about the hardware itself, so a
	// previously determined health status and telemetry are preserved.
	// A disk that was never assessed is reported as Unknown, unless a kernel
	// storage error was just merged for it (mergePendingKernelError runs
	// before SMART collection in reconcileDisk), in which case it is reported
	// as KernelErrors immediately instead of waiting for the next successful
	// SMART poll to notice the finding.
	switch {
	case disk.Status.Health == "":
		if hasKernelFinding(disk.Status.Findings) {
			m.setHealth(disk, v1alpha1.StatusKernelErrors)
		} else {
			m.setHealth(disk, v1alpha1.StatusUnknown)
		}
	case hasKernelFinding(disk.Status.Findings) &&
		(disk.Status.Health == v1alpha1.StatusGood || disk.Status.Health == v1alpha1.StatusAttributeFailedInPast):
		m.setHealth(disk, v1alpha1.StatusKernelErrors)
	}
}

func (m *DiskMonitor) applyCollectionSuccess(disk *v1alpha1.PhysicalDisk, diskInfo v1alpha1.DiskInfo, smartData *smartmontools.SmartctlOutput) {
	metrics.CollectionSuccess.WithLabelValues(m.Options.NodeName, disk.Name, diskInfo.Path).Set(1)

	now := metav1.Now()
	disk.Status.LastDataCollectedTime = &now
	disk.Status.Telemetry = extractTelemetry(smartData)

	meta.SetStatusCondition(&disk.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.ConditionDataCollected,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonSucceeded,
		Message: "SMART data collected successfully",
	})

	assessmentResult := assessment.Evaluate(smartData)

	// Preserve any existing kernel storage findings across SMART collections.
	var kernelFindings []v1alpha1.Finding
	for _, f := range disk.Status.Findings {
		if strings.HasPrefix(f.ID, kernellog.FindingIDPrefix) {
			kernelFindings = append(kernelFindings, f)
		}
	}

	disk.Status.Findings = assessmentResult.Findings
	if len(kernelFindings) > 0 {
		disk.Status.Findings = append(disk.Status.Findings, kernelFindings...)
	}

	// Determine final health status:
	// Confirmed hardware errors from SMART (SelfAssessmentFailed, ExcessiveSectorErrors,
	// AttributeFailingNow, SectorErrors) take precedence.
	// If SMART reports Good or AttributeFailedInPast, but this disk has experienced
	// kernel storage errors, it remains degraded as StatusKernelErrors (no auto-healing).
	finalStatus := assessmentResult.Status
	if hasKernelFinding(disk.Status.Findings) &&
		(finalStatus == v1alpha1.StatusGood || finalStatus == v1alpha1.StatusAttributeFailedInPast) {
		finalStatus = v1alpha1.StatusKernelErrors
	}

	m.setHealth(disk, finalStatus)
	m.publishHealthMetrics(disk, diskInfo, smartData, finalStatus)
}

// extractTelemetry builds a compact snapshot of operational data from smartctl output.
func extractTelemetry(smartData *smartmontools.SmartctlOutput) *v1alpha1.DiskTelemetry {
	if smartData == nil {
		return nil
	}
	diskTelemetry := &v1alpha1.DiskTelemetry{}

	// 1. Temperature (prefer normalized top-level, fallback to NVMe or ATA attribute).
	if smartData.Temperature != nil && smartData.Temperature.Current > 0 {
		diskTelemetry.TemperatureCelsius = new(smartData.Temperature.Current)
	} else if smartData.NvmeSmart != nil && smartData.NvmeSmart.Temperature > 0 {
		diskTelemetry.TemperatureCelsius = new(smartData.NvmeSmart.Temperature)
	}

	// 2. Power-on time (prefer top-level).
	if smartData.PowerOnTime != nil {
		diskTelemetry.PowerOnHours = new(smartData.PowerOnTime.Hours)
	}

	// 3. Power-cycle count (prefer top-level).
	if smartData.PowerCycleCount != nil {
		diskTelemetry.PowerCycleCount = new(*smartData.PowerCycleCount)
	}

	// 4. ATA-specific attributes.
	if smartData.AtaSmartAttributes != nil {
		for _, attr := range smartData.AtaSmartAttributes.Table {
			switch attr.ID {
			case 5:
				diskTelemetry.ReallocatedSectors = new(attr.Raw.Value)
			case 197:
				diskTelemetry.PendingSectors = new(attr.Raw.Value)
			case 194, 190:
				if diskTelemetry.TemperatureCelsius == nil && attr.Raw.Value > 0 {
					diskTelemetry.TemperatureCelsius = new(int(attr.Raw.Value))
				}
			case 9:
				if diskTelemetry.PowerOnHours == nil && attr.Raw.Value > 0 {
					diskTelemetry.PowerOnHours = new(attr.Raw.Value)
				}
			case 12:
				if diskTelemetry.PowerCycleCount == nil && attr.Raw.Value > 0 {
					diskTelemetry.PowerCycleCount = new(attr.Raw.Value)
				}
			}
		}
	}

	// 5. NVMe-specific telemetry.
	if smartData.NvmeSmart != nil {
		diskTelemetry.PercentageUsed = new(smartData.NvmeSmart.PercentageUsed)
		diskTelemetry.AvailableSpare = new(smartData.NvmeSmart.AvailableSpare)
		diskTelemetry.CriticalWarning = new(smartData.NvmeSmart.CriticalWarning)
		diskTelemetry.MediaErrors = new(smartData.NvmeSmart.MediaErrors)
	}

	return diskTelemetry
}

// setHealth keeps status.health and the Degraded condition reason in sync
// and reports a health transition exactly once.
func (m *DiskMonitor) setHealth(disk *v1alpha1.PhysicalDisk, status v1alpha1.DiskHealthStatus) {
	disk.Status.Health = status

	var conditionStatus metav1.ConditionStatus
	switch status {
	case v1alpha1.StatusUnknown:
		conditionStatus = metav1.ConditionUnknown
	case v1alpha1.StatusGood:
		conditionStatus = metav1.ConditionFalse
	default:
		conditionStatus = metav1.ConditionTrue
	}

	changed := meta.SetStatusCondition(&disk.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.ConditionDegraded,
		Status:  conditionStatus,
		Reason:  string(status),
		Message: fmt.Sprintf("Disk health assessed as %s", status),
	})
	if !changed {
		return
	}

	switch conditionStatus {
	case metav1.ConditionTrue:
		m.recordEvent(disk, corev1.EventTypeWarning, string(status),
			fmt.Sprintf("Disk %s is degraded: %s", disk.Status.Info.Path, status))
	case metav1.ConditionFalse:
		m.recordEvent(disk, corev1.EventTypeNormal, string(status),
			fmt.Sprintf("Disk %s is healthy", disk.Status.Info.Path))
	}
}

func (m *DiskMonitor) publishHealthMetrics(
	disk *v1alpha1.PhysicalDisk,
	diskInfo v1alpha1.DiskInfo,
	smartData *smartmontools.SmartctlOutput,
	status v1alpha1.DiskHealthStatus,
) {
	node := m.Options.NodeName
	diskName := disk.Name
	devicePath := diskInfo.Path

	// 1. Static smartctl_device metadata compatible with smartctl_exporter dashboards.
	metrics.Device.WithLabelValues(
		node,
		diskName,
		devicePath,
		smartData.Device.Type,
		smartData.Device.Protocol,
		smartData.ModelFamily,
		smartData.ModelName,
		smartData.SerialNumber,
		"",
		diskInfo.FirmwareVersion,
		"",
		"",
		"",
		"",
		"",
		"",
		"",
	).Set(1.0)
	metrics.Version.WithLabelValues(node, "1", metrics.SmartctlVersion(smartData.Smartctl.Version), "", "").Set(1.0)

	// 2. SMART overall status (1 = passed, 0 = failed).
	smartPassed := 0.0
	if smartData.SmartStatus.Passed {
		smartPassed = 1.0
	}
	metrics.SmartStatus.WithLabelValues(node, diskName, devicePath).Set(smartPassed)

	// 3. Evaluated Health status.
	for _, candidate := range v1alpha1.AllDiskHealthStatuses {
		value := 0.0
		if candidate == status {
			value = 1.0
		}
		metrics.HealthStatus.
			WithLabelValues(node, diskName, devicePath, string(candidate)).
			Set(value)
	}

	// 4. Temperature.
	if disk.Status.Telemetry != nil && disk.Status.Telemetry.TemperatureCelsius != nil {
		metrics.Temperature.
			WithLabelValues(node, diskName, devicePath, "current").
			Set(float64(*disk.Status.Telemetry.TemperatureCelsius))
	}

	// 5. Power-on seconds.
	if disk.Status.Telemetry != nil && disk.Status.Telemetry.PowerOnHours != nil {
		metrics.PowerOnSeconds.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(*disk.Status.Telemetry.PowerOnHours * 3600))
	}

	// 6. Power cycle count.
	if disk.Status.Telemetry != nil && disk.Status.Telemetry.PowerCycleCount != nil {
		metrics.PowerCycleCount.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(*disk.Status.Telemetry.PowerCycleCount))
	}

	// 7. NVMe metrics.
	if smartData.NvmeSmart != nil {
		metrics.PercentageUsed.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.PercentageUsed))
		metrics.AvailableSpare.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.AvailableSpare))
		metrics.AvailableSpareThreshold.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.AvailableSpareThreshold))
		metrics.CriticalWarning.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.CriticalWarning))
		metrics.MediaErrors.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.MediaErrors))
		metrics.NumErrLogEntries.
			WithLabelValues(node, diskName, devicePath).
			Set(float64(smartData.NvmeSmart.NumErrLogEntries))
	}

	// 8. ATA Attributes (smartctl_device_attribute).
	if smartData.AtaSmartAttributes != nil {
		for _, attr := range smartData.AtaSmartAttributes.Table {
			attrIDStr := strconv.Itoa(attr.ID)
			metrics.Attribute.
				WithLabelValues(node, diskName, devicePath, attr.Name, attrIDStr, "raw").
				Set(float64(attr.Raw.Value))
			metrics.Attribute.
				WithLabelValues(node, diskName, devicePath, attr.Name, attrIDStr, "value").
				Set(float64(attr.Value))
			metrics.Attribute.
				WithLabelValues(node, diskName, devicePath, attr.Name, attrIDStr, "worst").
				Set(float64(attr.Worst))
			metrics.Attribute.
				WithLabelValues(node, diskName, devicePath, attr.Name, attrIDStr, "thresh").
				Set(float64(attr.Threshold))
		}
	}
}

func (m *DiskMonitor) recordEvent(disk *v1alpha1.PhysicalDisk, eventType, reason, message string) {
	if m.Recorder == nil {
		return
	}
	m.Recorder.Event(disk, eventType, reason, message)
}
