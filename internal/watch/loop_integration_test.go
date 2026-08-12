package watch_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wildmanpeace/crew/internal/cli"
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/tmux"
	"github.com/wildmanpeace/crew/internal/watch"
)

// These tests drive the real loop, in real tmux windows, against a real git
// repository, with a scripted stand-in for the claude CLI. Everything except
// the model is genuine, which is what makes the cycle cap, the retry rule and
// the crash recovery meaningful rather than asserted.

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "crew-e2e-bin-*")
		if buildErr != nil {
			return
		}
		for _, pkg := range []string{"crew", "crew-run", "crew-check", "fakeclaude"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, pkg),
				"github.com/wildmanpeace/crew/cmd/"+pkg)
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", pkg, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return binDir
}

const e2eTasks = `## task: alpha
- brief: make Allow refuse when empty
- paths: ratelimit/**
- acceptance_criteria:
    - judged: true
      description: Allow refuses when the bucket is empty.
`

const e2eConfig = `{
  "concurrency_cap": 3,
  "poll_interval_seconds": 1,
  "max_cycles": 3,
  "per_task_cost_cap_usd": 5.00,
  "project_cost_cap_usd_per_day": 25.00,
  "per_worker_budget_usd": 1.50,
  "budget_safety_margin": 0.25,
  "min_spawn_budget_usd": 0.10,
  "budget_timezone": "America/Denver",
  "main_branch": "main",
  "check_commands": {
    "test": {"argv": ["go","test"], "default_args": ["./..."]}
  }
}`

type harness struct {
	root    string
	app     *cli.App
	loop    *watch.Loop
	session string
	t       *testing.T
}

func newHarness(t *testing.T, script []map[string]any) *harness {
	t.Helper()
	if !tmux.Available() {
		t.Skip("tmux is not installed")
	}
	bins := binaries(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := gitx.New(root)
	mustGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(root, "go.mod"), "module e2e\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "ratelimit", "bucket.go"),
		"package ratelimit\n\nfunc Allow() bool { return true }\n")
	writeFile(t, filepath.Join(root, "TASKS.md"), e2eTasks)
	writeFile(t, filepath.Join(root, ".crew", "config.json"), e2eConfig)
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-qm", "base")

	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".crew", "fake-script.json"), string(raw))

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := cfg.Location()
	store, err := state.Open(root, loc)
	if err != nil {
		t.Fatal(err)
	}

	session := fmt.Sprintf("crewe2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { tmux.KillSession(session) })

	app := &cli.App{
		Root: root, Cfg: cfg, Store: store, Repo: repo, Loc: loc,
		Stdout: io_discard{}, Stderr: io_discard{}, Session: session,
		IsTTY: func() bool { return true },
	}
	loop := &watch.Loop{
		Root: root, Cfg: cfg, Store: store, Repo: repo,
		CrewBin:   filepath.Join(bins, "crew"),
		ClaudeBin: filepath.Join(bins, "fakeclaude"),
		Session:   session, Loc: loc,
		Land: app.Land,
	}
	h := &harness{root: root, app: app, loop: loop, session: session, t: t}
	if err := app.InstallDispatchersFrom(bins); err != nil {
		t.Fatal(err)
	}
	if err := app.WriteWorkerSettings(); err != nil {
		t.Fatal(err)
	}
	return h
}

type io_discard struct{}

func (io_discard) Write(p []byte) (int, error) { return len(p), nil }

