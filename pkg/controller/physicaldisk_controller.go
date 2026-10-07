// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
	"github.com/votdev/node-disk-sentinel/pkg/metrics"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
	"github.com/votdev/node-disk-sentinel/pkg/utils"
)

const (
	defaultPollInterval  = 10 * time.Minute
	defaultEventDebounce = time.Second

	// monitorRetryInterval is the pause before the udev monitor is restarted.
	monitorRetryInterval = 10 * time.Second
)

// MonitorOptions contains configuration options for the local disk monitor and reconciler.
type MonitorOptions struct {
	NodeName      string
	SysDir        string // sysfs mount, defaults to /sys
	UdevDataDir   string // udev runtime database, defaults to /run/udev/data
	PollInterval  time.Duration
	EventDebounce time.Duration
	ExcludeRules  []discovery.ExcludeRule
}

// DiskMonitor coordinates local udev disk discovery, periodic inventory scans,
// and acts as a controller-runtime Reconciler for PhysicalDisk spec changes.
type DiskMonitor struct {
	client.Client
	Recorder    record.EventRecorder
	SmartRunner smartmontools.Runner
	Options     MonitorOptions

	// watch reports inventory changes by calling trigger until ctx is done. It
	// is discovery.Monitor, except in tests.
	watch func(ctx context.Context, trigger func()) error

	nodeRef atomic.Pointer[corev1.Node]
	ready   atomic.Bool
}

// NewDiskMonitor creates a new DiskMonitor instance.
func NewDiskMonitor(c client.Client, recorder record.EventRecorder, runner smartmontools.Runner, opts MonitorOptions) *DiskMonitor {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.EventDebounce <= 0 {
		opts.EventDebounce = defaultEventDebounce
	}
	return &DiskMonitor{
		Client:      c,
		Recorder:    recorder,
		SmartRunner: runner,
		Options:     opts,
		watch:       discovery.Monitor,
	}
}

// ignoreNonUpdateEvents drops create, delete and generic events: the local
// inventory loop creates and deletes PhysicalDisks itself and needs no reconcile
// for them. Update events are filtered by GenerationChangedPredicate.
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
	diskInfos, err := discovery.DiscoverDisks(m.Options.SysDir, m.Options.UdevDataDir, m.Options.NodeName, m.Options.ExcludeRules...)
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

// Start implements manager.Runnable. It reconciles the local disk inventory
// whenever the udev monitor reports a change, and periodically as a backstop.
func (m *DiskMonitor) Start(ctx context.Context) error {
	// Cache Node reference for ownerReference injection.
	var node corev1.Node
	if err := m.Get(ctx, client.ObjectKey{Name: m.Options.NodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node %s for ownerReference: %w", m.Options.NodeName, err)
	}
	m.nodeRef.Store(&node)
	defer m.ready.Store(false)

	// dirty holds "the inventory may have changed". Any number of hints collapse
	// into one pending scan, so the event reader never blocks on a slow scan.
	dirty := make(chan struct{}, 1)
	trigger := func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}

	// The first scan covers disks that existed before the pod started. It also
	// runs where no events arrive, for example in a Kind node, which does not
	// share the host's network namespace.
	trigger()

	// Events from before the socket was bound are not replayed, so the monitor
	// triggers a scan right after every (re)bind. At start-up that trigger and the
	// one above fall into the same delay and cost one scan.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := m.watch(ctx, trigger); err != nil && ctx.Err() == nil {
			klog.ErrorS(err, "udev monitor stopped, reconnecting", "retryIn", monitorRetryInterval)
		}
	}, monitorRetryInterval)

	// Netlink multicast is lossy and not replayable, so a periodic full scan is
	// the authoritative repair path for every missed notification.
	ticker := time.NewTicker(m.Options.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-dirty:
			// Several disks usually appear together (boot, HBA or enclosure
			// hot-plug) and announce themselves one by one. Wait a fixed delay after
			// the first hint instead of restarting a timer on every event, so a
			// steady event stream cannot postpone the scan indefinitely.
			select {
			case <-time.After(m.Options.EventDebounce):
			case <-ctx.Done():
				return nil
			}
			// Hints that arrived during the window are covered by this scan.
			select {
			case <-dirty:
			default:
			}
		}

		if err := m.ReconcileAll(ctx); err != nil {
			klog.ErrorS(err, "Reconcile failed")
		}
		m.ready.Store(true)
	}
}

