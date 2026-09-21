package jobengine

import (
	"context"
	"fmt"
	"strings"
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
	claimJob    *dtos.JobItem
	claimErr    error
	claimBlock  bool // when true, ClaimJob blocks until its context is done
	callOrder   []string
}

type completedCall struct {
	jobID  uint
	status string
	errMsg string
}

func (f *fakeJobClient) record(call string) {
	f.callOrder = append(f.callOrder, call)
}

func (f *fakeJobClient) ClaimJob(ctx context.Context, jobID uint) (*dtos.JobItem, error) {
	f.mu.Lock()
	f.record("claim")
	block := f.claimBlock
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.claimJob != nil {
		return f.claimJob, nil
	}
	return &dtos.JobItem{ID: jobID}, nil
}

func (f *fakeJobClient) StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stream")
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
	f.record("complete")
	f.completions = append(f.completions, completedCall{jobID: jobID, status: status, errMsg: errorMsg})
	return nil
}

func (f *fakeJobClient) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.callOrder...)
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

func (f *fakeJobClient) allSequences() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint64
	for _, b := range f.batches {
		for _, l := range b {
			out = append(out, l.Sequence)
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

// Regression test for ekilie/ekilied#2: sequences must keep counting across
// flushes instead of restarting at 1 when the buffer is emptied.
func TestLogBatcherSequenceMonotonicAcrossFlushes(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 42, client)

	const total = 250
	const perFlush = 40
	for i := 0; i < total; i++ {
		if _, err := fmt.Fprintf(lb, "line-%d\n", i); err != nil {
			t.Fatalf("write line %d: %v", i, err)
		}
		if (i+1)%perFlush == 0 {
			lb.flushNow()
		}
	}
	lb.Close()

	if got := client.totalLines(); got != total {
		t.Fatalf("delivered %d lines, want %d", got, total)
	}
	if got := len(client.batches); got < 2 {
		t.Fatalf("expected multiple flushed batches, got %d", got)
	}

	seqs := client.allSequences()
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("sequence %d = %d, want %d (sequences must be strictly increasing across flushes)", i, seq, i+1)
		}
	}
}

// All writers (Write, WriteErr, Writef, heartbeat) must share one counter.
func TestLogBatcherSequenceSharedAcrossWriters(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 43, client)

	if _, err := fmt.Fprint(lb, "stdout-1\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := lb.WriteErr([]byte("stderr-1\n")); err != nil {
		t.Fatalf("WriteErr: %v", err)
	}
	lb.Writef("info", "system", "system-%d", 1)

	// Force the heartbeat condition without waiting 30 seconds.
	lb.lastAppend = time.Now().Add(-time.Minute)
	lb.maybeWriteHeartbeat(time.Now())

	lb.flushNow()

	if _, err := fmt.Fprint(lb, "stdout-2\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	lb.Writef("info", "system", "system-%d", 2)
	lb.Close()

	want := []string{"stdout-1", "stderr-1", "system-1", "", "stdout-2", "system-2"}
	lines := client.allLines()
	if len(lines) != len(want) {
		t.Fatalf("delivered %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i, w := range want {
		if w == "" {
			if !strings.Contains(lines[i], "[heartbeat] still running") {
				t.Errorf("line %d = %q, want heartbeat line", i, lines[i])
			}
			continue
		}
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}

	seqs := client.allSequences()
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("sequence %d = %d, want %d (writers must share one counter)", i, seq, i+1)
		}
	}
}

// The heartbeat line must not be emitted while real output is recent.
func TestLogBatcherHeartbeatSuppressedByOutput(t *testing.T) {
	client := &fakeJobClient{}
	lb := NewLogBatcher(context.Background(), 44, client)

	if _, err := fmt.Fprint(lb, "recent output\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	lb.maybeWriteHeartbeat(time.Now())
	lb.Close()

	if got := client.totalLines(); got != 1 {
		t.Fatalf("delivered %d lines, want 1 (no heartbeat expected)", got)
	}
}