func mustGit(t *testing.T, r gitx.Repo, args ...string) {
	t.Helper()
	if _, err := r.Run(args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// settle ticks the loop until the task reaches one of the wanted statuses.
func (h *harness) settle(taskID string, want ...state.Status) *state.TaskState {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.loop.Tick(h.t.Context()); err != nil {
			h.t.Fatalf("Tick: %v", err)
		}
		st, err := h.loop.Store.Read()
		if err != nil {
			h.t.Fatal(err)
		}
		ts := st.Tasks[taskID]
		if ts != nil {
			for _, w := range want {
				if ts.Status == w {
					return ts
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	st, _ := h.loop.Store.Read()
	h.t.Fatalf("task %q never reached %v; last state: %+v", taskID, want, st.Tasks[taskID])
	return nil
}

// commitInWorktree gives a task a real commit to land. Nothing the fake worker
// does touches the tree, so without this there is no diff to approve.
func (h *harness) commitInWorktree(taskID, body string) {
	h.t.Helper()
	wt := watch.WorktreePath(h.root, taskID, 1)
	writeFile(h.t, filepath.Join(wt, "ratelimit", "bucket.go"),
		"package ratelimit\n\n"+body+"\n")
	wtRepo := gitx.New(wt)
	mustGit(h.t, wtRepo, "add", "-A")
	mustGit(h.t, wtRepo, "commit", "-qm", "implement")
}

func (h *harness) start(taskID string) {
	h.t.Helper()
	if err := h.app.Spawn(taskID, false); err != nil {
		h.t.Fatalf("Spawn: %v", err)
	}
	// crew watch owns starting the first implementer.
	if err := h.loop.SpawnWorker(taskID, config.RoleImplementer, 1, "implement it"); err != nil {
		h.t.Fatalf("SpawnWorker: %v", err)
	}
}

func implReport(status string) map[string]any {
	r := map[string]any{"task_id": "alpha", "role": "implementer", "status": status}
	if status == "done" {
		r["summary"] = "did the work"
		r["checks_run"] = []map[string]any{{"command": "crew-run test", "exit_code": 0}}
	}
	return r
}

func verifierReport(status string, met bool) map[string]any {
	return map[string]any{
		"task_id": "alpha", "role": "verifier", "status": status,
		"criteria_results": []map[string]any{{
			"description": "Allow refuses when the bucket is empty.",
			"evaluation":  "judged",
			"met":         met,
			"notes":       "read the diff",
		}},
		"finished_at": time.Now().UTC().Format(time.RFC3339),
	}
}

func step(cost float64, report map[string]any) map[string]any {
	s := map[string]any{"cost_usd": cost, "exit": 0}
	if report != nil {
		s["report"] = report
	} else {
		s["no_report"] = true
	}
	return s
}

// The implementer's work: a change to pre-existing API, so a test written
// against it compiles at the merge base as well as at head.
const e2eImplementation = "package ratelimit\n\nfunc Allow() bool { return false }\n"

// The verifier's test. It is deliberately never committed: crew-check has no
// commit verb and the hook denies raw git, so this is the only state a
// verifier-authored test can be in when the control runs.
const e2eVerifyTest = `package ratelimit

import "testing"

func TestAllowRefusesWhenEmpty(t *testing.T) {
	if Allow() {
		t.Fatal("Allow returned true with an empty bucket")
	}
}
`

const e2eVerifyTestFile = "ratelimit/empty_crewverify_test.go"

// implStep is an implementer that actually writes and commits its work.
func implStep(cost float64) map[string]any {
	s := step(cost, implReport("done"))
	s["files"] = map[string]string{"ratelimit/bucket.go": e2eImplementation}
	s["commit"] = true
	return s
}

// negControlStep is a verifier that writes a real test and claims a
// negative-control criterion against it. The claim is provisional; crew's own
// control is what decides.
func negControlStep(cost float64) map[string]any {
	s := step(cost, map[string]any{
		"task_id": "alpha", "role": "verifier", "status": "satisfied",
		"criteria_results": []map[string]any{{
			"description": "Allow refuses when the bucket is empty.",
			"evaluation":  "negative_control_test",
			"test_file":   e2eVerifyTestFile,
			"met":         true,
			"notes":       "wrote a test that fails without the change",
		}},
		"finished_at": time.Now().UTC().Format(time.RFC3339),
	})
	s["files"] = map[string]string{e2eVerifyTestFile: e2eVerifyTest}
	return s
}

// events returns everything crew has recorded, in order.
func (h *harness) events() []state.Event {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.root, ".crew", "events.jsonl"))
	if err != nil {
		h.t.Fatalf("read events: %v", err)
	}
	var out []state.Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev state.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			h.t.Fatalf("parse event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func (h *harness) eventsOfKind(kind string) []state.Event {
	h.t.Helper()
	var out []state.Event
	for _, ev := range h.events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// The flagship guarantee, driven end to end: a verifier-authored negative
// control must be measured against the verifier's own test.
//
// The verifier writes its test into the task worktree and cannot commit it, so
// before this the scratch worktree — built from the committed branch head —
// never contained it. Both phases ran the implementer's committed tests
// instead, of which there are none here, so the control concluded
// "passes_at_merge_base" about a test it had never executed and the criterion
// failed unfixably. Nothing in the suite exercised this shape, which is why it
// shipped green.
func TestVerifierAuthoredNegativeControlIsMeasuredAgainstTheVerifiersOwnTest(t *testing.T) {
	h := newHarness(t, []map[string]any{
		implStep(0.10),
		negControlStep(0.05),
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusReadyForReview, state.StatusNeedsReframe,
		state.StatusBlocked, state.StatusFailed)

	controls := h.eventsOfKind("negative_control")
	if len(controls) != 1 {
		t.Fatalf("recorded %d negative controls, want exactly 1: %+v", len(controls), controls)
	}
	payload, ok := controls[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("negative control payload = %T", controls[0].Payload)
	}
	if got := payload["classification"]; got != "discriminating" {
		t.Errorf("classification = %v, want discriminating; the verifier's test was not in the control\n"+
			"merge-base output:\n%v", got, payload["merge_base_output"])
	}
	if got := payload["test_file"]; got != e2eVerifyTestFile {
		t.Errorf("test_file = %v, want %s", got, e2eVerifyTestFile)
	}
	// The proof the verifier's assertion actually ran: its failure text can
	// only appear if the file was present in the reverted tree.
	if out, _ := payload["merge_base_output"].(string); !strings.Contains(out, "Allow returned true") {
		t.Errorf("the verifier's assertion never ran in the control:\n%s", out)
	}

	if ts.Status != state.StatusReadyForReview {
		t.Fatalf("Status = %q, want ready_for_review: %s", ts.Status, ts.Notes)
	}
	ready := h.eventsOfKind("ready_for_review")
	if len(ready) == 0 {
		t.Fatal("no ready_for_review event")
	}
	if got := ready[len(ready)-1].Ratio; got != "1 mechanical / 0 judged" {
		t.Errorf("ratio = %q, want %q", got, "1 mechanical / 0 judged")
	}
	if ts.Cycle != 1 {
		t.Errorf("Cycle = %d, want 1; the control sent real work back", ts.Cycle)
	}
}

// The other half of the guarantee: a verifier test that passes with the
// implementation taken away must be caught. It is the same path as the test
// above, so a copy-in that happened to leave the file out of the revert would
// pass one and fail the other.
func TestAVacuousVerifierTestIsCaughtByTheControl(t *testing.T) {
	vacuous := negControlStep(0.05)
	vacuous["files"] = map[string]string{e2eVerifyTestFile: `package ratelimit

import "testing"

func TestAllowIsCallable(t *testing.T) {
	_ = Allow()
}
`}
	h := newHarness(t, []map[string]any{
		implStep(0.10),
		vacuous,
		implStep(0.10),
		negControlStep(0.05),
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusReadyForReview, state.StatusNeedsReframe)

	controls := h.eventsOfKind("negative_control")
	if len(controls) == 0 {
		t.Fatal("no negative control ran")
	}
	first, _ := controls[0].Payload.(map[string]any)
	if got := first["classification"]; got != "passes_at_merge_base" {
		t.Fatalf("classification = %v, want passes_at_merge_base", got)
	}
	// A vacuous test is a verify failure, so the task must have gone back for
	// another cycle rather than passing on the verifier's own say-so.
	if ts.Cycle != 2 {
		t.Fatalf("Cycle = %d, want 2; a test that cannot fail was accepted as evidence", ts.Cycle)
	}
}

// The happy path, end to end through real tmux windows.
func TestLoopReachesReadyForReview(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusReadyForReview)

	if ts.Cycle != 1 {
		t.Errorf("Cycle = %d, want 1", ts.Cycle)
	}
	// Cost is captured by crew from the CLI's output, never self-reported.
	if ts.SpendUSD < 0.14 || ts.SpendUSD > 0.16 {
		t.Errorf("SpendUSD = %v, want ~0.15", ts.SpendUSD)
	}
	if ts.HeadSha == "" {
		t.Error("ready_for_review carries no approval sha")
	}
}

// Three cycles and no more: the fourth implementer is refused.
func TestLoopStopsAtTheCycleCapAndAsksForAReframe(t *testing.T) {
	var script []map[string]any
	for range 3 {
		script = append(script,
			step(0.10, implReport("done")),
			step(0.05, verifierReport("verify_failed", false)))
	}
	h := newHarness(t, script)
	h.start("alpha")
	ts := h.settle("alpha", state.StatusNeedsReframe)

	if ts.Cycle != 3 {
		t.Fatalf("Cycle = %d, want exactly 3", ts.Cycle)
	}
	// A fourth implementer would have consumed two more steps; the script
	// having any left proves the cap held.
	raw, _ := os.ReadFile(filepath.Join(h.root, ".crew", "fake-step"))
	if strings.TrimSpace(string(raw)) != "6" {
		t.Errorf("consumed %s script steps, want exactly 6 (3 cycles)", strings.TrimSpace(string(raw)))
	}
}

// A claimed satisfied that its own numbers contradict must not pass.
func TestClaimedSatisfiedWithAFailingCheckIsDowngraded(t *testing.T) {
	failing := map[string]any{
		"task_id": "alpha", "role": "verifier", "status": "satisfied",
		"criteria_results": []map[string]any{{
			"description": "Allow refuses when the bucket is empty.",
			"evaluation":  "mechanical_check",
			"command":     "crew-check test ./ratelimit/...",
			"exit_code":   1,
			"met":         true,
		}},
	}
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, failing),
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusReadyForReview)

	// The downgrade must have forced a second cycle rather than passing.
	if ts.Cycle != 2 {
		t.Fatalf("Cycle = %d, want 2 (the false satisfied should have been downgraded)", ts.Cycle)
	}
}

// A verifier that produces no report is retried once, and that retry must not
// come out of the implementation's cycle budget.
func TestVerifierCrashIsRetriedWithoutConsumingACycle(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.02, nil), // verifier writes nothing
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusReadyForReview)

	if ts.Cycle != 1 {
		t.Fatalf("Cycle = %d, want 1; the verifier's own failure consumed a cycle", ts.Cycle)
	}
}

// A second consecutive verifier failure is a block, not an infinite retry.
func TestSecondConsecutiveVerifierFailureBlocks(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.02, nil),
		step(0.02, nil),
	})
	h.start("alpha")
	h.settle("alpha", state.StatusBlocked)
}

