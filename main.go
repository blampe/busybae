// busybae watches Bazel's non-native-GC cache directories (repository_cache,
// disk_cache, repo_contents_cache) and periodically evicts entries older
// than a configurable age.
//
// It is designed to be invoked on every `bazel` command via a bazelisk
// wrapper. The invocation is cheap: if a daemon is already running, the
// process opens a Unix socket, sends a single-byte poke, and exits. If no
// daemon is running, it forks a detached one and exits. The daemon itself
// exits after a configurable idle period, so a user who stops running bazel
// stops paying for the daemon.
//
// Cache directories are discovered by parsing the workspace's .bazelrc
// (including transitive `import` / `try-import` files), not by hardcoding
// their default locations.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"busybae/internal/bazelrc"
	"busybae/internal/daemon"
	"busybae/internal/gc"
	"busybae/internal/version"
	"busybae/internal/wrapper"
)

const (
	defaultIdleTimeout      = 30 * time.Minute
	defaultGCInterval       = 1 * time.Hour
	defaultMaxAge           = 7 * 24 * time.Hour
	defaultMaxAgeOutputBase = 30 * 24 * time.Hour
)

// zeroDuration is the sentinel meaning "unset — fall back to --max-age".
// Duration flags can't be nil, so we use 0.
const zeroDuration = time.Duration(0)

type globalFlags struct {
	socket            string
	workspace         string
	idleTimeout       time.Duration
	gcInterval        time.Duration
	maxAge            time.Duration
	maxAgeDownload    time.Duration
	maxAgeDiskCache   time.Duration
	maxAgeRepoContent time.Duration
	maxAgeOutputBase  time.Duration
	sweepOutputBase   bool
	dryRun            bool
	logPath           string
	verbose           bool
	includeHardlinked bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.socket, "socket", "", "Unix socket path (default: derived from workspace)")
	fs.StringVar(&g.workspace, "workspace", "", "Workspace root (default: walk up from cwd)")
	fs.DurationVar(&g.idleTimeout, "idle-timeout", defaultIdleTimeout, "Daemon exits after this long without a poke")
	fs.DurationVar(&g.gcInterval, "gc-interval", defaultGCInterval, "How often the daemon sweeps")
	fs.DurationVar(&g.maxAge, "max-age", defaultMaxAge, "Default cache-entry age cutoff (per-type flags override)")
	fs.DurationVar(&g.maxAgeDownload, "max-age-repository-cache", zeroDuration, "Override --max-age for the --repository_cache download subtree (0 = inherit)")
	fs.DurationVar(&g.maxAgeDiskCache, "max-age-disk-cache", zeroDuration, "Override --max-age for --disk_cache (0 = inherit)")
	fs.DurationVar(&g.maxAgeRepoContent, "max-age-repo-contents-cache", zeroDuration, "Override --max-age for --repo_contents_cache (0 = inherit)")
	fs.DurationVar(&g.maxAgeOutputBase, "max-age-output-base", defaultMaxAgeOutputBase, "Age cutoff for orphaned output_base subtrees (only used with --sweep-output-base)")
	fs.BoolVar(&g.sweepOutputBase, "sweep-output-base", false, "Also sweep stale output_base directories under output_user_root (worktree cleanup)")
	fs.BoolVar(&g.dryRun, "dry-run", false, "Log what would be removed without deleting anything")
	fs.StringVar(&g.logPath, "log", "", "Log file (default: alongside socket)")
	fs.BoolVar(&g.verbose, "verbose", false, "Debug-level logging")
	fs.BoolVar(&g.includeHardlinked, "include-hardlinked", false, "Evict entries even when a live workspace still hardlinks them")
}

// maxAgeFor picks the effective max age for a cache-type flag, falling back
// to the shared --max-age when the specific one is zero.
func (g globalFlags) maxAgeFor(specific time.Duration) time.Duration {
	if specific > 0 {
		return specific
	}
	return g.maxAge
}

