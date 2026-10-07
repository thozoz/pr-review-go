package sandbox

import (
	"bytes"
	"fmt"
	"sync"
)

// DefaultMaxRetainedBytes is the 100 KiB per-stream limit defined in D-08.
const DefaultMaxRetainedBytes = 100 * 1024

// LimitedCollector is a thread-safe, non-blocking io.Writer that retains at most
// maxBytes in memory, discarding any excess bytes while tracking dropped counts
// and truncation state. This prevents unbounded buffer growth while ensuring
// streaming child processes never deadlock.
type LimitedCollector struct {
	mu           sync.Mutex
	buf          bytes.Buffer
	maxBytes     int
	droppedBytes int64
	totalBytes   int64
	truncated    bool
}

// NewLimitedCollector creates a new LimitedCollector with the specified byte cap.
// If maxBytes <= 0, DefaultMaxRetainedBytes is used.
func NewLimitedCollector(maxBytes int) *LimitedCollector {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRetainedBytes
	}
	return &LimitedCollector{
		maxBytes: maxBytes,
	}
}

// Write appends bytes up to the capacity limit and drains the remainder.
// It always reports len(p), nil to prevent caller deadlocks.
func (c *LimitedCollector) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n = len(p)
	c.totalBytes += int64(n)

	currentLen := c.buf.Len()
	if currentLen >= c.maxBytes {
		c.droppedBytes += int64(n)
		c.truncated = true
		return n, nil
	}

	remaining := c.maxBytes - currentLen
	if n <= remaining {
		c.buf.Write(p)
	} else {
		c.buf.Write(p[:remaining])
		c.droppedBytes += int64(n - remaining)
		c.truncated = true
	}

	return n, nil
}

// String returns the currently retained string content.
func (c *LimitedCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Bytes returns a copy of the currently retained bytes.
func (c *LimitedCollector) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out
}

// Truncated returns whether any bytes were discarded.
func (c *LimitedCollector) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// DroppedBytes returns the total count of bytes discarded beyond the limit.
func (c *LimitedCollector) DroppedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.droppedBytes
}

// TotalBytes returns the total count of bytes passed to Write.
func (c *LimitedCollector) TotalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalBytes
}

// Formatted returns the retained content, appending an explicit truncation notice
// if bytes were dropped.
func (c *LimitedCollector) Formatted(streamName string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.buf.String()
	if c.truncated {
		if streamName == "" {
			streamName = "output"
		}
		s += fmt.Sprintf("\n... [%s truncated, exceeded %d KiB cap, dropped %d bytes] ...",
			streamName, c.maxBytes/1024, c.droppedBytes)
	}
	return s
}