// An implementer claiming done while its own recorded check failed is not a
// completion.
func TestImplementerDoneWithAFailingCheckIsRejected(t *testing.T) {
	bad := map[string]any{
		"task_id": "alpha", "role": "implementer", "status": "done",
		"summary":    "claimed",
		"checks_run": []map[string]any{{"command": "crew-run test", "exit_code": 1}},
	}
	h := newHarness(t, []map[string]any{step(0.10, bad)})
	h.start("alpha")
	h.settle("alpha", state.StatusFailed)
}

// No report at all is always a failure; process exit is liveness, not content.
func TestImplementerWithNoReportFails(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, nil)})
	h.start("alpha")
	h.settle("alpha", state.StatusFailed)
}

// The budget is enforced before spawning, not discovered afterwards.
func TestSpawnIsRefusedOnceTheTaskCapIsSpent(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, implReport("done"))})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	h.loop.Store.Update(func(st *state.State) error {
		st.Tasks["alpha"].SpendUSD = 5.00
		return nil
	})
	if err := h.loop.SpawnWorker("alpha", config.RoleImplementer, 1, "go"); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}
	st, _ := h.loop.Store.Read()
	if got := st.Tasks["alpha"].Status; got != state.StatusBlocked {
		t.Fatalf("Status = %q, want blocked before any worker started", got)
	}
	// Nothing may have been spawned.
	raw, err := os.ReadFile(filepath.Join(h.root, ".crew", "fake-step"))
	if err == nil && strings.TrimSpace(string(raw)) != "0" {
		t.Fatalf("a worker ran despite the exhausted cap (step counter = %s)",
			strings.TrimSpace(string(raw)))
	}
}

