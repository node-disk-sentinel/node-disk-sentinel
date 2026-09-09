// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package assessment

import (
	"testing"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
	"github.com/votdev/node-disk-sentinel/pkg/smartmontools"
)

func TestEvaluate_AtaCascade(t *testing.T) {
	t.Run("Level 1: SelfAssessmentFailed", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: false},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{ID: 5, Name: "Reallocated_Sector_Ct", Raw: smartmontools.AtaAttributeRaw{Value: 100}},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSelfAssessmentFailed {
			t.Errorf("expected StatusSelfAssessmentFailed, got %v", res.Status)
		}
	})

	t.Run("Level 2: ExcessiveSectorErrors (native smartctl -j when_failed: now)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         5,
						Name:       "Reallocated_Sector_Ct",
						WhenFailed: smartmontools.AttributeWhenFailedNow,
						Value:      1,
						Threshold:  5,
						Raw:        smartmontools.AtaAttributeRaw{Value: 35000},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusExcessiveSectorErrors {
			t.Errorf("expected StatusExcessiveSectorErrors, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "EXCESSIVE_SECTOR_ERRORS" {
			t.Errorf("expected EXCESSIVE_SECTOR_ERRORS finding, got %+v", res.Findings)
		}
	})

	t.Run("Level 2: ExcessiveSectorErrors (sector attribute failing now)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         5,
						Name:       "Reallocated_Sector_Ct",
						WhenFailed: smartmontools.AttributeWhenFailedFailingNow,
						Value:      1,
						Threshold:  5,
						Raw:        smartmontools.AtaAttributeRaw{Value: 35000},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusExcessiveSectorErrors {
			t.Errorf("expected StatusExcessiveSectorErrors, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "EXCESSIVE_SECTOR_ERRORS" {
			t.Errorf("expected EXCESSIVE_SECTOR_ERRORS finding, got %+v", res.Findings)
		}
	})

	t.Run("Level 3: AttributeFailingNow (native smartctl -j when_failed: now)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: smartmontools.AttributeWhenFailedNow,
						Value:      10,
						Threshold:  36,
						Raw:        smartmontools.AtaAttributeRaw{Value: 50},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailingNow {
			t.Errorf("expected StatusAttributeFailingNow, got %v", res.Status)
		}
	})

	t.Run("Level 3: AttributeFailingNow (value <= threshold fallback)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: "", // empty when_failed in JSON
						Value:      10,
						Threshold:  36,
						Raw:        smartmontools.AtaAttributeRaw{Value: 50},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailingNow {
			t.Errorf("expected StatusAttributeFailingNow, got %v", res.Status)
		}
	})

	t.Run("Level 3: AttributeFailingNow (non-sector attribute failing now)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: smartmontools.AttributeWhenFailedFailingNow,
						Value:      10,
						Threshold:  36,
						Raw:        smartmontools.AtaAttributeRaw{Value: 50},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailingNow {
			t.Errorf("expected StatusAttributeFailingNow, got %v", res.Status)
		}
		if len(res.Findings) != 1 {
			t.Errorf("expected 1 finding, got %d", len(res.Findings))
		}
	})

	t.Run("Level 4: SectorErrors (bad sectors without threshold breach)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{ID: 5, Name: "Reallocated_Sector_Ct", Raw: smartmontools.AtaAttributeRaw{Value: 8}},
					{ID: 197, Name: "Current_Pending_Sector", Raw: smartmontools.AtaAttributeRaw{Value: 0}},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSectorErrors {
			t.Errorf("expected StatusSectorErrors, got %v", res.Status)
		}
	})

	t.Run("Level 4: SectorErrors with large counts without threshold breach", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{ID: 5, Name: "Reallocated_Sector_Ct", Raw: smartmontools.AtaAttributeRaw{Value: 25000}},
					{ID: 197, Name: "Current_Pending_Sector", Raw: smartmontools.AtaAttributeRaw{Value: 500}},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSectorErrors {
			t.Errorf("expected StatusSectorErrors, got %v", res.Status)
		}
	})

	t.Run("Level 5: AttributeFailedInPast (native smartctl -j when_failed: past)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: smartmontools.AttributeWhenFailedPast,
						Raw:        smartmontools.AtaAttributeRaw{Value: 0},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailedInPast {
			t.Errorf("expected StatusAttributeFailedInPast, got %v", res.Status)
		}
	})

	t.Run("Level 5: AttributeFailedInPast (worst <= threshold fallback)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: "",
						Value:      100,
						Worst:      25,
						Threshold:  36,
						Raw:        smartmontools.AtaAttributeRaw{Value: 0},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailedInPast {
			t.Errorf("expected StatusAttributeFailedInPast, got %v", res.Status)
		}
	})

	t.Run("Level 5: AttributeFailedInPast (legacy alias in_the_past)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{
						ID:         1,
						Name:       "Raw_Read_Error_Rate",
						WhenFailed: smartmontools.AttributeWhenFailedInThePast,
						Raw:        smartmontools.AtaAttributeRaw{Value: 0},
					},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailedInPast {
			t.Errorf("expected StatusAttributeFailedInPast, got %v", res.Status)
		}
	})

	t.Run("Level 6: Good", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			AtaSmartAttributes: &smartmontools.AtaSmartAttributes{
				Table: []smartmontools.AtaAttribute{
					{ID: 5, Name: "Reallocated_Sector_Ct", WhenFailed: smartmontools.AttributeWhenFailedNone, Raw: smartmontools.AtaAttributeRaw{Value: 0}},
					{ID: 197, Name: "Current_Pending_Sector", WhenFailed: smartmontools.AttributeWhenFailedNone, Raw: smartmontools.AtaAttributeRaw{Value: 0}},
				},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusGood {
			t.Errorf("expected StatusGood, got %v", res.Status)
		}
		if len(res.Findings) != 0 {
			t.Errorf("expected 0 findings, got %d", len(res.Findings))
		}
	})
}