// applyBazelrcTTL overrides the max-age defaults with the workspace's
// disk_cache GC age when the user didn't pass --max-age / --max-age-output-base
// explicitly. This keeps busybae's eviction policy tied to the same TTL Bazel
// uses for its own disk-cache GC, so the two agree on what "old" means.
func (g *globalFlags) applyBazelrcTTL(fs *flag.FlagSet, bazelrcAge time.Duration) {
	if bazelrcAge <= 0 {
		return
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["max-age"] {
		g.maxAge = bazelrcAge
	}
	if !set["max-age-output-base"] {
		g.maxAgeOutputBase = bazelrcAge
	}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Default subcommand is "poke" (spawn if not running).
	sub := "poke"
	rest := args
	if len(args) > 0 {
		switch args[0] {
		case "poke", "daemon", "gc", "dirs", "install-wrapper", "version", "help", "-h", "--help":
			sub = args[0]
			rest = args[1:]
		}
	}
	switch sub {
	case "help", "-h", "--help":
		printHelp()
		return 0
	case "version":
		fmt.Printf("busybae %s (%s)\n", version.Version, version.Commit)
		return 0
	case "poke":
		return cmdPoke(rest)
	case "daemon":
		return cmdDaemon(rest)
	case "gc":
		return cmdGC(rest)
	case "dirs":
		return cmdDirs(rest)
	case "install-wrapper":
		return cmdInstallWrapper(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", sub)
		return 2
	}
}

func cmdInstallWrapper(args []string) int {
	fs := flag.NewFlagSet("install-wrapper", flag.ContinueOnError)
	var (
		out        = fs.String("out", wrapper.DefaultOutputPath, "Output path for the wrapper")
		releaseURL = fs.String("release-url", wrapper.DefaultReleaseURL, "Base URL for release artifacts")
		shaFile    = fs.String("sha-file", "", "Read SHA256SUMS from a local file instead of fetching from --release-url")
		versionArg = fs.String("version", "", "Override the pinned version (defaults to this binary's stamped version)")
		dryRun     = fs.Bool("dry-run", false, "Print the rendered wrapper to stdout without writing")
		check      = fs.Bool("check", false, "Exit non-zero if the wrapper on disk differs (for CI drift detection)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v := *versionArg
	if v == "" {
		v = version.Version
	}
	if err := wrapper.Install(wrapper.Options{
		Version:    v,
		ReleaseURL: *releaseURL,
		SHAFile:    *shaFile,
		Out:        *out,
		DryRun:     *dryRun,
		Check:      *check,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func printHelp() {
	fmt.Print(`busybae — background GC daemon for Bazel caches.

Usage:
  busybae [poke]        Poke the daemon; start one if none is running.
  busybae daemon        Run the daemon in the foreground (add --once for a single sweep).
  busybae gc            One-shot sweep, no daemon.
  busybae dirs          Print the cache directories discovered from .bazelrc.
  busybae install-wrapper  Write the bazelisk wrapper (tools/bazel) for this repo.
  busybae version       Print the busybae version.

Common flags (see 'busybae daemon -h' for the full list):
  --socket PATH                    Override socket path
  --workspace PATH                 Workspace root (default: walk up from cwd)
  --idle-timeout DUR               Daemon exits after this long idle (default 30m)
  --gc-interval DUR                Sweep interval (default 1h)
  --max-age DUR                    Default age cutoff (default 168h)
  --max-age-repository-cache DUR   Override for --repository_cache
  --max-age-disk-cache DUR         Override for --disk_cache
  --max-age-repo-contents-cache DUR Override for --repo_contents_cache
  --sweep-output-base              Also sweep stale output_base subtrees
  --max-age-output-base DUR        Age cutoff for output_base sweep (default 720h)
  --include-hardlinked             Evict even hardlinked entries
  --dry-run                        Print what would be removed
  --verbose                        Debug logging
`)
}

// resolveSocket computes the socket path from --socket or, when unset, from
// the workspace path.
func resolveSocket(explicit, workspace string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	tmp := os.TempDir()
	if runtime.GOOS == "linux" {
		if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
			tmp = xdg
		}
	}
	h := sha256.Sum256([]byte(workspace))
	name := "busybae-" + hex.EncodeToString(h[:4]) + ".sock"
	return filepath.Join(tmp, name), nil
}

// resolveWorkspace returns explicit if set, otherwise walks up from cwd
// looking for MODULE.bazel or WORKSPACE(.bazel) markers.
func resolveWorkspace(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		for _, marker := range []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no MODULE.bazel/WORKSPACE found above %s", cwd)
		}
		dir = parent
	}
}

// sweepTarget describes one directory to sweep along with the type-specific
// max age that applies to it.
type sweepTarget struct {
	Kind   string // one of the sweepKind* constants
	Path   string
	MaxAge time.Duration
}

// Kinds are distinct on-disk formats; each dispatches to its own sweep
// function in runSweeps.
const (
	sweepKindDownload     = "download_cache"      // <root>/<hashName>/<key>/{file,id-*}
	sweepKindDiskCache    = "disk_cache"          // <root>/{ac,cas,tmp,gc}
	sweepKindRepoContents = "repo_contents_cache" // <root>/<hash>/{<UUID>,<UUID>.recorded_inputs}
	sweepKindOutputBase   = "output_base"         // <output_user_root>/<32-hex>/
)

func discoverDirs(workspace string) (bazelrc.CacheDirs, error) {
	home, _ := os.UserHomeDir()
	l := &bazelrc.Loader{Workspace: workspace, Home: home}
	entries, err := l.Load()
	if err != nil {
		return bazelrc.CacheDirs{}, err
	}
	return bazelrc.ExtractCacheDirs(entries, workspace, home, activeConfigs(entries))
}

// activeConfigs returns the .bazelrc :config suffixes that would fire on this
// host. Today that's just the OS-specific config activated by Bazel's
// --enable_platform_specific_config (linux/macos/windows/freebsd/openbsd). If
// the flag never appears, no auto-config is active.
//
// The flag is only consulted on `build` and `common` entries because
// cache-dir flags all live under `build`. Other commands (e.g. `info
// --noenable_platform_specific_config`) do not affect our discovery.
func activeConfigs(entries []bazelrc.Entry) []string {
	var enabled bool
	for _, e := range entries {
		if e.Command != "build" && e.Command != "common" {
			continue
		}
		if bazelrc.HasFlag(e.Args, "enable_platform_specific_config") {
			enabled = true
		}
		if bazelrc.HasFlag(e.Args, "noenable_platform_specific_config") {
			enabled = false
		}
	}
	if !enabled {
		return nil
	}
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "macos"
	case "linux", "windows", "freebsd", "openbsd":
		name = runtime.GOOS
	default:
		return nil
	}
	return []string{name}
}

// sweepTargets zips the discovered cache dirs with the per-type max ages
// the user configured, dropping any that are unset. The download-cache
// and repo-contents-cache subtrees are separate targets even when they
// share a `--repository_cache` root — they have different on-disk layouts
// and need different sweep algorithms.
func (g globalFlags) sweepTargets(c bazelrc.CacheDirs) []sweepTarget {
	var out []sweepTarget
	if c.DownloadCache != "" {
		out = append(out, sweepTarget{
			Kind: sweepKindDownload, Path: c.DownloadCache,
			MaxAge: g.maxAgeFor(g.maxAgeDownload),
		})
	}
	if c.DiskCache != "" {
		out = append(out, sweepTarget{
			Kind: sweepKindDiskCache, Path: c.DiskCache,
			MaxAge: g.maxAgeFor(g.maxAgeDiskCache),
		})
	}
	if c.RepoContentsCache != "" {
		out = append(out, sweepTarget{
			Kind: sweepKindRepoContents, Path: c.RepoContentsCache,
			MaxAge: g.maxAgeFor(g.maxAgeRepoContent),
		})
	}
	return out
}

func newLogger(verbose bool, logPath string) (*slog.Logger, io.Closer, error) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	var w io.Writer = os.Stderr
	var closer io.Closer
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closer = f
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h), closer, nil
}

func cmdPoke(args []string) int {
	var g globalFlags
	fs := flag.NewFlagSet("poke", flag.ContinueOnError)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ws, err := resolveWorkspace(g.workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sock, err := resolveSocket(g.socket, ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	err = daemon.Poke(sock, 500*time.Millisecond)
	if err == nil {
		return 0
	}
	if !daemon.IsUnavailable(err) {
		fmt.Fprintf(os.Stderr, "poke error: %v\n", err)
		return 1
	}

	// Spawn a detached daemon and return.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve executable: %v\n", err)
		return 1
	}
	// Resolve any symlink so `os.StartProcess` after the wrapper is
	// gone still points to a real file.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	daemonArgs := []string{"daemon"}
	daemonArgs = append(daemonArgs, forwardFlags(g)...)
	logPath := g.logPath
	if logPath == "" {
		logPath = defaultLogPath(sock)
	}
	if err := daemon.SpawnDetached(exe, daemonArgs, logPath); err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		return 1
	}
	return 0
}

// defaultLogPath places the daemon log next to its socket, replacing the
// .sock suffix with .log.
func defaultLogPath(sock string) string {
	base := sock
	if filepath.Ext(base) == ".sock" {
		base = base[:len(base)-len(".sock")]
	}
	return base + ".log"
}

// forwardFlags reconstructs command-line flags from g so the spawned daemon
// sees the same configuration the CLI was invoked with.
func forwardFlags(g globalFlags) []string {
	var out []string
	if g.socket != "" {
		out = append(out, "--socket", g.socket)
	}
	if g.workspace != "" {
		out = append(out, "--workspace", g.workspace)
	}
	if g.idleTimeout != defaultIdleTimeout {
		out = append(out, "--idle-timeout", g.idleTimeout.String())
	}
	if g.gcInterval != defaultGCInterval {
		out = append(out, "--gc-interval", g.gcInterval.String())
	}
	if g.maxAge != defaultMaxAge {
		out = append(out, "--max-age", g.maxAge.String())
	}
	if g.dryRun {
		out = append(out, "--dry-run")
	}
	if g.includeHardlinked {
		out = append(out, "--include-hardlinked")
	}
	if g.maxAgeDownload > 0 {
		out = append(out, "--max-age-repository-cache", g.maxAgeDownload.String())
	}
	if g.maxAgeDiskCache > 0 {
		out = append(out, "--max-age-disk-cache", g.maxAgeDiskCache.String())
	}
	if g.maxAgeRepoContent > 0 {
		out = append(out, "--max-age-repo-contents-cache", g.maxAgeRepoContent.String())
	}
	if g.maxAgeOutputBase != defaultMaxAgeOutputBase {
		out = append(out, "--max-age-output-base", g.maxAgeOutputBase.String())
	}
	if g.sweepOutputBase {
		out = append(out, "--sweep-output-base")
	}
	if g.logPath != "" {
		out = append(out, "--log", g.logPath)
	}
	if g.verbose {
		out = append(out, "--verbose")
	}
	return out
}

func cmdDaemon(args []string) int {
	var g globalFlags
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	g.register(fs)
	once := fs.Bool("once", false, "Run a single sweep and exit without binding a socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ws, err := resolveWorkspace(g.workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log, closer, err := newLogger(g.verbose, g.logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log: %v\n", err)
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}

	cache, err := discoverDirs(ws)
	if err != nil {
		log.Warn("could not discover cache dirs", slog.Any("err", err))
	}
	g.applyBazelrcTTL(fs, cache.DiskCacheGCMaxAge)
	targets := g.sweepTargets(cache)
	log.Info("discovered cache dirs",
		slog.Any("targets", summarizeTargets(targets)),
		slog.String("output_user_root", cache.OutputUserRoot),
		slog.Bool("sweep_output_base", g.sweepOutputBase),
		slog.Duration("bazelrc_gc_max_age", cache.DiskCacheGCMaxAge),
		slog.String("workspace", ws))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if *once {
		results := runSweeps(ctx, log, g, targets, cache.OutputUserRoot)
		printSweepSummary(os.Stderr, results, g.dryRun)
		return 0
	}

	sock, err := resolveSocket(g.socket, ws)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if len(targets) == 0 && !(g.sweepOutputBase && cache.OutputUserRoot != "") {
		log.Info("no dirs to sweep — daemon will still run so pokes keep it alive")
	}

	// Run a sweep once at startup so a long-running daemon isn't
	// gated on the first tick.
	sweep := func(sctx context.Context) {
		runSweeps(sctx, log, g, targets, cache.OutputUserRoot)
	}
	go sweep(ctx)

	err = daemon.Run(ctx, daemon.Config{
		SocketPath:  sock,
		IdleTimeout: g.idleTimeout,
		GCInterval:  g.gcInterval,
		OnGC:        sweep,
		Logger:      log,
	})
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		log.Info("another daemon is already running")
		return 0
	}
	if err != nil {
		log.Error("daemon failed", slog.Any("err", err))
		return 1
	}
	return 0
}

