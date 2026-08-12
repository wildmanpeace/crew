package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/negctl"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tasks"
	"github.com/wildmanpeace/crew/internal/worker"
)

// Task loads a task's declared intent from TASKS.md.
func (l *Loop) Task(id string) (tasks.Task, error) {
	all, err := tasks.ParseFile(filepath.Join(l.Root, "TASKS.md"))
	if err != nil {
		return tasks.Task{}, err
	}
	t, ok := tasks.ByID(all)[id]
	if !ok {
		return tasks.Task{}, fmt.Errorf("task %q is not declared in TASKS.md", id)
	}
	return t, nil
}

// SpawnWorker starts one worker for a task.
//
// The budget is checked before the spawn, never after: a cap that can only be
// found breached is a report, not a control. The spawn intent, including the
// window name the effect will use, is recorded before the window is created,
// so a crash between the two is repairable.
func (l *Loop) SpawnWorker(taskID string, role config.Role, cycle int, brief string) error {
	budgetUSD, err := l.BudgetFor(taskID)
	if err != nil {
		l.blockTask(taskID, "budget: "+err.Error())
		return nil
	}

	st, err := l.Store.Read()
	if err != nil {
		return err
	}
	ts := st.Tasks[taskID]
	if ts == nil {
		return fmt.Errorf("task %q has no state", taskID)
	}

	runID := worker.RunID(taskID, ts.Attempt, cycle, role)
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)
	settings := filepath.Join(l.Root, ".crew", string(role)+"-settings.json")

	spec := worker.Spec{
		Role:         role,
		TaskID:       taskID,
		Attempt:      ts.Attempt,
		Cycle:        cycle,
		Worktree:     worktree,
		ProjectRoot:  l.Root,
		RunID:        runID,
		Brief:        brief,
		Model:        l.modelFor(role),
		BudgetUSD:    budgetUSD,
		SettingsPath: settings,
	}
	spec.Args = worker.ClaudeArgs(spec, l.Cfg)

	// Intent before effect.
	if _, err := l.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		t.Status = state.StatusSpawning
		t.Role = string(role)
		t.Cycle = cycle
		t.RunID = runID
		t.RunStartedAt = l.now()
		t.Window = tmuxWindow(runID)
		t.PendingIntent = &state.Intent{
			Action: state.IntentSpawnWindow,
			Window: tmuxWindow(runID),
			RunID:  runID,
			At:     l.now(),
		}
		t.UpdatedAt = l.now()
		return nil
	}); err != nil {
		return err
	}

	job := worker.Job{
		Spec:      spec,
		ClaudeBin: l.ClaudeBin,
		RunsDir:   l.runsDir(),
		BasePath:  os.Getenv("PATH"),
	}
	if err := worker.Spawn(job, l.CrewBin, l.Session); err != nil {
		l.clearIntent(taskID, state.StatusQueued)
		return fmt.Errorf("spawn %s for %s: %w", role, taskID, err)
	}

	l.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		t.PendingIntent = nil
		t.Status = state.StatusRunning
		if role == config.RoleVerifier {
			t.Status = state.StatusVerifying
		}
		t.UpdatedAt = l.now()
		return nil
	})
	l.record(state.Event{TaskID: taskID, Kind: "spawned",
		Detail: fmt.Sprintf("%s run %s, cycle %d, budget $%.2f", role, runID, cycle, budgetUSD)})
	return nil
}

func tmuxWindow(runID string) string { return "crew-" + runID }

func (l *Loop) modelFor(role config.Role) string {
	if role == config.RoleVerifier {
		return l.Cfg.VerifierModel
	}
	return l.Cfg.ImplementerModel
}

func (l *Loop) setStatus(taskID string, s state.Status, note string) {
	l.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		if t == nil {
			return nil
		}
		t.Status = s
		t.Notes = note
		t.PendingIntent = nil
		t.UpdatedAt = l.now()
		return nil
	})
}

func (l *Loop) blockTask(taskID, reason string) {
	l.setStatus(taskID, state.StatusBlocked, reason)
	l.emit(state.Event{TaskID: taskID, Kind: "blocked", Detail: reason})
}

