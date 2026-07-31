package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
)

type capture struct {
	argv []string
	dir  string
	code int
}

func (c *capture) run(argv []string, dir string) (int, error) {
	c.argv = slices.Clone(argv)
	c.dir = dir
	return c.code, nil
}

// fixture builds a project root with a config and a worktree beneath it.
func fixture(t *testing.T) (root, worktree string, cfg *config.Config) {
	t.Helper()
	root = t.TempDir()
	// Resolve symlinks up front: on macOS /tmp is a symlink to /private/tmp,
	// and the containment check compares resolved paths.
	root, _ = filepath.EvalSymlinks(root)

	crewDir := filepath.Join(root, ".crew")
	if err := os.MkdirAll(crewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"check_commands": map[string]any{
			"test":  map[string]any{"argv": []string{"go", "test"}, "default_args": []string{"./..."}},
			"lint":  map[string]any{"argv": []string{"golangci-lint", "run"}},
			"build": map[string]any{"argv": []string{"go", "build"}, "default_args": []string{"./..."}},
		},
	}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(crewDir, "config.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	worktree = filepath.Join(crewDir, "worktrees", "alpha", "attempt-1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, worktree, c
}

func env(root, wt string) Env {
	return Env{ProjectRoot: root, Worktree: wt, TaskID: "alpha"}
}

func TestTestVerbResolvesAndRunsInWorktree(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	code, err := Dispatch(config.RoleImplementer, []string{"test"}, env(root, wt), cfg, c.run)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d", code)
	}
	if want := []string{"go", "test", "./..."}; !reflect.DeepEqual(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
	if c.dir != wt {
		t.Errorf("dir = %q, want worktree %q", c.dir, wt)
	}
}

func TestWorkerArgsReachTheCommand(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	_, err := Dispatch(config.RoleVerifier, []string{"test", "./middleware/...", "-run", "TestRateLimit429"}, env(root, wt), cfg, c.run)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "test", "./middleware/...", "-run", "TestRateLimit429"}
	if !reflect.DeepEqual(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
}

func TestChildExitCodeIsPassedThrough(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{code: 7}
	code, err := Dispatch(config.RoleImplementer, []string{"test"}, env(root, wt), cfg, c.run)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if code != 7 {
		t.Fatalf("code = %d, want 7 passed through", code)
	}
}

// The role boundary: crew-check exposes no commit and no diff at all.
func TestVerifierCannotReachCommitOrDiff(t *testing.T) {
	root, wt, cfg := fixture(t)
	for _, verb := range []string{"commit", "diff"} {
		c := &capture{}
		code, err := Dispatch(config.RoleVerifier, []string{verb, "msg"}, env(root, wt), cfg, c.run)
		if err == nil {
			t.Errorf("verifier %q succeeded, want refusal", verb)
		}
		if code != ExitPolicy {
			t.Errorf("verifier %q exit = %d, want ExitPolicy", verb, code)
		}
		if c.argv != nil {
			t.Errorf("verifier %q reached the runner with %q", verb, c.argv)
		}
	}
}

func TestImplementerCanReachCommitAndDiff(t *testing.T) {
	root, wt, cfg := fixture(t)
	for _, args := range [][]string{{"diff"}, {"commit", "a message"}} {
		c := &capture{}
		if _, err := Dispatch(config.RoleImplementer, args, env(root, wt), cfg, c.run); err != nil {
			t.Errorf("implementer %q: %v", args, err)
		}
		if c.argv == nil {
			t.Errorf("implementer %q never reached the runner", args)
		}
	}
}

func TestUnknownVerbIsHardError(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	code, err := Dispatch(config.RoleImplementer, []string{"frobnicate"}, env(root, wt), cfg, c.run)
	if err == nil {
		t.Fatal("unknown verb succeeded, want hard error")
	}
	if code != ExitPolicy {
		t.Errorf("code = %d, want ExitPolicy", code)
	}
	if c.argv != nil {
		t.Error("unknown verb reached the runner")
	}
}

func TestNoVerbIsHardError(t *testing.T) {
	root, wt, cfg := fixture(t)
	if _, err := Dispatch(config.RoleImplementer, nil, env(root, wt), cfg, (&capture{}).run); err == nil {
		t.Fatal("empty argv succeeded, want error")
	}
}

// The commit message is one argv element and is never interpolated.
func TestCommitMessageIsASingleArgvElement(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	msg := `oops"; rm -rf / #`
	if _, err := Dispatch(config.RoleImplementer, []string{"commit", msg}, env(root, wt), cfg, c.run); err != nil {
		t.Fatal(err)
	}
	idx := slices.Index(c.argv, "-m")
	if idx < 0 || idx+1 >= len(c.argv) {
		t.Fatalf("no -m <msg> pair in %q", c.argv)
	}
	if c.argv[idx+1] != msg {
		t.Fatalf("message = %q, want the exact original %q", c.argv[idx+1], msg)
	}
	for _, a := range c.argv {
		if strings.Contains(a, "rm -rf /") && a != msg {
			t.Fatalf("message leaked into another argv element: %q", c.argv)
		}
	}
}

func TestCommitRequiresExactlyOneMessageArg(t *testing.T) {
	root, wt, cfg := fixture(t)
	for _, args := range [][]string{{"commit"}, {"commit", "a", "b"}} {
		if _, err := Dispatch(config.RoleImplementer, args, env(root, wt), cfg, (&capture{}).run); err == nil {
			t.Errorf("commit %q succeeded, want error", args)
		}
	}
}

// Nothing the dispatcher builds may be handed to a shell.
func TestNeverInvokesAShell(t *testing.T) {
	root, wt, cfg := fixture(t)
	cases := [][]string{{"test"}, {"lint"}, {"build"}, {"diff"}, {"commit", "m"}}
	for _, args := range cases {
		c := &capture{}
		if _, err := Dispatch(config.RoleImplementer, args, env(root, wt), cfg, c.run); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		// The only thing that makes metacharacters dangerous is a shell
		// interpreting them. Since argv[0] is exec'd directly, the guarantee
		// is that argv[0] is never an interpreter.
		switch filepath.Base(c.argv[0]) {
		case "sh", "bash", "zsh", "ksh", "fish", "env", "eval", "xargs":
			t.Fatalf("%q produced an interpreter invocation: %q", args, c.argv)
		}
		if !slices.Contains([]string{"git", "go", "golangci-lint"}, filepath.Base(c.argv[0])) {
			t.Fatalf("%q produced an unexpected program %q: %q", args, c.argv[0], c.argv)
		}
	}
}

func TestDiffExcludesVerifierAuthoredTests(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	if _, err := Dispatch(config.RoleImplementer, []string{"diff"}, env(root, wt), cfg, c.run); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.argv, " ")
	if !strings.Contains(joined, cfg.VerifyTestSuffix) {
		t.Fatalf("diff does not exclude %q: %q", cfg.VerifyTestSuffix, c.argv)
	}
	if !strings.Contains(joined, "exclude") {
		t.Fatalf("diff has no exclude pathspec: %q", c.argv)
	}
}

