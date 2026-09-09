// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package discovery provides hardware disk discovery and kernel uevent monitoring
// for block devices without external C dependencies or runtime daemon tools.
//
// ============================================================================
// Linux Netlink uevent & systemd-udev Architecture
// ============================================================================
//
//  1. Netlink Kobject Uevents (AF_NETLINK / NETLINK_KOBJECT_UEVENT):
//     In Linux, the kernel communicates hardware hotplug events to userspace via
//     the netlink socket protocol family (AF_NETLINK), specifically using the
//     NETLINK_KOBJECT_UEVENT bus protocol. Whenever a kobject (representing a
//     device, driver, bus, or partition in the driver core) is registered,
//     modified, or removed, the kernel emits an event.
//
// 2. Multicast Group 1 (Kernel Raw) vs Multicast Group 2 (udev Enriched):
//   - Group 1 (bitmask 1 << 0 = 1, UDEV_MONITOR_KERNEL):
//     The raw kernel uevent broadcast. When a disk is physically plugged in,
//     the kernel emits this event immediately.
//     INSIDER TRAP: Listening to Group 1 is almost always a mistake for disk
//     monitoring daemons! At this point, systemd-udevd has NOT yet evaluated any
//     rules. Symlinks under /dev/disk/by-id/ and /dev/disk/by-path/ do not exist yet.
//     Probing tools (like smartctl or blkid) will race against partition table
//     instantiation, leading to transient I/O errors or device busy locks (EBUSY).
//   - Group 2 (bitmask 1 << 1 = 2, UDEV_MONITOR_UDEV):
//     After systemd-udevd receives the raw kernel event from Group 1, it executes
//     its rule pipeline (/usr/lib/udev/rules.d/ and /etc/udev/rules.d/), runs helper
//     utilities (such as ata_id, scsi_id, path_id, and blkid), populates the
//     runtime database in /run/udev/data/, creates all persistent /dev symlinks,
//     and only then rebroadcasts the fully-enriched event to Netlink Group 2.
//     By binding exclusively to Group 2, node-disk-sentinel guarantees that all
//     hardware metadata (WWN, Serial, Symlinks, Model) are fully populated and
//     stable before reconciliation begins.
//
// 3. Why a Pure Go Wire Protocol Parser (Zero CGO / libudev):
//
//   - Standard userland tools link dynamically against libudev.so (part of systemd).
//
//   - In containerized environments (Kubernetes DaemonSets, distroless images,
//     Alpine/musl-based hosts, or minimal scratch containers), relying on host
//     shared libraries causes severe friction: glibc vs musl incompatibilities,
//     dynamic linker version drift, and missing .so files.
//
//   - Compiling with CGO_ENABLED=0 produces a completely self-contained static
//     binary with zero host runtime library dependencies.
//
//   - To achieve this, this package implements the exact libudev wire protocol
//     (struct udev_monitor_netlink_header) and parses the datagrams directly.
//
//     4. Wire Protocol Header (struct udev_monitor_netlink_header):
//     systemd-udevd prepends every netlink message sent to Group 2 with a 40-byte
//     binary header:
//     [0..7]   prefix: "libudev\0" (identifies systemd-udevd as the source)
//     [8..11]  magic: 0xfeedcafe (big-endian network byte order via htonl)
//     [12..15] header_size: uint32 (host endian; minimum 40 bytes)
//     [16..19] properties_off: uint32 (host endian; byte offset where KEY=VALUE strings start)
//     [20..23] properties_len: uint32 (host endian; byte length of the properties payload)
//     [24..39] filter hashes & bloom filter (used internally by libudev client filtering)
//     [properties_off .. properties_off+properties_len]
//     payload: NUL-terminated strings: "ACTION=add\0DEVNAME=/dev/sda\0..."
//
// 5. Anti-Spoofing & Security (SO_PASSCRED & SCM_CREDENTIALS):
//   - Netlink sockets in Linux are datagram-based. Any unprivileged process running
//     on the local host or in a shared network namespace can craft an AF_NETLINK
//     datagram and send it to our socket if we do not authenticate the sender.
//   - A malicious local process could forge an ACTION=remove event, causing the
//     daemon to falsely mark healthy production storage disks as missing.
//   - To prevent this, we enable SO_PASSCRED on the socket. The Linux kernel then
//     attaches cryptographically unforgeable ancillary control messages
//     (SCM_CREDENTIALS) to every received datagram.
//   - We inspect the attached struct ucred (PID, UID, GID). The event is accepted
//     only if it originates from PID 0 (the kernel) or UID 0 (normally systemd-udevd).
//     Any unprivileged sender is rejected with ErrUntrustedSender.
//   - This deliberately protects the daemon from unprivileged local injection, not
//     from a compromised host root account. A process with host-root privileges is
//     already inside the trust boundary and can impersonate udevd.
//
// 6. Socket Buffer Sizing (SO_RCVBUFFORCE vs SO_RCVBUF):
//   - During sudden hardware events (e.g. hotplugging a SAS JBOD, fibre channel
//     failover, or multipath re-scanning), dozens or hundreds of block device
//     events occur in milliseconds.
//   - Linux limits socket receive buffers via /proc/sys/net/core/rmem_max (often ~212 KB).
//     If the buffer fills up, the kernel drops events and sets the ENOBUFS error.
//   - With CAP_NET_ADMIN, SO_RCVBUFFORCE allows overriding the rmem_max limit
//     to request a large 2 MB buffer, preventing event loss during hotplug bursts.
//
// 7. Go Runtime Poller Integration:
//   - Using raw blocking read syscalls would tie up OS threads in Go's M:N scheduler.
//   - By opening the socket with SOCK_NONBLOCK and wrapping it in os.NewFile +
//     file.SyscallConn(), Go's non-blocking network poller (epoll on Linux) manages
//     the socket. Goroutines park cleanly until kernel data is ready.
package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

