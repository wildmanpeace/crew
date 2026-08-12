// Package report parses the .crew-report.json a worker writes before it
// exits.
//
// A report is a claim, not evidence. Phase 0 observed a worker describing a
// hook-denied command as having "ran", quoting the denial text as if it were
// output. Everything here therefore validates the claim against the numbers
// the worker also recorded, and crew separately captures exit codes and cost
// from outside the worker.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Filename is the report a worker writes into its worktree root.
const Filename = ".crew-report.json"

// Implementer statuses.
const (
	StatusDone    = "done"
	StatusBlocked = "blocked"
	StatusFailed  = "failed"
)

// Verifier statuses.
const (
	StatusSatisfied    = "satisfied"
	StatusVerifyFailed = "verify_failed"
)

// Criterion evaluation kinds.
const (
	EvalMechanical      = "mechanical_check"
	EvalNegativeControl = "negative_control_test"
	EvalJudged          = "judged"
)

// rejectedError marks a report that parsed but violates a crew rule, as
// distinct from a report that could not be read at all.
type rejectedError struct{ msg string }

func (e *rejectedError) Error() string { return e.msg }

func reject(format string, a ...any) error {
	return &rejectedError{msg: fmt.Sprintf(format, a...)}
}

// IsRejected reports whether err is a rule violation rather than an IO or
// parse failure.
func IsRejected(err error) bool {
	var r *rejectedError
	return errors.As(err, &r)
}

// Check is one command the implementer ran, with the exit code it observed.
type Check struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
}

// Implementer is an implementer's report.
type Implementer struct {
	TaskID    string  `json:"task_id"`
	Role      string  `json:"role"`
	Status    string  `json:"status"`
	Summary   string  `json:"summary"`
	ChecksRun []Check `json:"checks_run"`
}

