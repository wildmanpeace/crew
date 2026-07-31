// Package dispatch implements the crew-run and crew-check dispatchers.
//
// These are the only shell access a worker is given. The role boundary is
// which binary exists on the worker's PATH: crew-check simply has no commit
// or diff verb to expose, so there is nothing to permit even if a verifier
// tried. This package is shared, but each binary hard-codes its own role at
// compile time — the role is never read from the environment, because a
// worker controls its own environment.
//
// Every command is executed directly. Nothing here ever constructs a shell
// invocation, so shell metacharacters in any argument are inert.
package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
)

// Exit codes. Policy refusals are distinguished from a check command's own
// failure so crew can tell "the check failed" from "the worker tried
// something it is not allowed to do".
const (
	ExitOK     = 0
	ExitPolicy = 2
)

// Env is the dispatcher's scope, supplied by crew when it spawns the worker.
type Env struct {
	Worktree    string
	ProjectRoot string
	TaskID      string
}

// EnvFrom reads the dispatcher environment using the given lookup function.
func EnvFrom(get func(string) string) Env {
	return Env{
		Worktree:    get("CREW_WORKTREE"),
		ProjectRoot: get("CREW_PROJECT_ROOT"),
		TaskID:      get("CREW_TASK_ID"),
	}
}

// RunFunc executes argv with the given working directory and returns the
// child's exit code.
type RunFunc func(argv []string, dir string) (int, error)

// Dispatch validates and performs one dispatcher invocation. It returns the
// exit code the dispatcher should exit with.
func Dispatch(role config.Role, args []string, env Env, cfg *config.Config, run RunFunc) (int, error) {
	if len(args) == 0 {
		return ExitPolicy, fmt.Errorf("usage: %s <%s> [args...]",
			binaryFor(role), strings.Join(config.VerbsForRole(role), "|"))
	}
	verb, rest := args[0], args[1:]

	// The role gate comes first: an unreachable verb must never touch config,
	// the worktree, or git.
	if !config.RoleAllows(role, verb) {
		return ExitPolicy, fmt.Errorf("%s does not expose verb %q (allowed: %s)",
			binaryFor(role), verb, strings.Join(config.VerbsForRole(role), ", "))
	}

	worktree, err := resolveWorktree(env)
	if err != nil {
		return ExitPolicy, err
	}

	switch verb {
	case "commit":
		return runCommit(worktree, rest, run)
	case "diff":
		return runDiff(worktree, cfg, rest, run)
	default:
		argv, err := cfg.Resolve(verb, rest)
		if err != nil {
			return ExitPolicy, err
		}
		return run(argv, worktree)
	}
}

func binaryFor(role config.Role) string {
	if role == config.RoleVerifier {
		return "crew-check"
	}
	return "crew-run"
}

// resolveWorktree checks that the worktree is an absolute, existing directory
// contained within the project's .crew/worktrees tree. Paths are resolved
// through symlinks before comparison so neither a symlinked temp directory
// nor a ".." segment can escape the containment check.
func resolveWorktree(env Env) (string, error) {
	if env.Worktree == "" {
		return "", fmt.Errorf("CREW_WORKTREE is not set")
	}
	if env.ProjectRoot == "" {
		return "", fmt.Errorf("CREW_PROJECT_ROOT is not set")
	}
	if !filepath.IsAbs(env.Worktree) {
		return "", fmt.Errorf("CREW_WORKTREE must be an absolute path, got %q", env.Worktree)
	}

	wt, err := filepath.EvalSymlinks(env.Worktree)
	if err != nil {
		return "", fmt.Errorf("worktree %q is not usable: %w", env.Worktree, err)
	}
	info, err := os.Stat(wt)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("worktree %q is not a directory", env.Worktree)
	}
	root, err := filepath.EvalSymlinks(env.ProjectRoot)
	if err != nil {
		return "", fmt.Errorf("project root %q is not usable: %w", env.ProjectRoot, err)
	}

	base := filepath.Join(root, ".crew", "worktrees")
	rel, err := filepath.Rel(base, wt)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree %q is outside %q", env.Worktree, base)
	}
	return wt, nil
}

// gitBase pins an identity so a commit succeeds in a worktree whose repo has
// no user configured, and never depends on the ambient git config.
func gitBase() []string {
	return []string{"git", "-c", "user.name=crew", "-c", "user.email=crew@localhost"}
}

// runCommit stages everything in the worktree and commits. The message is
// taken as exactly one argv element and passed through untouched; it is never
// interpolated into a larger string.
func runCommit(worktree string, rest []string, run RunFunc) (int, error) {
	if len(rest) != 1 {
		return ExitPolicy, fmt.Errorf("commit takes exactly one argument, the message (got %d)", len(rest))
	}
	msg := rest[0]
	if strings.TrimSpace(msg) == "" {
		return ExitPolicy, fmt.Errorf("commit message must not be empty")
	}
	if code, err := run([]string{"git", "add", "-A"}, worktree); err != nil || code != 0 {
		if err != nil {
			return code, fmt.Errorf("git add: %w", err)
		}
		return code, nil
	}
	argv := append(gitBase(), "commit", "-m", msg)
	return run(argv, worktree)
}

// runDiff shows the worker its own uncommitted changes, excluding
// verifier-authored tests so they never appear in a diff a worker reads.
//
// "git diff HEAD" alone omits untracked files, which would hide newly created
// implementation files — usually the most important part of the change. A
// preceding intent-to-add registers those paths without staging their
// contents, so they appear in the diff.
func runDiff(worktree string, cfg *config.Config, rest []string, run RunFunc) (int, error) {
	if len(rest) != 0 {
		return ExitPolicy, fmt.Errorf("diff takes no arguments (got %d)", len(rest))
	}
	if code, err := run([]string{"git", "add", "-N", "--", "."}, worktree); err != nil {
		return code, fmt.Errorf("git add -N: %w", err)
	}
	argv := []string{"git", "diff", "HEAD", "--", ".",
		":(exclude)*" + cfg.VerifyTestSuffix}
	return run(argv, worktree)
}
