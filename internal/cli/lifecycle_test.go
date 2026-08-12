package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/state"
)

const tasksMD = `## task: alpha
- brief: do alpha
- paths: alpha/**
- acceptance_criteria:
    - judged: true
      description: it works

## task: beta
- depends_on: alpha
- brief: do beta
- paths: beta/**
- acceptance_criteria:
    - judged: true
      description: it works
`

// fixture builds a real git project with a task branch already created.
func fixture(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := gitx.New(root)
	mustRun(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(root, "README.md"), "base\n")
	writeFile(t, filepath.Join(root, "TASKS.md"), tasksMD)
	mustRun(t, repo, "add", "-A")
	mustRun(t, repo, "commit", "-qm", "base")

	loc, _ := time.LoadLocation("America/Denver")
	store, err := state.Open(root, loc)
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	app := &App{
		Root: root,
		Cfg: &config.Config{
			MainBranch: "main", ConcurrencyCap: 3, MaxCycles: 3,
			PerWorkerBudgetUSD: 1.5, PerTaskCostCapUSD: 5, ProjectCostCapUSDPerDay: 25,
			BudgetSafetyMargin: 0.25, MinSpawnBudgetUSD: 0.10,
			VerifyTestSuffix: "_crewverify_test.go", PollIntervalSeconds: 15,
			CheckCommands: map[string]config.CheckCommand{
				"test": {Argv: []string{"go", "test"}},
			},
		},
		Store: store, Repo: repo, Loc: loc,
		Stdout: out, Stderr: out,
		Session: "crewtest-cli",
		Now:     func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, loc) },
		IsTTY:   func() bool { return true },
	}
	return app, out
}

