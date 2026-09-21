package jobengine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// stubRestart swaps the restart collaborators for a test and restores them
// afterwards. Must not be used with t.Parallel (package-level vars).
func stubRestart(t *testing.T, systemd bool, systemctlOut string, systemctlErr, reexecErr error, systemctlCalls, reexecCalls *int) {
	t.Helper()
	oldUnder, oldSystemctl, oldReexec := underSystemd, runSystemctlRestart, reexecSelfFunc
	underSystemd = func() bool { return systemd }
	runSystemctlRestart = func(ctx context.Context) (string, error) {
		*systemctlCalls++
		return systemctlOut, systemctlErr
	}
	reexecSelfFunc = func() error {
		*reexecCalls++
		return reexecErr
	}
	t.Cleanup(func() {
		underSystemd, runSystemctlRestart, reexecSelfFunc = oldUnder, oldSystemctl, oldReexec
	})
}

func TestDetectSystemdViaEnv(t *testing.T) {
	t.Setenv("INVOCATION_ID", "test-invocation-id")
	t.Setenv("JOURNAL_STREAM", "")
	if !detectSystemd() {
		t.Fatal("detectSystemd should be true with INVOCATION_ID set")
	}

	t.Setenv("INVOCATION_ID", "")
	t.Setenv("JOURNAL_STREAM", "123:456")
	if !detectSystemd() {
		t.Fatal("detectSystemd should be true with JOURNAL_STREAM set")
	}
}

func TestDetectSystemdFollowsRuntimeDir(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("JOURNAL_STREAM", "")
	_, statErr := os.Stat("/run/systemd/system")
	want := statErr == nil
	if got := detectSystemd(); got != want {
		t.Fatalf("detectSystemd without env = %v, want %v (dir present: %v)", got, want, want)
	}
}

func TestRestartAgentSystemdSuccess(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, true, "", nil, nil, &syscalls, &reexecs)

	if err := RestartAgent(context.Background()); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
	if syscalls != 1 || reexecs != 0 {
		t.Fatalf("want 1 systemctl call and 0 re-execs, got %d/%d", syscalls, reexecs)
	}
}

func TestRestartAgentSystemdFailure(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, true, "System has not been booted", errors.New("exit status 1"), nil, &syscalls, &reexecs)

	err := RestartAgent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "systemctl") ||
		!strings.Contains(err.Error(), "System has not been booted") {
		t.Fatalf("want wrapped systemctl error with command output, got %v", err)
	}
	if syscalls != 1 || reexecs != 0 {
		t.Fatalf("want 1 systemctl call and 0 re-execs, got %d/%d", syscalls, reexecs)
	}
}

func TestRestartAgentReexec(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, false, "", nil, nil, &syscalls, &reexecs)

	if err := RestartAgent(context.Background()); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
	if syscalls != 0 || reexecs != 1 {
		t.Fatalf("want 0 systemctl calls and 1 re-exec, got %d/%d", syscalls, reexecs)
	}
}

func TestRestartAgentReexecFailure(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, false, "", nil, errors.New("exec /usr/local/bin/ekilied: permission denied"), &syscalls, &reexecs)

	err := RestartAgent(context.Background())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want re-exec error, got %v", err)
	}
}

// finishSelfUpdate under systemd: restart first, then report success.
func TestFinishSelfUpdateSystemd(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, true, "", nil, nil, &syscalls, &reexecs)

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	lb := NewLogBatcher(context.Background(), 77, client)
	defer lb.Close()
	logf := func(format string, args ...any) {}

	if err := e.finishSelfUpdate(context.Background(), 77, "self_update", lb, logf); err != nil {
		t.Fatalf("finishSelfUpdate: %v", err)
	}
	if syscalls != 1 {
		t.Fatalf("want 1 systemctl call, got %d", syscalls)
	}
	if got := client.successCompletions(); len(got) != 1 || got[0].jobID != 77 {
		t.Fatalf("want exactly 1 success completion for job 77, got %+v", got)
	}
}

// Regression test for ekilie/ekilied#47: a failed restart must surface as an
// error (so the caller reports the job as failed) instead of being ignored.
func TestFinishSelfUpdateRestartFailure(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, true, "unit not found", errors.New("exit status 1"), nil, &syscalls, &reexecs)

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	lb := NewLogBatcher(context.Background(), 78, client)
	defer lb.Close()
	logf := func(format string, args ...any) {}

	err := e.finishSelfUpdate(context.Background(), 78, "self_update", lb, logf)
	if err == nil || !strings.Contains(err.Error(), "restart agent") {
		t.Fatalf("want restart error, got %v", err)
	}
	if got := client.successCompletions(); len(got) != 0 {
		t.Fatalf("failed restart must not report success, got %+v", got)
	}
}

// Non-systemd path: re-exec never returns on success, so the success report
// must already be out before RestartAgent runs.
func TestFinishSelfUpdateReexecReportsFirst(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, false, "", nil, nil, &syscalls, &reexecs)

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	lb := NewLogBatcher(context.Background(), 79, client)
	defer lb.Close()
	logf := func(format string, args ...any) {}

	if err := e.finishSelfUpdate(context.Background(), 79, "self_update", lb, logf); err != nil {
		t.Fatalf("finishSelfUpdate: %v", err)
	}
	if reexecs != 1 {
		t.Fatalf("want 1 re-exec, got %d", reexecs)
	}
	if got := client.successCompletions(); len(got) != 1 {
		t.Fatalf("want success reported before re-exec, got %+v", got)
	}
}

// Documents the known tradeoff: if the re-exec fails, success was already
// reported (it cannot be known to fail before attempting it). The returned
// error ensures loud logs and a failed job trail on the systemd path; here
// the caller still sees the error for logging.
func TestFinishSelfUpdateReexecFailureReturnsError(t *testing.T) {
	var syscalls, reexecs int
	stubRestart(t, false, "", nil, errors.New("exec: permission denied"), &syscalls, &reexecs)

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	lb := NewLogBatcher(context.Background(), 80, client)
	defer lb.Close()
	logf := func(format string, args ...any) {}

	if err := e.finishSelfUpdate(context.Background(), 80, "self_update", lb, logf); err == nil {
		t.Fatal("want re-exec error, got nil")
	}
}
