package watch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wildmanpeace/crew/internal/negctl"
	"github.com/wildmanpeace/crew/internal/report"
)

func TestCommandArgsStripsTheDispatcher(t *testing.T) {
	got := CommandArgs("crew-check test ./ratelimit/... -run TestAllowRefusesWhenEmpty")
	want := []string{"./ratelimit/...", "-run", "TestAllowRefusesWhenEmpty"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CommandArgs = %q, want %q", got, want)
	}
	if got := CommandArgs("crew-run test ./..."); !reflect.DeepEqual(got, []string{"./..."}) {
		t.Errorf("CommandArgs = %q", got)
	}
	if got := CommandArgs("something else entirely"); got != nil {
		t.Errorf("CommandArgs = %q, want nil", got)
	}
}

func TestRunFilterHandlesBothForms(t *testing.T) {
	if got := RunFilter([]string{"./...", "-run", "TestX"}); got != "TestX" {
		t.Errorf("RunFilter = %q", got)
	}
	if got := RunFilter([]string{"./...", "-run=TestY"}); got != "TestY" {
		t.Errorf("RunFilter = %q", got)
	}
	if got := RunFilter([]string{"./..."}); got != "" {
		t.Errorf("RunFilter = %q, want empty", got)
	}
}

func writeTest(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The live-run scenario: the criterion names a test the branch added, so the
// control must run.
func TestAttributionFindsABranchAddedTest(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "ratelimit/bucket_test.go",
		"package ratelimit\n\nfunc TestAllowRefusesWhenEmpty(t *testing.T) {}\n")

	got := AttributeCriterion(wt,
		"crew-check test ./ratelimit/... -run TestAllowRefusesWhenEmpty",
		[]string{"ratelimit/bucket_test.go"})

	if got.Authorship != AuthorBranch {
		t.Fatalf("Authorship = %q, want %q", got.Authorship, AuthorBranch)
	}
	if got.File != "ratelimit/bucket_test.go" {
		t.Errorf("File = %q", got.File)
	}
	if want := []string{"./ratelimit/...", "-run", "TestAllowRefusesWhenEmpty"}; !reflect.DeepEqual(got.RunArgs, want) {
		t.Errorf("RunArgs = %q, want %q", got.RunArgs, want)
	}
}

// A test that already existed on main needs no control: the implementer could
// not have shaped it to suit itself.
func TestAttributionTreatsUnmatchedTestsAsPreExisting(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "ratelimit/new_test.go",
		"package ratelimit\n\nfunc TestSomethingElse(t *testing.T) {}\n")

	got := AttributeCriterion(wt,
		"crew-check test ./ratelimit/... -run TestPreExisting",
		[]string{"ratelimit/new_test.go"})

	if got.Authorship != AuthorPreExisting {
		t.Fatalf("Authorship = %q, want %q", got.Authorship, AuthorPreExisting)
	}
}

func TestAttributionWithNoChangedTestsIsPreExisting(t *testing.T) {
	got := AttributeCriterion(t.TempDir(), "crew-check test ./... -run TestX", nil)
	if got.Authorship != AuthorPreExisting {
		t.Fatalf("Authorship = %q", got.Authorship)
	}
}

// Without a name filter the command may span many tests, so no single file
// can be held responsible.
func TestAttributionWithoutARunFilterIsUnattributable(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "a_test.go", "package a\n\nfunc TestA(t *testing.T) {}\n")
	got := AttributeCriterion(wt, "crew-check test ./...", []string{"a_test.go"})
	if got.Authorship != AuthorUnknown {
		t.Fatalf("Authorship = %q, want %q", got.Authorship, AuthorUnknown)
	}
}

// Guessing between two candidates would produce a confident wrong answer.
func TestAttributionIsConservativeWhenAmbiguous(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "a_test.go", "package a\n\nfunc TestDup(t *testing.T) {}\n")
	writeTest(t, wt, "b_test.go", "package b\n\nfunc TestDup(t *testing.T) {}\n")
	got := AttributeCriterion(wt, "crew-check test ./... -run TestDup",
		[]string{"a_test.go", "b_test.go"})
	if got.Authorship != AuthorUnknown {
		t.Fatalf("Authorship = %q, want %q", got.Authorship, AuthorUnknown)
	}
}

// A file that merely mentions the name must not be blamed for it.
func TestAttributionRequiresADeclarationNotAMention(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "a_test.go",
		"package a\n\n// see TestAllowRefusesWhenEmpty for the real case\nfunc TestOther(t *testing.T) {}\n")
	got := AttributeCriterion(wt, "crew-check test ./... -run TestAllowRefusesWhenEmpty",
		[]string{"a_test.go"})
	if got.Authorship == AuthorBranch {
		t.Fatal("a passing mention was treated as a declaration")
	}
}

// A self-authored test that genuinely fails without the implementation is
// real evidence.
func TestSelfAuthoredDiscriminatingStaysMechanicalAndMet(t *testing.T) {
	c := &report.CriterionResult{
		Description: "Allow refuses when empty", Evaluation: report.EvalMechanical,
		Command: "crew-check test", ExitCode: intp(0), Met: boolp(true),
	}
	ApplySelfAuthoredControl(c, negctl.Result{
		Classification: negctl.Discriminating, FailsAtMergeBase: true, PassesAtHead: true,
	})
	if c.Evaluation != report.EvalMechanical {
		t.Errorf("Evaluation = %q, want it to stay mechanical", c.Evaluation)
	}
	if !c.Satisfied() {
		t.Fatal("a discriminating self-authored test was not counted as evidence")
	}
	if c.TestAuthor != string(AuthorBranch) {
		t.Errorf("TestAuthor = %q", c.TestAuthor)
	}
}

