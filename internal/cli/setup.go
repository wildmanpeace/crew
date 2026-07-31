package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/tmux"
	"github.com/wildmanpeace/crew/internal/worker"
)

// Notify delivers a best-effort desktop notification.
//
// The durable record is always .crew/events.jsonl; this is only the nudge, so
// a missing or failing notifier is never an error.
func Notify(title, body string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", body, title)
	_ = exec.Command("osascript", "-e", script).Run()
}

// WriteWorkerSettings writes the per-role settings each worker is launched
// with.
//
// They live in .crew, outside every worktree, so no worker holds a permission
// rule that would let it edit the guardrails it runs under.
func (a *App) WriteWorkerSettings() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var excludes []string
	if home, err := os.UserHomeDir(); err == nil {
		// Keep the captain's personal conventions out of worker context: they
		// describe how the captain works, not this project.
		excludes = append(excludes, filepath.Join(home, ".claude", "CLAUDE.md"))
	}
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		body, err := worker.Settings(self, role, excludes)
		if err != nil {
			return err
		}
		path := filepath.Join(a.Root, ".crew", string(role)+"-settings.json")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// DispatcherSearchPath lists where the dispatcher binaries are looked for.
//
// A libexec directory is preferred over the directory holding crew itself.
// crew belongs on the captain's PATH; the dispatchers must not be, because the
// role boundary is that a verifier has no crew-run reachable anywhere. Keeping
// them out of any PATH directory keeps that true by construction rather than
// by filtering after the fact.
func DispatcherSearchPath() []string {
	var dirs []string
	if v := os.Getenv("CREW_LIBEXEC"); v != "" {
		dirs = append(dirs, v)
	}
	if self, err := os.Executable(); err == nil {
		binDir := filepath.Dir(self)
		dirs = append(dirs,
			filepath.Join(filepath.Dir(binDir), "libexec", "crew"),
			binDir, // development convenience: built alongside crew
		)
	}
	return dirs
}

// InstallDispatchers copies each role's dispatcher into its own directory
// under .crew/bin, which is the only place a worker can reach one.
func (a *App) InstallDispatchers() error {
	dirs := DispatcherSearchPath()
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		name := worker.DispatcherName(role)
		src, err := findDispatcher(dirs, name)
		if err != nil {
			return err
		}
		dstDir := worker.RoleBinDir(a.Root, role)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}
		if err := copyExecutable(src, filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func findDispatcher(dirs []string, name string) (string, error) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in any of %v; build it with:\n    go build -o <dir>/%s ./cmd/%s",
		name, dirs, name, name)
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Send is a debug-only escape hatch. A one-shot worker under an automatic
// permission mode has nothing to answer in the normal flow, so this exists for
// diagnosis rather than for the loop.
func (a *App) Send(taskID, text string) error {
	ts, err := a.taskState(taskID)
	if err != nil {
		return err
	}
	if ts.Window == "" {
		return fmt.Errorf("task %q has no live window", taskID)
	}
	return tmux.SendKeys(a.Session, ts.Window, text)
}

// InstallDispatchersFrom installs the dispatchers from an explicit directory.
// Tests use it so they exercise the binaries they just built rather than
// whatever happens to be installed on the machine.
func (a *App) InstallDispatchersFrom(srcDir string) error {
	for _, role := range []config.Role{config.RoleImplementer, config.RoleVerifier} {
		name := worker.DispatcherName(role)
		src := filepath.Join(srcDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("%s not found in %s: %w", name, srcDir, err)
		}
		dstDir := worker.RoleBinDir(a.Root, role)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}
		if err := copyExecutable(src, filepath.Join(dstDir, name)); err != nil {
			return err
		}
	}
	return nil
}
