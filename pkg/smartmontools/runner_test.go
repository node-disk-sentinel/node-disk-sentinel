// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package smartmontools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSmartctl creates an executable stub that mimics smartctl by printing the
// given JSON and terminating with the given exit status.
func fakeSmartctl(t *testing.T, stdout string, exitCode int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "smartctl")
	script := fmt.Sprintf("#!/bin/sh\ncat <<'SMARTCTL_EOF'\n%s\nSMARTCTL_EOF\nexit %d\n", stdout, exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake smartctl: %v", err)
	}
	return path
}

func collect(t *testing.T, stdout string, exitCode int) (*SmartctlOutput, error) {
	devFile := filepath.Join(t.TempDir(), "sda")
	if err := os.WriteFile(devFile, []byte("fake-device"), 0o600); err != nil {
		t.Fatalf("failed to create fake device file: %v", err)
	}
	return collectDev(t, stdout, exitCode, devFile)
}

func collectDev(t *testing.T, stdout string, exitCode int, devPath string) (*SmartctlOutput, error) {
	t.Helper()

	runner := &ExecRunner{BinaryPath: fakeSmartctl(t, stdout, exitCode), Timeout: 5 * time.Second}
	return runner.Collect(context.Background(), devPath, "")
}

// smartctl encodes messages as objects. Decoding them as strings breaks every
// response that carries one, so this guards the real wire format.
func TestSmartctlMessagesDecodeAsObjects(t *testing.T) {
	raw := []byte(`{"smartctl":{"version":[7,4],"exit_status":0,` +
		`"messages":[{"string":"Device is in STANDBY mode, exit(2)","severity":"information"}]}}`)

	var out SmartctlOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("failed to decode smartctl messages: %v", err)
	}
	if len(out.Smartctl.Messages) != 1 {
		t.Fatalf("got %d messages; want 1", len(out.Smartctl.Messages))
	}
	if !out.Smartctl.IndicatesLowPower() {
		t.Error("IndicatesLowPower() = false; want true for a STANDBY message")
	}
	if got := out.Smartctl.Text(); !strings.Contains(got, "STANDBY") {
		t.Errorf("Text() = %q; want it to mention STANDBY", got)
	}
}

// A healthy disk that merely emits an informational message must still be
// collected successfully.
func TestCollectSucceedsWithInformationalMessages(t *testing.T) {
	out, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 0,
			"messages": [{"string": "Note: informational", "severity": "information"}]},
		"device": {"name": "/dev/sda", "protocol": "ATA"},
		"smart_support": {"available": true, "enabled": true},
		"smart_status": {"passed": true}
	}`, 0)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if !out.SmartStatus.Passed {
		t.Error("expected the disk to report a passing self-assessment")
	}
}

func TestCollectStandbyDevice(t *testing.T) {
	_, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 2,
			"messages": [{"string": "Device is in STANDBY mode, exit(2)", "severity": "information"}]},
		"device": {"name": "/dev/sda", "protocol": "ATA"}
	}`, 2)
	if !errors.Is(err, ErrDeviceInStandby) {
		t.Fatalf("error = %v; want ErrDeviceInStandby", err)
	}
}

// A failed open sets the same exit bit as standby and must not be mistaken
// for a sleeping disk.
func TestCollectDeviceOpenFailure(t *testing.T) {
	_, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 2,
			"messages": [{"string": "/dev/sda: No such device or address", "severity": "error"}]}
	}`, 2)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("error = %v; want ErrDeviceNotFound", err)
	}
	if !strings.Contains(err.Error(), "No such device or address") {
		t.Errorf("error = %v; want it to include the smartctl message", err)
	}
}

// smartctl reports a vanished device with the command line bit, because our
// argument list itself is fixed and always valid.
func TestCollectMissingDevice(t *testing.T) {
	_, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 1,
			"messages": [{"string": "/dev/sda: No such file or directory", "severity": "error"}]}
	}`, 1)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("error = %v; want ErrDeviceNotFound", err)
	}
}

// When smartctl cannot determine the device type or ioctl fails (e.g. VirtIO / virtual disks),
// it must be reported as ErrSmartUnsupported rather than ErrDeviceNotFound IF the device exists.
func TestCollectDeviceTypeUndetectableIsSmartUnsupported(t *testing.T) {
	_, err := collect(t, `{
		"smartctl": {"version": [7,5], "exit_status": 1,
			"messages": [{"string": "/dev/vda: Unable to detect device type", "severity": "error"}]}
	}`, 1)
	if !errors.Is(err, ErrSmartUnsupported) {
		t.Fatalf("error = %v; want ErrSmartUnsupported", err)
	}
}

