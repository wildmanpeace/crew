package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
)

// If crew's binaries were ever installed into a directory already on PATH, a
// verifier would inherit crew-run and the role boundary would silently
// dissolve. WorkerPath must drop any such directory.
func TestWorkerPathDropsAnyDirectoryHoldingADispatcher(t *testing.T) {
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "crew-run"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	clean := t.TempDir()

	got := WorkerPath("/proj", config.RoleVerifier, shared+":"+clean)
	parts := strings.Split(got, ":")

	if parts[0] != RoleBinDir("/proj", config.RoleVerifier) {
		t.Fatalf("PATH[0] = %q, want the verifier bin dir", parts[0])
	}
	for _, p := range parts {
		if p == shared {
			t.Fatalf("PATH retained %q, which contains crew-run", shared)
		}
	}
	found := false
	for _, p := range parts {
		if p == clean {
			found = true
		}
	}
	if !found {
		t.Errorf("PATH dropped an unrelated toolchain directory: %q", got)
	}
}

// A worker is the claude CLI: without HOME it cannot reach its own login and
// fails with "Not logged in" before doing any work. This was only visible in
// a live run, so it is pinned here.
func TestEnvPassesThroughHomeAndCredentials(t *testing.T) {
	t.Setenv("HOME", "/home/captain")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	got := Env(Spec{
		Role: config.RoleImplementer, TaskID: "alpha",
		Worktree: "/proj/.crew/worktrees/alpha/attempt-1", ProjectRoot: "/proj",
	}, "/usr/bin")

	index := map[string]string{}
	for _, kv := range got {
		if k, v, ok := strings.Cut(kv, "="); ok {
			index[k] = v
		}
	}
	if index["HOME"] != "/home/captain" {
		t.Errorf("HOME = %q; the worker cannot authenticate without it", index["HOME"])
	}
	if index["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("ANTHROPIC_API_KEY was not passed through")
	}
	// PATH is still crew's, not the ambient one.
	if !strings.HasPrefix(index["PATH"], RoleBinDir("/proj", config.RoleImplementer)) {
		t.Errorf("PATH = %q, want it to lead with the role bin dir", index["PATH"])
	}
	for _, k := range []string{"CREW_WORKTREE", "CREW_PROJECT_ROOT", "CREW_TASK_ID"} {
		if index[k] == "" {
			t.Errorf("%s missing", k)
		}
	}
}
