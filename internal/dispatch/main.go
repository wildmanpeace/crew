package dispatch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/wildmanpeace/crew/internal/config"
)

// execRun runs argv directly in dir. There is no shell anywhere in this path:
// argv[0] is resolved against PATH by the OS and the remaining elements are
// passed through untouched.
func execRun(argv []string, dir string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Hand the child a deliberate environment rather than the worker's.
	cmd.Env = childEnv()

	err := cmd.Run()
	if err == nil {
		return ExitOK, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// The command ran and failed. That is a result, not a dispatcher
		// error: crew reads the exit code as the check's verdict.
		return ee.ExitCode(), nil
	}
	return ExitPolicy, fmt.Errorf("exec %s: %w", argv[0], err)
}

// childEnv passes through only what a build or test toolchain legitimately
// needs, so a worker cannot smuggle behaviour into the child through the
// environment.
func childEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TERM",
		"GOPATH", "GOMODCACHE", "GOCACHE", "GOROOT", "GOFLAGS", "GOPROXY"}
	var out []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Main is the entry point shared by both dispatcher binaries. The role is
// supplied by the binary at compile time and is never read from the
// environment, which the worker controls.
func Main(role config.Role) {
	env := EnvFrom(os.Getenv)

	cfg, err := config.Load(env.ProjectRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binaryFor(role), err)
		os.Exit(ExitPolicy)
	}

	code, err := Dispatch(role, os.Args[1:], env, cfg, execRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", binaryFor(role), err)
	}
	os.Exit(code)
}
