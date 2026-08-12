package negctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/gitx"
)

// These tests reproduce the Phase 0 probe as an executable regression: one
// criterion covering a modification to existing API, one covering brand-new
// API. The first must discriminate; the second structurally cannot.

var markers = []string{"[build failed]", "undefined: ", "cannot find package"}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitx.New(dir).Run(args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

const counterBase = `package counter

import "errors"

var ErrLimit = errors.New("counter: limit exceeded")

type Counter struct{ n int }

func (c *Counter) Add() error {
	c.n++
	return nil
}

func (c *Counter) N() int { return c.n }
`

const counterModified = `package counter

import "errors"

var ErrLimit = errors.New("counter: limit exceeded")

const Max = 10

type Counter struct{ n int }

func (c *Counter) Add() error {
	if c.n >= Max {
		return ErrLimit
	}
	c.n++
	return nil
}

func (c *Counter) N() int { return c.n }
`

const limiterAdded = `package counter

type Limiter struct{ max int }

func NewLimiter(max int) *Limiter { return &Limiter{max: max} }

func (l *Limiter) Allow(n int) bool { return n < l.max }
`

// capTest covers a MODIFICATION and deliberately uses a literal 10 rather
// than the new Max constant, so it still compiles against the old code.
const capTest = `package counter

import "testing"

func TestAddCapsAtTen(t *testing.T) {
	var c Counter
	for i := 0; i < 10; i++ {
		if err := c.Add(); err != nil {
			t.Fatalf("Add #%d returned %v, want nil", i, err)
		}
	}
	if err := c.Add(); err == nil {
		t.Fatal("11th Add returned nil, want limit error")
	}
}
`

// limiterTest covers NEW API surface.
const limiterTest = `package counter

import "testing"

func TestLimiterAllow(t *testing.T) {
	l := NewLimiter(5)
	if !l.Allow(3) {
		t.Fatal("Allow(3) = false, want true")
	}
	if l.Allow(9) {
		t.Fatal("Allow(9) = true, want false")
	}
}
`

// repo builds a project with a merge-base commit and a feature branch that
// both modifies existing API and adds new API.
//
// The verifier's tests are written into the worktree *after* the commit and
// left uncommitted, which is the only state they can be in during a real run:
// crew-check has no commit verb and the hook denies raw git, so a verifier
// physically cannot put its test on the branch. Committing them here made
// every control measure a tree that production never produces.
func repo(t *testing.T, verifyTests map[string]string) (gitx.Repo, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "init", "-q", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module probe\n\ngo 1.26\n")
	write(t, filepath.Join(root, "counter", "counter.go"), counterBase)
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "base")

	mustGit(t, root, "checkout", "-q", "-b", "crew/alpha/attempt-1")
	write(t, filepath.Join(root, "counter", "counter.go"), counterModified)
	write(t, filepath.Join(root, "counter", "limiter.go"), limiterAdded)
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "feature")

	for p, body := range verifyTests {
		write(t, filepath.Join(root, p), body)
	}
	return gitx.New(root), root
}

func params(r gitx.Repo, root, testFile string) Params {
	return Params{
		Repo:                r,
		MainBranch:          "main",
		Branch:              "crew/alpha/attempt-1",
		SourceWorktree:      root,
		TestFile:            testFile,
		TestArgv:            []string{"go", "test", "./counter/..."},
		BuildFailureMarkers: markers,
		ScratchDir:          filepath.Join(root, ".crew", "scratch"),
	}
}

// A criterion covering a change to pre-existing API produces real evidence.
func TestModificationDiscriminates(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})

	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Classification != Discriminating {
		t.Fatalf("Classification = %q, want %q\nmerge-base output:\n%s",
			got.Classification, Discriminating, got.MergeBaseOutput)
	}
	if !got.PassesAtHead || !got.FailsAtMergeBase || !got.Met {
		t.Errorf("result = %+v", got)
	}
	if got.Downgrades() {
		t.Error("discriminating result should not downgrade")
	}
	if !strings.Contains(got.MergeBaseOutput, "11th Add returned nil") {
		t.Errorf("expected the assertion failure in the output:\n%s", got.MergeBaseOutput)
	}
}

