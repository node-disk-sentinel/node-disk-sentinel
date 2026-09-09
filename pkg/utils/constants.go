// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package utils

const (
	prefix = "node-disk-sentinel.org"

	AppName = "node-disk-sentinel"
	// LabelNodeName is set on every PhysicalDisk to associate it with its host
	// node. This enables node-scoped label selectors on cluster-wide watches.
	LabelNodeName = prefix + "/node-name"
)