func TestGitVerbsPinIdentityAndTargetTheWorktree(t *testing.T) {
	root, wt, cfg := fixture(t)
	c := &capture{}
	if _, err := Dispatch(config.RoleImplementer, []string{"commit", "m"}, env(root, wt), cfg, c.run); err != nil {
		t.Fatal(err)
	}
	if c.argv[0] != "git" {
		t.Fatalf("argv[0] = %q, want git", c.argv[0])
	}
	if !slices.Contains(c.argv, "user.name=crew") {
		t.Errorf("commit does not pin an identity: %q", c.argv)
	}
	if c.dir != wt {
		t.Errorf("dir = %q, want %q", c.dir, wt)
	}
}

func TestMissingWorktreeEnvIsError(t *testing.T) {
	root, _, cfg := fixture(t)
	e := Env{ProjectRoot: root, TaskID: "alpha"}
	if _, err := Dispatch(config.RoleImplementer, []string{"test"}, e, cfg, (&capture{}).run); err == nil {
		t.Fatal("missing CREW_WORKTREE succeeded, want error")
	}
}

func TestNonexistentWorktreeIsError(t *testing.T) {
	root, _, cfg := fixture(t)
	e := env(root, filepath.Join(root, ".crew", "worktrees", "ghost"))
	if _, err := Dispatch(config.RoleImplementer, []string{"test"}, e, cfg, (&capture{}).run); err == nil {
		t.Fatal("nonexistent worktree succeeded, want error")
	}
}

// The dispatcher is scoped to a worktree under the project; it must refuse to
// operate anywhere else even if the environment says otherwise.
func TestWorktreeOutsideProjectIsRefused(t *testing.T) {
	root, _, cfg := fixture(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	e := env(root, outside)
	code, err := Dispatch(config.RoleImplementer, []string{"test"}, e, cfg, (&capture{}).run)
	if err == nil {
		t.Fatal("worktree outside the project succeeded, want refusal")
	}
	if code != ExitPolicy {
		t.Errorf("code = %d, want ExitPolicy", code)
	}
}

func TestWorktreeEscapeViaDotDotIsRefused(t *testing.T) {
	root, wt, cfg := fixture(t)
	escape := filepath.Join(wt, "..", "..", "..", "..")
	e := env(root, escape)
	if _, err := Dispatch(config.RoleImplementer, []string{"test"}, e, cfg, (&capture{}).run); err == nil {
		t.Fatal("../ escape succeeded, want refusal")
	}
}

func TestRelativeWorktreeIsRefused(t *testing.T) {
	root, _, cfg := fixture(t)
	e := env(root, "relative/path")
	if _, err := Dispatch(config.RoleImplementer, []string{"test"}, e, cfg, (&capture{}).run); err == nil {
		t.Fatal("relative worktree succeeded, want refusal")
	}
}

func TestEnvFromMapReadsExpectedKeys(t *testing.T) {
	got := EnvFrom(func(k string) string {
		switch k {
		case "CREW_WORKTREE":
			return "/w"
		case "CREW_PROJECT_ROOT":
			return "/p"
		case "CREW_TASK_ID":
			return "alpha"
		}
		return ""
	})
	want := Env{Worktree: "/w", ProjectRoot: "/p", TaskID: "alpha"}
	if got != want {
		t.Fatalf("EnvFrom = %+v, want %+v", got, want)
	}
}
