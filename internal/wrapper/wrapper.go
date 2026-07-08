// Package wrapper implements the `busybae install-wrapper` subcommand.
//
// It renders a self-installing bazelisk wrapper script into
// <workspace>/tools/bazel. The script pins the busybae release version
// and per-platform SHA256 checksums so a fresh clone materializes the
// binary lazily, verifiably, and without any bootstrap step.
//
// The version is expected to be stamped in by Bazel (see
// internal/version); SHAs are fetched from the release's SHA256SUMS
// file at command time, so the current-tag SHAs are always used without
// requiring an additional stamp channel.
package wrapper

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

//go:embed bazel.tmpl
var tmplText string

// Platforms is the set of GOOS_GOARCH pairs we publish binaries for.
var Platforms = []string{
	"darwin_arm64",
	"darwin_amd64",
	"linux_arm64",
	"linux_amd64",
}

// DefaultReleaseURL is the base for release artifacts.
const DefaultReleaseURL = "https://github.com/brycelampe/busybae/releases/download"

// DefaultOutputPath is where the generated wrapper lands under the
// consumer's workspace root.
const DefaultOutputPath = "tools/bazel"

type platformSHA struct {
	Key string
	SHA string
}

type templateData struct {
	Version    string
	ReleaseURL string
	Platforms  []platformSHA
}

// Options controls Install.
type Options struct {
	// Version is the release tag this wrapper should pin. Required.
	Version string
	// ReleaseURL overrides DefaultReleaseURL.
	ReleaseURL string
	// SHAFile, if set, is a local path to a SHA256SUMS file to read
	// instead of fetching from the release URL. Useful for offline
	// generation and for release-pipeline integration.
	SHAFile string
	// Out is the wrapper output path. Defaults to DefaultOutputPath.
	Out string
	// DryRun prints the rendered wrapper to stdout instead of writing.
	DryRun bool
	// Check exits with an error (without writing) when the wrapper on
	// disk differs from what would be generated. Used in CI to catch
	// wrapper drift when the version bumps.
	Check bool
	// HTTPClient overrides the http.Client used to fetch SHAs.
	HTTPClient *http.Client
}

// Install renders and (unless DryRun/Check) writes the wrapper.
func Install(opts Options) error {
	if opts.Version == "" || opts.Version == "dev" {
		return errors.New("install-wrapper requires a stamped release version; " +
			"build with `bazel build --stamp //:busybae` or pass --version explicitly")
	}
	if opts.ReleaseURL == "" {
		opts.ReleaseURL = DefaultReleaseURL
	}
	if opts.Out == "" {
		opts.Out = DefaultOutputPath
	}

	shas, err := fetchSHAs(opts)
	if err != nil {
		return err
	}

	data := templateData{
		Version:    opts.Version,
		ReleaseURL: opts.ReleaseURL,
		Platforms:  make([]platformSHA, 0, len(Platforms)),
	}
	for _, p := range Platforms {
		data.Platforms = append(data.Platforms, platformSHA{Key: p, SHA: shas[p]})
	}

	rendered, err := render(data)
	if err != nil {
		return err
	}

	if opts.Check {
		existing, err := os.ReadFile(opts.Out)
		if err != nil {
			return fmt.Errorf("read %s: %w", opts.Out, err)
		}
		if string(existing) != rendered {
			return fmt.Errorf("wrapper at %s is out of date; run `busybae install-wrapper` to update", opts.Out)
		}
		return nil
	}

	if opts.DryRun {
		_, err := os.Stdout.WriteString(rendered)
		return err
	}

	if err := os.MkdirAll(filepath.Dir(opts.Out), 0o755); err != nil {
		return err
	}
	// Write to a sibling and rename for atomicity.
	tmp := opts.Out + ".tmp"
	if err := os.WriteFile(tmp, []byte(rendered), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, opts.Out)
}

func render(data templateData) (string, error) {
	t, err := template.New("wrapper").Parse(tmplText)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func fetchSHAs(opts Options) (map[string]string, error) {
	if opts.SHAFile != "" {
		f, err := os.Open(opts.SHAFile)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", opts.SHAFile, err)
		}
		defer f.Close()
		return parseSHAs(f)
	}
	url := fmt.Sprintf("%s/v%s/SHA256SUMS", opts.ReleaseURL, opts.Version)
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return parseSHAs(resp.Body)
}

// parseSHAs reads a `sha256sum`-format file and picks out our per-platform
// tarball entries. Extra entries (a checksum for the SHA256SUMS file
// itself, source tarballs, etc.) are ignored.
func parseSHAs(r io.Reader) (map[string]string, error) {
	out := make(map[string]string, len(Platforms))
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := fields[0]
		// sha256sum output uses "*" as a marker for binary mode.
		name := filepath.Base(strings.TrimPrefix(fields[1], "*"))
		for _, p := range Platforms {
			if name == "busybae-"+p+".tar.gz" {
				out[p] = hash
				break
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	missing := make([]string, 0, len(Platforms))
	for _, p := range Platforms {
		if out[p] == "" {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("SHA256SUMS is missing entries for: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
