// Package gc implements a time-based sweep of a Bazel cache directory.
//
// The sweep is designed to be safe when Bazel is running against the same
// cache:
//
//   - We only touch entries whose mtime is older than MaxAge. Bazel updates
//     mtime on write (and Bazel builds with a live cache touch its recently
//     used files on read, if cache_test_results / --experimental_repository_cache_hardlinks
//     is off — mtime for our purposes is a conservative proxy for "not used
//     recently").
//   - We move eligible files into a trash subdirectory rooted inside the
//     cache dir itself. Both source and destination are on the same
//     filesystem, so the rename is atomic. Any Bazel process holding an
//     open fd against the old path continues to read successfully; any
//     future open sees ENOENT and Bazel's own logic will refetch/recompute.
//   - Only after the walk completes do we blow away the trash directory. If
//     the process dies mid-sweep, the next start collects the leftover
//     trash.
package gc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const trashPrefix = ".busybae-trash-"

// Options configures a sweep.
type Options struct {
	// MaxAge is the mtime cutoff. Files older than now-MaxAge are
	// eligible for removal. Zero means "never eligible" (no-op sweep).
	MaxAge time.Duration
	// DryRun logs what would be removed without renaming/deleting.
	DryRun bool
	// IncludeHardlinked, when true, evicts cache entries even when they
	// have additional hardlinks pointing at them.
	//
	// Bazel's repository cache uses hardlinks (default behavior in
	// modern Bazel; see --experimental_repository_cache_hardlinks): a
	// cache entry with nlink > 1 is being held by a live workspace's
	// external tree, and removing our copy does not reclaim disk space
	// — it just makes the next fetch re-materialize the link. The safe
	// default (false) is therefore to skip hardlinked entries.
	//
	// Set this to true when you know the linked workspaces are stale
	// (e.g. output_bases you no longer use) and you want to force
	// eviction anyway.
	IncludeHardlinked bool
	// Logger receives structured events. Nil means slog.Default().
	Logger *slog.Logger
	// Now is a clock hook for tests.
	Now func() time.Time
}

// Stats summarizes a sweep.
type Stats struct {
	Scanned            int
	Removed            int
	Bytes              int64
	Errors             int
	SkippedHardlinks   int
	OrphanedWorkspaces int // output_base sweeps: number of removals attributed to a missing workspace path
}

// Sweep walks root and removes files with mtime older than the cutoff. It
// is safe to call against a live Bazel cache directory.
//
// If root does not exist, Sweep returns nil with zero stats.
func Sweep(ctx context.Context, root string, opts Options) (Stats, error) {
	var stats Stats
	if opts.MaxAge <= 0 {
		return stats, nil
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return stats, nil
		}
		return stats, err
	}
	if !info.IsDir() {
		return stats, fmt.Errorf("sweep target %q is not a directory", root)
	}

	// First, clean up any leftover trash from previous runs. We do this
	// unconditionally so a crashed prior sweep doesn't leak space.
	cleanupOldTrash(root, log)

	// Create a fresh trash dir inside root. Same filesystem → atomic
	// renames.
	var trash string
	if !opts.DryRun {
		t, terr := os.MkdirTemp(root, trashPrefix)
		if terr != nil {
			return stats, fmt.Errorf("create trash dir: %w", terr)
		}
		trash = t
		defer func() {
			if rmErr := os.RemoveAll(trash); rmErr != nil {
				log.Warn("removing trash failed", slog.String("path", trash), slog.Any("err", rmErr))
			}
		}()
	}

	cutoff := now().Add(-opts.MaxAge)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			// A transient stat error (Bazel writing? disk hiccup?)
			// shouldn't kill the whole sweep. Skip and record.
			stats.Errors++
			log.Debug("walk error", slog.String("path", path), slog.Any("err", err))
			return nil
		}
		// Skip our own trash tree.
		if d.IsDir() && strings.HasPrefix(d.Name(), trashPrefix) {
			return fs.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		stats.Scanned++
		info, ierr := d.Info()
		if ierr != nil {
			stats.Errors++
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		if !opts.IncludeHardlinked && hasExtraHardlinks(info) {
			stats.SkippedHardlinks++
			return nil
		}
		if opts.DryRun {
			stats.Removed++
			stats.Bytes += info.Size()
			log.Info("would remove",
				slog.String("path", path),
				slog.Time("mtime", info.ModTime()),
				slog.Int64("bytes", info.Size()))
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			stats.Errors++
			return nil
		}
		dst := filepath.Join(trash, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			stats.Errors++
			log.Debug("mkdir trash subpath failed", slog.String("path", path), slog.Any("err", err))
			return nil
		}
		if err := os.Rename(path, dst); err != nil {
			// If Bazel raced us and the file vanished, that's fine.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			stats.Errors++
			log.Debug("rename to trash failed", slog.String("path", path), slog.Any("err", err))
			return nil
		}
		stats.Removed++
		stats.Bytes += info.Size()
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return stats, walkErr
	}
	log.Info("sweep complete",
		slog.String("root", root),
		slog.Int("scanned", stats.Scanned),
		slog.Int("removed", stats.Removed),
		slog.Int64("bytes", stats.Bytes),
		slog.Int("errors", stats.Errors),
		slog.Int("skipped_hardlinks", stats.SkippedHardlinks),
		slog.Bool("dry_run", opts.DryRun))
	return stats, walkErr
}

// cleanupOldTrash removes any residual trash directories from prior runs.
// Failures here are logged and swallowed; they don't affect the current
// sweep's correctness.
func cleanupOldTrash(root string, log *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), trashPrefix) {
			continue
		}
		p := filepath.Join(root, e.Name())
		if err := os.RemoveAll(p); err != nil {
			log.Warn("cleanup old trash failed", slog.String("path", p), slog.Any("err", err))
		}
	}
}
