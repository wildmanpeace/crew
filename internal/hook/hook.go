// Package hook implements the PreToolUse gate.
//
// It exists because the permission layer alone cannot close everything. Phase
// 0 confirmed that Claude Code's built-in read-only command set (cat, ls,
// grep, echo, read-only git, and others) runs unprompted in every permission
// mode and is not configurable. The hook is the only layer that can refuse
// those, so it takes the strict position: the sole shell command a worker may
// run is its own role's dispatcher, in exactly the shape
//
//	<dispatcher> <verb> [args...]
//
// Anything else is denied. A deny from a PreToolUse hook beats an allowing
// entry in --allowedTools, which Phase 0 verified against the real CLI.
//
// The role is passed in by crew when it writes the settings file; it is never
// read from the worker's environment.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/wildmanpeace/crew/internal/config"
)

// Input is the subset of the PreToolUse payload this gate needs.
type Input struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// Decision is the gate's verdict.
type Decision struct {
	Denied bool
	Reason string
}

// Deny reports whether the call is refused.
func (d Decision) Deny() bool { return d.Denied }

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

type response struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// Response returns the JSON body to emit, or nil to defer. Deferring emits
// nothing at all, so the permission system still applies and deny-first
// precedence is preserved.
func (d Decision) Response() any {
	if !d.Denied {
		return nil
	}
	return response{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: d.Reason,
	}}
}

func deny(format string, a ...any) Decision {
	return Decision{Denied: true, Reason: fmt.Sprintf(format, a...)}
}

// shellOperators split a command into independently checked subcommands.
// These mirror the separators Claude Code's own matcher recognises.
var shellOperators = []string{"&&", "||", "|&", ";", "|", "&", "\n", "\r"}

// dangerousShellSyntax is refused outright: none of it has a legitimate use in
// a dispatcher invocation, and each could redirect or compose behaviour.
var dangerousShellSyntax = []string{"$(", "`", ">", "<", "${", "$((", "\\"}

// Evaluate reads a PreToolUse payload and returns the gate's decision.
func Evaluate(role config.Role, r io.Reader) (Decision, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Decision{}, fmt.Errorf("read hook input: %w", err)
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return Decision{}, fmt.Errorf("parse hook input: %w", err)
	}
	// Only Bash is gated here; other tools are governed by path rules.
	if in.ToolName != "Bash" {
		return Decision{}, nil
	}
	return evaluateCommand(role, in.ToolInput.Command), nil
}

func evaluateCommand(role config.Role, command string) Decision {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return deny("empty command")
	}
	for _, bad := range dangerousShellSyntax {
		if strings.Contains(cmd, bad) {
			return deny("command contains shell syntax %q; only a bare %s invocation is permitted",
				bad, dispatcherFor(role))
		}
	}
	// The permitted shape is a single command, not a chain of individually
	// permitted ones. Checking each subcommand and allowing the whole would
	// admit "crew-run test & crew-run lint", which is two concurrent
	// dispatchers, and more importantly admits any composition crew did not
	// intend.
	subs := splitSubcommands(cmd)
	if len(subs) != 1 {
		return deny("compound commands are not permitted; run a single %s invocation",
			dispatcherFor(role))
	}
	return checkSubcommand(role, subs[0])
}

func splitSubcommands(cmd string) []string {
	parts := []string{cmd}
	for _, op := range shellOperators {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, op)...)
		}
		parts = next
	}
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func checkSubcommand(role config.Role, sub string) Decision {
	fields := strings.Fields(sub)
	if len(fields) == 0 {
		return deny("empty subcommand")
	}
	prog := fields[0]

	// A leading assignment is refused rather than stripped: it could redirect
	// PATH or inject toolchain behaviour, and no legitimate dispatcher call
	// needs one.
	if strings.Contains(prog, "=") {
		return deny("leading environment assignment %q is not permitted", prog)
	}

	want := dispatcherFor(role)
	if prog != want {
		// Any path qualification would sidestep the PATH boundary that keeps
		// the other role's dispatcher unreachable.
		if strings.Contains(prog, "/") && strings.HasSuffix(prog, want) {
			return deny("%s must be invoked by bare name, not by path (%q)", want, prog)
		}
		return deny("only %s may be run by this worker; got %q", want, prog)
	}
	if len(fields) < 2 {
		return deny("%s requires a verb (one of: %s)", want, strings.Join(config.VerbsForRole(role), ", "))
	}
	verb := fields[1]
	if !config.RoleAllows(role, verb) {
		return deny("%s does not expose verb %q (allowed: %s)", want, verb, strings.Join(config.VerbsForRole(role), ", "))
	}
	return Decision{}
}

func dispatcherFor(role config.Role) string {
	if role == config.RoleVerifier {
		return "crew-check"
	}
	return "crew-run"
}

// Run reads a payload, writes any decision, and reports whether the call was
// denied. Emitting nothing on a defer keeps the permission system in charge.
func Run(role config.Role, in io.Reader, out io.Writer) (bool, error) {
	d, err := Evaluate(role, in)
	if err != nil {
		return false, err
	}
	resp := d.Response()
	if resp == nil {
		return false, nil
	}
	enc := json.NewEncoder(out)
	if err := enc.Encode(resp); err != nil {
		return true, fmt.Errorf("write hook decision: %w", err)
	}
	return true, nil
}
