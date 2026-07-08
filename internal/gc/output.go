package gc

import (
	"context"
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

// SweepOutputBases removes stale output_base directories under
// outputUserRoot. This is the mechanism for cleaning up after a workspace
// (typically a git worktree) has been removed: Bazel leaves behind an
// output_base subtree that will never be reused, and there is no built-in
// GC for it.
//
// Each top-level entry in outputUserRoot is treated as one atomic unit —
// either kept intact or moved to trash and removed. The subtree's
// "last-activity" time is taken from output_base/command.log if present
// (Bazel updates command.log on every invocation), and otherwise from the
// directory's mtime.
//
// The shared "install/" subtree is never swept; it holds the extracted
// Bazel binary that every workspace under the same output_user_root shares.
// The repository-cache directory ("cache/", used by older Bazel layouts) is
// also skipped — it is swept separately by Sweep.
//
// SweepOutputBases refuses to run if outputUserRoot doesn't at least look
// like an output_user_root (i.e. contains no plausible output_base
// directories), to avoid catastrophic damage if a caller misconfigures the
// path.
func SweepOutputBases(ctx context.Context, outputUserRoot string, opts Options) (Stats, error) {
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

	info, err := os.Stat(outputUserRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return stats, nil
		}
		return stats, err
	}
	if !info.IsDir() {
		return stats, fmt.Errorf("output_user_root %q is not a directory", outputUserRoot)
	}

	// Clean any prior-run trash before we begin.
	cleanupOldTrash(outputUserRoot, log)

	entries, err := os.ReadDir(outputUserRoot)
	if err != nil {
		return stats, err
	}

	// Safety check: refuse if nothing looks like an output_base. Prevents
	// catastrophic behavior if a caller passes ~/ or / by mistake.
	sawPlausible := false
	for _, e := range entries {
		if isPlausibleOutputBase(e.Name()) && e.IsDir() {
			sawPlausible = true
			break
		}
	}
	if !sawPlausible {
		log.Debug("no plausible output_base entries found; skipping",
			slog.String("root", outputUserRoot))
		return stats, nil
	}

	// Prepare trash (skipped in dry-run mode).
	var trash string
	if !opts.DryRun {
		t, terr := os.MkdirTemp(outputUserRoot, trashPrefix)
		if terr != nil {
			return stats, fmt.Errorf("create trash dir: %w", terr)
		}
		trash = t
		defer func() {
			if rmErr := os.RemoveAll(trash); rmErr != nil {
				log.Warn("removing trash failed",
					slog.String("path", trash), slog.Any("err", rmErr))
			}
		}()
	}

	cutoff := now().Add(-opts.MaxAge)

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, trashPrefix) {
			continue
		}
		// Never sweep the shared install base or the repository
		// cache. Anything else that doesn't look like an output_base
		// hash is left alone too.
		if name == "install" || name == "cache" {
			continue
		}
		if !isPlausibleOutputBase(name) {
			continue
		}
		stats.Scanned++
		path := filepath.Join(outputUserRoot, name)
		reason, activity, workspace, err := classifyOutputBase(path, cutoff)
		if err != nil {
			stats.Errors++
			log.Debug("classify failed", slog.String("path", path), slog.Any("err", err))
			continue
		}
		if reason == outputBaseKeep {
			continue
		}
		if reason == outputBaseOrphaned {
			stats.OrphanedWorkspaces++
		}
		if opts.DryRun {
			stats.Removed++
			log.Info("would remove output_base",
				slog.String("path", path),
				slog.String("reason", reason.String()),
				slog.String("workspace", workspace),
				slog.Time("last_activity", activity))
			continue
		}
		dst := filepath.Join(trash, name)
		if err := os.Rename(path, dst); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			stats.Errors++
			log.Warn("rename output_base to trash failed",
				slog.String("path", path), slog.Any("err", err))
			continue
		}
		stats.Removed++
		log.Info("evicted output_base",
			slog.String("path", path),
			slog.String("reason", reason.String()),
			slog.String("workspace", workspace),
			slog.Time("last_activity", activity))
	}

	log.Info("output-base sweep complete",
		slog.String("root", outputUserRoot),
		slog.Int("scanned", stats.Scanned),
		slog.Int("removed", stats.Removed),
		slog.Int("orphaned", stats.OrphanedWorkspaces),
		slog.Int("errors", stats.Errors),
		slog.Bool("dry_run", opts.DryRun))
	return stats, nil
}

