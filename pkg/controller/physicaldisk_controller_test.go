// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
	"github.com/votdev/node-disk-sentinel/pkg/discovery"
	"github.com/votdev/node-disk-sentinel/pkg/metrics"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
	"github.com/votdev/node-disk-sentinel/pkg/utils"
)

const testNodeName = "test-node"

func TestPhysicalDiskSpecUpdatePredicate(t *testing.T) {
	oldDisk := &v1alpha1.PhysicalDisk{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	updatedDisk := oldDisk.DeepCopy()
	updatedDisk.Generation = 2

	if physicalDiskSpecUpdatePredicate.Create(event.CreateEvent{Object: oldDisk}) {
		t.Error("Create event must not trigger a redundant reconciliation")
	}
	if physicalDiskSpecUpdatePredicate.Delete(event.DeleteEvent{Object: oldDisk}) {
		t.Error("Delete event must not trigger reconciliation")
	}
	if physicalDiskSpecUpdatePredicate.Generic(event.GenericEvent{Object: oldDisk}) {
		t.Error("Generic event must not trigger reconciliation")
	}
	if physicalDiskSpecUpdatePredicate.Update(event.UpdateEvent{ObjectOld: oldDisk, ObjectNew: oldDisk.DeepCopy()}) {
		t.Error("Update without a generation change must not trigger reconciliation")
	}
	if !physicalDiskSpecUpdatePredicate.Update(event.UpdateEvent{ObjectOld: oldDisk, ObjectNew: updatedDisk}) {
		t.Error("Update with a generation change must trigger reconciliation")
	}
	if physicalDiskSpecUpdatePredicate.Update(event.UpdateEvent{}) {
		t.Error("Update without both objects must not trigger reconciliation")
	}
}

type mockSmartctlRunner struct {
	output *smartmontools.SmartctlOutput
	err    error
}

func (m *mockSmartctlRunner) Collect(context.Context, string, string) (*smartmontools.SmartctlOutput, error) {
	return m.output, m.err
}

func healthyATADisk() *smartmontools.SmartctlOutput {
	return &smartmontools.SmartctlOutput{
		FirmwareVersion: "01.01A01",
		SmartStatus:     smartmontools.SmartStatus{Passed: true},
		AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
			Table: []smartmontools.AtaAttribute{
				{ID: 5, Name: "Reallocated_Sector_Ct", Raw: smartmontools.AtaAttributeRaw{Value: 0}},
				{ID: 197, Name: "Current_Pending_Sector", Raw: smartmontools.AtaAttributeRaw{Value: 0}},
			},
		},
	}
}

func failingATADisk() *smartmontools.SmartctlOutput {
	return &smartmontools.SmartctlOutput{
		SmartStatus:        smartmontools.SmartStatus{Passed: false},
		AtaSmartAttributes: &smartmontools.AtaSmartAttributes{},
	}
}

// newTestMonitor wires a monitor against a fake client and a udev database
// containing a single ATA disk.
func newTestMonitor(t *testing.T, runner smartmontools.Runner) (*DiskMonitor, client.Client, *record.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register api scheme: %v", err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName, UID: "node-uid-12345"}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(node).
		WithStatusSubresource(&v1alpha1.PhysicalDisk{}).
		Build()

	udevDir := t.TempDir()
	udevRecord := `S:disk/by-id/ata-WDC_WD10EZEX-08WN4A0
S:disk/by-id/wwn-0x50014ee265882b7f
E:DEVNAME=/dev/sda
E:DEVTYPE=disk
E:ID_BUS=ata
E:ID_MODEL=WDC_WD10EZEX-08WN4A0
E:ID_SERIAL=WDC_WD10EZEX-08WN4A0_WD-WCC6Y7PL7345
E:ID_WWN=0x50014ee265882b7f
E:MAJOR=8
E:MINOR=0
`
	if err := os.WriteFile(filepath.Join(udevDir, "b8:0"), []byte(udevRecord), 0o644); err != nil {
		t.Fatalf("failed to write udev record: %v", err)
	}

	recorder := record.NewFakeRecorder(16)
	monitor := NewDiskMonitor(fakeClient, recorder, runner, MonitorOptions{
		NodeName:     testNodeName,
		UdevDataDir:  udevDir,
		PollInterval: 10 * time.Minute,
	})
	monitor.nodeRef = node

	return monitor, fakeClient, recorder
}

