// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"context"
	"errors"
	"io"
)

// Monitor reads kernel messages from a Reader, parses disk errors using DefaultRules,
// filters for relevant devices, and emits detected KernelErrors to a channel.
type Monitor struct {
	reader Reader
	filter func(dev string) bool
}

// NewMonitor creates a new kernel log monitor. A nil filter accepts every device.
func NewMonitor(reader Reader, filter func(dev string) bool) *Monitor {
	return &Monitor{reader: reader, filter: filter}
}

// Listen streams kernel log entries, parses them, and delivers matched KernelErrors to out.
// It stops when ctx is canceled or when the underlying reader returns an error.
func (m *Monitor) Listen(ctx context.Context, out chan<- *KernelError) error {
	defer func() {
		_ = m.reader.Close()
	}()

	for {
		line, err := m.reader.ReadMessage(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}

		kErr, ok := ParseKernelMessage(line)
		if !ok {
			continue
		}

		if m.filter != nil && !m.filter(kErr.Device) {
			continue
		}

		select {
		case out <- kErr:
		case <-ctx.Done():
			return nil
		}
	}
}
