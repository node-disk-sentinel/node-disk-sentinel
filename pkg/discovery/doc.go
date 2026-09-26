// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package discovery provides hardware disk discovery, persistent device naming,
// and kernel uevent monitoring for block devices without external C dependencies
// or runtime daemon tools.
//
// ============================================================================
// Linux Netlink uevent & systemd-udev Architecture
// ============================================================================
//
// 1. Netlink Kobject Uevents (AF_NETLINK / NETLINK_KOBJECT_UEVENT):
//    In Linux, the kernel communicates hardware hotplug events to userspace via
//    the netlink socket protocol family (AF_NETLINK), specifically using the
//    NETLINK_KOBJECT_UEVENT bus protocol. Whenever a kobject (representing a
//    device, driver, bus, or partition in the driver core) is registered,
//    modified, or removed, the kernel emits an event.
//
// 2. Multicast Group 1 (Kernel Raw) vs Multicast Group 2 (udev Enriched):
//    - Group 1 (bitmask 1 << 0 = 1, UDEV_MONITOR_KERNEL):
//      The raw kernel uevent broadcast. When a disk is physically plugged in,
//      the kernel emits this event immediately.
//      INSIDER TRAP: Listening to Group 1 is almost always a mistake for disk
//      monitoring daemons! At this point, systemd-udevd has NOT yet evaluated any
//      rules. Symlinks under /dev/disk/by-id/ and /dev/disk/by-path/ do not exist yet.
//      Probing tools (like smartctl or blkid) will race against partition table
//      instantiation, leading to transient I/O errors or device busy locks (EBUSY).
//    - Group 2 (bitmask 1 << 1 = 2, UDEV_MONITOR_UDEV):
//      After systemd-udevd receives the raw kernel event from Group 1, it executes
//      its rule pipeline (/usr/lib/udev/rules.d/ and /etc/udev/rules.d/), runs helper
//      utilities (such as ata_id, scsi_id, path_id, and blkid), populates the
//      runtime database in /run/udev/data/, creates all persistent /dev symlinks,
//      and only then rebroadcasts the fully-enriched event to Netlink Group 2.
//      By binding exclusively to Group 2, node-disk-sentinel guarantees that all
//      hardware metadata (WWN, Serial, Symlinks, Model) are fully populated and
//      stable before reconciliation begins.
//
// 3. Why a Pure Go Wire Protocol Parser (Zero CGO / libudev):
//    - Standard userland tools link dynamically against libudev.so (part of systemd).
//    - In containerized environments (Kubernetes DaemonSets, distroless images,
//      Alpine/musl-based hosts, or minimal scratch containers), relying on host
//      shared libraries causes severe friction: glibc vs musl incompatibilities,
//      dynamic linker version drift, and missing .so files.
//    - Compiling with CGO_ENABLED=0 produces a completely self-contained static
//      binary with zero host runtime library dependencies.
//    - To achieve this, this package implements the exact libudev wire protocol
//      (struct udev_monitor_netlink_header) and parses the datagrams directly.
//
// 4. Wire Protocol Header (struct udev_monitor_netlink_header):
//    systemd-udevd prepends every netlink message sent to Group 2 with a 40-byte
//    binary header:
//    [0..7]   prefix: "libudev\0" (identifies systemd-udevd as the source)
//    [8..11]  magic: 0xfeedcafe (big-endian network byte order via htonl)
//    [12..15] header_size: uint32 (host endian; minimum 40 bytes)
//    [16..19] properties_off: uint32 (host endian; byte offset where KEY=VALUE strings start)
//    [20..23] properties_len: uint32 (host endian; byte length of the properties payload)
//    [24..39] filter hashes & bloom filter (used internally by libudev client filtering)
//    [properties_off .. properties_off+properties_len]
//    payload: NUL-terminated strings: "ACTION=add\0DEVNAME=/dev/sda\0..."
//
// 5. Anti-Spoofing & Security (SO_PASSCRED & SCM_CREDENTIALS):
//    - Netlink sockets in Linux are datagram-based. Any unprivileged process running
//      on the local host or in a shared network namespace can craft an AF_NETLINK
//      datagram and send it to our socket if we do not authenticate the sender.
//    - A malicious local process could forge an ACTION=remove event, causing the
//      daemon to falsely mark healthy production storage disks as missing.
//    - To prevent this, we enable SO_PASSCRED on the socket. The Linux kernel then
//      attaches cryptographically unforgeable ancillary control messages
//      (SCM_CREDENTIALS) to every received datagram.
//    - We inspect the attached struct ucred (PID, UID, GID). The event is accepted
//      only if it originates from PID 0 (the kernel) or UID 0 (normally systemd-udevd).
//      Any unprivileged sender is rejected with ErrUntrustedSender.
//    - This deliberately protects the daemon from unprivileged local injection, not
//      from a compromised host root account. A process with host-root privileges is
//      already inside the trust boundary and can impersonate udevd.
//
// 6. Socket Buffer Sizing (SO_RCVBUFFORCE vs SO_RCVBUF):
//    - During sudden hardware events (e.g. hotplugging a SAS JBOD, fibre channel
//      failover, or multipath re-scanning), dozens or hundreds of block device
//      events occur in milliseconds.
//    - Linux limits socket receive buffers via /proc/sys/net/core/rmem_max (often ~212 KB).
//      If the buffer fills up, the kernel drops events and sets the ENOBUFS error.
//    - With CAP_NET_ADMIN, SO_RCVBUFFORCE allows overriding the rmem_max limit
//      to request a large 2 MB buffer, preventing event loss during hotplug bursts.
//
// 7. Go Runtime Poller Integration:
//    - Using raw blocking read syscalls would tie up OS threads in Go's M:N scheduler.
//    - By opening the socket with SOCK_NONBLOCK and wrapping it in os.NewFile +
//      file.SyscallConn(), Go's non-blocking network poller (epoll on Linux) manages
//      the socket. Goroutines park cleanly until kernel data is ready.
//
// ============================================================================
// Linux udev Runtime Database & sysfs Architecture
// ============================================================================
//
// 1. The /run/udev/data Runtime Database:
//    - On modern Linux distributions using systemd, 'systemd-udevd' manages a
//      high-speed in-memory database located at /run/udev/data (mounted on tmpfs;
//      historically /dev/.udev/db on pre-systemd distributions).
//    - Why read files directly instead of calling 'udevadm info'?
//      Executing 'udevadm' in a subprocess for every disk forks a process, dynamically
//      links libraries, and parses output on stdout. On large storage nodes with dozens
//      or hundreds of disks, this incurs significant CPU and latency overhead.
//      Furthermore, in containerized Kubernetes DaemonSets (especially distroless or
//      scratch images), the 'udevadm' binary is often absent. By mounting the host's
//      /run/udev/data read-only into the container, node-disk-sentinel discovers all
//      hardware metadata in microseconds with zero binary dependencies.
//    - This is a runtime cache, not durable inventory. Because /run is tmpfs, udev
//      rebuilds it after each host boot. A record can also disappear while a scan is
//      in progress during hot removal. Discovery therefore treats individual unreadable
//      records as transient and relies on the next event or periodic scan to converge.
//
// 2. File Naming Convention: b<major>:<minor> vs c<major>:<minor>:
//    - The Linux kernel uniquely addresses device nodes using a (type, major, minor) tuple:
//      'b' indicates a block device (disks, partitions, loopback).
//      'c' indicates a character device (ttys, mice, hardware random generators).
//      'major' identifies the kernel device driver/subsystem:
//        8   = SCSI disk subsystem (SATA, SAS, USB mass storage: /dev/sd*)
//        259 = NVMe subsystem (/dev/nvme*n*)
//        254 = Device Mapper (/dev/dm-*)
//      'minor' identifies the specific drive or partition instance within that driver.
//    - Consequently, udev names each database file after this exact tuple:
//        /run/udev/data/b8:0   -> Block device 8:0 (/dev/sda - whole disk)
//        /run/udev/data/b8:1   -> Block device 8:1 (/dev/sda1 - first partition)
//        /run/udev/data/b259:0 -> Block device 259:0 (/dev/nvme0n1 - NVMe namespace)
//
// 3. Database Syntax (Tag Prefixes):
//    Each line in a /run/udev/data file begins with a single-character tag and colon:
//    - S:<symlink>
//      Persistent symlinks created under /dev for this device, stored relative to /dev
//      (e.g. "S:disk/by-id/ata-WDC_WD10EZEX...", "S:disk/by-path/pci-...").
//    - E:<KEY>=<VALUE>
//      Environment variables populated by udev rules (e.g. ata_id, scsi_id, blkid):
//      DEVNAME, DEVTYPE, ID_BUS, ID_MODEL, ID_SERIAL, ID_WWN, etc.
//    - G:<tag>, W:<watch>, A:<attr>
//      Tags, inotify watch descriptors, and cached sysfs attributes (ignored by our parser).
//
// 4. Sysfs Fallbacks (/sys/dev/block/<major>:<minor>):
//    - In virtualized or cloud environments (QEMU/KVM with VirtIO-blk, cloud-init,
//      or trimmed container rootfs), udev records can occasionally omit DEVNAME or DEVTYPE.
//    - Linux sysfs exposes a deterministic lookup directory at /sys/dev/block/<major>:<minor>.
//      This is a symlink resolving to the full kobject path under /sys/devices/.../block/<name>.
//      By resolving this link with filepath.EvalSymlinks, we can reliably recover the
//      canonical device name and inspect sysfs attributes directly.
//
// ============================================================================
// ResolvePredictablePath: Predictable Path Hierarchy & Deterministic Naming
// ============================================================================
//
// In the Linux kernel, device letters (/dev/sda, /dev/sdb, etc.) are assigned
// dynamically and asynchronously based on the order storage controllers and
// SATA/SAS/PCIe lanes finish driver initialization during boot.
//   - If a drive responds 50ms slower on one reboot, /dev/sda and /dev/sdb can swap.
//   - If a USB drive is plugged in during boot, it might steal /dev/sda.
//   - If an SAS controller resets or hot-plugs a disk, the letter changes.
//
// systemd-udev solves this by creating persistent symlinks under /dev/disk/.
// ResolvePredictablePath evaluates all available symlinks and selects the most
// immutable path according to this strict hierarchy:
//
//  1. /dev/disk/by-id/wwn-* (World Wide Name):
//     IEEE standard 64-bit or 128-bit globally unique hardware identifier burned
//     into drive firmware by the factory. Survives cable swaps, controller changes,
//     and server motherboard replacements.
//
//  2. /dev/disk/by-id/nvme-eui.* (NVMe Extended Unique Identifier):
//     IEEE EUI-64 globally unique identifier assigned to NVMe namespaces.
//
//  3. /dev/disk/by-id/ata-*, nvme-*, scsi-* (Serial Number & Model):
//     Constructed from the drive's inquiry serial number and model name. Highly
//     persistent across reboots on the same machine.
//
//  4. /dev/disk/by-path/* (Physical Bus Topology):
//     Encodes the physical PCI slot, SAS channel, and port (e.g. pci-0000:00:1f.2-ata-1).
//     Guarantees predictability based on physical enclosure slot location.
//
//  5. Canonical kernel device path (/dev/<devname>):
//     Last-resort fallback if udev symlinks were not generated or mounted.
//
// GenerateCRName uses these stable identifiers to generate deterministic,
// RFC-1123-compliant PhysicalDisk resource names (<nodename>-<device-id>) that
// survive reboots and controller re-enumerations.
package discovery
