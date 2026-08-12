package watch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wildmanpeace/crew/internal/budget"
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/state"
)

func denver(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func loop(t *testing.T) *Loop {
	t.Helper()
	root := t.TempDir()
	loc := denver(t)
	store, err := state.Open(root, loc)
	if err != nil {
		t.Fatal(err)
	}
	c := &config.Config{
		MaxCycles: 3, ConcurrencyCap: 3, PollIntervalSeconds: 15,
		PerWorkerBudgetUSD: 1.50, PerTaskCostCapUSD: 5.00, ProjectCostCapUSDPerDay: 25.00,
		BudgetSafetyMargin: 0.25, MinSpawnBudgetUSD: 0.10,
		VerifyTestSuffix: "_crewverify_test.go",
	}
	return &Loop{
		Root: root, Cfg: c, Store: store, Loc: loc,
		Session: "crewtest",
		Now:     func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, loc) },
	}
}

// A second watch must fail loudly rather than quietly drive the same tasks.
func TestSingletonLockRefusesASecondWatch(t *testing.T) {
	root := t.TempDir()
	release, err := Lock(root)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer release()

	if _, err := Lock(root); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Lock err = %v, want ErrAlreadyRunning", err)
	}
}

func TestLockIsReacquirableAfterRelease(t *testing.T) {
	root := t.TempDir()
	release, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release2, err := Lock(root)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	release2()
}

func TestTickWritesHeartbeat(t *testing.T) {
	l := loop(t)
	if err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := l.Store.Read()
	if st.WatchHeartbeat.IsZero() {
		t.Fatal("no heartbeat written")
	}
	if st.WatchPID != os.Getpid() {
		t.Errorf("WatchPID = %d, want %d", st.WatchPID, os.Getpid())
	}
}

// A crash between recording the intent and creating the window must not leave
// the task stranded in spawning.
func TestRepairReturnsAStrandedSpawnToQueued(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		ts := &state.TaskState{ID: "alpha", Status: state.StatusSpawning, RunID: "alpha-a1-c1-impl"}
		ts.PendingIntent = &state.Intent{
			Action: state.IntentSpawnWindow,
			Window: "crew-alpha-a1-c1-impl",
			RunID:  "alpha-a1-c1-impl",
		}
		st.Upsert(ts)
		return nil
	})
	if err := l.Repair(); err != nil {
		t.Fatal(err)
	}
	st, _ := l.Store.Read()
	ts := st.Tasks["alpha"]
	if ts.Status != state.StatusQueued {
		t.Fatalf("Status = %q, want queued", ts.Status)
	}
	if ts.PendingIntent != nil {
		t.Error("intent not cleared")
	}
}

// If the run actually completed, repair must not undo it.
func TestRepairLeavesACompletedRunAlone(t *testing.T) {
	l := loop(t)
	runs := filepath.Join(l.Root, ".crew", "runs")
	os.MkdirAll(runs, 0o755)
	os.WriteFile(filepath.Join(runs, "alpha-a1-c1-impl.exit"), []byte("0\n"), 0o644)

	l.Store.Update(func(st *state.State) error {
		ts := &state.TaskState{ID: "alpha", Status: state.StatusRunning, RunID: "alpha-a1-c1-impl"}
		ts.PendingIntent = &state.Intent{
			Action: state.IntentSpawnWindow, Window: "w", RunID: "alpha-a1-c1-impl",
		}
		st.Upsert(ts)
		return nil
	})
	if err := l.Repair(); err != nil {
		t.Fatal(err)
	}
	st, _ := l.Store.Read()
	if got := st.Tasks["alpha"].Status; got == state.StatusQueued {
		t.Fatal("repair reverted a run that had already completed")
	}
}

func TestBudgetIsRefusedBeforeSpawnWhenTaskCapIsSpent(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", SpendUSD: 5.00})
		return nil
	})
	if _, err := l.BudgetFor("alpha"); !errors.Is(err, budget.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestBudgetIsRefusedWhenDailyCapIsSpent(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha"})
		st.AddSpend(l.now(), l.Loc, 25.00)
		return nil
	})
	if _, err := l.BudgetFor("alpha"); !errors.Is(err, budget.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestBudgetReflectsAccumulatedTaskSpend(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", SpendUSD: 4.50})
		return nil
	})
	got, err := l.BudgetFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// $0.50 of headroom, less the 25% margin.
	if got > 0.38 || got < 0.37 {
		t.Fatalf("budget = %v, want ~0.375", got)
	}
}