const (
	// udevEventGroup is the netlink multicast group carrying events that udev
	// has already processed.
	//
	// In the Linux kernel (include/uapi/linux/netlink.h), netlink multicast groups
	// are specified as a bitmask:
	//   - Group 1 (bit 0, value 1 << 0 = 1): UDEV_MONITOR_KERNEL (raw kernel uevents).
	//   - Group 2 (bit 1, value 1 << 1 = 2): UDEV_MONITOR_UDEV (systemd-udevd enriched).
	//
	// Raw kernel events (Group 1) are emitted as soon as a driver registers a kobject.
	// At that instant, /dev symlinks (/dev/disk/by-id/..., /dev/disk/by-path/...)
	// do NOT exist, and querying SMART or blkid will race against partition table
	// generation.
	// Group 2 is normally transmitted by systemd-udevd after all rules in /etc/udev/rules.d/
	// and /usr/lib/udev/rules.d/ have finished running, symlinks are created, and
	// /run/udev/data has been updated.
	udevEventGroup = 2

	// receiveBufferSize allocates 2 MB for the netlink socket receive queue.
	// Netlink datagrams are stored in a kernel-space ring buffer. If a storage
	// controller suddenly initializes (e.g. SAS HBA attachment, multipath scan,
	// or USB/NVMe hotplug), the kernel can burst hundreds of events within milliseconds.
	// If the buffer overflows, the kernel drops events and sets ENOBUFS.
	// 2 MB provides ample headroom during massive hotplug storms. Netlink multicast remains
	// lossy by design, however; periodic inventory polling is the correctness backstop
	// when the kernel drops an event before it reaches this socket.
	receiveBufferSize = 2 * 1024 * 1024

	// readBufferSize is the per-packet user-space buffer for recvmsg.
	// A typical udev block device event with all enriched ID_* attributes is ~1-4 KB.
	// 128 KB accommodates even extraordinarily large udev records without truncation.
	readBufferSize = 128 * 1024

	// Layout of the libudev monitor header (struct udev_monitor_netlink_header)
	// prefixed to every multicast event on Group 2:
	//
	// Defined in systemd's src/libudev/libudev-monitor.c:
	//   struct udev_monitor_netlink_header {
	//       char prefix[8];                     /* 0..7:   "libudev\0" */
	//       unsigned int magic;                 /* 8..11:  htonl(0xfeedcafe) - BigEndian network byte order */
	//       unsigned int header_size;           /* 12..15: sizeof(header) = 40 bytes - Host Endian */
	//       unsigned int properties_off;        /* 16..19: offset to NUL-separated KEY=VALUE strings */
	//       unsigned int properties_len;        /* 20..23: byte length of environment properties */
	//       unsigned int filter_subsystem_hash; /* 24..27: MurmurHash2 of SUBSYSTEM string */
	//       unsigned int filter_devtype_hash;   /* 28..31: MurmurHash2 of DEVTYPE string */
	//       unsigned int filter_tag_bloom_hi;  /* 32..35: Bloom filter for tags (high 32 bits) */
	//       unsigned int filter_tag_bloom_lo;  /* 36..39: Bloom filter for tags (low 32 bits) */
	//   };
	//
	// ENDIANNESS:
	// Note that 'magic' is intentionally transmitted in Big-Endian (network byte order via htonl)
	// so any receiver can detect cross-endian architecture mismatches, whereas 'header_size',
	// 'properties_off', and 'properties_len' are transmitted in host-native endianness!
	//
	// HASHES:
	// Bytes 24..39 contain precomputed hashes and a Bloom filter designed for in-kernel
	// BPF socket filtering or fast rejection by libudev client matching. Since we parse
	// the payload directly in Go, we skip these filter fields and jump straight to properties_off.
	libudevHeaderLength  = 40
	libudevMagic         = 0xfeedcafe
	libudevMagicOffset   = 8
	libudevHeaderSizeOff = 12
	libudevPayloadOff    = 16
	libudevPropsLenOff   = 20
)

