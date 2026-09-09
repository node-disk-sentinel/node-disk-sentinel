// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// newUdevMessage builds a synthetic udev-processed netlink message matching the exact
// binary layout of struct udev_monitor_netlink_header sent by systemd-udevd:
//
//	[0..7]   "libudev\0"
//	[8..11]  0xfeedcafe (big-endian magic via htonl)
//	[12..15] header_size (40 bytes, native endian)
//	[16..19] properties_off (40 bytes, native endian)
//
// followed by the NUL-separated environment payload.
func newUdevMessage(payload string) []byte {
	message := make([]byte, libudevHeaderLength)
	copy(message, libudevPrefix)
	binary.BigEndian.PutUint32(message[libudevMagicOffset:], libudevMagic)
	binary.NativeEndian.PutUint32(message[libudevHeaderSizeOff:], libudevHeaderLength)
	binary.NativeEndian.PutUint32(message[libudevPayloadOff:], libudevHeaderLength)
	return append(message, payload...)
}

func TestParseUEventPayload_Add(t *testing.T) {
	raw := newUdevMessage("ACTION=add\x00" +
		"DEVPATH=/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda\x00" +
		"SUBSYSTEM=block\x00" +
		"DEVNAME=/dev/sda\x00" +
		"DEVTYPE=disk\x00" +
		"MAJOR=8\x00" +
		"MINOR=0\x00\x00")

	uevent, err := ParseUEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed: %v", err)
	}

	if uevent.Action != "add" {
		t.Errorf("Action = %q; want add", uevent.Action)
	}
	if uevent.Subsystem != "block" {
		t.Errorf("Subsystem = %q; want block", uevent.Subsystem)
	}
	if uevent.DevName != "/dev/sda" {
		t.Errorf("DevName = %q; want /dev/sda", uevent.DevName)
	}
	if uevent.DevType != "disk" {
		t.Errorf("DevType = %q; want disk", uevent.DevType)
	}
	if uevent.KernelName() != "sda" {
		t.Errorf("KernelName() = %q; want sda", uevent.KernelName())
	}
	if uevent.Env["MAJOR"] != "8" || uevent.Env["MINOR"] != "0" {
		t.Errorf("MAJOR/MINOR = %q/%q; want 8/0", uevent.Env["MAJOR"], uevent.Env["MINOR"])
	}
}

func TestParseUEventPayload_Remove(t *testing.T) {
	raw := newUdevMessage("ACTION=remove\x00" +
		"DEVPATH=/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sdb\x00" +
		"SUBSYSTEM=block\x00" +
		"DEVNAME=/dev/sdb\x00" +
		"DEVTYPE=disk\x00\x00")

	uevent, err := ParseUEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed: %v", err)
	}
	if uevent.Action != "remove" {
		t.Errorf("Action = %q; want remove", uevent.Action)
	}
	if uevent.KernelName() != "sdb" {
		t.Errorf("KernelName() = %q; want sdb", uevent.KernelName())
	}
}

// KernelName must still identify the device when udev omits DEVNAME, which
// happens for some remove events.
func TestUEventKernelNameFallsBackToDevPath(t *testing.T) {
	raw := newUdevMessage("ACTION=remove\x00" +
		"DEVPATH=/devices/virtual/block/nvme0n1\x00" +
		"SUBSYSTEM=block\x00" +
		"DEVTYPE=disk\x00\x00")

	uevent, err := ParseUEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed: %v", err)
	}
	if uevent.KernelName() != "nvme0n1" {
		t.Errorf("KernelName() = %q; want nvme0n1", uevent.KernelName())
	}
}

