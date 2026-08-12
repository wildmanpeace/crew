package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wildmanpeace/crew/internal/budget"
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/report"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tmux"
	"github.com/wildmanpeace/crew/internal/worker"
)

// ErrAlreadyRunning means another crew watch holds the singleton lock.
var ErrAlreadyRunning = errors.New("another crew watch is already running")

// Loop is the always-on driver. It is never self-daemonizing; something
// external keeps it alive.
type Loop struct {
	Root      string
	Cfg       *config.Config
	Store     *state.Store
	Repo      gitx.Repo
	CrewBin   string
	ClaudeBin string
	Session   string
	Loc       *time.Location

	// Now is injectable so budget rollover and heartbeat behaviour are
	// testable without waiting for real time to pass.
	Now func() time.Time

	// Notify delivers captain-facing events. Internal transitions such as
	// verify_failed are recorded but never notified.
	Notify func(state.Event)

	// Land merges an approved task, normally cli.App.Land. It is injected
	// rather than reimplemented so the loop and a manual crew land share one
	// set of checks: the approved sha, a clean main, and a scratch merge.
	// A nil Land leaves approved tasks for the captain to land themselves.
	Land func(taskID string) error

	// landDeferred remembers which tasks have already reported a land that
	// could not proceed, so a main left dirty for an hour notifies once rather
	// than every poll. It is per-process on purpose: a restart is a fine
	// moment to be told again.
	landDeferred map[string]bool
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

func (l *Loop) runsDir() string { return filepath.Join(l.Root, ".crew", "runs") }

func (l *Loop) emit(ev state.Event) {
	if ev.At.IsZero() {
		ev.At = l.now()
	}
	l.Store.Append(ev)
	if l.Notify != nil {
		l.Notify(ev)
	}
}

// record writes an event without notifying, for internal transitions.
func (l *Loop) record(ev state.Event) {
	if ev.At.IsZero() {
		ev.At = l.now()
	}
	l.Store.Append(ev)
}

// Lock acquires the singleton lock, held for the process lifetime.
//
// It is non-blocking on purpose: a second watch should fail loudly rather
// than wait and then silently start driving the same tasks.
func Lock(root string) (release func() error, err error) {
	path := filepath.Join(root, ".crew", "watch.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create .crew: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open watch lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrAlreadyRunning
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() error {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return os.Remove(path)
	}, nil
}

// Run holds the singleton lock, repairs any interrupted state, and polls
// until the context is cancelled.
func (l *Loop) Run(ctx context.Context) error {
	release, err := Lock(l.Root)
	if err != nil {
		return err
	}
	defer release()

	if err := l.Repair(); err != nil {
		return fmt.Errorf("startup repair: %w", err)
	}

	interval := time.Duration(l.Cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := l.Tick(ctx); err != nil {
			l.record(state.Event{Kind: "watch_error", Detail: err.Error()})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Repair reconciles intents recorded before an effect that may never have
// happened.
//
// Every side effect is preceded by a durable record naming the identifiers
// the effect will use, so a crash between the two is detectable: if neither
// the window nor the run's exit marker exists, the effect never took place
// and the task is returned to its prior status rather than being left
// stranded in spawning.
func (l *Loop) Repair() error {
	st, err := l.Store.Read()
	if err != nil {
		return err
	}
	for _, ts := range st.PendingIntents() {
		intent := ts.PendingIntent
		if intent.Action != state.IntentSpawnWindow {
			continue
		}
		windowLive, _ := tmux.WindowExists(l.Session, intent.Window)
		_, runDone, _ := worker.ReadExit(l.runsDir(), intent.RunID)

		switch {
		case runDone:
			// The effect completed; the normal completion path will pick it up.
			l.clearIntent(ts.ID, "")
		case windowLive:
			// The effect happened and is still going.
			l.clearIntent(ts.ID, state.StatusRunning)
		default:
			// The effect never happened.
			l.clearIntent(ts.ID, state.StatusQueued)
			l.record(state.Event{TaskID: ts.ID, Kind: "repaired",
				Detail: fmt.Sprintf("spawn intent for run %s never took effect; returned to queued", intent.RunID)})
		}
	}
	return nil
}

func (l *Loop) clearIntent(taskID string, newStatus state.Status) {
	l.Store.Update(func(st *state.State) error {
		ts := st.Tasks[taskID]
		if ts == nil {
			return nil
		}
		ts.PendingIntent = nil
		if newStatus != "" {
			ts.Status = newStatus
		}
		ts.UpdatedAt = l.now()
		return nil
	})
}

// Tick performs one pass: heartbeat, then advance every task that has
// something to advance.
func (l *Loop) Tick(ctx context.Context) error {
	if _, err := l.Store.Update(func(st *state.State) error {
		st.WatchHeartbeat = l.now()
		st.WatchPID = os.Getpid()
		return nil
	}); err != nil {
		return err
	}

	st, err := l.Store.Read()
	if err != nil {
		return err
	}

	// Advance anything that has finished, and reconcile anything that has not.
	var active []string
	for id, ts := range st.Tasks {
		if ts.RunID != "" && isActive(ts.Status) {
			active = append(active, id)
		}
	}
	sort.Strings(active)

	for _, id := range active {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		ts := st.Tasks[id]
		code, done, err := worker.ReadExit(l.runsDir(), ts.RunID)
		switch {
		case err != nil:
			// The marker is written last and by atomic rename, so one that
			// cannot be read means the run is over and its outcome is lost.
			// Skipping the task instead left it in running forever: it held a
			// concurrency slot, reconcile was never reached so the wall clock
			// could not fire either, and nothing was recorded to say why.
			l.record(state.Event{TaskID: id, Kind: "watch_error",
				Detail: fmt.Sprintf("exit marker for run %s: %v", ts.RunID, err)})
			l.failRun(id, ts, fmt.Sprintf(
				"worker %s left an unreadable exit marker, so its outcome cannot be recovered: %v",
				ts.RunID, err), "exit_marker_unreadable")

		case done:
			if err := l.onWorkerExit(id, code); err != nil {
				l.record(state.Event{TaskID: id, Kind: "watch_error", Detail: err.Error()})
			}

		default:
			if err := l.reconcileActive(id, ts); err != nil {
				l.record(state.Event{TaskID: id, Kind: "watch_error", Detail: err.Error()})
			}
		}
	}

	if err := l.landApproved(ctx); err != nil {
		l.record(state.Event{Kind: "watch_error", Detail: err.Error()})
	}

	// Start whatever anything queued is waiting on. crew spawn prepares the
	// branch and worktree and stops; every worker is started here, so there is
	// exactly one place that spawns.
	return l.startQueued(ctx)
}

// landApproved carries approved tasks the rest of the way.
//
// Approval is where the captain decides; landing re-checks the approved sha
// and merges, deciding nothing. Leaving it to be typed made the captain enact
// a call they had already made, and a task sit approved-but-unlanded in the
// meantime, blocking its dependents for no reason.
func (l *Loop) landApproved(ctx context.Context) error {
	if l.Land == nil || !l.Cfg.AutoLandEnabled() {
		return nil
	}
	st, err := l.Store.Read()
	if err != nil {
		return err
	}
	var approved []string
	for id, ts := range st.Tasks {
		if ts.Status == state.StatusApproved {
			approved = append(approved, id)
		}
	}
	sort.Strings(approved)

	for _, id := range approved {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := l.Land(id); err != nil {
			l.deferLand(id, err)
			continue
		}
		delete(l.landDeferred, id)
	}
	return nil
}

// deferLand reports a land that could not proceed, once per task per process.
//
// The common cause is uncommitted work on main, which is the captain's own
// doing and resolves on its own; the task stays approved and the next poll
// tries again. A cause that does not resolve — a conflict — has already moved
// the task out of approved by the time this is reached, so it is not retried.
func (l *Loop) deferLand(taskID string, cause error) {
	if l.landDeferred[taskID] {
		return
	}
	if l.landDeferred == nil {
		l.landDeferred = map[string]bool{}
	}
	l.landDeferred[taskID] = true
	l.emit(state.Event{TaskID: taskID, Kind: "land_deferred", Detail: cause.Error()})
}

// startQueued starts the next worker of each queued task, subject to the
// concurrency cap. Budget is checked inside SpawnWorker, before the spawn.
func (l *Loop) startQueued(ctx context.Context) error {
	st, err := l.Store.Read()
	if err != nil {
		return err
	}
	var queued []string
	for id, ts := range st.Tasks {
		if ts.Status == state.StatusQueued {
			queued = append(queued, id)
		}
	}
	sort.Strings(queued)

	for _, id := range queued {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		ok, err := l.SlotAvailable(id)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := l.startTask(id, st.Tasks[id]); err != nil {
			l.record(state.Event{TaskID: id, Kind: "watch_error", Detail: err.Error()})
		}
	}
	return nil
}

// startTask spawns the step a queued task is waiting on.
//
// It is the resume point as well as the start point: a spawn that failed
// transiently returns its task to queued with the role and cycle it was
// attempting still recorded, and that step is what must be retried.
func (l *Loop) startTask(taskID string, ts *state.TaskState) error {
	role, cycle := ResumeStep(ts)
	if role == config.RoleVerifier {
		// spawnVerifier rebuilds the brief from the diff, so a resumed
		// verifier is briefed exactly as the one it is replacing was.
		return l.spawnVerifier(taskID)
	}
	t, err := l.Task(taskID)
	if err != nil {
		l.blockTask(taskID, err.Error())
		return nil
	}
	return l.SpawnWorker(taskID, config.RoleImplementer, cycle,
		ImplementerBrief(t, cycle, l.resumeFailures(taskID, ts, cycle)))
}

// resumeFailures recovers the unmet criteria a retried implementer was to be
// briefed on.
//
// The brief is rebuilt from the verifier report still sitting in the worktree
// rather than carried in state, so a resumed cycle is told what the one it
// replaces would have been told. A first cycle has no verifier behind it, and
// an unreadable or non-verifier report simply yields nothing.
func (l *Loop) resumeFailures(taskID string, ts *state.TaskState, cycle int) []string {
	if cycle <= 1 || ts == nil {
		return nil
	}
	r, err := report.LoadVerifierWithSuffix(WorktreePath(l.Root, taskID, ts.Attempt),
		l.Cfg.VerifyTestSuffix)
	if err != nil || r == nil {
		return nil
	}
	_, failures := Outcome(r)
	return failures
}

func isActive(s state.Status) bool {
	return s == state.StatusSpawning || s == state.StatusRunning || s == state.StatusVerifying
}

// spawnGrace is how long after a spawn crew waits before believing a missing
// window. It absorbs the moment between recording the spawn and tmux reporting
// the window, so a healthy worker is never declared dead on its first tick.
const spawnGrace = 20 * time.Second

// reconcileActive handles a worker that has not reported completion.
//
// Without this, a worker whose window dies mid-run leaves its task in running
// forever: the loop only advances a task when the exit marker appears, so a
// process that is killed before writing one is never noticed. The task holds a
// concurrency slot indefinitely and fails crew doctor, which in turn refuses
// every subsequent spawn.
func (l *Loop) reconcileActive(id string, ts *state.TaskState) error {
	// A worker that has only just been spawned gets the benefit of the doubt.
	if !ts.RunStartedAt.IsZero() && l.now().Sub(ts.RunStartedAt) < spawnGrace {
		return nil
	}

	alive, err := tmux.WindowExists(l.Session, ts.Window)
	if err != nil {
		// tmux could not answer, so nothing is known. Leaving the task alone is
		// the safe reading: declaring it dead on an unreadable answer would
		// kill healthy work.
		return nil
	}

	if !alive {
		// Re-read the exit marker before concluding anything. A worker that
		// finished a moment ago has already closed its window, and calling that
		// orphaned would fail a run that actually succeeded.
		if code, done, err := worker.ReadExit(l.runsDir(), ts.RunID); err == nil && done {
			return l.onWorkerExit(id, code)
		}
		reason := fmt.Sprintf(
			"worker %s stopped without reporting: its window is gone and no exit marker was written",
			ts.RunID)
		l.failRun(id, ts, reason, "worker_vanished")
		return nil
	}

	// The window is alive, so the only remaining question is whether it has
	// outlived its wall clock.
	timeout := time.Duration(l.Cfg.WallClockTimeoutSeconds) * time.Second
	if timeout <= 0 || ts.RunStartedAt.IsZero() {
		return nil
	}
	if ran := l.now().Sub(ts.RunStartedAt); ran > timeout {
		if err := tmux.KillWindow(l.Session, ts.Window); err != nil {
			l.record(state.Event{TaskID: id, Kind: "watch_error",
				Detail: "kill timed-out window: " + err.Error()})
		}
		reason := fmt.Sprintf(
			"worker %s exceeded wall_clock_timeout_seconds (%s of %s) and was stopped",
			ts.RunID, ran.Round(time.Second), timeout)
		l.failRun(id, ts, reason, "worker_timeout")
	}
	return nil
}

// failRun ends a task whose worker never reported.
//
// Any spend the worker did incur is recorded first, so a run that burned money
// before dying still counts against the caps.
func (l *Loop) failRun(id string, ts *state.TaskState, reason, kind string) {
	l.recordSpend(ts)
	l.setStatus(id, state.StatusFailed, reason)
	l.emit(state.Event{TaskID: id, Kind: kind, Detail: reason})
}

// onWorkerExit is the completion path. It is invoked only after the process
// has exited, so the report file cannot be read mid-write.
//
// Cost is recorded before any decision, so a task cannot spend past its cap
// by way of a decision taken on stale spend.
func (l *Loop) onWorkerExit(taskID string, exitCode int) error {
	return l.Complete(taskID)
}

// recordSpend adds a finished run's cost to both the task total and the
// project's daily ledger.
func (l *Loop) recordSpend(ts *state.TaskState) error {
	raw, err := os.ReadFile(worker.RawOutputPath(l.runsDir(), ts.RunID))
	if err != nil {
		return nil // no output to account for
	}
	env, err := worker.ParseEnvelope(raw)
	if err != nil || env.TotalCostUSD == 0 {
		return nil
	}
	_, err = l.Store.Update(func(st *state.State) error {
		t := st.Tasks[ts.ID]
		if t == nil {
			return nil
		}
		t.SpendUSD += env.TotalCostUSD
		st.AddSpend(l.now(), l.Loc, env.TotalCostUSD)
		return nil
	})
	return err
}

// Caps builds the budget ceilings from config.
func (l *Loop) Caps() budget.Caps {
	return budget.Caps{
		PerWorker:    l.Cfg.PerWorkerBudgetUSD,
		PerTask:      l.Cfg.PerTaskCostCapUSD,
		PerDay:       l.Cfg.ProjectCostCapUSDPerDay,
		SafetyMargin: l.Cfg.BudgetSafetyMargin,
		MinSpawn:     l.Cfg.MinSpawnBudgetUSD,
	}
}

// BudgetFor returns the budget for a task's next worker, refusing before the
// spawn rather than discovering a breach after one.
func (l *Loop) BudgetFor(taskID string) (float64, error) {
	st, err := l.Store.Read()
	if err != nil {
		return 0, err
	}
	var taskSpent float64
	if ts := st.Tasks[taskID]; ts != nil {
		taskSpent = ts.SpendUSD
	}
	return budget.ForNextWorker(l.Caps(), taskSpent, st.DailySpend(l.now(), l.Loc))
}

// SlotAvailable reports whether the concurrency cap permits another task to
// start. Slots are counted in tasks, not workers: an implementer handing over
// to a verifier inside the same task never contends for a second slot.
func (l *Loop) SlotAvailable(excludingTask string) (bool, error) {
	st, err := l.Store.Read()
	if err != nil {
		return false, err
	}
	inFlight := 0
	for id, ts := range st.Tasks {
		if id == excludingTask {
			continue
		}
		if ts.Status.InFlight() {
			inFlight++
		}
	}
	return inFlight < l.Cfg.ConcurrencyCap, nil
}

// CleanVerifyTests removes verifier-authored tests from a worktree.
//
// They are deleted between cycles so the next implementer neither sees them
// nor can adapt its implementation to them, and so they never reach a diff a
// worker reads.
func CleanVerifyTests(worktree, suffix string) ([]string, error) {
	if suffix == "" {
		return nil, nil
	}
	var removed []string
	err := filepath.WalkDir(worktree, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
			rel, _ := filepath.Rel(worktree, path)
			removed = append(removed, rel)
		}
		return nil
	})
	return removed, err
}

// BranchName is the branch for a task attempt. Branches are namespaced by
// attempt from the start, so a reframe never collides with the attempt it is
// abandoning and the failed attempt stays readable.
func BranchName(taskID string, attempt int) string {
	return fmt.Sprintf("crew/%s/attempt-%d", taskID, attempt)
}

// WorktreePath is the worktree for a task attempt, namespaced to match.
func WorktreePath(root, taskID string, attempt int) string {
	return filepath.Join(root, ".crew", "worktrees", taskID, fmt.Sprintf("attempt-%d", attempt))
}
