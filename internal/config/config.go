// Package config loads and validates .crew/config.json.
//
// Check commands are structured argv rather than the spec's shell strings:
// the worker invokes `crew-check test ./middleware/... -run TestX`, and a
// string form cannot compose a base command with worker-supplied arguments
// without a shell. Nothing here ever produces a shell invocation.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Role determines which dispatcher binary a worker gets, and therefore which
// verbs are reachable at all. The boundary is which binary is on PATH, not a
// runtime decision.
type Role string

const (
	RoleImplementer Role = "implementer"
	RoleVerifier    Role = "verifier"
)

var roleVerbs = map[Role][]string{
	RoleImplementer: {"test", "lint", "build", "diff", "commit"},
	RoleVerifier:    {"test", "lint", "build"},
}

// VerbsForRole returns the verbs a role's dispatcher exposes.
func VerbsForRole(r Role) []string { return slices.Clone(roleVerbs[r]) }

// RoleAllows reports whether a role's dispatcher exposes verb.
func RoleAllows(r Role, verb string) bool { return slices.Contains(roleVerbs[r], verb) }

// configurableVerbs are the verbs that map to a project command. diff and
// commit are implemented by the dispatcher itself against git, so they are
// not configurable.
var configurableVerbs = []string{"test", "lint", "build"}

// CheckCommand is a project command as argv, never a shell string.
type CheckCommand struct {
	Argv        []string `json:"argv"`
	DefaultArgs []string `json:"default_args,omitempty"`
}

// Config is the parsed .crew/config.json plus applied defaults.
type Config struct {
	ConcurrencyCap          int     `json:"concurrency_cap"`
	PollIntervalSeconds     int     `json:"poll_interval_seconds"`
	WallClockTimeoutSeconds int     `json:"wall_clock_timeout_seconds"`
	MaxCycles               int     `json:"max_cycles"`
	PerTaskCostCapUSD       float64 `json:"per_task_cost_cap_usd"`
	ProjectCostCapUSDPerDay float64 `json:"project_cost_cap_usd_per_day"`
	PerWorkerBudgetUSD      float64 `json:"per_worker_budget_usd"`

	// BudgetSafetyMargin shrinks the budget handed to a worker. Phase 0
	// measured --max-budget-usd overshooting its cap by 3.5x, because the cap
	// is applied after the turn that breaches it. Without a margin, a worker
	// given exactly the remaining headroom can still breach the ceiling.
	BudgetSafetyMargin float64 `json:"budget_safety_margin"`

	// MinSpawnBudgetUSD is the smallest budget worth starting a worker with.
	// Below this, crew refuses the spawn rather than starting a worker that
	// will be killed mid-turn and still overshoot.
	MinSpawnBudgetUSD float64 `json:"min_spawn_budget_usd"`

	BudgetTimezone string `json:"budget_timezone"`
	MainBranch     string `json:"main_branch"`

	// AutoLand lets crew watch land a task the captain has approved. Approval
	// is the decision; the merge that follows re-checks the approved sha and
	// decides nothing, so making the captain type it is ceremony rather than a
	// gate. A pointer because false is a bool's zero value and an omitted key
	// has to mean on: use AutoLandEnabled rather than reading it directly.
	AutoLand *bool `json:"auto_land"`

	// VerifyTestSuffix identifies verifier-authored tests. They live beside
	// the code they exercise: Go ignores directories beginning with "." or
	// "_", so the spec's .crew-verify/ would never be compiled or run.
	VerifyTestSuffix string `json:"verify_test_suffix"`

	// TestFileSuffix identifies a test file in this project. It is what lets
	// crew tell a test the branch introduced from one that already existed,
	// which decides whether a mechanical criterion needs a negative control.
	TestFileSuffix string `json:"test_file_suffix"`

	ImplementerModel string `json:"implementer_model"`
	VerifierModel    string `json:"verifier_model"`

	CheckCommands map[string]CheckCommand `json:"check_commands"`

	// NegativeControlBuildFailureMarkers distinguish a compile/collect failure
	// from a genuine assertion failure. Both exit non-zero, so exit code alone
	// cannot tell them apart.
	NegativeControlBuildFailureMarkers []string `json:"negative_control_build_failure_markers"`
}

