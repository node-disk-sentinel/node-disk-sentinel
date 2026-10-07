// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// The monitor listens to udev's netlink multicast group, not to the raw kernel
// group. The kernel announces a new disk (group 1) before udev has run its
// rules, so its /dev/disk/* symlinks and its /run/udev/data record do not exist
// yet. systemd-udevd writes group 2 only after it applied the rules, ran the
// RUN= commands and updated the database (worker_process_device in
// src/udev/udev-worker.c). Netlink delivers within a network namespace, so the
// host's is needed (hostNetwork).
//
// The wire format is struct monitor_netlink_header in systemd's
// src/libsystemd/sd-device/device-monitor-private.h, read by
// sd_device_monitor_receive in device-monitor.c next to it:
//
//	struct monitor_netlink_header {
//	    char prefix[8];            // "libudev\0"
//	    unsigned magic;            // htobe32(0xfeedcafe), big endian
//	    unsigned header_size;      // native endian
//	    unsigned properties_off;   // native endian, start of the KEY=VALUE block
//	    ...                        // properties_len and filter hashes, unused here
//	};
//
// followed by NUL separated "KEY=VALUE" strings. Like sd_device_monitor_receive,
// only the prefix, the magic and properties_off are evaluated.
const (
	groupUdev = 2        // MONITOR_GROUP_UDEV
	rcvBuf    = 2 << 20  // absorbs hotplug bursts; the kernel drops events when full
	maxMsg    = 32 << 10 // older libudev (v239) read into 8 KiB; longer messages are flagged MSG_TRUNC
	prefix    = "libudev\x00"
	magic     = 0xfeedcafe
	magicOff  = 8
	propsOff  = 16
	minHeader = 40
)

// Monitor calls trigger whenever a whole block device was added to or removed
// from the host, until ctx is canceled or the socket fails.
//
// Events are only a hint that the inventory changed; callers must rescan the
// authoritative sources (sysfs, udev database) instead of acting on event data.
// Netlink multicast is lossy, so trigger is also called once the socket is bound
// and whenever the kernel reports lost or truncated messages. A caller that
// rescans on every trigger therefore never misses a state change, including those
// that happen while Monitor is not running.
//
// Unlike libudev and sd-device, Monitor does not check who sent a message. It
// need not: a forged event can only cause a rescan, and processes without
// CAP_NET_ADMIN cannot send one. netlink_sendmsg refuses an explicit unicast or
// multicast destination unless the protocol sets NL_CFG_F_NONROOT_SEND, and
// NETLINK_KOBJECT_UEVENT sets only NL_CFG_F_NONROOT_RECV (unchanged since Linux
// 4.4).
func Monitor(ctx context.Context, trigger func()) error {
	f, err := dial()
	if err != nil {
		return err
	}
	return listen(ctx, f, trigger)
}

// dial opens a netlink socket that is bound to the udev group.
func dial() (*os.File, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("open netlink socket: %w", err)
	}
	// From here on the file owns fd; closing it also wakes a blocked read.
	f := os.NewFile(uintptr(fd), "uevent")

	// SO_RCVBUFFORCE ignores net.core.rmem_max but needs CAP_NET_ADMIN.
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, rcvBuf) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, rcvBuf)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groupUdev}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("bind netlink socket: %w", err)
	}
	return f, nil
}

// listen reads datagrams from f through the Go network poller until ctx is done.
// It takes ownership of f.
func listen(ctx context.Context, f *os.File, trigger func()) error {
	defer f.Close()
	// Closing f is the only way to wake the blocked read below.
	defer context.AfterFunc(ctx, func() { _ = f.Close() })()

	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}

	// Bound and ready: whatever happens from now on is queued in the socket.
	trigger()

	buf := make([]byte, maxMsg)
	for {
		var (
			n, flags int
			recvErr  error
		)
		// Returning false on EAGAIN parks the goroutine until the socket is readable.
		if err := rc.Read(func(fd uintptr) bool {
			n, _, flags, _, recvErr = unix.Recvmsg(int(fd), buf, nil, 0)
			return !errors.Is(recvErr, unix.EAGAIN)
		}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read netlink socket: %w", err)
		}

		switch {
		case errors.Is(recvErr, unix.ENOBUFS), recvErr == nil && flags&unix.MSG_TRUNC != 0:
			klog.Warning("udev events were lost, forcing a rescan")
			trigger()
			continue
		case errors.Is(recvErr, unix.EINTR):
			continue
		case recvErr != nil:
			return fmt.Errorf("receive from netlink socket: %w", recvErr)
		}

		env, err := parseMessage(buf[:n])
		if err != nil {
			klog.V(5).InfoS("Ignoring udev datagram", "err", err)
			continue
		}
		if isDiskChange(env) {
			klog.V(4).InfoS("Block device uevent", "action", env["ACTION"], "devpath", env["DEVPATH"])
			trigger()
		}
	}
}

// parseMessage returns the properties of a libudev monitor message.
func parseMessage(b []byte) (map[string]string, error) {
	if len(b) < minHeader || !bytes.HasPrefix(b, []byte(prefix)) {
		return nil, errors.New("not a libudev message")
	}
	if binary.BigEndian.Uint32(b[magicOff:]) != magic {
		return nil, errors.New("invalid libudev magic")
	}
	off := binary.NativeEndian.Uint32(b[propsOff:])
	if off < minHeader || off >= uint32(len(b)) {
		return nil, fmt.Errorf("invalid properties offset %d", off)
	}

	env := make(map[string]string)
	for _, field := range bytes.Split(b[off:], []byte{0}) {
		if key, value, ok := bytes.Cut(field, []byte{'='}); ok && len(key) > 0 {
			env[string(key)] = string(value)
		}
	}
	return env, nil
}

// isDiskChange reports whether env describes a disk that appeared or vanished.
// "change" events are deliberately ignored: they are frequent (udev emits one
// whenever a tool closes a block device it had opened for writing, see
// OPTIONS+="watch" in 60-block.rules) and never alter which disks exist; the
// periodic scan picks up changed disk data.
func isDiskChange(env map[string]string) bool {
	if env["SUBSYSTEM"] != "block" || env["DEVTYPE"] != "disk" {
		return false
	}
	if a := env["ACTION"]; a != "add" && a != "remove" {
		return false
	}
	// DEVPATH always ends in the sysfs name, the same one /sys/block uses.
	return !ShouldIgnore(path.Base(env["DEVPATH"]), env)
}
