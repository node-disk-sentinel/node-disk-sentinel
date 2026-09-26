// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"context"
	"errors"
	"io"
)

// Monitor reads kernel messages from a Reader, parses disk errors using configured rules,
// filters for relevant devices, and emits detected KernelErrors to a channel.
type Monitor struct {
	reader Reader
	filter func(dev string) bool
	rules  []Rule
}

// NewMonitor creates a new kernel log monitor with DefaultRules.
func NewMonitor(reader Reader, filter func(dev string) bool) *Monitor {
	return NewMonitorWithRules(reader, filter, DefaultRules)
}

// NewMonitorWithRules creates a new kernel log monitor with custom rules.
func NewMonitorWithRules(reader Reader, filter func(dev string) bool, rules []Rule) *Monitor {
	if len(rules) == 0 {
		rules = DefaultRules
	}
	return &Monitor{
		reader: reader,
		filter: filter,
		rules:  rules,
	}
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

		kErr, ok := ParseKernelMessageWithRules(line, m.rules)
		if !ok || kErr == nil {
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
