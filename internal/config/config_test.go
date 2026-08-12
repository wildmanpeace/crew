package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	crewDir := filepath.Join(dir, ".crew")
	if err := os.MkdirAll(crewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewDir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const minimalCfg = `{
  "check_commands": {
    "test":  {"argv": ["go","test"], "default_args": ["./..."]},
    "lint":  {"argv": ["golangci-lint","run"]},
    "build": {"argv": ["go","build"], "default_args": ["./..."]}
  }
}`

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ConcurrencyCap != 3 {
		t.Errorf("ConcurrencyCap = %d, want 3", c.ConcurrencyCap)
	}
	if c.MaxCycles != 3 {
		t.Errorf("MaxCycles = %d, want 3", c.MaxCycles)
	}
	if c.MainBranch != "main" {
		t.Errorf("MainBranch = %q, want main", c.MainBranch)
	}
	if c.VerifyTestSuffix != "_crewverify_test.go" {
		t.Errorf("VerifyTestSuffix = %q", c.VerifyTestSuffix)
	}
	if c.ImplementerModel != "sonnet" || c.VerifierModel != "sonnet" {
		t.Errorf("models = %q/%q, want sonnet/sonnet", c.ImplementerModel, c.VerifierModel)
	}
}

// Phase 0 finding 3: --max-budget-usd overshot its cap 3.5x, so a margin is required.
func TestLoadDefaultsBudgetSafetyMargin(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.BudgetSafetyMargin <= 0 || c.BudgetSafetyMargin >= 1 {
		t.Fatalf("BudgetSafetyMargin = %v, want a fraction in (0,1)", c.BudgetSafetyMargin)
	}
}

// Phase 0 finding 2: the spec's "FAIL\tbuild failed" never matches real Go output.
func TestLoadDefaultsCorrectedBuildMarkers(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"[build failed]", "undefined: ", "cannot find package"}
	if !reflect.DeepEqual(c.NegativeControlBuildFailureMarkers, want) {
		t.Errorf("markers = %q, want %q", c.NegativeControlBuildFailureMarkers, want)
	}
}

func TestExplicitValuesOverrideDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, `{
	  "concurrency_cap": 7,
	  "max_cycles": 5,
	  "implementer_model": "opus",
	  "check_commands": {"test": {"argv": ["go","test"]}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.ConcurrencyCap != 7 || c.MaxCycles != 5 || c.ImplementerModel != "opus" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.VerifierModel != "sonnet" {
		t.Errorf("VerifierModel = %q, want default sonnet", c.VerifierModel)
	}
}

func TestResolveUsesDefaultArgsWhenWorkerPassesNone(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	got, err := c.Resolve("test", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "test", "./..."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveWorkerArgsReplaceDefaultArgs(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	got, err := c.Resolve("test", []string{"./middleware/...", "-run", "TestRateLimit429"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "test", "./middleware/...", "-run", "TestRateLimit429"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveVerbWithoutDefaultArgs(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	got, err := c.Resolve("lint", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"golangci-lint", "run"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

// Guardrail: an unconfigured verb is a hard error, never a passthrough.
func TestResolveUnknownVerbIsHardError(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	if _, err := c.Resolve("frobnicate", nil); err == nil {
		t.Fatal("Resolve(frobnicate) succeeded, want error")
	}
}

// Guardrail: Resolve must never produce something a shell would interpret.
func TestResolveNeverReturnsShellInvocation(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	got, _ := c.Resolve("test", nil)
	for _, bad := range []string{"sh", "bash", "zsh", "-c"} {
		if got[0] == bad {
			t.Fatalf("Resolve returned a shell invocation: %q", got)
		}
	}
}

func TestLoadRejectsEmptyArgv(t *testing.T) {
	if _, err := Load(writeCfg(t, `{"check_commands":{"test":{"argv":[]}}}`)); err == nil {
		t.Fatal("Load accepted an empty argv, want error")
	}
}

func TestLoadRejectsMissingTestCommand(t *testing.T) {
	if _, err := Load(writeCfg(t, `{"check_commands":{"lint":{"argv":["x"]}}}`)); err == nil {
		t.Fatal("Load accepted config with no test command, want error")
	}
}

func TestLoadRejectsUnknownVerbInConfig(t *testing.T) {
	_, err := Load(writeCfg(t, `{"check_commands":{"test":{"argv":["go","test"]},"deploy":{"argv":["sh"]}}}`))
	if err == nil {
		t.Fatal("Load accepted an unknown verb, want error")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load succeeded with no config file, want error")
	}
}

func TestVerbsAllowedForRole(t *testing.T) {
	if got := VerbsForRole(RoleImplementer); !reflect.DeepEqual(got, []string{"test", "lint", "build", "diff", "commit"}) {
		t.Errorf("implementer verbs = %q", got)
	}
	if got := VerbsForRole(RoleVerifier); !reflect.DeepEqual(got, []string{"test", "lint", "build"}) {
		t.Errorf("verifier verbs = %q", got)
	}
}

// The role boundary: crew-check must not expose commit or diff at all.
func TestVerifierCannotReachCommitOrDiff(t *testing.T) {
	for _, verb := range []string{"commit", "diff"} {
		if RoleAllows(RoleVerifier, verb) {
			t.Errorf("verifier is allowed %q, want denied", verb)
		}
		if !RoleAllows(RoleImplementer, verb) {
			t.Errorf("implementer is denied %q, want allowed", verb)
		}
	}
}

// Landing is bookkeeping once the captain has approved, so it runs by default.
// A bool cannot express "unset" by its zero value, hence the pointer: an
// omitted key must mean on, while an explicit false must stay off.
func TestAutoLandDefaultsOnAndHonoursAnExplicitFalse(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.AutoLandEnabled() {
		t.Error("AutoLandEnabled() = false for an omitted key, want true")
	}

	off, err := Load(writeCfg(t, `{"auto_land": false, "check_commands": {
	  "test": {"argv": ["go", "test"]}, "build": {"argv": ["go", "build"]}, "lint": {"argv": ["go", "vet"]}}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if off.AutoLandEnabled() {
		t.Error("AutoLandEnabled() = true for an explicit false")
	}
}