// A transient spawn failure returns the task to queued. Before this, the next
// poll always started a cycle-1 implementer, so a verifier that failed to
// spawn silently became a fresh implementation attempt: the work already done
// was never verified and the cycle cap started over.
func TestAFailedVerifierSpawnResumesTheVerifierNotAFreshImplementer(t *testing.T) {
	h := newHarness(t, []map[string]any{
		{"cost_usd": 0.01, "exit": 0, "sleep_seconds": 30, "no_report": true},
	})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	// The shape a failed worker.Spawn leaves behind: back to queued, with the
	// role and cycle the spawn was attempting still recorded.
	h.loop.Store.Update(func(st *state.State) error {
		ts := st.Tasks["alpha"]
		ts.Status = state.StatusQueued
		ts.Role = string(config.RoleVerifier)
		ts.Cycle = 2
		return nil
	})
	if err := h.loop.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	st, _ := h.loop.Store.Read()
	ts := st.Tasks["alpha"]
	if ts.Role != string(config.RoleVerifier) {
		t.Fatalf("Role = %q, want verifier; the failed verifier spawn restarted as an implementer", ts.Role)
	}
	if ts.Cycle != 2 {
		t.Fatalf("Cycle = %d, want 2; the cycle cap restarted", ts.Cycle)
	}
	if ts.RunID != "alpha-a1-c2-verify" {
		t.Errorf("RunID = %q, want alpha-a1-c2-verify", ts.RunID)
	}
}