func mustRun(t *testing.T, r gitx.Repo, args ...string) {
	t.Helper()
	if _, err := r.Run(args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readyTask spawns a task, puts a commit on its branch, and marks it ready.
func readyTask(t *testing.T, a *App, id string) string {
	t.Helper()
	if err := a.Spawn(id, false); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	wt := WorktreePath(a.Root, id, 1)
	writeFile(t, filepath.Join(wt, id+".txt"), "work\n")
	wtRepo := gitx.New(wt)
	mustRun(t, wtRepo, "add", "-A")
	mustRun(t, wtRepo, "commit", "-qm", "work")

	head, _ := a.Repo.RevParse(BranchName(id, 1))
	a.Store.Update(func(st *state.State) error {
		ts := st.Tasks[id]
		ts.Status = state.StatusReadyForReview
		ts.HeadSha = head
		return nil
	})
	return head
}

// The TTY requirement is what makes approval structurally captain-only.
func TestApproveRefusesWithoutATTY(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	a.IsTTY = func() bool { return false }

	err := a.Approve("alpha", head)
	if !errors.Is(err, ErrNotTTY) {
		t.Fatalf("err = %v, want ErrNotTTY", err)
	}
	ts, _ := a.taskState("alpha")
	if ts.Status == state.StatusApproved {
		t.Fatal("task was approved without a terminal")
	}
}

// An approval must name exactly the code that was read.
func TestApproveRefusesAStaleSha(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")

	// The branch moves after review.
	wt := WorktreePath(a.Root, "alpha", 1)
	writeFile(t, filepath.Join(wt, "more.txt"), "more\n")
	wtRepo := gitx.New(wt)
	mustRun(t, wtRepo, "add", "-A")
	mustRun(t, wtRepo, "commit", "-qm", "more")

	if err := a.Approve("alpha", head); err == nil {
		t.Fatal("a stale sha was approved")
	} else if !strings.Contains(err.Error(), "moved") {
		t.Errorf("err = %v", err)
	}
}

func TestApproveRequiresReadyForReview(t *testing.T) {
	a, _ := fixture(t)
	if err := a.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	head, _ := a.Repo.RevParse(BranchName("alpha", 1))
	if err := a.Approve("alpha", head); err == nil {
		t.Fatal("a queued task was approved")
	}
}

func TestApproveSucceedsOnCurrentHead(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	if err := a.Approve("alpha", head); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	ts, _ := a.taskState("alpha")
	if ts.Status != state.StatusApproved || ts.ApprovedSha != head {
		t.Fatalf("state = %+v", ts)
	}
}

// Review and verify read the task worktree, but an approval binds the
// committed head. Approving over uncommitted edits would approve work that
// cannot land.
func TestApproveRefusesADirtyTaskWorktree(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")

	// One untracked and one modified path, so both porcelain shapes are named.
	wt := WorktreePath(a.Root, "alpha", 1)
	writeFile(t, filepath.Join(wt, "late.txt"), "written after the last commit\n")
	writeFile(t, filepath.Join(wt, "alpha.txt"), "edited after the last commit\n")

	err := a.Approve("alpha", head)
	if err == nil {
		t.Fatal("approved a task with uncommitted edits in its worktree")
	}
	if !strings.Contains(err.Error(), "late.txt") || !strings.Contains(err.Error(), "alpha.txt") {
		t.Errorf("err = %v; it must name the dirty paths", err)
	}
	ts, _ := a.taskState("alpha")
	if ts.Status == state.StatusApproved {
		t.Fatal("the task was approved anyway")
	}
}

func TestLandRequiresApproval(t *testing.T) {
	a, _ := fixture(t)
	readyTask(t, a, "alpha")
	if err := a.Land("alpha"); err == nil {
		t.Fatal("an unapproved task landed")
	}
}

// The window between approving and landing must be closed.
func TestLandRefusesWhenTheBranchMovedAfterApproval(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	if err := a.Approve("alpha", head); err != nil {
		t.Fatal(err)
	}

	wt := WorktreePath(a.Root, "alpha", 1)
	writeFile(t, filepath.Join(wt, "sneaky.txt"), "added after approval\n")
	wtRepo := gitx.New(wt)
	mustRun(t, wtRepo, "add", "-A")
	mustRun(t, wtRepo, "commit", "-qm", "sneaky")

	err := a.Land("alpha")
	if err == nil {
		t.Fatal("code that was never approved was landed")
	}
	if !strings.Contains(err.Error(), "approved again") {
		t.Errorf("err = %v", err)
	}
}

func TestLandFastForwardsMainAndMarksLanded(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	if err := a.Approve("alpha", head); err != nil {
		t.Fatal(err)
	}
	before, _ := a.Repo.RevParse("main")
	if err := a.Land("alpha"); err != nil {
		t.Fatalf("Land: %v", err)
	}
	after, _ := a.Repo.RevParse("main")
	if before == after {
		t.Fatal("main did not move")
	}
	ts, _ := a.taskState("alpha")
	if ts.Status != state.StatusLanded {
		t.Fatalf("Status = %q", ts.Status)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "alpha.txt")); err != nil {
		t.Errorf("landed work is not present on main: %v", err)
	}
}

// The merge must bind the approved commit, not whatever the branch points at
// by the time git runs: the ref can move between the head check and the merge.
func TestMergeInScratchMergesTheApprovedShaNotTheBranchRef(t *testing.T) {
	a, _ := fixture(t)
	approved := readyTask(t, a, "alpha")

	// The branch moves after the approved sha was resolved.
	wt := WorktreePath(a.Root, "alpha", 1)
	writeFile(t, filepath.Join(wt, "unapproved.txt"), "never reviewed\n")
	wtRepo := gitx.New(wt)
	mustRun(t, wtRepo, "add", "-A")
	mustRun(t, wtRepo, "commit", "-qm", "unapproved")

	merged, err := a.mergeInScratch(BranchName("alpha", 1), approved)
	if err != nil {
		t.Fatalf("mergeInScratch: %v", err)
	}
	files, err := a.Repo.Run("ls-tree", "-r", "--name-only", merged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files, "alpha.txt") {
		t.Errorf("the approved work is missing from the merge:\n%s", files)
	}
	if strings.Contains(files, "unapproved.txt") {
		t.Errorf("a commit that was never approved was merged:\n%s", files)
	}
}

// Landing fast-forwards main in the root repo. With something else checked
// out, that fast-forward would advance the other ref and main would never
// move, so the mismatch is refused rather than silently recorded as landed.
func TestLandRefusesWhenMainIsNotCheckedOut(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	if err := a.Approve("alpha", head); err != nil {
		t.Fatal(err)
	}
	mustRun(t, a.Repo, "checkout", "-q", "-b", "captain-wip")
	before, _ := a.Repo.RevParse("main")

	err := a.Land("alpha")
	if err == nil {
		t.Fatal("landed with a branch other than main checked out")
	}
	if !strings.Contains(err.Error(), "captain-wip") {
		t.Errorf("err = %v; it must name what is checked out", err)
	}
	if after, _ := a.Repo.RevParse("main"); after != before {
		t.Error("main moved")
	}
	ts, _ := a.taskState("alpha")
	if ts.Status != state.StatusApproved {
		t.Fatalf("Status = %q, want approved", ts.Status)
	}
}

// A status transition that could not be persisted must not be reported done.
func TestSetStatusReportsAFailureToPersist(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, _ := fixture(t)
	readyTask(t, a, "alpha")

	dir := a.Store.Dir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if err := a.setStatus("alpha", state.StatusLandConflict, "conflicted"); err == nil {
		t.Fatal("a status transition that never persisted was reported as done")
	}
}

func TestLandRefusesOnDirtyMain(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	a.Approve("alpha", head)
	writeFile(t, filepath.Join(a.Root, "dirty.txt"), "uncommitted\n")

	if err := a.Land("alpha"); err == nil {
		t.Fatal("landed onto a dirty main")
	}
}

// depends_on gates on landed.
func TestLandUnblocksDependents(t *testing.T) {
	a, _ := fixture(t)
	a.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "beta", Status: state.StatusPending, Attempt: 1})
		return nil
	})
	head := readyTask(t, a, "alpha")
	a.Approve("alpha", head)
	if err := a.Land("alpha"); err != nil {
		t.Fatal(err)
	}
	ts, _ := a.taskState("beta")
	if ts.Status != state.StatusQueued {
		t.Fatalf("beta Status = %q, want queued", ts.Status)
	}
}

