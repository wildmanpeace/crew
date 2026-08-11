# crew

crew drives an implement/verify/land loop over one-shot `claude -p` workers,
and puts a mechanically enforced gate in front of anything that reaches `main`.

The loop itself contains no model. `crew watch` is a polling state machine: it
spawns an implementer, spawns a verifier, reads their reports, decides what
happens next, and stops. Every judgment call that could let unverified work
land is either a machine check or a person typing a command — never an agent
deciding it has understood well enough.

> Personal project, built for one workflow. macOS-only in practice (tmux for
> worker windows, `osascript` for notifications), and it assumes the `claude`
> CLI is on your PATH.

## The idea

An agent that both writes code and decides whether the code is good is not
providing evidence, it is providing a claim. crew separates the two roles and
then makes the separation structural:

- **The implementer and the verifier are different processes** with different
  binaries on their PATH. A verifier physically cannot commit, because the
  dispatcher that exposes a `commit` verb is not reachable from its worktree.
- **A verifier's passing test only counts once crew has broken it.** Every
  test the verifier writes gets a negative control: crew rebuilds the branch
  with the implementation removed and confirms the test now fails. A test that
  passes without the implementation is not evidence, and crew says so.
- **Approval requires a terminal.** `crew approve` checks for a TTY and
  refuses without one, so an agent session cannot approve its own work even if
  it talks itself into trying.
- **Landing re-checks the sha.** An approval is bound to a specific commit. If
  `main` moved, the approval is invalid and the task goes back for a fresh
  verify pass.

## Requirements

- Go 1.26+
- `git`, `tmux`
- The `claude` CLI on your PATH
- macOS for desktop notifications (everything else works anywhere)

## Install

```bash
go build -o ~/.local/bin/crew ./cmd/crew
go build -o ~/.local/libexec/crew/crew-run ./cmd/crew-run
go build -o ~/.local/libexec/crew/crew-check ./cmd/crew-check
```

The asymmetry is deliberate. `crew` belongs on your PATH; the two dispatchers
must **not** be on any PATH directory. Workers inherit PATH, and a verifier
that could find `crew-run` anywhere would dissolve the role boundary the whole
design rests on. `crew watch` copies each dispatcher into `.crew/bin/<role>/`
at startup, which is the only place a worker can reach one.

`crew watch` looks for the dispatchers in `$CREW_LIBEXEC`, then
`<crew's dir>/../libexec/crew`, then alongside the `crew` binary itself.

## Set up a project

crew drives a git repository from the outside. In the project you want driven,
create `.crew/config.json` — its presence is what marks the project root, and
crew walks up from your working directory to find it:

```json
{
  "main_branch": "main",
  "implementer_model": "sonnet",
  "verifier_model": "sonnet",
  "check_commands": {
    "test":  { "argv": ["go", "test"],  "default_args": ["./..."] },
    "build": { "argv": ["go", "build"], "default_args": ["./..."] },
    "lint":  { "argv": ["go", "vet"],   "default_args": ["./..."] }
  }
}
```

