// Command crew orchestrates the implement/verify/land loop for one project.
//
// The captain-facing commands are deliberately asymmetric. crew review is
// read-only and safe for an agent to run; crew approve refuses without a
// terminal so only a person can invoke it; crew land re-checks the approved
// sha rather than trusting the approval alone.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wildmanpeace/crew/internal/cli"
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/hook"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tmux"
	"github.com/wildmanpeace/crew/internal/watch"
	"github.com/wildmanpeace/crew/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "hook-gate":
		os.Exit(cmdHookGate(args))
	case "worker":
		os.Exit(cmdWorker(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	}

	app, err := newApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew: %v\n", err)
		os.Exit(1)
	}

	if err := dispatch(app, cmd, args); err != nil {
		fmt.Fprintf(os.Stderr, "crew %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func dispatch(app *cli.App, cmd string, args []string) error {
	switch cmd {
	case "spawn":
		fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
		force := fs.Bool("force", false, "override refused preconditions")
		id, err := taskArg(fs, args, "crew spawn <task-id> [--force]")
		if err != nil {
			return err
		}
		return app.Spawn(id, *force)

	case "review":
		id, err := requireArg(args, "crew review <task-id>")
		if err != nil {
			return err
		}
		return app.Review(id)

	case "approve":
		fs := flag.NewFlagSet("approve", flag.ContinueOnError)
		head := fs.String("head", "", "the branch head sha being approved")
		id, err := taskArg(fs, args, "crew approve <task-id> --head <sha>")
		if err != nil {
			return err
		}
		return app.Approve(id, *head)

	case "land":
		id, err := requireArg(args, "crew land <task-id>")
		if err != nil {
			return err
		}
		return app.Land(id)

	case "reframe":
		id, err := requireArg(args, "crew reframe <task-id>")
		if err != nil {
			return err
		}
		return app.Reframe(id)

	case "rebase":
		id, err := requireArg(args, "crew rebase <task-id>")
		if err != nil {
			return err
		}
		return app.Rebase(id)

	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "machine-readable output")
		asMD := fs.Bool("markdown", false, "markdown table output")
		if _, err := cli.ParseArgs(fs, args); err != nil {
			return err
		}
		return app.PrintStatus(*asJSON, *asMD)

	case "peek":
		fs := flag.NewFlagSet("peek", flag.ContinueOnError)
		lines := fs.Int("lines", 200, "how many lines of the pane to show")
		id, err := taskArg(fs, args, "crew peek <task-id> [--lines N]")
		if err != nil {
			return err
		}
		return app.Peek(id, *lines)

	case "teardown":
		fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
		rm := fs.Bool("remove-worktree", false, "also remove the worktree")
		id, err := taskArg(fs, args, "crew teardown <task-id> [--remove-worktree]")
		if err != nil {
			return err
		}
		return app.Teardown(id, *rm)

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		notify := fs.Bool("notify", false, "emit a notification when problems are found")
		if _, err := cli.ParseArgs(fs, args); err != nil {
			return err
		}
		err := app.Doctor()
		if err != nil && *notify {
			cli.Notify("crew doctor", err.Error())
		}
		return err

	case "gc":
		return app.GC()

	case "verify":
		fs := flag.NewFlagSet("verify", flag.ContinueOnError)
		force := fs.Bool("force", false, "required: this is a debug-only command")
		id, err := taskArg(fs, args, "crew verify <task-id> --force")
		if err != nil {
			return err
		}
		if !*force {
			return fmt.Errorf("crew verify is debug-only; pass --force")
		}
		return runWatch(app, func(l *watch.Loop) error { return l.VerifyNow(id) })

	case "send":
		if len(args) < 2 {
			return fmt.Errorf("usage: crew send <task-id> \"<text>\"")
		}
		return app.Send(args[0], args[1])

	case "watch":
		return runWatchLoop(app)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func requireArg(args []string, use string) (string, error) {
	if len(args) < 1 || args[0] == "" {
		return "", fmt.Errorf("usage: %s", use)
	}
	return args[0], nil
}

// taskArg parses a command's flags and returns its task id.
//
// Flags are permuted ahead of positionals first, so "crew teardown my-task
// --remove-worktree" behaves the same as the flag-first ordering. Go's flag
// package stops at the first positional, which made every flag written after
// a task id silently ineffective.
func taskArg(fs *flag.FlagSet, args []string, use string) (string, error) {
	pos, err := cli.ParseArgs(fs, args)
	if err != nil {
		return "", fmt.Errorf("%w\nusage: %s", err, use)
	}
	if len(pos) == 0 {
		return "", fmt.Errorf("a task id is required\nusage: %s", use)
	}
	if len(pos) > 1 {
		return "", fmt.Errorf("unexpected arguments %q\nusage: %s", pos[1:], use)
	}
	return pos[0], nil
}

// findRoot walks up from the working directory looking for .crew/config.json,
// so crew works from anywhere inside the project.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".crew", "config.json")); err == nil {
			return filepath.EvalSymlinks(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .crew/config.json found in this directory or any parent")
		}
		dir = parent
	}
}