func singleDisk(t *testing.T, c client.Client) v1alpha1.PhysicalDisk {
	t.Helper()

	var disks v1alpha1.PhysicalDiskList
	if err := c.List(context.Background(), &disks); err != nil {
		t.Fatalf("failed to list disks: %v", err)
	}
	if len(disks.Items) != 1 {
		t.Fatalf("got %d disks; want exactly 1", len(disks.Items))
	}
	return disks.Items[0]
}

// assertHealthInvariant guards the documented contract that health and
// the Degraded condition reason never diverge.
func assertHealthInvariant(t *testing.T, disk v1alpha1.PhysicalDisk) {
	t.Helper()

	degraded := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDegraded)
	if degraded == nil {
		t.Fatalf("Degraded condition is missing")
	}
	if degraded.Reason != string(disk.Status.Health) {
		t.Errorf("Degraded reason = %q; want it to equal health %q",
			degraded.Reason, disk.Status.Health)
	}
}

func TestReconcileAllHealthyDisk(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})

	if err := monitor.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	if disk.Spec.NodeName != testNodeName {
		t.Errorf("nodeName = %q; want %q", disk.Spec.NodeName, testNodeName)
	}
	if disk.Labels[utils.LabelNodeName] != utils.MustFormatValue(testNodeName) {
		t.Errorf("label %s = %q; want %q", utils.LabelNodeName, disk.Labels[utils.LabelNodeName], utils.MustFormatValue(testNodeName))
	}
	if disk.Status.Health != v1alpha1.StatusGood {
		t.Errorf("health = %q; want Good", disk.Status.Health)
	}
	if disk.Status.Info.WWN != "0x50014ee265882b7f" {
		t.Errorf("wwn = %q; want 0x50014ee265882b7f", disk.Status.Info.WWN)
	}
	if disk.Status.Info.CanonicalPath != "/dev/sda" {
		t.Errorf("canonicalPath = %q; want /dev/sda", disk.Status.Info.CanonicalPath)
	}
	if disk.Status.Info.FirmwareVersion != "01.01A01" {
		t.Errorf("firmwareVersion = %q; want 01.01A01", disk.Status.Info.FirmwareVersion)
	}
	if disk.Status.LastDataCollectedTime == nil {
		t.Error("lastDataCollectedTime must be set after a successful collection")
	}
	if len(disk.OwnerReferences) != 1 || disk.OwnerReferences[0].Kind != "Node" {
		t.Errorf("expected a single Node ownerReference, got %#v", disk.OwnerReferences)
	}
	if disk.OwnerReferences[0].BlockOwnerDeletion != nil && *disk.OwnerReferences[0].BlockOwnerDeletion {
		t.Errorf("BlockOwnerDeletion must not be true, got %v", *disk.OwnerReferences[0].BlockOwnerDeletion)
	}

	assertHealthInvariant(t, disk)
	if degraded := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDegraded); degraded.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %q; want False for a healthy disk", degraded.Status)
	}
	collected := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDataCollected)
	if collected == nil || collected.Status != metav1.ConditionTrue || collected.Reason != v1alpha1.ReasonSucceeded {
		t.Errorf("unexpected DataCollected condition: %#v", collected)
	}
}

