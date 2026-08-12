package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tmux"
)

// Review prints everything needed to decide on a task, and nothing else.
//
// It is read-only and forms no judgment, which is what makes it safe for the
// first mate to run on the captain's behalf. It exists so the friction of
// finding the sha and the diff by hand does not push the captain into asking
// an agent to summarise them, which would reintroduce exactly the judgment the
// approval gate is built to exclude.
func (a *App) Review(taskID string) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	branch := BranchName(taskID, ts.Attempt)
	head, err := a.Repo.RevParse(branch)
	if err != nil {
		return fmt.Errorf("resolve branch head: %w", err)
	}

	a.out("task:    %s\nstatus:  %s\nattempt: %d (cycle %d)\nbranch:  %s\nhead:    %s\nspend:   $%.2f\n\n",
		taskID, ts.Status, ts.Attempt, ts.Cycle, branch, head, ts.SpendUSD)

	results, ratio, warning := a.lastCriteria(taskID)
	if warning != "" {
		// This is the surface the captain approves from, so a log the reader
		// could not fully trust has to say so here, not just in a log
		// somewhere else no one is looking at.
		a.out("%s\n\n", warning)
	}
	if len(results) == 0 {
		a.out("No criteria results recorded yet.\n\n")
	} else {
		a.out("Criteria (%s):\n", ratio)
		for _, c := range results {
			mark := "FAIL"
			if c.Satisfied() {
				mark = "ok"
			}
			a.out("  [%-4s] %s\n         evaluation: %s\n", mark, c.Description, c.Evaluation)
			if c.Command != "" {
				code := -1
				if c.ExitCode != nil {
					code = *c.ExitCode
				}
				a.out("         command: %s (exit %d)\n", c.Command, code)
			}
			if c.TestFile != "" {
				a.out("         test: %s\n", c.TestFile)
			}
			// Whether a mechanical result was actually controlled is the
			// difference between evidence and a green tick, so it is stated
			// rather than left to be inferred from the ratio.
			switch {
			case c.Classification != "":
				a.out("         negative control: %s (test %s)\n", c.Classification, testAuthorLabel(c.TestAuthor))
			case c.NegativeControlStatus == "not_required":
				a.out("         negative control: not needed, the test predates this branch\n")
			case c.NegativeControlStatus == "not_attributable":
				a.out("         negative control: NOT RUN - could not tie this check to a single new test\n")
			}
			if c.DowngradeReason != "" {
				a.out("         downgraded: %s\n", c.DowngradeReason)
			}
			if c.Notes != "" {
				a.out("         notes: %s\n", c.Notes)
			}
		}
		a.out("\n")
	}

	mergeBase, err := a.Repo.MergeBase(a.Cfg.MainBranch, branch)
	if err != nil {
		return fmt.Errorf("resolve merge base: %w", err)
	}
	diff, err := gitx.New(WorktreePath(a.Root, taskID, ts.Attempt)).
		Diff(mergeBase, "*"+a.Cfg.VerifyTestSuffix)
	if err != nil {
		return fmt.Errorf("build diff: %w", err)
	}
	a.out("Diff against %s (excluding verifier-authored tests):\n\n%s\n", a.Cfg.MainBranch, diff)

	if ts.Status == state.StatusReadyForReview {
		a.out("To approve, run this yourself in a terminal:\n\n    crew approve %s --head %s\n\n", taskID, head)
	} else {
		a.out("This task is %s; approval applies only to ready_for_review.\n", ts.Status)
	}
	return nil
}

