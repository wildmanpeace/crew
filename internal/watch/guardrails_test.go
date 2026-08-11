package watch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/cli"
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/state"
	"github.com/wildmanpeace/crew/internal/watch"
	"github.com/wildmanpeace/crew/internal/worker"
)

// This file states crew's safety contract in one place, against the real
// binaries. Each of these properties is relied on elsewhere in the design; if
// one of them stops holding, the corresponding argument in the docs is no
// longer true.

// A verifier cannot commit, because no binary on its PATH exposes the verb.
func TestGuardrailVerifierCannotCommit(t *testing.T) {
	h := newHarness(t, []map[string]any{step(0.1, implReport("done"))})
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatal(err)
	}
	wt := watch.WorktreePath(h.root, "alpha", 1)

	out, code := runDispatcher(t, h.root, wt, config.RoleVerifier, "commit", "sneaky")
	if code != 2 {
		t.Fatalf("crew-check commit exit = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "does not expose verb") {
		t.Errorf("output = %q", out)
	}
}

// The verifier's dispatcher directory contains exactly one binary, and it is
// not crew-run. This is the boundary the role separation actually rests on.
func TestGuardrailVerifierHasNoImplementerDispatcher(t *testing.T) {
	h := newHarness(t, nil)
	verifierBin := worker.RoleBinDir(h.root, config.RoleVerifier)
	entries, err := os.ReadDir(verifierBin)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "crew-check" {
		t.Fatalf("verifier bin contains %v, want exactly [crew-check]", names)
	}
	if _, err := os.Stat(filepath.Join(verifierBin, "crew-run")); err == nil {
		t.Fatal("crew-run is reachable from the verifier's bin directory")
	}
}

// A worker's PATH must not reach the other role's dispatcher, however crew
// itself happens to be installed.
func TestGuardrailWorkerPathExcludesTheOtherRole(t *testing.T) {
	h := newHarness(t, nil)
	implBin := worker.RoleBinDir(h.root, config.RoleImplementer)
	verBin := worker.RoleBinDir(h.root, config.RoleVerifier)

	got := worker.WorkerPath(h.root, config.RoleVerifier, os.Getenv("PATH"))
	for _, p := range strings.Split(got, ":") {
		if p == implBin {
			t.Fatal("verifier PATH contains the implementer bin directory")
		}
		if _, err := os.Stat(filepath.Join(p, "crew-run")); err == nil {
			t.Fatalf("verifier PATH entry %q contains crew-run", p)
		}
	}
	if !strings.HasPrefix(got, verBin) {
		t.Errorf("verifier PATH does not lead with its own bin: %q", got)
	}
}

// Pushing is impossible by construction: it is denied at the permission layer
// and there is no dispatcher verb for it.
func TestGuardrailPushIsUnreachable(t *testing.T) {
	h := newHarness(t, nil)
	wt := watch.WorktreePath(h.root, "alpha", 1)
	spec := worker.Spec{
		Role: config.RoleImplementer, TaskID: "alpha", Worktree: wt,
		ProjectRoot: h.root, RunID: "x", Model: "sonnet", BudgetUSD: 1,
	}
	argv := worker.ClaudeArgs(spec, h.app.Cfg)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "Bash(git *)") {
		t.Error("git is not denied at the permission layer")
	}
	for _, verb := range config.VerbsForRole(config.RoleImplementer) {
		if verb == "push" {
			t.Fatal("a push verb exists on the implementer dispatcher")
		}
	}
}

// The hook denies anything that is not the role's own dispatcher in the
// expected shape, including the built-in read-only commands the permission
// layer cannot close.
func TestGuardrailHookDeniesEverythingButTheDispatcher(t *testing.T) {
	bins := binaries(t)
	cases := []struct {
		role    config.Role
		command string
	}{
		{config.RoleImplementer, "cat /etc/passwd"},
		{config.RoleImplementer, "git log --oneline"},
		{config.RoleImplementer, "crew-run test && git push origin main"},
		{config.RoleImplementer, "crew-run frobnicate"},
		{config.RoleImplementer, "PATH=/tmp/evil crew-run test"},
		{config.RoleVerifier, "crew-run test"},
		{config.RoleVerifier, "crew-check commit x"},
		{config.RoleVerifier, "/abs/path/crew-check test"},
	}
	for _, tc := range cases {
		payload := `{"tool_name":"Bash","tool_input":{"command":` + quote(tc.command) + `}}`
		cmd := exec.Command(filepath.Join(bins, "crew"), "hook-gate", "--role", string(tc.role))
		cmd.Stdin = strings.NewReader(payload)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hook-gate: %v", err)
		}
		if !strings.Contains(string(out), `"permissionDecision":"deny"`) {
			t.Errorf("%s / %q was not denied: %s", tc.role, tc.command, out)
		}
	}
}