func cmdGC(args []string) int {
	var g globalFlags
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ws, err := resolveWorkspace(g.workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log, closer, err := newLogger(g.verbose, g.logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log: %v\n", err)
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}
	cache, err := discoverDirs(ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", err)
		return 1
	}
	g.applyBazelrcTTL(fs, cache.DiskCacheGCMaxAge)
	targets := g.sweepTargets(cache)
	if len(targets) == 0 && !(g.sweepOutputBase && cache.OutputUserRoot != "") {
		fmt.Fprintln(os.Stderr, "no dirs to sweep")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	results := runSweeps(ctx, log, g, targets, cache.OutputUserRoot)
	printSweepSummary(os.Stderr, results, g.dryRun)
	return 0
}

// sweepResult records the stats from a single sweep for summary reporting.
type sweepResult struct {
	Kind  string
	Path  string
	Stats gc.Stats
}

// runSweeps dispatches each target to the sweep function that matches its
// cache layout. Errors from any one sweep are logged and swallowed — a
// broken repository cache should not skip disk_cache eviction.
func runSweeps(ctx context.Context, log *slog.Logger, g globalFlags, targets []sweepTarget, outputUserRoot string) []sweepResult {
	results := make([]sweepResult, 0, len(targets)+1)
	for _, t := range targets {
		opts := gc.Options{
			MaxAge:            t.MaxAge,
			DryRun:            g.dryRun,
			IncludeHardlinked: g.includeHardlinked,
			Logger:            log,
		}
		var (
			stats gc.Stats
			err   error
		)
		switch t.Kind {
		case sweepKindDownload:
			stats, err = gc.SweepDownloadCache(ctx, t.Path, opts)
		case sweepKindDiskCache:
			stats, err = gc.SweepDiskCache(ctx, t.Path, opts)
		case sweepKindRepoContents:
			stats, err = gc.SweepRepoContentsCache(ctx, t.Path, opts)
		default:
			log.Warn("unknown sweep kind — skipping",
				slog.String("kind", t.Kind),
				slog.String("dir", t.Path))
			continue
		}
		if err != nil {
			log.Warn("sweep failed",
				slog.String("kind", t.Kind),
				slog.String("dir", t.Path),
				slog.Any("err", err))
		}
		results = append(results, sweepResult{Kind: t.Kind, Path: t.Path, Stats: stats})
	}
	if g.sweepOutputBase && outputUserRoot != "" {
		stats, err := gc.SweepOutputBases(ctx, outputUserRoot, gc.Options{
			MaxAge: g.maxAgeOutputBase,
			DryRun: g.dryRun,
			Logger: log.With(slog.String("kind", sweepKindOutputBase)),
		})
		if err != nil {
			log.Warn("output-base sweep failed",
				slog.String("root", outputUserRoot),
				slog.Any("err", err))
		}
		results = append(results, sweepResult{Kind: sweepKindOutputBase, Path: outputUserRoot, Stats: stats})
	}
	return results
}

// printSweepSummary writes a compact human-readable summary of one or more
// sweeps. It flips its verbs between "removed" and "would remove" so the
// dry-run and real-run outputs stay directly comparable.
func printSweepSummary(w io.Writer, results []sweepResult, dryRun bool) {
	if len(results) == 0 {
		return
	}
	verb := "removed"
	header := "sweep summary"
	if dryRun {
		verb = "would remove"
		header = "sweep summary (dry run — nothing was deleted)"
	}
	fmt.Fprintln(w, header)
	var totalRemoved, totalErrors, totalSkipped, totalOrphaned int
	var totalBytes int64
	for _, r := range results {
		fmt.Fprintf(w, "  %-20s %s\n", r.Kind, r.Path)
		fmt.Fprintf(w, "    %s %d of %d entries (%s)",
			verb, r.Stats.Removed, r.Stats.Scanned, humanBytes(r.Stats.Bytes))
		if r.Stats.SkippedHardlinks > 0 {
			fmt.Fprintf(w, ", skipped %d hardlinked", r.Stats.SkippedHardlinks)
		}
		if r.Stats.OrphanedWorkspaces > 0 {
			fmt.Fprintf(w, ", %d orphaned workspaces", r.Stats.OrphanedWorkspaces)
		}
		if r.Stats.Errors > 0 {
			fmt.Fprintf(w, ", %d errors", r.Stats.Errors)
		}
		fmt.Fprintln(w)
		totalRemoved += r.Stats.Removed
		totalErrors += r.Stats.Errors
		totalSkipped += r.Stats.SkippedHardlinks
		totalOrphaned += r.Stats.OrphanedWorkspaces
		totalBytes += r.Stats.Bytes
	}
	if len(results) > 1 {
		fmt.Fprintf(w, "  total: %s %d entries (%s)",
			verb, totalRemoved, humanBytes(totalBytes))
		if totalSkipped > 0 {
			fmt.Fprintf(w, ", skipped %d hardlinked", totalSkipped)
		}
		if totalOrphaned > 0 {
			fmt.Fprintf(w, ", %d orphaned workspaces", totalOrphaned)
		}
		if totalErrors > 0 {
			fmt.Fprintf(w, ", %d errors", totalErrors)
		}
		fmt.Fprintln(w)
	}
}

// humanBytes renders n as a short IEC size string. Under 1 KiB we print the
// raw byte count.
func humanBytes(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
		tib = 1 << 40
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	}
	return fmt.Sprintf("%d B", n)
}

// summarizeTargets renders targets in a form suitable for log attributes.
func summarizeTargets(ts []sweepTarget) []map[string]string {
	out := make([]map[string]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]string{
			"kind":    t.Kind,
			"path":    t.Path,
			"max_age": t.MaxAge.String(),
		})
	}
	return out
}

