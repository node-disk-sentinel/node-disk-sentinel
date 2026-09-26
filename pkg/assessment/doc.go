// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package assessment implements a deterministic, prioritized health evaluation
// cascade for ATA, NVMe, and SCSI block devices based on smartctl telemetry.
//
// ============================================================================
// Design Rationale: Why Formal Specifications, Not Custom Heuristics
// ============================================================================
//
// Rather than inventing arbitrary failure heuristics or unvalidated thresholds,
// node-disk-sentinel aligns its evaluation model with formal storage specifications
// and proven concepts from the Linux storage ecosystem.
//
// In particular, the ATA assessment incorporates design principles from libatasmart,
// prioritizing manufacturer-calibrated thresholds over synthetic heuristics to
// avoid false positives. This approach is reinforced by large-scale empirical failure
// research (such as Backblaze Drive Stats), which demonstrates that raw counts of
// Reallocated Sectors (ATA 5), Current Pending Sectors (ATA 197), and NVMe Media
// Errors serve as the most dependable early indicators of drive degradation well
// before overall self-tests fail. The cascade below prioritizes sector error
// detection accordingly.
//
// ============================================================================
// Evaluate: Per-Protocol Severity Cascades & Specification References
// ============================================================================
//
// 1. ATA Cascade (incorporating tiered severity concepts inspired by libatasmart):
//
//	Level 1 - SelfAssessmentFailed: Overall SMART health self-test failed (smart_status.passed == false
//	          or smartctl exit bit 3). Corresponds to SK_SMART_OVERALL_BAD_STATUS.
//	Level 2 - ExcessiveSectorErrors: Reallocated (ATA 5) or pending (ATA 197) sector count has reached
//	          or breached the manufacturer normalized threshold (when_failed == "failing_now").
//	          Corresponds to SK_SMART_OVERALL_BAD_SECTOR_MANY in libatasmart.
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
package assessment