// A crash between recording a spawn intent and creating the window must be
// repaired, not left stranded.
func TestCrashBetweenIntentAndEffectIsRepaired(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, implReport("done"))})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash: the intent is durable, the effect never happened.
	h.loop.Store.Update(func(st *state.State) error {
		ts := st.Tasks["alpha"]
		ts.Status = state.StatusSpawning
		ts.RunID = "alpha-a1-c1-impl"
		ts.PendingIntent = &state.Intent{
			Action: state.IntentSpawnWindow,
			Window: "crew-alpha-a1-c1-impl",
			RunID:  "alpha-a1-c1-impl",
		}
		return nil
	})
	if err := h.loop.Repair(); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	st, _ := h.loop.Store.Read()
	ts := st.Tasks["alpha"]
	if ts.Status != state.StatusQueued {
		t.Fatalf("Status = %q, want queued", ts.Status)
	}
	if ts.PendingIntent != nil {
		t.Error("intent survived repair")
	}
}

// A crash between recording the create-worktree intent and creating it left a
// dangling intent Repair did not know about, while crew doctor claimed a
// restart would clear it. Any doctor finding refuses every spawn, so this
// wedged all future work behind a finding no restart could ever clear.
func TestCrashBeforeTheWorktreeExistsIsCleanedUpNotWedged(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, implReport("done"))})
	branch, worktree := watch.BranchName("alpha", 1), watch.WorktreePath(h.root, "alpha", 1)
	h.loop.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{
			ID: "alpha", Status: state.StatusPending, Attempt: 1,
			Branch: branch, Worktree: worktree,
			PendingIntent: &state.Intent{
				Action: state.IntentCreateWorktr, Branch: branch, Worktree: worktree,
			},
		})
		return nil
	})

	if err := h.loop.Repair(); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	st, _ := h.loop.Store.Read()
	if st.Tasks["alpha"].PendingIntent != nil {
		t.Fatal("the create-worktree intent survived repair")
	}

	// Doctor must come back clean, or every future spawn stays refused.
	problems, err := h.app.DoctorFindings()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("doctor still failing after repair: %s: %s", p.TaskID, p.Detail)
	}
	// The proof that matters: the task can be spawned again.
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatalf("Spawn after repair: %v", err)
	}
}

// The other outcome of the same crash: the worktree was created and only the
// intent was left behind. Rolling that back would throw away a good worktree.
func TestACompletedWorktreeIntentIsRolledForward(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, implReport("done"))})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	h.loop.Store.Update(func(st *state.State) error {
		ts := st.Tasks["alpha"]
		ts.Status = state.StatusPending
		ts.PendingIntent = &state.Intent{
			Action: state.IntentCreateWorktr, Branch: ts.Branch, Worktree: ts.Worktree,
		}
		return nil
	})

	if err := h.loop.Repair(); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	st, _ := h.loop.Store.Read()
	ts := st.Tasks["alpha"]
	if ts.PendingIntent != nil {
		t.Fatal("the create-worktree intent survived repair")
	}
	if ts.Status != state.StatusQueued {
		t.Fatalf("Status = %q, want queued; a worktree that exists was thrown away", ts.Status)
	}
	if _, err := os.Stat(ts.Worktree); err != nil {
		t.Errorf("the worktree was removed: %v", err)
	}
}