// When smartctl reports unable to detect device type on a missing file / broken symlink,
// it must be reported as ErrDeviceNotFound.
func TestCollectBrokenSymlinkIsDeviceNotFound(t *testing.T) {
	brokenLink := filepath.Join(t.TempDir(), "broken-symlink")
	_ = os.Symlink("/nonexistent/target/device", brokenLink)

	_, err := collectDev(t, `{
		"smartctl": {"version": [7,5], "exit_status": 1,
			"messages": [{"string": "`+brokenLink+`: Unable to detect device type", "severity": "error"}]}
	}`, 1, brokenLink)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("error = %v; want ErrDeviceNotFound for broken symlink", err)
	}
}

func TestCollectSmartUnsupported(t *testing.T) {
	for _, support := range []string{
		`{"available": false, "enabled": false}`,
		`{"available": true, "enabled": false}`,
	} {
		_, err := collect(t, `{
			"smartctl": {"version": [7,4], "exit_status": 0},
			"smart_support": `+support+`,
			"smart_status": {"passed": true}
		}`, 0)
		if !errors.Is(err, ErrSmartUnsupported) {
			t.Errorf("smart_support %s: error = %v; want ErrSmartUnsupported", support, err)
		}
	}
}

// Health related exit bits still carry usable data and must not fail the call.
func TestCollectReturnsDataForFailingDisk(t *testing.T) {
	out, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 8},
		"device": {"name": "/dev/sda", "protocol": "ATA"},
		"smart_status": {"passed": false}
	}`, ExitBitDiskFailing)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if !out.Smartctl.HasExitBit(ExitBitDiskFailing) {
		t.Error("expected the disk failing exit bit to be preserved")
	}
}

func TestCollectRejectsUnparsableOutput(t *testing.T) {
	_, err := collect(t, `not json at all`, 0)
	if err == nil || !strings.Contains(err.Error(), "failed to parse smartctl output") {
		t.Fatalf("error = %v; want a parse failure", err)
	}
}

// SCSI counters are nested objects; decoding them must not fail and must mark
// the device as SCSI for health assessment.
func TestScsiErrorLogDecodes(t *testing.T) {
	out, err := collect(t, `{
		"smartctl": {"version": [7,4], "exit_status": 0},
		"device": {"name": "/dev/sda", "protocol": "SCSI"},
		"smart_status": {"passed": true},
		"scsi_error_counter_log": {
			"read": {"total_errors_corrected": 3, "total_uncorrected_errors": 0},
			"write": {"total_errors_corrected": 1, "total_uncorrected_errors": 0}
		}
	}`, 0)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if out.ScsiErrorLog == nil || out.ScsiErrorLog.Read == nil {
		t.Fatalf("expected the SCSI error counter log to be decoded, got %#v", out.ScsiErrorLog)
	}
	if out.ScsiErrorLog.Read.TotalErrorsCorrected != 3 {
		t.Errorf("read.total_errors_corrected = %d; want 3", out.ScsiErrorLog.Read.TotalErrorsCorrected)
	}
}

// Extra arguments passed to Collect must appear in the command execution
// before the device path.
func TestCollectPassesExtraCmdArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smartctl")
	// This script verifies that "-d" and "sat" were passed as arguments,
	// and that the device path is the last argument.
	script := `#!/bin/sh
found_device=0
found_sat=0
for arg in "$@"; do
	if [ "$arg" = "sat" ]; then
		found_sat=1
	fi
	last_arg="$arg"
done

if [ "$found_sat" -eq 1 ] && [ "$last_arg" = "/dev/sda" ]; then
	cat <<'EOF'
{"smartctl":{"version":[7,4],"exit_status":0},"device":{"name":"/dev/sda","protocol":"ATA"},"smart_status":{"passed":true}}
EOF
	exit 0
fi
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	runner := &ExecRunner{BinaryPath: path, Timeout: 5 * time.Second}
	out, err := runner.Collect(context.Background(), "/dev/sda", "-d sat")
	if err != nil {
		t.Fatalf("Collect with extraCmdArgs failed: %v", err)
	}
	if !out.SmartStatus.Passed {
		t.Errorf("expected Passed = true")
	}
}

