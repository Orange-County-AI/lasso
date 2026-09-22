package main

import (
	_ "embed"
	"fmt"
	"os"
)

// `lasso skill` — print lasso's own agent skill to stdout.
//
// The skill is how an agent learns to reach for lasso's MCP tools instead of
// poking at lasso.db and the filesystem, and an agent that can't read this
// repo (which is most of them, on most hosts) had no way to get at it. The
// binary is already on PATH wherever a lasso agent runs, so it carries the
// text itself:
//
//	lasso skill                                    read it
//	lasso skill > ~/.claude/skills/lasso/SKILL.md  install it
//
// The file lives at assets/SKILL.md and the repo-root SKILL.md — the path skill
// installers look for — is a SYMLINK to it, rather than the other way round:
// go:embed cannot reach above the module root (src/) and refuses to follow a
// symlink, so only a real file inside src/ can be embedded. `npx skills add`
// copies with dereference, so it installs the link's target as a real file
// (verified against skills 1.7.0). Two caveats that direction carries: a
// Windows checkout without symlink support gets a one-line text file instead of
// the skill, and a raw.githubusercontent.com fetch of the root path returns the
// link target string, not the content — so link to src/assets/SKILL.md if
// anything ever needs a raw URL.
//
//go:embed assets/SKILL.md
var skillMarkdown string

func cliSkill(args []string) {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Println("usage: lasso skill\n\nPrint lasso's agent skill (SKILL.md) to stdout.")
			return
		default:
			fmt.Fprintf(os.Stderr, "lasso skill: unexpected argument %q\n", a)
			os.Exit(2)
		}
	}
	os.Stdout.WriteString(skillMarkdown)
}
