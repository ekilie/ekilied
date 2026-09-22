package jobengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ekilie/ekilied/internals/dtos"
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

// Regression test for ekilie/ekilied#6: a panic inside job execution must
// fail that job instead of crashing the daemon, and must leave the engine
// usable for the next job. The panic is injected through the first log flush,
// which runs synchronously inside Execute.
func TestExecuteRecoversFromPanic(t *testing.T) {
	client := &fakeJobClient{panicStreamOnce: true}
	e := NewJobEngine(client)

	// Unknown actions finish quickly; the first StreamLogs call panics.
	e.Execute(context.Background(), 77, "unknown_test_action", nil)

	var panicked *completedCall
	for _, c := range client.allCompletions() {
		if c.jobID == 77 {
			cc := c
			panicked = &cc
		}
	}
	if panicked == nil {
		t.Fatal("panicking job was never completed")
	}
	if panicked.status != "failed" {
		t.Fatalf("status = %q, want failed", panicked.status)
	}
	if !strings.Contains(panicked.errMsg, "panic: boom in stream logs") {
		t.Fatalf("error = %q, want the panic text", panicked.errMsg)
	}
	if e.IsDispatched(77) {
		t.Fatal("job 77 still marked dispatched after panic; poll could not retry")
	}

	// The engine must keep working: the next job runs to completion.
	e.Execute(context.Background(), 78, "unknown_test_action", nil)
	found := false
	for _, c := range client.allCompletions() {
		if c.jobID == 78 {
			found = true
		}
	}
	if !found {
		t.Fatal("engine did not process a job after a recovered panic")
	}
}

// marshalBomb panics if anything tries to JSON-marshal it. It proves job
// params travel from claim/WS payload to Execute without a marshal round
// trip: any such trip would trip the bomb.
type marshalBomb struct{}

func (marshalBomb) MarshalJSON() ([]byte, error) {
	panic("job params were marshaled")
}

// Regression test for ekilie/ekilied#16 (job_full path).
func TestHandleJobTriggerFullDoesNotMarshalParams(t *testing.T) {
	client := &fakeJobClient{}
	e := NewJobEngine(client)

	params := map[string]any{"site_name": "demo", "bomb": marshalBomb{}}
	e.HandleJobTriggerFull(context.Background(), 90, "unknown_test_action", params)

	comps := client.allCompletions()
	if len(comps) != 1 || comps[0].jobID != 90 {
		t.Fatalf("completions = %+v, want one for job 90", comps)
	}
	if comps[0].status != "failed" {
		t.Fatalf("status = %q, want failed (unknown action)", comps[0].status)
	}
	if strings.Contains(comps[0].errMsg, "marshaled") {
		t.Fatalf("params were marshaled on dispatch: %q", comps[0].errMsg)
	}
}

// Regression test for ekilie/ekilied#16 (claim path).
func TestHandleJobTriggerDoesNotMarshalParams(t *testing.T) {
	client := &fakeJobClient{claimJob: &dtos.JobItem{
		ID:     91,
		Action: "unknown_test_action",
		Params: map[string]any{"site_name": "demo", "bomb": marshalBomb{}},
	}}
	e := NewJobEngine(client)

	e.HandleJobTrigger(context.Background(), 91)

	comps := client.allCompletions()
	if len(comps) != 1 || comps[0].jobID != 91 {
		t.Fatalf("completions = %+v, want one for job 91", comps)
	}
	if strings.Contains(comps[0].errMsg, "marshaled") {
		t.Fatalf("params were marshaled on dispatch: %q", comps[0].errMsg)
	}
}
