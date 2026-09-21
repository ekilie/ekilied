package jobengine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ekilie/ekilied/internals/dtos"
)

// fakeJobClient records every StreamLogs batch and whether the context was
// already cancelled when the call arrived. It mimics the real WSClient, which
// builds its HTTP request with the passed context and therefore fails
// immediately on a cancelled one.
type fakeJobClient struct {
	mu          sync.Mutex
	batches     [][]dtos.LogLine
	ctxErrs     []error
	completions []completedCall
	streamCh    chan struct{} // closed on every StreamLogs call, if set
}

type completedCall struct {
	jobID  uint
	status string
	errMsg string
}

func (f *fakeJobClient) ClaimJob(ctx context.Context, jobID uint) (*dtos.JobItem, error) {
	return &dtos.JobItem{ID: jobID}, nil
}

func (f *fakeJobClient) StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	cp := append([]dtos.LogLine(nil), lines...)
	f.batches = append(f.batches, cp)
	if f.streamCh != nil {
		close(f.streamCh)
		f.streamCh = nil
	}
	return nil
}

func (f *fakeJobClient) CompleteJob(ctx context.Context, jobID uint, status, errorMsg, step string, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completions = append(f.completions, completedCall{jobID: jobID, status: status, errMsg: errorMsg})
	return nil
}

func (f *fakeJobClient) successCompletions() []completedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []completedCall
	for _, c := range f.completions {
		if c.status == "success" {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeJobClient) totalLines() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func (f *fakeJobClient) allLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.batches {
		for _, l := range b {
			out = append(out, l.Line)
		}
	}
	return out
}

func (f *fakeJobClient) cancelledCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, err := range f.ctxErrs {
		if err != nil {
			n++
		}
	}
	return n
}

func writeLines(t *testing.T, lb *LogBatcher, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := fmt.Fprintf(lb, "%s-%d\n", prefix, i); err != nil {
			t.Fatalf("write line: %v", err)
		}
	}
}

func assertLinesInOrder(t *testing.T, got []string, prefix string, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d lines, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("%s-%d", prefix, i)
		if got[i] != want {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want)
		}
	}
}

// Regression test for ekilie/ekilied#1: lines written after the last flush
// tick must still reach StreamLogs when the batcher is closed.
func TestLogBatcherDeliversAllLinesOnClose(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 42, client)

	const n = 25
	writeLines(t, lb, "line", n)
	lb.Close()

	assertLinesInOrder(t, client.allLines(), "line", n)
	if got := client.cancelledCalls(); got != 0 {
		t.Fatalf("StreamLogs called with cancelled context %d times", got)
	}
}

// The final flush must use a live context even when Close runs after the
// parent context was cancelled (e.g. daemon shutdown racing job completion).
func TestLogBatcherCloseAfterParentCancel(t *testing.T) {
	client := &fakeJobClient{}
	parent, cancel := context.WithCancel(context.Background())
	lb := NewLogBatcher(parent, 7, client)

	const n = 10
	writeLines(t, lb, "late", n)
	cancel() // simulate shutdown racing the job
	lb.Close()

	assertLinesInOrder(t, client.allLines(), "late", n)
	if got := client.cancelledCalls(); got != 0 {
		t.Fatalf("final flush used cancelled context %d times", got)
	}
}

// Close must be idempotent: no duplicate deliveries, no panics.
func TestLogBatcherCloseIsIdempotent(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 9, client)

	const n = 5
	writeLines(t, lb, "once", n)
	lb.Close()
	lb.Close()
	lb.Close()

	assertLinesInOrder(t, client.allLines(), "once", n)
}

// Periodic ticks keep working alongside the new Close path: lines flushed by
// the ticker plus the trailing lines flushed by Close must total exactly N.
func TestLogBatcherPeriodicFlushThenClose(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 11, client)

	const n = 8
	writeLines(t, lb, "tick", n)

	deadline := time.Now().Add(3 * time.Second)
	for client.totalLines() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if client.totalLines() == 0 {
		t.Fatal("periodic flush delivered nothing within 3s")
	}

	lb.Close()
	assertLinesInOrder(t, client.allLines(), "tick", n)
	if got := client.cancelledCalls(); got != 0 {
		t.Fatalf("StreamLogs called with cancelled context %d times", got)
	}
}
