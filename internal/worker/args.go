// Package worker builds and runs the one-shot claude -p invocations that do
// the actual implementing and verifying.
//
// The permission flags here encode what Phase 0 measured rather than what the
// documentation implies:
//
//   - --max-turns does not exist in this Claude Code version; --max-budget-usd
//     is the bound that holds during a run.
//   - Write(path) rules are accepted and never consulted. Only Edit(path) and
//     Read(path) are checked, and Edit rules cover every file-editing tool.
//   - An absolute path rule needs a // prefix; a single leading slash anchors
//     at the settings source instead of the filesystem root.
//   - A bare Edit deny also blocks the verifier's own permitted writes, so the
//     verifier is confined by narrow allow rules instead.
package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/report"
)

// Spec is everything needed to invoke one worker.
type Spec struct {
	Role        config.Role `json:"role"`
	TaskID      string      `json:"task_id"`
	Attempt     int         `json:"attempt"`
	Cycle       int         `json:"cycle"`
	Worktree    string      `json:"worktree"`
	ProjectRoot string      `json:"project_root"`
	RunID       string      `json:"run_id"`

	// Brief is the prompt body. For a verifier it is built from the original
	// brief, the tagged criteria, and the diff; it never contains the
	// implementer's account of its own work.
	Brief string `json:"brief"`

	// ImplementerSummary is carried for logging and for the captain's review.
	// It is deliberately never passed to a verifier.
	ImplementerSummary string `json:"implementer_summary,omitempty"`

	Model        string  `json:"model"`
	BudgetUSD    float64 `json:"budget_usd"`
	SettingsPath string  `json:"settings_path"`

	// Args is the claude argument list, built by ClaudeArgs. It is a field so
	// the exact invocation crew ran can be recorded and replayed.
	Args []string `json:"args,omitempty"`
}

// rulePath renders an absolute filesystem path as a permission rule path.
// The doubled leading slash anchors at the filesystem root.
func rulePath(abs string, elem ...string) string {
	p := filepath.Join(append([]string{abs}, elem...)...)
	return "/" + p
}

// ClaudeArgs builds the full argument list for `claude`, excluding the
// program name.
func ClaudeArgs(s Spec, cfg *config.Config) []string {
	wt := filepath.Clean(s.Worktree)

	argv := []string{
		"-p", s.Brief,
		"--model", s.Model,
		"--output-format", "json",
		"--max-budget-usd", strconv.FormatFloat(s.BudgetUSD, 'f', 2, 64),
		"--permission-mode", "dontAsk",
		"--settings", s.SettingsPath,
		"--add-dir", wt,
	}

	allow := []string{"Read(" + rulePath(wt, "**") + ")"}
	switch s.Role {
	case config.RoleVerifier:
		// Narrow allow rules are the confinement: the verifier may add its own
		// tests and write its report, and has no rule permitting anything else.
		allow = append(allow,
			"Edit("+rulePath(wt, "**", "*"+cfg.VerifyTestSuffix)+")",
			"Edit("+rulePath(wt, report.Filename)+")",
		)
		for _, verb := range config.VerbsForRole(config.RoleVerifier) {
			allow = append(allow, "Bash(crew-check "+verb+" *)")
		}
	default:
		allow = append(allow,
			"Edit("+rulePath(wt, "**")+")",
			"Bash(crew-run *)",
		)
	}

	// Deny rules take precedence over allow rules and over the built-in
	// read-only command set, which is otherwise not closable by configuration.
	deny := []string{
		"Bash(git *)",
		"Bash(mv *)",
		"Bash(cp *)",
		"Bash(find *)",
		"Bash(curl *)",
		"Bash(wget *)",
		"Bash(ssh *)",
		"Bash(nc *)",
		// The worktree is a checkout of the repo and so contains the project's
		// own control directories. A worker editing those would be editing
		// crew's guardrails, and the change would land.
		"Edit(" + rulePath(wt, ".claude", "**") + ")",
		"Edit(" + rulePath(wt, ".crew", "**") + ")",
	}

	argv = append(argv, "--allowedTools", strings.Join(allow, ","))
	argv = append(argv, "--disallowedTools", strings.Join(deny, ","))
	return argv
}

// RoleBinDir is the only directory containing a role's dispatcher. The two
// roles never share one: the boundary the design rests on is which binary is
// reachable at all, so a shared bin directory would dissolve it.
func RoleBinDir(projectRoot string, role config.Role) string {
	return filepath.Join(projectRoot, ".crew", "bin", string(role))
}

// DispatcherName is the binary a role's workers invoke.
func DispatcherName(role config.Role) string {
	if role == config.RoleVerifier {
		return "crew-check"
	}
	return "crew-run"
}

// WorkerPath builds the PATH for a worker: its own role's dispatcher
// directory first, then the toolchain directories the checks need.
//
// Any directory that contains a dispatcher is dropped from the inherited
// path, not just the other role's project-local bin. The role boundary is
// that a verifier has no crew-run reachable anywhere; if crew's own binaries
// happened to be installed somewhere already on PATH, inheriting it verbatim
// would quietly dissolve that.
func WorkerPath(projectRoot string, role config.Role, basePath string) string {
	mine := RoleBinDir(projectRoot, role)
	var kept []string
	for _, p := range filepath.SplitList(basePath) {
		if p == "" || p == mine || containsDispatcher(p) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(append([]string{mine}, kept...), ":")
}

// containsDispatcher reports whether a directory holds either dispatcher.
func containsDispatcher(dir string) bool {
	for _, name := range []string{"crew-run", "crew-check"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func otherRole(r config.Role) config.Role {
	if r == config.RoleVerifier {
		return config.RoleImplementer
	}
	return config.RoleVerifier
}

// workerEnvPassthrough is what a worker inherits from crew's own environment.
//
// The list is explicit rather than a wholesale copy so a worker's environment
// is something crew decides. It must include HOME and the credential
// variables: the worker is the claude CLI, and without them it cannot reach
// its own login and fails with "Not logged in" before doing any work.
var workerEnvPassthrough = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM",
	"LANG", "LC_ALL", "LC_CTYPE",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	// Credentials and CLI configuration.
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"AWS_REGION", "AWS_PROFILE", "GOOGLE_APPLICATION_CREDENTIALS",
	// Toolchain caches, so checks do not redownload the world each run.
	"GOPATH", "GOMODCACHE", "GOCACHE", "GOROOT", "GOPROXY", "GOFLAGS",
}

// Env builds the environment a worker runs under.
//
// PATH is replaced rather than inherited, so the role's dispatcher is the only
// one reachable; everything else is an explicit passthrough.
func Env(s Spec, basePath string) []string {
	env := []string{
		"CREW_WORKTREE=" + s.Worktree,
		"CREW_PROJECT_ROOT=" + s.ProjectRoot,
		"CREW_TASK_ID=" + s.TaskID,
		"PATH=" + WorkerPath(s.ProjectRoot, s.Role, basePath),
	}
	for _, k := range workerEnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// RunID names a single worker invocation, unique across a task's attempts and
// cycles.
func RunID(taskID string, attempt, cycle int, role config.Role) string {
	short := "impl"
	if role == config.RoleVerifier {
		short = "verify"
	}
	return fmt.Sprintf("%s-a%d-c%d-%s", taskID, attempt, cycle, short)
}
