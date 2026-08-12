package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/gitx"
)

func initRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := gitx.New(root)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "crew@test"},
		{"config", "user.name", "crew"},
	} {
		if _, err := repo.Run(args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n"), 0o644)
	if _, err := repo.Run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Run("commit", "-qm", "initial"); err != nil {
		t.Fatal(err)
	}
	return root
}

// withDispatchers puts crew-run and crew-check on the real search path via
// CREW_LIBEXEC, so the preflight exercises the lookup it uses in production
// rather than a stub of it.
func withDispatchers(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"crew-run", "crew-check"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CREW_LIBEXEC", dir)
}

func found(string) (string, error)     { return "/usr/local/bin/stub", nil }
func missing(n string) (string, error) { return "", errors.New(n + " not found") }
func missingClaude(n string) (string, error) {
	if n == "claude" {
		return "", errors.New("claude not found")
	}
	return found(n)
}

// Init has to leave a directory crew can actually drive, detecting the
// language rather than being told.
func TestInitScaffoldsADetectedProject(t *testing.T) {
	withDispatchers(t)
	root := initRepo(t)
	var out bytes.Buffer

	if err := Init(root, &out, InitOptions{LookPath: found}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	if cfg.TestFileSuffix != "_test.go" {
		t.Errorf("TestFileSuffix = %q, want the Go suffix", cfg.TestFileSuffix)
	}
	if body := out.String(); !strings.Contains(body, "AGENTS.md") {
		t.Errorf("the report does not say what was written:\n%s", body)
	}
}

// --lang is the escape hatch for a directory detection refuses to guess at.
func TestInitAcceptsAnExplicitLanguage(t *testing.T) {
	withDispatchers(t)
	root := initRepo(t)
	os.Remove(filepath.Join(root, "go.mod"))

	if err := Init(root, &bytes.Buffer{}, InitOptions{Lang: "typescript", LookPath: found}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TestFileSuffix != ".test.ts" {
		t.Errorf("TestFileSuffix = %q, want the TypeScript suffix", cfg.TestFileSuffix)
	}
}

func TestInitRejectsAnUnknownLanguage(t *testing.T) {
	withDispatchers(t)
	err := Init(initRepo(t), &bytes.Buffer{}, InitOptions{Lang: "cobol", LookPath: found})
	if err == nil {
		t.Fatal("Init accepted an unknown language")
	}
	if !strings.Contains(err.Error(), "cobol") {
		t.Errorf("error %q does not name the language", err)
	}
}

// A setup that is written but cannot run is the failure init exists to
// prevent, so a missing dependency has to be said out loud and exit non-zero.
func TestInitReportsAnEnvironmentThatCannotRunTheLoop(t *testing.T) {
	root := initRepo(t)
	var out bytes.Buffer

	err := Init(root, &out, InitOptions{LookPath: missingClaude})
	if err == nil {
		t.Fatal("Init succeeded with claude missing")
	}
	if !strings.Contains(out.String(), "claude") {
		t.Errorf("the report does not mention claude:\n%s", out.String())
	}
	// The files are still written: the environment is fixable, and rerunning
	// init to get them would be a worse experience than being told.
	if _, statErr := os.Stat(filepath.Join(root, ".crew", "config.json")); statErr != nil {
		t.Errorf("config was not written despite a fixable environment: %v", statErr)
	}
}

// Preflight is the part that catches the mistakes documentation cannot.
func TestPreflightNamesEveryMissingDependency(t *testing.T) {
	checks := Preflight(initRepo(t), missing)

	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"claude", "tmux", "dispatchers"} {
		c, ok := byName[name]
		if !ok {
			t.Errorf("no check named %q", name)
			continue
		}
		if c.OK {
			t.Errorf("check %q passed with nothing installed", name)
		}
	}
	if c := byName["git repository"]; !c.OK {
		t.Error("a real git repository was not recognised")
	}
}

// A repository with no commits yet has no main branch. That is a normal place
// to run init, so it must not be treated as a failure.
func TestPreflightDoesNotFailAFreshRepository(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.New(root).Run("init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}

	for _, c := range Preflight(root, found) {
		if c.Name == "main branch" && c.Fatal {
			t.Error("an unborn main branch is fatal; a fresh repository is a normal starting point")
		}
	}
}
