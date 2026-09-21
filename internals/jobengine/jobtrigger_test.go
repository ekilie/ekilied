package jobengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func firstCallIndex(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

// Regression test for ekilie/ekilied#3: a fast job must never report completion
// before the backend has accepted its claim.
func TestHandleJobTriggerFullClaimsBeforeComplete(t *testing.T) {
	client := &fakeJobClient{}
	e := NewJobEngine(client)

	// An unknown action finishes instantly without side effects, which is the
	// worst case for the old background-claim race.
	e.HandleJobTriggerFull(context.Background(), 42, "unknown_test_action", map[string]any{})

	calls := client.calls()
	claimAt := firstCallIndex(calls, "claim")
	completeAt := firstCallIndex(calls, "complete")
	if claimAt == -1 || completeAt == -1 {
		t.Fatalf("calls = %v, want both claim and complete", calls)
	}
	if claimAt != 0 {
		t.Fatalf("calls = %v, want claim to be the first call", calls)
	}
	if claimAt > completeAt {
		t.Fatalf("complete before claim: %v", calls)
	}
	if e.IsDispatched(42) {
		t.Fatal("job 42 still marked dispatched after execution finished")
	}
}

// A 409 means another worker owns the job: no execution, no completion report,
// and the dispatch mark must be cleared so the poll loop can retry.
func TestHandleJobTriggerFullSkipsWhenAlreadyClaimed(t *testing.T) {
	client := &fakeJobClient{claimErr: fmt.Errorf("claim job 43: %w", ErrJobAlreadyClaimed)}
	e := NewJobEngine(client)

	e.HandleJobTriggerFull(context.Background(), 43, "unknown_test_action", map[string]any{})

	if calls := client.calls(); len(calls) != 1 || calls[0] != "claim" {
		t.Fatalf("calls = %v, want only the failed claim", calls)
	}
	if e.IsDispatched(43) {
		t.Fatal("job 43 still marked dispatched after claim conflict; poll could not retry")
	}
}

// A transient claim failure aborts execution; the poll loop redelivers.
func TestHandleJobTriggerFullSkipsOnClaimError(t *testing.T) {
	client := &fakeJobClient{claimErr: errors.New("network down")}
	e := NewJobEngine(client)

	e.HandleJobTriggerFull(context.Background(), 44, "unknown_test_action", map[string]any{})

	if calls := client.calls(); len(calls) != 1 || calls[0] != "claim" {
		t.Fatalf("calls = %v, want only the failed claim", calls)
	}
	if e.IsDispatched(44) {
		t.Fatal("job 44 still marked dispatched after claim error; poll could not retry")
	}
}

// The claim call must be bounded by claimTimeout even if the HTTP call hangs.
func TestHandleJobTriggerFullClaimTimeout(t *testing.T) {
	old := claimTimeout
	claimTimeout = 50 * time.Millisecond
	t.Cleanup(func() { claimTimeout = old })

	client := &fakeJobClient{claimBlock: true}
	e := NewJobEngine(client)

	start := time.Now()
	e.HandleJobTriggerFull(context.Background(), 45, "unknown_test_action", map[string]any{})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("claim was not bounded by claimTimeout: took %v", elapsed)
	}

	if calls := client.calls(); len(calls) != 1 || calls[0] != "claim" {
		t.Fatalf("calls = %v, want only the timed-out claim", calls)
	}
	if e.IsDispatched(45) {
		t.Fatal("job 45 still marked dispatched after claim timeout; poll could not retry")
	}
}

// The lightweight trigger path keeps claim-first semantics and clears its
// dispatch mark on claim failure so the poll loop can retry.
func TestHandleJobTriggerAbortsOnClaimFailure(t *testing.T) {
	client := &fakeJobClient{claimErr: errors.New("boom")}
	e := NewJobEngine(client)

	e.HandleJobTrigger(context.Background(), 46)

	if calls := client.calls(); len(calls) != 1 || calls[0] != "claim" {
		t.Fatalf("calls = %v, want only the failed claim", calls)
	}
	if e.IsDispatched(46) {
		t.Fatal("job 46 still marked dispatched after claim failure; poll could not retry")
	}
}
