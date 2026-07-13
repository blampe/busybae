package gc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contentsEntry lays out a `<hash>/<UUID>/…` pair with its marker file,
// mimicking what Bazel's LocalRepoContentsCache.moveToCache produces.
func contentsEntry(t *testing.T, root, hash, uuid, moduleBazel string, mtime time.Time) (contentsDir, markerPath string) {
	t.Helper()
	contentsDir = filepath.Join(root, hash, uuid)
	if err := os.MkdirAll(contentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A representative file whose mtime looks stale but is irrelevant
	// to the sweep — the sweep only cares about the marker's mtime.
	if err := os.WriteFile(filepath.Join(contentsDir, "MODULE.bazel"), []byte(moduleBazel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(contentsDir, "MODULE.bazel"), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	markerPath = filepath.Join(root, hash, uuid+recordedInputsExtension)
	writeFileAt(t, markerPath, "recorded", mtime)
	return contentsDir, markerPath
}

func TestSweepRepoContentsCache_EvictsOldMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("a", 64)
	uuid := "11111111-2222-3333-4444-555555555555"
	contentsDir, markerPath := contentsEntry(t, root, hash, uuid, "module(name=\"x\")", now.Add(-72*time.Hour))

	stats, err := SweepRepoContentsCache(context.Background(), root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Removed != 1 {
		t.Fatalf("stats = %+v, want Scanned=1 Removed=1", stats)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker should be gone: %v", err)
	}
	if _, err := os.Stat(contentsDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("contents dir should be gone: %v", err)
	}
	// _trash is Bazel's, but it should end up empty.
	trashEntries, err := os.ReadDir(filepath.Join(root, repoContentsTrashName))
	if err != nil {
		t.Fatalf("_trash should exist and be readable: %v", err)
	}
	if len(trashEntries) != 0 {
		t.Fatalf("_trash should be empty after sweep, got %v", trashEntries)
	}
}

func TestSweepRepoContentsCache_KeepsFreshMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("b", 64)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	contentsDir, markerPath := contentsEntry(t, root, hash, uuid, "module()", now.Add(-1*time.Hour))

	stats, err := SweepRepoContentsCache(context.Background(), root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("fresh marker must be kept, stats=%+v", stats)
	}
	if _, err := os.Stat(contentsDir); err != nil {
		t.Fatalf("contents dir should remain: %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker should remain: %v", err)
	}
	// MODULE.bazel inside the contents dir must NOT have been touched
	// even though its mtime is ancient. This is the bug the whole
	// exercise was about: the old walker would have evicted it.
	if _, err := os.Stat(filepath.Join(contentsDir, "MODULE.bazel")); err != nil {
		t.Fatalf("MODULE.bazel must be preserved regardless of its own mtime: %v", err)
	}
}

func TestSweepRepoContentsCache_PreservesReservedNames(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	// A live marker to make the root look like a real cache.
	hash := strings.Repeat("c", 64)
	uuid := "12345678-1234-1234-1234-123456789012"
	contentsEntry(t, root, hash, uuid, "module()", now.Add(-1*time.Hour))

	// Simulate `_trash/` containing an old evictable-looking dir.
	trash := filepath.Join(root, repoContentsTrashName)
	if err := os.MkdirAll(filepath.Join(trash, "leftover"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing gc_lock file.
	writeFileAt(t, filepath.Join(root, repoContentsLockName), "", now)

	_, err := SweepRepoContentsCache(context.Background(), root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, repoContentsLockName)); err != nil {
		t.Fatalf("gc_lock must be preserved: %v", err)
	}
	if _, err := os.Stat(trash); err != nil {
		t.Fatalf("_trash dir itself must be preserved: %v", err)
	}
	// The leftover in _trash should be cleaned as part of the sweep's
	// trailing "empty _trash" step.
	if _, err := os.Stat(filepath.Join(trash, "leftover")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("leftover trash entry should have been emptied: %v", err)
	}
}

func TestSweepRepoContentsCache_MarkerDeletedBeforeContentsRename(t *testing.T) {
	// The eviction order matters: the marker MUST be gone before the
	// contents dir is moved. We can't easily observe the intermediate
	// state from a black-box test, but we can assert the postcondition
	// — if a marker survives, its contents dir must also survive.
	// The property is: "no marker without its contents dir" (Bazel's
	// invariant). Any bug that renames first without removing the
	// marker would violate that. Here we assert the observable
	// consequence: after a partial run (mocked by cancelling the ctx
	// mid-eviction), the invariant still holds.
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("d", 64)
	for i := range 4 {
		uuid := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee" + string(rune('0'+i))
		contentsEntry(t, root, hash, uuid, "m", now.Add(-72*time.Hour))
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately to force early exit on the first ctx.Err.
	cancel()
	_, _ = SweepRepoContentsCache(ctx, root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})

	// Invariant check.
	hashDir := filepath.Join(root, hash)
	entries, err := os.ReadDir(hashDir)
	if err != nil {
		t.Fatal(err)
	}
	markers := map[string]bool{}
	dirs := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, recordedInputsExtension) {
			markers[strings.TrimSuffix(name, recordedInputsExtension)] = true
		} else if e.IsDir() {
			dirs[name] = true
		}
	}
	for uuid := range markers {
		if !dirs[uuid] {
			t.Fatalf("invariant violated: marker %q has no matching contents dir", uuid)
		}
	}
}

func TestSweepRepoContentsCache_ReportsContentsDirBytes(t *testing.T) {
	// Regression: the sweep previously accounted for the tiny marker
	// file's size instead of the paired contents dir, understating
	// reclaimed bytes by orders of magnitude.
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("a", 64)
	uuid := "11111111-2222-3333-4444-555555555555"
	contentsDir, _ := contentsEntry(t, root, hash, uuid, "module(name=\"x\")", now.Add(-72*time.Hour))
	// Add a bulky file inside the contents dir to make marker vs.
	// tree sizes obviously distinct.
	payload := make([]byte, 128*1024)
	if err := os.WriteFile(filepath.Join(contentsDir, "blob"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "real", true: "dry"}[dryRun], func(t *testing.T) {
			// Fresh copy per subtest since the real sweep deletes.
			testRoot := t.TempDir()
			testDir, _ := contentsEntry(t, testRoot, hash, uuid, "module()", now.Add(-72*time.Hour))
			if err := os.WriteFile(filepath.Join(testDir, "blob"), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			stats, err := SweepRepoContentsCache(context.Background(), testRoot, Options{
				MaxAge: 24 * time.Hour,
				DryRun: dryRun,
				Now:    func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Removed != 1 {
				t.Fatalf("stats=%+v, want Removed=1", stats)
			}
			if stats.Bytes < int64(len(payload)) {
				t.Fatalf("stats.Bytes=%d, want >= %d (payload size)", stats.Bytes, len(payload))
			}
		})
	}
}

func TestSweepRepoContentsCache_DryRun(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("f", 64)
	uuid := "cafecafe-cafe-cafe-cafe-cafecafecafe"
	contentsDir, markerPath := contentsEntry(t, root, hash, uuid, "m", now.Add(-72*time.Hour))
	stats, err := SweepRepoContentsCache(context.Background(), root, Options{
		MaxAge: time.Hour,
		DryRun: true,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 {
		t.Fatalf("dry-run should count, stats=%+v", stats)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("dry-run must not delete marker: %v", err)
	}
	if _, err := os.Stat(contentsDir); err != nil {
		t.Fatalf("dry-run must not delete contents: %v", err)
	}
}

func TestSweepRepoContentsCache_RejectsRandomRoot(t *testing.T) {
	// A directory with no plausible signals — no gc_lock, no _trash,
	// no hex-hash subdirs — must be skipped entirely.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "some-random-thing"), 0o700); err != nil {
		t.Fatal(err)
	}
	stats, err := SweepRepoContentsCache(context.Background(), root, Options{
		MaxAge: time.Nanosecond,
		Now:    func() time.Time { return time.Now().Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
}

func TestSweepRepoContentsCache_HonorsLock(t *testing.T) {
	// POSIX advisory locks are per-process, so we hold the lock in a
	// helper subprocess to simulate a concurrent Bazel invocation.
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	hash := strings.Repeat("a", 64)
	uuid := "11111111-2222-3333-4444-555555555555"
	contentsDir, markerPath := contentsEntry(t, root, hash, uuid, "m", now.Add(-72*time.Hour))

	withHeldLock(t, filepath.Join(root, repoContentsLockName), func() {
		stats, err := SweepRepoContentsCache(context.Background(), root, Options{
			MaxAge: time.Hour,
			Now:    func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("locked sweep must return cleanly: %v", err)
		}
		if stats.Removed != 0 {
			t.Fatalf("locked sweep must not evict, stats=%+v", stats)
		}
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("marker survives: %v", err)
		}
		if _, err := os.Stat(contentsDir); err != nil {
			t.Fatalf("contents survive: %v", err)
		}
	})
}