// lastCriteria returns the crew-authored criteria results from the event log,
// plus a warning if any line could not be parsed. crew's own findings, not
// the verifier's provisional claims, are what is shown.
//
// A malformed line is surfaced rather than swallowed: this is the captain's
// approval surface, and a corrupt or truncated trailing line (a crash mid-
// append) could be exactly the record that would have superseded what is
// shown, which makes silent staleness the worst possible outcome here. A hard
// failure that shows nothing would be safer still, but it throws away
// results that parsed cleanly for the sake of one that didn't; a loud warning
// next to whatever could still be recovered gives the captain the same
// information without that cost.
func (a *App) lastCriteria(taskID string) ([]report.CriterionResult, string, string) {
	raw, err := os.ReadFile(a.Store.EventsPath())
	if err != nil {
		return nil, "", ""
	}
	var results []report.CriterionResult
	var ratio string
	var malformed int
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			TaskID  string                   `json:"task_id"`
			Kind    string                   `json:"kind"`
			Ratio   string                   `json:"mechanical_vs_judged"`
			Payload []report.CriterionResult `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			malformed++
			continue
		}
		if ev.TaskID == taskID && ev.Kind == "criteria_results" {
			results, ratio = ev.Payload, ev.Ratio
		}
	}
	var warning string
	if malformed > 0 {
		warning = fmt.Sprintf(
			"WARNING: %d line(s) of %s could not be parsed. If one of them was a newer criteria_results record for this task, what follows is STALE.",
			malformed, filepath.Base(a.Store.EventsPath()))
	}
	return results, ratio, warning
}

// StatusReport is the machine-readable form of crew status.
type StatusReport struct {
	Tasks          []TaskStatus `json:"tasks"`
	DailySpendUSD  float64      `json:"daily_spend_usd"`
	DailyCapUSD    float64      `json:"daily_cap_usd"`
	WatchHeartbeat time.Time    `json:"watch_heartbeat"`
	WatchAlive     bool         `json:"watch_alive"`
	WatchStaleFor  string       `json:"watch_stale_for,omitempty"`
}

// TaskStatus is one task's line in crew status.
type TaskStatus struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	Attempt     int      `json:"attempt"`
	Cycle       int      `json:"cycle"`
	SpendUSD    float64  `json:"spend_usd"`
	WindowAlive bool     `json:"window_alive"`
	Suggestions []string `json:"suggestions,omitempty"`
}

// Status reconciles recorded state against ground truth.
func (a *App) Status() (StatusReport, error) {
	st, err := a.Store.Read()
	if err != nil {
		return StatusReport{}, err
	}
	rep := StatusReport{
		DailySpendUSD:  st.DailySpend(a.now(), a.Loc),
		DailyCapUSD:    a.Cfg.ProjectCostCapUSDPerDay,
		WatchHeartbeat: st.WatchHeartbeat,
	}
	// The heartbeat is only meaningful if something external checks it; crew
	// status reports staleness but is not that supervisor.
	stale := a.now().Sub(st.WatchHeartbeat)
	maxStale := time.Duration(a.Cfg.PollIntervalSeconds*4) * time.Second
	rep.WatchAlive = !st.WatchHeartbeat.IsZero() && stale < maxStale
	if !rep.WatchAlive && !st.WatchHeartbeat.IsZero() {
		rep.WatchStaleFor = stale.Round(time.Second).String()
	}

	var ids []string
	for id := range st.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ts := st.Tasks[id]
		alive := false
		if ts.Window != "" {
			alive, _ = tmux.WindowExists(a.Session, ts.Window)
		}
		var suggestions []string
		for crit := range ts.DegradedCounts {
			if ts.ShouldSuggestRetag(crit) {
				suggestions = append(suggestions, fmt.Sprintf(
					"criterion %q failed to produce mechanical evidence %d times; consider re-tagging it `judged: true` in TASKS.md",
					crit, ts.DegradedCount(crit)))
			}
		}
		sort.Strings(suggestions)
		rep.Tasks = append(rep.Tasks, TaskStatus{
			ID: id, Status: string(ts.Status), Attempt: ts.Attempt, Cycle: ts.Cycle,
			SpendUSD: ts.SpendUSD, WindowAlive: alive, Suggestions: suggestions,
		})
	}
	return rep, nil
}

// PrintStatus renders crew status for a terminal.
func (a *App) PrintStatus(asJSON, asMarkdown bool) error {
	rep, err := a.Status()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if asMarkdown {
		a.out("| task | status | attempt | cycle | spend | window |\n|---|---|---|---|---|---|\n")
		for _, t := range rep.Tasks {
			a.out("| %s | %s | %d | %d | $%.2f | %v |\n",
				t.ID, t.Status, t.Attempt, t.Cycle, t.SpendUSD, t.WindowAlive)
		}
	} else {
		if len(rep.Tasks) == 0 {
			a.out("no tasks have been spawned\n")
		}
		for _, t := range rep.Tasks {
			live := ""
			if t.WindowAlive {
				live = " [window live]"
			}
			a.out("%-24s %-16s attempt %d cycle %d  $%.2f%s\n",
				t.ID, t.Status, t.Attempt, t.Cycle, t.SpendUSD, live)
			for _, s := range t.Suggestions {
				a.out("    suggestion: %s\n", s)
			}
		}
	}
	a.out("\nspend today: $%.2f of $%.2f\n", rep.DailySpendUSD, rep.DailyCapUSD)
	switch {
	case rep.WatchHeartbeat.IsZero():
		a.out("crew watch: never seen\n")
	case rep.WatchAlive:
		a.out("crew watch: alive (heartbeat %s)\n", rep.WatchHeartbeat.Format(time.RFC3339))
	default:
		a.out("crew watch: STALE for %s\n", rep.WatchStaleFor)
	}
	return nil
}

// Problem is one mismatch between recorded state and ground truth.
type Problem struct {
	TaskID string
	Detail string
}

// DoctorFindings reconciles TASKS.md, state.json, and what is actually on
// disk. crew spawn refuses while any finding stands.
func (a *App) DoctorFindings() ([]Problem, error) {
	var problems []Problem

	st, err := a.Store.Read()
	if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	if all, err := a.Tasks(); err == nil {
		for _, t := range all {
			declared[t.ID] = true
		}
	} else {
		problems = append(problems, Problem{"", "TASKS.md does not parse: " + err.Error()})
	}

	for id, ts := range st.Tasks {
		if !declared[id] && len(declared) > 0 {
			problems = append(problems, Problem{id, "has state but is not declared in TASKS.md"})
		}
		if ts.PendingIntent != nil {
			// Only promise a repair the loop actually performs. Any finding
			// here refuses every spawn, so naming the wrong remedy leaves the
			// captain waiting on a restart that will not clear it.
			advice := "crew watch repairs this on restart"
			if ts.PendingIntent.Action == state.IntentLand {
				advice = "re-run crew land once the cause is resolved"
			}
			problems = append(problems, Problem{id,
				fmt.Sprintf("unfinished %s intent; %s", ts.PendingIntent.Action, advice)})
		}
		if ts.Window != "" && isActiveStatus(ts.Status) {
			alive, _ := tmux.WindowExists(a.Session, ts.Window)
			if !alive {
				problems = append(problems, Problem{id,
					fmt.Sprintf("status %s but tmux window %s is gone", ts.Status, ts.Window)})
			}
		}
		if ts.Worktree != "" && ts.Status.InFlight() {
			if _, err := os.Stat(ts.Worktree); err != nil {
				problems = append(problems, Problem{id, "worktree is missing: " + ts.Worktree})
			}
		}
	}

	// Worktrees on disk with no owning task.
	worktrees, _ := a.Repo.ListWorktrees()
	for _, wt := range worktrees {
		if !strings.Contains(wt, filepath.Join(".crew", "worktrees")) {
			continue
		}
		owned := false
		for _, ts := range st.Tasks {
			if ts.Worktree == wt {
				owned = true
			}
		}
		if !owned {
			problems = append(problems, Problem{"", "orphaned worktree: " + wt})
		}
	}
	return problems, nil
}

func isActiveStatus(s state.Status) bool {
	return s == state.StatusSpawning || s == state.StatusRunning || s == state.StatusVerifying
}

// Doctor prints the reconciliation findings.
func (a *App) Doctor() error {
	problems, err := a.DoctorFindings()
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		a.out("doctor: no problems found\n")
		return nil
	}
	for _, p := range problems {
		if p.TaskID != "" {
			a.out("%s: %s\n", p.TaskID, p.Detail)
		} else {
			a.out("%s\n", p.Detail)
		}
	}
	return fmt.Errorf("doctor found %d problem(s)", len(problems))
}

// GC removes orphaned worktrees, branches, and windows left by crashes or
// manual teardown. It is the cleanup counterpart to what doctor reports.
func (a *App) GC() error {
	st, err := a.Store.Read()
	if err != nil {
		return err
	}
	a.Repo.PruneWorktrees()

	owned := map[string]bool{}
	windows := map[string]bool{}
	for _, ts := range st.Tasks {
		if ts.Worktree != "" && ts.Status.InFlight() {
			owned[ts.Worktree] = true
		}
		if ts.Window != "" && isActiveStatus(ts.Status) {
			windows[ts.Window] = true
		}
	}

	worktrees, _ := a.Repo.ListWorktrees()
	for _, wt := range worktrees {
		if !strings.Contains(wt, filepath.Join(".crew", "worktrees")) || owned[wt] {
			continue
		}
		if err := a.Repo.RemoveWorktree(wt); err != nil {
			a.out("could not remove %s: %v\n", wt, err)
			continue
		}
		a.out("removed orphaned worktree %s\n", wt)
	}

	live, _ := tmux.ListWindows(a.Session)
	for _, w := range live {
		if strings.HasPrefix(w, "crew-") && !windows[w] {
			tmux.KillWindow(a.Session, w)
			a.out("killed orphaned window %s\n", w)
		}
	}
	a.Repo.PruneWorktrees()
	return nil
}

// Peek prints the tail of a task's pane, for watching a worker without
// interacting with it.
func (a *App) Peek(taskID string, lines int) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	if ts.Window == "" {
		return fmt.Errorf("task %q has no window recorded", taskID)
	}
	out, err := tmux.CapturePane(a.Session, ts.Window, lines)
	if err != nil {
		return fmt.Errorf("no live pane for %s; the worker has finished (its output is in %s): %w",
			taskID, filepath.Join(a.Root, ".crew", "runs"), err)
	}
	a.out("%s", out)
	return nil
}

// Teardown kills a task's window, leaving the worktree and branch in place
// unless explicitly asked otherwise.
func (a *App) Teardown(taskID string, removeWorktree bool) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	if ts.Window != "" {
		tmux.KillWindow(a.Session, ts.Window)
		a.out("killed window %s\n", ts.Window)
	}
	if removeWorktree && ts.Worktree != "" {
		if err := a.Repo.RemoveWorktree(ts.Worktree); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
		a.out("removed worktree %s\n", ts.Worktree)
	}
	if err := a.setStatus(taskID, state.StatusTornDown, "torn down by the captain"); err != nil {
		return fmt.Errorf("tore %s down, but recording that failed: %w", taskID, err)
	}
	a.emit(state.Event{TaskID: taskID, Kind: "torn_down"})
	return nil
}

// testAuthorLabel renders where a controlled test came from.
func testAuthorLabel(author string) string {
	switch author {
	case "branch_added":
		return "written by this branch"
	case "pre_existing":
		return "pre-existing"
	default:
		return "origin unknown"
	}
}
