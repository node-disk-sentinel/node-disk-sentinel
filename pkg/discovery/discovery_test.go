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

func TestShouldIgnoreByName(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"sda", false},
		{"nvme0n1", false},
		{"vda", false},
		{"mmcblk0", false},
		{"loop0", true},
		{"ram1", true},
		{"zram0", true},
		{"dm-0", true},
		{"md0", true},
		{"sr0", true},
		{"rbd0", true},
		{"drbd1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldIgnore(tt.name, nil); got != tt.expected {
				t.Errorf("ShouldIgnore(%q) = %v; want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestShouldIgnoreByProperties(t *testing.T) {
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
			got := ShouldIgnore("sda", tt.props)
			if got != tt.expected {
				t.Errorf("ShouldIgnore() = %v; want %v", got, tt.expected)
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
	filePath := filepath.Join(t.TempDir(), "b8:0")
	content := "\n\nx\n" + // short lines are skipped
		"S:disk/by-id/ata-WDC_WD10EZEX-08WN4A0_WD-WCC6Y7PL7345\n" +
		"S:disk/by-id/wwn-0x50014ee265882b7f\n" +
		"E:ID_MODEL=WDC_WD10EZEX-08WN4A0\n" +
		"E:ID_WWN=0x50014ee265882b7f\n" +
		"E:NO_EQUALS_HERE\n" +
		"G:systemd\nQ:systemd\nV:1\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	record, err := ParseUdevDataFile(filePath)
	if err != nil {
		t.Fatalf("ParseUdevDataFile failed: %v", err)
	}
	if len(record.Symlinks) != 2 || record.Symlinks[1] != "disk/by-id/wwn-0x50014ee265882b7f" {
		t.Errorf("Symlinks = %v; want 2 entries relative to /dev", record.Symlinks)
	}
	if record.Properties["ID_WWN"] != "0x50014ee265882b7f" || record.Properties["ID_MODEL"] != "WDC_WD10EZEX-08WN4A0" {
		t.Errorf("Properties = %v", record.Properties)
	}
	if len(record.Properties) != 2 {
		t.Errorf("got %d properties; want 2 (malformed and non-E lines ignored)", len(record.Properties))
	}

	if _, err := ParseUdevDataFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

// fakeHost builds a sysfs tree and a udev database in temporary directories.
type fakeHost struct {
	t            *testing.T
	sysDir, udev string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, sysDir: t.TempDir(), udev: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(h.sysDir, "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *fakeHost) write(path, content string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// addDisk creates /sys/block/<name> (a symlink into /sys/devices, like the
// kernel does) with the given sysfs attributes and, if record is not empty, the
// udev record for majorMinor.
func (h *fakeHost) addDisk(name, majorMinor, record string, attrs map[string]string) {
	h.t.Helper()
	kobject := filepath.Join(h.sysDir, "devices", "pci0000:00", "block", name)
	h.write(filepath.Join(kobject, "dev"), majorMinor+"\n")
	for file, value := range attrs {
		h.write(filepath.Join(kobject, file), value)
	}
	if err := os.Symlink(filepath.Join("..", "devices", "pci0000:00", "block", name), filepath.Join(h.sysDir, "block", name)); err != nil {
		h.t.Fatal(err)
	}
	if record != "" {
		h.write(filepath.Join(h.udev, "b"+majorMinor), record)
	}
}

func (h *fakeHost) discover(rules ...ExcludeRule) []v1alpha1.DiskInfo {
	h.t.Helper()
	disks, err := DiscoverDisks(h.sysDir, h.udev, "worker-01", rules...)
	if err != nil {
		h.t.Fatalf("DiscoverDisks failed: %v", err)
	}
	return disks
}

const sataRecord = `S:disk/by-path/pci-0000:00:1f.2-ata-1
S:disk/by-id/wwn-0x500253855031cfa5
E:ID_BUS=ata
E:ID_MODEL=Samsung_SSD_840_PRO_Series
E:ID_SERIAL=Samsung_SSD_840_PRO_Series_S1ATNEAD511307R
E:ID_SERIAL_SHORT=S1ATNEAD511307R
E:ID_WWN=0x500253855031cfa5
E:ID_PATH=pci-0000:00:1f.2-ata-1
E:ID_REVISION=DXM06B0Q
`

func TestDiscoverDisks(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, map[string]string{"size": "1000\n", "queue/rotational": "0\n"})

	disks := h.discover()
	if len(disks) != 1 {
		t.Fatalf("got %d disks; want 1", len(disks))
	}
	d := disks[0]
	if d.Name != "sda" || d.CanonicalPath != "/dev/sda" || d.Type != "disk" {
		t.Errorf("identity = %q %q %q", d.Name, d.CanonicalPath, d.Type)
	}
	if d.Path != "/dev/disk/by-id/wwn-0x500253855031cfa5" {
		t.Errorf("Path = %q; want the wwn symlink", d.Path)
	}
	if d.Major != 8 || d.Minor != 0 {
		t.Errorf("Major/Minor = %d:%d; want 8:0", d.Major, d.Minor)
	}
	if d.SysPath != "/devices/pci0000:00/block/sda" {
		t.Errorf("SysPath = %q", d.SysPath)
	}
	if d.Capacity != 512000 {
		t.Errorf("Capacity = %d; want 512000 (sectors * 512)", d.Capacity)
	}
	if d.Rotational == nil || *d.Rotational {
		t.Errorf("Rotational = %v; want false", d.Rotational)
	}
	if d.WWN != "0x500253855031cfa5" || d.SerialShort != "S1ATNEAD511307R" || d.FirmwareVersion != "DXM06B0Q" || d.Bus != "ata" {
		t.Errorf("udev properties not mapped: %+v", d)
	}
	if len(d.Links) != 2 {
		t.Errorf("Links = %v; want 2", d.Links)
	}
}

func TestDiscoverDisksSkipsWhatIsNotAMonitoredDisk(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, nil)
	h.addDisk("loop0", "7:0", "E:ID_FS_TYPE=squashfs\n", nil)
	h.addDisk("sdb", "8:16", "", nil) // not yet processed by udev
	h.addDisk("sdc", "8:32", "E:ID_BUS=scsi\nE:ID_VENDOR=IET\nE:ID_MODEL=VIRTUAL-DISK\nE:ID_PATH=ip-10.0.0.1:3260-iscsi-iqn.2019-10.io.longhorn:pvc-1-lun-1\n", nil)
	h.addDisk("vda", "253:0", "E:ID_PATH=pci-0000:00:04.0\n", nil) // VM disk without most properties
	// sysfs-rules.rst treats /sys/block and /sys/class/block as interchangeable,
	// and the latter lists partitions, so a partition in the listing must be
	// skipped by its "partition" attribute.
	h.addDisk("sda1", "8:1", sataRecord, map[string]string{"partition": "1\n"})

	var names []string
	for _, d := range h.discover() {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "sda,vda" {
		t.Errorf("discovered %v; want [sda vda]", names)
	}
}

func TestDiscoverDisksToleratesMissingSysfsAttributes(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("vda", "253:0", "E:ID_PATH=pci-0000:00:04.0\n", map[string]string{"size": "garbage"})

	disks := h.discover()
	if len(disks) != 1 {
		t.Fatalf("got %d disks; want 1", len(disks))
	}
	if disks[0].Capacity != 0 || disks[0].Rotational != nil {
		t.Errorf("Capacity = %d, Rotational = %v; want unknown values", disks[0].Capacity, disks[0].Rotational)
	}
	if disks[0].Path != "/dev/vda" {
		t.Errorf("Path = %q; want the kernel path without udev symlinks", disks[0].Path)
	}
}

func TestDiscoverDisksSkipsBrokenEntries(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, nil)
	h.addDisk("sdb", "garbage", "", nil)
	if err := os.Symlink("nowhere", filepath.Join(h.sysDir, "block", "sdc")); err != nil { // vanished mid-scan
		t.Fatal(err)
	}

	if disks := h.discover(); len(disks) != 1 || disks[0].Name != "sda" {
		t.Errorf("DiscoverDisks() = %+v; want only sda", disks)
	}
}

func TestDiscoverDisksAppliesExcludeRules(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, nil)

	if got := h.discover(ExcludeRule{WWN: "0X500253855031CFA5"}); len(got) != 0 {
		t.Errorf("got %d disks; want the excluded disk to be dropped", len(got))
	}
	if got := h.discover(ExcludeRule{Vendor: "other"}); len(got) != 1 {
		t.Errorf("got %d disks; want a non-matching rule to keep the disk", len(got))
	}
}

func TestDiscoverDisksMissingSysfs(t *testing.T) {
	if _, err := DiscoverDisks(t.TempDir(), t.TempDir(), "worker-01"); err == nil {
		t.Error("expected an error when <sys>/block does not exist")
	}
}

// Without the udev database every disk would look unknown, which must not be
// mistaken for "every disk vanished": the caller would flag all of them missing.
func TestDiscoverDisksFailsWithoutUdevDatabase(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, nil)
	h.addDisk("loop0", "7:0", "E:ID_FS_TYPE=squashfs\n", nil)

	tests := []struct {
		name   string
		udev   string
		remove []string // records to delete first
	}{
		{"directory does not exist", filepath.Join(h.udev, "not-mounted"), nil},
		// containerd creates a missing hostPath as an empty directory.
		{"directory is empty", h.udev, []string{"b8:0", "b7:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, record := range tt.remove {
				if err := os.Remove(filepath.Join(h.udev, record)); err != nil {
					t.Fatal(err)
				}
			}
			if disks, err := DiscoverDisks(h.sysDir, tt.udev, "worker-01"); err == nil {
				t.Errorf("DiscoverDisks() = %+v, nil; want an error", disks)
			}
		})
	}
}

