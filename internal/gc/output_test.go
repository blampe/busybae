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

// outputBaseHash is a placeholder 32-hex-char name; the real hash content
// doesn't matter to us, only its shape.
const outputBaseHash = "0123456789abcdef0123456789abcdef"

func mkdirAt(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSweepOutputBasesEvictsStaleAndKeepsFresh(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	stale := filepath.Join(root, outputBaseHash)
	fresh := filepath.Join(root, "fedcba9876543210fedcba9876543210")

	mkdirAt(t, stale, now.Add(-40*24*time.Hour))
	writeFileAt(t, filepath.Join(stale, "command.log"), "x", now.Add(-40*24*time.Hour))
	mkdirAt(t, fresh, now.Add(-1*24*time.Hour))
	writeFileAt(t, filepath.Join(fresh, "command.log"), "x", now.Add(-1*24*time.Hour))

	// Shared install/ and cache/ must survive regardless of mtime.
	install := filepath.Join(root, "install")
	repoCache := filepath.Join(root, "cache")
	mkdirAt(t, install, now.Add(-90*24*time.Hour))
	mkdirAt(t, repoCache, now.Add(-90*24*time.Hour))

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.Scanned != 2 {
		t.Fatalf("stats = %+v, want Scanned=2 Removed=1", stats)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale should be gone, stat=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh should remain: %v", err)
	}
	if _, err := os.Stat(install); err != nil {
		t.Fatalf("install/ must never be touched: %v", err)
	}
	if _, err := os.Stat(repoCache); err != nil {
		t.Fatalf("cache/ must never be touched: %v", err)
	}
}

func TestSweepOutputBasesPrefersCommandLogMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	// Directory mtime is ancient but command.log was updated yesterday
	// — the output_base is active and must be kept.
	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-90*24*time.Hour))
	writeFileAt(t, filepath.Join(base, "command.log"), "recent", now.Add(-1*time.Hour))

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("must not remove active output_base, stats=%+v", stats)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("active output_base gone: %v", err)
	}
}

func TestSweepOutputBasesFallsBackToDirMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	// No command.log — must use dir mtime.
	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-40*24*time.Hour))

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 {
		t.Fatalf("expected removal via dir mtime fallback, stats=%+v", stats)
	}
}

func TestSweepOutputBasesIgnoresNonHashDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	// Wrong shape: not 32 hex chars — must be skipped even though it's
	// ancient.
	weird := filepath.Join(root, "not-a-hash")
	mkdirAt(t, weird, now.Add(-90*24*time.Hour))
	// A clearly-fresh hash-shaped dir so the safety check passes and we
	// don't accidentally sweep it either.
	mkdirAt(t, filepath.Join(root, outputBaseHash), now)

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("non-hash entry must not be swept, stats=%+v", stats)
	}
	if _, err := os.Stat(weird); err != nil {
		t.Fatalf("weird entry removed: %v", err)
	}
}

func TestSweepOutputBasesSafetyRefusesRandomRoot(t *testing.T) {
	// A directory with no plausible output_base entries — SweepOutputBases
	// must decline to touch anything.
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "some-file"), "x", time.Unix(0, 0))
	mkdirAt(t, filepath.Join(root, "random-dir"), time.Unix(0, 0))

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: time.Nanosecond,
		Now:    func() time.Time { return time.Now().Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 || stats.Scanned != 0 {
		t.Fatalf("expected zero stats on non-output_user_root, got %+v", stats)
	}
}

func TestSweepOutputBasesDryRun(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-90*24*time.Hour))
	writeFileAt(t, filepath.Join(base, "command.log"), "x", now.Add(-90*24*time.Hour))

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		DryRun: true,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 {
		t.Fatalf("dry-run should still count, stats=%+v", stats)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("dry-run must not delete: %v", err)
	}
	// And there must be no trash directory hanging around.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), trashPrefix) {
			t.Fatalf("dry-run should not create trash: %s", e.Name())
		}
	}
}

func TestSweepOutputBasesOrphanedByDoNotBuildHere(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	base := filepath.Join(root, outputBaseHash)
	// Pretend the workspace lived here and was deleted.
	deletedWorkspace := filepath.Join(t.TempDir(), "worktree-gone")
	// (t.TempDir returns an existing dir; use a subpath that we never
	// create, so os.Stat returns ErrNotExist.)
	mkdirAt(t, base, now.Add(-1*time.Hour))
	// Fresh command.log — mtime-based logic would keep this. The
	// orphan signal must override.
	writeFileAt(t, filepath.Join(base, "command.log"), "x", now.Add(-1*time.Minute))
	if err := os.WriteFile(filepath.Join(base, "DO_NOT_BUILD_HERE"), []byte(deletedWorkspace+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.OrphanedWorkspaces != 1 {
		t.Fatalf("stats = %+v, want Removed=1 OrphanedWorkspaces=1", stats)
	}
	if _, err := os.Stat(base); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphaned output_base should be gone: %v", err)
	}
}

func TestSweepOutputBasesLiveWorkspaceKept(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	liveWorkspace := t.TempDir() // exists

	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-1*time.Hour))
	writeFileAt(t, filepath.Join(base, "command.log"), "x", now.Add(-1*time.Minute))
	if err := os.WriteFile(filepath.Join(base, "DO_NOT_BUILD_HERE"), []byte(liveWorkspace), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 {
		t.Fatalf("live workspace must not be swept, stats=%+v", stats)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("output_base for live workspace removed: %v", err)
	}
}

func TestSweepOutputBasesLiveWorkspaceButIdle(t *testing.T) {
	// Workspace exists on disk, but no activity in ages — should still
	// be swept by the mtime path (labeled "idle", not "orphaned").
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	liveWorkspace := t.TempDir()

	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-90*24*time.Hour))
	writeFileAt(t, filepath.Join(base, "command.log"), "x", now.Add(-90*24*time.Hour))
	if err := os.WriteFile(filepath.Join(base, "DO_NOT_BUILD_HERE"), []byte(liveWorkspace), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.OrphanedWorkspaces != 0 {
		t.Fatalf("stats = %+v, want Removed=1 OrphanedWorkspaces=0 (idle, not orphaned)", stats)
	}
}

func TestSweepOutputBasesUnreadableMarkerFallsBackToMtime(t *testing.T) {
	// DO_NOT_BUILD_HERE exists but is empty / not an absolute path.
	// We must treat that as "workspace unknown" and use mtime.
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	base := filepath.Join(root, outputBaseHash)
	mkdirAt(t, base, now.Add(-90*24*time.Hour))
	writeFileAt(t, filepath.Join(base, "command.log"), "x", now.Add(-90*24*time.Hour))
	if err := os.WriteFile(filepath.Join(base, "DO_NOT_BUILD_HERE"), []byte("not-absolute\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err := SweepOutputBases(context.Background(), root, Options{
		MaxAge: 30 * 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.OrphanedWorkspaces != 0 {
		t.Fatalf("stats = %+v, want mtime fallback (Removed=1 Orphaned=0)", stats)
	}
}

func TestIsPlausibleOutputBase(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{outputBaseHash, true},
		{"0123456789ABCDEF0123456789ABCDEF", true}, // upper-hex still valid
		{"install", false},
		{"cache", false},
		{"", false},
		{"01234567", false},                          // too short
		{"0123456789abcdef0123456789abcdefX", false}, // too long
		{"0123456789abcdef0123456789abcdeg", false},  // bad hex
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isPlausibleOutputBase(tc.in); got != tc.want {
				t.Fatalf("isPlausibleOutputBase(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
