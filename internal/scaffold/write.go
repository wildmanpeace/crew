package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/wildmanpeace/crew/internal/config"
)

//go:embed templates/*.tmpl
var templates embed.FS

// ApproveDenyRule refuses crew approve to any agent session running under the
// project's settings. The TTY check in crew approve already makes approval
// structurally captain-only; this fails earlier and more legibly, and says out
// loud that the boundary is deliberate.
const ApproveDenyRule = "Bash(crew approve *)"

// gitignoreLines are the files crew generates as it runs. config.json is
// absent on purpose: it is project intent and belongs in version control.
var gitignoreLines = []string{
	"/.crew/state.json",
	"/.crew/events.jsonl",
	"/.crew/state.lock",
	"/.crew/watch.lock",
	"/.crew/runs/",
	"/.crew/worktrees/",
	"/.crew/scratch/",
	"/.crew/bin/",
	"/.crew/implementer-settings.json",
	"/.crew/verifier-settings.json",
	"/.crew-report.json",
}

const gitignoreHeader = "# crew's mechanical record and working directories."

// Result reports what init did, so nothing it declined to touch is a surprise.
type Result struct {
	Written []string
	Skipped []string
}

// Write generates a project's crew setup under root.
//
// Files crew owns outright are written whole, and skipped if they already
// exist unless force is set. Files the project owns — .gitignore and
// .claude/settings.json — are merged instead, because overwriting someone
// else's configuration is never the helpful choice.
func Write(root string, p Profile, force bool) (Result, error) {
	var res Result

	rendered, err := render(p)
	if err != nil {
		return res, err
	}
	for _, f := range rendered {
		wrote, err := writeFile(root, f.name, f.body, force)
		if err != nil {
			return res, err
		}
		res.record(f.name, wrote)
	}

	changed, err := mergeGitignore(root)
	if err != nil {
		return res, err
	}
	res.record(".gitignore", changed)

	changed, err = mergeSettings(root)
	if err != nil {
		return res, err
	}
	res.record(".claude/settings.json", changed)

	return res, nil
}

func (r *Result) record(name string, wrote bool) {
	if wrote {
		r.Written = append(r.Written, name)
		return
	}
	r.Skipped = append(r.Skipped, name)
}

type file struct {
	name string
	body []byte
}

func render(p Profile) ([]file, error) {
	cfg, err := configJSON(p)
	if err != nil {
		return nil, err
	}
	out := []file{{name: filepath.Join(".crew", "config.json"), body: cfg}}

	data := struct {
		Profile
		BuildCommand string
		TestCommand  string
	}{
		Profile:      p,
		BuildCommand: strings.Join(p.CheckCommands["build"].Argv, " "),
		TestCommand:  strings.Join(p.CheckCommands["test"].Argv, " "),
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "TASKS.md"} {
		t, err := template.ParseFS(templates, "templates/"+name+".tmpl")
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		out = append(out, file{name: name, body: buf.Bytes()})
	}
	return out, nil
}

// configJSON writes only the keys a project must decide for itself. Everything
// else is left out so it picks up crew's defaults, and so the generated file
// stays short enough to read.
func configJSON(p Profile) ([]byte, error) {
	doc := struct {
		MainBranch        string                         `json:"main_branch"`
		ImplementerModel  string                         `json:"implementer_model"`
		VerifierModel     string                         `json:"verifier_model"`
		TestFileSuffix    string                         `json:"test_file_suffix"`
		VerifyTestSuffix  string                         `json:"verify_test_suffix"`
		CheckCommands     map[string]config.CheckCommand `json:"check_commands"`
		NegControlMarkers []string                       `json:"negative_control_build_failure_markers"`
	}{
		MainBranch:        "main",
		ImplementerModel:  "sonnet",
		VerifierModel:     "sonnet",
		TestFileSuffix:    p.TestFileSuffix,
		VerifyTestSuffix:  p.VerifyTestSuffix,
		CheckCommands:     p.CheckCommands,
		NegControlMarkers: p.BuildFailureMarkers,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func writeFile(root, name string, body []byte, force bool) (bool, error) {
	path := filepath.Join(root, name)
	if !force {
		if _, err := os.Stat(path); err == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	return true, nil
}

// mergeGitignore appends only the entries that are missing, so running init
// twice does not double the block and an existing .gitignore keeps its own.
func mergeGitignore(root string) (bool, error) {
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}

	var missing []string
	for _, line := range gitignoreLines {
		if !have[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}

	var buf bytes.Buffer
	if len(existing) > 0 {
		buf.Write(existing)
		if !bytes.HasSuffix(existing, []byte("\n")) {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	if !have[gitignoreHeader] {
		buf.WriteString(gitignoreHeader + "\n")
		buf.WriteString("# config.json is checked in: it is project intent, not a working file.\n")
	}
	for _, line := range missing {
		buf.WriteString(line + "\n")
	}
	return true, os.WriteFile(path, buf.Bytes(), 0o644)
}

// mergeSettings adds the approve deny rule while preserving every other key,
// including permission rules crew knows nothing about.
func mergeSettings(root string) (bool, error) {
	path := filepath.Join(root, ".claude", "settings.json")
	doc := map[string]any{}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	case !os.IsNotExist(err):
		return false, err
	}

	perms := map[string]any{}
	if raw, ok := doc["permissions"]; ok {
		perms, ok = raw.(map[string]any)
		if !ok {
			return false, fmt.Errorf("%s: %q is %T, want an object", path, "permissions", raw)
		}
	}
	deny, err := toStrings(perms["deny"])
	if err != nil {
		return false, fmt.Errorf("%s: %q is %w", path, "deny", err)
	}
	if slices.Contains(deny, ApproveDenyRule) {
		return false, nil
	}
	perms["deny"] = append(deny, ApproveDenyRule)
	doc["permissions"] = perms

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(body, '\n'), 0o644)
}

// toStrings converts a JSON array field to a string slice. A missing field
// (nil) is not an error — the caller simply has none yet — but a present
// field that isn't an array of strings is a shape crew doesn't understand,
// and coercing it to nil would silently discard whatever the project had
// there.
func toStrings(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%T, want an array", v)
	}
	var out []string
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("contains a %T, want a string", it)
		}
		out = append(out, s)
	}
	return out, nil
}