// Doctor must only promise a repair that exists. Land is not repaired by the
// loop, so telling the captain to restart it leaves them waiting on something
// that will never happen.
func TestDoctorOnlyPromisesRepairsTheLoopActuallyPerforms(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.10, implReport("done"))})
	h.loop.Store.Update(func(st *state.State) error {
		st.Upsert(&state.TaskState{ID: "alpha", Status: state.StatusApproved, Attempt: 1,
			PendingIntent: &state.Intent{Action: state.IntentLand}})
		return nil
	})
	problems, err := h.app.DoctorFindings()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range problems {
		if !strings.Contains(p.Detail, "intent") {
			continue
		}
		found = true
		if strings.Contains(p.Detail, "repairs this on restart") {
			t.Errorf("doctor promises a restart repairs a land intent: %q", p.Detail)
		}
		if !strings.Contains(p.Detail, string(state.IntentLand)) {
			t.Errorf("Detail = %q, want it to name the intent", p.Detail)
		}
	}
	if !found {
		t.Fatal("doctor did not report the unfinished intent at all")
	}
}

// The whole point of the loop: reaching a captain decision and landing it.
func TestFullPathThroughApproveAndLand(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")

	// The implementer must actually commit something for there to be a diff.
	h.settle("alpha", state.StatusReadyForReview)
	h.commitInWorktree("alpha", "func Allow() bool { return false }")

	head, _ := h.app.Repo.RevParse(watch.BranchName("alpha", 1))
	if err := h.app.Approve("alpha", head); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := h.app.Land("alpha"); err != nil {
		t.Fatalf("Land: %v", err)
	}
	st, _ := h.loop.Store.Read()
	if st.Tasks["alpha"].Status != state.StatusLanded {
		t.Fatalf("Status = %q", st.Tasks["alpha"].Status)
	}
	body, err := os.ReadFile(filepath.Join(h.root, "ratelimit", "bucket.go"))
	if err != nil || !strings.Contains(string(body), "return false") {
		t.Fatalf("the change did not land on main: %v %s", err, body)
	}
}

// crew spawn prepares the branch and stops; the loop must be what actually
// starts the first implementer, or a queued task sits forever.
func TestLoopStartsQueuedTasksItself(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	// Deliberately no manual SpawnWorker here.
	ts := h.settle("alpha", state.StatusReadyForReview)
	if ts.Cycle != 1 {
		t.Fatalf("Cycle = %d, want 1", ts.Cycle)
	}
}

// A run that stopped at its budget ceiling is blocked, not failed. The two
// call for different responses: failed invites a reframe, while this calls
// for a larger cap or a narrower task.
func TestBudgetExhaustionIsBlockedNotFailed(t *testing.T) {
	h := newHarness(t, []map[string]any{
		{"cost_usd": 0.46, "exit": 1, "no_report": true, "subtype": "error_max_budget_usd"},
	})
	h.start("alpha")
	ts := h.settle("alpha", state.StatusBlocked, state.StatusFailed)
	if ts.Status != state.StatusBlocked {
		t.Fatalf("Status = %q, want blocked", ts.Status)
	}
	if !strings.Contains(ts.Notes, "budget ceiling") {
		t.Errorf("Notes = %q, want it to name the budget", ts.Notes)
	}
}

// A worker whose window dies mid-run without writing an exit marker must be
// noticed. Before this, the task sat in running forever, held a concurrency
// slot, and failed crew doctor, which refused every later spawn.
func TestVanishedWorkerIsFailedNotLeftRunning(t *testing.T) {
	h := newHarness(t, []map[string]any{
		{"cost_usd": 0.05, "exit": 0, "sleep_seconds": 120, "no_report": true},
	})
	h.loop.Cfg.WallClockTimeoutSeconds = 0 // isolate the vanished-window path
	h.start("alpha")

	// Let the worker get past the spawn grace, then kill its window as an
	// abrupt death would.
	st, _ := h.loop.Store.Read()
	window := st.Tasks["alpha"].Window
	if window == "" {
		t.Fatal("no window recorded")
	}
	h.rewindRunStart("alpha", 2*time.Minute)
	if err := tmux.KillWindow(h.session, window); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}

	ts := h.settle("alpha", state.StatusFailed)
	if !strings.Contains(ts.Notes, "without reporting") {
		t.Errorf("Notes = %q, want it to name the vanished worker", ts.Notes)
	}
	// The slot must be released.
	if ts.Status.InFlight() {
		t.Error("a dead task is still holding a concurrency slot")
	}
}

