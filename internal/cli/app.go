// Package cli implements crew's command surface.
//
// The commands that change what lands are deliberately narrow. crew review
// presents data and forms no judgment; crew approve is structurally
// captain-only; crew land re-checks the approved sha at land time rather than
// trusting the approval alone.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tasks"
)

// App carries everything a command needs. The indirection on Now and IsTTY
// exists so the safety rules can be tested without a terminal or a clock.
type App struct {
	Root   string
	Cfg    *config.Config
	Store  *state.Store
	Repo   gitx.Repo
	Loc    *time.Location
	Stdout io.Writer
	Stderr io.Writer

	Now   func() time.Time
	IsTTY func() bool

	// Notify, when set, delivers this App's events the way crew watch delivers
	// its own. It stays nil for commands the captain runs themselves, whose
	// output they are already reading. crew watch sets it, because once the
	// loop lands a task the captain is no longer watching a command run, and a
	// land that conflicted would otherwise be silent.
	Notify func(state.Event)

	// Session is the tmux session worker windows live in.
	Session string
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

func (a *App) tty() bool {
	if a.IsTTY != nil {
		return a.IsTTY()
	}
	return isTerminal(os.Stdin)
}

func (a *App) out(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}

// TasksPath is the project's hand-authored task list.
func (a *App) TasksPath() string { return filepath.Join(a.Root, "TASKS.md") }

// Tasks parses TASKS.md.
func (a *App) Tasks() ([]tasks.Task, error) { return tasks.ParseFile(a.TasksPath()) }

// Task returns one declared task.
func (a *App) Task(id string) (tasks.Task, error) {
	all, err := a.Tasks()
	if err != nil {
		return tasks.Task{}, err
	}
	t, ok := tasks.ByID(all)[id]
	if !ok {
		return tasks.Task{}, fmt.Errorf("task %q is not declared in %s", id, a.TasksPath())
	}
	return t, nil
}

// taskState returns a task's mechanical state.
func (a *App) taskState(id string) (*state.TaskState, error) {
	st, err := a.Store.Read()
	if err != nil {
		return nil, err
	}
	ts := st.Tasks[id]
	if ts == nil {
		return nil, fmt.Errorf("task %q has no recorded state; has it been spawned?", id)
	}
	return ts, nil
}

// BranchName and WorktreePath mirror the loop's naming. Branches are
// namespaced by attempt from the start so a reframe never collides with the
// attempt it abandons.
func BranchName(taskID string, attempt int) string {
	return fmt.Sprintf("crew/%s/attempt-%d", taskID, attempt)
}

// WorktreePath is the worktree for a task attempt.
func WorktreePath(root, taskID string, attempt int) string {
	return filepath.Join(root, ".crew", "worktrees", taskID, fmt.Sprintf("attempt-%d", attempt))
}

func (a *App) emit(ev state.Event) {
	if ev.At.IsZero() {
		ev.At = a.now()
	}
	a.Store.Append(ev)
	if a.Notify != nil {
		a.Notify(ev)
	}
}
