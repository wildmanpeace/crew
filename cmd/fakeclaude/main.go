// Command fakeclaude is a stand-in for the claude CLI, used to drive the
// whole loop offline with no API spend.
//
// It is deliberately a real binary rather than a test fixture: the loop's
// edge cases (three cycles, the cap, a verifier crash, the mechanical
// downgrade, crash recovery) are exactly the ones that are impractical to
// provoke against a live model, and they are the ones worth testing.
//
// It takes no direction from its arguments. Everything comes from the
// environment crew already passes a worker, plus a scripted list of steps at
// <CREW_PROJECT_ROOT>/.crew/fake-script.json:
//
//	[
//	  {"cost_usd": 0.10, "exit": 0, "report": { ...implementer report... }},
//	  {"cost_usd": 0.05, "exit": 0, "report": { ...verifier report... }},
//	  {"cost_usd": 0.05, "exit": 1, "no_report": true}
//	]
//
// Steps are consumed in order across the whole run, tracked in
// .crew/fake-step, so a scenario reads top to bottom.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type step struct {
	CostUSD  float64         `json:"cost_usd"`
	Exit     int             `json:"exit"`
	Report   json.RawMessage `json:"report,omitempty"`
	NoReport bool            `json:"no_report,omitempty"`
	Subtype  string          `json:"subtype,omitempty"`
}

func main() {
	root := os.Getenv("CREW_PROJECT_ROOT")
	worktree := os.Getenv("CREW_WORKTREE")
	if root == "" || worktree == "" {
		fmt.Fprintln(os.Stderr, "fakeclaude: CREW_PROJECT_ROOT and CREW_WORKTREE must be set")
		os.Exit(2)
	}

	steps, err := loadSteps(filepath.Join(root, ".crew", "fake-script.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: %v\n", err)
		os.Exit(2)
	}
	idx, err := nextIndex(filepath.Join(root, ".crew", "fake-step"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: %v\n", err)
		os.Exit(2)
	}
	if idx >= len(steps) {
		fmt.Fprintf(os.Stderr, "fakeclaude: script exhausted at step %d\n", idx)
		os.Exit(2)
	}
	s := steps[idx]

	fmt.Fprintf(os.Stderr, "fakeclaude: step %d for %s\n", idx, worktree)

	if !s.NoReport && len(s.Report) > 0 {
		if err := os.WriteFile(filepath.Join(worktree, ".crew-report.json"), s.Report, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: write report: %v\n", err)
			os.Exit(2)
		}
	}

	subtype := s.Subtype
	if subtype == "" {
		subtype = "success"
		if s.Exit != 0 {
			subtype = "error"
		}
	}
	env := map[string]any{
		"type":           "result",
		"subtype":        subtype,
		"is_error":       s.Exit != 0,
		"num_turns":      3,
		"total_cost_usd": s.CostUSD,
		"duration_ms":    1200,
		"session_id":     fmt.Sprintf("fake-%d", idx),
	}
	out, _ := json.Marshal(env)
	fmt.Println(string(out))
	os.Exit(s.Exit)
}

func loadSteps(path string) ([]step, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script: %w", err)
	}
	var steps []step
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("parse script: %w", err)
	}
	return steps, nil
}

// nextIndex reads, increments, and rewrites the step counter.
func nextIndex(path string) (int, error) {
	idx := 0
	if raw, err := os.ReadFile(path); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			idx = n
		}
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(idx+1)+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("write step counter: %w", err)
	}
	return idx, nil
}
