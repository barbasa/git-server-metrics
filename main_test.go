package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSanitizeRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "repo-name_1", want: "repo-name_1"},
		{in: "repo/name", want: "repo_0x2F_name"},
		{in: "repo_0x2F_name", want: "repo_0x_0x2F_name"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := sanitizeRepoName(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeRepoName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseCountObjectsOutput(t *testing.T) {
	t.Parallel()

	in := `
count: 12
size: 3
in-pack: 12345
packs: 7
size-pack: 456789
`
	got, err := parseCountObjectsOutput(in)
	if err != nil {
		t.Fatalf("parseCountObjectsOutput returned error: %v", err)
	}

	if got.NumberOfLooseObjects != 12 ||
		got.SizeOfLooseObjects != 3 ||
		got.NumberOfPackedObjects != 12345 ||
		got.NumberOfPackFiles != 7 ||
		got.SizeOfPackedObjects != 456789 {
		t.Fatalf("unexpected parsed stats: %+v", got)
	}
}

func TestConfigNormalizeUsesSingleScrapeInterval(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Repos: []RepoSpec{{Path: "/tmp/example.git"}},
	}
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize returned error: %v", err)
	}
	if cfg.ScrapeInterval != 30*time.Second {
		t.Fatalf("ScrapeInterval = %v, want %v", cfg.ScrapeInterval, 30*time.Second)
	}
}

func TestCountPackedRefs(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	contents := "" +
		"# pack-refs with: peeled fully-peeled sorted\n" +
		"1111111111111111111111111111111111111111 refs/heads/main\n" +
		"2222222222222222222222222222222222222222 refs/tags/v1\n" +
		"^3333333333333333333333333333333333333333\n"

	if err := os.WriteFile(filepath.Join(repo, "packed-refs"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write packed-refs: %v", err)
	}

	got, err := countPackedRefs(repo)
	if err != nil {
		t.Fatalf("countPackedRefs returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("countPackedRefs = %d, want 2", got)
	}
}

func TestCountLooseRefs(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	mustMkdirAll(t, filepath.Join(repo, "refs", "heads"))
	mustMkdirAll(t, filepath.Join(repo, "refs", "tags"))
	mustWriteFile(t, filepath.Join(repo, "refs", "heads", "main"), "a")
	mustWriteFile(t, filepath.Join(repo, "refs", "heads", "dev"), "b")
	mustWriteFile(t, filepath.Join(repo, "refs", "tags", "v1"), "c")

	got, err := countLooseRefs(repo)
	if err != nil {
		t.Fatalf("countLooseRefs returned error: %v", err)
	}
	if got != 3 {
		t.Fatalf("countLooseRefs = %d, want 3", got)
	}
}

func TestCollectFSStats(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	objects := filepath.Join(repo, "objects")
	mustMkdirAll(t, filepath.Join(objects, "pack"))
	mustMkdirAll(t, filepath.Join(objects, "aa"))
	mustMkdirAll(t, filepath.Join(objects, "empty"))
	mustWriteFile(t, filepath.Join(objects, "pack", "pack-a.pack"), "x")
	mustWriteFile(t, filepath.Join(objects, "pack", "pack-a.keep"), "x")
	mustWriteFile(t, filepath.Join(objects, "aa", "bb"), "x")

	got, err := collectFSStats(RepoSpec{Path: repo})
	if err != nil {
		t.Fatalf("collectFSStats returned error: %v", err)
	}

	if got.NumberOfDirectories != 4 {
		t.Fatalf("NumberOfDirectories = %v, want 4", got.NumberOfDirectories)
	}
	if got.NumberOfEmptyDirectories != 1 {
		t.Fatalf("NumberOfEmptyDirectories = %v, want 1", got.NumberOfEmptyDirectories)
	}
	if got.NumberOfFiles != 3 {
		t.Fatalf("NumberOfFiles = %v, want 3", got.NumberOfFiles)
	}
	if got.NumberOfKeepFiles != 1 {
		t.Fatalf("NumberOfKeepFiles = %v, want 1", got.NumberOfKeepFiles)
	}
}

func TestCountBitmapFiles(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	packDir := filepath.Join(repo, "objects", "pack")
	mustMkdirAll(t, packDir)

	mustWriteFile(t, filepath.Join(packDir, "pack-a.bitmap"), "bitmap")
	mustWriteFile(t, filepath.Join(packDir, "pack-b.bitmap"), "bitmap")
	mustWriteFile(t, filepath.Join(packDir, "pack-a.pack"), "pack")

	got, err := countBitmapFiles(repo)
	if err != nil {
		t.Fatalf("countBitmapFiles returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("countBitmapFiles = %d, want 2", got)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