func TestNewExecRunner(t *testing.T) {
	runner := NewExecRunner()
	if runner.BinaryPath != "smartctl" {
		t.Errorf("BinaryPath = %s, want smartctl", runner.BinaryPath)
	}
	if runner.Timeout <= 0 {
		t.Errorf("Timeout should be positive, got %v", runner.Timeout)
	}

	// Test invalid extraCmdArgs returns error.
	_, err := runner.Collect(context.Background(), "/dev/sda", "unterminated 'quote")
	if err == nil {
		t.Errorf("expected error for unterminated quote in extraCmdArgs")
	}
}

// When BinaryPath is empty and Timeout is 0, Collect applies defaults ("smartctl", 30s).
func TestCollect_DefaultsFromEmptyBinaryAndZeroTimeout(t *testing.T) {
	dir := t.TempDir()
	mockPath := filepath.Join(dir, "smartctl")
	script := "#!/bin/sh\ncat <<'EOF'\n{\"smartctl\":{\"version\":[7,4],\"exit_status\":0},\"device\":{\"name\":\"/dev/sda\",\"protocol\":\"ATA\"},\"smart_status\":{\"passed\":true}}\nEOF\nexit 0\n"
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock smartctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := &ExecRunner{BinaryPath: "", Timeout: 0}
	out, err := runner.Collect(context.Background(), "/dev/sda", "")
	if err != nil {
		t.Fatalf("Collect with empty binary and zero timeout failed: %v", err)
	}
	if !out.SmartStatus.Passed {
		t.Errorf("expected Passed = true")
	}
}

func TestCollect_ExecutionFailureNonExitError(t *testing.T) {
	runner := &ExecRunner{BinaryPath: "/nonexistent/binary/path/that/cannot/exist", Timeout: 5 * time.Second}
	_, err := runner.Collect(context.Background(), "/dev/sda", "")
	if err == nil || !strings.Contains(err.Error(), "failed to execute") {
		t.Fatalf("expected 'failed to execute' error, got %v", err)
	}
}

func TestCollect_Timeout(t *testing.T) {
	dir := t.TempDir()
	mockPath := filepath.Join(dir, "smartctl")
	script := "#!/bin/sh\nsleep 0.1\nexit 0\n"
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock smartctl: %v", err)
	}

	runner := &ExecRunner{BinaryPath: mockPath, Timeout: 50 * time.Millisecond}
	_, err := runner.Collect(context.Background(), "/dev/sda", "")
	if err == nil || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestCollect_DiagnosticsFallbackToStderr(t *testing.T) {
	dir := t.TempDir()
	mockPath := filepath.Join(dir, "smartctl")
	script := "#!/bin/sh\necho 'smartctl: fatal error on device' >&2\nexit 1\n"
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock smartctl: %v", err)
	}

	runner := &ExecRunner{BinaryPath: mockPath, Timeout: 5 * time.Second}
	_, err := runner.Collect(context.Background(), "/dev/sda", "")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "smartctl: fatal error on device") {
		t.Errorf("expected error to contain stderr diagnostic text, got %v", err)
	}
}

func TestCollect_DiagnosticsNoDiagnosticOutput(t *testing.T) {
	dir := t.TempDir()
	mockPath := filepath.Join(dir, "smartctl")
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(mockPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock smartctl: %v", err)
	}

	runner := &ExecRunner{BinaryPath: mockPath, Timeout: 5 * time.Second}
	_, err := runner.Collect(context.Background(), "/dev/sda", "")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "no diagnostic output") {
		t.Errorf("expected 'no diagnostic output', got %v", err)
	}
}

func TestCollect_SmartUnsupportedDiagnosticsPatterns(t *testing.T) {
	for _, pattern := range []string{
		"Mandatory SMART command failed",
		"Inappropriate ioctl for device",
	} {
		t.Run(pattern, func(t *testing.T) {
			out := fmt.Sprintf(`{
				"smartctl": {"version": [7,4], "exit_status": 1,
					"messages": [{"string": "%s", "severity": "error"}]}
			}`, pattern)
			_, err := collect(t, out, 1)
			if !errors.Is(err, ErrSmartUnsupported) {
				t.Fatalf("expected ErrSmartUnsupported for pattern %q, got %v", pattern, err)
			}
		})
	}
}
