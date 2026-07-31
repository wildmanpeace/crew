package worker

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	c := &config.Config{
		CheckCommands: map[string]config.CheckCommand{
			"test": {Argv: []string{"go", "test"}, DefaultArgs: []string{"./..."}},
		},
	}
	// Round-trip through the package's own defaulting.
	raw, _ := json.Marshal(c)
	var out config.Config
	json.Unmarshal(raw, &out)
	out.VerifyTestSuffix = "_crewverify_test.go"
	out.ImplementerModel = "sonnet"
	out.VerifierModel = "sonnet"
	return &out
}

func spec(role config.Role) Spec {
	return Spec{
		Role:         role,
		TaskID:       "alpha",
		Attempt:      1,
		Cycle:        1,
		Worktree:     "/proj/.crew/worktrees/alpha/attempt-1",
		ProjectRoot:  "/proj",
		RunID:        "alpha-a1-c1-impl",
		Brief:        "do the thing",
		Model:        "sonnet",
		BudgetUSD:    1.125,
		SettingsPath: "/proj/.crew/worker-settings.json",
	}
}

// flagValues returns every value passed for a repeated or single flag.
func flagValues(argv []string, flag string) []string {
	var out []string
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func firstValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	v := flagValues(argv, flag)
	if len(v) == 0 {
		t.Fatalf("flag %s not present in %q", flag, argv)
	}
	return v[0]
}

// Phase 0: --max-turns does not exist in this Claude Code version.
func TestNeverPassesMaxTurns(t *testing.T) {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		argv := ClaudeArgs(spec(role), testCfg(t))
		if slices.Contains(argv, "--max-turns") {
			t.Fatalf("%s passes --max-turns, which does not exist: %q", role, argv)
		}
	}
}

func TestPassesMaxBudgetUSD(t *testing.T) {
	argv := ClaudeArgs(spec(config.RoleImplementer), testCfg(t))
	got := firstValue(t, argv, "--max-budget-usd")
	if got != "1.13" && got != "1.12" {
		t.Fatalf("--max-budget-usd = %q, want the spec budget rounded to cents", got)
	}
}

func TestCoreFlags(t *testing.T) {
	argv := ClaudeArgs(spec(config.RoleImplementer), testCfg(t))
	if argv[0] != "-p" {
		t.Errorf("argv[0] = %q, want -p", argv[0])
	}
	if firstValue(t, argv, "--output-format") != "json" {
		t.Error("--output-format json missing")
	}
	if firstValue(t, argv, "--permission-mode") != "dontAsk" {
		t.Error("--permission-mode dontAsk missing")
	}
	if firstValue(t, argv, "--model") != "sonnet" {
		t.Error("--model missing")
	}
	if firstValue(t, argv, "--settings") != "/proj/.crew/worker-settings.json" {
		t.Error("--settings missing")
	}
}

// Phase 0 finding 1: Write(path) rules are accepted and never consulted.
// Only Edit(path) and Read(path) are checked, and Edit covers all
// file-editing tools.
func TestNeverUsesWritePathRules(t *testing.T) {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		argv := ClaudeArgs(spec(role), testCfg(t))
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "Write(") {
			t.Fatalf("%s uses a Write(path) rule, which is silently ignored: %q", role, joined)
		}
	}
}

// Phase 0 finding 1: a single leading slash anchors at the settings source,
// not the filesystem root. Absolute path rules need a // prefix.
func TestAbsolutePathRulesUseDoubleSlash(t *testing.T) {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		argv := ClaudeArgs(spec(role), testCfg(t))
		for _, flag := range []string{"--allowedTools", "--disallowedTools"} {
			for _, val := range flagValues(argv, flag) {
				for _, rule := range strings.Split(val, ",") {
					open := strings.Index(rule, "(")
					if open < 0 {
						continue
					}
					inner := rule[open+1 : len(rule)-1]
					if strings.HasPrefix(inner, "/") && !strings.HasPrefix(inner, "//") {
						t.Errorf("%s rule %q uses a single leading slash; it will anchor at the settings source", role, rule)
					}
				}
			}
		}
	}
}

// Phase 0 finding 1: a bare Edit deny also kills the verifier's permitted
// writes, because Edit rules cover every file-editing tool.
func TestVerifierDoesNotDenyBareEdit(t *testing.T) {
	argv := ClaudeArgs(spec(config.RoleVerifier), testCfg(t))
	for _, val := range flagValues(argv, "--disallowedTools") {
		for _, rule := range strings.Split(val, ",") {
			if strings.TrimSpace(rule) == "Edit" {
				t.Fatal("verifier denies bare Edit, which also blocks its own permitted writes")
			}
		}
	}
}

func TestVerifierMayWriteOnlyVerifyTestsAndItsReport(t *testing.T) {
	argv := ClaudeArgs(spec(config.RoleVerifier), testCfg(t))
	allow := strings.Join(flagValues(argv, "--allowedTools"), ",")
	wantEdits := []string{
		"Edit(//proj/.crew/worktrees/alpha/attempt-1/**/*_crewverify_test.go)",
		"Edit(//proj/.crew/worktrees/alpha/attempt-1/.crew-report.json)",
	}
	for _, w := range wantEdits {
		if !strings.Contains(allow, w) {
			t.Errorf("verifier allow-list missing %q\ngot: %s", w, allow)
		}
	}
	if strings.Contains(allow, "Edit(//proj/.crew/worktrees/alpha/attempt-1/**)") {
		t.Error("verifier may edit the whole worktree; it must not touch implementation code")
	}
}