// Slots are counted in tasks, not workers.
func TestSlotAvailableCountsTasks(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "a", Status: state.StatusRunning})
		st.Upsert(&state.TaskState{ID: "b", Status: state.StatusVerifying})
		st.Upsert(&state.TaskState{ID: "c", Status: state.StatusLanded})
		return nil
	})
	ok, err := l.SlotAvailable("")
	if err != nil || !ok {
		t.Fatalf("SlotAvailable = %v, %v; want true (2 of 3 used)", ok, err)
	}
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "d", Status: state.StatusReadyForReview})
		return nil
	})
	ok, _ = l.SlotAvailable("")
	if ok {
		t.Fatal("SlotAvailable = true with 3 of 3 slots used")
	}
}

// A task already holding a slot does not contend with itself.
func TestSlotAvailableExcludesTheTaskItself(t *testing.T) {
	l := loop(t)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "a", Status: state.StatusRunning})
		st.Upsert(&state.TaskState{ID: "b", Status: state.StatusRunning})
		st.Upsert(&state.TaskState{ID: "c", Status: state.StatusRunning})
		return nil
	})
	ok, _ := l.SlotAvailable("c")
	if !ok {
		t.Fatal("a task holding its own slot was refused")
	}
}

func TestCleanVerifyTestsRemovesOnlyVerifierAuthoredFiles(t *testing.T) {
	wt := t.TempDir()
	files := map[string]string{
		"counter/counter.go":                   "package counter\n",
		"counter/counter_test.go":              "package counter\n",
		"counter/cap_crewverify_test.go":       "package counter\n",
		"middleware/reload_crewverify_test.go": "package middleware\n",
		"middleware/middleware.go":             "package middleware\n",
	}
	for p, body := range files {
		full := filepath.Join(wt, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	removed, err := CleanVerifyTests(wt, "_crewverify_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want 2 files", removed)
	}
	for _, p := range []string{"counter/counter.go", "counter/counter_test.go", "middleware/middleware.go"} {
		if _, err := os.Stat(filepath.Join(wt, p)); err != nil {
			t.Errorf("%s was removed but should have been kept", p)
		}
	}
	for _, p := range []string{"counter/cap_crewverify_test.go", "middleware/reload_crewverify_test.go"} {
		if _, err := os.Stat(filepath.Join(wt, p)); err == nil {
			t.Errorf("%s survived cleanup", p)
		}
	}
}

func TestCleanVerifyTestsSkipsGitDirectory(t *testing.T) {
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	os.WriteFile(filepath.Join(wt, ".git", "x_crewverify_test.go"), []byte("x"), 0o644)
	removed, err := CleanVerifyTests(wt, "_crewverify_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("cleanup reached into .git: %v", removed)
	}
}

// Branches and worktrees are namespaced by attempt from the start, so a
// reframe cannot collide with the attempt it abandons.
func TestBranchAndWorktreeAreNamespacedByAttempt(t *testing.T) {
	if got := BranchName("add-rate-limiting", 1); got != "crew/add-rate-limiting/attempt-1" {
		t.Errorf("BranchName = %q", got)
	}
	if BranchName("x", 1) == BranchName("x", 2) {
		t.Fatal("attempts share a branch name")
	}
	w1 := WorktreePath("/proj", "x", 1)
	w2 := WorktreePath("/proj", "x", 2)
	if w1 == w2 {
		t.Fatal("attempts share a worktree path")
	}
	if !slices.Contains([]string{filepath.Join("/proj", ".crew", "worktrees", "x", "attempt-1")}, w1) {
		t.Errorf("WorktreePath = %q", w1)
	}
}

func TestRecordSpendAccumulatesIntoBothLedgers(t *testing.T) {
	l := loop(t)
	runs := l.runsDir()
	os.MkdirAll(runs, 0o755)
	os.WriteFile(filepath.Join(runs, "alpha-a1-c1-impl.json"),
		[]byte(`{"type":"result","subtype":"success","total_cost_usd":0.40}`), 0o644)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", RunID: "alpha-a1-c1-impl", SpendUSD: 1.00})
		return nil
	})
	st, _ := l.Store.Read()
	if err := l.recordSpend(st.Tasks["alpha"]); err != nil {
		t.Fatal(err)
	}
	st, _ = l.Store.Read()
	if got := st.Tasks["alpha"].SpendUSD; got < 1.39 || got > 1.41 {
		t.Errorf("task SpendUSD = %v, want ~1.40", got)
	}
	if got := st.DailySpend(l.now(), l.Loc); got < 0.39 || got > 0.41 {
		t.Errorf("daily spend = %v, want ~0.40", got)
	}
}

