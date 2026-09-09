// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DiskHealthStatus represents the evaluated health status of a disk.
// +kubebuilder:validation:Enum=Good;SectorErrors;ExcessiveSectorErrors;AttributeFailingNow;AttributeFailedInPast;SelfAssessmentFailed;Unknown
type DiskHealthStatus string

const (
	StatusGood                  DiskHealthStatus = "Good"
	StatusSectorErrors          DiskHealthStatus = "SectorErrors"
	StatusExcessiveSectorErrors DiskHealthStatus = "ExcessiveSectorErrors"
	StatusAttributeFailingNow   DiskHealthStatus = "AttributeFailingNow"
	StatusAttributeFailedInPast DiskHealthStatus = "AttributeFailedInPast"
	StatusSelfAssessmentFailed  DiskHealthStatus = "SelfAssessmentFailed"
	StatusUnknown               DiskHealthStatus = "Unknown"
)

// AllDiskHealthStatuses lists every health status a disk can report. It lets
// consumers such as the metrics exporter emit a complete, gap-free series.
var AllDiskHealthStatuses = []DiskHealthStatus{
	StatusGood,
	StatusSectorErrors,
	StatusExcessiveSectorErrors,
	StatusAttributeFailingNow,
	StatusAttributeFailedInPast,
	StatusSelfAssessmentFailed,
	StatusUnknown,
}

// Condition types for PhysicalDisk.
const (
	// ConditionDegraded indicates the health state of the disk.
	ConditionDegraded string = "Degraded"
	// ConditionDataCollected indicates whether data collection succeeded or failed.
	ConditionDataCollected string = "DataCollected"
)

// Reasons for ConditionDataCollected.
const (
	ReasonSucceeded        = "Succeeded"
	ReasonFailed           = "Failed"
	ReasonSkippedStandby   = "SkippedStandby"
	ReasonDiskMissing      = "DiskMissing"
	ReasonSmartUnsupported = "SmartUnsupported"
)

// SmartctlSpec contains smartctl-specific tuning for this disk.
type SmartctlSpec struct {
	// ExtraCmdArgs specifies additional smartctl command-line arguments
	// (e.g. "-d sat -T permissive").
	// +optional
	// +kubebuilder:validation:MaxLength=256
	ExtraCmdArgs string `json:"extraCmdArgs,omitempty"`
}

// SmartmontoolsSpec contains configuration for the Smartmontools suite.
type SmartmontoolsSpec struct {
	// Smartctl configures the smartctl collector.
	// +optional
	Smartctl *SmartctlSpec `json:"smartctl,omitempty"`
}

// PhysicalDiskSpec defines the desired state of PhysicalDisk.
type PhysicalDiskSpec struct {
	// NodeName is the name of the Kubernetes Node where this disk resides.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="nodeName is immutable once set"
	NodeName string `json:"nodeName"`

	// Smartmontools defines optional Smartmontools collector configuration.
	// +optional
	Smartmontools *SmartmontoolsSpec `json:"smartmontools,omitempty"`
}

// DiskInfo contains static and udev-reported hardware metadata.
type DiskInfo struct {
	// Path is the primary canonical or persistent device path (e.g. /dev/disk/by-id/wwn-...).
	Path string `json:"path,omitempty"`
	// CanonicalPath is the kernel device path resolved from udev (e.g. /dev/sda, /dev/nvme0n1).
	CanonicalPath string `json:"canonicalPath,omitempty"`
	// Name is the kernel device name (e.g. sda, nvme0n1).
	Name string `json:"name,omitempty"`
	// SysPath is the sysfs path (e.g. /sys/devices/...).
	SysPath string `json:"sysPath,omitempty"`
	// Links lists all symlinks pointing to this device (e.g. by-id, by-path).
	Links []string `json:"links,omitempty"`
	// Major device number.
	Major int `json:"major,omitempty"`
	// Minor device number.
	Minor int `json:"minor,omitempty"`
	// Type of device (e.g. disk).
	Type string `json:"type,omitempty"`
	// Bus subsystem or transport (e.g. ata, scsi, nvme, usb).
	Bus string `json:"bus,omitempty"`
	// Model identifier reported by hardware or udev.
	Model string `json:"model,omitempty"`
	// Vendor identifier.
	Vendor string `json:"vendor,omitempty"`
	// Serial number of the device.
	Serial string `json:"serial,omitempty"`
	// SerialShort is the shortened serial number from udev.
	SerialShort string `json:"serialShort,omitempty"`
	// WWN is the World Wide Name identifier.
	WWN string `json:"wwn,omitempty"`
	// BusPath is the hardware bus topology path.
	BusPath string `json:"busPath,omitempty"`
	// Capacity is the total disk size in bytes.
	Capacity int64 `json:"capacity,omitempty"`
	// Rotational indicates whether the disk is a rotational medium (HDD) or non-rotational (SSD/NVMe).
	// +optional
	Rotational *bool `json:"rotational,omitempty"`
	// FirmwareVersion is the firmware revision of the drive.
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
}

