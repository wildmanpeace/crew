// Package gitx wraps the git operations crew needs.
//
// Every call is exec'd directly with an explicit repository directory; no
// command is ever assembled into a shell string, and no operation depends on
// the caller's current working directory.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a git repository or worktree on disk.
type Repo struct{ Dir string }

// New returns a Repo rooted at dir.
func New(dir string) Repo { return Repo{Dir: dir} }

// Run executes a git command in the repo and returns its stdout.
func (r Repo) Run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
	// Pin an identity so operations work in repos with no user configured.
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=crew", "GIT_AUTHOR_EMAIL=crew@localhost",
		"GIT_COMMITTER_NAME=crew", "GIT_COMMITTER_EMAIL=crew@localhost")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), fmt.Errorf("git %s: %s",
				strings.Join(args, " "), strings.TrimSpace(errb.String()))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

func (r Repo) trimmed(args ...string) (string, error) {
	out, err := r.Run(args...)
	return strings.TrimSpace(out), err
}

// RevParse resolves a revision to a full sha.
func (r Repo) RevParse(rev string) (string, error) { return r.trimmed("rev-parse", rev) }

// MergeBase returns the best common ancestor of two revisions.
func (r Repo) MergeBase(a, b string) (string, error) { return r.trimmed("merge-base", a, b) }

// Change is one file's status between two revisions.
type Change struct {
	// Status is git's name-status letter: A added, M modified, D deleted,
	// R renamed, C copied, T type-changed.
	Status string
	Path   string
}

// Added reports whether the file did not exist at the base revision. Such a
// file has no base version to restore, so reverting it means deleting it.
func (c Change) Added() bool { return c.Status == "A" }

// ChangedFiles lists what changed between two revisions.
func (r Repo) ChangedFiles(from, to string) ([]Change, error) {
	// -M is deliberately omitted: rename detection would report a single R
	// entry, and the negative control needs the added path treated as added.
	out, err := r.Run("diff", "--name-status", "--no-renames", from+".."+to)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			continue
		}
		changes = append(changes, Change{
			Status: strings.TrimSpace(fields[0])[:1],
			Path:   strings.TrimSpace(fields[1]),
		})
	}
	return changes, nil
}

// AddWorktreeDetached checks out a revision into a new detached worktree.
func (r Repo) AddWorktreeDetached(path, rev string) error {
	_, err := r.Run("worktree", "add", "-q", "--detach", path, rev)
	return err
}

// AddWorktreeBranch creates a new branch at base and checks it out into a new
// worktree.
func (r Repo) AddWorktreeBranch(path, branch, base string) error {
	_, err := r.Run("worktree", "add", "-q", "-b", branch, path, base)
	return err
}

// RemoveWorktree deletes a worktree, discarding any changes in it.
func (r Repo) RemoveWorktree(path string) error {
	_, err := r.Run("worktree", "remove", "--force", path)
	return err
}

// PruneWorktrees drops administrative entries for worktrees whose
// directories have gone away.
func (r Repo) PruneWorktrees() error {
	_, err := r.Run("worktree", "prune")
	return err
}

// ListWorktrees returns the worktree paths registered with the repo.
func (r Repo) ListWorktrees() ([]string, error) {
	out, err := r.Run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// RestorePaths replaces the given paths with their content at rev.
func (r Repo) RestorePaths(rev string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"checkout", rev, "--"}, paths...)
	_, err := r.Run(args...)
	return err
}

// BranchExists reports whether a local branch is present.
func (r Repo) BranchExists(name string) (bool, error) {
	_, err := r.Run("show-ref", "--verify", "--quiet", "refs/heads/"+name)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// CurrentBranch returns the checked-out branch name.
func (r Repo) CurrentBranch() (string, error) {
	return r.trimmed("rev-parse", "--abbrev-ref", "HEAD")
}

// IsClean reports whether the working tree has no uncommitted changes.
func (r Repo) IsClean() (bool, error) { return r.IsCleanExcluding() }

// IsCleanExcluding reports whether the working tree is clean, ignoring the
// given paths.
//
// crew keeps its worktrees and scratch space under .crew inside the
// repository, so that directory would otherwise make a perfectly clean main
// look dirty and block landing. Excluding it here means the check does not
// depend on the project remembering to gitignore crew's own files.
func (r Repo) IsCleanExcluding(excludes ...string) (bool, error) {
	args := []string{"status", "--porcelain", "--", "."}
	for _, e := range excludes {
		args = append(args, ":(exclude)"+e)
	}
	out, err := r.Run(args...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// Diff returns the diff between rev and the working tree, excluding paths
// matching the given globs.
func (r Repo) Diff(rev string, excludeGlobs ...string) (string, error) {
	args := []string{"diff", rev, "--", "."}
	for _, g := range excludeGlobs {
		args = append(args, ":(exclude)"+g)
	}
	return r.Run(args...)
}

// CanFastForward reports whether target can be fast-forwarded to rev, that is
// whether target is an ancestor of rev.
func (r Repo) CanFastForward(target, rev string) (bool, error) {
	_, err := r.Run("merge-base", "--is-ancestor", target, rev)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// DeleteBranch removes a local branch.
func (r Repo) DeleteBranch(name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := r.Run("branch", flag, name)
	return err
}