// evictionReason describes why SweepOutputBases decided (or didn't decide)
// to remove an output_base.
type evictionReason int

const (
	// outputBaseKeep means the output_base is active: a live workspace
	// points at it and its last-activity time is within the cutoff.
	outputBaseKeep evictionReason = iota
	// outputBaseIdle means the workspace still exists (or is unknown)
	// but there has been no activity since the cutoff.
	outputBaseIdle
	// outputBaseOrphaned means DO_NOT_BUILD_HERE identifies a workspace
	// path that no longer exists on disk — the worktree or checkout has
	// been removed. Eligible regardless of mtime.
	outputBaseOrphaned
)

func (r evictionReason) String() string {
	switch r {
	case outputBaseKeep:
		return "keep"
	case outputBaseIdle:
		return "idle"
	case outputBaseOrphaned:
		return "orphaned"
	}
	return "unknown"
}

// classifyOutputBase decides the eviction reason for one output_base
// subtree.
//
// The strongest signal is DO_NOT_BUILD_HERE: Bazel writes this file into
// every output_base with the absolute path of the workspace root it was
// created for. When that path is gone from disk, the output_base is
// definitively orphaned (someone removed the worktree) and can be evicted
// regardless of mtime.
//
// When the workspace still exists — or when DO_NOT_BUILD_HERE is missing
// or unreadable — we fall back to a last-activity check against cutoff,
// using command.log's mtime with a directory-mtime backup.
func classifyOutputBase(path string, cutoff time.Time) (reason evictionReason, activity time.Time, workspace string, err error) {
	workspace, workspaceKnown, workspaceExists := workspaceFromDoNotBuildHere(path)
	if workspaceKnown && !workspaceExists {
		return outputBaseOrphaned, time.Time{}, workspace, nil
	}
	activity, err = lastActivity(path)
	if err != nil {
		return outputBaseKeep, time.Time{}, workspace, err
	}
	if activity.After(cutoff) {
		return outputBaseKeep, activity, workspace, nil
	}
	return outputBaseIdle, activity, workspace, nil
}

// workspaceFromDoNotBuildHere reads output_base/DO_NOT_BUILD_HERE.
//
// The returned workspace string is empty when the marker is missing or its
// content isn't an absolute path we can act on. `known` reports whether we
// have a workspace path to check at all; `exists` reports whether that path
// currently resolves to something on disk (meaningless when known is
// false).
func workspaceFromDoNotBuildHere(outputBase string) (workspace string, known, exists bool) {
	// Cap the read: the marker is a single path, but we don't want to
	// slurp an arbitrarily large file if something has replaced it.
	f, err := os.Open(filepath.Join(outputBase, "DO_NOT_BUILD_HERE"))
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	content := strings.TrimSpace(string(buf[:n]))
	if content == "" || !filepath.IsAbs(content) {
		return "", false, false
	}
	if _, err := os.Stat(content); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return content, true, false
		}
		// A permission error (or any other stat failure) is ambiguous:
		// the workspace might still exist, we just can't see it. Treat
		// as "unknown" and fall back to mtime.
		return content, false, false
	}
	return content, true, true
}

// isPlausibleOutputBase reports whether name looks like a Bazel output_base
// hash — a 32-character hex string (Bazel computes MD5(workspace path)).
func isPlausibleOutputBase(name string) bool {
	if len(name) != 32 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// lastActivity returns the most recent signal of activity in an output_base.
// It prefers command.log's mtime (Bazel touches it on every invocation),
// falling back to the directory mtime when command.log is missing.
func lastActivity(outputBase string) (time.Time, error) {
	if info, err := os.Stat(filepath.Join(outputBase, "command.log")); err == nil {
		return info.ModTime(), nil
	}
	info, err := os.Stat(outputBase)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
