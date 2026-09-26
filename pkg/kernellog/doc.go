// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package kernellog monitors real-time Linux kernel storage errors directly via /dev/kmsg
// without userspace daemon or CGO dependencies.
//
// ============================================================================
// Architecture & Design Rationale: /dev/kmsg vs systemd-journald
// ============================================================================
//
// 1. Zero CGO & Static Binary Philosophy:
//    node-disk-sentinel compiles as a completely static, standalone binary
//    (CGO_ENABLED=0) targeting minimal container environments (Alpine/scratch/distroless).
//    Reading systemd journals natively in Go requires dynamic linking against
//    libsystemd.so (e.g. via go-systemd/sdjournal), introducing glibc/musl
//    incompatibilities, host runtime library drift, and cross-compilation friction.
//
// 2. Linux Distribution Agnostic:
//    The Linux kernel block layer emits I/O errors directly to the kernel printk
//    ring buffer (/dev/kmsg). systemd-journald merely ingests from /dev/kmsg as a
//    downstream consumer. By streaming directly from /dev/kmsg, node-disk-sentinel
//    functions identically across diverse Kubernetes node operating systems—whether
//    they use systemd, OpenRC, or minimal immutable container OSes (e.g. Talos Linux,
//    Flatcar Container Linux, or Container-Optimized OS) without requiring host
//    journal directory mounts (/var/log/journal or /run/log/journal).
//
// ============================================================================
// Kernel Ring Buffer Mechanics & Real-Time Ingestion
// ============================================================================
//
// 1. Seeking & Replay Prevention (io.SeekEnd):
//    On startup, the reader seeks to io.SeekEnd so that historic messages from
//    previous boots or prior daemon lifetimes are not replayed as new events.
//    Any I/O errors occurring while the daemon is offline are not ingested from kmsg,
//    but previously recorded degradations remain safely persisted in the PhysicalDisk
//    Custom Resource in etcd.
//
// 2. Ring Buffer Overrun Handling (EPIPE):
//    The kernel printk ring buffer is circular. If consumption falls behind during
//    a severe logging storm and unread records are overwritten by the kernel, the
//    read syscall returns EPIPE. The reader handles EPIPE transparently by advancing
//    to the next available record without terminating the stream.
//
// 3. Security Requirements & Kernel Privileges (CAP_SYSLOG):
//    On hardened Linux kernels where kernel.dmesg_restrict = 1 (or dmesg_restrict sysctl),
//    reading /dev/kmsg requires CAP_SYSLOG or CAP_SYS_ADMIN. This is satisfied by
//    the privileged DaemonSet securityContext.
//
// ============================================================================
// Persistence, Debouncing & Error Storm Protection
// ============================================================================
//
// 1. Hardware Persistence & Degradation Model:
//    Storage hardware does not heal spontaneously from physical block I/O errors.
//    When an unrecoverable kernel I/O error occurs, the PhysicalDisk is immediately
//    marked as degraded (StatusKernelErrors, Degraded=True) and a diagnostic finding
//    (e.g. KERNEL_BLOCK_IO_ERROR, KERNEL_SCSI_ERROR, KERNEL_NVME_ERROR) is attached.
//    This degradation is persistent across routine SMART polls: subsequent periodic
//    SMART cycles reporting "Good" will NOT heal the drive back to Good. If drive
//    firmware later confirms sector reallocation or pending errors, the status
//    gracefully upgrades to SectorErrors or ExcessiveSectorErrors.
//
// 2. Dual Debouncing (--event-debounce vs --kmsg-debounce):
//    Failing drives can emit hundreds of kernel log lines per second during active I/O.
//    Incoming kernel errors are buffered in-memory without issuing direct Kubernetes
//    API calls. The daemon uses a dedicated quiet period (--kmsg-debounce=5s) that is
//    longer than hotplug uevent debouncing (--event-debounce=1s) to allow kernel SCSI
//    error recovery (command aborts, link resets, driver retries) and drive firmware
//    bad-sector reallocation to settle before triggering an ad-hoc SMART scan and
//    updating the Kubernetes API server.
package kernellog
