package jobengine

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Regression test for ekilie/ekilied#15: command output must stream to the
// writer while the stored error stays bounded to the output tail.
func TestRunStreamsOutputAndBoundsError(t *testing.T) {
	var streamed bytes.Buffer
	// About 20KB of stdout, a stderr marker, and a non-zero exit.
	err := run(context.Background(), &streamed, "sh", "-c", "printf '%20000s' ''; echo boom >&2; exit 7")
	if err == nil {
		t.Fatal("run = nil, want an error")
	}
	if streamed.Len() < 20000 {
		t.Fatalf("streamed %d bytes, want at least 20000", streamed.Len())
	}

	msg := err.Error()
	if !strings.Contains(msg, "boom") {
		t.Fatalf("error %q does not include the tail of the output", msg)
	}
	if !strings.Contains(msg, "exit status 7") {
		t.Fatalf("error %q does not include the exit status", msg)
	}
	if len(msg) > maxErrorTailBytes+200 {
		t.Fatalf("error length = %d, want bounded near %d", len(msg), maxErrorTailBytes)
	}
}

func TestRunSuccessStreamsOutput(t *testing.T) {
	var streamed bytes.Buffer
	if err := run(context.Background(), &streamed, "sh", "-c", "echo hello"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := streamed.String(); !strings.Contains(got, "hello") {
		t.Fatalf("streamed output = %q, want hello", got)
	}
}

// A nil writer (no job log available) must not panic or fail the command.
func TestRunNilWriter(t *testing.T) {
	err := run(context.Background(), nil, "sh", "-c", "exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("run = %v, want exit status error", err)
	}
}

func TestTailWriterKeepsLastBytes(t *testing.T) {
	var streamed bytes.Buffer
	w := newTailWriter(&streamed, 5)

	w.Write([]byte("0123456789"))
	w.Write([]byte("abc"))

	if got := streamed.String(); got != "0123456789abc" {
		t.Fatalf("streamed = %q, want the full output", got)
	}
	if got := w.Tail(); got != "89abc" {
		t.Fatalf("Tail = %q, want %q", got, "89abc")
	}
}

func TestTruncateError(t *testing.T) {
	short := "short error"
	if got := truncateError(short, 100); got != short {
		t.Fatalf("short = %q, want unchanged", got)
	}

	long := strings.Repeat("x", 100)
	got := truncateError(long, 10)
	if len(got) != 10+len("...(truncated)") {
		t.Fatalf("truncated length = %d, want %d", len(got), 10+len("...(truncated)"))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("truncated = %q, want the truncation marker", got)
	}
}
