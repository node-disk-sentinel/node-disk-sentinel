// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

const (
	defaultSysDir  = "/sys"
	defaultUdevDir = "/run/udev/data"
)

// UdevRecord holds what udev stored for one block device in /run/udev/data.
type UdevRecord struct {
	// Properties are the "E:KEY=VALUE" entries set by udev rules (ID_MODEL, ID_WWN, ...).
	// Kernel properties such as DEVNAME, DEVTYPE or MAJOR are not stored there.
	Properties map[string]string
	// Symlinks are the "S:" entries, relative to /dev (e.g. "disk/by-id/wwn-0x...").
	Symlinks []string
}

// ParseUdevDataFile reads a udev database record such as /run/udev/data/b8:0.
// The format is internal to udev (see device_update_db in systemd's
// sd-device), so unknown tags and malformed lines are skipped.
func ParseUdevDataFile(path string) (*UdevRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	record := &UdevRecord{Properties: make(map[string]string)}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "S:"):
			record.Symlinks = append(record.Symlinks, line[2:])
		case strings.HasPrefix(line, "E:"):
			if k, v, ok := strings.Cut(line[2:], "="); ok && k != "" {
				record.Properties[k] = v
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return record, nil
}

// DiscoverDisks returns the physical disks of nodeName that are not excluded.
//
// Like lsblk it walks <sysDir>/block. Each entry's "dev" file holds
// "major:minor", which names the udev record <udevDir>/b<major>:<minor> with the
// ID_* properties and the persistent /dev symlinks. A disk without a record is
// skipped: udev has not processed it yet and will announce it with an event. If
// no disk has a record, the udev database itself is unavailable and DiscoverDisks
// fails instead of reporting an empty inventory.
//
// Partitions are skipped by their "partition" attribute. /sys/block lists only
// whole disks today, but sysfs-rules.rst (Documentation/admin-guide) treats
// /sys/block and /sys/class/block as interchangeable, and the latter lists
// partitions next to their disk.
func DiscoverDisks(sysDir, udevDir, nodeName string, excludeRules ...ExcludeRule) ([]v1alpha1.DiskInfo, error) {
	if sysDir == "" {
		sysDir = defaultSysDir
	}
	if udevDir == "" {
		udevDir = defaultUdevDir
	}

	blockDir := filepath.Join(sysDir, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list block devices: %w", err)
	}

	var (
		disks     []v1alpha1.DiskInfo
		whole     int   // whole disks found in sysfs
		recorded  int   // of those, the ones that have a udev record
		recordErr error // why the first record could not be read
	)
	for _, entry := range entries {
		name := entry.Name()
		sysPath := filepath.Join(blockDir, name)

		// A disk can vanish at any point of the scan (hot unplug); skip it then.
		devNum, err := os.ReadFile(filepath.Join(sysPath, "dev"))
		if err != nil {
			continue
		}
		var major, minor int
		if _, err := fmt.Sscanf(string(devNum), "%d:%d", &major, &minor); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(sysPath, "partition")); err == nil {
			continue
		}
		whole++
		record, err := ParseUdevDataFile(filepath.Join(udevDir, fmt.Sprintf("b%d:%d", major, minor)))
		if err != nil {
			if recordErr == nil {
				recordErr = err
			}
			continue
		}
		recorded++
		if ShouldIgnore(name, record.Properties) {
			continue
		}

		// The kernel replaces "/" in a device name by "!" in sysfs ("cciss!c0d0"
		// is /dev/cciss/c0d0).
		devName := strings.ReplaceAll(name, "!", "/")
		props := record.Properties
		disk := v1alpha1.DiskInfo{
			Path:            ResolvePredictablePath(devName, record.Symlinks),
			CanonicalPath:   "/dev/" + devName,
			Name:            name,
			SysPath:         sysDevicePath(sysPath),
			Links:           record.Symlinks,
			Major:           major,
			Minor:           minor,
			Type:            "disk",
			Bus:             props["ID_BUS"],
			Model:           props["ID_MODEL"],
			Vendor:          props["ID_VENDOR"],
			Serial:          props["ID_SERIAL"],
			SerialShort:     props["ID_SERIAL_SHORT"],
			WWN:             props["ID_WWN"],
			BusPath:         props["ID_PATH"],
			Capacity:        capacityBytes(sysPath),
			Rotational:      rotational(sysPath),
			FirmwareVersion: props["ID_REVISION"],
		}
		if !IsExcluded(nodeName, disk, excludeRules) {
			disks = append(disks, disk)
		}
	}

	// Without the database every disk would look unknown, and the caller would
	// flag all of them as missing. Every block device that udev has processed has
	// a record, loop devices included, so finding none means the database is not
	// mounted or not populated yet. Checking only for the directory is not enough:
	// containerd creates a missing hostPath as an empty directory.
	if whole > 0 && recorded == 0 {
		return nil, fmt.Errorf("udev database not available: none of the %d block devices has a record in %s: %w", whole, udevDir, recordErr)
	}
	return disks, nil
}

// sysDevicePath returns the kobject path ("/devices/pci.../block/sda") that the
// /sys/block/<name> symlink points to ("../devices/pci.../block/sda"), or "" if
// it is not a symlink.
func sysDevicePath(blockEntry string) string {
	target, err := os.Readlink(blockEntry)
	if err != nil {
		return ""
	}
	return path.Join("/block", target)
}

// capacityBytes reads the disk size. The kernel always reports it in 512-byte
// sectors, whatever the physical sector size is. Unknown stays 0 rather than
// hiding the disk: sysfs can vanish partway through a hot unplug.
func capacityBytes(blockEntry string) int64 {
	data, err := os.ReadFile(filepath.Join(blockEntry, "size"))
	if err != nil {
		return 0
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return sectors * 512
}

// rotational reports whether the disk has spinning platters, as the kernel
// states in queue/rotational. It returns nil if the attribute is unreadable or
// holds neither 0 nor 1.
func rotational(blockEntry string) *bool {
	data, err := os.ReadFile(filepath.Join(blockEntry, "queue", "rotational"))
	if err != nil {
		return nil
	}
	switch strings.TrimSpace(string(data)) {
	case "1":
		return new(true)
	case "0":
		return new(false)
	}
	return nil
}