Every other field has a default; see [Configuration](#configuration).

Check commands are argv arrays, never shell strings. Nothing in crew ever
constructs a shell invocation, so metacharacters in any argument are inert.

Then add `.crew/` working files to the project's `.gitignore` — everything
except `config.json`, which is project intent and belongs in version control:

```gitignore
/.crew/state.json
/.crew/events.jsonl
/.crew/*.lock
/.crew/runs/
/.crew/worktrees/
/.crew/scratch/
/.crew/bin/
/.crew/implementer-settings.json
/.crew/verifier-settings.json
/.crew-report.json
```

## Declare a task

`TASKS.md` at the project root is hand-authored intent, and never carries
status. Status lives exclusively in `.crew/state.json`.

```markdown
## task: allow-refuses-when-empty
- depends_on: none
- paths: ratelimit/**
- brief: >
    Bucket.Allow must return false and consume nothing once tokens are
    exhausted. Do not touch Tokens, Capacity, or Refill.
- acceptance_criteria:
    - check: "crew-check test ./ratelimit/... -run TestAllowRefusesWhenEmpty"
      description: Allow returns false and consumes no token when empty.
    - judged: true
      description: The refill path is left semantically unchanged.
```

Each criterion is **either** a check command **or** `judged: true` — never
both, never neither. `paths` is a mutual-exclusion declaration: two tasks with
overlapping paths cannot be in flight at once, and `crew spawn` refuses the
overlap.

## Run it

```bash
crew watch
```

Leave that running in its own terminal — it never daemonizes itself. Then, in
another:

```bash
crew spawn allow-refuses-when-empty
```

From there the loop runs on its own. It notifies you (desktop + `events.jsonl`)
when it reaches something only you can resolve.

When a task reaches `ready_for_review`:

```bash
crew review allow-refuses-when-empty   # criteria, diff, and the approve line
crew approve allow-refuses-when-empty --head <sha>
crew land allow-refuses-when-empty
```

## The loop

```
pending ──spawn──▶ queued ──▶ running ──▶ verifying
                                 ▲            │
                                 │            ├─ all criteria satisfied ──▶ ready_for_review
                                 └────────────┤                                   │
                                   next cycle │                              approve (TTY)
                                              │                                   │
                                              ├─ cycles exhausted ──▶ needs_reframe▼
                                              │                              approved
                                              ├─ verifier failed twice ──▶ blocked  │
                                              │                                land │
                                              └─ worker died / timed out ─▶ failed  ▼
                                                                                landed
```

`land_conflict` is the fifth resting state: `main` moved after approval, so the
approval no longer describes what would land. `crew rebase` clears the stale
approval and forces a fresh verify pass.

A failed cycle does not get its own status — the task simply goes back to
`running` as the next implementer starts. A verifier that crashes or reports
blocked is retried **once without consuming a cycle**, because the cycle budget
belongs to the implementation attempt, not to the checker's own failure.

## Acceptance criteria and the negative control

This is the part worth understanding before writing criteria.

crew runs a negative control on every test a verifier authors: rebuild the
branch, remove the implementation, re-run the test, and require that it now
fails. Only that transition counts as mechanical evidence. A criterion whose
control doesn't produce it is **downgraded to judgment**, and `crew review`
reports the ratio of mechanical to judged so a green tick can't hide a weak one.

Two consequences shape how you write criteria:

- **Brand-new API usually can't be controlled.** Removing a new symbol makes
  the test fail to compile rather than fail an assertion; crew classifies that
  as a build failure and falls back to judgment. Criteria describing *changes
  to existing behaviour* produce real evidence; criteria describing *entirely
  new surface* tend to end up judged whatever you tag them.
- **A criterion asserting something is *unchanged* must be `judged: true`.** It
  passes at merge-base by construction, so the control refuses it as evidence
  every single cycle and the task burns its whole budget chasing something no
  implementation can satisfy.

When a criterion degrades twice, `crew status` surfaces a suggestion to re-tag
it. The suggestion is advice for a person — crew never edits `TASKS.md`.

## How workers are confined

Each worker runs `claude -p` in its own git worktree, in its own tmux window,
under three independent layers:

1. **Permissions.** The implementer may edit anything in its worktree; the
   verifier may edit only files matching the verify-test suffix, plus its own
   report. Neither has a rule permitting anything else.
2. **A PreToolUse hook.** Claude Code's built-in read-only command set (`cat`,
   `ls`, `grep`, read-only `git`, …) runs unprompted in every permission mode
   and is not configurable, so permissions alone cannot close the shell. The
   gate takes the strict position: the only shell command a worker may run is
   its own dispatcher, in exactly the shape `<dispatcher> <verb> [args...]`.
   Everything else is denied, and a hook deny beats an allowing permission rule.
3. **The dispatcher itself.** `crew-run` (implementer) exposes `test`, `lint`,
   `build`, `diff`, `commit`. `crew-check` (verifier) exposes `test`, `lint`,
   `build` — there is no commit verb to permit. Each binary hard-codes its role
   at compile time; the role is never read from the environment, because a
   worker controls its own environment.

A worker's *context* is confined the same way. Auto memory is off, so nothing
accumulates between one-shot runs, and `claudeMdExcludes` covers both the
captain's global `~/.claude/CLAUDE.md` and everything under `.crew/worktrees/`.
That second one matters more than it looks: a worktree is a full checkout, so
without it a worker would load whatever memory files the project wrote for the
captain's interactive session. A worker's context is its brief and the code.