func TestReconcileAllExcludesDiskAndDeletesExistingResource(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})
	if err := monitor.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("initial ReconcileAll failed: %v", err)
	}

	_ = singleDisk(t, c)
	if count := testutil.CollectAndCount(metrics.CollectionSuccess); count < 1 {
		t.Errorf("expected CollectionSuccess metric to be recorded, got count %d", count)
	}

	monitor.Options.ExcludeRules = []discovery.ExcludeRule{{WWN: "0x50014ee265882b7f"}}
	if err := monitor.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("excluded ReconcileAll failed: %v", err)
	}

	var disks v1alpha1.PhysicalDiskList
	if err := c.List(context.Background(), &disks); err != nil {
		t.Fatalf("list disks: %v", err)
	}
	if len(disks.Items) != 0 {
		t.Fatalf("got %d disks; want excluded disk deleted", len(disks.Items))
	}

	if count := testutil.CollectAndCount(metrics.CollectionSuccess); count != 0 {
		t.Errorf("expected CollectionSuccess metric to be deleted for excluded disk, got count %d", count)
	}

	// Removing the exclude rule should discover the disk again.
	monitor.Options.ExcludeRules = nil
	if err := monitor.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("un-excluded ReconcileAll failed: %v", err)
	}
	_ = singleDisk(t, c)
}

func TestReconcileAllDegradedDisk(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: failingATADisk()})

	if err := monitor.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	if disk.Status.Health != v1alpha1.StatusSelfAssessmentFailed {
		t.Errorf("health = %q; want SelfAssessmentFailed", disk.Status.Health)
	}
	assertHealthInvariant(t, disk)
	if degraded := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDegraded); degraded.Status != metav1.ConditionTrue {
		t.Errorf("Degraded = %q; want True", degraded.Status)
	}
}

// A collection failure must be reported through DataCollected without
// pretending that fresh data was obtained.
func TestCollectionFailureKeepsTimestampUnset(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"standby", smartmontools.ErrDeviceInStandby, v1alpha1.ReasonSkippedStandby},
		{"missing device", smartmontools.ErrDeviceNotFound, v1alpha1.ReasonDiskMissing},
		{"smart unsupported", smartmontools.ErrSmartUnsupported, v1alpha1.ReasonSmartUnsupported},
		{"generic failure", errors.New("boom"), v1alpha1.ReasonFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{err: tt.err})

			if err := monitor.ReconcileAll(context.Background()); err != nil {
				t.Fatalf("ReconcileAll failed: %v", err)
			}

			disk := singleDisk(t, c)
			if disk.Status.LastDataCollectedTime != nil {
				t.Error("lastDataCollectedTime must stay unset when collection fails")
			}

			collected := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDataCollected)
			if collected == nil || collected.Status != metav1.ConditionFalse {
				t.Fatalf("unexpected DataCollected condition: %#v", collected)
			}
			if collected.Reason != tt.wantReason {
				t.Errorf("reason = %q; want %q", collected.Reason, tt.wantReason)
			}

			// The hardware was never assessed, so the health is Unknown
			// rather than silently absent.
			if disk.Status.Health != v1alpha1.StatusUnknown {
				t.Errorf("health = %q; want Unknown", disk.Status.Health)
			}
			assertHealthInvariant(t, disk)
		})
	}
}

// A transient collection error must not discard the last known health.
func TestCollectionFailurePreservesKnownHealth(t *testing.T) {
	runner := &mockSmartctlRunner{output: healthyATADisk()}
	monitor, c, _ := newTestMonitor(t, runner)
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("initial ReconcileAll failed: %v", err)
	}
	collectedAt := singleDisk(t, c).Status.LastDataCollectedTime
	if collectedAt == nil {
		t.Fatal("expected lastDataCollectedTime after the first success")
	}

	runner.output, runner.err = nil, errors.New("smartctl unavailable")
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("second ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	if disk.Status.Health != v1alpha1.StatusGood {
		t.Errorf("health = %q; want the previous Good to be preserved", disk.Status.Health)
	}
	if !disk.Status.LastDataCollectedTime.Equal(collectedAt) {
		t.Errorf("lastDataCollectedTime = %v; want it unchanged at %v",
			disk.Status.LastDataCollectedTime, collectedAt)
	}
	collected := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDataCollected)
	if collected.Status != metav1.ConditionFalse || collected.Reason != v1alpha1.ReasonFailed {
		t.Errorf("unexpected DataCollected condition: %#v", collected)
	}
}

