package bazelrc

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CacheDirs holds cache directories declared across the loaded .bazelrc
// entries. Paths are resolved relative to the .bazelrc file that declared
// them; unset fields remain empty strings.
//
// When a flag appears multiple times, the last occurrence wins — matching
// Bazel's own semantics.
type CacheDirs struct {
	// RepositoryCache is --repository_cache.
	RepositoryCache string
	// DiskCache is --disk_cache.
	DiskCache string
	// RepoContentsCache is --repo_contents_cache (Bazel 7+).
	RepoContentsCache string
	// OutputUserRoot is startup --output_user_root.
	OutputUserRoot string
	// DiskCacheGCMaxAge is --experimental_disk_cache_gc_max_age, if the
	// workspace pinned one. Zero when unset. Callers use this as an
	// authoritative TTL for all cache sweeps so busybae stays in lockstep
	// with Bazel's own GC policy.
	DiskCacheGCMaxAge time.Duration
}

// ExtractCacheDirs scans entries for cache-related flags and resolves them
// against the workspace and home directories.
//
// Only user-facing cache dirs are pulled out: repository_cache, disk_cache,
// and repo_contents_cache. output_user_root is exposed for completeness but
// is managed by Bazel itself and should not be swept by a third-party GC.
//
// Entries scoped to a `:config` are considered only when that config is in
// activeConfigs. Unscoped entries always apply.
func ExtractCacheDirs(entries []Entry, workspace, home string, activeConfigs []string) (CacheDirs, error) {
	active := make(map[string]bool, len(activeConfigs))
	for _, c := range activeConfigs {
		active[c] = true
	}
	var c CacheDirs
	// Track the "winning" occurrence's source file so we can resolve
	// relative paths against the right base directory.
	var repoSrc, diskSrc, contentsSrc, ourSrc string
	for _, e := range entries {
		if e.Config != "" && !active[e.Config] {
			continue
		}
		// repository/disk caches can appear under any command; startup
		// flags stay under `startup`. Bazel accepts them under
		// `common` and any command name too. We accept anywhere; it's
		// harmless for GC to over-recognize.
		if v, ok := ExtractFlag(e.Args, "repository_cache"); ok {
			c.RepositoryCache = v
			repoSrc = e.Source
		}
		if v, ok := ExtractFlag(e.Args, "disk_cache"); ok {
			c.DiskCache = v
			diskSrc = e.Source
		}
		if v, ok := ExtractFlag(e.Args, "repo_contents_cache"); ok {
			c.RepoContentsCache = v
			contentsSrc = e.Source
		}
		if e.Command == "startup" {
			if v, ok := ExtractFlag(e.Args, "output_user_root"); ok {
				c.OutputUserRoot = v
				ourSrc = e.Source
			}
		}
		if v, ok := ExtractFlag(e.Args, "experimental_disk_cache_gc_max_age"); ok {
			d, derr := ParseBazelDuration(v)
			if derr != nil {
				return c, fmt.Errorf("%s: parse experimental_disk_cache_gc_max_age=%q: %w", e.Source, v, derr)
			}
			c.DiskCacheGCMaxAge = d
		}
	}
	resolve := func(dst *string, src string) error {
		if *dst == "" {
			return nil
		}
		r, err := Resolve(*dst, filepath.Dir(src), workspace, home)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", *dst, err)
		}
		*dst = r
		return nil
	}
	if err := resolve(&c.RepositoryCache, repoSrc); err != nil {
		return c, err
	}
	if err := resolve(&c.DiskCache, diskSrc); err != nil {
		return c, err
	}
	if err := resolve(&c.RepoContentsCache, contentsSrc); err != nil {
		return c, err
	}
	if err := resolve(&c.OutputUserRoot, ourSrc); err != nil {
		return c, err
	}
	return c, nil
}

// ParseBazelDuration parses the duration format Bazel accepts for its GC-age
// flags — the standard Go units (ns, us, µs, ms, s, m, h) plus "d" for days.
// Go's time.ParseDuration does not understand "d", so we peel a trailing "d"
// off and translate it to hours before delegating.
func ParseBazelDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid days value: %w", err)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// SweepableDirs returns the cache directories that busybae should GC. It
// excludes OutputUserRoot (managed by Bazel) and drops empty entries.
func (c CacheDirs) SweepableDirs() []string {
	out := make([]string, 0, 3)
	for _, d := range []string{c.RepositoryCache, c.DiskCache, c.RepoContentsCache} {
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}
