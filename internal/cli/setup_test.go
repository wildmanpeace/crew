package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// A worker's context must be its brief and the code, nothing else. The
// project's own memory files live inside every worktree, so excluding them is
// what keeps a first-mate role document — or any other instruction written for
// an interactive session — from reaching a one-shot worker.
func TestWorkerClaudeMdExcludesCoversWorktreesAndTheCaptainsGlobal(t *testing.T) {
	got := WorkerClaudeMdExcludes("/proj", "/home/u")

	for _, want := range []string{
		"/home/u/.claude/CLAUDE.md",
		"/proj/.crew/worktrees/**",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("excludes %v, missing %q", got, want)
		}
	}
}

// The home directory is not always resolvable, and a worker that inherits the
// captain's conventions is a smaller problem than one that inherits the whole
// role document, so the worktree pattern must not depend on finding it.
func TestWorkerClaudeMdExcludesWithoutAHomeDirectory(t *testing.T) {
	got := WorkerClaudeMdExcludes("/proj", "")

	if !slices.Equal(got, []string{"/proj/.crew/worktrees/**"}) {
		t.Errorf("excludes = %v, want only the worktree pattern", got)
	}
}

// crew doctor --notify exists so a captain who isn't watching the terminal
// still finds out; a notifier that goes silent on an unsupported OS defeats
// that, so darwin and linux each need their own delivery command.
func TestNotifyCommandOnDarwinUsesOsascript(t *testing.T) {
	argv := notifyCommand("darwin", "crew doctor", "1 problem found")
	if len(argv) == 0 || argv[0] != "osascript" {
		t.Fatalf("notifyCommand(darwin) = %v, want an osascript invocation", argv)
	}
}

func TestNotifyCommandOnLinuxUsesNotifySend(t *testing.T) {
	argv := notifyCommand("linux", "crew doctor", "1 problem found")
	if !slices.Equal(argv, []string{"notify-send", "crew doctor", "1 problem found"}) {
		t.Errorf("notifyCommand(linux) = %v, want notify-send crew doctor \"1 problem found\"", argv)
	}
}

func TestNotifyCommandOnUnknownOSHasNoDelivery(t *testing.T) {
	if argv := notifyCommand("plan9", "t", "b"); argv != nil {
		t.Errorf("notifyCommand(plan9) = %v, want nil", argv)
	}
}

// An OS with no known notifier must not let the notification vanish: it has
// to land somewhere the captain can still find it.
func TestDeliverNotificationFallsBackWhenNoCommandIsKnown(t *testing.T) {
	var fallback bytes.Buffer
	ran := false
	deliverNotification(nil, "crew doctor", "1 problem found",
		func([]string) error { ran = true; return nil }, &fallback)

	if ran {
		t.Fatal("deliverNotification ran a command when none was given")
	}
	body := fallback.String()
	if !strings.Contains(body, "crew doctor") || !strings.Contains(body, "1 problem found") {
		t.Errorf("fallback = %q, want the title and body", body)
	}
}

// The notifier itself can fail (missing binary, no display server, ...); that
// must fall back the same way a missing command does, not get swallowed.
func TestDeliverNotificationFallsBackWhenTheCommandFails(t *testing.T) {
	var fallback bytes.Buffer
	deliverNotification([]string{"notify-send", "t", "b"}, "crew doctor", "1 problem found",
		func([]string) error { return errors.New("exec: \"notify-send\": executable file not found in $PATH") },
		&fallback)

	if !strings.Contains(fallback.String(), "crew doctor") {
		t.Errorf("fallback = %q, want the notification written to it", fallback.String())
	}
}

// A working notifier must not also spam the fallback.
func TestDeliverNotificationSkipsFallbackOnSuccess(t *testing.T) {
	var fallback bytes.Buffer
	deliverNotification([]string{"notify-send", "t", "b"}, "crew doctor", "1 problem found",
		func([]string) error { return nil }, &fallback)

	if fallback.Len() != 0 {
		t.Errorf("fallback = %q, want nothing written on success", fallback.String())
	}
}