func TestEvaluate_NVMe(t *testing.T) {
	t.Run("NVMe Good", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			NvmeSmart: &smartmontools.NvmeSmartHealth{
				CriticalWarning:         0,
				AvailableSpare:          100,
				AvailableSpareThreshold: 10,
				PercentageUsed:          15,
				MediaErrors:             0,
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusGood {
			t.Errorf("expected StatusGood, got %v", res.Status)
		}
		if len(res.Findings) != 0 {
			t.Errorf("expected 0 findings, got %d", len(res.Findings))
		}
	})

	t.Run("NVMe Failed Self-Assessment", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: false},
			NvmeSmart:   &smartmontools.NvmeSmartHealth{CriticalWarning: 1},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSelfAssessmentFailed {
			t.Errorf("expected StatusSelfAssessmentFailed, got %v", res.Status)
		}
	})

	t.Run("NVMe Critical Warning Bitmask set even if passed is true", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			NvmeSmart:   &smartmontools.NvmeSmartHealth{CriticalWarning: 4}, // degraded reliability
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSelfAssessmentFailed {
			t.Errorf("expected StatusSelfAssessmentFailed, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "NVME_CRITICAL_WARNING" {
			t.Errorf("expected NVME_CRITICAL_WARNING finding, got %+v", res.Findings)
		}
	})

	t.Run("NVMe Spare Below Threshold", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			NvmeSmart: &smartmontools.NvmeSmartHealth{
				CriticalWarning:         0,
				AvailableSpare:          5,
				AvailableSpareThreshold: 10,
				PercentageUsed:          80,
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailingNow {
			t.Errorf("expected StatusAttributeFailingNow, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "NVME_SPARE_BELOW_THRESHOLD" {
			t.Errorf("expected NVME_SPARE_BELOW_THRESHOLD finding, got %+v", res.Findings)
		}
	})

	t.Run("NVMe Endurance Exhausted (PercentageUsed >= 100)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			NvmeSmart: &smartmontools.NvmeSmartHealth{
				CriticalWarning:         0,
				AvailableSpare:          90,
				AvailableSpareThreshold: 10,
				PercentageUsed:          105,
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusAttributeFailingNow {
			t.Errorf("expected StatusAttributeFailingNow, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "NVME_ENDURANCE_EXHAUSTED" {
			t.Errorf("expected NVME_ENDURANCE_EXHAUSTED finding, got %+v", res.Findings)
		}
	})

	t.Run("NVMe Media Errors (SectorErrors)", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			NvmeSmart: &smartmontools.NvmeSmartHealth{
				CriticalWarning:         0,
				AvailableSpare:          90,
				AvailableSpareThreshold: 10,
				PercentageUsed:          20,
				MediaErrors:             5,
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSectorErrors {
			t.Errorf("expected StatusSectorErrors, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "NVME_MEDIA_ERRORS" {
			t.Errorf("expected NVME_MEDIA_ERRORS finding, got %+v", res.Findings)
		}
	})
}

func TestEvaluate_SCSI(t *testing.T) {
	t.Run("SCSI Good", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			ScsiErrorLog: &smartmontools.ScsiErrorLog{
				Read:  &smartmontools.ScsiErrorCounters{TotalUncorrectedErrors: 0},
				Write: &smartmontools.ScsiErrorCounters{TotalUncorrectedErrors: 0},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusGood {
			t.Errorf("expected StatusGood, got %v", res.Status)
		}
	})

	t.Run("SCSI Uncorrected Errors", func(t *testing.T) {
		out := &smartmontools.SmartctlOutput{
			SmartStatus: smartmontools.SmartStatus{Passed: true},
			ScsiErrorLog: &smartmontools.ScsiErrorLog{
				Read:  &smartmontools.ScsiErrorCounters{TotalUncorrectedErrors: 3},
				Write: &smartmontools.ScsiErrorCounters{TotalUncorrectedErrors: 0},
			},
		}
		res := Evaluate(out)
		if res.Status != v1alpha1.StatusSectorErrors {
			t.Errorf("expected StatusSectorErrors, got %v", res.Status)
		}
		if len(res.Findings) == 0 || res.Findings[0].ID != "SCSI_UNCORRECTED_ERRORS" {
			t.Errorf("expected SCSI_UNCORRECTED_ERRORS finding, got %+v", res.Findings)
		}
	})
}