// Complete is the completion path for a finished worker. It is invoked only
// after the process has exited, so the report cannot be read mid-write.
func (l *Loop) Complete(taskID string) error {
	st, err := l.Store.Read()
	if err != nil {
		return err
	}
	ts := st.Tasks[taskID]
	if ts == nil {
		return nil
	}
	if err := l.recordSpend(ts); err != nil {
		return err
	}

	// A run that ended by hitting its budget did not fail at the work; it ran
	// out of room. The CLI reports that distinctly, so it is worth
	// distinguishing: "failed" invites a reframe, while the useful response
	// here is to raise a cap or narrow the task.
	if l.hitBudget(ts.RunID) {
		reason := fmt.Sprintf(
			"worker %s stopped at its budget ceiling before finishing; raise per_worker_budget_usd or narrow the task",
			ts.RunID)
		l.blockTask(taskID, reason)
		return nil
	}

	if config.Role(ts.Role) == config.RoleVerifier {
		return l.completeVerifier(taskID)
	}
	return l.completeImplementer(taskID)
}

// hitBudget reports whether a run ended because it exhausted its budget.
func (l *Loop) hitBudget(runID string) bool {
	raw, err := os.ReadFile(worker.RawOutputPath(l.runsDir(), runID))
	if err != nil {
		return false
	}
	env, err := worker.ParseEnvelope(raw)
	if err != nil {
		return false
	}
	return env.BudgetExhausted()
}

func (l *Loop) completeImplementer(taskID string) error {
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)

	r, loadErr := report.LoadImplementerFor(worktree, taskID)
	plan := AfterImplementer(r, loadErr)
	l.record(state.Event{TaskID: taskID, Kind: "implementer_done", Detail: plan.Reason})

	switch plan.Action {
	case ActionSpawnVerifier:
		summary := ""
		if r != nil {
			summary = r.Summary
		}
		l.Store.Update(func(st *state.State) error {
			st.Tasks[taskID].Notes = summary
			return nil
		})
		return l.spawnVerifier(taskID)
	case ActionBlocked:
		l.blockTask(taskID, plan.Reason)
	default:
		l.setStatus(taskID, state.StatusFailed, plan.Reason)
		l.emit(state.Event{TaskID: taskID, Kind: "failed", Detail: plan.Reason})
	}
	return nil
}

func (l *Loop) spawnVerifier(taskID string) error {
	t, err := l.Task(taskID)
	if err != nil {
		l.blockTask(taskID, err.Error())
		return nil
	}
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)

	// The verifier is never shown verifier-authored tests from a prior cycle.
	mergeBase, err := l.Repo.MergeBase(l.Cfg.MainBranch, BranchName(taskID, ts.Attempt))
	if err != nil {
		l.blockTask(taskID, "resolve merge base: "+err.Error())
		return nil
	}
	diff, err := gitx.New(worktree).Diff(mergeBase, "*"+l.Cfg.VerifyTestSuffix)
	if err != nil {
		l.blockTask(taskID, "build diff: "+err.Error())
		return nil
	}
	return l.SpawnWorker(taskID, config.RoleVerifier, ts.Cycle,
		VerifierBrief(t, diff, l.Cfg.VerifyTestSuffix))
}