// A criterion covering brand-new API cannot discriminate: reverting deletes
// the symbol, so the test no longer compiles.
func TestNewAPISurfaceDowngradesToBuildFailure(t *testing.T) {
	file := "counter/limiter_crewverify_test.go"
	r, root := repo(t, map[string]string{file: limiterTest})

	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Classification != BuildFailed {
		t.Fatalf("Classification = %q, want %q\noutput:\n%s",
			got.Classification, BuildFailed, got.MergeBaseOutput)
	}
	if got.Met {
		t.Error("a build failure must not count as evidence")
	}
	if !got.Downgrades() {
		t.Error("build failure should downgrade to judgment")
	}
}

// The fix for the branch-added-file gap: an added implementation file has no
// merge-base version, so it must be deleted or the revert is a no-op.
func TestBranchAddedFilesAreDeletedNotSkipped(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})

	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, d := range got.DeletedFiles {
		if d == "counter/limiter.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("branch-added file was not deleted; deleted = %v", got.DeletedFiles)
	}
	if len(got.RevertedFiles) == 0 {
		t.Error("modified files were not restored to their merge-base content")
	}
}

// The verifier's own test must survive the revert, or there is nothing to run.
func TestTheVerifierTestIsPreserved(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})
	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got.DeletedFiles {
		if d == file {
			t.Fatal("the verifier's test was deleted by the revert")
		}
	}
	if !strings.Contains(got.MergeBaseOutput, "TestAddCapsAtTen") {
		t.Fatalf("the verifier's test did not run after the revert:\n%s", got.MergeBaseOutput)
	}
}

// The flagship guarantee: a test that exists only in the task worktree must be
// present in BOTH phases. Building the scratch worktree from the committed
// branch head and stopping there ran the control without the verifier's test
// at all, so it measured whatever tests the implementer had committed.
func TestTheVerifierTestIsRunEvenThoughItIsUncommitted(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})

	// Nothing on the branch knows about this file; it exists only in the tree.
	changes, err := r.ChangedFiles("main", "crew/alpha/attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Path == file {
			t.Fatalf("fixture is wrong: %s is committed, which production cannot produce", file)
		}
	}

	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(got.HeadOutput, "ok") {
		t.Errorf("phase one did not run the package:\n%s", got.HeadOutput)
	}
	if got.Classification != Discriminating {
		t.Fatalf("Classification = %q, want %q; the verifier's test was not in the control\n"+
			"head output:\n%s\nmerge-base output:\n%s",
			got.Classification, Discriminating, got.HeadOutput, got.MergeBaseOutput)
	}
	if !strings.Contains(got.MergeBaseOutput, "11th Add returned nil") {
		t.Errorf("the control did not evaluate the verifier's assertion:\n%s", got.MergeBaseOutput)
	}
}

// A test crew can find in neither place is reported, not silently run without.
// Swallowing it would make the control measure the implementer's own tests.
func TestAMissingVerifierTestIsAnError(t *testing.T) {
	r, root := repo(t, nil)
	_, err := Run(params(r, root, "counter/absent_crewverify_test.go"))
	if err == nil {
		t.Fatal("Run succeeded with no test to control")
	}
	if !strings.Contains(err.Error(), "absent_crewverify_test.go") {
		t.Errorf("error = %v, want it to name the missing test", err)
	}
}