// A rebase rewrites shas, so an approval bound to the old head cannot stand.
func TestRebaseInvalidatesApproval(t *testing.T) {
	a, _ := fixture(t)
	head := readyTask(t, a, "alpha")
	if err := a.Approve("alpha", head); err != nil {
		t.Fatal(err)
	}
	// Move main so the rebase is real.
	writeFile(t, filepath.Join(a.Root, "other.txt"), "main moved\n")
	mustRun(t, a.Repo, "add", "-A")
	mustRun(t, a.Repo, "commit", "-qm", "main moves")

	if err := a.Rebase("alpha"); err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	ts, _ := a.taskState("alpha")
	if ts.ApprovedSha != "" {
		t.Fatal("approval survived a rebase")
	}
	if ts.Status != state.StatusVerifying {
		t.Fatalf("Status = %q, want verifying", ts.Status)
	}
	if err := a.Land("alpha"); err == nil {
		t.Fatal("a rebased task landed without re-approval")
	}
}

// A reframe must not collide with the attempt it abandons.
func TestReframeIncrementsAttemptAndPreservesTheOldBranch(t *testing.T) {
	a, _ := fixture(t)
	if err := a.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	a.Store.Update(func(st *state.State) error {
		st.Tasks["alpha"].Cycle = 3
		st.Tasks["alpha"].Status = state.StatusNeedsReframe
		return nil
	})
	if err := a.Reframe("alpha"); err != nil {
		t.Fatalf("Reframe: %v", err)
	}
	ts, _ := a.taskState("alpha")
	if ts.Attempt != 2 {
		t.Fatalf("Attempt = %d, want 2", ts.Attempt)
	}
	if ts.Cycle != 0 {
		t.Fatalf("Cycle = %d, want 0", ts.Cycle)
	}
	if ts.Status != state.StatusPending {
		t.Fatalf("Status = %q, want pending", ts.Status)
	}
	// The failed attempt stays readable.
	exists, _ := a.Repo.BranchExists(BranchName("alpha", 1))
	if !exists {
		t.Fatal("the abandoned attempt's branch was deleted")
	}
	// And the next attempt has somewhere collision-free to go.
	if BranchName("alpha", 1) == BranchName("alpha", 2) {
		t.Fatal("attempts share a branch name")
	}
}

