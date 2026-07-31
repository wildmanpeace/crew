package watch

import (
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/negctl"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
)

func cfg() *config.Config { return &config.Config{MaxCycles: 3} }

func intp(i int) *int    { return &i }
func boolp(b bool) *bool { return &b }

func TestCycleCapAllowsExactlyThreeImplementers(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1}
	for want := 1; want <= 3; want++ {
		got, allowed := NextImplementerCycle(ts, cfg())
		if !allowed || got != want {
			t.Fatalf("cycle %d: got (%d, %v), want (%d, true)", want, got, allowed, want)
		}
		ts.Cycle = got
	}
	if _, allowed := NextImplementerCycle(ts, cfg()); allowed {
		t.Fatal("a fourth implementer was permitted")
	}
}

func TestFourthImplementerBecomesNeedsReframe(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 3}
	p := AfterVerifyFailed(ts, cfg())
	if p.Action != ActionNeedsReframe {
		t.Fatalf("Action = %q, want %q", p.Action, ActionNeedsReframe)
	}
	if !strings.Contains(p.Reason, "cycle cap") {
		t.Errorf("Reason = %q", p.Reason)
	}
}

func TestVerifyFailedSpawnsNextImplementerBelowCap(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	p := AfterVerifyFailed(ts, cfg())
	if p.Action != ActionSpawnImplementer {
		t.Fatalf("Action = %q", p.Action)
	}
	if !strings.Contains(p.Reason, "cycle 2") {
		t.Errorf("Reason = %q", p.Reason)
	}
}

// A reframe resets the cycle budget, so a fresh attempt gets three more.
func TestReframeResetsTheCycleBudget(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 2, Cycle: 0}
	if _, allowed := NextImplementerCycle(ts, cfg()); !allowed {
		t.Fatal("a reframed attempt was refused its first implementer")
	}
}

func TestImplementerDoneSpawnsVerifier(t *testing.T) {
	r := &report.Implementer{Status: report.StatusDone}
	if p := AfterImplementer(r, nil); p.Action != ActionSpawnVerifier {
		t.Fatalf("Action = %q", p.Action)
	}
}

func TestImplementerBlockedIsTerminal(t *testing.T) {
	r := &report.Implementer{Status: report.StatusBlocked, Summary: "needs a decision"}
	if p := AfterImplementer(r, nil); p.Action != ActionBlocked {
		t.Fatalf("Action = %q", p.Action)
	}
}

// No report is always a failure; process exit is liveness, not content.
func TestMissingImplementerReportIsFailure(t *testing.T) {
	p := AfterImplementer(nil, errNoReport{})
	if p.Action != ActionFailed {
		t.Fatalf("Action = %q, want failed", p.Action)
	}
}

type errNoReport struct{}

func (errNoReport) Error() string { return "read .crew-report.json: no such file" }

// The verifier's own failure must not consume the implementation's cycle
// budget.
func TestVerifierCrashRetriesOnceWithoutConsumingACycle(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	p := AfterVerifier(ts, cfg(), nil, errNoReport{}, false)
	if p.Action != ActionRetryVerifier {
		t.Fatalf("Action = %q, want retry", p.Action)
	}
	if ts.Cycle != 1 {
		t.Fatalf("Cycle = %d, want it unchanged at 1", ts.Cycle)
	}
}

func TestSecondConsecutiveVerifierFailureBlocks(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	p := AfterVerifier(ts, cfg(), nil, errNoReport{}, true)
	if p.Action != ActionBlocked {
		t.Fatalf("Action = %q, want blocked", p.Action)
	}
}

func TestVerifierBlockedIsRetriedThenBlocks(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	r := &report.Verifier{Status: report.StatusBlocked}
	if p := AfterVerifier(ts, cfg(), r, nil, false); p.Action != ActionRetryVerifier {
		t.Fatalf("first: Action = %q", p.Action)
	}
	if p := AfterVerifier(ts, cfg(), r, nil, true); p.Action != ActionBlocked {
		t.Fatalf("second: Action = %q", p.Action)
	}
}

func TestAllCriteriaSatisfiedIsReadyForReview(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	r := &report.Verifier{Status: report.StatusSatisfied, CriteriaResults: []report.CriterionResult{
		{Description: "a", Evaluation: report.EvalMechanical, Command: "crew-check test", ExitCode: intp(0), Met: boolp(true)},
		{Description: "b", Evaluation: report.EvalJudged, Met: boolp(true)},
	}}
	p := AfterVerifier(ts, cfg(), r, nil, false)
	if p.Action != ActionReadyForReview {
		t.Fatalf("Action = %q, want ready_for_review", p.Action)
	}
	if !strings.Contains(p.Reason, "1 mechanical / 1 judged") {
		t.Errorf("Reason should carry the ratio: %q", p.Reason)
	}
}

