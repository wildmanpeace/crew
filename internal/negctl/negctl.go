// Package negctl performs crew's negative control.
//
// The question it answers is whether a verifier-authored test actually
// discriminates: does it fail when the implementation is taken away, and pass
// when it is present? Only that transition counts as evidence.
//
// The control runs entirely inside crew, in a throwaway worktree no worker and
// neither dispatcher can reach. The verifier's tool surface does not grow at
// all to support it.
//
// Phase 0 measured a hard limit worth stating plainly: this discriminates on
// changes to pre-existing API surface, and structurally cannot on new API
// surface. Removing the implementation of a new symbol makes any test
// referencing it fail to compile, which is a build failure rather than an
// assertion failure. Those runs are classified and downgraded to judgment
// rather than being counted as evidence.
package negctl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wildmanpeace/crew/internal/gitx"
)

// Classification is what crew concluded about a negative-control run.
type Classification string

const (
	// Discriminating is the only outcome that counts as evidence.
	Discriminating Classification = "discriminating"
	// PassesAtMergeBase means the test passes without the implementation, so
	// it proves nothing about it.
	PassesAtMergeBase Classification = "passes_at_merge_base"
	// BuildFailed means the reverted tree would not compile, so no assertion
	// was ever evaluated.
	BuildFailed Classification = "build_failed"
	// FailsAtHead means the test does not even pass with the implementation
	// present, so the control cannot be interpreted.
	FailsAtHead Classification = "fails_at_head"
)

// Params describes one negative-control run.
type Params struct {
	Repo       gitx.Repo
	MainBranch string
	Branch     string

	// TestFile is the verifier-authored test, relative to the worktree root.
	TestFile string

	// TestArgv is the project's configured test command, already scoped to
	// the package under test.
	TestArgv []string

	BuildFailureMarkers []string
	ScratchDir          string
}

// Result is crew's own finding. It overwrites whatever the verifier claimed.
type Result struct {
	Classification   Classification
	FailsAtMergeBase bool
	PassesAtHead     bool
	Met              bool
	Reason           string

	// Raw output from both phases is kept so an over-broad build-failure
	// marker is tunable rather than silently discarding good evidence.
	HeadOutput      string
	MergeBaseOutput string
	RevertedFiles   []string
	DeletedFiles    []string
}

// Run performs the control and returns crew's finding.
func Run(p Params) (Result, error) {
	res := Result{}

	head, err := p.Repo.RevParse(p.Branch)
	if err != nil {
		return res, fmt.Errorf("resolve branch head: %w", err)
	}
	mergeBase, err := p.Repo.MergeBase(p.MainBranch, p.Branch)
	if err != nil {
		return res, fmt.Errorf("resolve merge base: %w", err)
	}

	if err := os.MkdirAll(p.ScratchDir, 0o755); err != nil {
		return res, fmt.Errorf("create scratch dir: %w", err)
	}
	wt, err := os.MkdirTemp(p.ScratchDir, "negctl-*")
	if err != nil {
		return res, fmt.Errorf("create scratch worktree path: %w", err)
	}
	// git worktree add requires the path not to exist yet.
	os.Remove(wt)

	// The worktree is built at branch head so the test keeps its package
	// structure, imports, and surrounding module, and therefore compiles.
	if err := p.Repo.AddWorktreeDetached(wt, head); err != nil {
		return res, fmt.Errorf("create throwaway worktree: %w", err)
	}
	defer func() {
		p.Repo.RemoveWorktree(wt)
		p.Repo.PruneWorktrees()
	}()

	// Phase one: the test must pass with the implementation present.
	headCode, headOut := runTest(wt, p.TestArgv)
	res.HeadOutput = headOut
	res.PassesAtHead = headCode == 0
	if !res.PassesAtHead {
		res.Classification = FailsAtHead
		res.Reason = "the test does not pass at branch head, so the control cannot be interpreted"
		return res, nil
	}

	// Phase two: take the implementation away, keeping the test in place.
	reverted, deleted, err := revertImplementation(p, wt, mergeBase, head)
	if err != nil {
		return res, err
	}
	res.RevertedFiles = reverted
	res.DeletedFiles = deleted

	mbCode, mbOut := runTest(wt, p.TestArgv)
	res.MergeBaseOutput = mbOut
	res.FailsAtMergeBase = mbCode != 0

	switch {
	case !res.FailsAtMergeBase:
		res.Classification = PassesAtMergeBase
		res.Reason = "the test passes without the implementation, so it does not discriminate"
	case matchesAnyMarker(mbOut, p.BuildFailureMarkers):
		// Expected whenever the criterion covers new API surface.
		res.Classification = BuildFailed
		res.Reason = "the reverted tree does not build, so no assertion was evaluated"
	default:
		res.Classification = Discriminating
		res.Met = true
		res.Reason = "the test fails without the implementation and passes with it"
	}
	return res, nil
}

// revertImplementation restores every changed file except the verifier's test
// to its merge-base content.
//
// Files the branch added have no merge-base version, so restoring them is not
// possible: they are deleted instead. Without that, a purely additive feature
// is never actually reverted, the test still passes, and the control silently
// proves nothing.
func revertImplementation(p Params, wt, mergeBase, head string) (reverted, deleted []string, err error) {
	changes, err := p.Repo.ChangedFiles(mergeBase, head)
	if err != nil {
		return nil, nil, fmt.Errorf("list changed files: %w", err)
	}
	keep := filepath.Clean(p.TestFile)
	scratch := gitx.New(wt)

	var restore []string
	for _, c := range changes {
		if filepath.Clean(c.Path) == keep {
			continue
		}
		if c.Added() {
			if err := os.Remove(filepath.Join(wt, c.Path)); err != nil && !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("delete added file %s: %w", c.Path, err)
			}
			deleted = append(deleted, c.Path)
			continue
		}
		restore = append(restore, c.Path)
	}
	if err := scratch.RestorePaths(mergeBase, restore); err != nil {
		return nil, nil, fmt.Errorf("restore merge-base files: %w", err)
	}
	return restore, deleted, nil
}

func runTest(dir string, argv []string) (int, string) {
	if len(argv) == 0 {
		return -1, "no test command configured"
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return -1, fmt.Sprintf("%v", err)
		}
	}
	return code, string(out)
}

func matchesAnyMarker(output string, markers []string) bool {
	for _, m := range markers {
		if m != "" && strings.Contains(output, m) {
			return true
		}
	}
	return false
}

// Downgrades reports whether this result should fall back to judgment rather
// than count as mechanical evidence.
func (r Result) Downgrades() bool { return r.Classification != Discriminating }
