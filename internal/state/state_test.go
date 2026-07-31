package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func denver(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), denver(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadOnMissingFileReturnsEmptyState(t *testing.T) {
	st, err := newStore(t).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(st.Tasks) != 0 {
		t.Errorf("Tasks = %v, want empty", st.Tasks)
	}
	if st.Version != Version {
		t.Errorf("Version = %d, want %d", st.Version, Version)
	}
}

func TestUpdatePersists(t *testing.T) {
	s := newStore(t)
	if _, err := s.Update(func(st *State) error {
		st.Upsert(&TaskState{ID: "alpha", Status: StatusRunning, Attempt: 1, Cycle: 2})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	got := st.Tasks["alpha"]
	if got == nil || got.Status != StatusRunning || got.Attempt != 1 || got.Cycle != 2 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

// An error from the mutator must leave the file untouched.
func TestUpdateRollsBackOnError(t *testing.T) {
	s := newStore(t)
	s.Update(func(st *State) error {
		st.Upsert(&TaskState{ID: "alpha", Status: StatusPending})
		return nil
	})
	wantErr := os.ErrInvalid
	if _, err := s.Update(func(st *State) error {
		st.Upsert(&TaskState{ID: "beta", Status: StatusRunning})
		return wantErr
	}); err == nil {
		t.Fatal("Update returned nil, want the mutator's error")
	}
	st, _ := s.Read()
	if _, ok := st.Tasks["beta"]; ok {
		t.Error("failed update was persisted anyway")
	}
	if _, ok := st.Tasks["alpha"]; !ok {
		t.Error("failed update clobbered prior state")
	}
}

// Concurrent read-modify-writes must serialize; every increment must survive.
func TestUpdateIsSerializedUnderConcurrency(t *testing.T) {
	s := newStore(t)
	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			s.Update(func(st *State) error {
				ts := st.Tasks["alpha"]
				if ts == nil {
					ts = &TaskState{ID: "alpha"}
					st.Upsert(ts)
				}
				ts.Cycle++
				return nil
			})
		}()
	}
	wg.Wait()
	st, _ := s.Read()
	if got := st.Tasks["alpha"].Cycle; got != n {
		t.Fatalf("Cycle = %d, want %d (lost updates)", got, n)
	}
}

// The file on disk must always be complete and parseable, never a partial write.
func TestWriteIsAtomic(t *testing.T) {
	s := newStore(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			s.Update(func(st *State) error {
				ts := &TaskState{ID: "alpha", Status: StatusRunning}
				ts.Notes = strings.Repeat("x", 4096)
				st.Upsert(ts)
				return nil
			})
		}
	}()
	for range 200 {
		raw, err := os.ReadFile(s.StatePath())
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var probe State
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("observed a torn write: %v", err)
		}
	}
	<-done
}

func TestUpdateLeavesNoTempFiles(t *testing.T) {
	s := newStore(t)
	for range 5 {
		s.Update(func(st *State) error {
			st.Upsert(&TaskState{ID: "alpha"})
			return nil
		})
	}
	entries, err := os.ReadDir(filepath.Dir(s.StatePath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// Intent-before-effect: the intent is durable before the effect is attempted,
// so a crash between the two is detectable on restart.
func TestPendingIntentSurvivesAndClears(t *testing.T) {
	s := newStore(t)
	s.Update(func(st *State) error {
		ts := &TaskState{ID: "alpha", Status: StatusSpawning}
		ts.PendingIntent = &Intent{Action: IntentSpawnWindow, Window: "crew-alpha-a1c0", RunID: "r1"}
		st.Upsert(ts)
		return nil
	})
	st, _ := s.Read()
	pending := st.PendingIntents()
	if len(pending) != 1 || pending[0].ID != "alpha" {
		t.Fatalf("PendingIntents = %+v, want alpha", pending)
	}
	if got := st.Tasks["alpha"].PendingIntent.Window; got != "crew-alpha-a1c0" {
		t.Errorf("Window = %q", got)
	}
	s.Update(func(st *State) error {
		st.Tasks["alpha"].PendingIntent = nil
		return nil
	})
	st, _ = s.Read()
	if len(st.PendingIntents()) != 0 {
		t.Error("intent survived being cleared")
	}
}

func TestSpendAccumulatesWithinSameDay(t *testing.T) {
	loc := denver(t)
	st := NewState()
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, loc)
	st.AddSpend(now, loc, 1.25)
	st.AddSpend(now.Add(2*time.Hour), loc, 0.75)
	if got := st.DailySpend(now, loc); got != 2.0 {
		t.Fatalf("DailySpend = %v, want 2.0", got)
	}
}

// The ledger rolls over at midnight in the configured timezone, not UTC.
func TestSpendRollsOverAtBudgetTimezoneMidnight(t *testing.T) {
	loc := denver(t)
	st := NewState()
	before := time.Date(2026, 7, 30, 23, 30, 0, 0, loc)
	st.AddSpend(before, loc, 4.00)
	after := time.Date(2026, 7, 31, 0, 30, 0, 0, loc)
	if got := st.DailySpend(after, loc); got != 0 {
		t.Fatalf("DailySpend after rollover = %v, want 0", got)
	}
	st.AddSpend(after, loc, 1.00)
	if got := st.DailySpend(after, loc); got != 1.00 {
		t.Fatalf("DailySpend = %v, want 1.00", got)
	}
}

// 23:30 Denver is already the next UTC day; the ledger must not roll over then.
func TestSpendDoesNotRollOverOnUTCMidnight(t *testing.T) {
	loc := denver(t)
	st := NewState()
	evening := time.Date(2026, 7, 30, 18, 0, 0, 0, loc)
	st.AddSpend(evening, loc, 3.00)
	lateSameLocalDay := time.Date(2026, 7, 30, 23, 30, 0, 0, loc)
	if lateSameLocalDay.UTC().Day() == lateSameLocalDay.Day() {
		t.Skip("timezone offset assumption no longer holds")
	}
	if got := st.DailySpend(lateSameLocalDay, loc); got != 3.00 {
		t.Fatalf("DailySpend = %v, want 3.00 (UTC rollover must not reset it)", got)
	}
}

// The ledger is read from disk, so it survives a crew watch restart.
func TestSpendSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	loc := denver(t)
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, loc)
	s1, _ := Open(dir, loc)
	s1.Update(func(st *State) error {
		st.AddSpend(now, loc, 2.50)
		return nil
	})
	s2, _ := Open(dir, loc)
	st, _ := s2.Read()
	if got := st.DailySpend(now, loc); got != 2.50 {
		t.Fatalf("DailySpend after reopen = %v, want 2.50", got)
	}
}

