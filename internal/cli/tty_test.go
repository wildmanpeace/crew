//go:build unix

package cli

import (
	"os"
	"testing"
)

// Guardrail: /dev/null is a character device, so the stat-mode heuristic this
// check used to be accepted it, and `crew approve <id> < /dev/null` satisfied
// a gate whose entire purpose is that an agent session cannot pass it. The
// mode bit is asserted first, so a regression back to a stat-based check fails
// here rather than in the field.
func TestIsTerminalRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is not a character device here; the bypass this guards does not apply", os.DevNull)
	}

	if isTerminal(f) {
		t.Fatalf("isTerminal(%s) = true, want false: the approval gate is bypassable with `< %s`",
			os.DevNull, os.DevNull)
	}
}

// A regular file is the other shape a redirected stdin takes.
func TestIsTerminalRejectsRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if isTerminal(f) {
		t.Fatal("isTerminal(regular file) = true, want false")
	}
}

// A closed file has no usable descriptor; the gate must fail closed rather
// than panic or report a terminal.
func TestIsTerminalRejectsClosedFile(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if isTerminal(f) {
		t.Fatal("isTerminal(closed file) = true, want false")
	}
}