// wall_clock_timeout_seconds was configured but never read, so a hung worker
// ran forever.
func TestHungWorkerIsStoppedAtTheWallClock(t *testing.T) {
	h := newHarness(t, []map[string]any{
		{"cost_usd": 0.05, "exit": 0, "sleep_seconds": 120, "no_report": true},
	})
	h.loop.Cfg.WallClockTimeoutSeconds = 1
	h.start("alpha")

	st, _ := h.loop.Store.Read()
	window := st.Tasks["alpha"].Window
	h.rewindRunStart("alpha", 2*time.Minute)

	ts := h.settle("alpha", state.StatusFailed)
	if !strings.Contains(ts.Notes, "wall_clock_timeout_seconds") {
		t.Errorf("Notes = %q, want it to name the timeout", ts.Notes)
	}
	// The worker must actually be stopped, not merely marked failed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := tmux.WindowExists(h.session, window); !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the timed-out worker's window is still running")
}

// A worker that finishes normally must never be mistaken for a dead one, even
// though its window closes the moment it exits.
func TestFinishedWorkerIsNotMistakenForVanished(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	// No timeout: the question here is only whether a closed window is
	// mistaken for a dead worker.
	h.loop.Cfg.WallClockTimeoutSeconds = 0
	h.start("alpha")
	// Backdate past the spawn grace so the vanished-window check is live on
	// every tick while the workers are exiting. Each worker's window closes the
	// instant it finishes, so the exit marker must win the race.
	h.rewindRunStart("alpha", 2*time.Minute)

	ts := h.settle("alpha", state.StatusReadyForReview, state.StatusFailed)
	if ts.Status != state.StatusReadyForReview {
		t.Fatalf("Status = %q, want ready_for_review; a completed run was misread as dead: %s",
			ts.Status, ts.Notes)
	}
}

// rewindRunStart backdates a run's start so timeout and grace behaviour can be
// exercised without waiting in real time.
func (h *harness) rewindRunStart(taskID string, by time.Duration) {
	h.t.Helper()
	if _, err := h.loop.Store.Update(func(st *state.State) error {
		ts := st.Tasks[taskID]
		if ts == nil {
			return nil
		}
		ts.RunStartedAt = ts.RunStartedAt.Add(-by)
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

// Approval is the captain's decision. The merge that follows re-checks the
// approved sha and decides nothing, so the loop carries an approved task the
// rest of the way rather than making the captain enact a call already made.
func TestLoopLandsAnApprovedTask(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")
	h.settle("alpha", state.StatusReadyForReview)
	h.commitInWorktree("alpha", "func Allow() bool { return false }")

	head, _ := h.app.Repo.RevParse(watch.BranchName("alpha", 1))
	if err := h.app.Approve("alpha", head); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Deliberately no Land call: the loop is what must carry it.
	h.settle("alpha", state.StatusLanded)

	body, err := os.ReadFile(filepath.Join(h.root, "ratelimit", "bucket.go"))
	if err != nil || !strings.Contains(string(body), "return false") {
		t.Fatalf("the change did not land on main: %v %s", err, body)
	}
}

// A dirty main is the captain's own work in progress, not a fault in the task.
// Landing waits for it rather than failing the task or merging over them.
func TestLoopWaitsRatherThanFailingWhenMainIsDirty(t *testing.T) {
	h := newHarness(t, []map[string]any{
		step(0.10, implReport("done")),
		step(0.05, verifierReport("satisfied", true)),
	})
	h.start("alpha")
	h.settle("alpha", state.StatusReadyForReview)
	h.commitInWorktree("alpha", "func Allow() bool { return false }")

	head, _ := h.app.Repo.RevParse(watch.BranchName("alpha", 1))
	if err := h.app.Approve("alpha", head); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	writeFile(t, filepath.Join(h.root, "ratelimit", "scratch.go"), "package ratelimit\n")

	for range 3 {
		if err := h.loop.Tick(t.Context()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	st, _ := h.loop.Store.Read()
	if got := st.Tasks["alpha"].Status; got != state.StatusApproved {
		t.Fatalf("Status = %q, want approved: a dirty main must not fail the task", got)
	}
}