// AutoLandEnabled reports whether crew watch should land approved tasks. A nil
// AutoLand means the key was omitted, which is on.
func (c *Config) AutoLandEnabled() bool { return c.AutoLand == nil || *c.AutoLand }

// Path returns the config file location for a project root.
func Path(projectRoot string) string {
	return filepath.Join(projectRoot, ".crew", "config.json")
}

// Load reads, defaults, and validates the project config.
func Load(projectRoot string) (*Config, error) {
	raw, err := os.ReadFile(Path(projectRoot))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	setInt := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	setFloat := func(p *float64, v float64) {
		if *p == 0 {
			*p = v
		}
	}
	setStr := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	setInt(&c.ConcurrencyCap, 3)
	setInt(&c.PollIntervalSeconds, 15)
	setInt(&c.WallClockTimeoutSeconds, 1800)
	setInt(&c.MaxCycles, 3)
	setFloat(&c.PerTaskCostCapUSD, 5.00)
	setFloat(&c.ProjectCostCapUSDPerDay, 25.00)
	setFloat(&c.PerWorkerBudgetUSD, 1.50)
	setFloat(&c.BudgetSafetyMargin, 0.25)
	setFloat(&c.MinSpawnBudgetUSD, 0.10)
	setStr(&c.BudgetTimezone, "America/Denver")
	setStr(&c.MainBranch, "main")
	setStr(&c.VerifyTestSuffix, "_crewverify_test.go")
	setStr(&c.TestFileSuffix, "_test.go")
	setStr(&c.ImplementerModel, "sonnet")
	setStr(&c.VerifierModel, "sonnet")
	if c.NegativeControlBuildFailureMarkers == nil {
		c.NegativeControlBuildFailureMarkers = []string{
			"[build failed]",
			"undefined: ",
			"cannot find package",
		}
	}
}

// Validate rejects a config that would produce an unsafe or unusable run.
func (c *Config) Validate() error {
	if len(c.CheckCommands) == 0 {
		return fmt.Errorf("check_commands must not be empty")
	}
	for verb, cmd := range c.CheckCommands {
		if !slices.Contains(configurableVerbs, verb) {
			return fmt.Errorf("check_commands: unknown verb %q (want one of %v)", verb, configurableVerbs)
		}
		if len(cmd.Argv) == 0 {
			return fmt.Errorf("check_commands.%s: argv must not be empty", verb)
		}
		if cmd.Argv[0] == "" {
			return fmt.Errorf("check_commands.%s: argv[0] must not be empty", verb)
		}
	}
	if _, ok := c.CheckCommands["test"]; !ok {
		return fmt.Errorf("check_commands must define %q", "test")
	}
	if c.MaxCycles < 1 {
		return fmt.Errorf("max_cycles must be >= 1")
	}
	if c.ConcurrencyCap < 1 {
		return fmt.Errorf("concurrency_cap must be >= 1")
	}
	if c.BudgetSafetyMargin < 0 || c.BudgetSafetyMargin >= 1 {
		return fmt.Errorf("budget_safety_margin must be in [0,1)")
	}
	if _, err := c.Location(); err != nil {
		return err
	}
	return nil
}

// Location resolves the configured budget timezone.
func (c *Config) Location() (*time.Location, error) {
	loc, err := time.LoadLocation(c.BudgetTimezone)
	if err != nil {
		return nil, fmt.Errorf("budget_timezone %q: %w", c.BudgetTimezone, err)
	}
	return loc, nil
}

// Resolve composes the argv for a configured verb. Worker-supplied arguments
// replace the configured defaults; when the worker supplies none, the
// defaults apply. An unconfigured verb is a hard error.
func (c *Config) Resolve(verb string, workerArgs []string) ([]string, error) {
	cmd, ok := c.CheckCommands[verb]
	if !ok {
		return nil, fmt.Errorf("verb %q is not configured in check_commands", verb)
	}
	argv := slices.Clone(cmd.Argv)
	if len(workerArgs) > 0 {
		return append(argv, workerArgs...), nil
	}
	return append(argv, cmd.DefaultArgs...), nil
}
