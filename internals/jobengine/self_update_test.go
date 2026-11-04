package jobengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func writeBinary(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Regression test for the cross-device update failure: the extracted binary
// used to be renamed from /tmp straight into /usr/local/bin, which fails with
// EXDEV when /tmp is tmpfs.
func TestReplaceBinaryAcrossFilesystems(t *testing.T) {
	if info, err := os.Stat("/dev/shm"); err != nil || !info.IsDir() {
		t.Skip("/dev/shm is not available")
	}

	srcDir, err := os.MkdirTemp("/dev/shm", "ekilied-update-test-*")
	if err != nil {
		t.Skipf("cannot use /dev/shm: %v", err)
	}
	defer os.RemoveAll(srcDir)

	targetDir := t.TempDir()
	selfPath := filepath.Join(targetDir, "ekilied")
	newPath := filepath.Join(srcDir, "ekilied")

	writeBinary(t, selfPath, "old binary")
	writeBinary(t, newPath, "new binary")

	if err := replaceBinary(selfPath, newPath); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(selfPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("installed binary = %q, want %q", got, "new binary")
	}

	backup, err := os.ReadFile(selfPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old binary" {
		t.Fatalf("backup = %q, want %q", backup, "old binary")
	}

	leftovers, err := filepath.Glob(filepath.Join(targetDir, ".ekilied-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("staged files left behind: %v", leftovers)
	}

	info, err := os.Stat(selfPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Fatalf("installed mode = %o, want 755", got)
	}
}

func TestReplaceBinarySameFilesystem(t *testing.T) {
	targetDir := t.TempDir()
	selfPath := filepath.Join(targetDir, "ekilied")
	newPath := filepath.Join(t.TempDir(), "ekilied")

	writeBinary(t, selfPath, "old binary")
	writeBinary(t, newPath, "new binary")

	if err := replaceBinary(selfPath, newPath); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(selfPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("installed binary = %q, want %q", got, "new binary")
	}
}

func TestReplaceBinaryRestoresOnRenameFailure(t *testing.T) {
	targetDir := t.TempDir()
	selfPath := filepath.Join(targetDir, "ekilied")
	writeBinary(t, selfPath, "old binary")

	// A missing source makes staging fail before the current binary is moved,
	// so the old binary must stay in place.
	err := replaceBinary(selfPath, filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected an error for a missing source binary")
	}
	if !strings.Contains(err.Error(), "open new binary") {
		t.Fatalf("unexpected error: %v", err)
	}
	got, readErr := os.ReadFile(selfPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old binary" {
		t.Fatalf("self binary changed to %q", got)
	}
}
