// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	RuleIDBlockIOError = "KERNEL_BLOCK_IO_ERROR"
	RuleIDScsiError    = "KERNEL_SCSI_ERROR"
	RuleIDNvmeError    = "KERNEL_NVME_ERROR"
)

// DefaultRules is the built-in rule set evaluated against kernel storage log entries.
// It deliberately targets physical block-layer and hardware-driver errors only
// (drivers/block, drivers/scsi, drivers/nvme). Filesystem-level errors (such as
// Buffer I/O errors from fs/buffer.c) are intentionally omitted to avoid false
// positives from purely logical filesystem corruptions or unmount events.
var DefaultRules = []Rule{
	{
		ID:          RuleIDBlockIOError,
		Description: "Kernel block layer I/O error",
		Regex: regexp.MustCompile(
			`(?i)(?:(?:blk_update_request|print_req_error|end_request):\s+)?I/O error,\s+dev\s+([a-zA-Z0-9_\-]+)(?:[,\s]+sector\s+(\d+))?(?:[,\s]+op\s+([^,\s]+(?:\s*\([^)]+\))?))?`,
		),
		Extract: func(m []string, msg string) (string, int64, Op) {
			rawDev := m[1]
			var sector int64 = -1
			if len(m) >= 3 && m[2] != "" {
				if s, err := strconv.ParseInt(m[2], 10, 64); err == nil {
					sector = s
				}
			}
			op := OpUnknown
			if len(m) >= 4 && m[3] != "" {
				op = ParseOp(m[3])
			}
			if op == OpUnknown {
				op = ParseOp(msg)
			}
			return rawDev, sector, op
		},
	},
	{
		ID:          RuleIDScsiError,
		Description: "SCSI command failure or medium error",
		Regex: regexp.MustCompile(
			`(?i)sd\s+\d+:\d+:\d+:\d+:\s+\[([a-zA-Z0-9_\-]+)\]\s+tag#\d+\s+(?:FAILED Result|Sense Key\s*:\s*Medium Error|Add\.\s*Sense\s*:\s*Unrecovered)`,
		),
		Extract: func(m []string, msg string) (string, int64, Op) {
			rawDev := m[1]
			return rawDev, -1, ParseOp(msg)
		},
	},
	{
		ID:          RuleIDNvmeError,
		Description: "NVMe controller or media error",
		Regex: regexp.MustCompile(
			`(?i)(?:nvme\s+)?(nvme\d+n\d+):\s+(?:I/O\s+\d+\s+QID\s+\d+\s+timeout|(?:READ|WRITE).*Status:\s*0x[0-9a-fA-F]+)`,
		),
		Extract: func(m []string, msg string) (string, int64, Op) {
			rawDev := m[1]
			return rawDev, -1, ParseOp(msg)
		},
	},
}

var (
	// nvmePartitionRegex matches NVMe partitions like nvme0n1p1.
	nvmePartitionRegex = regexp.MustCompile(`^(nvme\d+n\d+)p\d+$`)

	// standardPartitionRegex matches traditional disk partitions like sda1, vda2, hda1.
	standardPartitionRegex = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|hd[a-z]+|xvd[a-z]+)\d+$`)
)

// BaseDeviceName normalizes a partition or block device path/name to its
// base physical disk device name (e.g. "sdb1" -> "sdb", "nvme0n1p2" -> "nvme0n1").
func BaseDeviceName(raw string) string {
	clean := strings.TrimPrefix(raw, "/dev/")

	if m := nvmePartitionRegex.FindStringSubmatch(clean); len(m) == 2 {
		return m[1]
	}
	if m := standardPartitionRegex.FindStringSubmatch(clean); len(m) == 2 {
		return m[1]
	}
	return clean
}

// CleanKernelMessage extracts the payload message from a /dev/kmsg or syslog line.
func CleanKernelMessage(line string) string {
	line = strings.TrimSpace(line)

	if semiIdx := strings.Index(line, ";"); semiIdx != -1 {
		line = line[semiIdx+1:]
	}

	if kernIdx := strings.Index(line, "kernel: "); kernIdx != -1 {
		line = line[kernIdx+len("kernel: "):]
	}

	return strings.TrimSpace(line)
}

// ParseOp normalizes an operation string to an Op enum.
func ParseOp(raw string) Op {
	upper := strings.ToUpper(raw)
	switch {
	case strings.Contains(upper, "WRITE"):
		return OpWrite
	case strings.Contains(upper, "READ"):
		return OpRead
	case strings.Contains(upper, "DISCARD"):
		return OpDiscard
	case strings.Contains(upper, "FLUSH"):
		return OpFlush
	default:
		return OpUnknown
	}
}

// ParseKernelMessage inspects a raw kernel log line using DefaultRules and extracts
// a KernelError if it represents a storage failure.
func ParseKernelMessage(rawLine string) (*KernelError, bool) {
	return ParseKernelMessageWithRules(rawLine, DefaultRules)
}

// ParseKernelMessageWithRules matches a raw kernel line against a custom set of rules.
func ParseKernelMessageWithRules(rawLine string, rules []Rule) (*KernelError, bool) {
	msg := CleanKernelMessage(rawLine)
	if msg == "" {
		return nil, false
	}

	for _, rule := range rules {
		m := rule.Regex.FindStringSubmatch(msg)
		if len(m) == 0 {
			continue
		}

		rawDev, sector, op := rule.Extract(m, msg)
		if rawDev == "" {
			continue
		}

		return &KernelError{
			RuleID:      rule.ID,
			Description: rule.Description,
			Device:      BaseDeviceName(rawDev),
			RawDevice:   rawDev,
			Sector:      sector,
			Op:          op,
			RawMessage:  msg,
			Timestamp:   time.Now(),
		}, true
	}

	return nil, false
}
