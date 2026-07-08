package bazelrc

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		baseDir   string
		workspace string
		home      string
		want      string
		err       bool
	}{
		{
			name: "absolute", raw: "/tmp/x", baseDir: "/wd", want: "/tmp/x",
		},
		{
			name: "workspace_token", raw: "%workspace%/foo", workspace: "/w", baseDir: "/base", want: "/w/foo",
		},
		{
			name: "workspace_token_no_slash", raw: "%workspace%", workspace: "/w", baseDir: "/base", want: "/w",
		},
		{
			name: "home_tilde", raw: "~/x", home: "/h", baseDir: "/base", want: "/h/x",
		},
		{
			name: "bare_tilde", raw: "~", home: "/h", baseDir: "/base", want: "/h",
		},
		{
			name: "relative_to_basedir", raw: "sub/foo", baseDir: "/base", want: "/base/sub/foo",
		},
		{
			name: "workspace_but_none", raw: "%workspace%/x", baseDir: "/base", err: true,
		},
		{
			name: "tilde_but_no_home", raw: "~/x", baseDir: "/base", err: true,
		},
		{
			name: "empty_input", raw: "", baseDir: "/base", err: true,
		},
		{
			name: "cleans", raw: "%workspace%/../x", workspace: "/w/inner", want: "/w/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.raw, tc.baseDir, tc.workspace, tc.home)
			if tc.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}