// The core rule: a claimed satisfied is downgraded when the numbers disagree.
func TestClaimedSatisfiedIsDowngradedByAFailingCheck(t *testing.T) {
	ts := &state.TaskState{ID: "alpha", Attempt: 1, Cycle: 1}
	r := &report.Verifier{Status: report.StatusSatisfied, CriteriaResults: []report.CriterionResult{
		{Description: "rate limiting works", Evaluation: report.EvalMechanical,
			Command: "crew-check test", ExitCode: intp(1), Met: boolp(true)},
	}}
	p := AfterVerifier(ts, cfg(), r, nil, false)
	if p.Action != ActionSpawnImplementer {
		t.Fatalf("Action = %q, want another implementer", p.Action)
	}
	if !strings.Contains(p.Reason, "rate limiting works") {
		t.Errorf("Reason should name the unmet criterion: %q", p.Reason)
	}
}

// Judgment may add failures on top of a clean mechanical run.
func TestJudgmentCanAddAFailure(t *testing.T) {
	r := &report.Verifier{Status: report.StatusSatisfied, CriteriaResults: []report.CriterionResult{
		{Description: "a", Evaluation: report.EvalMechanical, Command: "c", ExitCode: intp(0), Met: boolp(true)},
		{Description: "auth unchanged", Evaluation: report.EvalJudged, Met: boolp(false), Notes: "auth middleware was touched"},
	}}
	allClear, failures := Outcome(r)
	if allClear {
		t.Fatal("judged failure was ignored")
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "auth unchanged") {
		t.Fatalf("failures = %v", failures)
	}
}

// A discriminating control is crew's evidence that the criterion is met.
func TestApplyNegativeControlDiscriminating(t *testing.T) {
	c := report.CriterionResult{Description: "d", Evaluation: report.EvalNegativeControl,
		TestFile: "x_crewverify_test.go", NegativeControlStatus: "pending_crew_evaluation"}
	ApplyNegativeControl(&c, negctl.Result{
		Classification: negctl.Discriminating, FailsAtMergeBase: true, PassesAtHead: true, Met: true,
		MergeBaseOutput: "FAIL", Reason: "fails without the implementation",
	})
	if !c.Satisfied() {
		t.Fatalf("criterion not satisfied: %+v", c)
	}
	if c.Evaluation != report.EvalNegativeControl {
		t.Errorf("Evaluation = %q, should stay mechanical", c.Evaluation)
	}
}

// A build failure produced no evidence, so the criterion falls back to
// judgment rather than counting as met.
func TestApplyNegativeControlBuildFailureDowngradesToJudged(t *testing.T) {
	c := report.CriterionResult{Description: "d", Evaluation: report.EvalNegativeControl,
		TestFile: "x_crewverify_test.go"}
	ApplyNegativeControl(&c, negctl.Result{
		Classification: negctl.BuildFailed, FailsAtMergeBase: true, PassesAtHead: true,
		MergeBaseOutput: "undefined: NewLimiter", Reason: "the reverted tree does not build",
	})
	if c.Evaluation != report.EvalJudged {
		t.Fatalf("Evaluation = %q, want judged", c.Evaluation)
	}
	// Judged, not failed: the control could not answer, and no further work
	// by an implementer could make it answer.
	if !c.Satisfied() {
		t.Fatal("a criterion the control could not evaluate was failed outright")
	}
	if c.DowngradeReason == "" {
		t.Error("no downgrade reason recorded")
	}
	if c.RawOutput == "" {
		t.Error("raw output not retained for marker tuning")
	}
}

// crew's finding overwrites whatever the verifier claimed.
func TestCrewsFindingOverwritesTheVerifiersClaim(t *testing.T) {
	c := report.CriterionResult{Description: "d", Evaluation: report.EvalNegativeControl,
		TestFile: "x_crewverify_test.go", Met: boolp(true)}
	ApplyNegativeControl(&c, negctl.Result{
		Classification: negctl.PassesAtMergeBase, FailsAtMergeBase: false, PassesAtHead: true,
		Reason: "the test passes without the implementation",
	})
	if c.Satisfied() {
		t.Fatal("the verifier's optimistic claim survived crew's finding")
	}
}

func TestRetagSuggestionOnlyAfterTwoDegradations(t *testing.T) {
	ts := &state.TaskState{ID: "alpha"}
	const crit = "rate limit is configurable"
	ts.NoteDegraded(crit)
	if RetagSuggestion(ts, crit) != "" {
		t.Error("suggested a re-tag after a single degradation")
	}
	ts.NoteDegraded(crit)
	got := RetagSuggestion(ts, crit)
	if got == "" {
		t.Fatal("no suggestion after two degradations")
	}
	if !strings.Contains(got, "judged: true") || !strings.Contains(got, "TASKS.md") {
		t.Errorf("suggestion = %q", got)
	}
	if !strings.Contains(got, "consider") {
		t.Errorf("suggestion should read as advice, not an action: %q", got)
	}
}

func TestOutcomeNamesEveryUnmetCriterion(t *testing.T) {
	r := &report.Verifier{CriteriaResults: []report.CriterionResult{
		{Description: "one", Evaluation: report.EvalMechanical, Command: "c", ExitCode: intp(1), Met: boolp(false)},
		{Description: "two", Evaluation: report.EvalJudged, Met: boolp(false)},
		{Description: "three", Evaluation: report.EvalMechanical, Command: "c", ExitCode: intp(0), Met: boolp(true)},
	}}
	allClear, failures := Outcome(r)
	if allClear || len(failures) != 2 {
		t.Fatalf("allClear = %v, failures = %v", allClear, failures)
	}
}
