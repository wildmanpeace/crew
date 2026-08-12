package scaffold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/tasks"
)

func mustProfile(t *testing.T, lang Language) Profile {
	t.Helper()
	p, err := ProfileFor(lang)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// The whole point of init: a directory that crew can drive, without anyone
// having read the README first.
func TestWriteProducesAConfigCrewCanLoad(t *testing.T) {
	dir := t.TempDir()
	p := mustProfile(t, TypeScript)

	if _, err := Write(dir, p, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	if cfg.TestFileSuffix != p.TestFileSuffix {
		t.Errorf("TestFileSuffix = %q, want %q", cfg.TestFileSuffix, p.TestFileSuffix)
	}
	if cfg.VerifyTestSuffix != p.VerifyTestSuffix {
		t.Errorf("VerifyTestSuffix = %q, want %q", cfg.VerifyTestSuffix, p.VerifyTestSuffix)
	}
	if len(cfg.NegativeControlBuildFailureMarkers) != len(p.BuildFailureMarkers) {
		t.Errorf("markers = %v, want %v", cfg.NegativeControlBuildFailureMarkers, p.BuildFailureMarkers)
	}
	argv, err := cfg.Resolve("test", nil)
	if err != nil {
		t.Fatalf("Resolve(test): %v", err)
	}
	if !slices.Equal(argv, p.CheckCommands["test"].Argv) {
		t.Errorf("test argv = %v, want %v", argv, p.CheckCommands["test"].Argv)
	}
}

func TestWriteProducesEveryFileTheProjectNeeds(t *testing.T) {
	dir := t.TempDir()

	res, err := Write(dir, mustProfile(t, Go), false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, name := range []string{
		".crew/config.json", ".gitignore", ".claude/settings.json",
		"AGENTS.md", "CLAUDE.md", "TASKS.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
		if !slices.Contains(res.Written, name) {
			t.Errorf("%s is missing from the report", name)
		}
	}
}

// Claude Code reads CLAUDE.md, not AGENTS.md, so the import is what makes the
// role load at all.
func TestWrittenClaudeMdImportsAgentsMd(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, mustProfile(t, Go), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, dir, "CLAUDE.md"), "@AGENTS.md") {
		t.Error("CLAUDE.md does not import AGENTS.md; the first mate role would never load")
	}
}

// A first mate that proposes Go check commands in a TypeScript repo wastes a
// cycle before anyone notices.
func TestWrittenAgentsMdSpeaksTheProjectsLanguage(t *testing.T) {
	dir := t.TempDir()
	p := mustProfile(t, TypeScript)
	if _, err := Write(dir, p, false); err != nil {
		t.Fatal(err)
	}
	body := read(t, dir, "AGENTS.md")
	if !strings.Contains(body, p.CheckExample) {
		t.Errorf("AGENTS.md does not carry the project's check example %q", p.CheckExample)
	}
	if strings.Contains(body, "./pkg/...") || strings.Contains(body, "_test.go") {
		t.Error("AGENTS.md leaked Go examples into a TypeScript project")
	}
}

// The scaffolded TASKS.md carries a worked example inside a fenced code
// block so a fresh project has something to imitate. That example must never
// be mistaken for a real task, or a brand-new project fails to parse until
// the captain hand-edits the stub.
func TestScaffoldedTasksMdParsesToZeroTasks(t *testing.T) {
	for _, lang := range Languages() {
		t.Run(string(lang), func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Write(dir, mustProfile(t, lang), false); err != nil {
				t.Fatalf("Write: %v", err)
			}
			f, err := os.Open(filepath.Join(dir, "TASKS.md"))
			if err != nil {
				t.Fatalf("open TASKS.md: %v", err)
			}
			defer f.Close()
			got, err := tasks.Parse(f)
			if err != nil {
				t.Fatalf("the scaffolded TASKS.md does not parse: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %d tasks from the scaffold's worked example, want 0", len(got))
			}
		})
	}
}

// Init runs against real projects, which already have these files.
func TestWriteNeverClobbersWithoutForce(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "AGENTS.md", "my own instructions\n")
	write(t, dir, "TASKS.md", "## task: existing\n")

	res, err := Write(dir, mustProfile(t, Go), false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := read(t, dir, "AGENTS.md"); got != "my own instructions\n" {
		t.Errorf("AGENTS.md was overwritten: %q", got)
	}
	for _, name := range []string{"AGENTS.md", "TASKS.md"} {
		if !slices.Contains(res.Skipped, name) {
			t.Errorf("%s was skipped but not reported", name)
		}
	}

	if _, err := Write(dir, mustProfile(t, Go), true); err != nil {
		t.Fatalf("Write(force): %v", err)
	}
	if read(t, dir, "AGENTS.md") == "my own instructions\n" {
		t.Error("--force did not overwrite AGENTS.md")
	}
}

// .gitignore and settings.json belong to the project, not to crew, so they are
// merged rather than written.
func TestGitignoreIsAppendedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".gitignore", "node_modules/\n")

	if _, err := Write(dir, mustProfile(t, Go), false); err != nil {
		t.Fatal(err)
	}
	body := read(t, dir, ".gitignore")
	if !strings.Contains(body, "node_modules/") {
		t.Error("the project's own .gitignore entries were lost")
	}
	if !strings.Contains(body, "/.crew/state.json") {
		t.Error("crew's working files were not ignored")
	}

	// Running init twice must not duplicate every line.
	if _, err := Write(dir, mustProfile(t, Go), false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(read(t, dir, ".gitignore"), "/.crew/state.json"); n != 1 {
		t.Errorf("/.crew/state.json appears %d times, want 1", n)
	}
}

func TestSettingsMergeKeepsExistingKeysAndAddsTheDenyRule(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".claude/settings.json",
		`{"permissions":{"allow":["Bash(go test *)"],"deny":["Bash(rm *)"]}}`)

	if _, err := Write(dir, mustProfile(t, Go), false); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(read(t, dir, ".claude/settings.json")), &got); err != nil {
		t.Fatalf("the merged settings are not valid JSON: %v", err)
	}
	if !slices.Contains(got.Permissions.Allow, "Bash(go test *)") {
		t.Error("an existing allow rule was dropped")
	}
	if !slices.Contains(got.Permissions.Deny, "Bash(rm *)") {
		t.Error("an existing deny rule was dropped")
	}
	if !slices.Contains(got.Permissions.Deny, ApproveDenyRule) {
		t.Errorf("deny = %v, missing %q", got.Permissions.Deny, ApproveDenyRule)
	}

	// Idempotent: a second run must not double the rule.
	if _, err := Write(dir, mustProfile(t, Go), false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(read(t, dir, ".claude/settings.json"), ApproveDenyRule); n != 1 {
		t.Errorf("the deny rule appears %d times, want 1", n)
	}
}