// The legitimate shape must still be permitted, or the gate would be useless.
func TestGuardrailHookPermitsTheLegitimateShape(t *testing.T) {
	bins := binaries(t)
	for _, tc := range []struct {
		role    config.Role
		command string
	}{
		{config.RoleImplementer, "crew-run test ./..."},
		{config.RoleImplementer, "crew-run commit \"a message\""},
		{config.RoleVerifier, "crew-check test ./ratelimit/..."},
	} {
		payload := `{"tool_name":"Bash","tool_input":{"command":` + quote(tc.command) + `}}`
		cmd := exec.Command(filepath.Join(bins, "crew"), "hook-gate", "--role", string(tc.role))
		cmd.Stdin = strings.NewReader(payload)
		out, _ := cmd.Output()
		if strings.Contains(string(out), "deny") {
			t.Errorf("%s / %q was denied: %s", tc.role, tc.command, out)
		}
	}
}

// Approval is captain-only: without a terminal it is refused outright.
func TestGuardrailApproveRefusesWithoutATTY(t *testing.T) {
	h := newHarness(t, nil)
	h.app.IsTTY = func() bool { return false }
	if err := h.app.Approve("alpha", "deadbeefdeadbeef"); err == nil {
		t.Fatal("approve succeeded without a terminal")
	}
}

// The captain's own conventions and any cross-run memory are kept out of
// worker context, so each worker is a clean one-shot.
func TestGuardrailWorkerContextIsIsolated(t *testing.T) {
	h := newHarness(t, nil)
	raw, err := os.ReadFile(filepath.Join(h.root, ".crew", "verifier-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"autoMemoryEnabled": false`) {
		t.Error("auto memory is not disabled; state would leak between one-shot workers")
	}
	if !strings.Contains(body, "claudeMdExcludes") {
		t.Error("the captain's global CLAUDE.md is not excluded from worker context")
	}
	// A worktree is a full checkout, so the project's own memory files sit
	// inside it. Without this exclusion a worker loads whatever the project
	// wrote for the captain's interactive session, which is not its job.
	wtPattern := filepath.Join(h.root, ".crew", "worktrees", "**")
	if !strings.Contains(body, wtPattern) {
		t.Errorf("worktree memory files are not excluded; want %q in:\n%s", wtPattern, body)
	}
}

// The red path: three cycles, a reframe, and a fresh attempt that does not
// collide with the one it abandoned.
func TestRedPathReframeStartsACleanAttemptAndPreservesTheOld(t *testing.T) {
	var script []map[string]any
	for range 3 {
		script = append(script,
			step(0.10, implReport("done")),
			step(0.05, verifierReport("verify_failed", false)))
	}
	h := newHarness(t, script)
	h.start("alpha")
	h.settle("alpha", state.StatusNeedsReframe)

	if err := h.app.Reframe("alpha"); err != nil {
		t.Fatalf("Reframe: %v", err)
	}
	st, _ := h.loop.Store.Read()
	ts := st.Tasks["alpha"]
	if ts.Attempt != 2 || ts.Cycle != 0 {
		t.Fatalf("after reframe: attempt %d cycle %d, want 2/0", ts.Attempt, ts.Cycle)
	}

	// The abandoned attempt stays readable for forensics.
	exists, _ := h.app.Repo.BranchExists(watch.BranchName("alpha", 1))
	if !exists {
		t.Fatal("the failed attempt's branch was deleted")
	}
	if _, err := os.Stat(watch.WorktreePath(h.root, "alpha", 1)); err != nil {
		t.Errorf("the failed attempt's worktree was destroyed: %v", err)
	}

	// And the new attempt has somewhere collision-free to go.
	if err := h.app.Spawn("alpha", false); err != nil {
		t.Fatalf("spawn after reframe: %v", err)
	}
	if _, err := os.Stat(watch.WorktreePath(h.root, "alpha", 2)); err != nil {
		t.Fatalf("attempt-2 worktree not created: %v", err)
	}
}

func runDispatcher(t *testing.T, root, worktree string, role config.Role, args ...string) (string, int) {
	t.Helper()
	bin := filepath.Join(worker.RoleBinDir(root, role), worker.DispatcherName(role))
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"CREW_WORKTREE="+worktree, "CREW_PROJECT_ROOT="+root, "CREW_TASK_ID=alpha")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func quote(s string) string {
	b, _ := jsonMarshalString(s)
	return b
}

func jsonMarshalString(s string) (string, error) {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String(), nil
}

var _ = cli.App{}
