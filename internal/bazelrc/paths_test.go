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
	// --repository_cache implies both subtrees.
	want := CacheDirs{
		RepositoryCache:   "/ws/cache/repo",
		DownloadCache:     "/ws/cache/repo/content_addressable",
		RepoContentsCache: "/ws/cache/repo/contents",
		DiskCache:         "/absolute/override",
		OutputUserRoot:    "/tmp/our",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}

func TestExtractCacheDirs_RepoContentsCacheOverride(t *testing.T) {
	// --repo_contents_cache overrides the default <repo_cache>/contents.
	entries := []Entry{
		{Command: "build", Args: []string{"--repository_cache=/rc"}, Source: "/ws/.bazelrc"},
		{Command: "common", Args: []string{"--repo_contents_cache=/rcc"}, Source: "/ws/.bazelrc"},
	}
	got, err := ExtractCacheDirs(entries, "/ws", "/home/u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownloadCache != "/rc/content_addressable" {
		t.Fatalf("DownloadCache = %q", got.DownloadCache)
	}
	if got.RepoContentsCache != "/rcc" {
		t.Fatalf("RepoContentsCache = %q", got.RepoContentsCache)
	}
}

func TestExtractCacheDirs_RepoContentsCacheAlone(t *testing.T) {
	// Without --repository_cache, only the explicit --repo_contents_cache
	// is populated; no download cache is implied.
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
	if got.DownloadCache != "" {
		t.Fatalf("DownloadCache should be empty, got %q", got.DownloadCache)
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
		DownloadCache:     "/a/content_addressable",
		DiskCache:         "",
		RepoContentsCache: "/c",
		OutputUserRoot:    "/managed",
	}
	// OutputUserRoot must NOT appear; the raw RepositoryCache root must
	// NOT appear either (it isn't itself a cache — it's a container for
	// the two derived subtrees).
	got := c.SweepableDirs()
	want := []string{"/a/content_addressable", "/c"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("mismatch:\n%s", diff)
	}
}