// A disk without a record is only skipped as long as udev has records for others.
func TestDiscoverDisksSkipsDiskWithoutRecordIfDatabaseIsPopulated(t *testing.T) {
	h := newFakeHost(t)
	h.addDisk("sda", "8:0", sataRecord, nil)
	h.addDisk("sdb", "8:16", "", nil)

	if disks := h.discover(); len(disks) != 1 || disks[0].Name != "sda" {
		t.Errorf("DiscoverDisks() = %+v; want only sda", disks)
	}
}

func TestDiscoverDisksWithoutBlockDevices(t *testing.T) {
	h := newFakeHost(t)
	if disks := h.discover(); len(disks) != 0 {
		t.Errorf("DiscoverDisks() = %+v; want none", disks)
	}
}

func TestDiscoverDisksConvertsSysfsNameToDeviceNode(t *testing.T) {
	h := newFakeHost(t)
	// The kernel shows /dev/cciss/c0d0 as "cciss!c0d0" in sysfs.
	h.addDisk("cciss!c0d0", "104:0", "E:ID_BUS=scsi\n", nil)

	disks := h.discover()
	if len(disks) != 1 {
		t.Fatalf("got %d disks; want 1", len(disks))
	}
	if disks[0].CanonicalPath != "/dev/cciss/c0d0" || disks[0].Path != "/dev/cciss/c0d0" {
		t.Errorf("CanonicalPath = %q, Path = %q; want /dev/cciss/c0d0", disks[0].CanonicalPath, disks[0].Path)
	}
	if disks[0].Name != "cciss!c0d0" {
		t.Errorf("Name = %q; want the sysfs name", disks[0].Name)
	}
}
