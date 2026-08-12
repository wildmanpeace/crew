package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tasks"
)

// ErrNotTTY is returned when a captain-only command is invoked without a
// terminal.
var ErrNotTTY = fmt.Errorf("crew approve requires an interactive terminal")

// Approve records the captain's approval of a specific branch head.
//
// Two structural gates: it refuses without a terminal, so an agent session
// cannot invoke it, and it refuses unless the supplied sha is the branch's
// current head, so an approval always names exactly the code that was read.
func (a *App) Approve(taskID, sha string) error {
	if !a.tty() {
		return ErrNotTTY
	}
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("crew approve requires --head <sha>")
	}
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	if ts.Status != state.StatusReadyForReview {
		return fmt.Errorf("task %q is %s, not ready_for_review", taskID, ts.Status)
	}
	// Review and verify read the worktree; approval binds the committed head.
	// Uncommitted edits are therefore reviewed and then dropped at land time,
	// so they are refused here rather than approved and silently lost.
	dirty, err := a.dirtyWorktreePaths(taskID, ts.Attempt)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf(
			"task %q has uncommitted changes in its worktree (%s); approval binds the committed head, so those edits would not land - commit them and review again",
			taskID, strings.Join(dirty, ", "))
	}
	head, err := a.Repo.RevParse(BranchName(taskID, ts.Attempt))
	if err != nil {
		return fmt.Errorf("resolve branch head: %w", err)
	}
	if !shaMatches(head, sha) {
		return fmt.Errorf("branch head is %s, not %s; the branch moved since you reviewed it", short(head), short(sha))
	}

	if _, err := a.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		t.Status = state.StatusApproved
		t.ApprovedSha = head
		t.HeadSha = head
		t.UpdatedAt = a.now()
		return nil
	}); err != nil {
		return err
	}
	a.emit(state.Event{TaskID: taskID, Kind: "approved", Sha: head})
	a.out("approved %s at %s\n", taskID, short(head))
	return nil
}

// dirtyWorktreePaths lists a task attempt's uncommitted paths. A worktree
// that is no longer on disk has no uncommitted work to lose, so it is not an
// error here.
func (a *App) dirtyWorktreePaths(taskID string, attempt int) ([]string, error) {
	worktree := WorktreePath(a.Root, taskID, attempt)
	if _, err := os.Stat(worktree); err != nil {
		return nil, nil
	}
	dirty, err := gitx.New(worktree).DirtyPaths()
	if err != nil {
		return nil, fmt.Errorf("check %s worktree: %w", taskID, err)
	}
	return dirty, nil
}

