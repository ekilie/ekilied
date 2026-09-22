package jobengine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// maxErrorTailBytes is how much trailing command output is kept for the
	// error message when a command fails. Full output is streamed to the job
	// log; only this tail is stored on the control plane.
	maxErrorTailBytes = 8 * 1024
	// maxJobErrorBytes caps the error string reported for any job, including
	// errors that do not come from run().
	maxJobErrorBytes = 4 * 1024
)

// tailWriter streams everything it receives to out (when out is non-nil) and
// keeps only the last max bytes, so a failing command can report a bounded
// tail instead of its entire output.
type tailWriter struct {
	out io.Writer
	max int
	buf []byte
}

func newTailWriter(out io.Writer, max int) *tailWriter {
	return &tailWriter{out: out, max: max}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if w.out != nil {
		// A log sink failure must not fail the command; the command's output
		// is still bounded below.
		_, _ = w.out.Write(p)
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// Tail returns the last bytes seen, trimmed for error messages.
func (w *tailWriter) Tail() string {
	return strings.TrimSpace(string(w.buf))
}

// run executes a command with a context (for cancellation) and a 10-minute
// timeout, streaming stdout and stderr to out (which may be nil, for example
// when there is no job log). On failure the returned error carries only the
// last maxErrorTailBytes of output, never the full output.
func run(ctx context.Context, out io.Writer, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	tail := newTailWriter(out, maxErrorTailBytes)
	cmd.Stdout = tail
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		if tailStr := tail.Tail(); tailStr != "" {
			return fmt.Errorf("%s failed: %w (output tail: %s)", name, err, tailStr)
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// truncateError bounds an error string that will be stored on the control
// plane, keeping the start of the message.
func truncateError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// writeFile writes content to a file with 0644 permissions.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// orDefaultStr returns s if non-empty, otherwise def.
func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// fmtBytes converts a byte count to a human-readable string.
func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats a duration as a human-readable string (e.g. "2m 35s").
func formatDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm %ds", h, m, s)
}

// splitLines splits a string into lines, preserving empty trailing content.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
