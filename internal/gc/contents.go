package gc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bazel's LocalRepoContentsCache reserves these paths at the root of the
// contents cache. Both must be preserved by any GC pass.
const (
	repoContentsLockName    = "gc_lock"
	repoContentsTrashName   = "_trash"
	recordedInputsExtension = ".recorded_inputs"
)

// SweepRepoContentsCache evicts stale entries from a Bazel
// `--repo_contents_cache` directory (also the `<root>/contents/` subtree of
// a `--repository_cache`). It mirrors LocalRepoContentsCache.runGc in the
// Bazel tree.
//
// Layout expected at root:
//
//	<root>/
//	  gc_lock                                # exclusive during GC, shared during reads
//	  _trash/                                # reserved trash; name starts with '_'
//	  <predeclaredInputHash>/                # one dir per repo-rule inputs hash
//	     <UUID>/                             # extracted repo contents — atomic unit
//	     <UUID>.recorded_inputs              # sibling marker; mtime touched on cache hit
//	     …
//
// The `<UUID>/` contents dir is symlinked into every live workspace's
// external tree (see LocalRepoContentsCache.moveToCache). Deleting a file
// inside it — the way our old, generic walker did — would surface as
// "MODULE.bazel expected but not found at output_base/external/<repo>/…"
// in any concurrent Bazel invocation. Instead, this sweep operates only at
// the marker-file level:
//
//  1. Take the exclusive `gc_lock` (interoping with Bazel via fcntl).
//  2. For each `<hash>/*.recorded_inputs` older than the cutoff, delete
//     the marker file first, then rename the paired `<UUID>/` dir into
//     `_trash/<random>`.
//  3. Release the lock and empty `_trash/`. Because the marker was
//     deleted before the contents dir was moved, no Bazel process can
//     observe a live marker whose contents dir is gone; the worst case is
//     "cache miss, re-fetch".
//
// The sweep never descends into a `<UUID>/` contents dir.
func SweepRepoContentsCache(ctx context.Context, root string, opts Options) (Stats, error) {
	var stats Stats
	if opts.MaxAge <= 0 {
		return stats, nil
	}
	log, now := resolveOptions(opts)
	log = log.With(slog.String("kind", "repo_contents_cache"))

	ok, err := prepareRoot(root)
	if err != nil || !ok {
		return stats, err
	}

	// Acquire Bazel's GC lock. Bazel uses this file with fcntl-style
	// POSIX advisory locks — shared while a build reads the cache,
	// exclusive during its own GC. We must take exclusive.
	unlock, err := tryLockExclusive(filepath.Join(root, repoContentsLockName))
	if err != nil {
		if errors.Is(err, errLockHeld) {
			log.Info("repo contents cache lock held by another process; skipping")
			return stats, nil
		}
		return stats, fmt.Errorf("acquire repo contents cache lock: %w", err)
	}
	defer unlock()

	entries, err := os.ReadDir(root)
	if err != nil {
		return stats, err
	}

	if !repoContentsCacheLooksPlausible(entries) {
		log.Debug("root does not look like a repo_contents_cache; skipping",
			slog.String("root", root))
		return stats, nil
	}

	// Prepare `_trash/` (skipped in dry-run mode).
	var trashDir string
	if !opts.DryRun {
		trashDir = filepath.Join(root, repoContentsTrashName)
		if err := os.MkdirAll(trashDir, 0o700); err != nil {
			return stats, fmt.Errorf("prepare trash dir: %w", err)
		}
	}

	cutoff := now().Add(-opts.MaxAge)

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == repoContentsTrashName || name == repoContentsLockName || strings.HasPrefix(name, trashPrefix) {
			continue
		}
		if !isPlausibleRepoContentsHash(name) {
			continue
		}
		if err := sweepRepoContentsHashDir(ctx, filepath.Join(root, name), trashDir, cutoff, opts, log, &stats); err != nil {
			return stats, err
		}
	}

	// Empty `_trash/` after releasing the marker-file work above. The
	// unlock defer only fires when we return; that's fine — the trash
	// entries are already disconnected from any live marker, so it's
	// safe to keep draining them even under the lock.
	if !opts.DryRun {
		if err := emptyDirContents(trashDir); err != nil {
			log.Warn("emptying trash failed", slog.String("path", trashDir), slog.Any("err", err))
		}
	}

	log.Info("repo-contents-cache sweep complete",
		slog.String("root", root),
		slog.Int("scanned", stats.Scanned),
		slog.Int("removed", stats.Removed),
		slog.Int("errors", stats.Errors),
		slog.Bool("dry_run", opts.DryRun))
	return stats, nil
}

