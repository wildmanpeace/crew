package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wildmanpeace/crew/internal/tmux"
)

// Job is everything the in-window runner needs. crew writes it to disk before
// creating the window, so the window's command line stays short and the exact
// invocation is auditable after the fact.
type Job struct {
	Spec      Spec   `json:"spec"`
	ClaudeBin string `json:"claude_bin"`
	RunsDir   string `json:"runs_dir"`
	BasePath  string `json:"base_path"`
}

// JobPath is where a run's job description lives.
func JobPath(runsDir, runID string) string {
	return filepath.Join(runsDir, runID+".job.json")
}

// WriteJob persists a job description.
func WriteJob(j Job) error {
	if err := os.MkdirAll(j.RunsDir, 0o755); err != nil {
		return fmt.Errorf("create runs dir: %w", err)
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job: %w", err)
	}
	return writeAtomic(JobPath(j.RunsDir, j.Spec.RunID), append(raw, '\n'))
}

// ReadJob loads a job description.
func ReadJob(path string) (Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Job{}, fmt.Errorf("read job: %w", err)
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return Job{}, fmt.Errorf("parse job: %w", err)
	}
	return j, nil
}

// Spawn writes the job and starts it in a tmux window.
//
// The caller records its intent, including the window name, before calling
// this, so a crash between the record and the window's creation is detectable
// on restart.
func Spawn(j Job, crewBin, session string) error {
	if err := WriteJob(j); err != nil {
		return err
	}
	if err := tmux.EnsureSession(session); err != nil {
		return fmt.Errorf("ensure tmux session: %w", err)
	}
	window := tmux.WindowName(j.Spec.RunID)
	argv := []string{crewBin, "worker", "--job", JobPath(j.RunsDir, j.Spec.RunID)}
	if err := tmux.NewWindow(session, window, j.Spec.Worktree, argv, nil); err != nil {
		return fmt.Errorf("start worker window: %w", err)
	}
	return nil
}

// RunJob executes a job. It is what runs inside the tmux window.
func RunJob(j Job) (RunResult, error) {
	return RunOnce(j.Spec, j.ClaudeBin, j.RunsDir, j.BasePath, nil)
}