// An implementer-authored test is already on the branch, so the checkout
// supplies it and no source worktree is needed.
func TestACommittedTestNeedsNoSourceWorktree(t *testing.T) {
	file := "counter/cap_test.go"
	r, root := repo(t, nil)
	write(t, filepath.Join(root, file), capTest)
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "self-authored test")

	p := params(r, root, file)
	p.SourceWorktree = ""
	got, err := Run(p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Classification != Discriminating {
		t.Fatalf("Classification = %q, want %q\n%s", got.Classification, Discriminating, got.MergeBaseOutput)
	}
}

// Only the criterion's own verifier test is placed in the scratch worktree, so
// an unrelated one cannot contaminate this criterion's result.
func TestOtherVerifierTestsAreRemoved(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	other := "counter/limiter_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest, other: limiterTest})

	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Without removing the other test, the reverted tree would fail to build
	// on NewLimiter and be misclassified as a build failure.
	if got.Classification != Discriminating {
		t.Fatalf("Classification = %q, want %q\noutput:\n%s",
			got.Classification, Discriminating, got.MergeBaseOutput)
	}
}

// A test that passes without the implementation proves nothing.
func TestNonDiscriminatingTestIsCaught(t *testing.T) {
	file := "counter/weak_crewverify_test.go"
	weak := `package counter

import "testing"

func TestWeak(t *testing.T) {
	var c Counter
	if err := c.Add(); err != nil {
		t.Fatal(err)
	}
}
`
	r, root := repo(t, map[string]string{file: weak})
	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Classification != PassesAtMergeBase {
		t.Fatalf("Classification = %q, want %q", got.Classification, PassesAtMergeBase)
	}
	if got.Met {
		t.Error("a test that passes without the implementation must not count as evidence")
	}
}

// If the test does not pass at head, the control cannot be interpreted.
func TestFailingAtHeadIsReportedNotMisread(t *testing.T) {
	file := "counter/broken_crewverify_test.go"
	broken := `package counter

import "testing"

func TestBroken(t *testing.T) { t.Fatal("always fails") }
`
	r, root := repo(t, map[string]string{file: broken})
	got, err := Run(params(r, root, file))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Classification != FailsAtHead {
		t.Fatalf("Classification = %q, want %q", got.Classification, FailsAtHead)
	}
	if got.Met {
		t.Error("must not count as evidence")
	}
}

// Raw output from both phases is retained so markers can be tuned.
func TestRawOutputIsRetainedForTuning(t *testing.T) {
	file := "counter/limiter_crewverify_test.go"
	r, root := repo(t, map[string]string{file: limiterTest})
	got, _ := Run(params(r, root, file))
	if strings.TrimSpace(got.MergeBaseOutput) == "" {
		t.Error("merge-base output was not retained")
	}
	if strings.TrimSpace(got.HeadOutput) == "" {
		t.Error("head output was not retained")
	}
}

// The throwaway worktree must not be left behind.
func TestScratchWorktreeIsCleanedUp(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})
	if _, err := Run(params(r, root, file)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".crew", "scratch"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("scratch worktree left behind: %s", e.Name())
	}
	worktrees, _ := r.ListWorktrees()
	if len(worktrees) != 1 {
		t.Errorf("worktrees still registered: %v", worktrees)
	}
}

// The real branch must be untouched by the control.
func TestBranchIsUnmodified(t *testing.T) {
	file := "counter/cap_crewverify_test.go"
	r, root := repo(t, map[string]string{file: capTest})
	before, _ := r.RevParse("crew/alpha/attempt-1")
	// The verifier's uncommitted test is the tree's only expected difference,
	// so the status is compared rather than required to be empty.
	statusBefore, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if _, err := Run(params(r, root, file)); err != nil {
		t.Fatal(err)
	}
	after, _ := r.RevParse("crew/alpha/attempt-1")
	if before != after {
		t.Fatal("the negative control moved the branch")
	}
	statusAfter, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if string(statusBefore) != string(statusAfter) {
		t.Fatalf("the negative control changed the working tree:\nbefore:\n%s\nafter:\n%s",
			statusBefore, statusAfter)
	}
}