// shaMatches accepts an abbreviated sha as long as it prefixes the full one.
func shaMatches(full, given string) bool {
	given = strings.TrimSpace(given)
	if len(given) < 7 {
		return false
	}
	return strings.HasPrefix(full, given)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Land merges an approved task into main.
//
// The approved sha is re-checked here, not only at approval time. That closes
// the window between approving and landing: a rebase, an extra cycle, or any
// other move that rewrote the branch invalidates the approval, and the merge
// is refused rather than silently landing code nobody read.
//
// The merge happens in a throwaway worktree so a conflict can never leave the
// real main in a conflicted state; only a clean result is fast-forwarded in.
func (a *App) Land(taskID string) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	if ts.Status != state.StatusApproved {
		return fmt.Errorf("task %q is %s; only an approved task can land", taskID, ts.Status)
	}

	branch := BranchName(taskID, ts.Attempt)
	head, err := a.Repo.RevParse(branch)
	if err != nil {
		return fmt.Errorf("resolve branch head: %w", err)
	}
	if head != ts.ApprovedSha {
		return fmt.Errorf(
			"branch head is %s but %s was approved; the branch moved after approval, so it must be reviewed and approved again",
			short(head), short(ts.ApprovedSha))
	}

	// The fast-forward below moves whatever the root repo has checked out, so
	// landing with another ref checked out would advance that ref and leave
	// main - the branch dependents are cut from - untouched.
	current, err := a.Repo.CurrentBranch()
	if err != nil {
		return fmt.Errorf("resolve the checked-out branch: %w", err)
	}
	if current != a.Cfg.MainBranch {
		return fmt.Errorf("the repository has %s checked out, not %s; check out %s before landing",
			current, a.Cfg.MainBranch, a.Cfg.MainBranch)
	}

	// crew's own worktrees and scratch space live under .crew inside the repo,
	// so they are excluded: they are never part of what lands.
	clean, err := a.Repo.IsCleanExcluding(".crew")
	if err != nil {
		return err
	}
	if !clean {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them before landing", a.Cfg.MainBranch)
	}

	// The approved sha is merged, not the branch: the ref could have moved
	// since it was resolved above, and only what was approved may land.
	merged, mergeErr := a.mergeInScratch(branch, ts.ApprovedSha)
	if mergeErr != nil {
		if err := a.setStatus(taskID, state.StatusLandConflict, mergeErr.Error()); err != nil {
			return fmt.Errorf("landing %s conflicts with %s: %v; and recording that failed: %w",
				taskID, a.Cfg.MainBranch, mergeErr, err)
		}
		a.emit(state.Event{TaskID: taskID, Kind: "land_conflict", Detail: mergeErr.Error()})
		return fmt.Errorf("landing %s conflicts with %s: %w; run crew rebase %s",
			taskID, a.Cfg.MainBranch, mergeErr, taskID)
	}

	if _, err := a.Repo.Run("merge", "--ff-only", merged); err != nil {
		return fmt.Errorf("fast-forward %s: %w", a.Cfg.MainBranch, err)
	}

	if _, err := a.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		t.Status = state.StatusLanded
		t.HeadSha = merged
		t.UpdatedAt = a.now()
		return nil
	}); err != nil {
		return err
	}
	a.emit(state.Event{TaskID: taskID, Kind: "landed", Sha: merged})
	a.out("landed %s into %s at %s\n", taskID, a.Cfg.MainBranch, short(merged))

	unblocked, err := a.unblockDependents()
	if err != nil {
		return err
	}
	for _, id := range unblocked {
		a.out("unblocked %s\n", id)
	}
	return nil
}

// mergeInScratch merges rev into main inside a throwaway worktree and returns
// the resulting commit. branch names the merge for the commit message only;
// what is merged is rev, so a branch that moves cannot change what lands.
func (a *App) mergeInScratch(branch, rev string) (string, error) {
	scratch := filepath.Join(a.Root, ".crew", "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return "", err
	}
	wt, err := os.MkdirTemp(scratch, "land-*")
	if err != nil {
		return "", err
	}
	os.Remove(wt)

	if err := a.Repo.AddWorktreeDetached(wt, a.Cfg.MainBranch); err != nil {
		return "", fmt.Errorf("create merge worktree: %w", err)
	}
	defer func() {
		a.Repo.RemoveWorktree(wt)
		a.Repo.PruneWorktrees()
	}()

	scratchRepo := gitx.New(wt)
	if _, err := scratchRepo.Run("merge", "--no-ff", "-m",
		fmt.Sprintf("crew: land %s", branch), rev); err != nil {
		scratchRepo.Run("merge", "--abort")
		return "", err
	}
	return scratchRepo.RevParse("HEAD")
}

// unblockDependents moves tasks whose dependencies have all landed from
// pending to queued. depends_on gates on landed, not on merely finished.
func (a *App) unblockDependents() ([]string, error) {
	all, err := a.Tasks()
	if err != nil {
		return nil, err
	}
	st, err := a.Store.Read()
	if err != nil {
		return nil, err
	}
	var unblocked []string
	for _, t := range all {
		if len(t.DependsOn) == 0 {
			continue
		}
		cur := st.Tasks[t.ID]
		if cur != nil && cur.Status != state.StatusPending {
			continue
		}
		if !allLanded(st, t.DependsOn) {
			continue
		}
		unblocked = append(unblocked, t.ID)
	}
	if len(unblocked) == 0 {
		return nil, nil
	}
	_, err = a.Store.Update(func(st *state.State) error {
		for _, id := range unblocked {
			ts := st.Tasks[id]
			if ts == nil {
				ts = &state.TaskState{ID: id, Attempt: 1}
				st.Upsert(ts)
			}
			ts.Status = state.StatusQueued
			ts.UpdatedAt = a.now()
		}
		return nil
	})
	return unblocked, err
}

