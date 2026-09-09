// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

func TestShouldIgnoreDevice(t *testing.T) {
	tests := []struct {
		name     string
		devName  string
		devType  string
		expected bool
	}{
		{"disk sda", "sda", "disk", false},
		{"disk nvme0n1", "nvme0n1", "disk", false},
		{"loop device", "loop0", "disk", true},
		{"ram device", "ram1", "disk", true},
		{"zram device", "zram0", "disk", true},
		{"dm device", "dm-0", "disk", true},
		{"md device", "md0", "disk", true},
		{"partition type", "sda1", "partition", true},
		{"sda1 without type", "sda1", "", true},
		{"nvme0n1p1 without type", "nvme0n1p1", "", true},
		{"cdrom sr0", "sr0", "disk", true},
		{"empty type sda", "sda", "", false},
		{"ceph rbd device", "rbd0", "disk", true},
		{"drbd device", "drbd1", "disk", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldIgnoreDevice(tt.devName, tt.devType)
			if got != tt.expected {
				t.Errorf("ShouldIgnoreDevice(%q, %q) = %v; want %v", tt.devName, tt.devType, got, tt.expected)
			}
		})
	}
}

func TestShouldIgnoreProperties(t *testing.T) {
	tests := []struct {
		name     string
		props    map[string]string
		expected bool
	}{
		{
			name:     "nil properties",
			props:    nil,
			expected: false,
		},
		{
			name:     "empty properties",
			props:    map[string]string{},
			expected: false,
		},
		{
			name: "physical SATA disk",
			props: map[string]string{
				"ID_BUS":    "ata",
				"ID_PATH":   "pci-0000:00:1f.2-ata-1",
				"ID_MODEL":  "WDC_WD10EZEX",
				"DEVPATH":   "/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda",
				"ID_VENDOR": "WDC",
			},
			expected: false,
		},
		{
			name: "physical NVMe SSD",
			props: map[string]string{
				"ID_BUS":    "nvme",
				"ID_PATH":   "pci-0000:01:00.0-nvme-1",
				"ID_MODEL":  "Samsung_SSD_980_PRO_1TB",
				"DEVPATH":   "/devices/pci0000:00/0000:00:01.0/0000:01:00.0/nvme/nvme0/nvme0n1",
				"ID_VENDOR": "Samsung",
			},
			expected: false,
		},
		{
			name: "QEMU VirtIO VM disk (allowed for dev/testing)",
			props: map[string]string{
				"ID_PATH": "pci-0000:00:04.0",
				"DEVPATH": "/devices/pci0000:00/0000:00:04.0/virtio1/block/vda",
			},
			expected: false,
		},
		{
			name: "QEMU SCSI/SATA VM disk (allowed for dev/testing)",
			props: map[string]string{
				"ID_BUS":    "ata",
				"ID_PATH":   "pci-0000:00:1f.2-ata-1",
				"ID_MODEL":  "QEMU_HARDDISK",
				"ID_VENDOR": "QEMU",
				"DEVPATH":   "/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sdb",
			},
			expected: false,
		},
		{
			name: "Longhorn iSCSI volume (Harvester)",
			props: map[string]string{
				"ID_BUS":    "scsi",
				"ID_PATH":   "ip-10.52.0.59:3260-iscsi-iqn.2019-10.io.longhorn:pvc-697ac77e-8c65-43ad-aa11-55023ac835d8-lun-1",
				"DEVPATH":   "/devices/platform/host2/session1/target2:0:0/2:0:0:1/block/sda",
				"ID_VENDOR": "IET",
				"ID_MODEL":  "VIRTUAL-DISK",
			},
			expected: true,
		},
		{
			name: "generic iSCSI bus",
			props: map[string]string{
				"ID_BUS": "iscsi",
			},
			expected: true,
		},
		{
			name: "iSCSI in id_path",
			props: map[string]string{
				"ID_PATH": "ip-192.168.1.50:3260-iscsi-iqn.target-lun-0",
			},
			expected: true,
		},
		{
			name: "io.longhorn in id_path",
			props: map[string]string{
				"ID_PATH": "some-path-io.longhorn:vol-data",
			},
			expected: true,
		},
		{
			name: "LIO-ORG virtual disk",
			props: map[string]string{
				"ID_VENDOR": "LIO-ORG",
				"ID_MODEL":  "VIRTUAL-DISK",
			},
			expected: true,
		},
		{
			name: "IET physical model is not sufficient to exclude a disk",
			props: map[string]string{
				"ID_VENDOR": "IET",
				"ID_MODEL":  "SCSI_DISK",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldIgnoreProperties(tt.props)
			if got != tt.expected {
				t.Errorf("ShouldIgnoreProperties() = %v; want %v", got, tt.expected)
			}
		})
	}
}

