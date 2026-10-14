package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// A worktree inherits its repo's ".grepaiignore" with "!*". A wildcard in
// config ignore must still drop the whole folder, including future ones.
func TestHardExclude_ConfigWildcardBeatsNestedReinclude(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "repo/main.go", "package main\n")
	writeTestFile(t, root, "repo/.grepaiignore", "!*\nvendor\n")
	writeTestFile(t, root, "repo-worktree-3/.grepaiignore", "!*\nvendor\n")
	writeTestFile(t, root, "repo-worktree-3/src/a.go", "package src\n")

	m, err := NewIgnoreMatcher(root, []string{".git", "*-worktree-*"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"repo-worktree-3", "repo-worktree-3/src", "repo-worktree-3/src/a.go", "repo-worktree-9/x.go"} {
		if !m.ShouldIgnore(p) {
			t.Errorf("ShouldIgnore(%q) = false, want true", p)
		}
	}
	if !m.ShouldSkipDir("repo-worktree-3") {
		t.Error("ShouldSkipDir(repo-worktree-3) = false, want true")
	}
	if m.ShouldIgnore("repo/main.go") {
		t.Error("repo/main.go ignored, want indexed")
	}

	scanner := NewScanner(root, m)
	files, _, err := scanner.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.ToSlash(f.Path) != "repo/main.go" {
			t.Errorf("scanned %q, want only repo/main.go", f.Path)
		}
	}
	if len(files) != 1 {
		t.Errorf("scanned %d files, want 1", len(files))
	}
}

// The Exclusions panel writes the external gitignore; its folder patterns
// are hard too.
func TestHardExclude_ExternalGitignoreFolderBeatsNestedReinclude(t *testing.T) {
	root := t.TempDir()
	ext := filepath.Join(t.TempDir(), "global.gitignore")
	if err := os.WriteFile(ext, []byte("*-worktree-*/\n*.csv\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "app-worktree-1/.grepaiignore", "!*\n")
	writeTestFile(t, root, "app-worktree-1/b.go", "package b\n")
	writeTestFile(t, root, "app/.grepaiignore", "!*\n")
	writeTestFile(t, root, "app/data.csv", "a,b\n")
	writeTestFile(t, root, "app/c.go", "package c\n")

	m, err := NewIgnoreMatcher(root, nil, ext)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"app-worktree-1":      true,
		"app-worktree-1/b.go": true,
		"app/data.csv":        true, // nested "!*" no longer re-includes global file patterns
		"app/c.go":            false,
	}
	for p, want := range cases {
		if got := m.ShouldIgnore(p); got != want {
			t.Errorf("ShouldIgnore(%q) = %v, want %v", p, got, want)
		}
	}
	if !m.ShouldSkipDir("app-worktree-1") {
		t.Error("ShouldSkipDir(app-worktree-1) = false, want true")
	}
}

// A folder the root .grepaiignore excludes stays excluded even when the
// folder carries its own "!*" file.
func TestHardExclude_RootGrepaiignoreFolderBeatsNestedReinclude(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".grepaiignore", "/*-worktree-*/\n")
	writeTestFile(t, root, "x-worktree-2/.grepaiignore", "!*\n")
	writeTestFile(t, root, "x-worktree-2/deep/d.go", "package deep\n")
	writeTestFile(t, root, "x/.grepaiignore", "!*\n")
	writeTestFile(t, root, "x/e.go", "package x\n")

	m, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.ShouldSkipDir("x-worktree-2") {
		t.Error("ShouldSkipDir(x-worktree-2) = false, want true")
	}
	if !m.ShouldIgnore("x-worktree-2/deep/d.go") {
		t.Error("x-worktree-2/deep/d.go indexed, want ignored")
	}
	if m.ShouldIgnore("x/e.go") {
		t.Error("x/e.go ignored, want indexed")
	}
}

// Upstream contract kept: the root .grepaiignore may still re-include what
// config ignore excludes, and nested files may still override .gitignore.
func TestHardExclude_RootNegationAndGitignoreOverrideKept(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".grepaiignore", "!vendor/keep.go\n")
	writeTestFile(t, root, "vendor/keep.go", "package vendor\n")
	writeTestFile(t, root, "vendor/drop.go", "package vendor\n")
	writeTestFile(t, root, "svc/.gitignore", "gen/\n")
	writeTestFile(t, root, "svc/.grepaiignore", "!gen/\n")
	writeTestFile(t, root, "svc/gen/g.go", "package gen\n")

	m, err := NewIgnoreMatcher(root, []string{"vendor"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"vendor/keep.go": false,
		"vendor/drop.go": true,
		"svc/gen/g.go":   false,
	}
	for p, want := range cases {
		if got := m.ShouldIgnore(p); got != want {
			t.Errorf("ShouldIgnore(%q) = %v, want %v", p, got, want)
		}
	}
	if m.ShouldSkipDir("vendor") {
		t.Error("ShouldSkipDir(vendor) = true, want false (root re-includes a file inside)")
	}
}
