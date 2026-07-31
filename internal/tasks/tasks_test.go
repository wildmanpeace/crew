package tasks

import (
	"reflect"
	"strings"
	"testing"
)

// specExample is the TASKS.md block from the design spec, verbatim.
const specExample = "## task: add-rate-limiting\n" +
	"- depends_on: none\n" +
	"- paths: middleware/**, router.go\n" +
	"- brief: >\n" +
	"    Add a token-bucket rate limiter to the public API middleware.\n" +
	"    Config via env var RATE_LIMIT_RPS. Do not touch the auth middleware.\n" +
	"- acceptance_criteria:\n" +
	"    - check: \"crew-check test ./middleware/... -run TestRateLimit429\"\n" +
	"      description: Requests over the configured rate return HTTP 429.\n" +
	"    - check: \"crew-check test ./middleware/... -run TestConfigReload\"\n" +
	"      description: Rate limit is configurable via RATE_LIMIT_RPS without a restart.\n" +
	"    - judged: true\n" +
	"      description: Existing auth middleware behavior is unchanged.\n"

func TestParseSpecExample(t *testing.T) {
	got, err := Parse(strings.NewReader(specExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got))
	}
	task := got[0]
	if task.ID != "add-rate-limiting" {
		t.Errorf("ID = %q", task.ID)
	}
	if len(task.DependsOn) != 0 {
		t.Errorf("DependsOn = %q, want empty for 'none'", task.DependsOn)
	}
	if want := []string{"middleware/**", "router.go"}; !reflect.DeepEqual(task.Paths, want) {
		t.Errorf("Paths = %q, want %q", task.Paths, want)
	}
	if !strings.Contains(task.Brief, "token-bucket rate limiter") {
		t.Errorf("Brief = %q", task.Brief)
	}
	if !strings.Contains(task.Brief, "RATE_LIMIT_RPS") {
		t.Errorf("Brief lost its folded continuation line: %q", task.Brief)
	}
	if len(task.AcceptanceCriteria) != 3 {
		t.Fatalf("got %d criteria, want 3", len(task.AcceptanceCriteria))
	}
	c0 := task.AcceptanceCriteria[0]
	if c0.Check != "crew-check test ./middleware/... -run TestRateLimit429" {
		t.Errorf("criterion 0 Check = %q", c0.Check)
	}
	if c0.Judged {
		t.Error("criterion 0 should not be judged")
	}
	if !c0.IsMechanical() {
		t.Error("criterion 0 should be mechanical")
	}
	c2 := task.AcceptanceCriteria[2]
	if !c2.Judged || c2.Check != "" {
		t.Errorf("criterion 2 = %+v, want judged with no check", c2)
	}
	if c2.IsMechanical() {
		t.Error("criterion 2 should not be mechanical")
	}
}

func TestParseMultipleTasksAndDependsOn(t *testing.T) {
	src := "# Tasks\n\nSome prose that is not a task.\n\n" +
		"## task: alpha\n- brief: do alpha\n- acceptance_criteria:\n    - judged: true\n      description: ok\n\n" +
		"## task: gamma\n- brief: do gamma\n- acceptance_criteria:\n    - judged: true\n      description: ok\n\n" +
		"## task: beta\n- depends_on: alpha, gamma\n- brief: do beta\n" +
		"- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	got, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d tasks, want 3", len(got))
	}
	if got[0].ID != "alpha" || got[1].ID != "gamma" || got[2].ID != "beta" {
		t.Fatalf("ids = %q, %q, %q", got[0].ID, got[1].ID, got[2].ID)
	}
	if want := []string{"alpha", "gamma"}; !reflect.DeepEqual(got[2].DependsOn, want) {
		t.Errorf("beta DependsOn = %q, want %q", got[2].DependsOn, want)
	}
}

// TASKS.md is intent only and must never carry status.
func TestParseRejectsStatusField(t *testing.T) {
	src := "## task: alpha\n- brief: x\n- status: running\n" +
		"- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	_, err := Parse(strings.NewReader(src))
	if err == nil {
		t.Fatal("Parse accepted a status field, want error")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("error = %v, want it to mention status", err)
	}
}

func TestParseRejectsCriterionWithNeitherCheckNorJudged(t *testing.T) {
	src := "## task: alpha\n- brief: x\n- acceptance_criteria:\n    - description: floating\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted an untagged criterion, want error")
	}
}

func TestParseRejectsCriterionWithBothCheckAndJudged(t *testing.T) {
	src := "## task: alpha\n- brief: x\n- acceptance_criteria:\n" +
		"    - check: \"crew-check test ./...\"\n      judged: true\n      description: both\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a criterion tagged both ways, want error")
	}
}

func TestParseRejectsCriterionWithoutDescription(t *testing.T) {
	src := "## task: alpha\n- brief: x\n- acceptance_criteria:\n    - judged: true\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a criterion with no description, want error")
	}
}

func TestParseRejectsDuplicateTaskID(t *testing.T) {
	one := "## task: alpha\n- brief: x\n- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	if _, err := Parse(strings.NewReader(one + one)); err == nil {
		t.Fatal("Parse accepted a duplicate task id, want error")
	}
}

func TestParseRejectsMissingBrief(t *testing.T) {
	src := "## task: alpha\n- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a task with no brief, want error")
	}
}

func TestParseRejectsNoCriteria(t *testing.T) {
	src := "## task: alpha\n- brief: x\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a task with no acceptance criteria, want error")
	}
}

func TestParseRejectsSelfDependency(t *testing.T) {
	src := "## task: alpha\n- depends_on: alpha\n- brief: x\n" +
		"- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a self-dependency, want error")
	}
}

func TestParseRejectsUnknownDependency(t *testing.T) {
	src := "## task: alpha\n- depends_on: ghost\n- brief: x\n" +
		"- acceptance_criteria:\n    - judged: true\n      description: ok\n"
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("Parse accepted a dependency on an undefined task, want error")
	}
}

func TestParseEmptyDocument(t *testing.T) {
	got, err := Parse(strings.NewReader("# Nothing here\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d tasks, want 0", len(got))
	}
}

func TestPathsOverlap(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"middleware/**"}, []string{"middleware/**"}, true},
		{[]string{"middleware/**"}, []string{"middleware/rate.go"}, true},
		{[]string{"middleware/**"}, []string{"router.go"}, false},
		{[]string{"router.go"}, []string{"router.go"}, true},
		{[]string{"a/**"}, []string{"b/**"}, false},
		{nil, []string{"a/**"}, false},
	}
	for _, tc := range cases {
		if got := Overlaps(tc.a, tc.b); got != tc.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestByIDLookup(t *testing.T) {
	got, err := Parse(strings.NewReader(specExample))
	if err != nil {
		t.Fatal(err)
	}
	idx := ByID(got)
	if _, ok := idx["add-rate-limiting"]; !ok {
		t.Fatal("ByID missing task")
	}
	if _, ok := idx["nope"]; ok {
		t.Fatal("ByID invented a task")
	}
}
