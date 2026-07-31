package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
)

func TestParseEnvelopeSuccess(t *testing.T) {
	e, err := ParseEnvelope([]byte(`{
	  "type":"result","subtype":"success","is_error":false,
	  "num_turns":9,"total_cost_usd":0.16565,"duration_ms":35180,
	  "session_id":"abc","result":"done"
	}`))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	// Phase 0: the field is total_cost_usd, not the spec's cost_usd.
	if e.TotalCostUSD != 0.16565 {
		t.Errorf("TotalCostUSD = %v", e.TotalCostUSD)
	}
	if e.NumTurns != 9 || e.IsError {
		t.Errorf("envelope = %+v", e)
	}
	if e.BudgetExhausted() {
		t.Error("success envelope reported as budget-exhausted")
	}
}

// Phase 0 measured this exact shape when --max-budget-usd was hit.
func TestParseEnvelopeBudgetExhaustion(t *testing.T) {
	e, err := ParseEnvelope([]byte(`{
	  "type":"result","subtype":"error_max_budget_usd","is_error":true,
	  "num_turns":1,"total_cost_usd":0.0355209,"result":null
	}`))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if !e.BudgetExhausted() {
		t.Fatal("budget exhaustion not recognised")
	}
	// The cap is applied after the breaching turn, so spend can exceed it.
	if e.TotalCostUSD <= 0 {
		t.Error("cost not captured from a failed run")
	}
}

func TestParseEnvelopeMalformed(t *testing.T) {
	if _, err := ParseEnvelope([]byte("not json")); err == nil {
		t.Fatal("malformed envelope accepted")
	}
}

// Cost is captured by crew from the CLI's own output, never self-reported.
func TestRunOnceCapturesCostAndExitCode(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeClaude(t, dir, `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"total_cost_usd":0.25,"duration_ms":1000}`, 0)

	runsDir := filepath.Join(dir, "runs")
	res, err := RunOnce(realSpec(t, dir), fake, runsDir, os.Getenv("PATH"), nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d", res.ExitCode)
	}
	if res.TotalCostUSD != 0.25 {
		t.Errorf("TotalCostUSD = %v, want 0.25", res.TotalCostUSD)
	}
}

// The .exit file is the completion signal crew watch polls for; it must be
// written even when the worker fails.
func TestRunOnceWritesExitMarkerOnFailure(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeClaude(t, dir, `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.04}`, 1)

	runsDir := filepath.Join(dir, "runs")
	s := realSpec(t, dir)
	res, err := RunOnce(s, fake, runsDir, os.Getenv("PATH"), nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	if !res.BudgetExhausted {
		t.Error("budget exhaustion not surfaced")
	}
	code, done, err := ReadExit(runsDir, s.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !done || code != 1 {
		t.Fatalf("exit marker = (%d, %v), want (1, true)", code, done)
	}
}

func TestReadExitReportsNotDoneBeforeCompletion(t *testing.T) {
	_, done, err := ReadExit(t.TempDir(), "nope")
	if err != nil {
		t.Fatalf("ReadExit: %v", err)
	}
	if done {
		t.Fatal("a missing exit marker was reported as complete")
	}
}

// The raw CLI output is kept for forensics and cost auditing.
func TestRunOncePersistsRawOutput(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeClaude(t, dir, `{"type":"result","subtype":"success","total_cost_usd":0.1}`, 0)
	runsDir := filepath.Join(dir, "runs")
	s := realSpec(t, dir)
	if _, err := RunOnce(s, fake, runsDir, os.Getenv("PATH"), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(runsDir, s.RunID+".json"))
	if err != nil {
		t.Fatalf("raw output not persisted: %v", err)
	}
	if !strings.Contains(string(raw), "total_cost_usd") {
		t.Errorf("raw output = %s", raw)
	}
}

func TestSettingsDisableAutoMemoryAndExcludeGlobalClaudeMd(t *testing.T) {
	raw, err := Settings("/usr/local/bin/crew", config.RoleVerifier, []string{"/home/u/.claude/CLAUDE.md"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings are not valid JSON: %v", err)
	}
	// Auto memory would carry state between one-shot workers and break their
	// determinism.
	if got["autoMemoryEnabled"] != false {
		t.Errorf("autoMemoryEnabled = %v, want false", got["autoMemoryEnabled"])
	}
	ex, _ := got["claudeMdExcludes"].([]any)
	if len(ex) != 1 || ex[0] != "/home/u/.claude/CLAUDE.md" {
		t.Errorf("claudeMdExcludes = %v", got["claudeMdExcludes"])
	}
}

func TestSettingsWireTheHookToTheCorrectRole(t *testing.T) {
	raw, err := Settings("/usr/local/bin/crew", config.RoleVerifier, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "PreToolUse") {
		t.Fatal("no PreToolUse hook configured")
	}
	if !strings.Contains(body, "hook-gate") || !strings.Contains(body, string(config.RoleVerifier)) {
		t.Fatalf("hook not wired to the verifier role: %s", body)
	}
	if strings.Contains(body, string(config.RoleImplementer)) {
		t.Fatalf("verifier settings mention the implementer role: %s", body)
	}
}

func TestSettingsMatcherIsBash(t *testing.T) {
	raw, _ := Settings("/bin/crew", config.RoleImplementer, nil)
	var got struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	json.Unmarshal(raw, &got)
	if len(got.Hooks.PreToolUse) != 1 || got.Hooks.PreToolUse[0].Matcher != "Bash" {
		t.Fatalf("matcher = %+v", got.Hooks.PreToolUse)
	}
	if got.Hooks.PreToolUse[0].Hooks[0].Type != "command" {
		t.Error("hook type is not command")
	}
}

// realSpec is a Spec whose worktree actually exists on disk, so the run path
// can be exercised for real.
func realSpec(t *testing.T, root string) Spec {
	t.Helper()
	wt := filepath.Join(root, ".crew", "worktrees", "alpha", "attempt-1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	s := spec(config.RoleImplementer)
	s.Worktree = wt
	s.ProjectRoot = root
	return s
}

// writeFakeClaude creates a stub CLI that emits a canned envelope and exits
// with a chosen code, so the run path is testable with no API spend.
func writeFakeClaude(t *testing.T, dir, envelope string, code int) string {
	t.Helper()
	p := filepath.Join(dir, "fake-claude")
	body := "#!/bin/sh\ncat <<'ENVJSON'\n" + envelope + "\nENVJSON\nexit " + itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}
