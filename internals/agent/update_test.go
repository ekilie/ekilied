package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/jobengine"
)

// stubUpdates swaps the update collaborators for a test and restores them
// afterwards. Must not be used with t.Parallel (package-level vars).
func stubUpdates(t *testing.T, check func(string, string) (*jobengine.GitHubRelease, bool, error), self func(string, *jobengine.GitHubRelease) error, restart func(context.Context) error) {
	t.Helper()
	oldCheck, oldSelf, oldRestart := checkForUpdateFunc, selfUpdateFunc, restartAgentFunc
	checkForUpdateFunc, selfUpdateFunc, restartAgentFunc = check, self, restart
	t.Cleanup(func() {
		checkForUpdateFunc, selfUpdateFunc, restartAgentFunc = oldCheck, oldSelf, oldRestart
	})
}

// newTestAgent returns an agent whose update interval is 24h, so any check
// observed in tests must be the immediate startup check, never a tick.
func newTestAgent() (*Ekilied, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Ekilied{
		cfg: &config.Config{UpdateCheckInterval: 86400},
		ctx: ctx,
	}
	return e, cancel
}

// Regression test for ekilie/ekilied#44: the first update check must happen
// at loop start, not after one full interval.
func TestUpdateCheckRunsImmediatelyOnStartup(t *testing.T) {
	checked := make(chan struct{}, 1)
	stubUpdates(t,
		func(string, string) (*jobengine.GitHubRelease, bool, error) {
			select {
			case checked <- struct{}{}:
			default:
			}
			return nil, false, nil
		},
		func(string, *jobengine.GitHubRelease) error {
			t.Error("SelfUpdate must not run when up to date")
			return nil
		},
		func(context.Context) error {
			t.Error("restart must not run when up to date")
			return nil
		},
	)

	e, cancel := newTestAgent()
	defer cancel()

	done := make(chan struct{})
	go func() { e.updateCheckLoop(); close(done) }()

	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("no update check within 2s of loop start (interval is 24h)")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}

func TestUpdateCheckAppliesReleaseAndStops(t *testing.T) {
	var selfCalls, restartCalls atomic.Int32
	stubUpdates(t,
		func(string, string) (*jobengine.GitHubRelease, bool, error) {
			return &jobengine.GitHubRelease{TagName: "v9.9.9"}, true, nil
		},
		func(string, *jobengine.GitHubRelease) error {
			selfCalls.Add(1)
			return nil
		},
		func(context.Context) error {
			restartCalls.Add(1)
			return nil
		},
	)

	e, cancel := newTestAgent()
	defer cancel()

	done := make(chan struct{})
	go func() { e.updateCheckLoop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after applying an update")
	}
	if got := selfCalls.Load(); got != 1 {
		t.Errorf("SelfUpdate calls = %d, want 1", got)
	}
	if got := restartCalls.Load(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
}

func TestUpdateCheckSkipsWhenUpdateInProgress(t *testing.T) {
	checked := make(chan struct{}, 1)
	var restartCalls atomic.Int32
	stubUpdates(t,
		func(string, string) (*jobengine.GitHubRelease, bool, error) {
			select {
			case checked <- struct{}{}:
			default:
			}
			return &jobengine.GitHubRelease{TagName: "v9.9.9"}, true, nil
		},
		func(string, *jobengine.GitHubRelease) error {
			return jobengine.ErrUpdateInProgress
		},
		func(context.Context) error {
			restartCalls.Add(1)
			return nil
		},
	)

	e, cancel := newTestAgent()
	defer cancel()

	done := make(chan struct{})
	go func() { e.updateCheckLoop(); close(done) }()

	<-checked
	time.Sleep(50 * time.Millisecond) // give the loop a chance to wrongly restart
	if got := restartCalls.Load(); got != 0 {
		t.Errorf("restart calls = %d, want 0 while another update is in progress", got)
	}
	select {
	case <-done:
		t.Fatal("loop exited after skipping an in-progress update")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}

func TestUpdateCheckFailureKeepsLoopRunning(t *testing.T) {
	checked := make(chan struct{}, 1)
	stubUpdates(t,
		func(string, string) (*jobengine.GitHubRelease, bool, error) {
			select {
			case checked <- struct{}{}:
			default:
			}
			return nil, false, errors.New("network down")
		},
		func(string, *jobengine.GitHubRelease) error {
			t.Error("SelfUpdate must not run when the check failed")
			return nil
		},
		func(context.Context) error {
			t.Error("restart must not run when the check failed")
			return nil
		},
	)

	e, cancel := newTestAgent()
	defer cancel()

	done := make(chan struct{})
	go func() { e.updateCheckLoop(); close(done) }()

	<-checked
	select {
	case <-done:
		t.Fatal("loop exited after a failed check")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}