func TestReframeThenSpawnUsesTheNewAttemptPath(t *testing.T) {
	a, _ := fixture(t)
	a.Spawn("alpha", false)
	a.Store.Update(func(st *state.State) error {
		st.Tasks["alpha"].Status = state.StatusNeedsReframe
		return nil
	})
	a.Reframe("alpha")
	if err := a.Spawn("alpha", false); err != nil {
		t.Fatalf("Spawn after reframe: %v", err)
	}
	if _, err := os.Stat(WorktreePath(a.Root, "alpha", 2)); err != nil {
		t.Fatalf("attempt-2 worktree not created: %v", err)
	}
	if _, err := os.Stat(WorktreePath(a.Root, "alpha", 1)); err != nil {
		t.Errorf("attempt-1 worktree was destroyed: %v", err)
	}
}

func TestSpawnRefusesUnmetDependencies(t *testing.T) {
	a, _ := fixture(t)
	refusals, err := a.SpawnChecks("beta")
	if err != nil {
		t.Fatal(err)
	}
	if !hasRefusal(refusals, "unmet dependencies") {
		t.Fatalf("refusals = %v", refusals)
	}
	if err := a.Spawn("beta", false); err == nil {
		t.Fatal("beta spawned with alpha unlanded")
	}
}

func TestSpawnRefusesAPreExistingBranch(t *testing.T) {
	a, _ := fixture(t)
	if err := a.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	// Reset the state so only the branch and worktree stand in the way.
	a.Store.Update(func(st *state.State) error {
		st.Tasks["alpha"].Status = state.StatusPending
		return nil
	})
	refusals, _ := a.SpawnChecks("alpha")
	if !hasRefusal(refusals, "branch already exists") {
		t.Fatalf("refusals = %v", refusals)
	}
}

func TestSpawnRefusesOnBudgetExhaustion(t *testing.T) {
	a, _ := fixture(t)
	a.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Attempt: 1, SpendUSD: 5.00})
		return nil
	})
	refusals, _ := a.SpawnChecks("alpha")
	if !hasRefusal(refusals, "insufficient budget") {
		t.Fatalf("refusals = %v", refusals)
	}
}

func TestSpawnRefusesOnConcurrencyCap(t *testing.T) {
	a, _ := fixture(t)
	a.Cfg.ConcurrencyCap = 1
	a.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "beta", Status: state.StatusRunning, Attempt: 1})
		return nil
	})
	refusals, _ := a.SpawnChecks("alpha")
	if !hasRefusal(refusals, "concurrency cap reached") {
		t.Fatalf("refusals = %v", refusals)
	}
}

func TestForceOverridesRefusals(t *testing.T) {
	a, out := fixture(t)
	if err := a.Spawn("beta", true); err != nil {
		t.Fatalf("forced spawn: %v", err)
	}
	if !strings.Contains(out.String(), "warning (--force)") {
		t.Errorf("force did not warn:\n%s", out.String())
	}
}

func hasRefusal(rs []Refusal, reason string) bool {
	for _, r := range rs {
		if r.Reason == reason {
			return true
		}
	}
	return false
}