// Repeated polls of an unchanged disk must not produce duplicate events.
func TestEventsAreOnlyEmittedOnStateChange(t *testing.T) {
	runner := &mockSmartctlRunner{output: failingATADisk()}
	monitor, _, recorder := newTestMonitor(t, runner)
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("first ReconcileAll failed: %v", err)
	}
	if got := len(recorder.Events); got != 1 {
		t.Fatalf("got %d events after the first reconcile; want 1", got)
	}
	<-recorder.Events

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("second ReconcileAll failed: %v", err)
	}
	if got := len(recorder.Events); got != 0 {
		t.Fatalf("got %d events for an unchanged disk; want 0", got)
	}

	// Recovering to a healthy state is a transition and must be reported.
	runner.output = healthyATADisk()
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("third ReconcileAll failed: %v", err)
	}
	if got := len(recorder.Events); got != 1 {
		t.Fatalf("got %d events after recovery; want 1", got)
	}
}

func TestHandleDeviceRemovalMarksDiskMissing(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll failed: %v", err)
	}

	monitor.handleDeviceRemoval(ctx, &discovery.UEvent{
		Action:    discovery.ActionRemove,
		Subsystem: "block",
		DevName:   "/dev/sda",
		DevPath:   "/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda",
		DevType:   "disk",
	})

	disk := singleDisk(t, c)
	collected := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDataCollected)
	if collected == nil || collected.Status != metav1.ConditionFalse {
		t.Fatalf("unexpected DataCollected condition: %#v", collected)
	}
	if collected.Reason != v1alpha1.ReasonDiskMissing {
		t.Errorf("reason = %q; want %q", collected.Reason, v1alpha1.ReasonDiskMissing)
	}
}

func TestReconcileAllMarksAbsentDiskMissing(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("initial ReconcileAll failed: %v", err)
	}

	if err := os.Remove(filepath.Join(monitor.Options.UdevDataDir, "b8:0")); err != nil {
		t.Fatalf("failed to remove udev record: %v", err)
	}

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("second ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	collected := meta.FindStatusCondition(disk.Status.Conditions, v1alpha1.ConditionDataCollected)
	if collected == nil || collected.Status != metav1.ConditionFalse || collected.Reason != v1alpha1.ReasonDiskMissing {
		t.Errorf("unexpected DataCollected condition: %#v", collected)
	}
}

func TestDiskMonitorStartSetsReady(t *testing.T) {
	monitor, _, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})
	if monitor.Ready() {
		t.Fatal("expected monitor.Ready() to be false initially")
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		_ = monitor.Start(ctx)
	}()

	for i := 0; i < 50; i++ {
		if monitor.Ready() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !monitor.Ready() {
		t.Error("expected monitor.Ready() to become true after Start initial scan")
	}

	cancel()
	<-startDone
	if monitor.Ready() {
		t.Error("expected monitor.Ready() to be false after Start returns")
	}
}

func TestNewDiskMonitorAppliesSafeDefaults(t *testing.T) {
	monitor := NewDiskMonitor(nil, nil, nil, MonitorOptions{NodeName: testNodeName})

	// A zero interval would make time.NewTicker panic at runtime.
	if monitor.Options.PollInterval != defaultPollInterval {
		t.Errorf("pollInterval = %s; want %s", monitor.Options.PollInterval, defaultPollInterval)
	}
	if monitor.Options.EventDebounce != defaultEventDebounce {
		t.Errorf("eventDebounce = %s; want %s", monitor.Options.EventDebounce, defaultEventDebounce)
	}
}

