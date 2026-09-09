// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package smartmontools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/votdev/node-disk-sentinel/pkg/utils"
)

var (
	ErrDeviceInStandby  = errors.New("device is in sleep or standby mode")
	ErrDeviceNotFound   = errors.New("device could not be opened")
	ErrSmartUnsupported = errors.New("SMART is unavailable or disabled")
)

// smartctl reports its result as a bitmask rather than a single status code.
// See the EXIT STATUS section of smartctl(8).
const (
	exitBitCommandLine = 1 << 0 // command line did not parse, or device type undetectable
	exitBitDeviceOpen  = 1 << 1 // open failed, or device is in low power mode with -n

	// ExitBitCommandFailed marks a failed SMART command; partial data may
	// still be present in the output.
	ExitBitCommandFailed = 1 << 2
	// ExitBitDiskFailing marks a failed overall-health self-assessment.
	ExitBitDiskFailing = 1 << 3
	// ExitBitPrefailBelow marks pre-failure attributes at or below threshold.
	ExitBitPrefailBelow = 1 << 4
	// ExitBitBelowPast marks attributes that fell below threshold in the past.
	ExitBitBelowPast = 1 << 5
)

// Runner collects SMART data for a single device.
type Runner interface {
	Collect(ctx context.Context, devicePath, extraCmdArgs string) (*SmartctlOutput, error)
}

// ExecRunner invokes the smartctl binary.
type ExecRunner struct {
	BinaryPath string
	Timeout    time.Duration
}

// NewExecRunner returns an ExecRunner with sane defaults.
func NewExecRunner() *ExecRunner {
	return &ExecRunner{BinaryPath: "smartctl", Timeout: 30 * time.Second}
}

// Collect runs "smartctl --json --all --nocheck=standby" against the device
// with any configured additional command-line arguments.
//
// --nocheck=standby keeps sleeping disks asleep, which makes an exit status
// with exitBitDeviceOpen an expected outcome rather than a failure. Higher bits
// describe the health of the disk and still come with usable data, so they are
// reported through the parsed output instead of an error.
func (r *ExecRunner) Collect(ctx context.Context, devicePath, extraCmdArgs string) (*SmartctlOutput, error) {
	binary := r.BinaryPath
	if binary == "" {
		binary = "smartctl"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--json", "--all", "--nocheck=standby"}
	extraArgs, err := utils.SplitCommandArgs(extraCmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid smartctl extraCmdArgs: %w", err)
	}
	args = append(args, extraArgs...)
	args = append(args, devicePath)

	cmd := exec.CommandContext(execCtx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("failed to execute %s: %w (stderr: %s)",
				binary, runErr, strings.TrimSpace(stderr.String()))
		}
		exitCode = exitErr.ExitCode()
	}
	if execCtx.Err() != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("smartctl timed out after %s on %s", timeout, devicePath)
	}

	// smartctl writes valid JSON even for most failures, so decode first and
	// use the payload to classify the exit status.
	var smartctlOutput SmartctlOutput
	parseErr := json.Unmarshal(stdout.Bytes(), &smartctlOutput)
	smartctlOutput.Smartctl.ExitStatus = exitCode

	// Check if smartctl reported that device type could not be detected or SMART is unsupported.
	diagText := diagnostics(parseErr, &smartctlOutput, &stderr)
	diagLower := strings.ToLower(diagText)

	// If smartctl failed (exitCode != 0), verify whether the target device node or symlink target exists.
	// When smartctl is called against a broken symlink or missing node, it frequently outputs
	// "Unable to detect device type". Without checking existence, this would be falsely
	// classified as ErrSmartUnsupported instead of ErrDeviceNotFound.
	if exitCode != 0 {
		if _, statErr := os.Stat(devicePath); statErr != nil {
			return nil, fmt.Errorf("%w: %s (%v)", ErrDeviceNotFound, diagText, statErr)
		}
	}

	if strings.Contains(diagLower, "unable to detect device type") ||
		strings.Contains(diagLower, "mandatory smart command failed") ||
		strings.Contains(diagLower, "inappropriate ioctl for device") {
		return nil, fmt.Errorf("%w: %s", ErrSmartUnsupported, diagText)
	}

	if exitCode&exitBitDeviceOpen != 0 {
		if parseErr == nil && smartctlOutput.Smartctl.IndicatesLowPower() {
			return nil, ErrDeviceInStandby
		}
		return nil, fmt.Errorf("%w: %s", ErrDeviceNotFound, diagText)
	}

	// Bit 0: command line error or missing device argument.
	if exitCode&exitBitCommandLine != 0 {
		return nil, fmt.Errorf("%w: %s", ErrDeviceNotFound, diagText)
	}

	if parseErr != nil {
		return nil, fmt.Errorf("failed to parse smartctl output for %s (exit %d): %w",
			devicePath, exitCode, parseErr)
	}

	if smartctlOutput.SmartSupport != nil && (!smartctlOutput.SmartSupport.Available || !smartctlOutput.SmartSupport.Enabled) {
		return nil, ErrSmartUnsupported
	}

	return &smartctlOutput, nil
}

// diagnostics builds the most informative description available for a failure.
func diagnostics(parseErr error, smartctlOutput *SmartctlOutput, stderr *bytes.Buffer) string {
	if parseErr == nil {
		if text := smartctlOutput.Smartctl.Text(); text != "" {
			return text
		}
	}
	if text := strings.TrimSpace(stderr.String()); text != "" {
		return text
	}
	return "no diagnostic output"
}