// Spend from a failed run still counts: the budget cap is applied after the
// turn that breaches it, so the money was genuinely spent.
func TestSpendFromABudgetExhaustedRunIsStillRecorded(t *testing.T) {
	l := loop(t)
	runs := l.runsDir()
	os.MkdirAll(runs, 0o755)
	os.WriteFile(filepath.Join(runs, "alpha-a1-c1-impl.json"),
		[]byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.0355}`), 0o644)
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", RunID: "alpha-a1-c1-impl"})
		return nil
	})
	st, _ := l.Store.Read()
	l.recordSpend(st.Tasks["alpha"])
	st, _ = l.Store.Read()
	if st.Tasks["alpha"].SpendUSD == 0 {
		t.Fatal("spend from a budget-exhausted run was not recorded")
	}
}

// events returns everything the loop has recorded.
func events(t *testing.T, l *Loop) []state.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.Root, ".crew", "events.jsonl"))
	if err != nil {
		return nil
	}
	var out []state.Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev state.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// A truncated exit marker left the task in running forever: the poll skipped
// it on every pass, so it held a concurrency slot, never reached reconcile and
// so could not even time out, and wrote nothing to events.jsonl explaining
// itself.
func TestACorruptExitMarkerFailsTheRunRatherThanStallingIt(t *testing.T) {
	l := loop(t)
	runs := filepath.Join(l.Root, ".crew", "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	// A marker truncated mid-write by a crash.
	if err := os.WriteFile(filepath.Join(runs, "alpha-a1-c1-impl.exit"), []byte("1\x00tr"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Status: state.StatusRunning,
			Attempt: 1, Cycle: 1, RunID: "alpha-a1-c1-impl", Window: "crew-alpha-a1-c1-impl"})
		return nil
	})

	if err := l.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	st, _ := l.Store.Read()
	ts := st.Tasks["alpha"]

	if ts.Status.InFlight() {
		t.Fatalf("Status = %q; the stalled task is still holding a concurrency slot", ts.Status)
	}
	if ts.Status != state.StatusFailed {
		t.Fatalf("Status = %q, want failed", ts.Status)
	}
	if !strings.Contains(ts.Notes, "alpha-a1-c1-impl") {
		t.Errorf("Notes = %q, want it to name the run", ts.Notes)
	}

	var sawDiagnostic bool
	for _, ev := range events(t, l) {
		if ev.Kind == "watch_error" && strings.Contains(ev.Detail, "exit marker") {
			sawDiagnostic = true
		}
	}
	if !sawDiagnostic {
		t.Errorf("nothing was recorded explaining the stall: %+v", events(t, l))
	}
}

// The lock must admit exactly one holder under contention.
//
// Release unlinks the file while the lock is still held. Unlocking first left
// a window in which one starter could acquire the old inode through the path
// while another, after the unlink, created and acquired a new one. The window
// is narrow enough that this exercises it rather than proving it closed; what
// it does guarantee is that nothing in the acquire/release cycle admits two
// holders at once.
func TestLockAdmitsOneHolderUnderContention(t *testing.T) {
	root := t.TempDir()
	var holders atomic.Int32
	var doubled atomic.Bool
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				release, err := Lock(root)
				if err != nil {
					continue // another goroutine holds it, which is the point
				}
				if holders.Add(1) != 1 {
					doubled.Store(true)
				}
				holders.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()

	if doubled.Load() {
		t.Fatal("two watches held the singleton lock at once")
	}
	if _, err := os.Stat(filepath.Join(root, ".crew", "watch.lock")); !os.IsNotExist(err) {
		t.Errorf("the lock file outlived its holder: %v", err)
	}
}
