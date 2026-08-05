package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
)

// This reproduces what a live run actually produced: an implementer that
// wrote both the implementation and the test its criterion named, and a
// verifier that ran that test and recorded a clean mechanical result. The
// question these tests settle is whether crew now notices when such a test
// cannot fail.

const bucketBase = `package ratelimit

type Bucket struct {
	capacity int
	tokens   int
}

func New(capacity int) *Bucket { return &Bucket{capacity: capacity, tokens: capacity} }

func (b *Bucket) Tokens() int { return b.tokens }

// Allow permits every request, which is the gap a task must close.
func (b *Bucket) Allow() bool {
	if b.tokens > 0 {
		b.tokens--
	}
	return true
}
`

const bucketFixed = `package ratelimit

type Bucket struct {
	capacity int
	tokens   int
}

func New(capacity int) *Bucket { return &Bucket{capacity: capacity, tokens: capacity} }

func (b *Bucket) Tokens() int { return b.tokens }

func (b *Bucket) Allow() bool {
	if b.tokens > 0 {
		b.tokens--
		return true
	}
	return false
}
`

// A test that genuinely pins the new behaviour: it fails against the old Allow.
const realTest = `package ratelimit

import "testing"

func TestAllowRefusesWhenEmpty(t *testing.T) {
	b := New(1)
	if !b.Allow() {
		t.Fatal("first Allow returned false")
	}
	if b.Allow() {
		t.Fatal("Allow returned true on an empty bucket")
	}
}
`

// A test that looks plausible, passes, and asserts nothing about the change:
// it passes just as well against the old Allow.
const vacuousTest = `package ratelimit

import "testing"

func TestAllowRefusesWhenEmpty(t *testing.T) {
	b := New(2)
	if !b.Allow() {
		t.Fatal("Allow returned false while tokens remained")
	}
	if b.Tokens() != 1 {
		t.Fatalf("Tokens() = %d, want 1", b.Tokens())
	}
}
`

// selfAuthoredFixture builds a repo whose branch adds an implementation and a
// test, exactly as an implementer would.
func selfAuthoredFixture(t *testing.T, testBody string) *Loop {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := gitx.New(root)
	mustRun(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module sa\n\ngo 1.26\n")
	write(t, filepath.Join(root, "ratelimit", "bucket.go"), bucketBase)
	mustRun(t, repo, "add", "-A")
	mustRun(t, repo, "commit", "-qm", "base")

	worktree := WorktreePath(root, "alpha", 1)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddWorktreeBranch(worktree, BranchName("alpha", 1), "main"); err != nil {
		t.Fatal(err)
	}
	// The implementer writes the fix and its own test in one commit.
	write(t, filepath.Join(worktree, "ratelimit", "bucket.go"), bucketFixed)
	write(t, filepath.Join(worktree, "ratelimit", "bucket_test.go"), testBody)
	wt := gitx.New(worktree)
	mustRun(t, wt, "add", "-A")
	mustRun(t, wt, "commit", "-qm", "implement and test")

	loc, _ := time.LoadLocation("America/Denver")
	store, err := state.Open(root, loc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		MainBranch: "main", MaxCycles: 3, ConcurrencyCap: 3,
		VerifyTestSuffix: "_crewverify_test.go", TestFileSuffix: "_test.go",
		NegativeControlBuildFailureMarkers: []string{"[build failed]", "undefined: ", "cannot find package"},
		CheckCommands: map[string]config.CheckCommand{
			"test": {Argv: []string{"go", "test"}, DefaultArgs: []string{"./..."}},
		},
	}
	store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1, Status: state.StatusVerifying})
		return nil
	})
	return &Loop{Root: root, Cfg: cfg, Store: store, Repo: repo, Loc: loc, Session: "sa-test"}
}

