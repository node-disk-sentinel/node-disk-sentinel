// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package discovery finds the physical disks of a node and notices when they
// come and go. It needs neither cgo/libudev nor the udevadm binary, so the
// daemon stays a static binary that runs in a minimal container.
//
// # Inventory
//
// DiscoverDisks combines two sources:
//
//   - /sys/block lists the disks of the kernel, as lsblk does. <name>/dev holds
//     "major:minor", <name>/size and <name>/queue/rotational describe the
//     hardware. Partitions show up below their disk and are skipped by their
//     "partition" attribute, because sysfs-rules.rst treats /sys/block and
//     /sys/class/block as interchangeable and the latter lists partitions next
//     to disks.
//   - /run/udev/data/b<major>:<minor> is udev's runtime database for that device
//     (a tmpfs, rebuilt by systemd-udevd after boot). Each line starts with a tag:
//     "S:" is a persistent /dev symlink, "E:" a property set by udev rules
//     (ID_MODEL, ID_SERIAL, ID_WWN, ...). Kernel properties such as DEVNAME,
//     DEVTYPE, MAJOR or MINOR are NOT stored there; sd-device, and with it
//     udevadm, reads them from the device's sysfs "uevent" file. The format is
//     internal to udev but versioned ("V:1").
//
// # Events
//
// Monitor subscribes to the netlink group that systemd-udevd writes after its
// rules ran (group 2). The raw kernel group would announce a disk before its
// symlinks and database record exist. Events are only a hint to rescan: netlink
// multicast is lossy, so Monitor also signals after (re)binding the socket and
// when the kernel reports dropped or truncated messages.
//
// # Naming
//
// Kernel names like /dev/sda change with probing order, so ResolvePredictablePath
// prefers persistent symlinks (WWN, NVMe EUI, model and serial, bus path) and
// GenerateCRName derives the PhysicalDisk name from the most stable identifier.
package discovery
