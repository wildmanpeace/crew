package report

import (
	"os"
	"path/filepath"
	"testing"
)

func writeReport(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// No report file is always a failure, never a silent success.
func TestLoadImplementerMissingFileIsFailure(t *testing.T) {
	_, err := LoadImplementer(t.TempDir())
	if err == nil {
		t.Fatal("missing report accepted, want error")
	}
}

func TestLoadImplementerMalformedJSONIsFailure(t *testing.T) {
	dir := writeReport(t, `{"task_id": "alpha", `)
	if _, err := LoadImplementer(dir); err == nil {
		t.Fatal("malformed report accepted, want error")
	}
}

func TestLoadImplementerDone(t *testing.T) {
	dir := writeReport(t, `{
	  "task_id": "alpha",
	  "role": "implementer",
	  "status": "done",
	  "summary": "added the limiter",
	  "checks_run": [
	    {"command": "crew-run test", "exit_code": 0},
	    {"command": "crew-run build", "exit_code": 0}
	  ]
	}`)
	r, err := LoadImplementer(dir)
	if err != nil {
		t.Fatalf("LoadImplementer: %v", err)
	}
	if r.Status != StatusDone {
		t.Errorf("Status = %q", r.Status)
	}
	if len(r.ChecksRun) != 2 {
		t.Errorf("ChecksRun = %d", len(r.ChecksRun))
	}
}

// crew rejects a claimed done when any recorded check exited non-zero.
func TestDoneIsRejectedOnNonZeroCheck(t *testing.T) {
	dir := writeReport(t, `{
	  "task_id": "alpha", "role": "implementer", "status": "done",
	  "checks_run": [
	    {"command": "crew-run test", "exit_code": 0},
	    {"command": "crew-run lint", "exit_code": 1}
	  ]
	}`)
	_, err := LoadImplementer(dir)
	if err == nil {
		t.Fatal("done accepted with a failing check, want rejection")
	}
	if !IsRejected(err) {
		t.Errorf("error = %v, want a rejection", err)
	}
}

// A worker cannot claim done without having run anything.
func TestDoneIsRejectedWithNoChecks(t *testing.T) {
	dir := writeReport(t, `{"task_id":"alpha","role":"implementer","status":"done","checks_run":[]}`)
	if _, err := LoadImplementer(dir); err == nil {
		t.Fatal("done accepted with no checks, want rejection")
	}
}

// blocked and failed are legitimate outcomes and are not subject to the
// check-exit rule; they are not claims of success.
func TestBlockedIsAcceptedWithoutChecks(t *testing.T) {
	dir := writeReport(t, `{"task_id":"alpha","role":"implementer","status":"blocked","summary":"needs a decision"}`)
	r, err := LoadImplementer(dir)
	if err != nil {
		t.Fatalf("LoadImplementer: %v", err)
	}
	if r.Status != StatusBlocked {
		t.Errorf("Status = %q", r.Status)
	}
}

func TestUnknownImplementerStatusIsRejected(t *testing.T) {
	dir := writeReport(t, `{"task_id":"alpha","role":"implementer","status":"satisfied"}`)
	if _, err := LoadImplementer(dir); err == nil {
		t.Fatal("unknown status accepted, want error")
	}
}

func TestTaskIDMismatchIsRejected(t *testing.T) {
	dir := writeReport(t, `{"task_id":"beta","role":"implementer","status":"blocked"}`)
	if _, err := LoadImplementerFor(dir, "alpha"); err == nil {
		t.Fatal("report for another task accepted, want error")
	}
}

func TestLoadVerifierSpecExample(t *testing.T) {
	dir := writeReport(t, `{
	  "task_id": "add-rate-limiting",
	  "role": "verifier",
	  "status": "satisfied",
	  "criteria_results": [
	    {
	      "description": "Requests over the configured rate return HTTP 429.",
	      "evaluation": "mechanical_check",
	      "command": "crew-check test ./middleware/... -run TestRateLimit429",
	      "exit_code": 0,
	      "met": true
	    },
	    {
	      "description": "Rate limit is configurable via RATE_LIMIT_RPS without a restart.",
	      "evaluation": "negative_control_test",
	      "test_file": "middleware/reload_crewverify_test.go",
	      "negative_control_status": "pending_crew_evaluation"
	    },
	    {
	      "description": "Existing auth middleware behavior is unchanged.",
	      "evaluation": "judged",
	      "met": true,
	      "notes": "Diff touches no files under middleware/auth/."
	    }
	  ],
	  "finished_at": "2026-07-21T14:35:00Z"
	}`)
	r, err := LoadVerifier(dir)
	if err != nil {
		t.Fatalf("LoadVerifier: %v", err)
	}
	if len(r.CriteriaResults) != 3 {
		t.Fatalf("got %d criteria, want 3", len(r.CriteriaResults))
	}
	if r.CriteriaResults[0].Evaluation != EvalMechanical {
		t.Errorf("evaluation = %q", r.CriteriaResults[0].Evaluation)
	}
	if r.CriteriaResults[1].TestFile == "" {
		t.Error("negative control entry lost its test file")
	}
	if !r.CriteriaResults[2].Judged() {
		t.Error("third criterion should be judged")
	}
	if r.FinishedAt.IsZero() {
		t.Error("finished_at not parsed")
	}
}

func TestVerifierUnknownEvaluationIsRejected(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[{"description":"d","evaluation":"vibes","met":true}]}`)
	if _, err := LoadVerifier(dir); err == nil {
		t.Fatal("unknown evaluation accepted, want error")
	}
}

func TestVerifierSatisfiedRequiresCriteria(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied","criteria_results":[]}`)
	if _, err := LoadVerifier(dir); err == nil {
		t.Fatal("satisfied accepted with no criteria, want error")
	}
}