// Any custom smartctl extraCmdArgs configured in spec must be passed to the runner.
func TestReconcilePassesSpecExtraCmdArgsToRunner(t *testing.T) {
	var capturedArgs string
	capturingRunner := &argCapturingRunner{
		onCollect: func(args string) {
			capturedArgs = args
		},
	}

	monitor, c, _ := newTestMonitor(t, capturingRunner)
	ctx := context.Background()

	// First reconcile creates the PhysicalDisk object.
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("initial ReconcileAll failed: %v", err)
	}
	if capturedArgs != "" {
		t.Errorf("expected no extraCmdArgs on initial creation, got %q", capturedArgs)
	}

	// User patches spec.smartmontools.smartctl.extraCmdArgs on the PhysicalDisk.
	disk := singleDisk(t, c)
	disk.Spec.Smartmontools = &v1alpha1.SmartmontoolsSpec{
		Smartctl: &v1alpha1.SmartctlSpec{
			ExtraCmdArgs: "-d sat -T permissive",
		},
	}
	if err := c.Update(ctx, &disk); err != nil {
		t.Fatalf("failed to update disk spec: %v", err)
	}

	// Subsequent reconcile must pass the configured extraCmdArgs to the runner.
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("second ReconcileAll failed: %v", err)
	}

	if capturedArgs != "-d sat -T permissive" {
		t.Errorf("capturedArgs = %q; want %q", capturedArgs, "-d sat -T permissive")
	}
}

type argCapturingRunner struct {
	onCollect func(args string)
}

func (a *argCapturingRunner) Collect(_ context.Context, _ string, extraCmdArgs string) (*smartmontools.SmartctlOutput, error) {
	if a.onCollect != nil {
		a.onCollect(extraCmdArgs)
	}
	return healthyATADisk(), nil
}

func TestReconcilePopulatesTelemetryAndPreservesOnFailure(t *testing.T) {
	smartData := &smartmontools.SmartctlOutput{
		SmartStatus:     smartmontools.SmartStatus{Passed: true},
		Temperature:     &smartmontools.DeviceTemperature{Current: 34},
		PowerOnTime:     &smartmontools.PowerOnTime{Hours: 1200},
		PowerCycleCount: new(int64(45)),
		AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
			Table: []smartmontools.AtaAttribute{
				{ID: 5, Name: "Reallocated_Sector_Ct", Raw: smartmontools.AtaAttributeRaw{Value: 3}},
				{ID: 197, Name: "Current_Pending_Sector", Raw: smartmontools.AtaAttributeRaw{Value: 1}},
			},
		},
	}

	runner := &mockSmartctlRunner{output: smartData}
	monitor, c, _ := newTestMonitor(t, runner)
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	if disk.Status.Telemetry == nil {
		t.Fatal("expected status.telemetry to be populated")
	}
	if disk.Status.Telemetry.TemperatureCelsius == nil || *disk.Status.Telemetry.TemperatureCelsius != 34 {
		t.Errorf("telemetry.temperatureCelsius = %v; want 34", disk.Status.Telemetry.TemperatureCelsius)
	}
	if disk.Status.Telemetry.PowerOnHours == nil || *disk.Status.Telemetry.PowerOnHours != 1200 {
		t.Errorf("telemetry.powerOnHours = %v; want 1200", disk.Status.Telemetry.PowerOnHours)
	}
	if disk.Status.Telemetry.PowerCycleCount == nil || *disk.Status.Telemetry.PowerCycleCount != 45 {
		t.Errorf("telemetry.powerCycleCount = %v; want 45", disk.Status.Telemetry.PowerCycleCount)
	}
	if disk.Status.Telemetry.ReallocatedSectors == nil || *disk.Status.Telemetry.ReallocatedSectors != 3 {
		t.Errorf("telemetry.reallocatedSectors = %v; want 3", disk.Status.Telemetry.ReallocatedSectors)
	}
	if disk.Status.Telemetry.PendingSectors == nil || *disk.Status.Telemetry.PendingSectors != 1 {
		t.Errorf("telemetry.pendingSectors = %v; want 1", disk.Status.Telemetry.PendingSectors)
	}

	// Next poll fails (transient error) - telemetry must be preserved.
	runner.output, runner.err = nil, errors.New("timeout")
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("second ReconcileAll failed: %v", err)
	}

	diskAfterFailure := singleDisk(t, c)
	if diskAfterFailure.Status.Telemetry == nil {
		t.Fatal("expected telemetry to be preserved across failure")
	}
	if *diskAfterFailure.Status.Telemetry.TemperatureCelsius != 34 {
		t.Errorf("preserved temperature = %d; want 34", *diskAfterFailure.Status.Telemetry.TemperatureCelsius)
	}
}

