package gc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFileAt creates path with the given mtime.
func writeFileAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRemovesOldFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	writeFileAt(t, filepath.Join(root, "sub", "old"), "old", now.Add(-48*time.Hour))
	writeFileAt(t, filepath.Join(root, "sub", "new"), "new", now.Add(-1*time.Hour))

	stats, err := Sweep(context.Background(), root, Options{
		MaxAge: 24 * time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 2 || stats.Removed != 1 {
		t.Fatalf("stats = %+v, want scanned=2 removed=1", stats)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "old")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("old file should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "new")); err != nil {
		t.Fatalf("new file should remain: %v", err)
	}
	// Trash should be cleaned up.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("trash dir left behind: %s", e.Name())
		}
	}
}

func TestSweepDryRun(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	writeFileAt(t, filepath.Join(root, "x"), "hi", now.Add(-72*time.Hour))

	stats, err := Sweep(context.Background(), root, Options{
		MaxAge: time.Hour,
		DryRun: true,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 || stats.Bytes != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err != nil {
		t.Fatal("dry-run must not delete anything")
	}
}

func TestSweepMissingRoot(t *testing.T) {
	stats, err := Sweep(context.Background(), "/nonexistent-busybae-target", Options{
		MaxAge: time.Hour,
		Now:    time.Now,
	})
	if err != nil {
		t.Fatalf("missing root should be a no-op, got %v", err)
	}
	if stats != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
}

func TestSweepZeroMaxAge(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "x"), "hi", time.Unix(0, 0))
	stats, err := Sweep(context.Background(), root, Options{MaxAge: 0})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", stats)
	}
	// File should still exist.
	if _, err := os.Stat(filepath.Join(root, "x")); err != nil {
		t.Fatalf("file removed with MaxAge=0: %v", err)
	}
}

func TestSweepCleansPriorTrash(t *testing.T) {
	root := t.TempDir()
	// Simulate a leftover trash dir from a killed prior run.
	old := filepath.Join(root, trashPrefix+"leftover")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Sweep with no eligible files. It should still clean the old trash.
	_, err := Sweep(context.Background(), root, Options{
		MaxAge: time.Hour,
		Now:    time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("leftover trash should be gone, stat=%v", err)
	}
}

func TestSweepSkipsHardlinkedByDefault(t *testing.T) {
	root := t.TempDir()
	sibling := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	target := filepath.Join(root, "cache", "entry")
	writeFileAt(t, target, "shared", now.Add(-72*time.Hour))
	// Create a hardlink from an "external repo" tree.
	if err := os.Link(target, filepath.Join(sibling, "external-link")); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Also chtimes the target to reset mtime AFTER the link is set up
	// (some filesystems bump mtime on link creation).
	if err := os.Chtimes(target, now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	stats, err := Sweep(context.Background(), root, Options{
		MaxAge: time.Hour,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 0 || stats.SkippedHardlinks != 1 {
		t.Fatalf("stats = %+v, want Removed=0 SkippedHardlinks=1", stats)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("hardlinked target should remain: %v", err)
	}
}

func TestSweepIncludeHardlinked(t *testing.T) {
	root := t.TempDir()
	sibling := t.TempDir()
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	target := filepath.Join(root, "cache", "entry")
	writeFileAt(t, target, "shared", now.Add(-72*time.Hour))
	if err := os.Link(target, filepath.Join(sibling, "external-link")); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := os.Chtimes(target, now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	stats, err := Sweep(context.Background(), root, Options{
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
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache entry should be removed, stat=%v", err)
	}
	// The sibling hardlink still points at a live inode.
	if _, err := os.Stat(filepath.Join(sibling, "external-link")); err != nil {
		t.Fatalf("sibling link should survive: %v", err)
	}
}

func TestSweepSkipsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	// Just a directory — WalkDir will visit it, but it isn't a regular
	// file, so it must not be counted.
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(context.Background(), root, Options{
		MaxAge: time.Nanosecond,
		Now:    func() time.Time { return time.Now().Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 0 {
		t.Fatalf("expected no files scanned, got %+v", stats)
	}
}
