// Package budget decides how much a worker may spend, before it is spawned.
//
// A cap that can only be found breached is a report, not a control, so every
// decision here happens ahead of the spawn. The number handed to a worker is
// deliberately smaller than the true headroom: Phase 0 measured
// --max-budget-usd overshooting its cap by 3.5x, because the cap is applied
// after the turn that breaches it rather than before.
package budget

import (
	"errors"
	"fmt"
	"math"
)

// ErrExhausted means no worker may be spawned right now.
var ErrExhausted = errors.New("budget exhausted")

// Caps are the ceilings a spawn is checked against. A zero ceiling means
// unlimited, so a partially configured project is not accidentally frozen.
type Caps struct {
	PerWorker    float64
	PerTask      float64
	PerDay       float64
	SafetyMargin float64
	MinSpawn     float64
}

// ForNextWorker returns the budget to pass as --max-budget-usd, or
// ErrExhausted if the spawn must be refused.
func ForNextWorker(c Caps, taskSpent, daySpent float64) (float64, error) {
	if WouldExceedTask(c, taskSpent) {
		return 0, fmt.Errorf("%w: task has spent $%.2f of its $%.2f cap", ErrExhausted, taskSpent, c.PerTask)
	}
	if WouldExceedDay(c, daySpent) {
		return 0, fmt.Errorf("%w: project has spent $%.2f of its $%.2f daily cap", ErrExhausted, daySpent, c.PerDay)
	}

	limit := c.PerWorker
	if c.PerTask > 0 {
		limit = math.Min(limit, c.PerTask-taskSpent)
	}
	if c.PerDay > 0 {
		limit = math.Min(limit, c.PerDay-daySpent)
	}
	if limit < c.MinSpawn {
		return 0, fmt.Errorf("%w: only $%.2f of headroom remains, below the $%.2f minimum",
			ErrExhausted, limit, c.MinSpawn)
	}

	// Shrink by the safety margin only after the floor check, so the floor
	// describes real headroom rather than the already-discounted figure.
	return limit * (1 - c.SafetyMargin), nil
}

// WouldExceedTask reports whether a task has reached its cumulative cap
// across all of its cycles and attempts.
func WouldExceedTask(c Caps, taskSpent float64) bool {
	return c.PerTask > 0 && taskSpent >= c.PerTask
}

// WouldExceedDay reports whether the project has reached its daily cap.
func WouldExceedDay(c Caps, daySpent float64) bool {
	return c.PerDay > 0 && daySpent >= c.PerDay
}