// TestReconcileTriggeredOnSpecChange verifies that DiskMonitor.Reconcile handles
// PhysicalDisk spec changes (e.g. extraCmdArgs) directly via the controller-runtime interface.
func TestReconcileTriggeredOnSpecChange(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register api scheme: %v", err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testNodeName, UID: "node-uid-12345"}}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(node).
		WithStatusSubresource(&v1alpha1.PhysicalDisk{}).
		Build()

	udevDir := t.TempDir()
	udevRecord := `S:disk/by-id/ata-WDC_WD10EZEX-08WN4A0
S:disk/by-id/wwn-0x50014ee265882b7f
E:DEVNAME=/dev/sda
E:DEVTYPE=disk
E:ID_BUS=ata
E:ID_MODEL=WDC_WD10EZEX-08WN4A0
E:ID_SERIAL=WDC_WD10EZEX-08WN4A0_WD-WCC6Y7PL7345
E:ID_WWN=0x50014ee265882b7f
E:MAJOR=8
E:MINOR=0
`
	if err := os.WriteFile(filepath.Join(udevDir, "b8:0"), []byte(udevRecord), 0o644); err != nil {
		t.Fatalf("failed to write udev record: %v", err)
	}

	var capturedArgs string
	runner := &argCapturingRunner{
		onCollect: func(args string) {
			capturedArgs = args
		},
	}

	monitor := NewDiskMonitor(fakeClient, record.NewFakeRecorder(10), runner, MonitorOptions{
		NodeName:    testNodeName,
		UdevDataDir: udevDir,
	})

	ctx := context.Background()

	// Initial reconcile creates the PhysicalDisk.
	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, fakeClient)
	disk.Spec.Smartmontools = &v1alpha1.SmartmontoolsSpec{
		Smartctl: &v1alpha1.SmartctlSpec{
			ExtraCmdArgs: "-d sat -T permissive",
		},
	}
	if err := fakeClient.Update(ctx, &disk); err != nil {
		t.Fatalf("failed to update disk spec: %v", err)
	}

	// Trigger Reconcile via controller-runtime request.
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: disk.Name}}
	res, err := monitor.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Errorf("unexpected requeue: %#v", res)
	}

	if capturedArgs != "-d sat -T permissive" {
		t.Errorf("capturedArgs = %q; want %q", capturedArgs, "-d sat -T permissive")
	}
}

func TestReconcileDeletesExcludedDisk(t *testing.T) {
	monitor, c, _ := newTestMonitor(t, &mockSmartctlRunner{output: healthyATADisk()})
	ctx := context.Background()

	if err := monitor.ReconcileAll(ctx); err != nil {
		t.Fatalf("initial ReconcileAll failed: %v", err)
	}

	disk := singleDisk(t, c)
	monitor.Options.ExcludeRules = []discovery.ExcludeRule{{WWN: disk.Status.Info.WWN}}

	result, err := monitor.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: disk.Name}})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("unexpected requeue: %#v", result)
	}

	var remaining v1alpha1.PhysicalDisk
	if err := c.Get(ctx, client.ObjectKey{Name: disk.Name}, &remaining); !apierrors.IsNotFound(err) {
		t.Errorf("excluded disk still exists or lookup failed: %v", err)
	}
}

