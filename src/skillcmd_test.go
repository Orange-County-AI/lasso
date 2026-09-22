package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The repo-root SKILL.md must stay a symlink to the embedded asset. Replacing
// it with a copy is the one change that silently reintroduces drift: `lasso
// skill` would keep printing assets/SKILL.md while installers ship the root
// file, and nothing else would notice.
func TestRepoRootSkillIsSymlinkToAsset(t *testing.T) {
	fi, err := os.Lstat("../SKILL.md")
	if os.IsNotExist(err) {
		t.Skip("no repo-root SKILL.md (building outside a checkout)")
	}
	if err != nil {
		t.Fatalf("lstat ../SKILL.md: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("SKILL.md is a regular file; it must be a symlink to src/assets/SKILL.md (go:embed cannot reach above src/)")
	}
	target, err := absReal("../SKILL.md")
	if err != nil {
		t.Fatalf("resolve ../SKILL.md: %v", err)
	}
	want, err := absReal("assets/SKILL.md")
	if err != nil {
		t.Fatalf("resolve assets/SKILL.md: %v", err)
	}
	if target != want {
		t.Fatalf("SKILL.md points at %s, want %s", target, want)
	}
}

func TestSkillHasFrontmatter(t *testing.T) {
	if len(skillMarkdown) < 4 || skillMarkdown[:4] != "---\n" {
		t.Fatal("embedded skill does not start with YAML frontmatter")
	}
}

// absReal resolves a path through symlinks AND to an absolute path — the two
// sides of the comparison are spelled relative to different directories, so
// EvalSymlinks alone compares "../src/assets/SKILL.md" against "assets/SKILL.md".
func absReal(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(r)
}
