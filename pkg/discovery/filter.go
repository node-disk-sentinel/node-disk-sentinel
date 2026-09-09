// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package discovery provides filtering rules to separate real, physical drives
// from virtual block devices, software abstractions, and drive partitions.
package discovery

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

// ExcludeRule identifies disks that must not be monitored. Every populated
// field must match a disk; multiple rules are evaluated as alternatives.
type ExcludeRule struct {
	Node   string
	Name   string
	Vendor string
	Model  string
	Serial string
	WWN    string
	Bus    string
}

// Empty reports whether none of the rule's criteria are populated.
func (r ExcludeRule) Empty() bool {
	return r.Node == "" && r.Name == "" && r.Vendor == "" && r.Model == "" &&
		r.Serial == "" && r.WWN == "" && r.Bus == ""
}

// String returns the comma-separated key=value representation of the exclusion rule.
func (r ExcludeRule) String() string {
	var parts []string
	if r.Node != "" {
		parts = append(parts, "node="+r.Node)
	}
	if r.Name != "" {
		parts = append(parts, "name="+r.Name)
	}
	if r.Vendor != "" {
		parts = append(parts, "vendor="+r.Vendor)
	}
	if r.Model != "" {
		parts = append(parts, "model="+r.Model)
	}
	if r.Serial != "" {
		parts = append(parts, "serial="+r.Serial)
	}
	if r.WWN != "" {
		parts = append(parts, "wwn="+r.WWN)
	}
	if r.Bus != "" {
		parts = append(parts, "bus="+r.Bus)
	}
	return strings.Join(parts, ",")
}

// Matches reports whether disk on nodeName matches the non-empty exclusion rule.
func (r ExcludeRule) Matches(nodeName string, disk v1alpha1.DiskInfo) bool {
	if r.Empty() {
		return false
	}
	return (r.Node == "" || r.Node == nodeName) &&
		(r.Name == "" || r.Name == disk.Name) &&
		(r.Vendor == "" || r.Vendor == disk.Vendor) &&
		(r.Model == "" || r.Model == disk.Model) &&
		(r.Serial == "" || r.Serial == disk.Serial) &&
		(r.WWN == "" || strings.EqualFold(r.WWN, disk.WWN)) &&
		(r.Bus == "" || r.Bus == disk.Bus)
}

// ParseExcludeRule parses a comma-separated list of exact matches, for example
// "vendor=HP,model=LOGICAL_VOLUME".
func ParseExcludeRule(value string) (ExcludeRule, error) {
	var rule ExcludeRule
	for _, part := range strings.Split(value, ",") {
		key, val, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !ok || key == "" || val == "" {
			return ExcludeRule{}, fmt.Errorf("invalid exclusion %q; expected key=value pairs", part)
		}

		switch key {
		case "node":
			if rule.Node != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Node = val
		case "name":
			if rule.Name != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Name = val
		case "vendor":
			if rule.Vendor != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Vendor = val
		case "model":
			if rule.Model != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Model = val
		case "serial":
			if rule.Serial != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Serial = val
		case "wwn":
			if rule.WWN != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.WWN = val
		case "bus":
			if rule.Bus != "" {
				return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
			}
			rule.Bus = val
		default:
			return ExcludeRule{}, fmt.Errorf("unsupported exclusion field %q", key)
		}
	}

	if rule.Empty() {
		return ExcludeRule{}, fmt.Errorf("exclusion must contain at least one match")
	}
	return rule, nil
}

// FindMatchingRule returns the first non-empty exclusion rule that matches disk on nodeName and true,
// or an empty rule and false if no rule matches.
func FindMatchingRule(nodeName string, disk v1alpha1.DiskInfo, rules []ExcludeRule) (ExcludeRule, bool) {
	for _, rule := range rules {
		if rule.Matches(nodeName, disk) {
			return rule, true
		}
	}
	return ExcludeRule{}, false
}

// IsExcluded reports whether disk on nodeName matches any non-empty exclusion rule.
func IsExcluded(nodeName string, disk v1alpha1.DiskInfo, rules []ExcludeRule) bool {
	_, matched := FindMatchingRule(nodeName, disk, rules)
	return matched
}