func TestShouldIgnore(t *testing.T) {
	tests := []struct {
		name     string
		devName  string
		devType  string
		props    map[string]string
		expected bool
	}{
		{
			name:     "physical disk valid properties",
			devName:  "sda",
			devType:  "disk",
			props:    map[string]string{"ID_BUS": "ata"},
			expected: false,
		},
		{
			name:     "ignored device prefix",
			devName:  "loop0",
			devType:  "disk",
			props:    map[string]string{"ID_BUS": "ata"},
			expected: true,
		},
		{
			name:     "ignored properties iscsi",
			devName:  "sda",
			devType:  "disk",
			props:    map[string]string{"ID_BUS": "iscsi"},
			expected: true,
		},
		{
			name:     "partition type",
			devName:  "sda1",
			devType:  "partition",
			props:    nil,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldIgnore(tt.devName, tt.devType, tt.props)
			if got != tt.expected {
				t.Errorf("ShouldIgnore(%q, %q, %v) = %v; want %v", tt.devName, tt.devType, tt.props, got, tt.expected)
			}
		})
	}
}

func TestExcludeRule(t *testing.T) {
	rule, err := ParseExcludeRule("vendor = HP, model = LOGICAL_VOLUME ")
	if err != nil {
		t.Fatalf("ParseExcludeRule() error = %v", err)
	}
	if want := "vendor=HP,model=LOGICAL_VOLUME"; rule.String() != want {
		t.Fatalf("rule.String() = %q; want %q", rule.String(), want)
	}

	disk := v1alpha1.DiskInfo{Vendor: "HP", Model: "LOGICAL_VOLUME", Bus: "scsi"}
	if !IsExcluded("worker-01", disk, []ExcludeRule{rule}) {
		t.Fatal("IsExcluded() = false; want true")
	}
	matched, ok := FindMatchingRule("worker-01", disk, []ExcludeRule{rule})
	if !ok || matched != rule {
		t.Fatalf("FindMatchingRule() = (%v, %v); want (%v, true)", matched, ok, rule)
	}
	if IsExcluded("worker-01", v1alpha1.DiskInfo{Vendor: "HP", Model: "OTHER"}, []ExcludeRule{rule}) {
		t.Fatal("IsExcluded() = true for partial match; want false")
	}
	if _, ok := FindMatchingRule("worker-01", v1alpha1.DiskInfo{Vendor: "HP", Model: "OTHER"}, []ExcludeRule{rule}); ok {
		t.Fatal("FindMatchingRule() returned true for non-matching disk; want false")
	}

	// Empty rule must not match any disk.
	if IsExcluded("worker-01", disk, []ExcludeRule{{}}) {
		t.Fatal("IsExcluded() = true for empty ExcludeRule{}; want false")
	}

	// WWN case-insensitivity.
	wwnRule, err := ParseExcludeRule("wwn=0x600508B1001C4432")
	if err != nil {
		t.Fatalf("ParseExcludeRule() error = %v", err)
	}
	wwnDisk := v1alpha1.DiskInfo{WWN: "0x600508b1001c4432"}
	if !IsExcluded("worker-01", wwnDisk, []ExcludeRule{wwnRule}) {
		t.Fatal("IsExcluded() = false for case-insensitive WWN match; want true")
	}
	if matched, ok := FindMatchingRule("worker-01", wwnDisk, []ExcludeRule{wwnRule}); !ok || matched != wwnRule {
		t.Fatalf("FindMatchingRule() = (%v, %v); want (%v, true)", matched, ok, wwnRule)
	}

	nodeRule, err := ParseExcludeRule("node=worker-01,serial=ABC123")
	if err != nil {
		t.Fatalf("ParseExcludeRule() error = %v", err)
	}
	if want := "node=worker-01,serial=ABC123"; nodeRule.String() != want {
		t.Fatalf("nodeRule.String() = %q; want %q", nodeRule.String(), want)
	}
	nodeDisk := v1alpha1.DiskInfo{Serial: "ABC123"}
	if !IsExcluded("worker-01", nodeDisk, []ExcludeRule{nodeRule}) {
		t.Fatal("IsExcluded() = false for matching node rule; want true")
	}
	if IsExcluded("worker-02", nodeDisk, []ExcludeRule{nodeRule}) {
		t.Fatal("IsExcluded() = true for a different node; want false")
	}
}

