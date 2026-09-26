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
// 3. Operational Mechanics:
//    - Seeking: On startup, the reader seeks to io.SeekEnd so that historic messages
//      from previous boots or prior daemon lifetimes are not replayed as new events.
//    - Overrun Handling (EPIPE): The kernel ring buffer is circular. If reader
//      consumption falls behind during a severe logging storm and messages are
//      overwritten, the kernel returns EPIPE. The reader handles EPIPE transparently
//      by advancing to the next available record without terminating.
//    - Non-Blocking Epoll Polling: Reading is integrated into Go's non-blocking I/O
//      poller, cleanly terminating when context is canceled.
//
// 4. Persistence & Degradation Model:
//    - Storage hardware does not heal spontaneously from physical block I/O errors.
//      When an unrecoverable kernel I/O error occurs, the PhysicalDisk is immediately
//      marked as degraded (StatusKernelErrors, Degraded=True) and a diagnostic
//      finding (e.g. KERNEL_BLOCK_IO_ERROR) is attached.
//    - This degradation is persistent in Kubernetes etcd across routine SMART polls;
//      subsequent periodic SMART cycles reporting "Good" will NOT heal the drive back
//      to Good. If drive firmware later confirms sector reallocation or pending errors,
//      the status gracefully upgrades to SectorErrors or ExcessiveSectorErrors.
//
// 5. Limitations & Security Requirements:
//    - Host Access & Privileges: On hardened Linux kernels where kernel.dmesg_restrict = 1,
//      reading /dev/kmsg requires CAP_SYSLOG or CAP_SYS_ADMIN (satisfied by the
//      privileged: true DaemonSet securityContext).
//    - Downtime Window: Because the stream starts at SEEK_END to avoid duplicate replay
//      after pod restarts, any I/O errors that occur while the DaemonSet pod is down
//      will not be read from kmsg. However, previously recorded degradation remains
//      safely persisted in the PhysicalDisk Kubernetes Custom Resource in etcd.
//
// 6. Debouncing & Error Storm Protection:
//    - Failing drives can emit hundreds of kernel log lines per second during active I/O.
//    - Incoming kernel errors are buffered in-memory (recordKernelError) without issuing
//      direct Kubernetes API requests.
//    - The daemon uses a dedicated quiet period (--kmsg-debounce=5s) that is longer than
//      hotplug uevent debouncing (--event-debounce=1s) to allow SCSI error recovery
//      (aborts, link resets) and drive firmware bad-sector reallocation to settle
//      before triggering an ad-hoc SMART scan and updating the Kubernetes API server.
package kernellog