Budgets are checked before a spawn, not discovered after. Each worker's budget
is shrunk by a safety margin because `--max-budget-usd` is applied *after* the
turn that breaches it — measured overshooting its cap by 3.5x — so a worker
handed exactly the remaining headroom can still blow through the ceiling.

## Commands

| Command | |
|---|---|
| `crew spawn <id> [--force]` | queue a task's next attempt |
| `crew review <id>` | read-only: criteria results, diff, approve line |
| `crew approve <id> --head <sha>` | **requires a terminal** |
| `crew land <id>` | merge an approved task into main |
| `crew reframe <id>` | abandon this attempt, start the next |
| `crew rebase <id>` | rebase onto main; invalidates approval |
| `crew status [--json] [--markdown]` | state, spend, watcher health |
| `crew peek <id> [--lines N]` | tail a live worker's pane |
| `crew teardown <id> [--remove-worktree]` | kill a task's window |
| `crew doctor [--notify]` | reconcile state against ground truth |
| `crew gc` | remove the orphans doctor found |
| `crew watch` | drive the loop |
| `crew verify <id> --force` | debug-only: re-run a verify pass |
| `crew send <id> "<text>"` | debug-only: send keys to a worker's pane |

`crew spawn` refuses on unmet dependencies, path overlap with an in-flight
task, the concurrency cap, an exhausted budget, a leftover branch or worktree,
or any outstanding `crew doctor` finding. `--force` overrides the
preconditions.

## Watching the watcher

`crew watch` writes a heartbeat every poll, but a heartbeat only means
something if something outside the loop reads it — the loop cannot report its
own death. `deploy/com.crew.doctor.plist` is a launchd job that runs
`crew doctor --notify` every five minutes for exactly that. Edit its
placeholder paths, then:

```bash
cp deploy/com.crew.doctor.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.crew.doctor.plist
```

`crew status` also reports the watcher as alive, stale, or never seen, along
with the day's spend against the cap. Those two numbers explain most cases of
"why has nothing moved?".

## Configuration

All of `.crew/config.json`. Defaults apply to anything omitted.

| Key | Default | |
|---|---|---|
| `concurrency_cap` | `3` | tasks in flight at once |
| `poll_interval_seconds` | `15` | how often the loop wakes |
| `wall_clock_timeout_seconds` | `1800` | after which a worker is killed and the task fails |
| `max_cycles` | `3` | implement/verify cycles before `needs_reframe` |
| `per_task_cost_cap_usd` | `5.00` | cumulative, across all of a task's workers |
| `project_cost_cap_usd_per_day` | `25.00` | rolls over in `budget_timezone` |
| `per_worker_budget_usd` | `1.50` | ceiling for a single worker |
| `budget_safety_margin` | `0.25` | shrinks each worker's budget; see above |
| `min_spawn_budget_usd` | `0.10` | below this, refuse rather than start |
| `budget_timezone` | `America/Denver` | |
| `main_branch` | `main` | |
| `verify_test_suffix` | `_crewverify_test.go` | marks verifier-authored tests |
| `test_file_suffix` | `_test.go` | how crew tells a new test from an existing one |
| `implementer_model` / `verifier_model` | `sonnet` | |
| `check_commands` | — | `test`, `lint`, `build` as argv |
| `negative_control_build_failure_markers` | build-failure strings | distinguishes a compile failure from a real assertion failure |

**`crew watch` reads this file once, at startup.** Change it while the loop is
running and the running loop keeps the old values.

## Development

```bash
go build ./...
go test ./...
```

The suite drives real git repositories, real tmux windows, and a `fakeclaude`
stub standing in for the CLI, so it is slower than a unit-test suite and needs
`git` and `tmux` present.

## Layout

```
cmd/crew/          the captain-facing CLI and the watch loop entrypoint
cmd/crew-run/      implementer dispatcher  (test lint build diff commit)
cmd/crew-check/    verifier dispatcher     (test lint build)
cmd/fakeclaude/    a scripted stand-in for the claude CLI: drives the whole
                   loop offline, with no API spend
internal/watch/    the state machine: spawn, plan, negative control
internal/dispatch/ the shared dispatcher implementation
internal/hook/     the PreToolUse gate
internal/negctl/   negative control execution
internal/state/    .crew/state.json and the event log
internal/tasks/    the TASKS.md parser
internal/budget/   spend ceilings and the safety margin
```
