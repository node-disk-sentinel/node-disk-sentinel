// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package smartmontools

import "strings"

// SmartctlOutput represents the top-level JSON emitted by smartctl --json --all.
type SmartctlOutput struct {
	Smartctl           SmartctlInfo        `json:"smartctl"`
	Device             DeviceHeader        `json:"device"`
	ModelFamily        string              `json:"model_family,omitempty"`
	ModelName          string              `json:"model_name,omitempty"`
	SerialNumber       string              `json:"serial_number,omitempty"`
	FirmwareVersion    string              `json:"firmware_version,omitempty"`
	SmartStatus        SmartStatus         `json:"smart_status"`
	SmartSupport       *SmartSupport       `json:"smart_support,omitempty"`
	Temperature        *DeviceTemperature  `json:"temperature,omitempty"`
	PowerOnTime        *PowerOnTime        `json:"power_on_time,omitempty"`
	PowerCycleCount    *int64              `json:"power_cycle_count,omitempty"`
	RotationRate       int                 `json:"rotation_rate,omitempty"`
	AtaSmartAttributes *AtaSmartAttributes `json:"ata_smart_attributes,omitempty"`
	NvmeSmart          *NvmeSmartHealth    `json:"nvme_smart_health_information_log,omitempty"`
	ScsiErrorLog       *ScsiErrorLog       `json:"scsi_error_counter_log,omitempty"`
}

type DeviceTemperature struct {
	Current int `json:"current"`
}

type PowerOnTime struct {
	Hours int64 `json:"hours"`
}

type SmartctlInfo struct {
	Version    []int     `json:"version"`
	ExitStatus int       `json:"exit_status"`
	Messages   []Message `json:"messages,omitempty"`
}

// Message is a diagnostic emitted by smartctl itself. The JSON output encodes
// these as objects, so decoding them as plain strings breaks every response
// that carries a message.
type Message struct {
	String   string `json:"string"`
	Severity string `json:"severity"`
}

// HasExitBit reports whether the smartctl exit status has the given bit set.
func (i SmartctlInfo) HasExitBit(bit int) bool {
	return i.ExitStatus&bit != 0
}

// Text joins all messages into a single human readable line.
func (i SmartctlInfo) Text() string {
	parts := make([]string, 0, len(i.Messages))
	for _, message := range i.Messages {
		if message.String != "" {
			parts = append(parts, message.String)
		}
	}
	return strings.Join(parts, "; ")
}

// IndicatesLowPower reports whether smartctl skipped the device because it was
// in a sleep or standby state, which is the expected outcome of --nocheck=standby.
func (i SmartctlInfo) IndicatesLowPower() bool {
	for _, message := range i.Messages {
		upper := strings.ToUpper(message.String)
		if strings.Contains(upper, "STANDBY") || strings.Contains(upper, "SLEEP") {
			return true
		}
	}
	return false
}

type DeviceHeader struct {
	Name     string `json:"name"`
	InfoName string `json:"info_name"`
	Type     string `json:"type"`
	Protocol string `json:"protocol"`
}

type SmartStatus struct {
	Passed bool `json:"passed"`
}

// SmartSupport reflects whether the device advertises SMART capability and
// whether it is currently enabled. Some older or virtualized devices (e.g.
// certain USB bridges, some virtual/cloud block devices) do not support SMART
// at all, in which case Available is false.
type SmartSupport struct {
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
}

const (
	AttributeWhenFailedNow  = "now"
	AttributeWhenFailedPast = "past"
	AttributeWhenFailedNone = ""

	// Plaintext / legacy aliases.
	AttributeWhenFailedFailingNow = "failing_now"
	AttributeWhenFailedInThePast  = "in_the_past"
	AttributeWhenFailedDash       = "-"
)

type AtaSmartAttributes struct {
	Revision int            `json:"revision"`
	Table    []AtaAttribute `json:"table"`
}

type AtaAttribute struct {
	ID         int             `json:"id"`
	Name       string          `json:"name"`
	Value      int             `json:"value"`
	Worst      int             `json:"worst"`
	Threshold  int             `json:"thresh"`
	WhenFailed string          `json:"when_failed"` // See WhenFailed* constants
	Flags      map[string]any  `json:"flags,omitempty"`
	Raw        AtaAttributeRaw `json:"raw"`
}

type AtaAttributeRaw struct {
	Value  int64  `json:"value"`
	String string `json:"string"`
}

type NvmeSmartHealth struct {
	CriticalWarning         int   `json:"critical_warning"`
	Temperature             int   `json:"temperature"`
	AvailableSpare          int   `json:"available_spare"`
	AvailableSpareThreshold int   `json:"available_spare_threshold"`
	PercentageUsed          int   `json:"percentage_used"`
	DataUnitsRead           int64 `json:"data_units_read"`
	DataUnitsWritten        int64 `json:"data_units_written"`
	HostReads               int64 `json:"host_reads"`
	HostWrites              int64 `json:"host_writes"`
	MediaErrors             int64 `json:"media_errors"`
	NumErrLogEntries        int64 `json:"num_err_log_entries"`
}

// ScsiErrorLog mirrors the nested shape smartctl uses for SCSI counters. Its
// presence is what identifies a SCSI device; the counters themselves are not
// part of the health assessment.
type ScsiErrorLog struct {
	Read  *ScsiErrorCounters `json:"read,omitempty"`
	Write *ScsiErrorCounters `json:"write,omitempty"`
}

type ScsiErrorCounters struct {
	TotalErrorsCorrected   int64 `json:"total_errors_corrected"`
	TotalUncorrectedErrors int64 `json:"total_uncorrected_errors"`
}
