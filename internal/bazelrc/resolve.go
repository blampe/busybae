package bazelrc

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resolve expands a path as it would appear in a .bazelrc file:
//
//   - Leading `~` (or `~/`) is expanded to home.
//   - `%workspace%` is replaced with workspace.
//   - The result is joined against baseDir when relative.
//
// baseDir is typically the directory containing the .bazelrc file that
// declared the path. workspace and home may be empty; in that case, using the
// corresponding token is an error.
func Resolve(raw, baseDir, workspace, home string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(raw, "%workspace%") {
		if workspace == "" {
			return "", fmt.Errorf("%%workspace%% used but no workspace known")
		}
		rest := strings.TrimPrefix(raw, "%workspace%")
		rest = strings.TrimPrefix(rest, "/")
		raw = filepath.Join(workspace, rest)
	} else if raw == "~" {
		if home == "" {
			return "", fmt.Errorf("~ used but no home known")
		}
		raw = home
	} else if strings.HasPrefix(raw, "~/") {
		if home == "" {
			return "", fmt.Errorf("~ used but no home known")
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}
	if !filepath.IsAbs(raw) {
		if baseDir == "" {
			return "", fmt.Errorf("relative path %q with no base directory", raw)
		}
		raw = filepath.Join(baseDir, raw)
	}
	return filepath.Clean(raw), nil
}
