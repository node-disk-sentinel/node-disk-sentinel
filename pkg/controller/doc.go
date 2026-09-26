// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package controller keeps the PhysicalDisk resources of one node in sync with
// the hardware: it discovers the local disks, collects and assesses their SMART
// data, and records the result in the Kubernetes API and in Prometheus metrics.
//
// DiskMonitor plays two roles:
//
//   - Runnable: Start rescans the inventory when the udev monitor reports a
//     change and on every poll interval. Hints from the monitor collapse into
//     one pending scan that runs a fixed delay (--event-debounce) after the
//     first of them, so disks that appear together cost a single scan and the
//     event reader never waits for a slow smartctl run. A disk that
//     disappeared is marked missing before the SMART collection starts.
//   - Reconciler: Reconcile reacts when a user edits a PhysicalDisk spec (for
//     example smartctl extraCmdArgs). Updates caused by the daemon itself are
//     filtered out by predicates.
//
// The manager cache is scoped to this node, which keeps the watch cheap. The
// monitor's own reads and writes use a direct client instead: a PhysicalDisk
// with a missing or stale node label is invisible to the scoped cache and
// would otherwise cause duplicate-create errors instead of being repaired.
//
// Kernel log: when enabled, Start also streams /dev/kmsg and buffers the block
// layer, SCSI and NVMe errors it finds for monitored disks; lines for other
// devices are dropped. A failing drive can log hundreds of lines per second, so
// the lines only fill an in-memory buffer and the next scan applies them to the
// PhysicalDisk once per device, as a finding, a Warning event and a metric. That
// scan runs a fixed delay (--kmsg-debounce) after the first line, which is longer
// than the udev delay: it gives the kernel time to finish error recovery (SCSI
// aborts, link resets) and the drive firmware time to update its reallocation
// counters before smartctl queries the drive.
//
// Kernel findings are kept across SMART collections. A disk that SMART reports as
// Good or AttributeFailedInPast but that has logged kernel errors stays at
// KernelErrors, so it does not heal until the drive is replaced; any more severe
// SMART status takes precedence.
package controller
