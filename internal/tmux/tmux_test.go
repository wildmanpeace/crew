package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("tmux is not installed")
	}
}

// session gives each test its own tmux session and tears it down afterwards.
func session(t *testing.T) string {
	t.Helper()
	requireTmux(t)
	name := fmt.Sprintf("crewtest-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := EnsureSession(name); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	t.Cleanup(func() { KillSession(name) })
	return name
}

// waitFor polls until cond holds, so tests do not race the tmux server.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEnsureSessionIsIdempotent(t *testing.T) {
	s := session(t)
	if err := EnsureSession(s); err != nil {
		t.Fatalf("second EnsureSession: %v", err)
	}
}

func TestWindowLifecycle(t *testing.T) {
	s := session(t)
	if err := NewWindow(s, "w1", "", []string{"sleep", "30"}, nil); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	waitFor(t, "window to appear", func() bool {
		ok, _ := WindowExists(s, "w1")
		return ok
	})
	if err := KillWindow(s, "w1"); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	waitFor(t, "window to disappear", func() bool {
		ok, _ := WindowExists(s, "w1")
		return !ok
	})
}

func TestKillWindowOnMissingWindowIsNotAnError(t *testing.T) {
	s := session(t)
	if err := KillWindow(s, "ghost"); err != nil {
		t.Fatalf("KillWindow on a missing window: %v", err)
	}
}

func TestWindowExistsOnMissingSessionIsNotAnError(t *testing.T) {
	requireTmux(t)
	ok, err := WindowExists("crewtest-definitely-absent", "w")
	if err != nil {
		t.Fatalf("WindowExists: %v", err)
	}
	if ok {
		t.Fatal("reported a window in a nonexistent session")
	}
}

// The command must reach the process untouched. If tmux routed it through a
// shell, spaces and metacharacters in an argument would be re-split or
// interpreted, and a commit message would be corrupted or executed.
func TestArgumentsAreNotShellMangled(t *testing.T) {
	s := session(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "argv.txt")
	canary := filepath.Join(dir, "PWNED")

	tricky := `hello world; touch ` + canary + ` && echo $HOME`
	// sh -c here is the *test's* own writer, receiving tricky as one argv
	// element; the question is whether tmux keeps it as one element.
	argv := []string{"/bin/sh", "-c", `printf '%s' "$1" > ` + out, "sh", tricky}

	if err := NewWindow(s, "w2", dir, argv, nil); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	waitFor(t, "argv file", func() bool {
		_, err := os.Stat(out)
		return err == nil
	})
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != tricky {
		t.Fatalf("argument was mangled:\n got: %q\nwant: %q", got, tricky)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("tmux interpreted the argument through a shell: canary was created")
	}
}

func TestNewWindowRejectsEmptyCommand(t *testing.T) {
	s := session(t)
	if err := NewWindow(s, "w3", "", nil, nil); err == nil {
		t.Fatal("empty command accepted")
	}
}

func TestEnvReachesTheProcess(t *testing.T) {
	s := session(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	argv := []string{"/bin/sh", "-c", `printf '%s' "$CREW_TASK_ID" > ` + out}

	if err := NewWindow(s, "w4", dir, argv, map[string]string{"CREW_TASK_ID": "alpha"}); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	waitFor(t, "env file", func() bool {
		_, err := os.Stat(out)
		return err == nil
	})
	got, _ := os.ReadFile(out)
	if string(got) != "alpha" {
		t.Fatalf("CREW_TASK_ID = %q, want alpha", got)
	}
}

func TestCapturePaneReturnsOutput(t *testing.T) {
	s := session(t)
	argv := []string{"/bin/sh", "-c", "echo crew-marker-9f3a; sleep 30"}
	if err := NewWindow(s, "w5", "", argv, nil); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	var got string
	waitFor(t, "pane output", func() bool {
		got, _ = CapturePane(s, "w5", 50)
		return strings.Contains(got, "crew-marker-9f3a")
	})
}

func TestListWindows(t *testing.T) {
	s := session(t)
	if err := NewWindow(s, "alpha-w", "", []string{"sleep", "30"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "window listed", func() bool {
		names, _ := ListWindows(s)
		for _, n := range names {
			if n == "alpha-w" {
				return true
			}
		}
		return false
	})
}

func TestWindowNameIsPrefixed(t *testing.T) {
	if got := WindowName("alpha-a1-c1-impl"); got != "crew-alpha-a1-c1-impl" {
		t.Fatalf("WindowName = %q", got)
	}
}