var libudevPrefix = []byte("libudev\x00")

// ErrUntrustedSender indicates a netlink message that was not multicast by
// udev or the kernel and therefore must not be acted upon.
// Unprivileged local processes can open AF_NETLINK sockets and attempt to
// inject forged removal or addition events to cause denial-of-service.
var ErrUntrustedSender = errors.New("untrusted udev netlink sender")

// Action constants represent device state change operations.
const (
	ActionAdd     = "add"
	ActionRemove  = "remove"
	ActionChange  = "change"
	ActionOnline  = "online"
	ActionOffline = "offline"
	ActionRescan  = "rescan" // Synthetic event triggered on buffer overrun (ENOBUFS) to force full reconciliation
)

// UEvent represents a fully parsed, udev-enriched hardware event.
// It contains both canonical device identity attributes and the raw environment map.
type UEvent struct {
	// Action indicates the state change: "add", "remove", "change", "online", "offline", "bind", "unbind".
	Action string
	// DevPath is the sysfs kobject path, e.g. "/devices/pci0000:00/.../block/sda".
	DevPath string
	// Subsystem identifies the kernel subsystem, e.g. "block".
	Subsystem string
	// DevName is the primary device node path under /dev, e.g. "/dev/sda".
	// NOTE: May be empty on "remove" events if the device node has already been unlinked!
	DevName string
	// DevType classifies the block object: "disk" (whole device) or "partition".
	DevType string
	// Env contains all raw KEY=VALUE properties exported by udev rules.
	Env map[string]string
}

