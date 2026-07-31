package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wildmanpeace/crew/internal/budget"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tasks"
)

// Refusal is a precondition a spawn failed. Each is overridable with --force,
// which is why they are enumerated rather than collapsed into one error.
type Refusal struct {
	Reason string
	Detail string
}

func (r Refusal) String() string { return r.Reason + ": " + r.Detail }

// SpawnChecks evaluates every precondition for spawning a task and returns
// the refusals. It performs no side effects, so crew doctor and crew spawn can
// share it.
func (a *App) SpawnChecks(taskID string) ([]Refusal, error) {
	var out []Refusal

	t, err := a.Task(taskID)
	if err != nil {
		return nil, err
	}
	st, err := a.Store.Read()
	if err != nil {
		return nil, err
	}

	if unmet := DependenciesMet(st, t); len(unmet) > 0 {
		out = append(out, Refusal{"unmet dependencies",
			fmt.Sprintf("%v have not landed", unmet)})
	}

	// Concurrency counts tasks in flight, not workers.
	inFlight := 0
	for id, ts := range st.Tasks {
		if id != taskID && ts.Status.InFlight() {
			inFlight++
		}
	}
	if inFlight >= a.Cfg.ConcurrencyCap {
		out = append(out, Refusal{"concurrency cap reached",
			fmt.Sprintf("%d of %d slots in use", inFlight, a.Cfg.ConcurrencyCap)})
	}

	// The budget is checked before spawning, not discovered after.
	var taskSpent float64
	if ts := st.Tasks[taskID]; ts != nil {
		taskSpent = ts.SpendUSD
	}
	caps := budget.Caps{
		PerWorker: a.Cfg.PerWorkerBudgetUSD, PerTask: a.Cfg.PerTaskCostCapUSD,
		PerDay: a.Cfg.ProjectCostCapUSDPerDay, SafetyMargin: a.Cfg.BudgetSafetyMargin,
		MinSpawn: a.Cfg.MinSpawnBudgetUSD,
	}
	if _, err := budget.ForNextWorker(caps, taskSpent, st.DailySpend(a.now(), a.Loc)); err != nil {
		if errors.Is(err, budget.ErrExhausted) {
			out = append(out, Refusal{"insufficient budget", err.Error()})
		} else {
			return nil, err
		}
	}

	// Two tasks editing overlapping paths would collide in ways neither can
	// see, so an overlap with a task already in flight is refused.
	all, err := a.Tasks()
	if err != nil {
		return nil, err
	}
	for _, other := range all {
		if other.ID == taskID {
			continue
		}
		ots := st.Tasks[other.ID]
		if ots == nil || !ots.Status.InFlight() {
			continue
		}
		if tasks.Overlaps(t.Paths, other.Paths) {
			out = append(out, Refusal{"paths overlap",
				fmt.Sprintf("%q is in flight and declares overlapping paths", other.ID)})
		}
	}

	attempt := 1
	if ts := st.Tasks[taskID]; ts != nil && ts.Attempt > 0 {
		attempt = ts.Attempt
	}
	branch := BranchName(taskID, attempt)
	worktree := WorktreePath(a.Root, taskID, attempt)

	if exists, _ := a.Repo.BranchExists(branch); exists {
		out = append(out, Refusal{"branch already exists",
			fmt.Sprintf("%s is present; reframe rather than reusing an attempt", branch)})
	}
	if _, err := os.Stat(worktree); err == nil {
		out = append(out, Refusal{"worktree already exists", worktree})
	}

	if problems, err := a.DoctorFindings(); err != nil {
		return nil, err
	} else if len(problems) > 0 {
		out = append(out, Refusal{"doctor is failing",
			fmt.Sprintf("%d unreconciled problem(s); run crew doctor", len(problems))})
	}
	return out, nil
}

// Spawn prepares a task's attempt and queues it for the loop to pick up.
//
// crew watch owns spawning workers; this creates the branch and worktree and
// marks the task queued, so there is exactly one place that starts workers.
func (a *App) Spawn(taskID string, force bool) error {
	refusals, err := a.SpawnChecks(taskID)
	if err != nil {
		return err
	}
	if len(refusals) > 0 && !force {
		for _, r := range refusals {
			fmt.Fprintf(a.Stderr, "refusing to spawn %s - %s\n", taskID, r)
		}
		return fmt.Errorf("crew spawn refused %d precondition(s); pass --force to override", len(refusals))
	}
	for _, r := range refusals {
		fmt.Fprintf(a.Stderr, "warning (--force): %s\n", r)
	}

	st, err := a.Store.Read()
	if err != nil {
		return err
	}
	attempt := 1
	if ts := st.Tasks[taskID]; ts != nil && ts.Attempt > 0 {
		attempt = ts.Attempt
	}
	branch := BranchName(taskID, attempt)
	worktree := WorktreePath(a.Root, taskID, attempt)

	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}

	// Intent before effect: the branch and worktree are recorded before they
	// are created, so a crash between the two is detectable.
	if _, err := a.Store.Update(func(st *state.State) error {
		ts := st.Tasks[taskID]
		if ts == nil {
			ts = &state.TaskState{ID: taskID}
			st.Upsert(ts)
		}
		ts.Attempt = attempt
		ts.Cycle = 0
		ts.Branch = branch
		ts.Worktree = worktree
		ts.PendingIntent = &state.Intent{
			Action: state.IntentCreateWorktr, Branch: branch, Worktree: worktree, At: a.now(),
		}
		ts.UpdatedAt = a.now()
		return nil
	}); err != nil {
		return err
	}

	if err := a.Repo.AddWorktreeBranch(worktree, branch, a.Cfg.MainBranch); err != nil {
		a.Store.Update(func(st *state.State) error {
			st.Tasks[taskID].PendingIntent = nil
			return nil
		})
		return fmt.Errorf("create worktree for %s: %w", taskID, err)
	}

	if _, err := a.Store.Update(func(st *state.State) error {
		ts := st.Tasks[taskID]
		ts.PendingIntent = nil
		ts.Status = state.StatusQueued
		ts.UpdatedAt = a.now()
		return nil
	}); err != nil {
		return err
	}
	a.emit(state.Event{TaskID: taskID, Kind: "queued",
		Detail: fmt.Sprintf("attempt %d on %s", attempt, branch)})
	a.out("queued %s (attempt %d, branch %s)\ncrew watch will start the first implementer.\n",
		taskID, attempt, branch)
	return nil
}
