// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// This file provides predictable path resolution and Kubernetes-compatible
// deterministic naming for physical storage hardware. See doc.go for the full
// predictable path hierarchy and naming rationale.

package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/votdev/node-disk-sentinel/pkg/apis/node-disk-sentinel.org/v1alpha1"
)

var (
	// invalidRFC1123SubdomainRegexp matches characters not permitted in RFC-1123 subdomains.
	invalidRFC1123SubdomainRegexp = regexp.MustCompile(`[^a-z0-9.-]+`)
)

// ResolvePredictablePath selects the most stable and persistent device path.
// See the package documentation (doc.go) for why kernel device letters are
// unstable and the full priority hierarchy of persistent udev symlinks evaluated.
func ResolvePredictablePath(devName string, links []string) string {
	var (
		wwnLink     string
		nvmeEuiLink string
		byIdLink    string
		byPathLink  string
	)

	for _, link := range links {
		// Normalize: ensure it starts with /dev/ if relative or symlink target path.
		path := link
		if !strings.HasPrefix(path, "/dev/") {
			path = "/dev/" + strings.TrimPrefix(path, "/")
		}

		if strings.HasPrefix(path, "/dev/disk/by-id/wwn-") {
			if wwnLink == "" {
				wwnLink = path
			}
		} else if strings.HasPrefix(path, "/dev/disk/by-id/nvme-eui.") {
			if nvmeEuiLink == "" {
				nvmeEuiLink = path
			}
		} else if strings.HasPrefix(path, "/dev/disk/by-id/ata-") ||
			strings.HasPrefix(path, "/dev/disk/by-id/nvme-") ||
			strings.HasPrefix(path, "/dev/disk/by-id/scsi-") {
			if byIdLink == "" {
				byIdLink = path
			}
		} else if strings.HasPrefix(path, "/dev/disk/by-path/") {
			if byPathLink == "" {
				byPathLink = path
			}
		}
	}

	if wwnLink != "" {
		return wwnLink
	}
	if nvmeEuiLink != "" {
		return nvmeEuiLink
	}
	if byIdLink != "" {
		return byIdLink
	}
	if byPathLink != "" {
		return byPathLink
	}

	if devName != "" {
		return "/dev/" + strings.TrimPrefix(devName, "/dev/")
	}
	return ""
}

// SanitizeRFC1123Subdomain converts an arbitrary string into a valid RFC-1123 DNS subdomain label.
// Lowercase, replaces non-alphanumeric chars (including '_') with '-', collapses consecutive hyphens,
// trims hyphens or periods at boundaries.
func SanitizeRFC1123Subdomain(s string) string {
	s = strings.ToLower(s)
	s = invalidRFC1123SubdomainRegexp.ReplaceAllString(s, "-")
	// Clean up multi-hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-.")
	return s
}

// GenerateCRName constructs a deterministic, cluster-unique, RFC-1123 compliant PhysicalDisk CR name:
// <nodename>-<device-identifier>
// Max 253 characters. If length exceeds 253 characters, appends a SHA256 hash suffix.
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