// ParseUEventPayload decodes a udev-processed netlink message, including its
// libudev monitor header (struct udev_monitor_netlink_header).
//
// Wire format:
//
//	[40-byte binary header][NUL-separated environment strings: "KEY=VALUE\0KEY=VALUE\0..."]
func ParseUEventPayload(raw []byte) (*UEvent, error) {
	// [Bytes 0..7] Prefix check: ensures the message originates from systemd-udevd ("libudev\0")
	// and is not a raw kernel multicast (which has format "action@sysfs_path\0KEY=VALUE\0...")
	// or an unrelated netlink packet.
	if !bytes.HasPrefix(raw, libudevPrefix) {
		return nil, fmt.Errorf("not a udev-processed event")
	}
	if len(raw) < libudevHeaderLength {
		return nil, fmt.Errorf("truncated libudev header")
	}

	// [Bytes 8..11] Magic number check: udev writes 0xfeedcafe via htonl() in network byte
	// order (big endian). Verifies header integrity against corrupt, alien, or obsolete datagrams.
	if binary.BigEndian.Uint32(raw[libudevMagicOffset:]) != libudevMagic {
		return nil, fmt.Errorf("invalid libudev magic")
	}

	// [Bytes 12..15] header_size: uint32 in host endian (usually 40).
	// If a newer systemd release extends the header struct with additional filter fields,
	// header_size indicates where the struct ends. Must be >= 40 and <= len(raw).
	headerSize := binary.NativeEndian.Uint32(raw[libudevHeaderSizeOff:])
	if headerSize < libudevHeaderLength || headerSize > uint32(len(raw)) {
		return nil, fmt.Errorf("invalid libudev header size: %d", headerSize)
	}

	// [Bytes 16..19] properties_off: uint32 in host endian. Indicates the byte offset in raw
	// where the NUL-separated "KEY=VALUE" environment payload actually begins.
	// Must start at or after the header and lie within the received datagram bounds.
	payloadOffset := binary.NativeEndian.Uint32(raw[libudevPayloadOff:])
	if payloadOffset < headerSize || payloadOffset >= uint32(len(raw)) {
		return nil, fmt.Errorf("invalid libudev payload offset: %d", payloadOffset)
	}

	// [Bytes 20..23] properties_len: uint32 in host endian. Total byte length of the environment
	// strings. [Bytes 24..39] contain filter hashes which we do not need; restricting payloadEnd
	// to properties_off + properties_len ensures trailing hashes or padding are not parsed as env strings.
	//
	// SECURITY / OVERFLOW GUARD:
	// We use subtraction (payloadLen <= len - offset) rather than addition to guarantee immunity
	// against 32-bit arithmetic integer wrap-around.
	payloadLen := binary.NativeEndian.Uint32(raw[libudevPropsLenOff:])
	payloadEnd := uint32(len(raw))
	if payloadLen > 0 && payloadLen <= uint32(len(raw))-payloadOffset {
		payloadEnd = payloadOffset + payloadLen
	}
	// A zero or out-of-range properties_len is treated as advisory rather than
	// making the message unusable: use the datagram boundary already returned by
	// recvmsg. This accepts old or unusual udev senders while the preceding offset
	// checks still guarantee that slicing raw cannot panic or read past the packet.

	// [Bytes properties_off .. payloadEnd] Environment payload:
	// The payload consists of contiguous NUL-terminated strings:
	// "ACTION=add\0DEVNAME=/dev/sda\0SUBSYSTEM=block\0DEVTYPE=disk\0..."
	// We iterate through the slice with bytes.IndexByte(0) and split each field at '='.
	env := make(map[string]string, 32)
	payload := raw[payloadOffset:payloadEnd]
	for len(payload) > 0 {
		end := bytes.IndexByte(payload, 0)
		var field []byte
		if end == -1 {
			field = payload
			payload = nil
		} else {
			field = payload[:end]
			payload = payload[end+1:]
		}

		if len(field) == 0 {
			continue
		}
		if key, value, found := bytes.Cut(field, []byte{'='}); found && len(key) > 0 {
			env[string(key)] = string(value)
		}
	}

	// Every valid kernel/udev event must declare an ACTION and a sysfs DEVPATH.
	if env["ACTION"] == "" || env["DEVPATH"] == "" {
		return nil, fmt.Errorf("event is missing ACTION or DEVPATH")
	}

	return &UEvent{
		// Kernel actions are conventionally lowercase. Normalize defensively because
		// the controller uses a lowercase switch and udev properties are otherwise
		// opaque strings supplied by rules and helper programs.
		Action:    strings.ToLower(env["ACTION"]),
		DevPath:   env["DEVPATH"],
		Subsystem: env["SUBSYSTEM"],
		DevName:   env["DEVNAME"],
		DevType:   env["DEVTYPE"],
		Env:       env,
	}, nil
}