func TestImplementerMayEditItsWorktree(t *testing.T) {
	argv := ClaudeArgs(spec(config.RoleImplementer), testCfg(t))
	allow := strings.Join(flagValues(argv, "--allowedTools"), ",")
	if !strings.Contains(allow, "Edit(//proj/.crew/worktrees/alpha/attempt-1/**)") {
		t.Errorf("implementer cannot edit its worktree: %s", allow)
	}
}

// The role boundary in the permission layer mirrors the PATH boundary.
func TestEachRoleSeesOnlyItsOwnDispatcher(t *testing.T) {
	impl := strings.Join(flagValues(ClaudeArgs(spec(config.RoleImplementer), testCfg(t)), "--allowedTools"), ",")
	if !strings.Contains(impl, "Bash(crew-run ") {
		t.Error("implementer cannot run crew-run")
	}
	if strings.Contains(impl, "crew-check") {
		t.Error("implementer allow-list mentions crew-check")
	}

	ver := strings.Join(flagValues(ClaudeArgs(spec(config.RoleVerifier), testCfg(t)), "--allowedTools"), ",")
	if strings.Contains(ver, "Bash(crew-run") {
		t.Error("verifier allow-list mentions crew-run")
	}
	for _, verb := range []string{"test", "lint", "build"} {
		if !strings.Contains(ver, "Bash(crew-check "+verb+" ") {
			t.Errorf("verifier cannot run crew-check %s: %s", verb, ver)
		}
	}
	if strings.Contains(ver, "crew-check commit") || strings.Contains(ver, "crew-check diff") {
		t.Error("verifier allow-list exposes commit or diff")
	}
}

func TestDenyRulesCloseTheReadOnlyCommandHole(t *testing.T) {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		deny := strings.Join(flagValues(ClaudeArgs(spec(role), testCfg(t)), "--disallowedTools"), ",")
		for _, want := range []string{"Bash(git ", "Bash(mv ", "Bash(cp ", "Bash(curl ", "Bash(wget "} {
			if !strings.Contains(deny, want) {
				t.Errorf("%s deny-list missing %q: %s", role, want, deny)
			}
		}
	}
}

// The worktree contains the repo's own .claude and .crew directories. A
// worker editing those would be editing crew's own guardrails.
func TestWorkersCannotEditProjectControlDirectories(t *testing.T) {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		deny := strings.Join(flagValues(ClaudeArgs(spec(role), testCfg(t)), "--disallowedTools"), ",")
		for _, want := range []string{".claude/**", ".crew/**"} {
			if !strings.Contains(deny, want) {
				t.Errorf("%s does not deny edits to %s: %s", role, want, deny)
			}
		}
	}
}

func TestPromptIsPresentAndCarriesTheBrief(t *testing.T) {
	s := spec(config.RoleImplementer)
	s.Brief = "add a token-bucket limiter"
	argv := ClaudeArgs(s, testCfg(t))
	joined := strings.Join(argv, "\n")
	if !strings.Contains(joined, "add a token-bucket limiter") {
		t.Fatal("brief not present in the invocation")
	}
}

// The verifier is briefed from the original criteria and the diff, never from
// the implementer's account of its own work.
func TestVerifierPromptExcludesImplementerSummary(t *testing.T) {
	s := spec(config.RoleVerifier)
	s.Brief = "verify the limiter"
	s.ImplementerSummary = "I definitely implemented everything correctly"
	argv := ClaudeArgs(s, testCfg(t))
	if strings.Contains(strings.Join(argv, "\n"), "definitely implemented") {
		t.Fatal("verifier invocation leaked the implementer's summary")
	}
}

func TestBudgetIsFormattedAsPlainDecimal(t *testing.T) {
	s := spec(config.RoleImplementer)
	s.BudgetUSD = 0.5
	got := firstValue(t, ClaudeArgs(s, testCfg(t)), "--max-budget-usd")
	if strings.ContainsAny(got, "e$ ") {
		t.Fatalf("--max-budget-usd = %q, want a plain decimal", got)
	}
}

func TestRoleBinDirsAreDistinctAndRoleScoped(t *testing.T) {
	impl := RoleBinDir("/proj", config.RoleImplementer)
	ver := RoleBinDir("/proj", config.RoleVerifier)
	if impl == ver {
		t.Fatal("both roles share a bin directory")
	}
	if !strings.HasPrefix(impl, filepath.Join("/proj", ".crew", "bin")) {
		t.Errorf("bin dir outside .crew/bin: %q", impl)
	}
}

// The dispatchers must never sit in a directory both roles can see, or the
// PATH boundary the design rests on would not exist.
func TestWorkerPathPutsOnlyItsOwnRoleBinFirst(t *testing.T) {
	base := "/usr/bin:/bin"
	got := WorkerPath("/proj", config.RoleVerifier, base)
	parts := strings.Split(got, ":")
	if parts[0] != RoleBinDir("/proj", config.RoleVerifier) {
		t.Fatalf("PATH[0] = %q, want the verifier bin dir", parts[0])
	}
	if slices.Contains(parts, RoleBinDir("/proj", config.RoleImplementer)) {
		t.Fatal("verifier PATH contains the implementer bin dir")
	}
}
