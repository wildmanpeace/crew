package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
)

// A crash that truncates the trailing line of events.jsonl must never let
// crew review present stale or empty criteria as if nothing were wrong: this
// is the captain's approval surface, so a malformed line has to be surfaced
// rather than silently skipped. The last well-formed record is still shown,
// because a hard failure that shows nothing is worse than a visible warning
// next to what could still be parsed.
func TestLastCriteriaWarnsOnAMalformedLine(t *testing.T) {
	a, _ := fixture(t)

	met := true
	if err := a.Store.Append(state.Event{
		TaskID: "alpha", Kind: "criteria_results", Ratio: "1 mechanical / 0 judged",
		Payload: []report.CriterionResult{
			{Description: "it works", Evaluation: report.EvalJudged, Met: &met},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-append: a truncated trailing line that would have
	// superseded the record above.
	f, err := os.OpenFile(a.Store.EventsPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"task_id":"alpha","kind":"criteria_resul`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	results, _, warning := a.lastCriteria("alpha")
	if warning == "" {
		t.Fatal("malformed trailing line was silently skipped, want a warning")
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want the last well-formed record still shown", len(results))
	}
}

// A well-formed log with nothing malformed in it must not carry a warning:
// otherwise the captain learns to ignore it.
func TestLastCriteriaNoWarningWhenTheLogIsClean(t *testing.T) {
	a, _ := fixture(t)

	met := true
	if err := a.Store.Append(state.Event{
		TaskID: "alpha", Kind: "criteria_results", Ratio: "1 mechanical / 0 judged",
		Payload: []report.CriterionResult{
			{Description: "it works", Evaluation: report.EvalJudged, Met: &met},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, _, warning := a.lastCriteria("alpha")
	if warning != "" {
		t.Fatalf("clean event log produced a warning: %q", warning)
	}
}

// crew review is the surface the captain approves from, so the warning has to
// reach it, not just the internal helper.
func TestReviewPrintsTheMalformedEventWarning(t *testing.T) {
	a, out := fixture(t)
	readyTask(t, a, "alpha")

	f, err := os.OpenFile(a.Store.EventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json at all\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := a.Review("alpha"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("review did not surface the malformed event warning:\n%s", out.String())
	}
}
