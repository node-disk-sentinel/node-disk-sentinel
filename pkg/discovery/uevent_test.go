// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/binary"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newUdevMessage builds a message in the layout of struct monitor_netlink_header
// followed by the NUL separated properties.
func newUdevMessage(payload string) []byte {
	msg := make([]byte, minHeader)
	copy(msg, prefix)
	binary.BigEndian.PutUint32(msg[magicOff:], magic)
	binary.NativeEndian.PutUint32(msg[12:], minHeader) // header_size
	binary.NativeEndian.PutUint32(msg[propsOff:], minHeader)
	return append(msg, payload...)
}

func blockEvent(action, name string) []byte {
	return newUdevMessage("ACTION=" + action + "\x00DEVPATH=/devices/pci0000:00/block/" + name +
		"\x00SUBSYSTEM=block\x00DEVNAME=/dev/" + name + "\x00DEVTYPE=disk\x00")
}

func TestParseMessage(t *testing.T) {
	env, err := parseMessage(newUdevMessage("ACTION=add\x00DEVPATH=/devices/x/block/sda\x00SUBSYSTEM=block\x00DEVTYPE=disk\x00ID_MODEL=A=B"))
	if err != nil {
		t.Fatalf("parseMessage failed: %v", err)
	}
	want := map[string]string{"ACTION": "add", "DEVPATH": "/devices/x/block/sda", "SUBSYSTEM": "block", "DEVTYPE": "disk", "ID_MODEL": "A=B"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q; want %q", k, env[k], v)
		}
	}
}

// TestParseMessageRealDatagram decodes a message captured from systemd-udevd
// 255.4 (Ubuntu 24.04) on the udev group while a loop device was set up with
// udisksctl. Unlike newUdevMessage it does not depend on our reading of the
// format; the expected values are what "udevadm monitor --udev --property"
// printed for the same event.
func TestParseMessageRealDatagram(t *testing.T) {
	raw, err := os.ReadFile("testdata/udevd-change-loop0.bin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := parseMessage(raw)
	if err != nil {
		t.Fatalf("parseMessage failed: %v", err)
	}
	want := map[string]string{
		"ACTION": "change", "DEVPATH": "/devices/virtual/block/loop0", "SUBSYSTEM": "block",
		"DEVNAME": "/dev/loop0", "DEVTYPE": "disk", "MAJOR": "7", "MINOR": "0", "SEQNUM": "7073",
		"ID_LOOP_BACKING_DEVICE": "259:1", "TAGS": ":systemd:",
		"DEVLINKS": "/dev/disk/by-loop-inode/259:1-39720277 /dev/disk/by-diskseq/14",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q; want %q", k, env[k], v)
		}
	}
	if len(env) != 16 {
		t.Errorf("got %d properties; want 16: %v", len(env), env)
	}
}

func TestParseMessageRejects(t *testing.T) {
	badMagic := newUdevMessage("ACTION=add\x00")
	binary.BigEndian.PutUint32(badMagic[magicOff:], 1)
	offsetOutOfRange := newUdevMessage("ACTION=add\x00")
	binary.NativeEndian.PutUint32(offsetOutOfRange[propsOff:], 4096)
	offsetInHeader := newUdevMessage("ACTION=add\x00")
	binary.NativeEndian.PutUint32(offsetInHeader[propsOff:], 20)

	tests := []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{"empty", nil, "not a libudev message"},
		{"raw kernel event", []byte("add@/devices/virtual/block/sda\x00ACTION=add\x00SUBSYSTEM=block\x00"), "not a libudev message"},
		{"truncated header", []byte(prefix), "not a libudev message"},
		{"bad magic", badMagic, "invalid libudev magic"},
		{"offset inside header", offsetInHeader, "invalid properties offset"},
		{"offset beyond datagram", offsetOutOfRange, "invalid properties offset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseMessage(tt.raw); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsDiskChange(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"disk added", map[string]string{"ACTION": "add", "SUBSYSTEM": "block", "DEVTYPE": "disk", "DEVPATH": "/devices/x/block/sda"}, true},
		{"disk removed", map[string]string{"ACTION": "remove", "SUBSYSTEM": "block", "DEVTYPE": "disk", "DEVPATH": "/devices/x/block/nvme0n1"}, true},
		{"change is ignored", map[string]string{"ACTION": "change", "SUBSYSTEM": "block", "DEVTYPE": "disk", "DEVPATH": "/devices/x/block/sda"}, false},
		{"partition", map[string]string{"ACTION": "add", "SUBSYSTEM": "block", "DEVTYPE": "partition", "DEVPATH": "/devices/x/block/sda/sda1"}, false},
		{"other subsystem", map[string]string{"ACTION": "add", "SUBSYSTEM": "net", "DEVPATH": "/devices/x/net/eth0"}, false},
		{"loop device", map[string]string{"ACTION": "add", "SUBSYSTEM": "block", "DEVTYPE": "disk", "DEVPATH": "/devices/virtual/block/loop3"}, false},
		{"iscsi disk", map[string]string{"ACTION": "add", "SUBSYSTEM": "block", "DEVTYPE": "disk", "DEVPATH": "/devices/x/block/sdb", "ID_BUS": "iscsi"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDiskChange(tt.env); got != tt.want {
				t.Errorf("isDiskChange() = %t; want %t", got, tt.want)
			}
		})
	}
}

