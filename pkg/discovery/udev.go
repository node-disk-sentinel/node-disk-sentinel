// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package discovery handles host disk detection by parsing udev's runtime database
// and sysfs attributes directly, avoiding any reliance on external CLI tools like 'udevadm'.
//
// ============================================================================
// Linux udev Runtime Database & sysfs Architecture
// ============================================================================
//
// 1. The /run/udev/data Runtime Database:
//   - On modern Linux distributions using systemd, 'systemd-udevd' manages a
//     high-speed in-memory database located at /run/udev/data (mounted on tmpfs;
//     historically /dev/.udev/db on pre-systemd distributions).
//   - Why read files directly instead of calling 'udevadm info'?
//     Executing 'udevadm' in a subprocess for every disk forks a process, dynamically
//     links libraries, and parses output on stdout. On large storage nodes with dozens
//     or hundreds of disks, this incurs significant CPU and latency overhead.
//     Furthermore, in containerized Kubernetes DaemonSets (especially distroless or
//     scratch images), the 'udevadm' binary is often absent. By mounting the host's
//     /run/udev/data read-only into the container, node-disk-sentinel discovers all
//     hardware metadata in microseconds with zero binary dependencies.
//   - This is a runtime cache, not durable inventory. Because /run is tmpfs, udev
//     rebuilds it after each host boot. A record can also disappear while a scan is
//     in progress during hot removal. Discovery therefore treats individual unreadable
//     records as transient and relies on the next event or periodic scan to converge.
//
// 2. File Naming Convention: b<major>:<minor> vs c<major>:<minor>:
//
//   - The Linux kernel uniquely addresses device nodes using a (type, major, minor) tuple:
//     'b' indicates a block device (disks, partitions, loopback).
//     'c' indicates a character device (ttys, mice, hardware random generators).
//     'major' identifies the kernel device driver/subsystem:
//     8   = SCSI disk subsystem (SATA, SAS, USB mass storage: /dev/sd*)
//     259 = NVMe subsystem (/dev/nvme*n*)
//     254 = Device Mapper (/dev/dm-*)
//     'minor' identifies the specific drive or partition instance within that driver.
//
//   - Consequently, udev names each database file after this exact tuple:
//     /run/udev/data/b8:0   -> Block device 8:0 (/dev/sda - whole disk)
//     /run/udev/data/b8:1   -> Block device 8:1 (/dev/sda1 - first partition)
//     /run/udev/data/b259:0 -> Block device 259:0 (/dev/nvme0n1 - NVMe namespace)
//
//     3. Database Syntax (Tag Prefixes):
//     Each line in a /run/udev/data file begins with a single-character tag and colon:
//
//   - S:<symlink>
//     Persistent symlinks created under /dev for this device, stored relative to /dev
//     (e.g. "S:disk/by-id/ata-WDC_WD10EZEX...", "S:disk/by-path/pci-...").
//
//   - E:<KEY>=<VALUE>
//     Environment variables populated by udev rules (e.g. ata_id, scsi_id, blkid):
//     DEVNAME, DEVTYPE, ID_BUS, ID_MODEL, ID_SERIAL, ID_WWN, etc.
//
//   - G:<tag>, W:<watch>, A:<attr>
//     Tags, inotify watch descriptors, and cached sysfs attributes (ignored by our parser).
//
// 4. Sysfs Fallbacks (/sys/dev/block/<major>:<minor>):
//   - In virtualized or cloud environments (QEMU/KVM with VirtIO-blk, cloud-init,
//     or trimmed container rootfs), udev records can occasionally omit DEVNAME or DEVTYPE.
//   - Linux sysfs exposes a deterministic lookup directory at /sys/dev/block/<major>:<minor>.
//     This is a symlink resolving to the full kobject path under /sys/devices/.../block/<name>.
//     By resolving this link with filepath.EvalSymlinks, we can reliably recover the
//     canonical device name and inspect sysfs attributes directly.
package discovery

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

// UdevRecord contains the raw key-value pairs and persistent symlinks parsed
// from a single udev database record (/run/udev/data/b<major>:<minor>).
type UdevRecord struct {
	// Major is the kernel device driver subsystem ID (e.g. 8 for SCSI/SATA, 259 for NVMe).
	Major int
	// Minor is the individual device or partition instance number.
	Minor int
	// Properties contains all "E:KEY=VALUE" variables exported by udev rules.
	Properties map[string]string
	// Symlinks contains all "S:<symlink>" relative paths created under /dev.
	Symlinks []string
}