func (m *DiskMonitor) markDiskMissing(ctx context.Context, disk *v1alpha1.PhysicalDisk, message string) error {
	// Readings of a detached disk must not keep being exported.
	metrics.DeleteDeviceHealthMetrics(m.Options.NodeName, disk.Name)
	metrics.CollectionSuccess.WithLabelValues(m.Options.NodeName, disk.Name, disk.Status.Info.Path).Set(0)

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

// ReconcileAll discovers the local disks, updates their PhysicalDisk CRs, and
// deletes or marks as missing the CRs whose disk is excluded or gone.
func (m *DiskMonitor) ReconcileAll(ctx context.Context) error {
	diskInfos, err := discovery.DiscoverDisks(m.Options.SysDir, m.Options.UdevDataDir, m.Options.NodeName, m.Options.ExcludeRules...)
	if err != nil {
		return fmt.Errorf("failed to discover disks: %w", err)
	}

	klog.InfoS("Discovered physical disks on node", "node", m.Options.NodeName, "count", len(diskInfos))

	// Removals come first: they are cheap and must not wait for the slow
	// smartctl runs below.
	errs := []error{m.markAbsentDisks(ctx, diskInfos)}

	for _, diskInfo := range diskInfos {
		errs = append(errs, m.reconcileDisk(ctx, diskInfo))
	}
	return errors.Join(errs...)
}

// markAbsentDisks deletes PhysicalDisks that are excluded by a rule and marks
// those that are no longer part of the discovered inventory as missing.
func (m *DiskMonitor) markAbsentDisks(ctx context.Context, diskInfos []v1alpha1.DiskInfo) error {
	var diskList v1alpha1.PhysicalDiskList
	if err := m.List(ctx, &diskList, client.MatchingLabels{
		utils.LabelNodeName: utils.MustFormatValue(m.Options.NodeName),
	}); err != nil {
		return fmt.Errorf("failed to list physical disks: %w", err)
	}

	discovered := make(map[string]struct{}, len(diskInfos))
	for _, diskInfo := range diskInfos {
		discovered[discovery.GenerateCRName(m.Options.NodeName, &diskInfo)] = struct{}{}
	}

	var errs []error
	for i := range diskList.Items {
		disk := &diskList.Items[i]
		if disk.Spec.NodeName != m.Options.NodeName {
			continue
		}
		deleted, err := m.deleteIfExcluded(ctx, disk)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, ok := discovered[disk.Name]; !deleted && !ok {
			if err := m.markDiskMissing(ctx, disk, "Hardware device not found during inventory scan"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
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
	if physicalDisk.Labels[utils.LabelNodeName] != nodeLabelValue {
		base := physicalDisk.DeepCopy()
		if physicalDisk.Labels == nil {
			physicalDisk.Labels = make(map[string]string)
		}
		physicalDisk.Labels[utils.LabelNodeName] = nodeLabelValue
		if err := m.Patch(ctx, &physicalDisk, client.MergeFrom(base)); err != nil {
			klog.V(2).InfoS("Failed to ensure node label on disk", "disk", name, "error", err)
		}
	}

	// The status is patched (not updated), so a concurrent write to the resource
	// cannot cause a conflict while the slow smartctl run is in progress.
	base := physicalDisk.DeepCopy()
	physicalDisk.Status.Info = diskInfo

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

	if err := m.Status().Patch(ctx, &physicalDisk, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("failed to update status of %s: %w", name, err)
	}

	klog.InfoS("Reconciled physical disk",
		"disk", name, "status", physicalDisk.Status.Health, "path", diskInfo.Path)
	return nil
}

// Ready reports whether the monitor has completed its initial hardware inventory scan.
func (m *DiskMonitor) Ready() bool {
	return m.ready.Load()
}

func (m *DiskMonitor) ownerReferences() []metav1.OwnerReference {
	node := m.nodeRef.Load()
	if node == nil {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       node.Name,
		UID:        node.UID,
		Controller: new(false),
	}}
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
	// Only a disk that was never assessed is reported as Unknown.
	if disk.Status.Health == "" {
		m.setHealth(disk, v1alpha1.StatusUnknown)
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
	disk.Status.Findings = assessmentResult.Findings
	m.setHealth(disk, assessmentResult.Status)
	m.publishHealthMetrics(disk, diskInfo, smartData, assessmentResult.Status)
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