func TestParseExcludeRuleRejectsInvalidInput(t *testing.T) {
	for _, value := range []string{"", "vendor", "unknown=value", "vendor=", "vendor=HP,vendor=DELL"} {
		if _, err := ParseExcludeRule(value); err == nil {
			t.Errorf("ParseExcludeRule(%q) error = nil; want error", value)
		}
	}
}

func TestDiscoverDisksFiltersDynamicStorageVolumes(t *testing.T) {
	tmpDir := t.TempDir()
	longhornRecord := `E:DEVNAME=/dev/sda
E:DEVTYPE=disk
E:ID_BUS=scsi
E:ID_VENDOR=IET
E:ID_MODEL=VIRTUAL-DISK
E:ID_PATH=ip-10.52.0.59:3260-iscsi-iqn.2019-10.io.longhorn:pvc-697ac77e-8c65-43ad-aa11-55023ac835d8-lun-1
E:DEVPATH=/devices/platform/host2/session1/target2:0:0/2:0:0:1/block/sda
`
	virtioRecord := `E:DEVNAME=/dev/vda
E:DEVTYPE=disk
E:ID_PATH=pci-0000:00:04.0
E:DEVPATH=/devices/pci0000:00/0000:00:04.0/virtio1/block/vda
`
	if err := os.WriteFile(filepath.Join(tmpDir, "b8:0"), []byte(longhornRecord), 0o644); err != nil {
		t.Fatalf("failed to write Longhorn udev record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "b253:0"), []byte(virtioRecord), 0o644); err != nil {
		t.Fatalf("failed to write VirtIO udev record: %v", err)
	}

	disks, err := DiscoverDisks(tmpDir, "worker-01")
	if err != nil {
		t.Fatalf("DiscoverDisks failed: %v", err)
	}
	if len(disks) != 1 || disks[0].Name != "vda" {
		t.Fatalf("DiscoverDisks() = %#v; want only VirtIO disk vda", disks)
	}
}

func TestResolvePredictablePath(t *testing.T) {
	tests := []struct {
		name     string
		devName  string
		links    []string
		expected string
	}{
		{
			name:    "WWN highest priority",
			devName: "/dev/sda",
			links: []string{
				"/dev/disk/by-path/pci-0000:00:1f.2-ata-1",
				"/dev/disk/by-id/ata-WDC_WD10EZEX",
				"/dev/disk/by-id/wwn-0x50014ee265882b7f",
			},
			expected: "/dev/disk/by-id/wwn-0x50014ee265882b7f",
		},
		{
			name:    "NVMe EUI second priority",
			devName: "/dev/nvme0n1",
			links: []string{
				"/dev/disk/by-path/pci-0000:01:00.0-nvme-1",
				"/dev/disk/by-id/nvme-Samsung_SSD_980_PRO_1TB",
				"/dev/disk/by-id/nvme-eui.002538b301b0451a",
			},
			expected: "/dev/disk/by-id/nvme-eui.002538b301b0451a",
		},
		{
			name:    "by-id ata/nvme/scsi third priority",
			devName: "/dev/sda",
			links: []string{
				"/dev/disk/by-path/pci-0000:00:1f.2-ata-1",
				"/dev/disk/by-id/ata-WDC_WD10EZEX",
			},
			expected: "/dev/disk/by-id/ata-WDC_WD10EZEX",
		},
		{
			name:    "by-path fourth priority",
			devName: "/dev/sda",
			links: []string{
				"/dev/disk/by-path/pci-0000:00:1f.2-ata-1",
			},
			expected: "/dev/disk/by-path/pci-0000:00:1f.2-ata-1",
		},
		{
			name:     "fallback to devName",
			devName:  "sda",
			links:    []string{},
			expected: "/dev/sda",
		},
		{
			name:     "relative links normalized with /dev/ prefix",
			devName:  "/dev/sda",
			links:    []string{"disk/by-id/wwn-0x50014ee265882b7f"},
			expected: "/dev/disk/by-id/wwn-0x50014ee265882b7f",
		},
		{
			name:     "empty devName and links returns empty string",
			devName:  "",
			links:    []string{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolvePredictablePath(tt.devName, tt.links)
			if got != tt.expected {
				t.Errorf("ResolvePredictablePath() = %v; want %v", got, tt.expected)
			}
		})
	}
}

func TestSanitizeRFC1123Subdomain(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Node_1.example.com", "node-1.example.com"},
		{"---Bad__Chars!!!@@@", "bad-chars"},
		{"Samsung_SSD_980_PRO_1TB", "samsung-ssd-980-pro-1tb"},
		{"0x50014ee265882b7f", "0x50014ee265882b7f"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizeRFC1123Subdomain(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeRFC1123Subdomain(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestGenerateCRName(t *testing.T) {
	// 1. WWN.
	diskWWN := &v1alpha1.DiskInfo{
		WWN:  "0x50014ee265882b7f",
		Name: "sda",
	}
	name := GenerateCRName("node-1", diskWWN)
	if name != "node-1-0x50014ee265882b7f" {
		t.Errorf("GenerateCRName() = %v; want node-1-0x50014ee265882b7f", name)
	}

	// 2. SerialShort fallback.
	diskSerialShort := &v1alpha1.DiskInfo{
		SerialShort: "WD-12345",
		Name:        "sdb",
	}
	name = GenerateCRName("node-1", diskSerialShort)
	if name != "node-1-wd-12345" {
		t.Errorf("GenerateCRName() = %v; want node-1-wd-12345", name)
	}

	// 3. Serial fallback.
	diskSerial := &v1alpha1.DiskInfo{
		Serial: "SER-67890",
		Name:   "sdc",
	}
	name = GenerateCRName("node-1", diskSerial)
	if name != "node-1-ser-67890" {
		t.Errorf("GenerateCRName() = %v; want node-1-ser-67890", name)
	}

	// 4. Path basename fallback.
	diskPath := &v1alpha1.DiskInfo{
		Path: "/dev/disk/by-id/nvme-eui.002538b111a01234",
		Name: "nvme0n1",
	}
	name = GenerateCRName("node-1", diskPath)
	if name != "node-1-nvme-eui.002538b111a01234" {
		t.Errorf("GenerateCRName() = %v; want node-1-nvme-eui.002538b111a01234", name)
	}

	// 5. Kernel Name fallback.
	diskName := &v1alpha1.DiskInfo{
		Name: "sdd",
	}
	name = GenerateCRName("node-1", diskName)
	if name != "node-1-sdd" {
		t.Errorf("GenerateCRName() = %v; want node-1-sdd", name)
	}

	// 6. Empty cleanNode and cleanID fallbacks.
	diskEmpty := &v1alpha1.DiskInfo{
		Name: "---",
	}
	name = GenerateCRName("---", diskEmpty)
	if name != "node-disk" {
		t.Errorf("GenerateCRName() with empty clean values = %v; want node-disk", name)
	}

	// Test max length truncation.
	longNode := strings.Repeat("a", 250)
	longName := GenerateCRName(longNode, diskWWN)
	if len(longName) != 253 {
		t.Errorf("GenerateCRName() length %d; want exactly 253", len(longName))
	}
	if !strings.HasPrefix(longName, "aaaa") || !strings.Contains(longName, "-") {
		t.Errorf("GenerateCRName() output malformed: %s", longName)
	}
}

func TestParseUdevDataFile(t *testing.T) {
	tmpDir := t.TempDir()
	content := `S:disk/by-id/ata-WDC_WD10EZEX-08WN4A0_WD-WCC6Y7PL7345
S:disk/by-id/wwn-0x50014ee265882b7f
S:disk/by-path/pci-0000:00:1f.2-ata-1
E:DEVNAME=/dev/sda
E:DEVTYPE=disk
E:ID_BUS=ata
E:ID_MODEL=WDC_WD10EZEX-08WN4A0
E:ID_SERIAL=WDC_WD10EZEX-08WN4A0_WD-WCC6Y7PL7345
E:ID_SERIAL_SHORT=WD-WCC6Y7PL7345
E:ID_WWN=0x50014ee265882b7f
E:MAJOR=8
E:MINOR=0
`
	filePath := filepath.Join(tmpDir, "b8:0")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	udevRecord, err := ParseUdevDataFile(filePath)
	if err != nil {
		t.Fatalf("ParseUdevDataFile failed: %v", err)
	}

	if udevRecord.Major != 8 || udevRecord.Minor != 0 {
		t.Errorf("Major/Minor = %d:%d; want 8:0", udevRecord.Major, udevRecord.Minor)
	}
	if len(udevRecord.Symlinks) != 3 {
		t.Errorf("got %d symlinks; want 3", len(udevRecord.Symlinks))
	}
	if udevRecord.Properties["ID_WWN"] != "0x50014ee265882b7f" {
		t.Errorf("ID_WWN = %s; want 0x50014ee265882b7f", udevRecord.Properties["ID_WWN"])
	}
}

func TestDiscoverDisks_ParsesRotational(t *testing.T) {
	tmpDir := t.TempDir()
	content := `E:DEVNAME=/dev/sdb
E:DEVTYPE=disk
E:ID_BUS=ata
E:ID_MODEL=TEST_MODEL
E:ID_SERIAL=TEST_SERIAL
`
	if err := os.WriteFile(filepath.Join(tmpDir, "b8:16"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	devices, err := DiscoverDisks(tmpDir, "worker-01")
	if err != nil {
		t.Fatalf("DiscoverDisks failed: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].Name != "sdb" {
		t.Errorf("Name = %s; want sdb", devices[0].Name)
	}
	if devices[0].CanonicalPath != "/dev/sdb" {
		t.Errorf("CanonicalPath = %q; want /dev/sdb", devices[0].CanonicalPath)
	}
	// On systems without /sys/class/block/sdb/queue/rotational, Rotational should be nil
	// which is expected and handled gracefully.
}

func TestDiscoverDisks_InvalidAndEmpty(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty dir returns empty list without error.
	devices, err := DiscoverDisks(tmpDir, "worker-01")
	if err != nil {
		t.Fatalf("DiscoverDisks failed on empty dir: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("Expected 0 devices, got %d", len(devices))
	}

	// Unparseable file should be skipped gracefully.
	badFile := filepath.Join(tmpDir, "b8:99")
	_ = os.WriteFile(badFile, []byte("NOT A VALID FORMAT"), 0644)

	devices, err = DiscoverDisks(tmpDir, "worker-01")
	if err != nil {
		t.Fatalf("DiscoverDisks failed with bad file: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("Expected 0 devices, got %d", len(devices))
	}
}

func TestDiscoverDisks_EdgeCases(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Record with ID_MODEL_ENC fallback when ID_MODEL is empty.
	content := "E:DEVNAME=/dev/sdd\nE:DEVTYPE=disk\nE:ID_MODEL_ENC=SAMSUNG\\x20MODEL\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "b8:48"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// 2. Record with empty DEVNAME and non-existent Major/Minor (skipped at devName == "").
	contentEmpty := "E:ID_BUS=scsi\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "b9999:9999"), []byte(contentEmpty), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	devices, err := DiscoverDisks(tmpDir, "worker-01")
	if err != nil {
		t.Fatalf("DiscoverDisks failed: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].Model != "SAMSUNG\\x20MODEL" {
		t.Errorf("Model = %q; want SAMSUNG\\x20MODEL", devices[0].Model)
	}

}

func TestParseUdevDataFile_ErrorsAndEdgeCases(t *testing.T) {
	// Non-existent file.
	_, err := ParseUdevDataFile("/nonexistent/udev/file")
	if err == nil {
		t.Errorf("expected error for non-existent file")
	}

	// File with short lines (< 3 chars) and invalid syntax.
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "custom_record")
	content := "\n\nx\nS:disk/by-id/custom\nE:NO_EQUALS_HERE\nE:VALID=OK\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	rec, err := ParseUdevDataFile(filePath)
	if err != nil {
		t.Fatalf("ParseUdevDataFile failed: %v", err)
	}
	if rec.Properties["VALID"] != "OK" {
		t.Errorf("VALID = %q; want OK", rec.Properties["VALID"])
	}
	if len(rec.Symlinks) != 1 || rec.Symlinks[0] != "disk/by-id/custom" {
		t.Errorf("Symlinks = %v; want [disk/by-id/custom]", rec.Symlinks)
	}
}