// ParseUdevDataFile reads and parses a single udev database record file (e.g. /run/udev/data/b8:0).
func ParseUdevDataFile(path string) (*UdevRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	udevRecord := &UdevRecord{
		Properties: make(map[string]string),
		Symlinks:   make([]string, 0),
	}

	// Filenames in /run/udev/data follow the format: b<major>:<minor> (for block devices)
	// Example: "b8:0" -> Major: 8, Minor: 0.
	base := filepath.Base(path)
	if majorStr, minorStr, ok := strings.Cut(strings.TrimPrefix(base, "b"), ":"); ok && strings.HasPrefix(base, "b") {
		udevRecord.Major, _ = strconv.Atoi(majorStr)
		udevRecord.Minor, _ = strconv.Atoi(minorStr)
	}

	// Line-by-line parser for udev internal database syntax. udev writes this
	// implementation detail, not a public transactional API, so tolerate unknown
	// tags and malformed individual fields rather than failing the complete inventory.
	//   S:<relative-symlink-under-/dev>
	//   E:<KEY>=<VALUE>
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) < 3 {
			continue
		}

		switch line[:2] {
		case "S:":
			// Symlink entry: e.g. "S:disk/by-id/wwn-0x50014ee265882b7f".
			udevRecord.Symlinks = append(udevRecord.Symlinks, line[2:])
		case "E:":
			// Environment property entry: e.g. "E:ID_MODEL=Samsung_SSD_980_PRO_1TB".
			if k, v, ok := strings.Cut(line[2:], "="); ok && k != "" {
				udevRecord.Properties[k] = v
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return udevRecord, nil
}

// DiscoverDisks scans the udev data directory (typically /run/udev/data) and returns DiskInfo
// for all qualifying physical disks on nodeName.
func DiscoverDisks(udevDir, nodeName string, excludeRules ...ExcludeRule) ([]v1alpha1.DiskInfo, error) {
	if udevDir == "" {
		udevDir = "/run/udev/data"
	}

	// Glob all block device entries ("b*"). This matches "b8:0", "b259:0", etc.,
	// while ignoring character devices ("c*") and metadata files.
	matches, err := filepath.Glob(filepath.Join(udevDir, "b*"))
	if err != nil {
		return nil, fmt.Errorf("failed to scan udev data dir: %w", err)
	}

	var diskInfos []v1alpha1.DiskInfo

	for _, udevPath := range matches {
		udevRecord, err := ParseUdevDataFile(udevPath)
		if err != nil {
			// The record may have been removed between Glob and Open by a hot-unplug.
			// Skipping one record is safe because the listener and the next poll repair
			// the inventory; aborting would unnecessarily discard all other disks.
			continue
		}

		devType := udevRecord.Properties["DEVTYPE"]
		devName := udevRecord.Properties["DEVNAME"]

		// FALLBACK FOR VIRTUALIZED HARDWARE (VirtIO / QEMU / minimal udev):
		// In some virtual machines or minimal OS images, udev records might not contain
		// DEVNAME or DEVTYPE. When that occurs, we resolve the device via sysfs:
		// /sys/dev/block/<major>:<minor> is a kernel symlink pointing to the real kobject path
		// (e.g. /sys/devices/pci0000:00/0000:00:04.0/virtio1/block/vda).
		if devName == "" && udevRecord.Major > 0 {
			sysDevLink := fmt.Sprintf("/sys/dev/block/%d:%d", udevRecord.Major, udevRecord.Minor)
			if target, err := filepath.EvalSymlinks(sysDevLink); err == nil {
				base := filepath.Base(target)
				devName = "/dev/" + base
				if devType == "" {
					// How to distinguish a partition from a whole disk in sysfs:
					// In Linux sysfs, a block device directory contains a file named "partition"
					// (which holds the partition index number, e.g. "1") IF AND ONLY IF it is a partition.
					// Whole physical disks do NOT contain this file.
					if _, err := os.Stat(filepath.Join(target, "partition")); err == nil {
						devType = "partition"
					} else {
						devType = "disk"
					}
				}
				if udevRecord.Properties["DEVPATH"] == "" {
					udevRecord.Properties["DEVPATH"] = strings.TrimPrefix(target, "/sys")
				}
			}
		}

		if devName == "" {
			// A record without a resolvable device node cannot be passed to smartctl and
			// cannot be correlated to a PhysicalDisk. It may be an incomplete udev rule
			// result, so leave it for a later event/poll instead of manufacturing a name.
			continue
		}

		baseName := filepath.Base(devName)
		// Discard non-physical block devices (loop, ram, zram, dm-crypt, md raid, cdrom),
		// partition devices (sda1, nvme0n1p1), and dynamic storage volumes (Longhorn, iSCSI).
		if ShouldIgnore(baseName, devType, udevRecord.Properties) {
			continue
		}

		// DISK CAPACITY DETECTION FROM SYSFS:
		// /sys/class/block/<name>/size contains the total capacity in sectors.
		//
		// INSIDER KNOWLEDGE:
		// In the Linux kernel block layer, the 'size' sysfs attribute is ALWAYS represented
		// in units of 512-byte sectors (KERNEL_SECTOR_SIZE = 512), regardless of whether the
		// underlying physical storage medium uses 4096-byte (4Kn Advanced Format) physical sectors
		// or 512e logical emulation!
		// Multiplying by 512 universally yields the correct capacity in bytes.
		var capacity int64
		sysSizePath := fmt.Sprintf("/sys/class/block/%s/size", baseName)
		if data, err := os.ReadFile(sysSizePath); err == nil {
			if blocks, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
				capacity = blocks * 512
			}
		}
		// A missing or unparsable size remains 0 (unknown). This intentionally does
		// not suppress the disk: sysfs can vanish partway through a hot-unplug, while
		// the udev record is still visible for a short time.

		// ROTATIONAL MEDIA (HDD vs SSD) DETECTION:
		// /sys/class/block/<name>/queue/rotational exposes the media type:
		//   '1' = Rotational media (mechanical spinning platter HDD).
		//   '0' = Non-rotational media (solid-state drive: SATA SSD, NVMe, Optane).
		// Note: Virtualized disks or RAID controllers without pass-through might omit this attribute.
		var rotational *bool
		sysRotPath := fmt.Sprintf("/sys/class/block/%s/queue/rotational", baseName)
		if data, err := os.ReadFile(sysRotPath); err == nil {
			switch strings.TrimSpace(string(data)) {
			case "1":
				rotational = new(true)
			case "0":
				rotational = new(false)
			}
		}

		// PREDICTABLE PATH RESOLUTION:
		// Kernel device names like /dev/sda are unstable and can change across reboots
		// or PCI bus enumerations. We select the most immutable path (e.g. /dev/disk/by-id/wwn-*).
		predictablePath := ResolvePredictablePath(devName, udevRecord.Symlinks)

		// Model fallback: ID_MODEL or ID_MODEL_ENC
		// ID_MODEL_ENC contains the model string hex-encoded by udev rules if the drive
		// returns unprintable or special whitespace characters in its ATA/SCSI inquiry.
		model := udevRecord.Properties["ID_MODEL"]
		if model == "" {
			model = udevRecord.Properties["ID_MODEL_ENC"]
		}

		diskInfo := v1alpha1.DiskInfo{
			Path:            predictablePath,
			CanonicalPath:   "/dev/" + baseName,
			Name:            baseName,
			SysPath:         udevRecord.Properties["DEVPATH"],
			Links:           udevRecord.Symlinks,
			Major:           udevRecord.Major,
			Minor:           udevRecord.Minor,
			Type:            devType,
			Bus:             udevRecord.Properties["ID_BUS"],
			Model:           model,
			Vendor:          udevRecord.Properties["ID_VENDOR"],
			Serial:          udevRecord.Properties["ID_SERIAL"],
			SerialShort:     udevRecord.Properties["ID_SERIAL_SHORT"],
			WWN:             udevRecord.Properties["ID_WWN"],
			BusPath:         udevRecord.Properties["ID_PATH"],
			Capacity:        capacity,
			Rotational:      rotational,
			FirmwareVersion: udevRecord.Properties["ID_REVISION"],
		}
		if IsExcluded(nodeName, diskInfo, excludeRules) {
			continue
		}
		diskInfos = append(diskInfos, diskInfo)
	}

	return diskInfos, nil
}
