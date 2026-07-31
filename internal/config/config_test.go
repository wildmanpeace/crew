package config

import (
	"os"
	"path/filepath"
	"reflect"
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