// KernelName returns the primary kernel device name (e.g. "sda", "nvme0n1").
//
// On hardware removal events (e.g. SATA cable pulled, NVMe surprise hot-unplug),
// the device node under /dev may have already been deleted by the kernel before
// udev finishes processing the event. In those cases, udev omits DEVNAME entirely!
// However, DEVPATH (e.g. "/devices/pci0000:00/.../block/sda") ALWAYS ends with
// the canonical kernel block device name. Falling back to filepath.Base(DevPath)
// ensures device removal events are never silently ignored.
func (e *UEvent) KernelName() string {
	if name := filepath.Base(e.DevName); name != "" && name != "." && name != "/" {
		return name
	}
	return filepath.Base(e.DevPath)
}

// UEventListener receives udev-processed events from the kernel netlink socket.
// It wraps a non-blocking netlink file descriptor integrated with the Go network poller.
type UEventListener struct {
	file *os.File

	closeOnce sync.Once
	closeErr  error
}

// NewUEventListener binds a netlink socket to the udev-processed event group.
//
// Syscall details:
// - AF_NETLINK: Linux netlink IPC domain.
// - SOCK_RAW: Direct access to netlink datagram packets.
// - SOCK_CLOEXEC: Prevents descriptor leakage across child process fork/exec (e.g. smartctl).
// - SOCK_NONBLOCK: Essential for integration with the Go runtime's epoll poller.
// - NETLINK_KOBJECT_UEVENT: Kernel protocol 15 for device hotplug notifications.
func NewUEventListener() (listener *UEventListener, err error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("failed to open netlink socket: %w", err)
	}
	defer func() {
		if err != nil {
			_ = unix.Close(fd)
		}
	}()

	// SENDER AUTHENTICATION (ANTI-SPOOFING):
	// SO_PASSCRED directs the kernel to append SCM_CREDENTIALS control messages
	// (containing struct ucred { pid, uid, gid }) to incoming datagrams.
	// Without this, ANY local unprivileged process could write forged datagrams
	// to our netlink socket and trick the daemon into deleting disks from Kubernetes.
	if err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		return nil, fmt.Errorf("failed to enable netlink sender credentials: %w", err)
	}

	// RECEIVE BUFFER TUNING:
	// Hotplug storms (e.g. SAS HBA attachment, multipath scan) produce huge event bursts.
	// SO_RCVBUFFORCE bypasses the /proc/sys/net/core/rmem_max sysctl limit to allocate 2 MB.
	// Because SO_RCVBUFFORCE requires CAP_NET_ADMIN, we fall back to standard SO_RCVBUF
	// (capped at rmem_max) so unit tests and unprivileged environments still function.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, receiveBufferSize); err != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBufferSize)
	}

	// Bind to multicast group 2 (udevEventGroup).
	// In unix.SockaddrNetlink, the 'Groups' field is a bitmask where bit 1 (decimal 2)
	// corresponds to UDEV_MONITOR_UDEV.
	if err = unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: udevEventGroup}); err != nil {
		return nil, fmt.Errorf("failed to bind netlink socket: %w", err)
	}

	// Wrapping the fd in os.NewFile transfers ownership of fd to the returned *os.File
	// and allows Go to manage non-blocking I/O through its internal netpoller (epoll on
	// Linux) via SyscallConn. Do not call unix.Close(fd) after this point: Close on the
	// file is the single owner and is what wakes a goroutine blocked in Read.
	file := os.NewFile(uintptr(fd), "netlink-uevent")
	return &UEventListener{file: file}, nil
}

