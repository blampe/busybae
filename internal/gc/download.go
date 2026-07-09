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

// SweepDownloadCache evicts stale entries from the download side of a Bazel
// repository cache — the `content_addressable/` subtree that stores fetched
// tarballs and other downloaded blobs.
//
// Layout expected at root:
//
//	<root>/<hashName>/<cacheKey>/
//	   file                <-- the blob; Bazel touches its mtime on every hit
//	   id-<idhash>         <-- canonical-id markers
//	   tmp-<uuid>          <-- in-flight writes
//
// The atomic unit is <cacheKey>/. This sweep considers a key eligible when
//
//   - the `file` inside has mtime <= cutoff, AND
//   - the `file` has nlink == 1 (no live workspace hardlinks it), unless
//     IncludeHardlinked is set, AND
//   - the key dir contains no `tmp-*` entries (in-flight write in progress).
//
// Eligible keys are moved wholesale into a per-sweep trash dir at the top of
// root, then deleted after the walk completes. Bazel's own DownloadCache
// doesn't ship a GC; this sweep is the only one that touches it.
func SweepDownloadCache(ctx context.Context, root string, opts Options) (Stats, error) {
	var stats Stats
	if opts.MaxAge <= 0 {
		return stats, nil
	}
	log, now := resolveOptions(opts)
	log = log.With(slog.String("kind", "download_cache"))

	ok, err := prepareRoot(root)
	if err != nil || !ok {
		return stats, err
	}

	cleanupOldTrash(root, log)

	// The download cache is meant to be shape-checked: at least one
	// top-level entry must look like a Bazel hash-name subdir. This
	// keeps us from doing damage if a caller passes ~/ or the wrong
	// path.
	topEntries, err := os.ReadDir(root)
	if err != nil {
		return stats, err
	}
	if !downloadCacheLooksPlausible(topEntries) {
		log.Debug("root does not look like a Bazel download cache; skipping",
			slog.String("root", root))
		return stats, nil
	}

	var (
		trash   string
		cleanup func()
	)
	if !opts.DryRun {
		trash, cleanup, err = makeTrash(root, log)
		if err != nil {
			return stats, fmt.Errorf("create trash: %w", err)
		}
		defer cleanup()
	}

	cutoff := now().Add(-opts.MaxAge)

	for _, hashDir := range topEntries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if !hashDir.IsDir() || !isKnownDownloadHashName(hashDir.Name()) {
			continue
		}
		hashRoot := filepath.Join(root, hashDir.Name())
		if err := sweepDownloadHashDir(ctx, hashRoot, hashDir.Name(), trash, cutoff, opts, log, &stats); err != nil {
			return stats, err
		}
	}

	log.Info("download-cache sweep complete",
		slog.String("root", root),
		slog.Int("scanned", stats.Scanned),
		slog.Int("removed", stats.Removed),
		slog.Int64("bytes", stats.Bytes),
		slog.Int("errors", stats.Errors),
		slog.Int("skipped_hardlinks", stats.SkippedHardlinks),
		slog.Bool("dry_run", opts.DryRun))
	return stats, nil
}

func sweepDownloadHashDir(ctx context.Context, hashRoot, hashName, trash string, cutoff time.Time, opts Options, log *slog.Logger, stats *Stats) error {
	entries, err := os.ReadDir(hashRoot)
	if err != nil {
		return nil
	}
	for _, keyDir := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !keyDir.IsDir() {
			continue
		}
		stats.Scanned++
		keyPath := filepath.Join(hashRoot, keyDir.Name())
		reason, size, err := classifyDownloadKey(keyPath, cutoff, opts.IncludeHardlinked)
		if err != nil {
			stats.Errors++
			log.Debug("classify failed", slog.String("path", keyPath), slog.Any("err", err))
			continue
		}
		switch reason {
		case downloadKeep:
			continue
		case downloadHardlinked:
			stats.SkippedHardlinks++
			continue
		case downloadEvict:
			// fall through
		}
		if opts.DryRun {
			stats.Removed++
			stats.Bytes += size
			log.Info("would remove download entry",
				slog.String("path", keyPath),
				slog.Int64("bytes", size))
			continue
		}
		// Move the whole key dir into trash under a scoped path so
		// concurrent hash names don't collide.
		dstParent := filepath.Join(trash, hashName)
		if err := os.MkdirAll(dstParent, 0o700); err != nil {
			stats.Errors++
			continue
		}
		dst := filepath.Join(dstParent, keyDir.Name())
		if err := os.Rename(keyPath, dst); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.ConcurrentUpdate = true
				continue
			}
			stats.Errors++
			log.Debug("rename to trash failed", slog.String("path", keyPath), slog.Any("err", err))
			continue
		}
		stats.Removed++
		stats.Bytes += size
	}
	return nil
}

type downloadKeyReason int

const (
	downloadKeep downloadKeyReason = iota
	downloadEvict
	downloadHardlinked
)

// classifyDownloadKey decides whether a `<hashName>/<key>/` directory is
// eligible for eviction. It short-circuits on tmp-* siblings (in-flight
// writes) and treats a missing `file` as "not our business".
func classifyDownloadKey(keyPath string, cutoff time.Time, includeHardlinked bool) (downloadKeyReason, int64, error) {
	entries, err := os.ReadDir(keyPath)
	if err != nil {
		return downloadKeep, 0, err
	}
	var fileInfo os.FileInfo
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "tmp-") {
			// Bazel is mid-write; leave the entry alone.
			return downloadKeep, 0, nil
		}
		if name == "file" {
			info, err := e.Info()
			if err != nil {
				return downloadKeep, 0, err
			}
			fileInfo = info
		}
	}
	if fileInfo == nil {
		return downloadKeep, 0, nil
	}
	if fileInfo.ModTime().After(cutoff) {
		return downloadKeep, 0, nil
	}
	if !includeHardlinked && hasExtraHardlinks(fileInfo) {
		return downloadHardlinked, 0, nil
	}
	return downloadEvict, fileInfo.Size(), nil
}

// downloadCacheLooksPlausible reports whether the top-level entries look
// like a Bazel content_addressable/ subtree — at least one directory named
// after a supported hash function.
func downloadCacheLooksPlausible(entries []fs.DirEntry) bool {
	for _, e := range entries {
		if e.IsDir() && isKnownDownloadHashName(e.Name()) {
			return true
		}
	}
	return false
}

// isKnownDownloadHashName lists the hash-name subdir names Bazel's
// DownloadCache.KeyType enum exposes (see bazel.git:
// src/main/java/com/google/devtools/build/lib/bazel/repository/cache/DownloadCache.java).
func isKnownDownloadHashName(name string) bool {
	switch name {
	case "sha1", "sha256", "sha384", "sha512", "blake3":
		return true
	}
	return false
}
