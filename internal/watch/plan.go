// Package watch drives the implement/verify loop.
//
// It is pure mechanism: it makes no LLM calls and forms no judgments. Every
// decision here is a rule applied to recorded state, which is what makes the
// cycle cap, the budget ceilings, and the mechanical downgrade enforceable
// rather than advisory.
package watch

import (
	"fmt"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/negctl"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
)

// Action is what the loop should do next for a task.
type Action string

const (
	ActionNone             Action = "none"
	ActionSpawnImplementer Action = "spawn_implementer"
	ActionSpawnVerifier    Action = "spawn_verifier"
	ActionRetryVerifier    Action = "retry_verifier"
	ActionNeedsReframe     Action = "needs_reframe"
	ActionReadyForReview   Action = "ready_for_review"
	ActionBlocked          Action = "blocked"
	ActionFailed           Action = "failed"
)

// Plan is a decision plus the reason it was reached, so events.jsonl records
// why the loop did what it did.
type Plan struct {
	Action Action
	Reason string
}

// NextImplementerCycle returns the cycle number a new implementer would run
// as, and whether the cap permits it.
//
// Cycle counts implementer spawns within an attempt: the first implementer of
// an attempt runs at cycle 1, and each retry after a failed verification
// increments it. A fourth at the cap becomes a reframe decision instead.
func NextImplementerCycle(ts *state.TaskState, cfg *config.Config) (cycle int, allowed bool) {
	next := ts.Cycle + 1
	return next, next <= cfg.MaxCycles
}

// ResumeStep returns the role and cycle a queued task should be started at.
//
// Queued is not only where crew spawn leaves a task; it is also where a spawn
// that failed transiently returns one, and by then the role and cycle that
// spawn was attempting are already recorded. Always restarting at cycle 1's
// implementer turned a failed verifier spawn into a fresh implementation
// attempt: the completed work was never verified, and the cycle cap started
// over. A task that has never had a worker has cycle 0, which is what
// distinguishes the two.
func ResumeStep(ts *state.TaskState) (config.Role, int) {
	if ts == nil || ts.Cycle < 1 {
		return config.RoleImplementer, 1
	}
	if config.Role(ts.Role) == config.RoleVerifier {
		return config.RoleVerifier, ts.Cycle
	}
	return config.RoleImplementer, ts.Cycle
}

// AfterVerifyFailed decides what happens when a verification pass fails.
func AfterVerifyFailed(ts *state.TaskState, cfg *config.Config) Plan {
	cycle, allowed := NextImplementerCycle(ts, cfg)
	if !allowed {
		return Plan{
			Action: ActionNeedsReframe,
			Reason: fmt.Sprintf("cycle cap reached: %d of %d cycles used in attempt %d",
				ts.Cycle, cfg.MaxCycles, ts.Attempt),
		}
	}
	return Plan{
		Action: ActionSpawnImplementer,
		Reason: fmt.Sprintf("verification failed; starting cycle %d of %d", cycle, cfg.MaxCycles),
	}
}

// AfterImplementer decides what happens when an implementer exits.
//
// A missing or rejected report is a failure: process exit is liveness only,
// and a claim of done that its own recorded checks contradict is not a
// completion.
func AfterImplementer(r *report.Implementer, loadErr error) Plan {
	if loadErr != nil {
		if report.IsRejected(loadErr) {
			return Plan{Action: ActionFailed, Reason: "implementer report rejected: " + loadErr.Error()}
		}
		return Plan{Action: ActionFailed, Reason: "no usable implementer report: " + loadErr.Error()}
	}
	switch r.Status {
	case report.StatusDone:
		return Plan{Action: ActionSpawnVerifier, Reason: "implementer reported done with all checks passing"}
	case report.StatusBlocked:
		return Plan{Action: ActionBlocked, Reason: "implementer reported blocked: " + r.Summary}
	default:
		return Plan{Action: ActionFailed, Reason: "implementer reported failed: " + r.Summary}
	}
}