func TestMechanicalCriterionRequiresCommandAndExitCode(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[{"description":"d","evaluation":"mechanical_check","met":true}]}`)
	if _, err := LoadVerifier(dir); err == nil {
		t.Fatal("mechanical criterion without command/exit_code accepted, want error")
	}
}

func TestNegativeControlCriterionRequiresTestFile(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[{"description":"d","evaluation":"negative_control_test"}]}`)
	if _, err := LoadVerifier(dir); err == nil {
		t.Fatal("negative control criterion without test_file accepted, want error")
	}
}

// Judgment may add failures on top of a clean mechanical run; it may never
// erase one. A non-zero exit code fails the criterion whatever "met" says.
func TestMechanicalExitCodeOverridesClaimedMet(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[
	    {"description":"d","evaluation":"mechanical_check","command":"crew-check test","exit_code":1,"met":true}
	  ]}`)
	r, err := LoadVerifier(dir)
	if err != nil {
		t.Fatalf("LoadVerifier: %v", err)
	}
	c := r.CriteriaResults[0]
	if c.Satisfied() {
		t.Fatal("criterion with a non-zero exit code reported as satisfied")
	}
}

func TestMechanicalZeroExitWithMetFalseStillFails(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[
	    {"description":"d","evaluation":"mechanical_check","command":"crew-check test","exit_code":0,"met":false}
	  ]}`)
	r, _ := LoadVerifier(dir)
	if r.CriteriaResults[0].Satisfied() {
		t.Fatal("criterion explicitly marked not met reported as satisfied")
	}
}

func TestVerifierTestFileMustCarryTheVerifySuffix(t *testing.T) {
	dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
	  "criteria_results":[{"description":"d","evaluation":"negative_control_test","test_file":"middleware/sneaky.go"}]}`)
	if _, err := LoadVerifierWithSuffix(dir, "_crewverify_test.go"); err == nil {
		t.Fatal("non-verify test file accepted, want error")
	}
}

func TestVerifierTestFileMustBeRelativeAndContained(t *testing.T) {
	for _, bad := range []string{"/etc/x_crewverify_test.go", "../x_crewverify_test.go"} {
		dir := writeReport(t, `{"task_id":"a","role":"verifier","status":"satisfied",
		  "criteria_results":[{"description":"d","evaluation":"negative_control_test","test_file":"`+bad+`"}]}`)
		if _, err := LoadVerifierWithSuffix(dir, "_crewverify_test.go"); err == nil {
			t.Errorf("test_file %q accepted, want error", bad)
		}
	}
}

func TestRatioCountsMechanicalVersusJudged(t *testing.T) {
	r := &Verifier{CriteriaResults: []CriterionResult{
		{Evaluation: EvalMechanical},
		{Evaluation: EvalNegativeControl},
		{Evaluation: EvalJudged},
	}}
	mech, judged := r.Ratio()
	if mech != 2 || judged != 1 {
		t.Fatalf("Ratio = %d/%d, want 2/1", mech, judged)
	}
	if got := r.RatioString(); got != "2 mechanical / 1 judged" {
		t.Errorf("RatioString = %q", got)
	}
}
