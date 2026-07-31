package dispatch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests exercise the real crew-run and crew-check binaries against a
// real git worktree. The unit tests prove the decision logic; these prove the
// binaries as a worker would actually encounter them on its PATH.

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "crew-bin-*")
		if buildErr != nil {
			return
		}
		for _, pkg := range []string{"crew-run", "crew-check"} {
			cmd := exec.Command("go", "build", "-o",
				filepath.Join(binDir, pkg), "github.com/wildmanpeace/crew/cmd/"+pkg)
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = err
				t.Logf("build %s: %s", pkg, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("building dispatchers: %v", buildErr)
	}
	return binDir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// project builds a real git repo with a Go module and a live worktree.
func project(t *testing.T) (root, worktree string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main")
	write(t, filepath.Join(root, "go.mod"), "module probe\n\ngo 1.26\n")
	write(t, filepath.Join(root, "ok.go"), "package probe\n\nfunc Add(a, b int) int { return a + b }\n")
	write(t, filepath.Join(root, "ok_test.go"),
		"package probe\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	crewDir := filepath.Join(root, ".crew")
	if err := os.MkdirAll(crewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(crewDir, "config.json"), `{
	  "check_commands": {
	    "test":  {"argv": ["go","test"], "default_args": ["./..."]},
	    "build": {"argv": ["go","build"], "default_args": ["./..."]}
	  }
	}`)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "base")

	worktree = filepath.Join(crewDir, "worktrees", "alpha", "attempt-1")
	git(t, root, "worktree", "add", "-q", "-b", "crew/alpha/attempt-1", worktree)
	return root, worktree
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	code   int
	output string
}

func dispatchRun(t *testing.T, bin, root, worktree string, args ...string) result {
	t.Helper()
	cmd := exec.Command(filepath.Join(binaries(t), bin), args...)
	cmd.Env = append(os.Environ(),
		"CREW_WORKTREE="+worktree,
		"CREW_PROJECT_ROOT="+root,
		"CREW_TASK_ID=alpha")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return result{code: code, output: string(out)}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func TestIntegrationImplementerCanRunConfiguredCheck(t *testing.T) {
	root, wt := project(t)
	got := dispatchRun(t, "crew-run", root, wt, "test")
	if got.code != 0 {
		t.Fatalf("crew-run test exit = %d\n%s", got.code, got.output)
	}
	if !strings.Contains(got.output, "ok") {
		t.Errorf("output does not look like go test:\n%s", got.output)
	}
}

// The core role boundary, proven against the real binary.
func TestIntegrationVerifierHasNoCommitOrDiff(t *testing.T) {
	root, wt := project(t)
	for _, verb := range []string{"commit", "diff"} {
		got := dispatchRun(t, "crew-check", root, wt, verb, "message")
		if got.code != 2 {
			t.Errorf("crew-check %s exit = %d, want 2\n%s", verb, got.code, got.output)
		}
		if !strings.Contains(got.output, "does not expose verb") {
			t.Errorf("crew-check %s output = %q", verb, got.output)
		}
	}
}

func TestIntegrationVerifierCanStillRunChecks(t *testing.T) {
	root, wt := project(t)
	if got := dispatchRun(t, "crew-check", root, wt, "test"); got.code != 0 {
		t.Fatalf("crew-check test exit = %d\n%s", got.code, got.output)
	}
}

func TestIntegrationUnknownVerbIsRefused(t *testing.T) {
	root, wt := project(t)
	got := dispatchRun(t, "crew-run", root, wt, "frobnicate")
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", got.code, got.output)
	}
}

// A failing check must surface as the check's own exit code, distinct from
// the policy refusal code, so crew can tell the two apart.
func TestIntegrationFailingCheckIsDistinctFromPolicyRefusal(t *testing.T) {
	root, wt := project(t)
	write(t, filepath.Join(wt, "fail_test.go"),
		"package probe\n\nimport \"testing\"\n\nfunc TestAlwaysFails(t *testing.T) { t.Fatal(\"nope\") }\n")
	got := dispatchRun(t, "crew-run", root, wt, "test")
	if got.code == 0 {
		t.Fatalf("failing test reported success\n%s", got.output)
	}
	if got.code == 2 {
		t.Fatalf("failing test was reported as a policy refusal\n%s", got.output)
	}
}

// Shell metacharacters are inert because nothing is ever handed to a shell.
func TestIntegrationShellMetacharactersAreInert(t *testing.T) {
	root, wt := project(t)
	canary := filepath.Join(root, "PWNED")
	got := dispatchRun(t, "crew-run", root, wt, "test", "; touch "+canary)
	if got.code == 0 {
		t.Errorf("expected go test to reject the bogus argument\n%s", got.output)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("metacharacters were interpreted: canary file was created")
	}
}

func TestIntegrationCommitMessageIsNotInterpolated(t *testing.T) {
	root, wt := project(t)
	write(t, filepath.Join(wt, "new.go"), "package probe\n\nvar X = 1\n")
	canary := filepath.Join(root, "PWNED_COMMIT")
	msg := `feat: add X"; touch ` + canary + ` #`

	got := dispatchRun(t, "crew-run", root, wt, "commit", msg)
	if got.code != 0 {
		t.Fatalf("commit exit = %d\n%s", got.code, got.output)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("commit message was interpreted by a shell")
	}
	subject := strings.TrimSpace(git(t, wt, "log", "-1", "--pretty=%s"))
	if subject != msg {
		t.Fatalf("commit subject = %q, want the exact message %q", subject, msg)
	}
}

func TestIntegrationCommitRequiresExactlyOneArgument(t *testing.T) {
	root, wt := project(t)
	write(t, filepath.Join(wt, "new.go"), "package probe\n\nvar X = 1\n")
	for _, args := range [][]string{{"commit"}, {"commit", "a", "b"}} {
		if got := dispatchRun(t, "crew-run", root, wt, args...); got.code != 2 {
			t.Errorf("crew-run %v exit = %d, want 2\n%s", args, got.code, got.output)
		}
	}
}

func TestIntegrationDiffExcludesVerifierTests(t *testing.T) {
	root, wt := project(t)
	write(t, filepath.Join(wt, "impl.go"), "package probe\n\nvar Y = 2\n")
	write(t, filepath.Join(wt, "thing_crewverify_test.go"),
		"package probe\n\nimport \"testing\"\n\nfunc TestVerifierAuthored(t *testing.T) {}\n")
	got := dispatchRun(t, "crew-run", root, wt, "diff")
	if got.code != 0 {
		t.Fatalf("diff exit = %d\n%s", got.code, got.output)
	}
	if !strings.Contains(got.output, "impl.go") {
		t.Errorf("diff omitted implementation changes:\n%s", got.output)
	}
	if strings.Contains(got.output, "crewverify") {
		t.Errorf("diff leaked verifier-authored tests:\n%s", got.output)
	}
}

func TestIntegrationWorktreeOutsideProjectIsRefused(t *testing.T) {
	root, _ := project(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	got := dispatchRun(t, "crew-run", root, outside, "test")
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", got.code, got.output)
	}
	if !strings.Contains(got.output, "outside") {
		t.Errorf("output = %q", got.output)
	}
}

// A worker controls its own environment, so the role must not be readable
// from it: setting CREW_ROLE must not turn crew-check into an implementer.
func TestIntegrationRoleCannotBeOverriddenByEnvironment(t *testing.T) {
	root, wt := project(t)
	cmd := exec.Command(filepath.Join(binaries(t), "crew-check"), "commit", "sneaky")
	cmd.Env = append(os.Environ(),
		"CREW_WORKTREE="+wt,
		"CREW_PROJECT_ROOT="+root,
		"CREW_TASK_ID=alpha",
		"CREW_ROLE=implementer",
		"ROLE=implementer")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("crew-check honoured an environment role override:\n%s", out)
	}
	if !strings.Contains(string(out), "does not expose verb") {
		t.Fatalf("unexpected failure: %s", out)
	}
}
