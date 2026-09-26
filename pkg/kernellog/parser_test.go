// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"testing"
)

func TestParseKernelMessage(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantOK     bool
		wantRuleID string
		wantDev    string
		wantRawDev string
		wantSector int64
		wantOp     Op
	}{
		{
			name:       "User example with syslog prefix",
			input:      "Sep 25 18:46:07 titan kernel: I/O error, dev sdb, sector 642872 op 0x1:(WRITE) flags 0x4000 phys_seg 64 prio class 0",
			wantOK:     true,
			wantRuleID: RuleIDBlockIOError,
			wantDev:    "sdb",
			wantRawDev: "sdb",
			wantSector: 642872,
			wantOp:     OpWrite,
		},
		{
			name:       "Structured /dev/kmsg line",
			input:      "3,105,987654321,-;I/O error, dev sdc, sector 123456 op 0x0:(READ) flags 0x0",
			wantOK:     true,
			wantRuleID: RuleIDBlockIOError,
			wantDev:    "sdc",
			wantRawDev: "sdc",
			wantSector: 123456,
			wantOp:     OpRead,
		},
		{
			name:       "blk_update_request format (Linux 5.x/6.x)",
			input:      "blk_update_request: I/O error, dev sda, sector 9999 op 0x1:(WRITE) flags 0x800",
			wantOK:     true,
			wantRuleID: RuleIDBlockIOError,
			wantDev:    "sda",
			wantRawDev: "sda",
			wantSector: 9999,
			wantOp:     OpWrite,
		},
		{
			name:       "print_req_error format (Linux 4.x)",
			input:      "print_req_error: I/O error, dev sdd, sector 54321",
			wantOK:     true,
			wantRuleID: RuleIDBlockIOError,
			wantDev:    "sdd",
			wantRawDev: "sdd",
			wantSector: 54321,
			wantOp:     OpUnknown,
		},
		{
			name:   "Buffer I/O error is ignored (filesystem/page-cache layer, not block layer)",
			input:  "Buffer I/O error on dev sdb1, logical block 500, lost async page write",
			wantOK: false,
		},
		{
			name:   "Buffer I/O error read is ignored",
			input:  "Buffer I/O error on dev nvme0n1p2, logical block 1024, async page read",
			wantOK: false,
		},
		{
			name:       "SCSI Medium Error sense key",
			input:      "sd 2:0:0:0: [sdb] tag#12 Sense Key : Medium Error [current]",
			wantOK:     true,
			wantRuleID: RuleIDScsiError,
			wantDev:    "sdb",
			wantRawDev: "sdb",
			wantSector: -1,
			wantOp:     OpUnknown,
		},
		{
			name:       "SCSI Unrecovered read error",
			input:      "sd 1:0:0:0: [sda] tag#4 Add. Sense: Unrecovered read error",
			wantOK:     true,
			wantRuleID: RuleIDScsiError,
			wantDev:    "sda",
			wantRawDev: "sda",
			wantSector: -1,
			wantOp:     OpRead,
		},
		{
			name:       "NVMe controller read error status",
			input:      "nvme nvme0n1: READ(0x2) ... Status: 0x281",
			wantOK:     true,
			wantRuleID: RuleIDNvmeError,
			wantDev:    "nvme0n1",
			wantRawDev: "nvme0n1",
			wantSector: -1,
			wantOp:     OpRead,
		},
		{
			name:       "NVMe timeout",
			input:      "nvme nvme1n1: I/O 456 QID 2 timeout, aborting",
			wantOK:     true,
			wantRuleID: RuleIDNvmeError,
			wantDev:    "nvme1n1",
			wantRawDev: "nvme1n1",
			wantSector: -1,
			wantOp:     OpUnknown,
		},
		{
			name:   "Non-disk kernel message (TCP SYN flood)",
			input:  "Sep 25 18:46:07 titan kernel: TCP: request_sock_TCP: Possible SYN flooding on port 80. Dropping request.",
			wantOK: false,
		},
		{
			name:   "Empty line",
			input:  "",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseKernelMessage(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ParseKernelMessage() ok = %v; want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.RuleID != tc.wantRuleID {
				t.Errorf("RuleID = %q; want %q", got.RuleID, tc.wantRuleID)
			}
			if got.Device != tc.wantDev {
				t.Errorf("Device = %q; want %q", got.Device, tc.wantDev)
			}
			if got.RawDevice != tc.wantRawDev {
				t.Errorf("RawDevice = %q; want %q", got.RawDevice, tc.wantRawDev)
			}
			if got.Sector != tc.wantSector {
				t.Errorf("Sector = %d; want %d", got.Sector, tc.wantSector)
			}
			if got.Op != tc.wantOp {
				t.Errorf("Op = %q; want %q", got.Op, tc.wantOp)
			}
		})
	}
}

func TestBaseDeviceName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sda", "sda"},
		{"sdb1", "sdb"},
		{"sdb12", "sdb"},
		{"/dev/sdc3", "sdc"},
		{"nvme0n1", "nvme0n1"},
		{"nvme0n1p1", "nvme0n1"},
		{"/dev/nvme2n1p3", "nvme2n1"},
		{"vda", "vda"},
		{"vda1", "vda"},
		{"xvda2", "xvda"},
		{"dm-0", "dm-0"},
		{"loop0", "loop0"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := BaseDeviceName(tc.input)
			if got != tc.want {
				t.Errorf("BaseDeviceName(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}
