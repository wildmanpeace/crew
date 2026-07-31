package budget

import (
	"errors"
	"math"
	"testing"
)

func caps() Caps {
	return Caps{PerWorker: 1.50, PerTask: 5.00, PerDay: 25.00, SafetyMargin: 0.25, MinSpawn: 0.10}
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// With plenty of headroom the per-worker cap binds, reduced by the margin.
func TestPerWorkerCapBindsWhenHeadroomIsLarge(t *testing.T) {
	got, err := ForNextWorker(caps(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, got, 1.50*0.75)
}

func TestTaskRemainingBindsWhenSmaller(t *testing.T) {
	// $4.30 already spent on this task leaves $0.70 of the $5 cap.
	got, err := ForNextWorker(caps(), 4.30, 0)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, got, 0.70*0.75)
}

func TestDailyRemainingBindsWhenSmallest(t *testing.T) {
	// $24.60 spent today leaves $0.40 project-wide.
	got, err := ForNextWorker(caps(), 0, 24.60)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, got, 0.40*0.75)
}

// Phase 0 measured a 3.5x overshoot, so the margin must actually shrink the
// number handed to the worker.
func TestSafetyMarginShrinksTheBudget(t *testing.T) {
	c := caps()
	withMargin, _ := ForNextWorker(c, 0, 0)
	c.SafetyMargin = 0
	without, _ := ForNextWorker(c, 0, 0)
	if !(withMargin < without) {
		t.Fatalf("margin did not shrink the budget: %v vs %v", withMargin, without)
	}
}

// The cap must be enforced before spawning, not discovered after a worker exits.
func TestTaskCapExhaustedIsRefusedBeforeSpawn(t *testing.T) {
	_, err := ForNextWorker(caps(), 5.00, 0)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestDailyCapExhaustedIsRefusedBeforeSpawn(t *testing.T) {
	_, err := ForNextWorker(caps(), 0, 25.00)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestOverspentTaskIsRefused(t *testing.T) {
	_, err := ForNextWorker(caps(), 7.50, 0)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

// A sliver of headroom is not worth spawning into: the worker would be killed
// mid-turn and still overshoot.
func TestRemainingBelowMinSpawnIsRefused(t *testing.T) {
	// $4.95 spent leaves $0.05, which is under the $0.10 floor.
	_, err := ForNextWorker(caps(), 4.95, 0)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestJustAboveMinSpawnIsAllowed(t *testing.T) {
	c := caps()
	c.SafetyMargin = 0
	// $4.80 spent leaves $0.20, above the $0.10 floor.
	got, err := ForNextWorker(c, 4.80, 0)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	approx(t, got, 0.20)
}

func TestWouldExceedReportsBreachWithoutSpawning(t *testing.T) {
	if !WouldExceedTask(caps(), 5.00) {
		t.Error("task cap breach not reported")
	}
	if WouldExceedTask(caps(), 1.00) {
		t.Error("false task cap breach")
	}
	if !WouldExceedDay(caps(), 25.10) {
		t.Error("daily cap breach not reported")
	}
	if WouldExceedDay(caps(), 1.00) {
		t.Error("false daily cap breach")
	}
}

func TestZeroCapsAreTreatedAsUnlimited(t *testing.T) {
	c := Caps{PerWorker: 1.0, SafetyMargin: 0, MinSpawn: 0.10}
	got, err := ForNextWorker(c, 100, 100)
	if err != nil {
		t.Fatalf("unset caps should not refuse: %v", err)
	}
	approx(t, got, 1.0)
}
