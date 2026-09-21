package jobengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// systemctlTimeout bounds the `systemctl --no-block restart` client call.
// With --no-block systemd only queues the restart job, so this returns fast;
// the timeout is purely a safety net for a wedged dbus.
const systemctlTimeout = 30 * time.Second

// underSystemd reports whether the agent runs under systemd. It is a variable
// (not a direct call) so tests can stub the environment.
var underSystemd = detectSystemd

// runSystemctlRestart runs `systemctl --no-block restart ekilied` and returns
// its trimmed combined output. Variable so tests can stub it without touching
// the host init system.
var runSystemctlRestart = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	// --no-block matters: a blocking restart would SIGTERM our own process
	// mid-call, so code after this point (like the job success report)
	// would never run.
	out, err := exec.CommandContext(ctx, "systemctl", "--no-block", "restart", "ekilied").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// reexecSelfFunc replaces the running process with the current binary.
// Variable so tests can stub it (a real re-exec would replace the test binary).
var reexecSelfFunc = reexecSelf

// detectSystemd reports whether the current process is managed by systemd,
// via the env vars systemd sets for managed units and the well-known runtime
// directory. Either signal is sufficient.
func detectSystemd() bool {
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	if os.Getenv("JOURNAL_STREAM") != "" {
		return true
	}
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// RestartAgent restarts the agent process so a newly installed binary takes
// effect.
//
// Under systemd it queues `systemctl restart ekilied` (non-blocking) and
// returns; the caller must report job success BEFORE the process is stopped.
//
// Without systemd it re-execs the current binary in place via syscall.Exec:
// on success it never returns, so the caller must report job success BEFORE
// calling it. It returns an error only when the restart could not even be
// attempted, in which case the old binary keeps running.
func RestartAgent(ctx context.Context) error {
	if !underSystemd() {
		log.Printf("[update] not running under systemd, re-execing new binary...")
		return reexecSelfFunc()
	}
	out, err := runSystemctlRestart(ctx)
	if err != nil {
		if out != "" {
			return fmt.Errorf("systemctl restart ekilied: %s: %w", out, err)
		}
		return fmt.Errorf("systemctl restart ekilied: %w", err)
	}
	return nil
}

// finishSelfUpdate reports a successful self-update job and restarts the agent
// so the new binary takes effect.
//
// Ordering is restart-mode dependent: a systemd restart queues first and the
// process is stopped right after, while a re-exec replaces the process image
// and never returns on success. Either way the success report must precede
// the point of no return. Returns nil when the update was reported (or the
// process is being replaced); returns an error when the restart failed, in
// which case the caller must report the job as failed.
func (e *JobEngine) finishSelfUpdate(ctx context.Context, jobID uint, action string, lb *LogBatcher, logf func(string, ...any)) error {
	logf("[update] updated successfully, restarting...")
	lb.flushNow()

	reportSuccess := func() {
		if err := e.client.CompleteJob(ctx, jobID, "success", "", action, nil); err != nil {
			log.Printf("complete job %d failed: %v", jobID, err)
		}
	}

	if !underSystemd() {
		// Re-exec never returns on success: the result must go out first.
		// The binary was stat-checked before the swap, so a failure here is
		// unexpected; it is returned (and logged loudly) below.
		reportSuccess()
	}
	if err := RestartAgent(ctx); err != nil {
		return fmt.Errorf("restart agent: %w", err)
	}
	if underSystemd() {
		// Restart queued; report before systemd stops this process.
		reportSuccess()
	}
	return nil
}
