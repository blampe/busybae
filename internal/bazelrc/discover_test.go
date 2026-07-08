package bazelrc

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// mapFSAdapter adapts fstest.MapFS to the FS interface. fstest.MapFS is
// rooted, so we accept absolute-looking paths and trim the leading slash.
type mapFSAdapter struct {
	fs fstest.MapFS
}

var _ FS = mapFSAdapter{}

func (m mapFSAdapter) ReadFile(path string) ([]byte, error) {
	return m.fs.ReadFile(strings.TrimPrefix(path, "/"))
}

func (m mapFSAdapter) Stat(path string) (fs.FileInfo, error) {
	return m.fs.Stat(strings.TrimPrefix(path, "/"))
}

func newFS(files map[string]string) FS {
	mf := make(fstest.MapFS, len(files))
	for k, v := range files {
		mf[strings.TrimPrefix(k, "/")] = &fstest.MapFile{
			Data:    []byte(v),
			Mode:    0o644,
			ModTime: time.Unix(0, 0),
		}
	}
	return mapFSAdapter{fs: mf}
}

func commands(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Command+" "+strings.Join(e.Args, " "))
	}
	return out
}

func TestLoaderSimpleWorkspace(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-", // disable
		FS: newFS(map[string]string{
			"/ws/.bazelrc": "build --foo=1\ncommon --bar\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build --foo=1", "common --bar"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderImportRelative(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":          "build --outer=1\nimport sub/child.bazelrc\nbuild --after=1\n",
			"/ws/sub/child.bazelrc": "build --inner=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build --outer=1", "build --inner=1", "build --after=1"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderImportWorkspaceToken(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":             "import %workspace%/nested/child.bazelrc\n",
			"/ws/nested/child.bazelrc": "build --nested=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"build --nested=1"}, commands(entries)); diff != "" {
		t.Fatalf("mismatch: %s", diff)
	}
}

func TestLoaderImportHomeToken(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":         "import ~/team.bazelrc\n",
			"/home/u/team.bazelrc": "build --team=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"build --team=1"}, commands(entries)); diff != "" {
		t.Fatalf("mismatch: %s", diff)
	}
}

func TestLoaderImportMissingIsError(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc": "import /nowhere.bazelrc\n",
		}),
	}
	if _, err := l.Load(); err == nil {
		t.Fatal("expected error for missing import")
	}
}

func TestLoaderTryImportMissingIsOK(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc": "build --before=1\ntry-import /nowhere.bazelrc\nbuild --after=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build --before=1", "build --after=1"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderImportCycleDetected(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":  "import %workspace%/a.bazelrc\n",
			"/ws/a.bazelrc": "import %workspace%/b.bazelrc\n",
			"/ws/b.bazelrc": "import %workspace%/a.bazelrc\n",
		}),
	}
	if _, err := l.Load(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestLoaderSkipHomeRC(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":     "startup --nohome_rc\nbuild --workspace=1\n",
			"/home/u/.bazelrc": "build --home=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Home rc is skipped; workspace rc appears (in full).
	want := []string{"startup --nohome_rc", "build --workspace=1"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderSkipWorkspaceRC(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		Home:      "/home/u",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			// User's home rc disables the workspace rc.
			"/home/u/.bazelrc": "startup --noworkspace_rc\nbuild --home=1\n",
			"/ws/.bazelrc":     "build --workspace=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"startup --noworkspace_rc", "build --home=1"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderExtraRCs(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		ExtraRCs:  []string{"/extra/one.bazelrc", "/extra/two.bazelrc"},
		FS: newFS(map[string]string{
			"/ws/.bazelrc":       "build --ws=1\n",
			"/extra/one.bazelrc": "build --one=1\n",
			"/extra/two.bazelrc": "build --two=1\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build --ws=1", "build --one=1", "build --two=1"}
	if diff := cmp.Diff(want, commands(entries), cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestLoaderExtraRCMissingIsError(t *testing.T) {
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		ExtraRCs:  []string{"/extra/missing.bazelrc"},
		FS: newFS(map[string]string{
			"/ws/.bazelrc": "build --ws=1\n",
		}),
	}
	if _, err := l.Load(); err == nil {
		t.Fatal("expected error for missing extra rc")
	}
}

func TestLoaderMissingWorkspaceRCNoError(t *testing.T) {
	// Workspace has no .bazelrc — that's fine.
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		FS:        newFS(map[string]string{}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %v", commands(entries))
	}
}

func TestLoaderLineContinuationAcrossFiles(t *testing.T) {
	// Two separate rc files, each with line continuations — make sure
	// they don't bleed into each other.
	l := &Loader{
		Workspace: "/ws",
		SystemRC:  "-",
		FS: newFS(map[string]string{
			"/ws/.bazelrc":      "build --a=1 \\\n --b=2\nimport %workspace%/child.bazelrc\n",
			"/ws/child.bazelrc": "test --c=3 \\\n --d=4\n",
		}),
	}
	entries, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build --a=1 --b=2", "test --c=3 --d=4"}
	if diff := cmp.Diff(want, commands(entries)); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}
