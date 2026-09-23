// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package assessment

import (
	"fmt"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
)

// AssessmentResult contains the assessed health status and any active findings.
type AssessmentResult struct {
	Status   v1alpha1.DiskHealthStatus
	Findings []v1alpha1.Finding
}

// Evaluate evaluates SMART output according to a prioritized severity cascade
// for ATA, NVMe, and SCSI block devices.
//
// Origin and specification references:
//
// 1. ATA Cascade:
// Derived from libatasmart (specifically upstream commit
// df3b96ab43fa24730455e8042c4649cab5692733 "Drop our own 'many bad sectors' heuristic"):
//
//	Level 1 - SelfAssessmentFailed: Overall SMART health self-test failed (smart_status.passed == false
//	          or smartctl exit bit 3). Corresponds to SK_SMART_OVERALL_BAD_STATUS.
//	Level 2 - ExcessiveSectorErrors: Reallocated (ATA 5) or pending (ATA 197) sector count has reached
//	          or breached the manufacturer normalized threshold (when_failed == "failing_now").
//	          Corresponds to SK_SMART_OVERALL_BAD_SECTOR_MANY in libatasmart commit df3b96ab.
//	Level 3 - AttributeFailingNow: Any other pre-failure SMART attribute currently failing its
//	          manufacturer threshold (when_failed == "failing_now" or smartctl exit bit 4).
//	          Corresponds to SK_SMART_OVERALL_BAD_ATTRIBUTE_NOW.
//	Level 4 - SectorErrors: Bad sectors exist (ATA 5 or ATA 197 raw count > 0), but the normalized
//	          attributes have not breached the manufacturer failure threshold.
//	          Corresponds to SK_SMART_OVERALL_BAD_SECTOR.
//	Level 5 - AttributeFailedInPast: An attribute dropped below threshold in the past but is not
//	          currently below threshold (when_failed == "in_the_past" or smartctl exit bit 5).
//	          Corresponds to SK_SMART_OVERALL_BAD_ATTRIBUTE_IN_THE_PAST.
//	Level 6 - Good: All monitored attributes within design parameters and zero sector errors observed.
//	          Corresponds to SK_SMART_OVERALL_GOOD.
//
// 2. NVMe Evaluation:
// Derived directly from the NVM Express Base Specification ("SMART / Health Information Log"
// and "Critical Warning" register definitions):
//
//	Level 1 - SelfAssessmentFailed: Device reported overall failure, smartctl exit bit 3, or
//	          CriticalWarning > 0. The Critical Warning byte is a hardware bitmask defined in the NVMe spec:
//	          Bit 0: Available spare space has fallen below threshold.
//	          Bit 1: Temperature above or below threshold.
//	          Bit 2: NVM subsystem reliability degraded (internal reliability or media errors).
//	          Bit 3: Media has been placed in read-only mode.
//	          Bit 4: Volatile memory backup device has failed (power loss protection failure).
//	          Bit 5: Persistent memory region has become read-only or unreachable.
//	Level 2 - AttributeFailingNow:
//	          AvailableSpare < AvailableSpareThreshold: Remaining spare capacity has breached the
//	          standardized manufacturer threshold (even if bit 0 was not yet latched by firmware).
//	          PercentageUsed >= 100: Estimated endurance of the NVM subsystem has been fully consumed.
//	Level 3 - SectorErrors:
//	          MediaErrors > 0: The NVMe controller recorded unrecovered data integrity errors (e.g.
//	          uncorrectable ECC, CRC checksum, or LBA tag mismatch). This is the exact NVMe functional
//	          equivalent to physical bad sectors on rotating media.
//	Level 4 - Good: Controller reports zero critical warnings, healthy spare capacity, and zero media errors.
//
// 3. SCSI / SAS Evaluation:
// Derived from the SCSI Primary Commands (SPC) and SCSI Block Commands (SBC) standards,
// collected via smartctl's scsi_error_counter_log:
//
//	Level 1 - SelfAssessmentFailed: SCSI device self-assessment test reported failing (or exit bit 3).
//	Level 2 - SectorErrors: TotalUncorrectedErrors > 0 in either read or write error counter logs.
//	Level 3 - Good: Zero uncorrected read/write errors.
func Evaluate(smartData *smartmontools.SmartctlOutput) AssessmentResult {
	if smartData == nil {
		return AssessmentResult{
			Status: v1alpha1.StatusUnknown,
		}
	}

	var findings []v1alpha1.Finding

	// Check if NVMe or SCSI (non-ATA).
	isNvmeOrScsi := smartData.NvmeSmart != nil || smartData.ScsiErrorLog != nil ||
		smartData.Device.Protocol == "NVMe" || smartData.Device.Protocol == "SCSI"

	if isNvmeOrScsi && smartData.AtaSmartAttributes == nil {
		return evaluateNvmeOrScsi(smartData)
	}

	// ATA Rules Cascade
	// Level 1: SelfAssessmentFailed.
	if !smartData.SmartStatus.Passed || smartData.Smartctl.HasExitBit(smartmontools.ExitBitDiskFailing) {
		findings = append(findings, v1alpha1.Finding{
			ID:            "SELF_ASSESSMENT_FAILED",
			AttributeName: "SmartStatus",
			Message:       "SMART overall-health self-assessment test failed (DISK FAILING)",
		})
		return AssessmentResult{
			Status:   v1alpha1.StatusSelfAssessmentFailed,
			Findings: findings,
		}
	}

	var (
		hasSectorFailingNow bool
		hasOtherFailingNow  bool
		hasFailedInPast     bool
		reallocatedSec      int64
		pendingSec          int64
	)

	if smartData.AtaSmartAttributes != nil {
		for _, attr := range smartData.AtaSmartAttributes.Table {
			if isAttributeFailingNow(attr) {
				if attr.ID == 5 || attr.ID == 197 {
					hasSectorFailingNow = true
				} else {
					hasOtherFailingNow = true
				}
				findings = append(findings, v1alpha1.Finding{
					ID:            fmt.Sprintf("ATTRIBUTE_%d_FAILING_NOW", attr.ID),
					AttributeID:   new(attr.ID),
					AttributeName: attr.Name,
					RawValue:      attr.Raw.Value,
					Message:       fmt.Sprintf("Attribute %s (ID %d) is currently FAILING NOW (value: %d, thresh: %d)", attr.Name, attr.ID, attr.Value, attr.Threshold),
				})
			} else if isAttributeFailedInPast(attr) {
				hasFailedInPast = true
				findings = append(findings, v1alpha1.Finding{
					ID:            fmt.Sprintf("ATTRIBUTE_%d_FAILED_PAST", attr.ID),
					AttributeID:   new(attr.ID),
					AttributeName: attr.Name,
					RawValue:      attr.Raw.Value,
					Message:       fmt.Sprintf("Attribute %s (ID %d) previously dropped below threshold (in the past)", attr.Name, attr.ID),
				})
			}

			// ATA 5: Reallocated_Sector_Ct.
			if attr.ID == 5 {
				reallocatedSec = attr.Raw.Value
			}
			// ATA 197: Current_Pending_Sector.
			if attr.ID == 197 {
				pendingSec = attr.Raw.Value
			}
		}
	}

	// Level 2: ExcessiveSectorErrors (reallocated or pending sector threshold breached)
	// Follows libatasmart upstream commit df3b96ab (SK_SMART_OVERALL_BAD_SECTOR_MANY):
	// when reallocated or pending sector count breaches manufacturer threshold.
	if hasSectorFailingNow {
		findings = append([]v1alpha1.Finding{{
			ID:            "EXCESSIVE_SECTOR_ERRORS",
			AttributeName: "ReallocatedAndPendingSectors",
			RawValue:      reallocatedSec + pendingSec,
			Message:       "Reallocated or pending sector count breached manufacturer threshold (failing now)",
		}}, findings...)
		return AssessmentResult{
			Status:   v1alpha1.StatusExcessiveSectorErrors,
			Findings: findings,
		}
	}

	// Level 3: AttributeFailingNow (any other attribute failing now).
	if hasOtherFailingNow || smartData.Smartctl.HasExitBit(smartmontools.ExitBitPrefailBelow) {
		return AssessmentResult{
			Status:   v1alpha1.StatusAttributeFailingNow,
			Findings: findings,
		}
	}

	// Level 4: SectorErrors (bad sectors present, but threshold not breached).
	totalBadSectors := reallocatedSec + pendingSec
	if totalBadSectors > 0 {
		findings = append([]v1alpha1.Finding{{
			ID:            "SECTOR_ERRORS",
			AttributeName: "ReallocatedAndPendingSectors",
			RawValue:      totalBadSectors,
			Message: fmt.Sprintf("Disk has sector errors (Reallocated: %d, Pending: %d)",
				reallocatedSec, pendingSec),
		}}, findings...)
		return AssessmentResult{
			Status:   v1alpha1.StatusSectorErrors,
			Findings: findings,
		}
	}

	// Level 5: AttributeFailedInPast.
	if hasFailedInPast || smartData.Smartctl.HasExitBit(smartmontools.ExitBitBelowPast) {
		return AssessmentResult{
			Status:   v1alpha1.StatusAttributeFailedInPast,
			Findings: findings,
		}
	}

	// Level 6: Good.
	return AssessmentResult{
		Status:   v1alpha1.StatusGood,
		Findings: findings,
	}
}

