// Package state owns .crew/state.json and .crew/events.jsonl.
//
// Everything here is mechanical and crew-authored; no worker and no LLM ever
// writes it. Read-modify-writes are guarded by an advisory lock on a separate
// lock file and committed by atomic rename, so a reader never observes a torn
// write and two crew processes never interleave.
//
// Writes follow intent-before-effect: a task records what it is about to do
// (with the identifiers the effect will use) before attempting it, so a crash
// between intent and effect is detectable and repairable on restart.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Version is the on-disk schema version.
const Version = 1

// Status is a task's lifecycle position. Each has a documented exit.
type Status string

const (
	StatusPending        Status = "pending"
	StatusQueued         Status = "queued"
	StatusSpawning       Status = "spawning"
	StatusRunning        Status = "running"
	StatusVerifying      Status = "verifying"
	StatusVerifyFailed   Status = "verify_failed"
	StatusNeedsReframe   Status = "needs_reframe"
	StatusBlocked        Status = "blocked"
	StatusFailed         Status = "failed"
	StatusReadyForReview Status = "ready_for_review"
	StatusApproved       Status = "approved"
	StatusLandConflict   Status = "land_conflict"
	StatusLanded         Status = "landed"
	StatusTornDown       Status = "torn_down"
)

// InFlight reports whether the task occupies a concurrency slot. A slot is
// held from spawn through landed or terminal failure, regardless of which
// worker is active inside it.
func (s Status) InFlight() bool {
	switch s {
	case StatusQueued, StatusSpawning, StatusRunning, StatusVerifying,
		StatusVerifyFailed, StatusReadyForReview, StatusApproved, StatusLandConflict:
		return true
	}
	return false
}

// Terminal reports whether the task has reached a resting state that crew
// will not advance on its own.
func (s Status) Terminal() bool {
	switch s {
	case StatusLanded, StatusTornDown, StatusBlocked, StatusFailed:
		return true
	}
	return false
}

// IntentAction names a side effect that is recorded before it is attempted.
type IntentAction string

const (
	IntentSpawnWindow  IntentAction = "spawn_window"
	IntentCreateWorktr IntentAction = "create_worktree"
	IntentLand         IntentAction = "land"
)

// Intent is a durable record of an effect crew is about to perform.
type Intent struct {
	Action   IntentAction `json:"action"`
	Window   string       `json:"window,omitempty"`
	RunID    string       `json:"run_id,omitempty"`
	Worktree string       `json:"worktree,omitempty"`
	Branch   string       `json:"branch,omitempty"`
	At       time.Time    `json:"at"`
}

