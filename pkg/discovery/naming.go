// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// This file provides predictable path resolution and Kubernetes-compatible
// deterministic naming for physical storage hardware.

package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

// invalidRFC1123SubdomainRegexp matches runs of characters that must be replaced
// in an RFC-1123 subdomain. Hyphens are included so that a run of hyphens and
// invalid characters collapses into a single hyphen.
var invalidRFC1123SubdomainRegexp = regexp.MustCompile(`[^a-z0-9.]+`)

// predictablePathPrefixes lists the persistent /dev symlink families from most
// to least stable:
//   - by-id/wwn-*:      World Wide Name, a globally unique id assigned by the manufacturer.
//   - by-id/nvme-eui.*: EUI-64 or NGUID of an NVMe namespace.
//   - by-id/ata-*, nvme-*, scsi-* (in this order): model and serial number.
//   - by-path/*:        physical bus topology (PCI slot, SAS port).
var predictablePathPrefixes = []string{
	"/dev/disk/by-id/wwn-",
	"/dev/disk/by-id/nvme-eui.",
	"/dev/disk/by-id/ata-",
	"/dev/disk/by-id/nvme-",
	"/dev/disk/by-id/scsi-",
	"/dev/disk/by-path/",
}

// ResolvePredictablePath selects the most stable device path from the udev
// symlinks (relative to /dev). Kernel names like /dev/sda depend on probing
// order and change across reboots; if no persistent symlink exists, the kernel
// path is the last resort.
func ResolvePredictablePath(devName string, links []string) string {
	for _, prefix := range predictablePathPrefixes {
		for _, link := range links {
			path := "/dev/" + strings.TrimPrefix(strings.TrimPrefix(link, "/dev/"), "/")
			if strings.HasPrefix(path, prefix) {
				return path
			}
		}
	}
	if devName != "" {
		return "/dev/" + strings.TrimPrefix(devName, "/dev/")
	}
	return ""
}

// SanitizeRFC1123Subdomain lowercases s and replaces every run of characters other
// than a-z, 0-9 and '.' (this includes '_' and '-') with a single '-'. Hyphens and
// periods at the start and end are trimmed. Periods are kept because node names
// are DNS subdomains. The result is not a valid subdomain for every input: a
// period next to another separator ("a._b" becomes "a.-b") leaves an invalid label.
func SanitizeRFC1123Subdomain(s string) string {
	s = invalidRFC1123SubdomainRegexp.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-.")
}

// GenerateCRName constructs the deterministic PhysicalDisk name
// <nodename>-<device-identifier> from sanitized parts (see SanitizeRFC1123Subdomain).
// Names are at most 253 characters; a longer name is truncated and ends in a
// hyphen and the first 8 hex digits of its SHA-256.
func GenerateCRName(nodeName string, diskInfo *v1alpha1.DiskInfo) string {
	cleanNode := SanitizeRFC1123Subdomain(nodeName)
	if cleanNode == "" {
		cleanNode = "node"
	}

	// Priority for identifier: WWN -> SerialShort/Serial -> Predictable Path basename -> Kernel Name.
	var rawID string
	if diskInfo.WWN != "" {
		rawID = diskInfo.WWN
	} else if diskInfo.SerialShort != "" {
		rawID = diskInfo.SerialShort
	} else if diskInfo.Serial != "" {
		rawID = diskInfo.Serial
	} else if diskInfo.Path != "" {
		parts := strings.Split(diskInfo.Path, "/")
		rawID = parts[len(parts)-1]
	} else {
		rawID = diskInfo.Name
	}

	cleanID := SanitizeRFC1123Subdomain(rawID)
	if cleanID == "" {
		cleanID = "disk"
	}

	name := cleanNode + "-" + cleanID

	// Kubernetes resource names must be at most 253 characters.
	const maxLen = 253
	if len(name) <= maxLen {
		return name
	}

	// If too long, truncate and append short hash.
	hash := sha256.Sum256([]byte(name))
	hashSuffix := "-" + hex.EncodeToString(hash[:])[:8]
	cutPoint := maxLen - len(hashSuffix)
	truncated := strings.TrimRight(name[:cutPoint], "-.")
	return truncated + hashSuffix
}
