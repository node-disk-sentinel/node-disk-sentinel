// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeReader struct {
	mu     sync.Mutex
	lines  []string
	closed bool
}

func newFakeReader(lines ...string) *fakeReader {
	return &fakeReader{lines: lines}
}

func (f *fakeReader) ReadMessage(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return "", io.EOF
	}
	if len(f.lines) == 0 {
		return "", io.EOF
	}

	line := f.lines[0]
	f.lines = f.lines[1:]
	return line, nil
}

func (f *fakeReader) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestMonitorListen(t *testing.T) {
	lines := []string{
		"blk_update_request: I/O error, dev sdb, sector 642872 op 0x1:(WRITE)",
		"Sep 25 18:46:07 titan kernel: TCP: request_sock_TCP: Possible SYN flooding",
		"sd 0:0:0:0: [sdc] tag#12 Sense Key : Medium Error [current]",
		"I/O error, dev sdd, sector 100",
	}

	reader := newFakeReader(lines...)
	// Only allow sdb and sdc.
	filter := func(dev string) bool {
		return dev == "sdb" || dev == "sdc"
	}

	monitor := NewMonitor(reader, filter)

	out := make(chan *KernelError, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := monitor.Listen(ctx, out)
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}

	close(out)

	var collected []*KernelError
	for errEvent := range out {
		collected = append(collected, errEvent)
	}

	if len(collected) != 2 {
		t.Fatalf("collected %d errors; want 2", len(collected))
	}

	if collected[0].Device != "sdb" || collected[0].Sector != 642872 || collected[0].Op != OpWrite {
		t.Errorf("first error mismatch: %+v", collected[0])
	}
	if collected[1].Device != "sdc" || collected[1].RuleID != RuleIDScsiError {
		t.Errorf("second error mismatch: %+v", collected[1])
	}
}