func mustRun(t *testing.T, r gitx.Repo, args ...string) {
	t.Helper()
	if _, err := r.Run(args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// verifierClaim is what a verifier reports after running a green test.
func verifierClaim() *report.Verifier {
	return &report.Verifier{
		TaskID: "alpha", Role: "verifier", Status: report.StatusSatisfied,
		CriteriaResults: []report.CriterionResult{{
			Description: "Allow returns false once the bucket is exhausted.",
			Evaluation:  report.EvalMechanical,
			Command:     "crew-check test ./ratelimit/... -run TestAllowRefusesWhenEmpty",
			ExitCode:    intp(0),
			Met:         boolp(true),
		}},
	}
}

// The hole, closed: a green exit code on a test that cannot fail no longer
// counts as mechanical evidence.
func TestVacuousSelfAuthoredTestIsCaught(t *testing.T) {
	l := selfAuthoredFixture(t, vacuousTest)
	r := verifierClaim()

	if err := l.runNegativeControls("alpha", r); err != nil {
		t.Fatalf("runNegativeControls: %v", err)
	}
	c := r.CriteriaResults[0]

	if c.TestAuthor != string(AuthorBranch) {
		t.Fatalf("TestAuthor = %q, want the test attributed to the branch", c.TestAuthor)
	}
	if c.Classification != string("passes_at_merge_base") {
		t.Fatalf("Classification = %q, want passes_at_merge_base\noutput:\n%s",
			c.Classification, c.RawOutput)
	}
	if c.Satisfied() {
		t.Fatal("a test that passes without the implementation still counted as evidence")
	}

	// And the task goes back for another cycle with an actionable reason.
	allClear, failures := Outcome(r)
	if allClear {
		t.Fatal("verification passed despite a vacuous test")
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "still passes with the implementation reverted") {
		t.Fatalf("failures = %v", failures)
	}
}

// The control must not punish honest work: a real test still passes.
func TestGenuineSelfAuthoredTestIsAccepted(t *testing.T) {
	l := selfAuthoredFixture(t, realTest)
	r := verifierClaim()

	if err := l.runNegativeControls("alpha", r); err != nil {
		t.Fatalf("runNegativeControls: %v", err)
	}
	c := r.CriteriaResults[0]

	if c.Classification != "discriminating" {
		t.Fatalf("Classification = %q, want discriminating\noutput:\n%s", c.Classification, c.RawOutput)
	}
	if !c.Satisfied() {
		t.Fatal("a genuinely discriminating test was rejected")
	}
	if allClear, failures := Outcome(r); !allClear {
		t.Fatalf("verification failed on good work: %v", failures)
	}
}

// A criterion whose test predates the branch needs no control, and must not
// be slowed down or second-guessed by one.
func TestPreExistingTestIsNotControlled(t *testing.T) {
	l := selfAuthoredFixture(t, realTest)
	r := verifierClaim()
	// Name a test that the branch did not add.
	r.CriteriaResults[0].Command = "crew-check test ./ratelimit/... -run TestSomethingOlder"

	if err := l.runNegativeControls("alpha", r); err != nil {
		t.Fatal(err)
	}
	c := r.CriteriaResults[0]
	if c.TestAuthor != string(AuthorPreExisting) {
		t.Fatalf("TestAuthor = %q, want pre_existing", c.TestAuthor)
	}
	if !c.Satisfied() {
		t.Fatal("a pre-existing test's criterion was rejected")
	}
}

// When crew cannot tie a criterion to one added test it says so rather than
// guessing, and the gap is visible instead of hidden.
func TestUnattributableCriterionIsRecordedNotGuessed(t *testing.T) {
	l := selfAuthoredFixture(t, realTest)
	r := verifierClaim()
	r.CriteriaResults[0].Command = "crew-check test ./ratelimit/..." // no -run filter

	if err := l.runNegativeControls("alpha", r); err != nil {
		t.Fatal(err)
	}
	c := r.CriteriaResults[0]
	if c.NegativeControlStatus != "not_attributable" {
		t.Fatalf("NegativeControlStatus = %q", c.NegativeControlStatus)
	}
	if !c.Satisfied() {
		t.Error("an unattributable criterion should still stand on its exit code")
	}
}

// The ordinary shape: the package already has a test file, and the worker
// appends its test to it. git reports that as a modification, not an
// addition, and treating only added files as self-authored would leave the
// hole open in precisely this common case.
func TestSelfAuthoredTestAppendedToAnExistingFileIsStillControlled(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := gitx.New(root)
	mustRun(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module sa\n\ngo 1.26\n")
	write(t, filepath.Join(root, "ratelimit", "bucket.go"), bucketBase)
	// A test file that already exists on main.
	write(t, filepath.Join(root, "ratelimit", "bucket_test.go"),
		"package ratelimit\n\nimport \"testing\"\n\nfunc TestNewStartsFull(t *testing.T) {\n\tif New(3).Tokens() != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	mustRun(t, repo, "add", "-A")
	mustRun(t, repo, "commit", "-qm", "base")

	worktree := WorktreePath(root, "alpha", 1)
	os.MkdirAll(filepath.Dir(worktree), 0o755)
	if err := repo.AddWorktreeBranch(worktree, BranchName("alpha", 1), "main"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(worktree, "ratelimit", "bucket.go"), bucketFixed)
	// The worker appends a vacuous test to the existing file.
	write(t, filepath.Join(worktree, "ratelimit", "bucket_test.go"),
		"package ratelimit\n\nimport \"testing\"\n\nfunc TestNewStartsFull(t *testing.T) {\n\tif New(3).Tokens() != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n\nfunc TestAllowRefusesWhenEmpty(t *testing.T) {\n\tb := New(2)\n\tif !b.Allow() {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
	wt := gitx.New(worktree)
	mustRun(t, wt, "add", "-A")
	mustRun(t, wt, "commit", "-qm", "implement and test")

	loc, _ := time.LoadLocation("America/Denver")
	store, _ := state.Open(root, loc)
	store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1, Status: state.StatusVerifying})
		return nil
	})
	l := &Loop{Root: root, Cfg: &config.Config{
		MainBranch: "main", MaxCycles: 3, ConcurrencyCap: 3,
		VerifyTestSuffix: "_crewverify_test.go", TestFileSuffix: "_test.go",
		NegativeControlBuildFailureMarkers: []string{"[build failed]", "undefined: ", "cannot find package"},
		CheckCommands: map[string]config.CheckCommand{
			"test": {Argv: []string{"go", "test"}, DefaultArgs: []string{"./..."}},
		},
	}, Store: store, Repo: repo, Loc: loc, Session: "sa-mod"}

	r := verifierClaim()
	if err := l.runNegativeControls("alpha", r); err != nil {
		t.Fatalf("runNegativeControls: %v", err)
	}
	c := r.CriteriaResults[0]
	if c.TestAuthor != string(AuthorBranch) {
		t.Fatalf("TestAuthor = %q; a test appended to an existing file was treated as pre-existing", c.TestAuthor)
	}
	if c.Satisfied() {
		t.Fatal("a vacuous test in a modified file still counted as evidence")
	}
}

// A criterion asserting that behaviour is *unchanged* passes at merge-base by
// construction, so no implementation can ever make it discriminate. crew must
// notice that and suggest re-tagging, rather than letting the task burn every
// cycle chasing it.
//
// This is what happened in the sandbox: the same criterion came back
// passes_at_merge_base on all three cycles, no suggestion was ever surfaced,
// and the task blocked having spent $4.74.
func TestRepeatedlyUnverifiableCriterionSuggestsARetag(t *testing.T) {
	l := selfAuthoredFixture(t, vacuousTest)

	for cycle := 1; cycle <= 2; cycle++ {
		r := verifierClaim()
		if err := l.runNegativeControls("alpha", r); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if r.CriteriaResults[0].Classification != "passes_at_merge_base" {
			t.Fatalf("cycle %d: classification = %q", cycle, r.CriteriaResults[0].Classification)
		}
	}

	st, _ := l.Store.Read()
	ts := st.Tasks["alpha"]
	const crit = "Allow returns false once the bucket is exhausted."

	if got := ts.DegradedCount(crit); got != 2 {
		t.Fatalf("DegradedCount = %d, want 2; a non-discriminating control was not counted", got)
	}
	got := RetagSuggestion(ts, crit)
	if got == "" {
		t.Fatal("no re-tag suggestion after two controls that produced no evidence")
	}
	if !strings.Contains(got, "judged: true") || !strings.Contains(got, "TASKS.md") {
		t.Errorf("suggestion = %q", got)
	}
}

// A criterion that discriminates must never accumulate a re-tag suggestion.
func TestDiscriminatingCriterionNeverSuggestsARetag(t *testing.T) {
	l := selfAuthoredFixture(t, realTest)
	for cycle := 1; cycle <= 2; cycle++ {
		r := verifierClaim()
		if err := l.runNegativeControls("alpha", r); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := l.Store.Read()
	ts := st.Tasks["alpha"]
	if got := ts.DegradedCount("Allow returns false once the bucket is exhausted."); got != 0 {
		t.Fatalf("DegradedCount = %d, want 0 for a criterion that produces evidence", got)
	}
}