func TestTaskSpendAccumulatesAcrossCyclesAndAttempts(t *testing.T) {
	s := newStore(t)
	for range 3 {
		s.Update(func(st *State) error {
			ts := st.Tasks["alpha"]
			if ts == nil {
				ts = &TaskState{ID: "alpha"}
				st.Upsert(ts)
			}
			ts.SpendUSD += 1.10
			return nil
		})
	}
	st, _ := s.Read()
	if got := st.Tasks["alpha"].SpendUSD; got < 3.29 || got > 3.31 {
		t.Fatalf("SpendUSD = %v, want ~3.30", got)
	}
}

func TestEventsAppendAsJSONL(t *testing.T) {
	s := newStore(t)
	if err := s.Append(Event{TaskID: "alpha", Kind: "ready_for_review", Detail: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{TaskID: "beta", Kind: "verify_failed", Detail: "second"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("line 2 is not valid JSON: %v", err)
	}
	if ev.TaskID != "beta" || ev.Kind != "verify_failed" {
		t.Errorf("event = %+v", ev)
	}
	if ev.At.IsZero() {
		t.Error("event has no timestamp")
	}
}

func TestStatusClassification(t *testing.T) {
	inFlight := []Status{StatusQueued, StatusSpawning, StatusRunning, StatusVerifying, StatusVerifyFailed, StatusReadyForReview, StatusApproved, StatusLandConflict}
	for _, s := range inFlight {
		if !s.InFlight() {
			t.Errorf("%s.InFlight() = false, want true", s)
		}
	}
	notInFlight := []Status{StatusPending, StatusLanded, StatusTornDown, StatusBlocked, StatusFailed, StatusNeedsReframe}
	for _, s := range notInFlight {
		if s.InFlight() {
			t.Errorf("%s.InFlight() = true, want false", s)
		}
	}
	for _, s := range []Status{StatusLanded, StatusTornDown, StatusBlocked, StatusFailed} {
		if !s.Terminal() {
			t.Errorf("%s.Terminal() = false, want true", s)
		}
	}
	if StatusRunning.Terminal() {
		t.Error("running must not be terminal")
	}
}

// Concurrency is counted in tasks, not workers.
func TestTasksInFlightCountsTasksNotWorkers(t *testing.T) {
	st := NewState()
	st.Upsert(&TaskState{ID: "a", Status: StatusRunning, Role: "implementer"})
	st.Upsert(&TaskState{ID: "b", Status: StatusVerifying, Role: "verifier"})
	st.Upsert(&TaskState{ID: "c", Status: StatusLanded})
	st.Upsert(&TaskState{ID: "d", Status: StatusPending})
	if got := st.TasksInFlight(); got != 2 {
		t.Fatalf("TasksInFlight = %d, want 2", got)
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	s.Update(func(st *State) error {
		st.WatchHeartbeat = now
		st.WatchPID = 4242
		return nil
	})
	st, _ := s.Read()
	if !st.WatchHeartbeat.Equal(now) {
		t.Errorf("WatchHeartbeat = %v, want %v", st.WatchHeartbeat, now)
	}
	if st.WatchPID != 4242 {
		t.Errorf("WatchPID = %d", st.WatchPID)
	}
}

func TestDegradedCountTracksPerCriterion(t *testing.T) {
	s := newStore(t)
	for range 2 {
		s.Update(func(st *State) error {
			ts := st.Tasks["alpha"]
			if ts == nil {
				ts = &TaskState{ID: "alpha"}
				st.Upsert(ts)
			}
			ts.NoteDegraded("Requests over the configured rate return HTTP 429.")
			return nil
		})
	}
	st, _ := s.Read()
	ts := st.Tasks["alpha"]
	if got := ts.DegradedCount("Requests over the configured rate return HTTP 429."); got != 2 {
		t.Fatalf("DegradedCount = %d, want 2", got)
	}
	if !ts.ShouldSuggestRetag("Requests over the configured rate return HTTP 429.") {
		t.Error("two degradations should trigger the re-tag suggestion")
	}
	if ts.ShouldSuggestRetag("some other criterion") {
		t.Error("unrelated criterion should not trigger the suggestion")
	}
}
