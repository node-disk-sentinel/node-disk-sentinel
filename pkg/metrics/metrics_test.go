// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRegistrationAndDeletion(t *testing.T) {
	node := "node-test"
	disk := "node-test-sda"
	device := "/dev/sda"

	Device.WithLabelValues(node, disk, device, "ata", "ATA", "FamilyZ", "ModelX", "SerialY", "", "", "", "", "", "", "", "", "").Set(1.0)
	Version.WithLabelValues(node, "1", "7.4", "", "").Set(1.0)
	SmartStatus.WithLabelValues(node, disk, device).Set(1.0)
	Temperature.WithLabelValues(node, disk, device, "current").Set(38.0)
	PowerOnSeconds.WithLabelValues(node, disk, device).Set(7200.0)
	PowerCycleCount.WithLabelValues(node, disk, device).Set(12.0)
	HealthStatus.WithLabelValues(node, disk, device, "Good").Set(1.0)
	CollectionSuccess.WithLabelValues(node, disk, device).Set(1.0)

	if count := testutil.CollectAndCount(Device); count < 1 {
		t.Errorf("expected Device metric to be recorded, got count %d", count)
	}
	if count := testutil.CollectAndCount(Temperature); count < 1 {
		t.Errorf("expected Temperature metric to be recorded, got count %d", count)
	}

	// Delete device health metrics on device removal.
	DeleteDeviceHealthMetrics(node, disk)

	// Device, Temperature, and HealthStatus should be deleted.
	if count := testutil.CollectAndCount(Temperature); count != 0 {
		t.Errorf("expected Temperature metric to be deleted, got count %d", count)
	}

	// CollectionSuccess must be preserved.
	if count := testutil.CollectAndCount(CollectionSuccess); count < 1 {
		t.Errorf("expected CollectionSuccess metric to be preserved, got count %d", count)
	}

	// Delete all disk metrics on explicit disk exclusion.
	DeleteAllDiskMetrics(node, disk)
	if count := testutil.CollectAndCount(CollectionSuccess); count != 0 {
		t.Errorf("expected CollectionSuccess metric to be deleted by DeleteAllDiskMetrics, got count %d", count)
	}
}
