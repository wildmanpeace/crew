package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
)

// Envelope is the result object `claude -p --output-format json` prints.
type Envelope struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int     `json:"duration_ms"`
	SessionID    string  `json:"session_id"`
}

// SubtypeBudgetExhausted is what the CLI reports when --max-budget-usd is
// reached. It is distinguishable from a generic failure, so crew can classify
// it as budget-blocked rather than failed.
const SubtypeBudgetExhausted = "error_max_budget_usd"

// BudgetExhausted reports whether the run ended by hitting its budget.
func (e Envelope) BudgetExhausted() bool { return e.Subtype == SubtypeBudgetExhausted }

// ParseEnvelope reads the CLI's result object.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &e); err != nil {
		return Envelope{}, fmt.Errorf("parse claude output: %w", err)
	}
	return e, nil
}

// RunResult is what crew records about one worker invocation. Every field is
// observed by crew from outside the worker; none of it is self-reported.
type RunResult struct {
	RunID           string
	ExitCode        int
	TotalCostUSD    float64
	NumTurns        int
	Subtype         string
	IsError         bool
	BudgetExhausted bool
	DurationMS      int
}

// RunOnce invokes claude for one worker, persists the raw output, and writes
// the completion marker.
//
// Process exit is the liveness signal and the report file is the content
// signal. Because crew only reads the report after the process has exited, a
// torn read is not possible.
func RunOnce(s Spec, claudeBin, runsDir, basePath string, extraEnv []string) (RunResult, error) {
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return RunResult{}, fmt.Errorf("create runs dir: %w", err)
	}
	if basePath == "" {
		basePath = os.Getenv("PATH")
	}

	res := RunResult{RunID: s.RunID}
	var out bytes.Buffer

	cmd := exec.Command(claudeBin, s.Args...)
	cmd.Dir = s.Worktree
	cmd.Env = append(Env(s, basePath), extraEnv...)
	// Tee to stdout so the tmux window shows the live transcript, while the
	// buffer is what crew actually parses.
	cmd.Stdout = io.MultiWriter(&out, os.Stdout)
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
	switch {
	case runErr == nil:
		res.ExitCode = 0
	default:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return res, fmt.Errorf("exec claude: %w", runErr)
		}
	}

	raw := out.Bytes()
	if err := writeAtomic(filepath.Join(runsDir, s.RunID+".json"), raw); err != nil {
		return res, err
	}

	if env, err := ParseEnvelope(raw); err == nil {
		res.TotalCostUSD = env.TotalCostUSD
		res.NumTurns = env.NumTurns
		res.Subtype = env.Subtype
		res.IsError = env.IsError
		res.DurationMS = env.DurationMS
		res.BudgetExhausted = env.BudgetExhausted()
	}

	// The exit marker is written last: its presence means everything else is
	// already on disk, so crew watch can treat it as the single completion
	// signal.
	if err := writeAtomic(filepath.Join(runsDir, s.RunID+".exit"),
		[]byte(strconv.Itoa(res.ExitCode)+"\n")); err != nil {
		return res, err
	}
	return res, nil
}

// ReadExit reports whether a run has completed and with what code.
func ReadExit(runsDir, runID string) (code int, done bool, err error) {
	raw, err := os.ReadFile(filepath.Join(runsDir, runID+".exit"))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read exit marker: %w", err)
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false, fmt.Errorf("parse exit marker: %w", err)
	}
	return code, true, nil
}

// RawOutputPath is where a run's CLI output is kept.
func RawOutputPath(runsDir, runID string) string {
	return filepath.Join(runsDir, runID+".json")
}

func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}

// Settings renders the worker settings file.
//
// It is written outside every worktree, so no worker has a permission rule
// that would let it edit its own guardrails.
func Settings(crewBin string, role config.Role, claudeMdExcludes []string) ([]byte, error) {
	type hookEntry struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string      `json:"matcher"`
		Hooks   []hookEntry `json:"hooks"`
	}
	doc := map[string]any{
		// One-shot workers must not accumulate state between runs.
		"autoMemoryEnabled": false,
		"hooks": map[string]any{
			"PreToolUse": []matcher{{
				Matcher: "Bash",
				Hooks: []hookEntry{{
					Type:    "command",
					Command: fmt.Sprintf("%s hook-gate --role %s", crewBin, role),
				}},
			}},
		},
	}
	if len(claudeMdExcludes) > 0 {
		doc["claudeMdExcludes"] = claudeMdExcludes
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode worker settings: %w", err)
	}
	return append(raw, '\n'), nil
}