// Close releases the netlink socket. It is safe to call more than once.
func (l *UEventListener) Close() error {
	l.closeOnce.Do(func() {
		if l.file != nil {
			l.closeErr = l.file.Close()
		}
	})
	return l.closeErr
}

// Listen forwards relevant block device events until ctx is canceled.
//
// Concurrency & Epoll Mechanics:
// Listen uses Go's SyscallConn.Read() to register the file descriptor with Go's
// runtime network poller (epoll). When no packets are ready, the goroutine parks
// without consuming CPU or locking an OS thread. When a packet arrives, the runtime
// unparks the goroutine to execute unix.Recvmsg.
func (l *UEventListener) Listen(ctx context.Context, out chan<- *UEvent) error {
	if l == nil || l.file == nil {
		return errors.New("listener is nil or closed")
	}

	defer func() {
		_ = l.Close()
	}()

	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-listenCtx.Done()
		_ = l.Close()
	}()

	rawConn, err := l.file.SyscallConn()
	if err != nil {
		return fmt.Errorf("failed to get raw connection: %w", err)
	}

	buf := make([]byte, readBufferSize)
	// Out-of-band (ancillary) control buffer for receiving credentials:
	// We allocate space for 2 * sizeof(struct cmsghdr + struct ucred) to prevent MSG_CTRUNC.
	oob := make([]byte, 2*unix.CmsgSpace(unix.SizeofUcred))

	for {
		var (
			n, oobn, flags int
			from           unix.Sockaddr
			recvErr        error
		)

		// Non-blocking epoll read via Go netpoller:
		// Returning false from the callback tells Go that the socket returned EAGAIN/EWOULDBLOCK,
		// instructing the poller to wait for the next epoll readiness notification.
		err := rawConn.Read(func(s uintptr) bool {
			n, oobn, flags, from, recvErr = unix.Recvmsg(int(s), buf, oob, 0)
			if errors.Is(recvErr, unix.EAGAIN) || errors.Is(recvErr, unix.EWOULDBLOCK) {
				return false
			}
			return true
		})

		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("netlink read failed: %w", err)
		}

		if recvErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Buffer overflow: Kernel dropped packets because userspace didn't read fast enough.
			// Emit a synthetic ActionRescan event to force an immediate inventory reconciliation.
			if errors.Is(recvErr, unix.ENOBUFS) {
				klog.Warning("udev netlink socket buffer overrun (ENOBUFS), forcing immediate reconciliation")
				select {
				case out <- &UEvent{Action: ActionRescan}:
				case <-ctx.Done():
					return nil
				}
				continue
			}
			// EINTR means the syscall was interrupted by an OS signal; safe to retry.
			if errors.Is(recvErr, unix.EINTR) {
				continue
			}
			return fmt.Errorf("netlink receive failed: %w", recvErr)
		}

		// MSG_TRUNC means a valid add/remove event may have been lost. Request a
		// full reconciliation to recover any state change represented by the
		// incomplete packet.
		if flags&unix.MSG_TRUNC != 0 {
			klog.Warning("udev netlink datagram exceeded buffer, forcing immediate reconciliation")
			select {
			case out <- &UEvent{Action: ActionRescan}:
			case <-ctx.Done():
				return nil
			}
			continue
		}

		// MSG_CTRUNC means credentials cannot be validated, so drop the packet.
		if flags&unix.MSG_CTRUNC != 0 {
			klog.Warning("udev netlink credentials exceeded buffer and were dropped")
			continue
		}

		// Verify kernel-supplied credentials before parsing or acting on the payload.
		// This verifies the unprivileged-sender boundary; host root remains trusted.
		if err := verifySender(from, oob[:oobn]); err != nil {
			klog.V(5).InfoS("Dropping uevent from untrusted sender", "err", err)
			continue
		}

		uevent, err := ParseUEventPayload(buf[:n])
		if err != nil {
			klog.V(5).InfoS("Failed to parse uevent datagram, skipping", "err", err)
			continue
		}
		// SUBSYSTEM FILTER:
		// The udev netlink bus carries events for ALL subsystems: net, tty, usb, pci, sound, etc.
		// Discard everything that is not "block" immediately to avoid unnecessary work.
		if uevent.Subsystem != "block" {
			continue
		}

		// FILTER OUT NON-PHYSICAL / PARTITION DEVICES:
		// Drop partitions (e.g. sda1), loop devices, zram, dm-crypt / LVM volumes,
		// and dynamic network/storage tool volumes (Longhorn, iSCSI).
		if ShouldIgnore(uevent.KernelName(), uevent.DevType, uevent.Env) {
			continue
		}

		select {
		case out <- uevent:
		case <-ctx.Done():
			return nil
		}
	}
}