func newApp() (*cli.App, error) {
	root, err := findRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(root, loc)
	if err != nil {
		return nil, err
	}
	return &cli.App{
		Root: root, Cfg: cfg, Store: store, Repo: gitx.New(root), Loc: loc,
		Stdout: os.Stdout, Stderr: os.Stderr, Session: tmux.Session,
	}, nil
}

func newLoop(app *cli.App) (*watch.Loop, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude is not on PATH: %w", err)
	}
	notify := func(ev state.Event) { cli.Notify("crew: "+ev.Kind, ev.TaskID+" "+ev.Detail) }
	// The loop lands through the same App the captain would, so auto-landing
	// and a manual crew land cannot drift apart in what they check.
	app.Notify = notify
	return &watch.Loop{
		Root: app.Root, Cfg: app.Cfg, Store: app.Store, Repo: app.Repo,
		CrewBin: self, ClaudeBin: claudeBin, Session: app.Session, Loc: app.Loc,
		Notify: notify,
		Land:   app.Land,
	}, nil
}

func runWatch(app *cli.App, fn func(*watch.Loop) error) error {
	l, err := newLoop(app)
	if err != nil {
		return err
	}
	return fn(l)
}

// runWatchLoop drives the loop until interrupted. It is never
// self-daemonizing: something external keeps it alive.
func runWatchLoop(app *cli.App) error {
	l, err := newLoop(app)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.WriteWorkerSettings(); err != nil {
		return err
	}
	if err := app.InstallDispatchers(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "crew watch: driving %s (poll %ds, tmux session %q)\n",
		app.Root, app.Cfg.PollIntervalSeconds, app.Session)
	start := time.Now()
	err = l.Run(ctx)
	fmt.Fprintf(os.Stdout, "crew watch: stopped after %s\n", time.Since(start).Round(time.Second))
	return err
}

// cmdHookGate evaluates one PreToolUse payload. It always exits 0: a denial is
// expressed in the JSON body, so a gate decision is never mistaken for a
// crashed hook.
func cmdHookGate(args []string) int {
	fs := flag.NewFlagSet("hook-gate", flag.ContinueOnError)
	role := fs.String("role", "", "worker role: implementer or verifier")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r := config.Role(*role)
	if r != config.RoleImplementer && r != config.RoleVerifier {
		fmt.Fprintf(os.Stderr, "crew hook-gate: --role must be implementer or verifier, got %q\n", *role)
		return 2
	}
	if _, err := hook.Run(r, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "crew hook-gate: %v\n", err)
		return 2
	}
	return 0
}

// cmdWorker runs one worker inside its tmux window. It exits non-zero only
// when crew could not run the worker at all; the worker's own conclusion
// travels through its exit marker and report.
func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	jobPath := fs.String("job", "", "path to the job description")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jobPath == "" {
		fmt.Fprintln(os.Stderr, "crew worker: --job is required")
		return 2
	}
	j, err := worker.ReadJob(*jobPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew worker: %v\n", err)
		return 2
	}
	res, err := worker.RunJob(j)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crew worker: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "\ncrew: run %s finished (exit %d, $%.4f, %d turns)\n",
		res.RunID, res.ExitCode, res.TotalCostUSD, res.NumTurns)
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `crew - agentic orchestration for one project

Captain:
  crew spawn <task-id> [--force]        queue a task's next attempt
  crew review <task-id>                 read-only: results, diff, approve line
  crew approve <task-id> --head <sha>   captain-only; requires a terminal
  crew land <task-id>                   merge an approved task into main
  crew reframe <task-id>                abandon this attempt, start the next
  crew rebase <task-id>                 rebase onto main; invalidates approval
  crew status [--json] [--markdown]     state, spend, watcher health
  crew peek <task-id> [--lines N]       tail a live worker's pane
  crew teardown <task-id> [--remove-worktree]
  crew doctor [--notify]                reconcile state against ground truth
  crew gc                               remove orphans doctor found

Loop:
  crew watch                            drive the implement/verify loop

Debug:
  crew verify <task-id> --force         re-run a verify pass
  crew send <task-id> "<text>"          send keys to a worker's pane

Internal:
  crew hook-gate --role <role>          PreToolUse gate (stdin JSON)
  crew worker --job <path>              run one worker (inside tmux)
`)
}
