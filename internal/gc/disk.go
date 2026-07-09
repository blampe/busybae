package gc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SweepDiskCache evicts stale entries from a Bazel `--disk_cache` directory.
//
// This mirrors DiskCacheGarbageCollector.java in the Bazel tree, in
// particular:
//
//   - Take an exclusive POSIX advisory lock on `<root>/gc/lock` so a busybae
//     sweep and Bazel's own idle-time GC never run in parallel.
//   - Exclude the `tmp/` and `gc/` subtrees from the scan (Bazel's atomic
//     write staging area and its lock directory, respectively).
//   - Sort candidates by (mtime ASC, path ASC) so `ac/*` entries — which
//     reference `cas/*` blobs — are deleted before the blobs themselves.
//     If the entire sweep completes we don't strictly need the order, but
//     any interruption (context cancellation, IO error, filesystem
//     glitch) leaves a partially-deleted cache in a state Bazel will treat
//     as a miss rather than a corruption.
//   - Re-stat each candidate immediately before deleting and skip anything
//     whose mtime or size changed under us. Bazel's GC does the same:
//     a fresh write in the interval means an active build is depending on
//     the entry.
//
// SweepDiskCache does not use a trash dir — Bazel's own GC deletes
// individual files in place and so do we, because the cache is
// content-addressed and losing a file mid-delete is just a cache miss.
func SweepDiskCache(ctx context.Context, root string, opts Options) (Stats, error) {
	var stats Stats
	if opts.MaxAge <= 0 {
		return stats, nil
	}
	log, now := resolveOptions(opts)
	log = log.With(slog.String("kind", "disk_cache"))

	ok, err := prepareRoot(root)
	if err != nil || !ok {
		return stats, err
	}

	// Take the same lock Bazel takes for its own disk-cache GC.
	lockDir := filepath.Join(root, "gc")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return stats, fmt.Errorf("prepare gc lock dir: %w", err)
	}
	unlock, err := tryLockExclusive(filepath.Join(lockDir, "lock"))
	if err != nil {
		if errors.Is(err, errLockHeld) {
			log.Info("disk cache gc lock held by another process; skipping")
			return stats, nil
		}
		return stats, fmt.Errorf("acquire disk cache gc lock: %w", err)
	}
	defer unlock()

	cutoff := now().Add(-opts.MaxAge)

	// Scan phase: collect (path, size, mtime).
	type entry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var candidates []entry
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			stats.Errors++
			return nil
		}
		if d.IsDir() {
			// Skip Bazel's own reserved dirs and any leftover
			// busybae trash from a prior run.
			name := d.Name()
			if path != root && (name == "tmp" || name == "gc" || strings.HasPrefix(name, trashPrefix)) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			stats.Errors++
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		candidates = append(candidates, entry{path: rel, size: info.Size(), mtime: info.ModTime()})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return stats, walkErr
	}
	stats.Scanned = len(candidates)

	// Sort (mtime ASC, path ASC). Path ordering guarantees "ac/…" beats
	// "cas/…" at equal mtimes, so we drop AC entries before the blobs
	// they reference.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].mtime.Equal(candidates[j].mtime) {
			return candidates[i].mtime.Before(candidates[j].mtime)
		}
		return candidates[i].path < candidates[j].path
	})

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		abs := filepath.Join(root, c.path)
		if opts.DryRun {
			stats.Removed++
			stats.Bytes += c.size
			log.Info("would remove disk entry",
				slog.String("path", abs),
				slog.Int64("bytes", c.size))
			continue
		}
		// Re-stat guard: if a concurrent build touched or replaced
		// the entry between the scan and now, skip it.
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.ConcurrentUpdate = true
				continue
			}
			stats.Errors++
			continue
		}
		if !info.ModTime().Equal(c.mtime) || info.Size() != c.size {
			stats.ConcurrentUpdate = true
			continue
		}
		if err := os.Remove(abs); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.ConcurrentUpdate = true
				continue
			}
			stats.Errors++
			log.Debug("remove failed", slog.String("path", abs), slog.Any("err", err))
			continue
		}
		stats.Removed++
		stats.Bytes += c.size
	}

	log.Info("disk-cache sweep complete",
		slog.String("root", root),
		slog.Int("scanned", stats.Scanned),
		slog.Int("removed", stats.Removed),
		slog.Int64("bytes", stats.Bytes),
		slog.Int("errors", stats.Errors),
		slog.Bool("concurrent_update", stats.ConcurrentUpdate),
		slog.Bool("dry_run", opts.DryRun))
	return stats, nil
}
