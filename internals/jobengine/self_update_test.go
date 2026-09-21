package jobengine

import (
	"errors"
	"sync"
	"testing"
)

// Regression tests for ekilie/ekilied#46: the 24h auto-update ticker and a
// control-plane self_update job could run SelfUpdate concurrently and
// interleave the binary swap.

func TestSelfUpdateRejectsConcurrentRun(t *testing.T) {
	if !updateInProgress.CompareAndSwap(false, true) {
		t.Fatal("guard unexpectedly held")
	}
	defer updateInProgress.Store(false)

	err := SelfUpdate("ekilie/ekilied", &GitHubRelease{TagName: "v9.9.9"})
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("SelfUpdate = %v, want ErrUpdateInProgress", err)
	}
}

func TestSelfUpdateConcurrentCallersAllSkipped(t *testing.T) {
	if !updateInProgress.CompareAndSwap(false, true) {
		t.Fatal("guard unexpectedly held")
	}
	defer updateInProgress.Store(false)

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = SelfUpdate("ekilie/ekilied", &GitHubRelease{TagName: "v9.9.9"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, ErrUpdateInProgress) {
			t.Errorf("caller %d: got %v, want ErrUpdateInProgress", i, err)
		}
	}
}

// A failed update must release the guard so the next ticker/job attempt can run.
func TestSelfUpdateReleasesGuardOnFailure(t *testing.T) {
	// A release with no matching assets fails before any network access,
	// exercising the deferred guard release.
	err := SelfUpdate("ekilie/ekilied", &GitHubRelease{TagName: "v9.9.9"})
	if err == nil || errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("SelfUpdate = %v, want no-binary error", err)
	}
	if updateInProgress.Load() {
		t.Fatal("guard still held after failed update")
	}
}

// Sequential attempts (the normal retry pattern) must not be blocked by a
// previous failure.
func TestSelfUpdateSequentialAfterFailure(t *testing.T) {
	for i := 0; i < 3; i++ {
		err := SelfUpdate("ekilie/ekilied", &GitHubRelease{TagName: "v9.9.9"})
		if err == nil || errors.Is(err, ErrUpdateInProgress) {
			t.Fatalf("call %d: SelfUpdate = %v, want no-binary error", i, err)
		}
	}
	if updateInProgress.Load() {
		t.Fatal("guard still held after sequential attempts")
	}
}