// AfterVerifier decides what happens when a verifier exits.
//
// A verifier that crashed or blocked is retried once without consuming a
// cycle, because the cycle budget belongs to the implementation attempt and
// should not be spent on the checker's own failure.
func AfterVerifier(ts *state.TaskState, cfg *config.Config, r *report.Verifier, loadErr error, alreadyRetried bool) Plan {
	if loadErr != nil || r == nil {
		reason := "verifier produced no usable report"
		if loadErr != nil {
			reason += ": " + loadErr.Error()
		}
		if alreadyRetried {
			return Plan{Action: ActionBlocked, Reason: reason + " (second consecutive failure)"}
		}
		return Plan{Action: ActionRetryVerifier, Reason: reason + "; retrying once without consuming a cycle"}
	}
	if r.Status == report.StatusBlocked || r.Status == report.StatusFailed {
		if alreadyRetried {
			return Plan{Action: ActionBlocked, Reason: "verifier blocked twice consecutively"}
		}
		return Plan{Action: ActionRetryVerifier, Reason: "verifier reported " + r.Status + "; retrying once without consuming a cycle"}
	}

	allClear, failures := Outcome(r)
	if allClear {
		return Plan{Action: ActionReadyForReview, Reason: "all criteria satisfied (" + r.RatioString() + ")"}
	}
	return AfterVerifyFailedWith(ts, cfg, failures)
}

// AfterVerifyFailedWith is AfterVerifyFailed with the specific failures
// attached, so the next implementer can be briefed on them.
func AfterVerifyFailedWith(ts *state.TaskState, cfg *config.Config, failures []string) Plan {
	p := AfterVerifyFailed(ts, cfg)
	if len(failures) > 0 {
		p.Reason += "; unmet: " + joinFailures(failures)
	}
	return p
}