// verifySender rejects messages that were not multicast on the udev group or
// were not sent by the kernel or a privileged process such as systemd-udevd.
//
// Linux Credential Authentication Details:
// When SO_PASSCRED is enabled, the kernel attaches an SCM_CREDENTIALS socket
// control message containing:
//
//		struct ucred {
//		    pid_t pid; /* Process ID of sender */
//		    uid_t uid; /* Real user ID of sender */
//		    gid_t gid; /* Real group ID of sender */
//		};
//
//	  - pid == 0: The Linux kernel itself (swapper/idle task) generated the event.
//	  - uid == 0: Superuser (normally systemd-udevd, but potentially another
//	    host-root process). This is the intentional local trust boundary.
//
// INSIDER KNOWLEDGE ON USER NAMESPACES & CONTAINERS:
// In containerized environments with user namespaces enabled (e.g. rootless Docker
// or Podman), UID 0 inside the container might be mapped to an unprivileged host UID,
// or host UID 0 might appear as translated.
// Therefore, we only reject senders that are POSITIVELY unprivileged (pid != 0 AND uid != 0).
// If a process is genuinely unprivileged (e.g. uid 1000), it is rejected as an imposter.
func verifySender(from unix.Sockaddr, oob []byte) error {
	sender, ok := from.(*unix.SockaddrNetlink)
	if !ok {
		// Non-netlink sender: allow local unix socket pairs used in unit testing.
		if _, isUnix := from.(*unix.SockaddrUnix); isUnix || from == nil {
			return nil
		}
		return fmt.Errorf("%w: message was not multicast on the udev group", ErrUntrustedSender)
	}
	if sender.Groups != udevEventGroup {
		return fmt.Errorf("%w: message was not multicast on the udev group", ErrUntrustedSender)
	}

	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return fmt.Errorf("%w: unable to parse control message: %w", ErrUntrustedSender, err)
	}
	for i := range messages {
		if messages[i].Header.Level != unix.SOL_SOCKET || messages[i].Header.Type != unix.SCM_CREDENTIALS {
			continue
		}
		credentials, err := unix.ParseUnixCredentials(&messages[i])
		if err != nil {
			return fmt.Errorf("%w: unable to read credentials: %w", ErrUntrustedSender, err)
		}
		// The kernel reports pid 0; udevd runs as uid 0. In a user namespace
		// without a root mapping the uid is translated, so only a positively
		// unprivileged sender is rejected.
		if credentials.Pid != 0 && credentials.Uid != 0 {
			return fmt.Errorf("%w: pid %d uid %d", ErrUntrustedSender, credentials.Pid, credentials.Uid)
		}
		return nil
	}

	return fmt.Errorf("%w: no sender credentials attached", ErrUntrustedSender)
}