// CriterionResult is one acceptance criterion's outcome. Fields below the
// verifier's own are crew-authored and overwrite whatever the verifier
// claimed.
type CriterionResult struct {
	Description string `json:"description"`
	Evaluation  string `json:"evaluation"`
	Command     string `json:"command,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Met         *bool  `json:"met,omitempty"`
	TestFile    string `json:"test_file,omitempty"`
	Notes       string `json:"notes,omitempty"`

	// TestAuthor records where the test behind a mechanical criterion came
	// from. A test the branch introduced is evidence only once the negative
	// control has shown it can fail.
	TestAuthor string `json:"test_author,omitempty"`

	NegativeControlStatus string `json:"negative_control_status,omitempty"`
	FailsAtMergeBase      *bool  `json:"fails_at_merge_base,omitempty"`
	PassesAtHead          *bool  `json:"passes_at_head,omitempty"`
	RawOutput             string `json:"raw_output,omitempty"`
	Classification        string `json:"classification,omitempty"`
	DowngradeReason       string `json:"downgrade_reason,omitempty"`
}

// Judged reports whether this criterion rests on judgment rather than a
// mechanical result.
func (c CriterionResult) Judged() bool { return c.Evaluation == EvalJudged }

// Satisfied reports whether the criterion is met. For a mechanical check a
// non-zero exit code fails it regardless of what the verifier claimed:
// judgment can add failures on top of a clean mechanical run, never erase one.
//
// That "never erase" rule is a one-way downgrade, not a substitute for the
// verifier's own claim: exit_code is required by LoadVerifier for a
// mechanical check, but met is not, and Satisfied still requires met to be
// explicitly true regardless of evaluation kind. A clean exit code makes a
// failure impossible; it does not make met optional. So a mechanical check
// that ran clean but never got an explicit "met": true -- an incomplete
// report, not a rejected one -- reads as unsatisfied, the same as an
// explicit "met": false would. That is intentional, not an oversight: this
// package's whole premise is that a report is a claim, not evidence, and the
// safe default for an approval gate is to require the claim rather than to
// infer it from a number the verifier could just as easily have gotten
// wrong. Everywhere crew itself computes met on the verifier's behalf --
// ApplyNegativeControl and ApplySelfAuthoredControl in internal/watch/plan.go
// -- it sets an explicit true or false rather than leaving it nil, so this
// path is only ever exercised by a report that under-claimed.
func (c CriterionResult) Satisfied() bool {
	if c.Evaluation == EvalMechanical && c.ExitCode != nil && *c.ExitCode != 0 {
		return false
	}
	if c.Evaluation == EvalNegativeControl {
		// Only a genuine fail-before/pass-after transition counts as evidence.
		if c.FailsAtMergeBase == nil || c.PassesAtHead == nil {
			return false
		}
		if !*c.FailsAtMergeBase || !*c.PassesAtHead {
			return false
		}
	}
	return c.Met != nil && *c.Met
}

// Verifier is a verifier's report.
type Verifier struct {
	TaskID          string            `json:"task_id"`
	Role            string            `json:"role"`
	Status          string            `json:"status"`
	CriteriaResults []CriterionResult `json:"criteria_results"`
	Notes           string            `json:"notes,omitempty"`
	FinishedAt      time.Time         `json:"finished_at"`
}

// Ratio returns how many criteria rest on mechanical evidence versus
// judgment. It is surfaced to the captain so a review's weight is visible
// before the diff is opened.
func (v *Verifier) Ratio() (mechanical, judged int) {
	for _, c := range v.CriteriaResults {
		if c.Judged() {
			judged++
		} else {
			mechanical++
		}
	}
	return mechanical, judged
}

// RatioString renders Ratio for notifications and crew review.
func (v *Verifier) RatioString() string {
	m, j := v.Ratio()
	return fmt.Sprintf("%d mechanical / %d judged", m, j)
}

// Path returns the report location for a worktree.
func Path(worktree string) string { return filepath.Join(worktree, Filename) }

func readRaw(worktree string) ([]byte, error) {
	raw, err := os.ReadFile(Path(worktree))
	if err != nil {
		// No report is always a failure. Process exit alone is liveness, not
		// content.
		return nil, fmt.Errorf("read %s: %w", Filename, err)
	}
	return raw, nil
}

// LoadImplementer reads and validates an implementer report.
func LoadImplementer(worktree string) (*Implementer, error) {
	return LoadImplementerFor(worktree, "")
}

// LoadImplementerFor additionally requires the report to name wantTaskID.
func LoadImplementerFor(worktree, wantTaskID string) (*Implementer, error) {
	raw, err := readRaw(worktree)
	if err != nil {
		return nil, err
	}
	var r Implementer
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Filename, err)
	}
	if wantTaskID != "" && r.TaskID != wantTaskID {
		return nil, reject("report names task %q, expected %q", r.TaskID, wantTaskID)
	}
	switch r.Status {
	case StatusDone:
		// A claim of done must be backed by checks that actually passed.
		if len(r.ChecksRun) == 0 {
			return nil, reject("status done with no checks_run recorded")
		}
		for _, c := range r.ChecksRun {
			if c.ExitCode != 0 {
				return nil, reject("status done but check %q exited %d", c.Command, c.ExitCode)
			}
		}
	case StatusBlocked, StatusFailed:
		// Not claims of success; no check requirement.
	default:
		return nil, reject("unknown implementer status %q", r.Status)
	}
	return &r, nil
}

// LoadVerifier reads and validates a verifier report.
func LoadVerifier(worktree string) (*Verifier, error) {
	return LoadVerifierWithSuffix(worktree, "")
}

// LoadVerifierWithSuffix additionally requires any named test file to carry
// the configured verify suffix and to sit inside the worktree.
func LoadVerifierWithSuffix(worktree, verifySuffix string) (*Verifier, error) {
	raw, err := readRaw(worktree)
	if err != nil {
		return nil, err
	}
	var r Verifier
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Filename, err)
	}
	switch r.Status {
	case StatusSatisfied:
		if len(r.CriteriaResults) == 0 {
			return nil, reject("status satisfied with no criteria_results")
		}
	case StatusVerifyFailed, StatusBlocked, StatusFailed:
	default:
		return nil, reject("unknown verifier status %q", r.Status)
	}

	for i, c := range r.CriteriaResults {
		if strings.TrimSpace(c.Description) == "" {
			return nil, reject("criterion %d has no description", i)
		}
		switch c.Evaluation {
		case EvalMechanical:
			if strings.TrimSpace(c.Command) == "" {
				return nil, reject("criterion %d is a mechanical check with no command", i)
			}
			if c.ExitCode == nil {
				return nil, reject("criterion %d is a mechanical check with no exit_code", i)
			}
		case EvalNegativeControl:
			if strings.TrimSpace(c.TestFile) == "" {
				return nil, reject("criterion %d is a negative control with no test_file", i)
			}
			if err := validateTestFile(c.TestFile, verifySuffix); err != nil {
				return nil, reject("criterion %d: %v", i, err)
			}
		case EvalJudged:
		default:
			return nil, reject("criterion %d has unknown evaluation %q", i, c.Evaluation)
		}
	}
	return &r, nil
}

// validateTestFile keeps a verifier-named path inside the worktree and
// recognisable as verifier-authored, so crew's negative control and its
// between-cycle cleanup cannot be pointed at implementation code.
func validateTestFile(p, verifySuffix string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("test_file %q must be relative to the worktree", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("test_file %q escapes the worktree", p)
	}
	if verifySuffix != "" && !strings.HasSuffix(clean, verifySuffix) {
		return fmt.Errorf("test_file %q does not carry the %q suffix", p, verifySuffix)
	}
	return nil
}
