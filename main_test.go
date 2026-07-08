package main

import (
	"bytes"
	"strings"
	"testing"

	"busybae/internal/gc"
)

func TestPrintSweepSummary(t *testing.T) {
	results := []sweepResult{
		{Kind: "repository_cache", Path: "/tmp/repo", Stats: gc.Stats{
			Scanned: 100, Removed: 42, Bytes: 5 << 20, SkippedHardlinks: 3,
		}},
		{Kind: "disk_cache", Path: "/tmp/disk", Stats: gc.Stats{
			Scanned: 50, Removed: 10, Bytes: 512, Errors: 1,
		}},
	}
	var buf bytes.Buffer
	printSweepSummary(&buf, results, false)
	out := buf.String()

	for _, want := range []string{
		"sweep summary\n",
		"repository_cache",
		"/tmp/repo",
		"removed 42 of 100 entries (5.0 MiB)",
		"skipped 3 hardlinked",
		"disk_cache",
		"removed 10 of 50 entries (512 B)",
		"1 errors",
		"total: removed 52 entries (5.0 MiB)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n---\n%s", want, out)
		}
	}
}

func TestPrintSweepSummaryDryRun(t *testing.T) {
	results := []sweepResult{
		{Kind: "disk_cache", Path: "/tmp/disk", Stats: gc.Stats{
			Scanned: 1, Removed: 1, Bytes: 1 << 30,
		}},
	}
	var buf bytes.Buffer
	printSweepSummary(&buf, results, true)
	out := buf.String()

	if !strings.Contains(out, "dry run") {
		t.Errorf("dry-run header missing:\n%s", out)
	}
	if !strings.Contains(out, "would remove 1 of 1 entries (1.00 GiB)") {
		t.Errorf("dry-run verb/size missing:\n%s", out)
	}
	if strings.Contains(out, "total:") {
		t.Errorf("single-target summary should not print a total line:\n%s", out)
	}
}

func TestPrintSweepSummaryEmpty(t *testing.T) {
	var buf bytes.Buffer
	printSweepSummary(&buf, nil, false)
	if buf.Len() != 0 {
		t.Fatalf("empty results should produce no output, got %q", buf.String())
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.00 GiB"},
		{2 * 1024 * 1024 * 1024 * 1024, "2.00 TiB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := humanBytes(tc.n); got != tc.want {
				t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}