func TestParseUEventPayload_Rejects(t *testing.T) {
	invalidMagic := make([]byte, libudevHeaderLength)
	copy(invalidMagic, libudevPrefix)
	binary.NativeEndian.PutUint32(invalidMagic[libudevHeaderSizeOff:], libudevHeaderLength)
	binary.NativeEndian.PutUint32(invalidMagic[libudevPayloadOff:], libudevHeaderLength)

	badOffset := make([]byte, libudevHeaderLength)
	copy(badOffset, libudevPrefix)
	binary.BigEndian.PutUint32(badOffset[libudevMagicOffset:], libudevMagic)
	binary.NativeEndian.PutUint32(badOffset[libudevHeaderSizeOff:], libudevHeaderLength)
	binary.NativeEndian.PutUint32(badOffset[libudevPayloadOff:], 4096)

	smallHeader := newUdevMessage("ACTION=add\x00DEVPATH=/dev/sda\x00")
	binary.NativeEndian.PutUint32(smallHeader[libudevHeaderSizeOff:], 20)

	largeHeader := newUdevMessage("ACTION=add\x00DEVPATH=/dev/sda\x00")
	binary.NativeEndian.PutUint32(largeHeader[libudevHeaderSizeOff:], uint32(len(largeHeader)+100))

	smallOffset := newUdevMessage("ACTION=add\x00DEVPATH=/dev/sda\x00")
	binary.NativeEndian.PutUint32(smallOffset[libudevPayloadOff:], 20)

	tests := []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{"empty", nil, "not a udev-processed event"},
		{"raw kernel event", []byte("add@/devices/virtual/block/sda\x00ACTION=add\x00"), "not a udev-processed event"},
		{"truncated header", libudevPrefix, "truncated libudev header"},
		{"invalid magic", invalidMagic, "invalid libudev magic"},
		{"header size too small", smallHeader, "invalid libudev header size"},
		{"header size exceeds datagram", largeHeader, "invalid libudev header size"},
		{"payload offset smaller than header", smallOffset, "invalid libudev payload offset"},
		{"payload offset out of range", badOffset, "invalid libudev payload offset"},
		{"missing action", newUdevMessage("DEVPATH=/devices/pci/block/sda\x00SUBSYSTEM=block\x00"), "missing ACTION or DEVPATH"},
		{"missing devpath", newUdevMessage("ACTION=add\x00SUBSYSTEM=block\x00\x00"), "missing ACTION or DEVPATH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseUEventPayload(tt.raw); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v; want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseUEventPayload_PropertiesLen(t *testing.T) {
	raw := newUdevMessage("ACTION=add\x00" +
		"DEVPATH=/devices/virtual/block/sdd\x00" +
		"SUBSYSTEM=block\x00" +
		"DEVNAME=/dev/sdd\x00" +
		"DEVTYPE=disk\x00")
	propsLen := len(raw) - libudevHeaderLength
	binary.NativeEndian.PutUint32(raw[libudevPropsLenOff:], uint32(propsLen))
	rawWithGarbage := append(raw, []byte("GARBAGE=SHOULD_BE_IGNORED\x00")...)

	uevent, err := ParseUEventPayload(rawWithGarbage)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed: %v", err)
	}
	if uevent.Env["GARBAGE"] != "" {
		t.Errorf("expected GARBAGE to be ignored beyond properties_len, but found: %q", uevent.Env["GARBAGE"])
	}
}

func TestParseUEventPayload_OversizedPropertiesLenUsesPacketBoundary(t *testing.T) {
	raw := newUdevMessage("ACTION=add\x00" +
		"DEVPATH=/devices/virtual/block/sde\x00" +
		"SUBSYSTEM=block\x00" +
		"DEVNAME=/dev/sde\x00" +
		"DEVTYPE=disk\x00")
	binary.NativeEndian.PutUint32(raw[libudevPropsLenOff:], ^uint32(0))

	uevent, err := ParseUEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed for oversized properties length: %v", err)
	}
	if uevent.DevName != "/dev/sde" {
		t.Errorf("DevName = %q; want /dev/sde", uevent.DevName)
	}
}

