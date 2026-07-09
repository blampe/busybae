// Package gc implements cache-specific sweeps for the four cache directories
// that Bazel does not clean up on its own or for which the user wants a more
// aggressive policy than Bazel's built-in GC:
//
//   - repository/download cache (`content_addressable/<hashName>/<key>/`)
//   - repository/contents cache (`contents/<predeclaredInputHash>/<UUID>/`)
//   - disk cache (`ac/`, `cas/`)
//   - output_user_root (whole output_base subtrees for gone workspaces)
//
// Each cache has a different atomicity boundary, and mixing them up is how
// you delete a MODULE.bazel out from under a live build. The sweep functions
// here mirror Bazel's own GC algorithms (see LocalRepoContentsCache,
// DiskCacheGarbageCollector, DownloadCache in the Bazel tree) so a concurrent
// Bazel process and a busybae sweep never disagree about what's safe to
// remove.
//
// Shared concerns live here; per-cache logic lives in one file each.
package gc

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trashPrefix names busybae's own per-sweep trash directories. The prefix
// is chosen so a) it can never collide with a Bazel-managed cache entry
// (all of Bazel's entries are hex or reserved names like `install`), and
// b) a leftover trash tree from a crashed sweep is easy to collect on the
// next run.
const trashPrefix = ".busybae-trash-"

// Options configures a sweep. Not every field is honored by every sweep
// (see per-function comments).
type Options struct {
	// MaxAge is the mtime cutoff. Files older than now-MaxAge are
	// eligible for removal. Zero means "never eligible" (no-op sweep).
	MaxAge time.Duration
	// DryRun logs what would be removed without renaming/deleting.
	DryRun bool
	// IncludeHardlinked, when true, evicts cache entries even when they
	// have additional hardlinks pointing at them. Only meaningful for
	// the download cache (see SweepDownloadCache).
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
	// ConcurrentUpdate reports whether the sweep noticed an entry
	// changing under it between scan and delete. It's informational —
	// the sweep still succeeds — but a persistently high rate suggests
	// the interval is too aggressive.
	ConcurrentUpdate bool
}

// resolveOptions fills in the standard defaults for logger and clock hooks.
func resolveOptions(opts Options) (*slog.Logger, func() time.Time) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return log, now
}

// prepareRoot stats root, treating a missing directory as a no-op. Returns
// ok=false when the caller should return early with zero stats.
func prepareRoot(root string) (ok bool, err error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, &fs.PathError{Op: "stat", Path: root, Err: fs.ErrInvalid}
	}
	return true, nil
}

// cleanupOldTrash removes any residual busybae trash directories from prior
// runs. Failures are logged and swallowed; they don't affect correctness.
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

// makeTrash creates a fresh trash directory inside root. Callers must call
// the cleanup func exactly once (typically via defer).
func makeTrash(root string, log *slog.Logger) (trash string, cleanup func(), err error) {
	t, err := os.MkdirTemp(root, trashPrefix)
	if err != nil {
		return "", nil, err
	}
	return t, func() {
		if rmErr := os.RemoveAll(t); rmErr != nil {
			log.Warn("removing trash failed", slog.String("path", t), slog.Any("err", rmErr))
		}
	}, nil
}