func cmdDirs(args []string) int {
	var g globalFlags
	fs := flag.NewFlagSet("dirs", flag.ContinueOnError)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ws, err := resolveWorkspace(g.workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cache, err := discoverDirs(ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", err)
		return 1
	}
	g.applyBazelrcTTL(fs, cache.DiskCacheGCMaxAge)
	targets := g.sweepTargets(cache)
	fmt.Printf("workspace:          %s\n", ws)
	fmt.Printf("repository_cache:   %s\n", cache.RepositoryCache)
	fmt.Printf("  download subtree: %s (max-age %s)\n", cache.DownloadCache, g.maxAgeFor(g.maxAgeDownload))
	fmt.Printf("  contents subtree: %s (max-age %s)\n", cache.RepoContentsCache, g.maxAgeFor(g.maxAgeRepoContent))
	fmt.Printf("disk_cache:         %s (max-age %s)\n", cache.DiskCache, g.maxAgeFor(g.maxAgeDiskCache))
	outputNote := "(not swept — pass --sweep-output-base to enable)"
	if g.sweepOutputBase {
		outputNote = fmt.Sprintf("(swept, max-age %s)", g.maxAgeOutputBase)
	}
	fmt.Printf("output_user_root:   %s %s\n", cache.OutputUserRoot, outputNote)
	fmt.Printf("swept targets:      %d\n", len(targets))
	return 0
}
