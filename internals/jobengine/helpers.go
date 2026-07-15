package jobengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// run executes a command with a context (for cancellation) and a 10-minute timeout.
func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", name, string(out))
	}
	return nil
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