func TestExtractTelemetry_NVMeAndFallback(t *testing.T) {
	// 1. Full NVMe output.
	tempNVMe := 41
	used := 12
	spare := 98
	crit := 0
	mediaErr := int64(0)
	hours := int64(450)
	cycles := int64(20)

	nvmeOutput := &smartmontools.SmartctlOutput{
		PowerOnTime:     &smartmontools.PowerOnTime{Hours: hours},
		PowerCycleCount: &cycles,
		NvmeSmart: &smartmontools.NvmeSmartHealth{
			Temperature:     tempNVMe,
			PercentageUsed:  used,
			AvailableSpare:  spare,
			CriticalWarning: crit,
			MediaErrors:     mediaErr,
		},
	}

	telem := extractTelemetry(nvmeOutput)
	if telem == nil {
		t.Fatal("expected telemetry, got nil")
	}
	if *telem.TemperatureCelsius != 41 {
		t.Errorf("Temperature = %d, want 41", *telem.TemperatureCelsius)
	}
	if *telem.PercentageUsed != 12 {
		t.Errorf("PercentageUsed = %d, want 12", *telem.PercentageUsed)
	}
	if *telem.AvailableSpare != 98 {
		t.Errorf("AvailableSpare = %d, want 98", *telem.AvailableSpare)
	}
	if *telem.CriticalWarning != 0 {
		t.Errorf("CriticalWarning = %d, want 0", *telem.CriticalWarning)
	}
	if *telem.MediaErrors != 0 {
		t.Errorf("MediaErrors = %d, want 0", *telem.MediaErrors)
	}

	// 2. ATA fallback for temperature (attr 194), power on hours (attr 9), power cycles (attr 12).
	ataFallbackOutput := &smartmontools.SmartctlOutput{
		AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
			Table: []smartmontools.AtaAttribute{
				{ID: 194, Name: "Temperature_Celsius", Raw: smartmontools.AtaAttributeRaw{Value: 35}},
				{ID: 9, Name: "Power_On_Hours", Raw: smartmontools.AtaAttributeRaw{Value: 1234}},
				{ID: 12, Name: "Power_Cycle_Count", Raw: smartmontools.AtaAttributeRaw{Value: 56}},
			},
		},
	}

	telem2 := extractTelemetry(ataFallbackOutput)
	if telem2 == nil {
		t.Fatal("expected telemetry, got nil")
	}
	if *telem2.TemperatureCelsius != 35 {
		t.Errorf("Temperature = %d, want 35", *telem2.TemperatureCelsius)
	}
	if *telem2.PowerOnHours != 1234 {
		t.Errorf("PowerOnHours = %d, want 1234", *telem2.PowerOnHours)
	}
	if *telem2.PowerCycleCount != 56 {
		t.Errorf("PowerCycleCount = %d, want 56", *telem2.PowerCycleCount)
	}

	// 3. Nil input returns nil.
	if extractTelemetry(nil) != nil {
		t.Errorf("expected nil telemetry for nil output")
	}
}

func TestReconcile_NotFoundAndForeignNode(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	monitor := NewDiskMonitor(fakeClient, record.NewFakeRecorder(10), &mockSmartctlRunner{}, MonitorOptions{
		NodeName: testNodeName,
	})

	// 1. NotFound disk returns no error.
	res, err := monitor.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "non-existent"}})
	if err != nil {
		t.Errorf("Reconcile on not-found disk returned error: %v", err)
	}
	if res.Requeue {
		t.Errorf("unexpected requeue")
	}

	// 2. Disk on another node is skipped.
	foreignDisk := &v1alpha1.PhysicalDisk{
		ObjectMeta: metav1.ObjectMeta{Name: "other-node-disk"},
		Spec:       v1alpha1.PhysicalDiskSpec{NodeName: "other-node"},
	}
	if err := fakeClient.Create(ctx, foreignDisk); err != nil {
		t.Fatalf("failed to create foreign disk: %v", err)
	}

	res, err = monitor.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "other-node-disk"}})
	if err != nil {
		t.Errorf("Reconcile on foreign disk returned error: %v", err)
	}
	if res.Requeue {
		t.Errorf("unexpected requeue")
	}
}