// The whole point: a test that passes with the implementation reverted proves
// nothing, and must fail its criterion rather than count as evidence.
func TestSelfAuthoredVacuousTestFailsTheCriterion(t *testing.T) {
	c := &report.CriterionResult{
		Description: "Allow refuses when empty", Evaluation: report.EvalMechanical,
		Command: "crew-check test", ExitCode: intp(0), Met: boolp(true),
	}
	ApplySelfAuthoredControl(c, negctl.Result{
		Classification: negctl.PassesAtMergeBase, FailsAtMergeBase: false, PassesAtHead: true,
	})
	if c.Satisfied() {
		t.Fatal("a test that cannot fail was counted as mechanical evidence")
	}
	if c.DowngradeReason == "" {
		t.Error("no reason recorded for the next implementer to act on")
	}
	// It stays mechanical: this is a defect in the work, not an absence of
	// evidence, so it should drive another cycle rather than a re-tag.
	if c.Evaluation != report.EvalMechanical {
		t.Errorf("Evaluation = %q, want it to stay mechanical", c.Evaluation)
	}
}

// New API cannot be controlled, so it falls back to judgment rather than
// punishing work that may well be correct.
func TestSelfAuthoredBuildFailureFallsBackToJudgment(t *testing.T) {
	c := &report.CriterionResult{
		Description: "NextAllowedIn reports 0 or 1", Evaluation: report.EvalMechanical,
		Command: "crew-check test", ExitCode: intp(0), Met: boolp(true),
	}
	ApplySelfAuthoredControl(c, negctl.Result{
		Classification: negctl.BuildFailed, FailsAtMergeBase: true, PassesAtHead: true,
		Reason: "the reverted tree does not build",
	})
	if c.Evaluation != report.EvalJudged {
		t.Fatalf("Evaluation = %q, want judged", c.Evaluation)
	}
	// It keeps the verifier's own conclusion. Failing it would block every
	// criterion covering new API surface, permanently and unfixably.
	if !c.Satisfied() {
		t.Error("an uncontrollable criterion was failed rather than left to judgment")
	}
	if c.DowngradeReason == "" {
		t.Error("no reason recorded, so the captain cannot see it is judged")
	}
}

// The gap is recorded rather than hidden, so review shows which mechanical
// results were actually controlled.
func TestUnattributableAndPreExistingAreRecorded(t *testing.T) {
	c := &report.CriterionResult{Evaluation: report.EvalMechanical, ExitCode: intp(0), Met: boolp(true)}
	MarkUnattributable(c)
	if c.TestAuthor != string(AuthorUnknown) || c.NegativeControlStatus != "not_attributable" {
		t.Errorf("unattributable not recorded: %+v", c)
	}
	if !c.Satisfied() {
		t.Error("an unattributable criterion should still stand on its exit code")
	}

	c2 := &report.CriterionResult{Evaluation: report.EvalMechanical, ExitCode: intp(0), Met: boolp(true)}
	MarkPreExisting(c2)
	if c2.TestAuthor != string(AuthorPreExisting) || c2.NegativeControlStatus != "not_required" {
		t.Errorf("pre-existing not recorded: %+v", c2)
	}
	if !c2.Satisfied() {
		t.Error("a pre-existing test's criterion should stand")
	}
}

// -run takes a regexp, so a filter carrying pattern syntax or shell quoting
// can never match a literal declaration. Reading that miss as "the test
// predates this branch" waived the control for a test the branch did author —
// exactly backwards, and silent. It is reported as unattributable instead.
func TestAttributionDoesNotReadAnUnmatchableFilterAsPreExisting(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "ratelimit/bucket_test.go",
		"package ratelimit\n\nfunc TestAllowRefusesWhenEmpty(t *testing.T) {}\n")

	for _, filter := range []string{"TestAllowRefusesWhenEmpty$", "^TestAllowRefusesWhenEmpty$",
		"'TestAllowRefusesWhenEmpty'", "TestAllow.*Empty", "TestAllowRefusesWhenEmpty/subcase"} {
		got := AttributeCriterion(wt,
			"crew-check test ./ratelimit/... -run "+filter,
			[]string{"ratelimit/bucket_test.go"})
		if got.Authorship != AuthorUnknown {
			t.Errorf("filter %q: Authorship = %q, want %q; a branch-authored test was waived as pre-existing",
				filter, got.Authorship, AuthorUnknown)
		}
	}
}

// A plain name that genuinely matches nothing the branch touched still means
// what it always did, or every criterion would become unattributable.
func TestAttributionStillRecognisesAGenuinelyPreExistingTest(t *testing.T) {
	wt := t.TempDir()
	writeTest(t, wt, "ratelimit/bucket_test.go",
		"package ratelimit\n\nfunc TestSomethingElse(t *testing.T) {}\n")

	got := AttributeCriterion(wt,
		"crew-check test ./ratelimit/... -run TestSomethingOlder",
		[]string{"ratelimit/bucket_test.go"})
	if got.Authorship != AuthorPreExisting {
		t.Fatalf("Authorship = %q, want %q", got.Authorship, AuthorPreExisting)
	}
}
