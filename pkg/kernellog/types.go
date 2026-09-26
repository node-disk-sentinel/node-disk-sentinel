// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"context"
	"regexp"
	"time"
)

// Op represents the I/O operation type.
type Op string

const (
	OpWrite   Op = "WRITE"
	OpRead    Op = "READ"
	OpDiscard Op = "DISCARD"
	OpFlush   Op = "FLUSH"
	OpUnknown Op = "UNKNOWN"
)

// KernelError represents a detected kernel storage, block layer, or device error.
type KernelError struct {
	// RuleID identifies the matched rule (e.g. "KERNEL_BLOCK_IO_ERROR", "KERNEL_SCSI_ERROR", "KERNEL_NVME_ERROR").
	RuleID string
	// Description provides a human-readable summary of the error type.
	Description string
	// Device is the canonical base block device name (e.g. "sdb", "nvme0n1").
	Device string
	// RawDevice is the device name as logged by the kernel (e.g. "sdb1", "sdb").
	RawDevice string
	// Sector is the affected block sector, or -1 if not specified in the log.
	Sector int64
	// Op is the I/O operation (WRITE, READ, DISCARD, FLUSH, UNKNOWN).
	Op Op
	// RawMessage is the unparsed kernel log message payload.
	RawMessage string
	// Timestamp is the time the error was observed.
	Timestamp time.Time
}

// FindingIDPrefix is the common prefix shared by all kernel-log-derived
// Finding IDs (e.g. "KERNEL_BLOCK_IO_ERROR"). Consumers can use this to
// identify and preserve kernel-derived findings across SMART collections.
const FindingIDPrefix = "KERNEL_"

// Rule defines a pattern matching rule for storage-related kernel messages.
type Rule struct {
	// ID is the unique identifier for this rule, used as the finding ID.
	ID string
	// Description provides a diagnostic description of the failure type.
	Description string
	// Regex matches against the cleaned kernel message.
	Regex *regexp.Regexp
	// Extract parses the device name, sector (-1 if absent), and op from regex matches.
	Extract func(matches []string, msg string) (rawDev string, sector int64, op Op)
}

// Reader provides an abstraction over kernel message sources (/dev/kmsg or mocks).
type Reader interface {
	// ReadMessage blocks until the next kernel log message is available or ctx is done.
	ReadMessage(ctx context.Context) (string, error)
	// Close releases any allocated resources.
	Close() error
}