func (l *Loop) completeVerifier(taskID string) error {
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)

	r, loadErr := report.LoadVerifierWithSuffix(worktree, l.Cfg.VerifyTestSuffix)
	retried := strings.Contains(ts.Notes, verifierRetryMarker)

	if r != nil && loadErr == nil {
		if err := l.runNegativeControls(taskID, r); err != nil {
			// A control that could not run is not a control that passed.
			// Recording the error and carrying on left every provisional claim
			// standing, so a transient git failure silently waived the whole
			// pass and still reported it as mechanical evidence.
			downgraded := DowngradeUncontrolled(r, err)
			l.emit(state.Event{TaskID: taskID, Kind: "negative_control_failed",
				Detail: fmt.Sprintf("%v; %d criterion(s) fall back to judgment",
					err, len(downgraded)),
				Payload: downgraded})
		}
		l.persistCriteria(taskID, r)
	}

	plan := AfterVerifier(ts, l.Cfg, r, loadErr, retried)
	l.record(state.Event{TaskID: taskID, Kind: "verifier_done", Detail: plan.Reason})

	switch plan.Action {
	case ActionReadyForReview:
		head, _ := l.Repo.RevParse(BranchName(taskID, ts.Attempt))
		l.Store.Update(func(st *state.State) error {
			t := st.Tasks[taskID]
			t.Status = state.StatusReadyForReview
			t.HeadSha = head
			t.PendingIntent = nil
			t.UpdatedAt = l.now()
			return nil
		})
		l.emit(state.Event{TaskID: taskID, Kind: "ready_for_review",
			Detail: plan.Reason + l.retagAdvice(taskID, r), Sha: head, Ratio: r.RatioString()})

	case ActionRetryVerifier:
		l.Store.Update(func(st *state.State) error {
			st.Tasks[taskID].Notes = verifierRetryMarker
			return nil
		})
		return l.spawnVerifier(taskID)

	case ActionSpawnImplementer:
		_, failures := Outcome(r)
		return l.nextImplementer(taskID, failures)

	case ActionNeedsReframe:
		l.setStatus(taskID, state.StatusNeedsReframe, plan.Reason)
		l.emit(state.Event{TaskID: taskID, Kind: "needs_reframe", Detail: plan.Reason})

	default:
		l.blockTask(taskID, plan.Reason)
	}
	return nil
}

const verifierRetryMarker = "crew:verifier-retried"

// nextImplementer starts the next cycle, clearing verifier-authored tests
// first so the new implementer neither sees them nor can write to them.
func (l *Loop) nextImplementer(taskID string, failures []string) error {
	t, err := l.Task(taskID)
	if err != nil {
		l.blockTask(taskID, err.Error())
		return nil
	}
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)

	if removed, err := CleanVerifyTests(worktree, l.Cfg.VerifyTestSuffix); err != nil {
		l.record(state.Event{TaskID: taskID, Kind: "watch_error", Detail: err.Error()})
	} else if len(removed) > 0 {
		l.record(state.Event{TaskID: taskID, Kind: "verify_tests_cleaned",
			Detail: strings.Join(removed, ", ")})
	}

	cycle, allowed := NextImplementerCycle(ts, l.Cfg)
	if !allowed {
		l.setStatus(taskID, state.StatusNeedsReframe, "cycle cap reached")
		l.emit(state.Event{TaskID: taskID, Kind: "needs_reframe", Detail: "cycle cap reached"})
		return nil
	}
	return l.SpawnWorker(taskID, config.RoleImplementer, cycle,
		ImplementerBrief(t, cycle, failures))
}

// runNegativeControls replaces every provisional claim about a test with
// crew's own finding.
//
// It covers two cases. The first is a test the verifier wrote, which the spec
// anticipated. The second is a test the *branch* introduced to satisfy a
// check-tagged criterion — in practice, one the implementer wrote alongside
// the implementation it is meant to check. A live run showed that path
// producing a clean "2 mechanical / 1 judged" with nothing having ever asked
// whether those tests could fail, which is the marking-your-own-homework
// problem the control exists to prevent.
func (l *Loop) runNegativeControls(taskID string, r *report.Verifier) error {
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	branch := BranchName(taskID, ts.Attempt)
	worktree := WorktreePath(l.Root, taskID, ts.Attempt)

	mergeBase, err := l.Repo.MergeBase(l.Cfg.MainBranch, branch)
	if err != nil {
		return fmt.Errorf("resolve merge base: %w", err)
	}
	head, err := l.Repo.RevParse(branch)
	if err != nil {
		return fmt.Errorf("resolve branch head: %w", err)
	}
	changedTests, err := ChangedTestFiles(l.Repo, mergeBase, head, l.Cfg.TestFileSuffix)
	if err != nil {
		return fmt.Errorf("list changed tests: %w", err)
	}

	for i := range r.CriteriaResults {
		c := &r.CriteriaResults[i]
		switch c.Evaluation {
		case report.EvalNegativeControl:
			if c.TestFile == "" {
				continue
			}
			if err := l.controlFor(taskID, branch, worktree, c, c.TestFile,
				scopeArgs(c.TestFile), false); err != nil {
				return err
			}

		case report.EvalMechanical:
			target := AttributeCriterion(worktree, c.Command, changedTests)
			switch target.Authorship {
			case AuthorBranch:
				// Run exactly the check the verifier ran, against a tree with
				// the implementation removed.
				if err := l.controlFor(taskID, branch, worktree, c, target.File,
					target.RunArgs, true); err != nil {
					return err
				}
			case AuthorUnknown:
				MarkUnattributable(c)
			default:
				MarkPreExisting(c)
			}
		}
	}
	return nil
}

