package cli

import (
	"slices"
	"testing"
)

// A worker's context must be its brief and the code, nothing else. The
// project's own memory files live inside every worktree, so excluding them is
// what keeps a first-mate role document — or any other instruction written for
// an interactive session — from reaching a one-shot worker.
func TestWorkerClaudeMdExcludesCoversWorktreesAndTheCaptainsGlobal(t *testing.T) {
	got := WorkerClaudeMdExcludes("/proj", "/home/u")

	for _, want := range []string{
		"/home/u/.claude/CLAUDE.md",
		"/proj/.crew/worktrees/**",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("excludes %v, missing %q", got, want)
		}
	}
}

// The home directory is not always resolvable, and a worker that inherits the
// captain's conventions is a smaller problem than one that inherits the whole
// role document, so the worktree pattern must not depend on finding it.
func TestWorkerClaudeMdExcludesWithoutAHomeDirectory(t *testing.T) {
	got := WorkerClaudeMdExcludes("/proj", "")

	if !slices.Equal(got, []string{"/proj/.crew/worktrees/**"}) {
		t.Errorf("excludes = %v, want only the worktree pattern", got)
	}
}