// DiskTelemetry contains the latest observed operational snapshot of the disk.
type DiskTelemetry struct {
	// TemperatureCelsius is the current temperature of the device in Celsius.
	// +optional
	TemperatureCelsius *int `json:"temperatureCelsius,omitempty"`
	// PowerOnHours is the lifetime power-on hours of the device.
	// +optional
	PowerOnHours *int64 `json:"powerOnHours,omitempty"`
	// PowerCycleCount is the number of power cycles.
	// +optional
	PowerCycleCount *int64 `json:"powerCycleCount,omitempty"`
	// ReallocatedSectors is the number of reallocated sectors (ATA 5).
	// +optional
	ReallocatedSectors *int64 `json:"reallocatedSectors,omitempty"`
	// PendingSectors is the number of pending sectors (ATA 197).
	// +optional
	PendingSectors *int64 `json:"pendingSectors,omitempty"`
	// PercentageUsed is the percentage of NVM subsystem life used (NVMe).
	// +optional
	PercentageUsed *int `json:"percentageUsed,omitempty"`
	// AvailableSpare is the percentage of remaining spare capacity (NVMe).
	// +optional
	AvailableSpare *int `json:"availableSpare,omitempty"`
	// CriticalWarning is the NVMe critical warning bitmask.
	// +optional
	CriticalWarning *int `json:"criticalWarning,omitempty"`
	// MediaErrors is the number of unrecovered data integrity errors (NVMe).
	// +optional
	MediaErrors *int64 `json:"mediaErrors,omitempty"`
}

// Finding represents a specific SMART attribute violation or diagnostic observation.
type Finding struct {
	// ID is an internal identifier for this finding.
	ID string `json:"id"`
	// AttributeID is the numeric SMART attribute ID (if ATA).
	AttributeID *int `json:"attributeId,omitempty"`
	// AttributeName is the human-readable attribute or metric name.
	AttributeName string `json:"attributeName,omitempty"`
	// RawValue is the raw value recorded for this attribute.
	RawValue int64 `json:"rawValue,omitempty"`
	// Message provides a diagnostic explanation of the finding.
	Message string `json:"message"`
}

// PhysicalDiskStatus defines the observed state of PhysicalDisk.
type PhysicalDiskStatus struct {
	// Health reflects the evaluated health status (e.g. Good, ExcessiveSectorErrors).
	// Invariant: health == conditions[Degraded].reason
	// +kubebuilder:validation:Enum=Good;SectorErrors;ExcessiveSectorErrors;AttributeFailingNow;AttributeFailedInPast;SelfAssessmentFailed;Unknown
	Health DiskHealthStatus `json:"health,omitempty"`

	// LastDataCollectedTime is the timestamp of the last successful data collection.
	LastDataCollectedTime *metav1.Time `json:"lastDataCollectedTime,omitempty"`

	// Conditions represents the latest available observations of a disk's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Info contains static and udev-reported hardware metadata.
	Info DiskInfo `json:"info,omitempty"`

	// Telemetry contains the latest observed operational snapshot of the disk.
	// +optional
	Telemetry *DiskTelemetry `json:"telemetry,omitempty"`

	// Findings lists any active diagnostic findings, errors, or threshold breaches.
	Findings []Finding `json:"findings,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pd
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Collection",type=string,JSONPath=`.status.conditions[?(@.type=="DataCollected")].reason`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.info.model`
// +kubebuilder:printcolumn:name="Rotational",type=boolean,JSONPath=`.status.info.rotational`
// +kubebuilder:printcolumn:name="Capacity",type=integer,JSONPath=`.status.info.capacity`
// +kubebuilder:printcolumn:name="Temp",type=integer,JSONPath=`.status.telemetry.temperatureCelsius`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PhysicalDisk is the Schema for the physicaldisks API.
type PhysicalDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalDiskSpec   `json:"spec,omitempty"`
	Status PhysicalDiskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PhysicalDiskList contains a list of PhysicalDisk.
type PhysicalDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalDisk `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PhysicalDisk{}, &PhysicalDiskList{})
}
