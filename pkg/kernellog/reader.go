// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
)

const (
	defaultKmsgPath = "/dev/kmsg"
	kmsgBufferSize  = 8192
)

// KmsgReader reads kernel log messages directly from /dev/kmsg.
type KmsgReader struct {
	file   *os.File
	lines  chan string
	errs   chan error
	done   chan struct{}
	closed atomic.Bool
}

// NewKmsgReader opens the specified kmsg path (or /dev/kmsg by default)
// and prepares it for streaming.
func NewKmsgReader(path string) (*KmsgReader, error) {
	if path == "" {
		path = defaultKmsgPath
	}

	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open kernel message file %s: %w", path, err)
	}

	// Seek to end of buffer so historic boot messages are not replayed as new events.
	// We ignore seek errors if the underlying file doesn't support seeking (e.g. pipes/fifos in tests).
	_, _ = file.Seek(0, io.SeekEnd)

	r := &KmsgReader{
		file:  file,
		lines: make(chan string, 128),
		errs:  make(chan error, 1),
		done:  make(chan struct{}),
	}

	go r.readLoop()
	return r, nil
}

func (r *KmsgReader) readLoop() {
	defer close(r.lines)
	buf := make([]byte, kmsgBufferSize)

	for {
		n, err := r.file.Read(buf)
		if err != nil {
			// EPIPE indicates the kernel ring buffer wrapped and dropped records.
			// The reader must continue reading next available record.
			if errors.Is(err, syscall.EPIPE) {
				continue
			}
			if r.closed.Load() || errors.Is(err, os.ErrClosed) {
				return
			}
			select {
			case r.errs <- err:
			case <-r.done:
			}
			return
		}

		line := string(buf[:n])
		select {
		case r.lines <- line:
		case <-r.done:
			return
		}
	}
}

// ReadMessage blocks until a message is received or ctx is canceled.
func (r *KmsgReader) ReadMessage(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-r.errs:
		return "", err
	case line, ok := <-r.lines:
		if !ok {
			return "", io.EOF
		}
		return line, nil
	}
}

// Close releases the kmsg file descriptor and terminates the reader loop.
func (r *KmsgReader) Close() error {
	if r.closed.CompareAndSwap(false, true) {
		close(r.done)
		return r.file.Close()
	}
	return nil
}

// ChannelReader implements Reader backed by a channel, suitable for testing.
type ChannelReader struct {
	C chan string
}

// NewChannelReader creates a new ChannelReader.
func NewChannelReader(buffer int) *ChannelReader {
	return &ChannelReader{C: make(chan string, buffer)}
}

// ReadMessage returns the next message from the channel or errors on ctx.Done.
func (c *ChannelReader) ReadMessage(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case msg, ok := <-c.C:
		if !ok {
			return "", io.EOF
		}
		return msg, nil
	}
}

// Close closes the underlying channel.
func (c *ChannelReader) Close() error {
	return nil
}
