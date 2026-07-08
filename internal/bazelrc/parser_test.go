package bazelrc

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		err  bool
	}{
		{"empty", "", nil, false},
		{"single", "build", []string{"build"}, false},
		{"multi", "build --foo --bar=baz", []string{"build", "--foo", "--bar=baz"}, false},
		{"whitespace_collapsed", "build   --foo\t\t--bar", []string{"build", "--foo", "--bar"}, false},
		{"double_quoted", `build --foo="hello world"`, []string{"build", "--foo=hello world"}, false},
		{"single_quoted", `build --foo='hello world'`, []string{"build", "--foo=hello world"}, false},
		{"quoted_mixed", `build "--foo=a b" --bar='c d'`, []string{"build", "--foo=a b", "--bar=c d"}, false},
		{"empty_quoted", `build --foo=""`, []string{"build", "--foo="}, false},
		{"escaped_space", `build --foo=a\ b`, []string{"build", "--foo=a b"}, false},
		{"escaped_quote", `build --foo=\"quoted\"`, []string{"build", `--foo="quoted"`}, false},
		{"unterminated_double", `build "hello`, nil, true},
		{"unterminated_single", `build 'hello`, nil, true},
		{"double_inside_single", `build '"nested"'`, []string{"build", `"nested"`}, false},
		{"single_inside_double", `build "it's"`, []string{"build", "it's"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tokenize(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in       string
		cmd, cfg string
	}{
		{"build", "build", ""},
		{"build:linux", "build", "linux"},
		{"common", "common", ""},
		{":linux", "", "linux"}, // syntactically weird but consistent
		{"build:", "build", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			cmd, cfg := splitCommand(tc.in)
			if cmd != tc.cmd || cfg != tc.cfg {
				t.Fatalf("splitCommand(%q) = (%q, %q), want (%q, %q)", tc.in, cmd, cfg, tc.cmd, tc.cfg)
			}
		})
	}
}

func TestStripComment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"# leading", ""},
		{"   # indented", "   "},
		{"build --foo # trailing", "build --foo "},
		{"build --foo=bar#baz", "build --foo=bar#baz"}, // not a comment (no leading ws)
		{`build "# in quote"`, `build "# in quote"`},
		{`build '# in quote'`, `build '# in quote'`},
		{`build \# escaped`, `build \# escaped`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := stripComment(tc.in)
			if got != tc.want {
				t.Fatalf("stripComment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	src := `
# a leading comment
build --foo=1
build:linux --bar=2

# blank lines and comments are dropped
startup --output_user_root=/tmp/x

build --long \
  --continued \
  --line

import %workspace%/other.bazelrc
try-import ~/opt.bazelrc

common --verbose_failures  # trailing comment
`
	got, err := Parse("test.bazelrc", strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Command: "build", Args: []string{"--foo=1"}, Source: "test.bazelrc", Line: 3},
		{Command: "build", Config: "linux", Args: []string{"--bar=2"}, Source: "test.bazelrc", Line: 4},
		{Command: "startup", Args: []string{"--output_user_root=/tmp/x"}, Source: "test.bazelrc", Line: 7},
		{Command: "build", Args: []string{"--long", "--continued", "--line"}, Source: "test.bazelrc", Line: 9},
		{Command: "import", Args: []string{"%workspace%/other.bazelrc"}, Source: "test.bazelrc", Line: 13},
		{Command: "try-import", Args: []string{"~/opt.bazelrc"}, Source: "test.bazelrc", Line: 14},
		{Command: "common", Args: []string{"--verbose_failures"}, Source: "test.bazelrc", Line: 16},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("Parse mismatch (-want +got):\n%s", diff)
	}
}

func TestParseEmpty(t *testing.T) {
	entries, err := Parse("empty.bazelrc", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(entries))
	}
}

func TestParseContinuationAtEOF(t *testing.T) {
	// A trailing backslash on the last line — the accumulated buffer
	// should still flush.
	got, err := Parse("t.bazelrc", strings.NewReader("build --foo \\\n --bar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d: %#v", len(got), got)
	}
	if diff := cmp.Diff([]string{"--foo", "--bar"}, got[0].Args); diff != "" {
		t.Fatalf("args mismatch: %s", diff)
	}
}

func TestExtractFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		flag string
		want string
		ok   bool
	}{
		{"equals", []string{"--repository_cache=/tmp/a"}, "repository_cache", "/tmp/a", true},
		{"space", []string{"--repository_cache", "/tmp/a"}, "repository_cache", "/tmp/a", true},
		{"last_wins", []string{"--x=1", "--x=2", "--x=3"}, "x", "3", true},
		{"missing", []string{"--foo=1"}, "bar", "", false},
		{"prefix_only_no_value", []string{"--x"}, "x", "", false},
		{"prefix_similar", []string{"--repository_cachex=1"}, "repository_cache", "", false},
		{"mixed_forms", []string{"--x=1", "--x", "2"}, "x", "2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := ExtractFlag(tc.args, tc.flag)
			if v != tc.want || ok != tc.ok {
				t.Fatalf("ExtractFlag(%v, %q) = (%q, %v), want (%q, %v)",
					tc.args, tc.flag, v, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHasFlag(t *testing.T) {
	if !HasFlag([]string{"--nosystem_rc"}, "nosystem_rc") {
		t.Fatal("expected true")
	}
	if HasFlag([]string{"--nosystem_rc=false"}, "nosystem_rc") {
		t.Fatal("--flag=value should not match bare --flag")
	}
	if HasFlag([]string{"--other"}, "nosystem_rc") {
		t.Fatal("expected false")
	}
}
