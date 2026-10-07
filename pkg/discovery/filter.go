// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// This file decides which disks are monitored: the built-in filter for virtual
// and network block devices, and the exclusion rules configured by the user.

package discovery

import (
	"fmt"
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

// fields lists the rule criteria in a stable order, keyed by their flag name.
func (r *ExcludeRule) fields() []struct {
	key   string
	value *string
} {
	return []struct {
		key   string
		value *string
	}{
		{"node", &r.Node}, {"name", &r.Name}, {"vendor", &r.Vendor}, {"model", &r.Model},
		{"serial", &r.Serial}, {"wwn", &r.WWN}, {"bus", &r.Bus},
	}
}

// String returns the comma-separated key=value representation of the exclusion rule.
func (r ExcludeRule) String() string {
	var parts []string
	for _, f := range r.fields() {
		if *f.value != "" {
			parts = append(parts, f.key+"="+*f.value)
		}
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
	fields := map[string]*string{}
	for _, f := range rule.fields() {
		fields[f.key] = f.value
	}

	for _, part := range strings.Split(value, ",") {
		key, val, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !ok || key == "" || val == "" {
			return ExcludeRule{}, fmt.Errorf("invalid exclusion %q; expected key=value pairs", part)
		}
		field, ok := fields[key]
		switch {
		case !ok:
			return ExcludeRule{}, fmt.Errorf("unsupported exclusion field %q", key)
		case *field != "":
			return ExcludeRule{}, fmt.Errorf("duplicate exclusion field %q", key)
		}
		*field = val
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

// ignoredPrefixes are kernel names of block devices that are never monitored:
// virtual ones (loop, ram, zram, device mapper, md RAID), network ones (nbd, rbd,
// drbd) and removable-media drives (sr optical, fd floppy).
var ignoredPrefixes = []string{
	"loop", "ram", "zram", "dm-", "md", "sr", "fd", "nbd", "rbd", "drbd",
}

// ShouldIgnore reports whether the whole disk with the given kernel name must
// not be monitored: the devices named in ignoredPrefixes, and dynamically
// attached volumes (iSCSI, Longhorn) identified by their udev properties.
// VM-emulated disks are kept for development. Partitions have to be filtered out
// by the caller.
func ShouldIgnore(name string, props map[string]string) bool {
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
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
	return (vendor == "IET" || vendor == "LIO-ORG") && model == "VIRTUAL-DISK"
}
