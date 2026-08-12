package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// git's name-status output is parsed field by field, and a line whose status
// field is empty must be skipped rather than indexed into.
func TestParseNameStatusSkipsAMalformedLine(t *testing.T) {
	changes := parseNameStatus("A\tadded.go\n\tno-status.go\nM\tmodified.go\n")

	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the two well-formed entries", changes)
	}
	if !changes[0].Added() || changes[0].Path != "added.go" {
		t.Errorf("changes[0] = %+v", changes[0])
	}
	if changes[1].Status != "M" || changes[1].Path != "modified.go" {
		t.Errorf("changes[1] = %+v", changes[1])
	}
}

// A refusal is only actionable if it can name what is dirty.
func TestDirtyPathsNamesTrackedAndUntrackedChanges(t *testing.T) {
	r := New(tempRepo(t))
	write(t, filepath.Join(r.Dir, "tracked.txt"), "edited\n")
	write(t, filepath.Join(r.Dir, "untracked.txt"), "new\n")

	paths, err := r.DirtyPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want both changes", paths)
	}
	for _, want := range []string{"tracked.txt", "untracked.txt"} {
		if !contains(paths, want) {
			t.Errorf("paths = %v, missing %s", paths, want)
		}
	}
	if clean, err := r.IsClean(); err != nil || clean {
		t.Errorf("IsClean = %v, %v; want false", clean, err)
	}
}

func TestCurrentBranchReportsTheCheckedOutBranch(t *testing.T) {
	r := New(tempRepo(t))
	got, err := r.CurrentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("CurrentBranch = %q, want main", got)
	}
}

// tempRepo builds a git repo with one commit on main.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := New(dir)
	mustGit(t, r, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "tracked.txt"), "base\n")
	mustGit(t, r, "add", "-A")
	mustGit(t, r, "commit", "-qm", "base")
	return dir
}

func mustGit(t *testing.T, r Repo, args ...string) {
	t.Helper()
	if _, err := r.Run(args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
