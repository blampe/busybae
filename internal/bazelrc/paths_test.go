package bazelrc

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestExtractCacheDirs(t *testing.T) {
	entries := []Entry{
		{Command: "build", Args: []string{"--repository_cache=%workspace%/cache/repo"}, Source: "/ws/.bazelrc"},
		{Command: "build", Args: []string{"--disk_cache=~/bazel-disk"}, Source: "/ws/.bazelrc"},
		{Command: "startup", Args: []string{"--output_user_root=/tmp/our"}, Source: "/ws/.bazelrc"},
		// Override: last wins.
		{Command: "common", Args: []string{"--disk_cache=/absolute/override"}, Source: "/etc/bazel.bazelrc"},
	}
	got, err := ExtractCacheDirs(entries, "/ws", "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := CacheDirs{
		RepositoryCache: "/ws/cache/repo",
		DiskCache:       "/absolute/override",
		OutputUserRoot:  "/tmp/our",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestExtractCacheDirs_RepoContentsCache(t *testing.T) {
	entries := []Entry{
		{Command: "common", Args: []string{"--repo_contents_cache=/rcc"}, Source: "/ws/.bazelrc"},
	}
	got, err := ExtractCacheDirs(entries, "/ws", "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoContentsCache != "/rcc" {
		t.Fatalf("want /rcc, got %q", got.RepoContentsCache)
	}
}

func TestExtractCacheDirs_Empty(t *testing.T) {
	got, err := ExtractCacheDirs(nil, "/ws", "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != (CacheDirs{}) {
		t.Fatalf("expected zero, got %+v", got)
	}
}

func TestSweepableDirs(t *testing.T) {
	c := CacheDirs{
		RepositoryCache:   "/a",
		DiskCache:         "",
		RepoContentsCache: "/c",
		OutputUserRoot:    "/managed",
	}
	// OutputUserRoot must NOT appear.
	got := c.SweepableDirs()
	want := []string{"/a", "/c"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}
