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

// downloadKey builds a `<hashName>/<cacheKey>/file` (and its parent dirs)
// with the given contents and mtime. It's the minimum valid Bazel
// download-cache entry.
func downloadKey(t *testing.T, root, hashName, key, contents string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, hashName, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "file")
	if err := os.WriteFile(blob, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(blob, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepDownloadCache_EvictsOldEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	old := downloadKey(t, root, "sha256", strings.Repeat("a", 64), "old", now.Add(-72*time.Hour))
	fresh := downloadKey(t, root, "sha256", strings.Repeat("b", 64), "fresh", now.Add(-1*time.Hour))

	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 2 || stats.Removed != 1 {
		t.Fatalf("stats = %+v, want Scanned=2 Removed=1", stats)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old entry should be gone: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh entry should remain: %v", err)
	}
}

func TestSweepDownloadCache_SkipsInFlightWrite(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	dir := downloadKey(t, root, "sha256", strings.Repeat("c", 64), "old", now.Add(-72*time.Hour))
	// A tmp-* sibling means Bazel is mid-write; we must not disturb it.
	if err := os.WriteFile(filepath.Join(dir, "tmp-inflight"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge: time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 || stats.Scanned != 1 {
		t.Fatalf("stats = %+v, want Scanned=1 Removed=0", stats)
	}
	if _, err := os.Stat(filepath.Join(dir, "file")); err != nil {
		t.Fatalf("in-flight entry should be preserved: %v", err)
	}
}

func TestSweepDownloadCache_SkipsHardlinkedByDefault(t *testing.T) {
	root := t.TempDir()
	sibling := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	dir := downloadKey(t, root, "sha256", strings.Repeat("d", 64), "shared", now.Add(-72*time.Hour))
	// Simulate a live workspace holding the cached blob via hardlink.
	if err := os.Link(filepath.Join(dir, "file"), filepath.Join(sibling, "held")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "file"), now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge: time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 || stats.SkippedHardlinks != 1 {
		t.Fatalf("stats = %+v, want SkippedHardlinks=1", stats)
	}
	if _, err := os.Stat(filepath.Join(dir, "file")); err != nil {
		t.Fatalf("hardlinked entry should remain: %v", err)
	}
}

func TestSweepDownloadCache_IncludeHardlinked(t *testing.T) {
	root := t.TempDir()
	sibling := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	dir := downloadKey(t, root, "sha256", strings.Repeat("e", 64), "shared", now.Add(-72*time.Hour))
	if err := os.Link(filepath.Join(dir, "file"), filepath.Join(sibling, "held")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "file"), now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge:            time.Hour,
		IncludeHardlinked: true,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.SkippedHardlinks != 0 {
		t.Fatalf("stats = %+v, want Removed=1 SkippedHardlinks=0", stats)
	}
	// The sibling link still points at a live inode.
	if _, err := os.Stat(filepath.Join(sibling, "held")); err != nil {
		t.Fatalf("sibling link should survive: %v", err)
	}
}

func TestSweepDownloadCache_RejectsNonDownloadCache(t *testing.T) {
	// A directory with no known hash-name subdir must be treated as
	// "not a download cache" — we must not evict anything.
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "random", "file"), "x", time.Unix(0, 0))
	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge: time.Nanosecond,
		Now:    func() time.Time { return time.Now().Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
	if _, err := os.Stat(filepath.Join(root, "random", "file")); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
}

func TestSweepDownloadCache_DryRun(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	dir := downloadKey(t, root, "sha256", strings.Repeat("f", 64), "old", now.Add(-72*time.Hour))
	stats, err := SweepDownloadCache(context.Background(), root, Options{
		MaxAge: time.Hour,
		DryRun: true,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 {
		t.Fatalf("dry-run should still count, stats=%+v", stats)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry-run must not delete: %v", err)
	}
	// No trash dir should be created in dry-run mode.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), trashPrefix) {
			t.Fatalf("dry-run should not create trash: %s", e.Name())
		}
	}
}

func TestSweepDownloadCache_MissingRoot(t *testing.T) {
	stats, err := SweepDownloadCache(context.Background(), "/nonexistent-busybae-cache", Options{
		MaxAge: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
}