func TestDeleteIfExcluded(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	ctx := context.Background()

	disk := &v1alpha1.PhysicalDisk{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node-sda"},
		Spec:       v1alpha1.PhysicalDiskSpec{NodeName: testNodeName},
		Status: v1alpha1.PhysicalDiskStatus{
			Info: v1alpha1.DiskInfo{
				Name:   "sda",
				Vendor: "HP",
				Model:  "LOGICAL_VOLUME",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(disk).Build()
	monitor := NewDiskMonitor(fakeClient, record.NewFakeRecorder(10), &mockSmartctlRunner{}, MonitorOptions{
		NodeName: testNodeName,
		ExcludeRules: []discovery.ExcludeRule{
			{Vendor: "HP", Model: "LOGICAL_VOLUME"},
		},
	})

	// 1. Non-excluded disk: returns (false, nil) and does not delete.
	nonExcludedDisk := &v1alpha1.PhysicalDisk{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node-sdb"},
		Spec:       v1alpha1.PhysicalDiskSpec{NodeName: testNodeName},
		Status: v1alpha1.PhysicalDiskStatus{
			Info: v1alpha1.DiskInfo{
				Name:   "sdb",
				Vendor: "Samsung",
			},
		},
	}
	deleted, err := monitor.deleteIfExcluded(ctx, nonExcludedDisk)
	if err != nil {
		t.Fatalf("deleteIfExcluded unexpected error: %v", err)
	}
	if deleted {
		t.Errorf("expected deleted = false for non-matching disk")
	}

	// 2. Excluded disk: returns (true, nil) and deletes the resource.
	deleted, err = monitor.deleteIfExcluded(ctx, disk)
	if err != nil {
		t.Fatalf("deleteIfExcluded error on matching disk: %v", err)
	}
	if !deleted {
		t.Errorf("expected deleted = true for matching disk")
	}

	var remaining v1alpha1.PhysicalDisk
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "test-node-sda"}, &remaining); err == nil {
		t.Errorf("expected excluded disk to be deleted from client")
	}
}

func TestMarkDiskMissing_Idempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	ctx := context.Background()

	disk := &v1alpha1.PhysicalDisk{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node-sdc"},
		Spec:       v1alpha1.PhysicalDiskSpec{NodeName: testNodeName},
		Status: v1alpha1.PhysicalDiskStatus{
			Info: v1alpha1.DiskInfo{Name: "sdc", Path: "/dev/sdc"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(disk).
		WithStatusSubresource(&v1alpha1.PhysicalDisk{}).
		Build()

	recorder := record.NewFakeRecorder(10)
	monitor := NewDiskMonitor(fakeClient, recorder, &mockSmartctlRunner{}, MonitorOptions{
		NodeName: testNodeName,
	})

	// First call marks the disk missing and records an event.
	if err := monitor.markDiskMissing(ctx, disk, "Device removed"); err != nil {
		t.Fatalf("markDiskMissing first call failed: %v", err)
	}
	select {
	case event := <-recorder.Events:
		if event == "" {
			t.Errorf("expected an event on first missing mark")
		}
	default:
		t.Errorf("expected event channel to have an event")
	}

	// Second call with same state must be a no-op (idempotent, no duplicate event).
	if err := monitor.markDiskMissing(ctx, disk, "Device removed"); err != nil {
		t.Fatalf("markDiskMissing second call failed: %v", err)
	}
	select {
	case event := <-recorder.Events:
		t.Errorf("unexpected duplicate event on second markDiskMissing: %s", event)
	default:
		// Expected: no event emitted.
	}
}
