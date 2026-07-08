package bazelrc

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Loader discovers and expands the effective set of .bazelrc entries for a
// workspace. It follows Bazel's precedence:
//
//  1. System rc (default /etc/bazel.bazelrc, unless startup --nosystem_rc).
//  2. Workspace rc (<workspace>/.bazelrc, unless startup --noworkspace_rc).
//  3. Home rc (~/.bazelrc, unless startup --nohome_rc).
//  4. Any --bazelrc paths provided via ExtraRCs (in order).
//
// Within each source, `import`/`try-import` entries are followed depth-first,
// with cycle detection. Startup flags that disable rc sources are evaluated
// as they appear.
type Loader struct {
	// Workspace is the workspace root (used to locate the workspace rc
	// and to expand %workspace%). May be empty; in that case the
	// workspace rc is skipped.
	Workspace string

	// Home is the user's home directory (used to expand `~`). Defaults
	// to os.UserHomeDir when empty.
	Home string

	// SystemRC overrides the system rc path. Defaults to
	// /etc/bazel.bazelrc on Unix. Set to a blank string to keep the
	// default; set the special value "-" to disable.
	SystemRC string

	// ExtraRCs are additional rc files loaded after the standard set (as
	// though passed via `--bazelrc=<path>` on the command line).
	ExtraRCs []string

	// FS is an optional filesystem hook, primarily for tests. When nil,
	// the real filesystem is used.
	FS FS
}

// FS abstracts file access so tests can inject an in-memory tree.
type FS interface {
	ReadFile(path string) ([]byte, error)
	Stat(path string) (fs.FileInfo, error)
}

type realFS struct{}

var _ FS = realFS{}

func (realFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (realFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// defaultSystemRC returns the platform-specific default system rc path, or ""
// when Bazel does not consult one on this platform.
func defaultSystemRC() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\bazel.bazelrc`
	}
	return "/etc/bazel.bazelrc"
}

// Load walks the discovery chain and returns entries in declaration order,
// with `import`/`try-import` directives expanded inline. import entries
// themselves are removed from the result — only the entries they pull in
// remain.
func (l *Loader) Load() ([]Entry, error) {
	fsys := l.FS
	if fsys == nil {
		fsys = realFS{}
	}
	home := l.Home
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}

	// We first read the workspace rc (if any) to check for startup flags
	// that disable the system/home rc. That's how bazel does it: startup
	// flags in the workspace rc control whether the other rcs are read
	// even though the workspace rc is not the first in precedence order.
	//
	// To keep the flow simple, we peek at ALL rc files for their startup
	// flags before deciding what to include. In practice these flags are
	// only meaningfully set in one place.
	systemRC := l.SystemRC
	if systemRC == "" {
		systemRC = defaultSystemRC()
	}

	// Candidates in precedence order.
	type candidate struct {
		path     string
		required bool
	}
	var candidates []candidate
	if systemRC != "" && systemRC != "-" {
		candidates = append(candidates, candidate{systemRC, false})
	}
	if l.Workspace != "" {
		candidates = append(candidates, candidate{filepath.Join(l.Workspace, ".bazelrc"), false})
	}
	if home != "" {
		candidates = append(candidates, candidate{filepath.Join(home, ".bazelrc"), false})
	}
	for _, p := range l.ExtraRCs {
		candidates = append(candidates, candidate{p, true})
	}

	// First pass: collect startup flags from every candidate so we know
	// which sources to skip on the real pass. A missing top-level rc
	// (for non-required candidates) is not an error — but a missing
	// nested import inside a present rc still is, so we split the
	// "top-level exists?" check from the actual load.
	var skipSystem, skipWorkspace, skipHome bool
	for _, c := range candidates {
		if _, err := fsys.Stat(c.path); err != nil {
			if !c.required && isNotExist(err) {
				continue
			}
			return nil, err
		}
		entries, err := loadOne(fsys, c.path, l.Workspace, home, nil)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Command != "startup" {
				continue
			}
			if HasFlag(e.Args, "nosystem_rc") {
				skipSystem = true
			}
			if HasFlag(e.Args, "noworkspace_rc") {
				skipWorkspace = true
			}
			if HasFlag(e.Args, "nohome_rc") {
				skipHome = true
			}
		}
	}

	// Real pass: honor the skip flags.
	var out []Entry
	for _, c := range candidates {
		switch {
		case skipSystem && systemRC != "" && c.path == systemRC:
			continue
		case skipWorkspace && l.Workspace != "" && c.path == filepath.Join(l.Workspace, ".bazelrc"):
			continue
		case skipHome && home != "" && c.path == filepath.Join(home, ".bazelrc"):
			continue
		}
		if _, err := fsys.Stat(c.path); err != nil {
			if !c.required && isNotExist(err) {
				continue
			}
			return nil, err
		}
		entries, err := loadOne(fsys, c.path, l.Workspace, home, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

// loadOne parses path and recursively expands import/try-import directives.
// visited tracks the current include chain for cycle detection.
func loadOne(fsys FS, path, workspace, home string, visited []string) ([]Entry, error) {
	// Cycle check.
	abs, _ := filepath.Abs(path)
	if slices.Contains(visited, abs) {
		return nil, fmt.Errorf("import cycle detected: %s", cycleString(append(visited, abs)))
	}
	visited = append(visited, abs)

	data, err := fsys.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := Parse(path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	var out []Entry
	baseDir := filepath.Dir(path)
	for _, e := range raw {
		switch e.Command {
		case "import", "try-import":
			if len(e.Args) == 0 {
				return nil, fmt.Errorf("%s:%d: %s requires a path", e.Source, e.Line, e.Command)
			}
			target, rerr := Resolve(e.Args[0], baseDir, workspace, home)
			if rerr != nil {
				return nil, fmt.Errorf("%s:%d: %w", e.Source, e.Line, rerr)
			}
			sub, serr := loadOne(fsys, target, workspace, home, visited)
			if serr != nil {
				if e.Command == "try-import" && isNotExist(serr) {
					continue
				}
				return nil, serr
			}
			out = append(out, sub...)
		default:
			out = append(out, e)
		}
	}
	return out, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist)
}

func cycleString(chain []string) string {
	return strings.Join(chain, " → ")
}