// startListen runs listen against one end of a SOCK_SEQPACKET socketpair, which
// keeps message boundaries like netlink datagrams without needing privileges. It
// returns the sending end, a trigger counter and the result channel of listen.
func startListen(t *testing.T, ctx context.Context) (tx *os.File, triggers *atomic.Int32, done chan error) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	rx := os.NewFile(uintptr(fds[0]), "rx")
	tx = os.NewFile(uintptr(fds[1]), "tx")
	t.Cleanup(func() { _ = tx.Close() })

	triggers = new(atomic.Int32)
	done = make(chan error, 1)
	go func() { done <- listen(ctx, rx, func() { triggers.Add(1) }) }()
	return tx, triggers, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestListen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, triggers, done := startListen(t, ctx)

	// A bound socket triggers once, so changes before the bind are picked up.
	waitFor(t, "trigger after bind", func() bool { return triggers.Load() == 1 })

	// None of these may trigger.
	for _, msg := range [][]byte{
		[]byte("garbage"),
		newUdevMessage("ACTION=add\x00DEVPATH=/devices/pci/net/eth0\x00SUBSYSTEM=net\x00"),
		blockEvent("change", "sda"),
	} {
		if _, err := tx.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Events are processed in order, so once this one is seen all before it were.
	if _, err := tx.Write(blockEvent("add", "sda")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "trigger for add", func() bool { return triggers.Load() >= 2 })
	if got := triggers.Load(); got != 2 {
		t.Errorf("triggers = %d; want 2 (bind, add)", got)
	}

	if _, err := tx.Write(blockEvent("remove", "sda")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "trigger for remove", func() bool { return triggers.Load() == 3 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("listen returned %v on cancel; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listen did not return after cancel")
	}
}

func TestListenRescansAfterTruncatedDatagram(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, triggers, _ := startListen(t, ctx)
	waitFor(t, "trigger after bind", func() bool { return triggers.Load() == 1 })

	if _, err := tx.Write(make([]byte, maxMsg+1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "rescan after truncation", func() bool { return triggers.Load() == 2 })
}

// dialInPrivateNetns returns a socket from dial and a second one that sends to
// the udev group, both in a new network namespace. That isolates the test from
// the real udev events of the host, even when it runs as root. Creating the
// namespace needs CAP_SYS_ADMIN, e.g. "unshare -Ur go test ./pkg/discovery".
func dialInPrivateNetns(t *testing.T) (rx *os.File, send func(msg []byte)) {
	t.Helper()
	type result struct {
		rx  *os.File
		fd  int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// The thread is never unlocked, so it ends with this goroutine instead
		// of returning to the pool while it is in the foreign namespace.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			ch <- result{err: err}
			return
		}
		rx, err := dial()
		if err != nil {
			ch <- result{err: err}
			return
		}
		fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
		if err != nil {
			_ = rx.Close()
		}
		ch <- result{rx, fd, err}
	}()

	r := <-ch
	if r.err != nil {
		t.Skipf("cannot create a network namespace: %v", r.err)
	}
	t.Cleanup(func() { _ = unix.Close(r.fd) })
	return r.rx, func(msg []byte) {
		t.Helper()
		// Only a privileged process may send to a netlink multicast group.
		if err := unix.Sendto(r.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groupUdev}); err != nil {
			t.Fatalf("send to udev group: %v", err)
		}
	}
}

// TestListenOnNetlink runs listen against a real netlink socket that receives
// from the multicast group like in production.
func TestListenOnNetlink(t *testing.T) {
	rx, send := dialInPrivateNetns(t)
	var triggers atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- listen(ctx, rx, func() { triggers.Add(1) }) }()
	waitFor(t, "trigger after bind", func() bool { return triggers.Load() == 1 })

	send([]byte("garbage"))
	send(blockEvent("change", "sda"))
	send(blockEvent("add", "sda"))
	waitFor(t, "trigger for add", func() bool { return triggers.Load() >= 2 })
	send(blockEvent("remove", "sda"))
	waitFor(t, "trigger for remove", func() bool { return triggers.Load() == 3 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("listen returned %v on cancel; want nil", err)
	}
}

// TestListenRescansAfterOverflow fills the receive queue of a real netlink
// socket, which makes the kernel drop messages and fail the next read with
// ENOBUFS, and expects a rescan trigger plus a working socket afterwards.
func TestListenRescansAfterOverflow(t *testing.T) {
	rx, send := dialInPrivateNetns(t)

	// A small queue overflows after a few messages.
	rc, err := rx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var sockErr error
	if err := rc.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
	}); err != nil || sockErr != nil {
		t.Fatalf("shrink receive buffer: %v %v", err, sockErr)
	}

	// listen is held in its first trigger, so nothing is read while the queue
	// is filled with messages that do not trigger by themselves.
	var triggers atomic.Int32
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- listen(ctx, rx, func() {
			if triggers.Add(1) == 1 {
				<-release
			}
		})
	}()
	waitFor(t, "trigger after bind", func() bool { return triggers.Load() == 1 })

	netEvent := newUdevMessage("ACTION=add\x00DEVPATH=/devices/pci/net/eth0\x00SUBSYSTEM=net\x00")
	for range 200 {
		send(netEvent)
	}
	close(release)

	waitFor(t, "rescan after overflow", func() bool { return triggers.Load() == 2 })
	send(blockEvent("add", "sda"))
	waitFor(t, "trigger for add after overflow", func() bool { return triggers.Load() == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("listen returned %v on cancel; want nil", err)
	}
}

// Binding the real netlink group needs no privileges (the uevent socket sets
// NL_CFG_F_NONROOT_RECV); a bound Monitor triggers once before it sees any event.
func TestMonitorTriggersAfterBindAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var triggers atomic.Int32
	done := make(chan error, 1)
	go func() { done <- Monitor(ctx, func() { triggers.Add(1) }) }()

	deadline := time.Now().Add(2 * time.Second)
	for triggers.Load() == 0 {
		select {
		case err := <-done:
			t.Skipf("netlink socket unavailable: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Monitor did not trigger after binding")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Monitor returned %v on cancel; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Monitor did not return after cancel")
	}
}