var (
	// ignoredPrefixes are kernel device names that must never be monitored.
	ignoredPrefixes = []string{
		"loop",
		"ram",
		"zram",
		"dm-",
		"md",
		"sr",
		"fd",
		"nbd",
		"rbd",
		"drbd",
	}

	// partitionRegex matches common partition suffix patterns on block devices:
	//   - SATA/SCSI (e.g. sda1, sdb2, vda1) -> ends with digits (\d+)
	//   - NVMe/eMMC (e.g. nvme0n1p1, mmcblk0p1) -> ends with 'p' followed by digits (p\d+).
	partitionRegex = regexp.MustCompile(`(p\d+|\d+)$`)
)

// ShouldIgnoreDevice checks DEVTYPE and device name against filtering rules.
// Rules:
// 1. Must be DEVTYPE=disk (DEVTYPE=partition or others are rejected).
// 2. Must not start with excluded prefixes (loop, ram, zram, dm-, md, sr, fd, nbd).
// 3. Must not be a partition name:
//   - For sdX, vdX, xvdX, hdX: digits at the end designate a partition (e.g. sda1).
//   - For nvmeXnY: 'nvme0n1' is Controller 0, Namespace 1 (a DISK, despite ending in '1').
//     Partitions on NVMe are formatted as 'nvme0n1p1' (containing 'p' + digits).
func ShouldIgnoreDevice(devName string, devType string) bool {
	devName = strings.TrimPrefix(devName, "/dev/")

	// DEVTYPE validation:
	// systemd-udev classifies block devices as either DEVTYPE=disk (whole disk / namespace)
	// or DEVTYPE=partition (a partition slice). Any non-disk type is immediately rejected.
	if devType != "" && devType != "disk" {
		return true
	}

	// Filter out virtual, pseudo, and network block devices by prefix.
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(devName, prefix) {
			return true
		}
	}

	// Disallow disk names that look like partitions when devType is ambiguous:
	// Standard SATA/SCSI (sdX), VirtIO (vdX), Xen (xvdX), and IDE (hdX):
	// A trailing digit indicates a partition: "sda" is a disk, "sda1" is a partition.
	if strings.HasPrefix(devName, "sd") || strings.HasPrefix(devName, "vd") || strings.HasPrefix(devName, "xvd") || strings.HasPrefix(devName, "hd") {
		if partitionRegex.MatchString(devName) {
			return true
		}
	} else if strings.HasPrefix(devName, "nvme") {
		// NVMe namespace naming insider rule:
		// "nvme0n1" is Controller 0, Namespace 1 -> this is a full disk device!
		// Even though it ends in the digit '1', it is NOT a partition.
		// On NVMe devices, partitions are always separated by a 'p', e.g. "nvme0n1p1".
		if strings.Contains(devName, "p") && partitionRegex.MatchString(devName) {
			return true
		}
	}

	return false
}

// ShouldIgnoreProperties filters dynamically attached storage volumes while
// preserving physical disks and VM-emulated disks for development.
func ShouldIgnoreProperties(props map[string]string) bool {
	if len(props) == 0 {
		return false
	}

	// An iSCSI transport path identifies a remotely attached block device.
	if strings.EqualFold(props["ID_BUS"], "iscsi") {
		return true
	}

	busPath := strings.ToLower(props["ID_PATH"])
	if strings.Contains(busPath, "-iscsi-") || strings.Contains(busPath, "io.longhorn") {
		return true
	}

	// IET and LIO-ORG are Linux iSCSI target implementations. Match their
	// virtual-disk identity together to avoid excluding unrelated SCSI devices.
	vendor := strings.ToUpper(strings.TrimSpace(props["ID_VENDOR"]))
	model := strings.ToUpper(strings.TrimSpace(props["ID_MODEL"]))
	if (vendor == "IET" || vendor == "LIO-ORG") && model == "VIRTUAL-DISK" {
		return true
	}

	return false
}

// ShouldIgnore reports whether a block device should be ignored by evaluating
// both device-level filtering rules (DEVTYPE, naming prefixes, partitions) and
// property-level rules (iSCSI, Longhorn, virtual disks).
func ShouldIgnore(devName, devType string, props map[string]string) bool {
	return ShouldIgnoreDevice(devName, devType) || ShouldIgnoreProperties(props)
}
