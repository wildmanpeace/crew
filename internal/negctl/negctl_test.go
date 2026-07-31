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
func repo(t *testing.T, extraTests map[string]string) (gitx.Repo, string) {
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
	for p, body := range extraTests {
		write(t, filepath.Join(root, p), body)
	}
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "feature")
	return gitx.New(root), root
}

func params(r gitx.Repo, root, testFile string) Params {
	return Params{
		Repo:                r,
		MainBranch:          "main",
		Branch:              "crew/alpha/attempt-1",
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
}

// Other criteria's verifier tests are removed, so an unrelated one cannot
// contaminate this criterion's result.
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
	if _, err := Run(params(r, root, file)); err != nil {
		t.Fatal(err)
	}
	after, _ := r.RevParse("crew/alpha/attempt-1")
	if before != after {
		t.Fatal("the negative control moved the branch")
	}
	clean, _ := r.IsClean()
	if !clean {
		out, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
		t.Fatalf("the negative control dirtied the working tree:\n%s", out)
	}
}
