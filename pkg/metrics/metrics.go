// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// Device exposes smartctl_exporter-compatible hardware metadata for PromQL joins.
	Device = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Name:      "device",
			Help:      "Device information (always 1.0)",
		},
		[]string{"node", "disk", "device", "interface", "protocol", "model_family", "model_name", "serial_number", "ata_additional_product_id", "firmware_version", "ata_version", "sata_version", "form_factor", "scsi_vendor", "scsi_product", "scsi_revision", "scsi_version"},
	)

	// Version exposes smartctl version metadata expected by common smartctl_exporter dashboards.
	Version = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Name:      "version",
			Help:      "smartctl version",
		},
		[]string{"node", "json_format_version", "smartctl_version", "svn_revision", "build_info"},
	)

	// SmartStatus exposes the overall-health self-assessment test result (1 = passed, 0 = failed).
	SmartStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "smart_status",
			Help:      "General SMART status (1 = passed, 0 = failed)",
		},
		[]string{"node", "disk", "device"},
	)

	// Temperature exposes device temperature in Celsius.
	Temperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "temperature",
			Help:      "Device temperature in Celsius",
		},
		[]string{"node", "disk", "device", "temperature_type"},
	)

	// PowerOnSeconds exposes device power-on time in seconds.
	PowerOnSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "power_on_seconds",
			Help:      "Device power on time in seconds",
		},
		[]string{"node", "disk", "device"},
	)

	// PowerCycleCount exposes the number of power on/off cycles.
	PowerCycleCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "power_cycle_count",
			Help:      "Device power cycle count",
		},
		[]string{"node", "disk", "device"},
	)

	// PercentageUsed exposes NVMe subsystem life used percentage (0 to 100+).
	PercentageUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "percentage_used",
			Help:      "Device write percentage used (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// AvailableSpare exposes NVMe remaining spare capacity percentage.
	AvailableSpare = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "available_spare",
			Help:      "Remaining spare capacity percentage (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// AvailableSpareThreshold exposes NVMe spare capacity threshold percentage.
	AvailableSpareThreshold = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "available_spare_threshold",
			Help:      "Available spare threshold percentage (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// CriticalWarning exposes NVMe critical warning bitmask.
	CriticalWarning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "critical_warning",
			Help:      "Critical warning bitmask for the controller (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// MediaErrors exposes NVMe unrecovered data integrity errors.
	MediaErrors = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "media_errors",
			Help:      "Number of unrecovered data integrity errors (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// NumErrLogEntries exposes NVMe error information log count.
	NumErrLogEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "num_err_log_entries",
			Help:      "Number of error log entries (NVMe)",
		},
		[]string{"node", "disk", "device"},
	)

	// Attribute exposes individual ATA SMART attributes compatible with smartctl_exporter.
	Attribute = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "attribute",
			Help:      "Device SMART attributes (ATA)",
		},
		[]string{"node", "disk", "device", "attribute_name", "attribute_id", "attribute_value_type"},
	)

	// HealthStatus metric exposes evaluated disk health status (1 = active status, 0 otherwise).
	HealthStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "health_status",
			Help:      "Evaluated health status of a monitored physical disk (1 = active status)",
		},
		[]string{"node", "disk", "device", "status"},
	)

	// CollectionSuccess records whether the last poll attempt succeeded.
	CollectionSuccess = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "smartctl",
			Subsystem: "device",
			Name:      "collection_success",
			Help:      "Whether the last SMART data collection succeeded (1 = yes, 0 = no)",
		},
		[]string{"node", "disk", "device"},
	)
)

func init() {
	prometheus.MustRegister(
		Device,
		Version,
		SmartStatus,
		Temperature,
		PowerOnSeconds,
		PowerCycleCount,
		PercentageUsed,
		AvailableSpare,
		AvailableSpareThreshold,
		CriticalWarning,
		MediaErrors,
		NumErrLogEntries,
		Attribute,
		HealthStatus,
		CollectionSuccess,
	)
}

// SmartctlVersion formats the smartctl JSON version array for metric labels.
func SmartctlVersion(version []int) string {
	parts := make([]string, len(version))
	for i, value := range version {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ".")
}

// DeleteDeviceHealthMetrics drops all hardware readings of a disk that is no
// longer present. Without this the last known values would be exported
// indefinitely. CollectionSuccess is intentionally preserved so alerts can
// continue to detect failure/missing status.
func DeleteDeviceHealthMetrics(node, disk string) {
	labels := prometheus.Labels{"node": node, "disk": disk}
	Device.DeletePartialMatch(labels)
	SmartStatus.DeletePartialMatch(labels)
	Temperature.DeletePartialMatch(labels)
	PowerOnSeconds.DeletePartialMatch(labels)
	PowerCycleCount.DeletePartialMatch(labels)
	PercentageUsed.DeletePartialMatch(labels)
	AvailableSpare.DeletePartialMatch(labels)
	AvailableSpareThreshold.DeletePartialMatch(labels)
	CriticalWarning.DeletePartialMatch(labels)
	MediaErrors.DeletePartialMatch(labels)
	NumErrLogEntries.DeletePartialMatch(labels)
	Attribute.DeletePartialMatch(labels)
	HealthStatus.DeletePartialMatch(labels)
}

// DeleteAllDiskMetrics drops all metrics for a disk, including CollectionSuccess.
// Used when a disk is explicitly excluded from monitoring.
func DeleteAllDiskMetrics(node, disk string) {
	DeleteDeviceHealthMetrics(node, disk)
	CollectionSuccess.DeletePartialMatch(prometheus.Labels{"node": node, "disk": disk})
}