// controlFor runs one negative control and applies its finding.
//
// The task worktree is passed through because a verifier-authored test lives
// only there: it is never committed, so the control has to be given the file
// rather than expecting to find it on the branch.
func (l *Loop) controlFor(taskID, branch, worktree string, c *report.CriterionResult,
	testFile string, testArgs []string, selfAuthored bool) error {

	argv, err := l.Cfg.Resolve("test", testArgs)
	if err != nil {
		return err
	}
	res, err := negctl.Run(negctl.Params{
		Repo:                l.Repo,
		MainBranch:          l.Cfg.MainBranch,
		Branch:              branch,
		SourceWorktree:      worktree,
		TestFile:            testFile,
		TestArgv:            argv,
		BuildFailureMarkers: l.Cfg.NegativeControlBuildFailureMarkers,
		ScratchDir:          filepath.Join(l.Root, ".crew", "scratch"),
	})
	if err != nil {
		return fmt.Errorf("negative control for %s: %w", testFile, err)
	}

	// Both the raw output and the decision are logged, so an over-broad
	// build-failure marker is tunable rather than silently discarding good
	// evidence.
	l.record(state.Event{TaskID: taskID, Kind: "negative_control",
		Detail: fmt.Sprintf("%s: %s (%s)", testFile, res.Classification, res.Reason),
		Payload: map[string]any{
			"test_file":           testFile,
			"self_authored":       selfAuthored,
			"classification":      string(res.Classification),
			"fails_at_merge_base": res.FailsAtMergeBase,
			"passes_at_head":      res.PassesAtHead,
			"deleted_files":       res.DeletedFiles,
			"reverted_files":      res.RevertedFiles,
			"merge_base_output":   res.MergeBaseOutput,
		}})

	description := c.Description
	if selfAuthored {
		c.TestFile = testFile
		ApplySelfAuthoredControl(c, res)
	} else {
		ApplyNegativeControl(c, res)
	}

	// Any control that produced no evidence is counted, not only one that fell
	// back to judgment. A criterion whose test keeps passing at merge-base
	// stays mechanical on purpose, but repeating that outcome is the strongest
	// signal there is that the criterion cannot be verified mechanically at
	// all: a criterion asserting behaviour is *unchanged* passes at merge-base
	// by construction, and no implementation can ever satisfy it.
	if res.Downgrades() {
		l.Store.Update(func(st *state.State) error {
			st.Tasks[taskID].NoteDegraded(description)
			return nil
		})
	}
	return nil
}

// scopeArgs narrows the test command to the package containing the test, so
// unrelated failures elsewhere cannot be misread as discrimination.
func scopeArgs(testFile string) []string {
	dir := filepath.Dir(testFile)
	if dir == "." || dir == "" {
		return nil
	}
	return []string{"./" + dir + "/..."}
}

func (l *Loop) persistCriteria(taskID string, r *report.Verifier) {
	l.Store.Update(func(st *state.State) error {
		st.Tasks[taskID].UpdatedAt = l.now()
		return nil
	})
	l.record(state.Event{TaskID: taskID, Kind: "criteria_results",
		Ratio: r.RatioString(), Payload: r.CriteriaResults})
}

// retagAdvice appends any re-tag suggestions. They are advice to the captain
// and never an edit: TASKS.md stays human-authored intent.
func (l *Loop) retagAdvice(taskID string, r *report.Verifier) string {
	st, _ := l.Store.Read()
	ts := st.Tasks[taskID]
	if ts == nil {
		return ""
	}
	var out []string
	for _, c := range r.CriteriaResults {
		if s := RetagSuggestion(ts, c.Description); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "\nSuggestions: " + strings.Join(out, "; ")
}

// VerifyNow re-runs a verification pass on demand. It is debug-only: the loop
// decides when to verify in the normal flow.
func (l *Loop) VerifyNow(taskID string) error {
	return l.spawnVerifier(taskID)
}