func TestParseUEventPayload_NoTrailingNul(t *testing.T) {
	raw := newUdevMessage("ACTION=add\x00DEVPATH=/devices/virtual/block/sdf\x00SUBSYSTEM=block\x00DEVNAME=/dev/sdf\x00DEVTYPE=disk") // no trailing \x00
	uevent, err := ParseUEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseUEventPayload failed: %v", err)
	}
	if uevent.DevType != "disk" {
		t.Errorf("DevType = %q; want disk", uevent.DevType)
	}
}

func TestUEventListener_NewAndClose(t *testing.T) {
	listener, err := NewUEventListener()
	if err != nil {
		t.Logf("NewUEventListener returned error (expected in unprivileged environment): %v", err)
	} else {
		defer listener.Close()
		// Safe to close multiple times.
		if err := listener.Close(); err != nil {
			t.Errorf("expected close to be idempotent, got %v", err)
		}
	}

	// Close on nil file.
	nilListener := &UEventListener{file: nil}
	if err := nilListener.Close(); err != nil {
		t.Errorf("expected close on nil file to succeed, got %v", err)
	}
}

func TestUEventListener_Listen_NilListener(t *testing.T) {
	var l *UEventListener
	if err := l.Listen(context.Background(), nil); err == nil {
		t.Error("expected error for nil listener, got nil")
	}

	l2 := &UEventListener{file: nil}
	if err := l2.Listen(context.Background(), nil); err == nil {
		t.Error("expected error for listener with nil file, got nil")
	}
}

func TestUEventListener_Listen_UnexpectedSocketClose(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	rx := os.NewFile(uintptr(fds[0]), "rx")
	tx := os.NewFile(uintptr(fds[1]), "tx")
	defer tx.Close()

	listener := &UEventListener{file: rx}
	out := make(chan *UEvent, 1)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- listener.Listen(ctx, out)
	}()

	packet := newUdevMessage("ACTION=add\x00DEVPATH=/devices/pci0000:00/block/sda\x00SUBSYSTEM=block\x00DEVNAME=/dev/sda\x00DEVTYPE=disk\x00")
	if _, err := tx.Write(packet); err != nil {
		t.Fatalf("failed to write uevent: %v", err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for listener to receive uevent")
	}

	// Context is NOT cancelled, but the active socket is closed unexpectedly.
	_ = listener.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected non-nil error when socket is closed unexpectedly, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Listen() to return on unexpected socket close")
	}
}

func TestActionRescan_Constant(t *testing.T) {
	if ActionRescan != "rescan" {
		t.Errorf("ActionRescan = %q; want rescan", ActionRescan)
	}
}

// TestUEventListener_Listen_Cancel verifies clean cancellation of the listen loop.
//
// Opening genuine AF_NETLINK sockets requires the CAP_NET_ADMIN capability or root privileges.
// To test Listen() in unprivileged environments (like CI containers and user laptops),
// we use unix.Socketpair(AF_UNIX, SOCK_SEQPACKET). SOCK_SEQPACKET emulates the exact
// message-boundary-preserving datagram semantics of Netlink while operating completely in user space.
func TestUEventListener_Listen_Cancel(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	rx := os.NewFile(uintptr(fds[0]), "rx")
	tx := os.NewFile(uintptr(fds[1]), "tx")
	defer tx.Close()

	listener := &UEventListener{file: rx}
	out := make(chan *UEvent, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- listener.Listen(ctx, out)
	}()

	// Cancelling context should exit Listen cleanly without blocking.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen() returned unexpected error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Listen() to return on cancel")
	}
}

func TestUEventListener_Listen_RescansAfterTruncatedDatagram(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	rx := os.NewFile(uintptr(fds[0]), "rx")
	tx := os.NewFile(uintptr(fds[1]), "tx")
	defer tx.Close()

	listener := &UEventListener{file: rx}
	out := make(chan *UEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- listener.Listen(ctx, out)
	}()

	if _, err := tx.Write(make([]byte, readBufferSize+1)); err != nil {
		t.Fatalf("failed to write oversized uevent: %v", err)
	}

	select {
	case event := <-out:
		if event.Action != ActionRescan {
			t.Errorf("Action = %q; want %q", event.Action, ActionRescan)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rescan after truncated uevent")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen() returned unexpected error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Listen() to return on cancel")
	}
}

