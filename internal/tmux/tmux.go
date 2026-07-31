// Package tmux hosts worker windows.
//
// tmux is used purely for visibility: it lets the captain watch or attach to a
// running worker. It is never the completion signal. Liveness comes from
// process exit and the run's exit marker, so a killed or missing window
// degrades observability rather than correctness.
package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Session is the tmux session crew keeps its worker windows in.
const Session = "crew"

func run(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), fmt.Errorf("tmux %s: %s", strings.Join(args, " "),
				strings.TrimSpace(errb.String()))
		}
		return "", fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// Available reports whether tmux is usable on this machine.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// EnsureSession creates the session if it does not already exist.
func EnsureSession(session string) error {
	if _, err := run("has-session", "-t", "="+session); err == nil {
		return nil
	}
	_, err := run("new-session", "-d", "-s", session)
	return err
}

// NewWindow starts argv in a new detached window.
//
// argv is passed as separate arguments so tmux execs it directly; nothing is
// handed to a shell, which keeps arguments containing spaces or shell
// metacharacters intact.
func NewWindow(session, window, dir string, argv []string, env map[string]string) error {
	if len(argv) == 0 {
		return fmt.Errorf("tmux: empty command for window %q", window)
	}
	args := []string{"new-window", "-d", "-t", session + ":", "-n", window}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, "--")
	args = append(args, argv...)
	_, err := run(args...)
	return err
}

// WindowExists reports whether a window is currently alive. crew uses this to
// reconcile recorded state against ground truth, never to decide that a
// worker finished.
func WindowExists(session, window string) (bool, error) {
	out, err := run("list-windows", "-t", "="+session, "-F", "#{window_name}")
	if err != nil {
		// A missing session means no windows, which is a valid answer.
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "session not found") ||
			strings.Contains(err.Error(), "can't find session") {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == window {
			return true, nil
		}
	}
	return false, nil
}

// ListWindows returns the live window names in a session.
func ListWindows(session string) ([]string, error) {
	out, err := run("list-windows", "-t", "="+session, "-F", "#{window_name}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "session not found") ||
			strings.Contains(err.Error(), "can't find session") {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// CapturePane returns the tail of a window's pane, for watching a worker
// without interacting with it.
func CapturePane(session, window string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	return run("capture-pane", "-p", "-t", session+":"+window,
		"-S", "-"+strconv.Itoa(lines))
}

// KillWindow removes a window if present.
func KillWindow(session, window string) error {
	exists, err := WindowExists(session, window)
	if err != nil || !exists {
		return err
	}
	_, err = run("kill-window", "-t", session+":"+window)
	return err
}

// KillSession removes the whole session.
func KillSession(session string) error {
	if ok, _ := WindowExists(session, ""); !ok {
		if _, err := run("has-session", "-t", "="+session); err != nil {
			return nil
		}
	}
	_, err := run("kill-session", "-t", "="+session)
	return err
}

// WindowName builds the window name for a worker run.
func WindowName(runID string) string { return "crew-" + runID }

// SendKeys types text into a window's pane, followed by Enter. It exists for
// the debug-only crew send escape hatch.
func SendKeys(session, window, text string) error {
	_, err := run("send-keys", "-t", session+":"+window, text, "Enter")
	return err
}
