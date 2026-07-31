package watch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wildmanpeace/crew/internal/gitx"
)

// Authorship records where the test behind a mechanical criterion came from.
//
// It matters because a criterion's exit code is only evidence to the extent
// that the test was not written by the same worker whose implementation it is
// checking. A live run showed an implementer writing both the implementation
// and the test its criterion named, then a verifier running that test and
// recording a clean mechanical result. Nothing in that loop ever asked whether
// the test could fail.
type Authorship string

const (
	// AuthorPreExisting means the test was already on the main branch. The
	// implementer could not have shaped it to suit itself.
	AuthorPreExisting Authorship = "pre_existing"
	// AuthorBranch means the branch added or modified the test, so it needs a control.
	AuthorBranch Authorship = "branch_added"
	// AuthorUnknown means crew could not attribute the criterion to a file.
	AuthorUnknown Authorship = "unattributable"
)

// TestTarget is the file a mechanical criterion's command exercises.
type TestTarget struct {
	Authorship Authorship
	File       string
	// RunArgs are the command's own arguments, so the control runs exactly
	// the check the verifier ran rather than an approximation of it.
	RunArgs []string
}

// ChangedTestFiles lists test files the branch touched.
//
// Modified files count, not only added ones. A worker adding a test function
// to a file that already existed has authored that test just as surely as one
// creating a new file, and in practice that is the common shape: a package
// usually already has a test file to append to. Looking only at added files
// would leave the hole open in exactly the ordinary case.
func ChangedTestFiles(repo gitx.Repo, mergeBase, head, testSuffix string) ([]string, error) {
	changes, err := repo.ChangedFiles(mergeBase, head)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range changes {
		if c.Status == "D" || !strings.HasSuffix(c.Path, testSuffix) {
			continue
		}
		out = append(out, c.Path)
	}
	return out, nil
}

// CommandArgs strips a dispatcher invocation down to the arguments the check
// command actually receives.
//
// "crew-check test ./ratelimit/... -run TestX" yields
// ["./ratelimit/...", "-run", "TestX"].
func CommandArgs(command string) []string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == "test" && i > 0 && strings.HasPrefix(fields[i-1], "crew-") {
			return fields[i+1:]
		}
	}
	return nil
}

// RunFilter extracts the test-name filter from a command's arguments, in
// either "-run Name" or "-run=Name" form.
func RunFilter(args []string) string {
	for i, a := range args {
		if a == "-run" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "-run="); ok {
			return v
		}
	}
	return ""
}

// AttributeCriterion decides which file, if any, a mechanical criterion's
// command exercises, and whether the branch introduced it.
//
// Attribution is deliberately conservative: a name matching more than one
// added file is treated as unattributable rather than guessed at, because
// running the control against the wrong file would produce a confident and
// wrong answer.
func AttributeCriterion(worktree, command string, changedTests []string) TestTarget {
	args := CommandArgs(command)
	target := TestTarget{Authorship: AuthorPreExisting, RunArgs: args}

	if len(changedTests) == 0 {
		// Nothing new; whatever this ran already existed on main.
		return target
	}
	name := RunFilter(args)
	if name == "" {
		// Without a name filter the command may span many tests, so no single
		// file can be held responsible.
		return TestTarget{Authorship: AuthorUnknown, RunArgs: args}
	}

	var matches []string
	for _, f := range changedTests {
		if fileDeclaresTest(filepath.Join(worktree, f), name) {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 0:
		// The named test is not in anything the branch added, so it predates
		// this work.
		return target
	case 1:
		return TestTarget{Authorship: AuthorBranch, File: matches[0], RunArgs: args}
	default:
		return TestTarget{Authorship: AuthorUnknown, RunArgs: args}
	}
}

// fileDeclaresTest reports whether a file declares the named test.
func fileDeclaresTest(path, name string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	body := string(raw)
	// Match a declaration rather than any mention, so a file that merely
	// references the name is not blamed for it.
	for _, form := range []string{"func " + name + "(", "func " + name + " ("} {
		if strings.Contains(body, form) {
			return true
		}
	}
	return false
}