// TestUEventListener_Listen_ReceivesEvent verifies that valid block uevents sent over
// the socket are properly decoded and forwarded to the output channel.
func TestUEventListener_Listen_ReceivesEvent(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	rx := os.NewFile(uintptr(fds[0]), "rx")
	tx := os.NewFile(uintptr(fds[1]), "tx")
	defer tx.Close()

	listener := &UEventListener{file: rx}
	out := make(chan *UEvent, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- listener.Listen(ctx, out)
	}()

	// 1. Send invalid/non-udev packet (should be skipped silently).
	_, err = tx.Write([]byte("garbage_data"))
	if err != nil {
		t.Fatalf("failed to write garbage: %v", err)
	}

	// 2. Send non-block subsystem packet (should be dropped).
	netMsg := newUdevMessage("ACTION=add\x00DEVPATH=/devices/pci/net/eth0\x00SUBSYSTEM=net\x00DEVNAME=eth0\x00")
	_, err = tx.Write(netMsg)
	if err != nil {
		t.Fatalf("failed to write net uevent: %v", err)
	}

	// 3. Send valid block device event.
	blockMsg := newUdevMessage("ACTION=add\x00DEVPATH=/devices/pci0000:00/block/sda\x00SUBSYSTEM=block\x00DEVNAME=/dev/sda\x00DEVTYPE=disk\x00")
	_, err = tx.Write(blockMsg)
	if err != nil {
		t.Fatalf("failed to write block uevent: %v", err)
	}

	select {
	case ev := <-out:
		if ev.Action != "add" {
			t.Errorf("expected Action=add, got %q", ev.Action)
		}
		if ev.KernelName() != "sda" {
			t.Errorf("expected KernelName=sda, got %q", ev.KernelName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for block uevent")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen() returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Listen() to return on cancel")
	}
}

func TestVerifySender(t *testing.T) {
	tests := []struct {
		name string
		from unix.Sockaddr
		oob  []byte
		want error
	}{
		{
			name: "reject non-netlink sender",
			from: &unix.SockaddrInet4{},
			want: ErrUntrustedSender,
		},
		{
			name: "reject wrong multicast group",
			from: &unix.SockaddrNetlink{Groups: 1},
			want: ErrUntrustedSender,
		},
		{
			name: "reject missing credentials",
			from: &unix.SockaddrNetlink{Groups: udevEventGroup},
			want: ErrUntrustedSender,
		},
		{
			name: "accept kernel credentials",
			from: &unix.SockaddrNetlink{Groups: udevEventGroup},
			oob:  unix.UnixCredentials(&unix.Ucred{Pid: 0, Uid: 0}),
		},
		{
			name: "reject unprivileged credentials",
			from: &unix.SockaddrNetlink{Groups: udevEventGroup},
			oob:  unix.UnixCredentials(&unix.Ucred{Pid: 1234, Uid: 1000}),
			want: ErrUntrustedSender,
		},
		{
			name: "accept udev daemon root credentials",
			from: &unix.SockaddrNetlink{Groups: udevEventGroup},
			oob:  unix.UnixCredentials(&unix.Ucred{Pid: 4242, Uid: 0}),
			want: nil,
		},
		{
			name: "reject corrupt control message",
			from: &unix.SockaddrNetlink{Groups: udevEventGroup},
			oob:  []byte{1, 2, 3},
			want: ErrUntrustedSender,
		},
		{
			name: "accept mock unix socketpair in tests",
			from: &unix.SockaddrUnix{},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySender(tt.from, tt.oob)
			if !errors.Is(err, tt.want) {
				t.Errorf("verifySender() error = %v; want %v", err, tt.want)
			}
		})
	}
}