func sweepRepoContentsHashDir(ctx context.Context, hashDir, trashDir string, cutoff time.Time, opts Options, log *slog.Logger, stats *Stats) error {
	entries, err := os.ReadDir(hashDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, recordedInputsExtension) {
			continue
		}
		stats.Scanned++
		markerPath := filepath.Join(hashDir, name)
		info, err := e.Info()
		if err != nil {
			stats.Errors++
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		uuid := strings.TrimSuffix(name, recordedInputsExtension)
		contentsPath := filepath.Join(hashDir, uuid)

		if opts.DryRun {
			stats.Removed++
			// We hold the exclusive gc_lock, so the contents dir is
			// stable under us for the duration of the walk.
			stats.Bytes += dirTreeSize(contentsPath)
			log.Info("would evict repo contents entry",
				slog.String("marker", markerPath),
				slog.String("contents", contentsPath),
				slog.Time("mtime", info.ModTime()))
			continue
		}
		// Delete the marker FIRST. If we crash between here and the
		// rename below, the next sweep will find an orphaned contents
		// dir with no marker; the classifier below treats a missing
		// marker as "not our entry" and the contents dir will be
		// cleaned up as part of the eventual full sweep. This ordering
		// matters — reversing it would leave a live marker pointing at
		// a moved contents dir, and any concurrent Bazel read (holding
		// the shared lock elsewhere in the codebase, but not necessarily
		// during a single fetch) would blow up exactly like the
		// MODULE.bazel error we chased down.
		if err := os.Remove(markerPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.ConcurrentUpdate = true
				continue
			}
			stats.Errors++
			log.Debug("remove marker failed", slog.String("path", markerPath), slog.Any("err", err))
			continue
		}
		dst := filepath.Join(trashDir, randomToken())
		if err := os.Rename(contentsPath, dst); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Marker had no paired contents dir. Fine — the
				// eviction is already effective, and there's
				// nothing to size.
				stats.Removed++
				continue
			}
			stats.Errors++
			log.Debug("rename contents failed", slog.String("path", contentsPath), slog.Any("err", err))
			continue
		}
		stats.Removed++
		// Size the tree post-rename: the dir is now isolated in
		// `_trash/` with no live workspace symlink pointing at it, so
		// the walk can't race with a concurrent Bazel process even
		// once we drop the gc_lock.
		stats.Bytes += dirTreeSize(dst)
	}
	return nil
}

// dirTreeSize sums the byte sizes of every regular file under root.
// Symlinks and errors are silently skipped — this is best-effort
// reporting, not accounting.
func dirTreeSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// repoContentsCacheLooksPlausible reports whether the given entries look
// like a Bazel repo_contents_cache — at least one hex-hash subdir or the
// reserved gc_lock/_trash names. The reserved names alone are enough to
// prove Bazel touched the directory.
func repoContentsCacheLooksPlausible(entries []fs.DirEntry) bool {
	for _, e := range entries {
		name := e.Name()
		if name == repoContentsLockName || name == repoContentsTrashName {
			return true
		}
		if e.IsDir() && isPlausibleRepoContentsHash(name) {
			return true
		}
	}
	return false
}

// isPlausibleRepoContentsHash reports whether name looks like a
// predeclared-input hash. Bazel currently uses SHA-256, so the on-disk
// name is 64 hex chars — matching that keeps us clear of Bazel's reserved
// names (`gc_lock`, `_trash`) and of anything a user might have dropped
// alongside by hand.
func isPlausibleRepoContentsHash(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// randomToken returns a short random hex string suitable for a trash
// subpath. Bazel uses UUIDs; a 16-hex-char token is plenty for our
// per-run scope.
func randomToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// emptyDirContents removes everything inside dir but leaves dir itself in
// place — Bazel's LocalRepoContentsCache expects `_trash/` to persist
// between GC runs.
func emptyDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