// A Config built in code, rather than loaded, must not panic on the pointer.
func TestAutoLandEnabledOnAZeroConfig(t *testing.T) {
	if !(&Config{}).AutoLandEnabled() {
		t.Error("a zero Config must default to landing, not panic or refuse")
	}
}

// Guardrail (the confinement escape): a worker's arguments are appended to a
// command it does not control, and `go test -exec <prog>` / `-toolexec <prog>`
// run an arbitrary program. A verifier that could pass either would have a
// commit path despite having no commit verb.
func TestResolveRejectsProgramRunningFlags(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	cases := [][]string{
		{"-exec", "/bin/sh"},
		{"-exec=/bin/sh"},
		{"--exec", "/bin/sh"},
		{"-toolexec=/usr/bin/evil"},
		{"--toolexec=/usr/bin/evil"},
		{"-o", "/tmp/out"},
		{"-o=/tmp/out"},
		{"./...", "-toolexec", "/usr/bin/evil"},
	}
	for _, args := range cases {
		if got, err := c.Resolve("test", args); err == nil {
			t.Errorf("Resolve(test, %q) = %q, want an error", args, got)
		}
	}
}

// The allow-list is closed: anything not named is rejected, so a flag nobody
// thought about does not arrive by default.
func TestResolveRejectsUnknownFlags(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	cases := [][]string{
		{"-gcflags=-N"},
		{"-ldflags", "-X=main.x=y"},
		{"-overlay", "/tmp/overlay.json"},
		{"-coverprofile", "/tmp/c.out"},
		{"-C", "/"},
		{"--"},
		{"-notaflag"},
	}
	for _, args := range cases {
		if got, err := c.Resolve("test", args); err == nil {
			t.Errorf("Resolve(test, %q) = %q, want an error", args, got)
		}
	}
}

// The rejection has to say which argument was refused; a worker that cannot
// tell what it did wrong will retry the same thing.
func TestResolveRejectionNamesTheFlag(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	_, err := c.Resolve("test", []string{"./...", "-toolexec=/usr/bin/evil"})
	if err == nil {
		t.Fatal("Resolve succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "-toolexec") {
		t.Errorf("error %q does not name the rejected flag", err)
	}
}

// Selectors are the point of worker arguments: the verifier narrows a run to
// the package and test it cares about, and that must keep working.
func TestResolveAllowsSelectors(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	cases := [][]string{
		{"./middleware/...", "-run", "TestRateLimit429"},
		{"./middleware/...", "-run=TestRateLimit429"},
		{"./ratelimit/...", "--run", "TestAllowRefusesWhenEmpty$"},
		{"-v", "-race", "./..."},
		{"-count=1", "-timeout", "60s", "./pkg/..."},
		{"github.com/wildmanpeace/crew/internal/config"},
		{"-run", "TestX", "-skip", "TestY", "-failfast", "-short", "./..."},
	}
	for _, args := range cases {
		got, err := c.Resolve("test", args)
		if err != nil {
			t.Errorf("Resolve(test, %q): %v", args, err)
			continue
		}
		want := append([]string{"go", "test"}, args...)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Resolve(test, %q) = %q, want %q", args, got, want)
		}
	}
}

// A value that is itself a flag would be swallowed as the previous flag's
// argument and reach the command unexamined.
func TestResolveRejectsFlagShapedValues(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	cases := [][]string{
		{"-run", "-exec"},
		{"-timeout", "-o"},
		{"-run"},
	}
	for _, args := range cases {
		if got, err := c.Resolve("test", args); err == nil {
			t.Errorf("Resolve(test, %q) = %q, want an error", args, got)
		}
	}
}

// The allow-list guards every verb a worker can reach, not just test.
func TestResolveAppliesAllowListToEveryVerb(t *testing.T) {
	c, _ := Load(writeCfg(t, minimalCfg))
	for _, verb := range []string{"test", "lint", "build"} {
		if got, err := c.Resolve(verb, []string{"-exec", "/bin/sh"}); err == nil {
			t.Errorf("Resolve(%s, -exec) = %q, want an error", verb, got)
		}
	}
}

// Configured default_args are the captain's, not a worker's, so they are not
// subject to the allow-list.
func TestResolveDoesNotAllowListConfiguredDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, `{"check_commands": {
	  "test": {"argv": ["go","test"], "default_args": ["-exec","/usr/local/bin/wrapper","./..."]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Resolve("test", nil)
	if err != nil {
		t.Fatalf("Resolve(test, nil): %v", err)
	}
	want := []string{"go", "test", "-exec", "/usr/local/bin/wrapper", "./..."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}