// TaskState is crew's mechanical record for one task. TASKS.md holds intent;
// this holds everything that changes as the task runs.
type TaskState struct {
	ID     string `json:"id"`
	Status Status `json:"status"`

	// Attempt increments only on a confirmed reframe and namespaces the
	// branch and worktree. Cycle counts implementer spawns within an attempt
	// and resets to 0 on reframe.
	Attempt int `json:"attempt"`
	Cycle   int `json:"cycle"`

	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Window   string `json:"window,omitempty"`
	Role     string `json:"role,omitempty"`
	RunID    string `json:"run_id,omitempty"`

	// RunStartedAt is when the active worker was spawned. It is what makes a
	// wall-clock timeout enforceable: without it, a worker that hangs is
	// indistinguishable from one that is simply taking a while.
	RunStartedAt time.Time `json:"run_started_at,omitempty"`

	SpendUSD    float64 `json:"spend_usd"`
	HeadSha     string  `json:"head_sha,omitempty"`
	ApprovedSha string  `json:"approved_sha,omitempty"`

	// DegradedCounts tracks, per criterion description, how many times a
	// check-tagged criterion's negative control fell back to judgment.
	DegradedCounts map[string]int `json:"degraded_counts,omitempty"`

	PendingIntent *Intent   `json:"pending_intent,omitempty"`
	Notes         string    `json:"notes,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// NoteDegraded records one negative-control downgrade for a criterion.
func (t *TaskState) NoteDegraded(criterion string) {
	if t.DegradedCounts == nil {
		t.DegradedCounts = map[string]int{}
	}
	t.DegradedCounts[criterion]++
}

// DegradedCount returns how many times a criterion has been downgraded.
func (t *TaskState) DegradedCount(criterion string) int { return t.DegradedCounts[criterion] }

// ShouldSuggestRetag reports whether crew should surface a suggestion to
// re-tag this criterion as judged in TASKS.md. It is only ever a suggestion
// shown to the captain; TASKS.md stays human-authored.
func (t *TaskState) ShouldSuggestRetag(criterion string) bool {
	return t.DegradedCount(criterion) >= 2
}

// Spend is the project-wide daily ledger. Date is a calendar date in the
// configured budget timezone.
type Spend struct {
	Date     string  `json:"date"`
	TotalUSD float64 `json:"total_usd"`
}

// State is the whole of .crew/state.json.
type State struct {
	Version        int                   `json:"version"`
	Tasks          map[string]*TaskState `json:"tasks"`
	Spend          Spend                 `json:"spend"`
	WatchHeartbeat time.Time             `json:"watch_heartbeat"`
	WatchPID       int                   `json:"watch_pid,omitempty"`
}

// NewState returns an empty state at the current schema version.
func NewState() *State {
	return &State{Version: Version, Tasks: map[string]*TaskState{}}
}

// Upsert inserts or replaces a task's state and stamps UpdatedAt.
func (s *State) Upsert(ts *TaskState) {
	if s.Tasks == nil {
		s.Tasks = map[string]*TaskState{}
	}
	ts.UpdatedAt = time.Now().UTC()
	s.Tasks[ts.ID] = ts
}

// TasksInFlight counts tasks holding a concurrency slot. It counts tasks, not
// workers: an internal implementer/verifier handoff never contends separately.
func (s *State) TasksInFlight() int {
	n := 0
	for _, t := range s.Tasks {
		if t.Status.InFlight() {
			n++
		}
	}
	return n
}

// PendingIntents returns tasks with an unfinished intent, sorted by id, for
// startup repair.
func (s *State) PendingIntents() []*TaskState {
	var out []*TaskState
	for _, t := range s.Tasks {
		if t.PendingIntent != nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ledgerDate(now time.Time, loc *time.Location) string {
	return now.In(loc).Format("2006-01-02")
}

// AddSpend adds to the daily ledger, rolling it over when the calendar date
// in the budget timezone has changed.
func (s *State) AddSpend(now time.Time, loc *time.Location, usd float64) {
	d := ledgerDate(now, loc)
	if s.Spend.Date != d {
		s.Spend = Spend{Date: d}
	}
	s.Spend.TotalUSD += usd
}

// DailySpend returns spend for the current budget-timezone day, treating a
// stale ledger date as zero rather than carrying yesterday's total forward.
func (s *State) DailySpend(now time.Time, loc *time.Location) float64 {
	if s.Spend.Date != ledgerDate(now, loc) {
		return 0
	}
	return s.Spend.TotalUSD
}

// Event is one line of .crew/events.jsonl.
type Event struct {
	At      time.Time `json:"at"`
	TaskID  string    `json:"task_id,omitempty"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail,omitempty"`
	Sha     string    `json:"sha,omitempty"`
	Ratio   string    `json:"mechanical_vs_judged,omitempty"`
	Payload any       `json:"payload,omitempty"`
}

// Store is a project-local state directory.
type Store struct {
	dir string
	loc *time.Location
}

// Open prepares .crew/ under projectRoot for reading and writing.
func Open(projectRoot string, loc *time.Location) (*Store, error) {
	dir := filepath.Join(projectRoot, ".crew")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create .crew: %w", err)
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Store{dir: dir, loc: loc}, nil
}

func (s *Store) Dir() string        { return s.dir }
func (s *Store) StatePath() string  { return filepath.Join(s.dir, "state.json") }
func (s *Store) EventsPath() string { return filepath.Join(s.dir, "events.jsonl") }
func (s *Store) lockPath() string   { return filepath.Join(s.dir, "state.lock") }

// Read returns the current state, or an empty state if none exists yet.
func (s *Store) Read() (*State, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.readLocked()
}

func (s *Store) readLocked() (*State, error) {
	raw, err := os.ReadFile(s.StatePath())
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if len(raw) == 0 {
		return NewState(), nil
	}
	st := NewState()
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if st.Tasks == nil {
		st.Tasks = map[string]*TaskState{}
	}
	return st, nil
}

// Update performs a locked read-modify-write. If fn returns an error nothing
// is written and the error is propagated, so a failed mutation cannot leave
// partial state behind.
func (s *Store) Update(fn func(*State) error) (*State, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	if err := fn(st); err != nil {
		return nil, err
	}
	st.Version = Version
	if err := s.writeLocked(st); err != nil {
		return nil, err
	}
	return st, nil
}

// writeLocked commits state by writing a sibling temp file, flushing it, and
// renaming over the target. Rename within a directory is atomic, so a
// concurrent reader sees either the old file or the new one.
func (s *Store) writeLocked(st *State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(s.dir, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.StatePath()); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	return nil
}

// Append adds one event to events.jsonl. Events are append-only and are the
// single home for escalation history.
func (s *Store) Append(ev Event) error {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	f, err := os.OpenFile(s.EventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open events: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// lock takes an exclusive advisory lock. The lock lives on its own file so it
// is unaffected by state.json being replaced via rename. Each call opens a
// fresh descriptor, so the lock serializes goroutines within a process as
// well as separate crew processes.
func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