func allLanded(st *state.State, deps []string) bool {
	for _, d := range deps {
		ts := st.Tasks[d]
		if ts == nil || ts.Status != state.StatusLanded {
			return false
		}
	}
	return true
}

// Rebase rebases a task onto current main and forces a fresh verification.
//
// A rebase rewrites shas, so any prior approval no longer describes the code
// that would land. The approval is therefore invalidated here as an explicit
// step rather than left implicit in the status field.
func (a *App) Rebase(taskID string) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	branch := BranchName(taskID, ts.Attempt)
	worktree := WorktreePath(a.Root, taskID, ts.Attempt)

	wtRepo := gitx.New(worktree)
	if _, err := wtRepo.Run("rebase", a.Cfg.MainBranch); err != nil {
		wtRepo.Run("rebase", "--abort")
		if setErr := a.setStatus(taskID, state.StatusLandConflict, err.Error()); setErr != nil {
			return fmt.Errorf("rebase %s onto %s failed: %v; and recording that failed: %w",
				branch, a.Cfg.MainBranch, err, setErr)
		}
		return fmt.Errorf("rebase %s onto %s failed: %w", branch, a.Cfg.MainBranch, err)
	}

	head, err := a.Repo.RevParse(branch)
	if err != nil {
		return fmt.Errorf("resolve branch head: %w", err)
	}
	if _, err := a.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		// The approval is bound to a sha the rebase has just rewritten.
		t.ApprovedSha = ""
		t.HeadSha = head
		t.Status = state.StatusVerifying
		t.UpdatedAt = a.now()
		return nil
	}); err != nil {
		return err
	}
	a.emit(state.Event{TaskID: taskID, Kind: "rebased", Sha: head,
		Detail: "prior approval invalidated; the task must be approved again before it can land"})
	a.out("rebased %s onto %s; any prior approval is invalidated and a fresh verify pass will run\n",
		taskID, a.Cfg.MainBranch)
	return nil
}

// Reframe abandons the current attempt and starts a new one.
//
// The failed attempt's branch is preserved, never deleted, so its cycles stay
// readable when deciding how to reframe.
func (a *App) Reframe(taskID string) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	// Re-read the updated, re-confirmed intent.
	if _, err := a.Task(taskID); err != nil {
		return err
	}
	oldBranch := BranchName(taskID, ts.Attempt)

	st, err := a.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		t.Attempt++
		t.Cycle = 0
		t.Status = state.StatusPending
		t.RunID = ""
		t.Window = ""
		t.Role = ""
		t.ApprovedSha = ""
		t.HeadSha = ""
		t.DegradedCounts = nil
		t.Notes = "reframed from " + oldBranch
		t.PendingIntent = nil
		t.UpdatedAt = a.now()
		return nil
	})
	if err != nil {
		return err
	}
	a.emit(state.Event{TaskID: taskID, Kind: "reframed",
		Detail: fmt.Sprintf("attempt %d abandoned (branch %s preserved); starting attempt %d",
			ts.Attempt, oldBranch, st.Tasks[taskID].Attempt)})
	a.out("reframed %s: attempt %d starts fresh; %s is preserved for forensics\n",
		taskID, st.Tasks[taskID].Attempt, oldBranch)
	return nil
}

// setStatus records a transition. It returns the store's error rather than
// swallowing it, so a caller cannot report a transition it never persisted.
func (a *App) setStatus(taskID string, s state.Status, note string) error {
	_, err := a.Store.Update(func(st *state.State) error {
		t := st.Tasks[taskID]
		if t == nil {
			return nil
		}
		t.Status = s
		t.Notes = note
		t.UpdatedAt = a.now()
		return nil
	})
	return err
}

// DependenciesMet reports which of a task's dependencies have not landed.
func DependenciesMet(st *state.State, t tasks.Task) []string {
	var unmet []string
	for _, d := range t.DependsOn {
		ts := st.Tasks[d]
		if ts == nil || ts.Status != state.StatusLanded {
			unmet = append(unmet, d)
		}
	}
	return unmet
}