func evaluateNvmeOrScsi(smartData *smartmontools.SmartctlOutput) AssessmentResult {
	var findings []v1alpha1.Finding

	// NVMe Evaluation.
	if smartData.NvmeSmart != nil {
		nvme := smartData.NvmeSmart

		// Level 1: SelfAssessmentFailed (Passed == false, exit bit 3, or CriticalWarning > 0).
		if !smartData.SmartStatus.Passed || smartData.Smartctl.HasExitBit(smartmontools.ExitBitDiskFailing) || nvme.CriticalWarning > 0 {
			msg := "Device self-assessment reported failing or critical warning"
			if nvme.CriticalWarning > 0 {
				msg = fmt.Sprintf("NVMe critical warning bitmask set (0x%02x)", nvme.CriticalWarning)
			}
			findings = append(findings, v1alpha1.Finding{
				ID:            "NVME_CRITICAL_WARNING",
				AttributeName: "CriticalWarning",
				RawValue:      int64(nvme.CriticalWarning),
				Message:       msg,
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusSelfAssessmentFailed,
				Findings: findings,
			}
		}

		// Level 2: AttributeFailingNow (AvailableSpare < AvailableSpareThreshold, or PercentageUsed >= 100).
		if nvme.AvailableSpareThreshold > 0 && nvme.AvailableSpare < nvme.AvailableSpareThreshold {
			findings = append(findings, v1alpha1.Finding{
				ID:            "NVME_SPARE_BELOW_THRESHOLD",
				AttributeName: "AvailableSpare",
				RawValue:      int64(nvme.AvailableSpare),
				Message: fmt.Sprintf("NVMe available spare (%d%%) is below threshold (%d%%)",
					nvme.AvailableSpare, nvme.AvailableSpareThreshold),
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusAttributeFailingNow,
				Findings: findings,
			}
		}

		if nvme.PercentageUsed >= 100 {
			findings = append(findings, v1alpha1.Finding{
				ID:            "NVME_ENDURANCE_EXHAUSTED",
				AttributeName: "PercentageUsed",
				RawValue:      int64(nvme.PercentageUsed),
				Message: fmt.Sprintf("NVMe estimated endurance exhausted (%d%% used)",
					nvme.PercentageUsed),
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusAttributeFailingNow,
				Findings: findings,
			}
		}

		// Level 3: SectorErrors (MediaErrors > 0).
		if nvme.MediaErrors > 0 {
			findings = append(findings, v1alpha1.Finding{
				ID:            "NVME_MEDIA_ERRORS",
				AttributeName: "MediaErrors",
				RawValue:      nvme.MediaErrors,
				Message: fmt.Sprintf("NVMe reported %d unrecovered data integrity errors",
					nvme.MediaErrors),
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusSectorErrors,
				Findings: findings,
			}
		}

		return AssessmentResult{
			Status:   v1alpha1.StatusGood,
			Findings: findings,
		}
	}

	// SCSI Evaluation.
	if smartData.ScsiErrorLog != nil || smartData.Device.Protocol == "SCSI" {
		if !smartData.SmartStatus.Passed || smartData.Smartctl.HasExitBit(smartmontools.ExitBitDiskFailing) {
			findings = append(findings, v1alpha1.Finding{
				ID:            "SCSI_SELF_ASSESSMENT_FAILED",
				AttributeName: "SmartStatus",
				Message:       "SCSI device self-assessment reported failing",
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusSelfAssessmentFailed,
				Findings: findings,
			}
		}

		var uncorrected int64
		if smartData.ScsiErrorLog != nil {
			if smartData.ScsiErrorLog.Read != nil {
				uncorrected += smartData.ScsiErrorLog.Read.TotalUncorrectedErrors
			}
			if smartData.ScsiErrorLog.Write != nil {
				uncorrected += smartData.ScsiErrorLog.Write.TotalUncorrectedErrors
			}
		}

		if uncorrected > 0 {
			findings = append(findings, v1alpha1.Finding{
				ID:            "SCSI_UNCORRECTED_ERRORS",
				AttributeName: "TotalUncorrectedErrors",
				RawValue:      uncorrected,
				Message: fmt.Sprintf("SCSI device reported %d uncorrected errors",
					uncorrected),
			})
			return AssessmentResult{
				Status:   v1alpha1.StatusSectorErrors,
				Findings: findings,
			}
		}

		return AssessmentResult{
			Status:   v1alpha1.StatusGood,
			Findings: findings,
		}
	}

	// Fallback for non-ATA without NvmeSmart / ScsiErrorLog.
	if !smartData.SmartStatus.Passed || smartData.Smartctl.HasExitBit(smartmontools.ExitBitDiskFailing) {
		findings = append(findings, v1alpha1.Finding{
			ID:            "DEVICE_SELF_ASSESSMENT_FAILED",
			AttributeName: "SmartStatus",
			Message:       "Device self-assessment reported failing",
		})
		return AssessmentResult{
			Status:   v1alpha1.StatusSelfAssessmentFailed,
			Findings: findings,
		}
	}

	return AssessmentResult{
		Status:   v1alpha1.StatusGood,
		Findings: findings,
	}
}

// isAttributeFailingNow evaluates whether an ATA attribute is currently failing its threshold.
// It checks smartctl -j native JSON values ("now"), plaintext aliases ("failing_now"),
// and normalized value vs threshold (1 <= threshold <= 0xFD).
func isAttributeFailingNow(attr smartmontools.AtaAttribute) bool {
	if attr.WhenFailed == smartmontools.AttributeWhenFailedNow ||
		strings.EqualFold(attr.WhenFailed, smartmontools.AttributeWhenFailedFailingNow) {
		return true
	}
	if attr.Threshold > 0 && attr.Threshold <= 0xFD && attr.Value > 0 && attr.Value <= attr.Threshold {
		return true
	}
	return false
}

// isAttributeFailedInPast evaluates whether an ATA attribute failed its threshold in the past.
// It checks smartctl -j native JSON values ("past"), plaintext aliases ("in_the_past"),
// and worst value vs threshold (1 <= threshold <= 0xFD).
func isAttributeFailedInPast(attr smartmontools.AtaAttribute) bool {
	if attr.WhenFailed == smartmontools.AttributeWhenFailedPast ||
		strings.EqualFold(attr.WhenFailed, smartmontools.AttributeWhenFailedInThePast) {
		return true
	}
	if attr.Threshold > 0 && attr.Threshold <= 0xFD && attr.Worst > 0 && attr.Worst <= attr.Threshold {
		return true
	}
	return false
}