func joinFailures(f []string) string {
	out := ""
	for i, s := range f {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// Outcome applies the mechanical downgrade.
//
// A claimed satisfied becomes a failure if any mechanically-checked or
// negative-control criterion failed. Judgment may add failures on top of a
// clean mechanical run; it may never erase one.
func Outcome(r *report.Verifier) (allClear bool, failures []string) {
	for _, c := range r.CriteriaResults {
		if !c.Satisfied() {
			failures = append(failures, describeFailure(c))
		}
	}
	return len(failures) == 0, failures
}

func describeFailure(c report.CriterionResult) string {
	// A criterion can fail with a clean exit code when the negative control
	// found its test cannot fail. Reporting "exited 0" there would tell the
	// next implementer nothing, so the control's reason wins.
	if c.DowngradeReason != "" {
		return fmt.Sprintf("%s (%s)", c.Description, c.DowngradeReason)
	}
	switch c.Evaluation {
	case report.EvalMechanical:
		code := -1
		if c.ExitCode != nil {
			code = *c.ExitCode
		}
		return fmt.Sprintf("%s (check %q exited %d)", c.Description, c.Command, code)
	case report.EvalNegativeControl:
		reason := c.DowngradeReason
		if reason == "" {
			reason = "negative control did not produce discriminating evidence"
		}
		return fmt.Sprintf("%s (%s)", c.Description, reason)
	default:
		notes := c.Notes
		if notes == "" {
			notes = "judged not met"
		}
		return fmt.Sprintf("%s (%s)", c.Description, notes)
	}
}

// ApplyNegativeControl overwrites a criterion with crew's own finding.
//
// The verifier's claim about its own test is provisional; this result is
// authoritative, on the same principle as the exit-code downgrade.
func ApplyNegativeControl(c *report.CriterionResult, res negctl.Result) {
	fails := res.FailsAtMergeBase
	passes := res.PassesAtHead
	c.FailsAtMergeBase = &fails
	c.PassesAtHead = &passes
	c.Classification = string(res.Classification)
	c.RawOutput = res.MergeBaseOutput
	c.NegativeControlStatus = string(res.Classification)

	switch res.Classification {
	case negctl.Discriminating:
		met := true
		c.Met = &met

	case negctl.PassesAtMergeBase:
		// Positive evidence that the test is vacuous, so the criterion fails.
		met := false
		c.Met = &met
		c.DowngradeReason = res.Reason

	default:
		// No evidence either way. The criterion becomes a judged one, with the
		// reason recorded so the captain can see it rests on judgment rather
		// than measurement.
		//
		// Forcing it unmet here would fail every criterion the control cannot
		// evaluate, which is every criterion covering new API surface, and no
		// amount of further work by an implementer could clear it.
		c.Evaluation = report.EvalJudged
		c.DowngradeReason = res.Reason
		if c.Met == nil {
			// A verifier's negative-control entry is provisional and carries
			// no conclusion of its own. What is known is that the test passes
			// at head, which the verifier confirmed; what is unknown is
			// whether it could fail. Record that as judged and met.
			met := res.PassesAtHead
			c.Met = &met
		}
	}
}

// ApplySelfAuthoredControl records crew's finding for a mechanical criterion
// whose test the branch introduced.
//
// The outcomes differ from the verifier-authored case because the failure
// modes mean different things. A verifier writing a test that cannot
// discriminate has produced no evidence, so the criterion falls back to
// judgment. An implementer writing a test that passes with its own
// implementation removed has produced a test that cannot fail, and that is a
// defect in the work rather than an absence of evidence: the criterion is
// unmet and the task goes back for another cycle.
func ApplySelfAuthoredControl(c *report.CriterionResult, res negctl.Result) {
	fails := res.FailsAtMergeBase
	passes := res.PassesAtHead
	c.TestAuthor = string(AuthorBranch)
	c.FailsAtMergeBase = &fails
	c.PassesAtHead = &passes
	c.Classification = string(res.Classification)
	c.RawOutput = res.MergeBaseOutput

	switch res.Classification {
	case negctl.Discriminating:
		// The test genuinely fails without the implementation, so the clean
		// exit code the verifier observed is real evidence.
		met := true
		c.Met = &met

	case negctl.PassesAtMergeBase:
		// The test passes with the implementation taken away, so it asserts
		// nothing about it. Failing the criterion sends the task back with a
		// specific, actionable reason.
		met := false
		c.Met = &met
		c.DowngradeReason = "the test still passes with the implementation reverted, so it does not verify this criterion"

	default:
		// A build failure or an uninterpretable run means the control could
		// not answer. That is the new-API case, and it is also what happens
		// when one test file mixes tests for changed behaviour with tests for
		// new symbols: reverting the implementation removes the symbols, so
		// nothing in the file compiles and no assertion is ever reached.
		//
		// The criterion becomes judged and keeps the verifier's conclusion.
		// Failing it instead would punish work that may be perfectly correct,
		// for a reason no implementer could act on.
		c.Evaluation = report.EvalJudged
		c.DowngradeReason = res.Reason
	}
}

// DowngradeUncontrolled turns every criterion the control run never reached
// into a judged one, and names them.
//
// A control that could not run is not a control that passed. Letting the
// error through left the verifier's provisional claims standing and a task
// reaching ready_for_review advertising "N mechanical / 0 judged" with
// nothing having been controlled at all — the one thing that ratio must never
// be able to say.
//
// The control records a classification or a status on everything it touches,
// so what is left unmarked is exactly what the failure waived. Two kinds are
// deliberately left alone. A criterion already failing on its own exit code
// stays failing: downgrading it to judgment would let the verifier's met
// claim rescue it, turning a missing control into an amnesty. A
// negative-control criterion carries no crew finding either, so it already
// fails of its own accord and needs nothing added.
func DowngradeUncontrolled(r *report.Verifier, cause error) []string {
	var downgraded []string
	for i := range r.CriteriaResults {
		c := &r.CriteriaResults[i]
		switch {
		case c.Evaluation != report.EvalMechanical:
			continue
		case c.Classification != "" || c.NegativeControlStatus != "":
			continue // the run reached this one before it failed
		case c.ExitCode != nil && *c.ExitCode != 0:
			continue // already unsatisfied on its own numbers
		}
		c.Evaluation = report.EvalJudged
		c.NegativeControlStatus = "not_run"
		c.DowngradeReason = "the negative control could not run: " + cause.Error()
		downgraded = append(downgraded, c.Description)
	}
	return downgraded
}

// MarkUnattributable records that a mechanical criterion could not be tied to
// a specific added test, so no control was possible.
//
// The criterion is left standing on its exit code, but the gap is recorded
// rather than hidden: crew review shows it, so the captain can see which
// mechanical results were actually controlled.
func MarkUnattributable(c *report.CriterionResult) {
	c.TestAuthor = string(AuthorUnknown)
	c.NegativeControlStatus = "not_attributable"
}

// MarkPreExisting records that a criterion's test predates the branch, so the
// implementer could not have shaped it to suit its own work.
func MarkPreExisting(c *report.CriterionResult) {
	c.TestAuthor = string(AuthorPreExisting)
	c.NegativeControlStatus = "not_required"
}

// RetagSuggestion returns advice to re-tag a criterion as judged in TASKS.md
// once its negative control has degraded twice.
//
// It is only ever a suggestion shown to the captain. TASKS.md stays
// human-authored intent and crew never edits it.
func RetagSuggestion(ts *state.TaskState, criterion string) string {
	if !ts.ShouldSuggestRetag(criterion) {
		return ""
	}
	return fmt.Sprintf(
		"criterion %q has failed to produce mechanical evidence %d times; consider re-tagging it as `judged: true` in TASKS.md",
		criterion, ts.DegradedCount(criterion))
}
