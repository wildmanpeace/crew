package watch

import (
	"fmt"
	"strings"

	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/tasks"
)

// ImplementerBrief builds the implementer's prompt.
//
// When a previous cycle failed verification, the specific unmet criteria are
// included so the next attempt is aimed rather than blind.
func ImplementerBrief(t tasks.Task, cycle int, previousFailures []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are implementing task %q (cycle %d).\n\n", t.ID, cycle)
	fmt.Fprintf(&b, "## Brief\n%s\n\n", t.Brief)

	if len(t.Paths) > 0 {
		fmt.Fprintf(&b, "## Files you may change\n%s\n\n", strings.Join(t.Paths, ", "))
	}

	b.WriteString("## Acceptance criteria\n")
	for i, c := range t.AcceptanceCriteria {
		if c.IsMechanical() {
			fmt.Fprintf(&b, "%d. %s\n   Verified by: %s\n", i+1, c.Description, c.Check)
		} else {
			fmt.Fprintf(&b, "%d. %s\n   Verified by judgment.\n", i+1, c.Description)
		}
	}

	if len(previousFailures) > 0 {
		b.WriteString("\n## A previous attempt failed verification on\n")
		for _, f := range previousFailures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\nAddress these specifically.\n")
	}

	b.WriteString(`
## How to work
Your only shell access is the crew-run dispatcher. It exposes exactly these
verbs and nothing else:

    crew-run test [args...]
    crew-run lint [args...]
    crew-run build [args...]
    crew-run diff
    crew-run commit "<message>"

Read and edit files with the Read and Edit tools, not through the shell.

Before you finish you must run your checks with crew-run and commit your work.
Then write ` + report.Filename + ` in the worktree root:

    {
      "task_id": "` + t.ID + `",
      "role": "implementer",
      "status": "done",
      "summary": "<what you changed and why>",
      "checks_run": [
        {"command": "crew-run test", "exit_code": 0}
      ]
    }

Record every check you ran with its real exit code. A status of "done" is
rejected if any recorded check exited non-zero or if no checks were recorded,
so do not claim done until they genuinely pass. Use "blocked" if you need a
decision only the captain can make, or "failed" if you cannot proceed.
`)
	return b.String()
}

// VerifierBrief builds the verifier's prompt.
//
// It is assembled from the original brief, the tagged criteria, and the diff.
// The implementer's summary is deliberately absent: a verifier that reads the
// implementer's account of its own work is checking the account, not the code.
func VerifierBrief(t tasks.Task, diff string, verifySuffix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are verifying task %q.\n\n", t.ID)
	fmt.Fprintf(&b, "## Original brief\n%s\n\n", t.Brief)

	b.WriteString("## Acceptance criteria to evaluate\n")
	for i, c := range t.AcceptanceCriteria {
		if c.IsMechanical() {
			fmt.Fprintf(&b, "%d. %s\n   Run this check: %s\n", i+1, c.Description, c.Check)
		} else {
			fmt.Fprintf(&b, "%d. %s\n   Tagged for judgment.\n", i+1, c.Description)
		}
	}

	fmt.Fprintf(&b, "\n## Diff under review\n```diff\n%s\n```\n", diff)

	fmt.Fprintf(&b, `
## How to verify
Your only shell access is the crew-check dispatcher:

    crew-check test [args...]
    crew-check lint [args...]
    crew-check build [args...]

There is no commit and no diff verb. You cannot modify implementation code;
the only files you may write are new tests ending in %s and your own report.

For each criterion:

- If it names a check command, run it with crew-check and record the real exit
  code. A non-zero exit fails the criterion regardless of your opinion.
- If it has no check and one could exist, write a new test ending in %s beside
  the code it exercises, confirm it passes now, and name the file. Do not try
  to evaluate whether it would fail without the implementation: crew performs
  that control itself, and your report's claim about it is provisional.
- If it is tagged for judgment, state your judgment and mark it judged.

Then write %s in the worktree root:

    {
      "task_id": "%s",
      "role": "verifier",
      "status": "satisfied",
      "criteria_results": [
        {"description": "...", "evaluation": "mechanical_check",
         "command": "crew-check test ./...", "exit_code": 0, "met": true},
        {"description": "...", "evaluation": "negative_control_test",
         "test_file": "pkg/thing%s",
         "negative_control_status": "pending_crew_evaluation"},
        {"description": "...", "evaluation": "judged", "met": true,
         "notes": "<your reasoning>"}
      ],
      "finished_at": "<RFC3339 timestamp>"
    }

Use "verify_failed" if any criterion is unmet, or "blocked" if you cannot
evaluate the task at all.
`, verifySuffix, verifySuffix, report.Filename, t.ID, verifySuffix)
	return b.String()
}
