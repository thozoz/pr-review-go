package sandbox

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestLimitedCollector_RetainsCapAndDiscardsExcess(t *testing.T) {
	capBytes := 1024 // 1 KiB for test
	c := NewLimitedCollector(capBytes)

	// Write 10 KiB
	chunk := bytes.Repeat([]byte("a"), 512)
	for i := 0; i < 20; i++ {
		n, err := c.Write(chunk)
		if err != nil {
			t.Fatalf("unexpected write error: %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("expected write to report full length %d, got %d", len(chunk), n)
		}
	}

	if len(c.Bytes()) != capBytes {
		t.Fatalf("expected exactly %d retained bytes, got %d", capBytes, len(c.Bytes()))
	}
	if !c.Truncated() {
		t.Fatalf("expected Truncated() to be true")
	}
	expectedTotal := int64(20 * 512)
	expectedDropped := expectedTotal - int64(capBytes)
	if c.TotalBytes() != expectedTotal {
		t.Fatalf("expected TotalBytes %d, got %d", expectedTotal, c.TotalBytes())
	}
	if c.DroppedBytes() != expectedDropped {
		t.Fatalf("expected DroppedBytes %d, got %d", expectedDropped, c.DroppedBytes())
	}

	formatted := c.Formatted("stdout")
	if !strings.Contains(formatted, "stdout truncated") {
		t.Fatalf("expected formatted string to indicate truncation, got: %s", formatted)
	}
	if !strings.Contains(formatted, fmt.Sprintf("dropped %d bytes", expectedDropped)) {
		t.Fatalf("expected formatted string to indicate dropped bytes, got: %s", formatted)
	}
}

func TestLimitedCollector_ConcurrentWritesDoNotDeadlockOrExceedCap(t *testing.T) {
	c := NewLimitedCollector(10 * 1024) // 10 KiB cap

	const goroutines = 10
	const writesPerGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte(fmt.Sprintf("%d", id%10)), 256)
			for w := 0; w < writesPerGoroutine; w++ {
				_, _ = c.Write(payload)
			}
		}(g)
	}

	wg.Wait()

	if len(c.Bytes()) > 10*1024 {
		t.Fatalf("retained bytes %d exceeded limit of %d", len(c.Bytes()), 10*1024)
	}
	if !c.Truncated() {
		t.Fatalf("expected output to be truncated")
	}
	if c.TotalBytes() != int64(goroutines*writesPerGoroutine*256) {
		t.Fatalf("unexpected total bytes: %d", c.TotalBytes())
	}
}

func TestLimitedCollector_StreamManyMiB_ConstantMemoryAndNoDeadlock(t *testing.T) {
	c := NewLimitedCollector(DefaultMaxRetainedBytes) // 100 KiB cap

	const totalMiB = 10
	const chunkSize = 64 * 1024 // 64 KiB chunks
	chunk := bytes.Repeat([]byte("X"), chunkSize)

	totalBytes := int64(totalMiB * 1024 * 1024)
	numChunks := int(totalBytes / chunkSize)

	for i := 0; i < numChunks; i++ {
		n, err := c.Write(chunk)
		if err != nil {
			t.Fatalf("unexpected error at chunk %d: %v", i, err)
		}
		if n != chunkSize {
			t.Fatalf("expected write %d bytes, got %d", chunkSize, n)
		}
	}

	if len(c.Bytes()) != DefaultMaxRetainedBytes {
		t.Fatalf("expected retained bytes exactly %d, got %d", DefaultMaxRetainedBytes, len(c.Bytes()))
	}
	expectedDropped := totalBytes - int64(DefaultMaxRetainedBytes)
	if c.DroppedBytes() != expectedDropped {
		t.Fatalf("expected dropped bytes %d, got %d", expectedDropped, c.DroppedBytes())
	}
	if c.TotalBytes() != totalBytes {
		t.Fatalf("expected total bytes %d, got %d", totalBytes, c.TotalBytes())
	}
	if !c.Truncated() {
		t.Fatalf("expected Truncated() to be true")
	}

	formatted := c.Formatted("stdout")
	if !strings.Contains(formatted, "exceeded 100 KiB cap") {
		t.Fatalf("expected formatted string to mention 100 KiB cap, got: %s", formatted)
	}
}