// crew review is read-only and must never change anything.
func TestReviewIsReadOnly(t *testing.T) {
	a, out := fixture(t)
	head := readyTask(t, a, "alpha")

	before, _ := a.Store.Read()
	beforeStatus := before.Tasks["alpha"].Status

	if err := a.Review("alpha"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	after, _ := a.Store.Read()
	if after.Tasks["alpha"].Status != beforeStatus {
		t.Fatal("review changed the task status")
	}
	if after.Tasks["alpha"].ApprovedSha != "" {
		t.Fatal("review approved something")
	}
	body := out.String()
	if !strings.Contains(body, head) {
		t.Error("review did not print the head sha")
	}
	if !strings.Contains(body, "crew approve alpha --head "+head) {
		t.Errorf("review did not print the exact approve line:\n%s", body)
	}
}

func TestReviewExcludesVerifierAuthoredTestsFromTheDiff(t *testing.T) {
	a, out := fixture(t)
	a.Spawn("alpha", false)
	wt := WorktreePath(a.Root, "alpha", 1)
	writeFile(t, filepath.Join(wt, "impl.go"), "package alpha\n")
	writeFile(t, filepath.Join(wt, "x_crewverify_test.go"), "package alpha\n")
	wtRepo := gitx.New(wt)
	mustRun(t, wtRepo, "add", "-A")
	mustRun(t, wtRepo, "commit", "-qm", "work")
	a.Store.Update(func(st *state.State) error {
		st.Tasks["alpha"].Status = state.StatusReadyForReview
		return nil
	})

	if err := a.Review("alpha"); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, "impl.go") {
		t.Error("diff omitted implementation changes")
	}
	if strings.Contains(body, "x_crewverify_test.go") {
		t.Error("diff leaked verifier-authored tests")
	}
}

func TestStatusReportsSpendAndStaleWatch(t *testing.T) {
	a, _ := fixture(t)
	a.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Status: state.StatusRunning, Attempt: 1, SpendUSD: 2.5})
		st.AddSpend(a.now(), a.Loc, 2.5)
		st.WatchHeartbeat = a.now().Add(-10 * time.Minute)
		return nil
	})
	rep, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	if rep.DailySpendUSD != 2.5 {
		t.Errorf("DailySpendUSD = %v", rep.DailySpendUSD)
	}
	if rep.WatchAlive {
		t.Error("a 10-minute-old heartbeat was reported alive")
	}
	if rep.WatchStaleFor == "" {
		t.Error("staleness not reported")
	}
}

func TestStatusSurfacesRetagSuggestions(t *testing.T) {
	a, _ := fixture(t)
	a.Store.Update(func(st *state.State) error {
		ts := &state.TaskState{ID: "alpha", Status: state.StatusReadyForReview, Attempt: 1}
		ts.NoteDegraded("rate limit is configurable")
		ts.NoteDegraded("rate limit is configurable")
		st.Upsert(ts)
		return nil
	})
	rep, _ := a.Status()
	if len(rep.Tasks) != 1 || len(rep.Tasks[0].Suggestions) != 1 {
		t.Fatalf("suggestions = %+v", rep.Tasks)
	}
	if !strings.Contains(rep.Tasks[0].Suggestions[0], "judged: true") {
		t.Errorf("suggestion = %q", rep.Tasks[0].Suggestions[0])
	}
}

func TestDoctorFlagsOrphanedWorktree(t *testing.T) {
	a, _ := fixture(t)
	a.Spawn("alpha", false)
	// Forget the task, leaving its worktree behind.
	a.Store.Update(func(st *state.State) error {
		delete(st.Tasks, "alpha")
		return nil
	})
	problems, err := a.DoctorFindings()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range problems {
		if strings.Contains(p.Detail, "orphaned worktree") {
			found = true
		}
	}
	if !found {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestGCRemovesOrphanedWorktrees(t *testing.T) {
	a, _ := fixture(t)
	a.Spawn("alpha", false)
	wt := WorktreePath(a.Root, "alpha", 1)
	a.Store.Update(func(st *state.State) error {
		delete(st.Tasks, "alpha")
		return nil
	})
	if err := a.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("orphaned worktree survived gc")
	}
}
