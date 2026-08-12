package cli

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/wildmanpeace/crew/internal/gitx"
	"github.com/wildmanpeace/crew/internal/scaffold"
)

// InitOptions are crew init's flags, plus the lookup it uses to find the
// binaries a worker needs, injected so the preflight can be tested on a
// machine where they happen to be installed.
type InitOptions struct {
	Lang     string
	Force    bool
	LookPath func(string) (string, error)
}

func (o InitOptions) lookPath() func(string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath
	}
	return exec.LookPath
}

// Check is one preflight finding. Fatal marks the ones that stop crew watch
// from working at all, as opposed to the ones that are merely worth knowing.
type Check struct {
	Name   string
	OK     bool
	Fatal  bool
	Detail string
}

// Init generates a project's crew setup and reports whether this machine can
// run it.
//
// The files are written even when the preflight fails. A missing tmux is
// fixable in a minute, and withholding the scaffold until it is installed
// would mean running init twice for no reason. The error is what makes the
// failure impossible to miss.
func Init(root string, out io.Writer, opts InitOptions) error {
	profile, err := resolveProfile(root, opts.Lang)
	if err != nil {
		return err
	}

	res, err := scaffold.Write(root, profile, opts.Force)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "crew init: %s project in %s\n\n", profile.Language, root)
	for _, name := range res.Written {
		fmt.Fprintf(out, "  wrote    %s\n", name)
	}
	for _, name := range res.Skipped {
		fmt.Fprintf(out, "  kept     %s (already present)\n", name)
	}

	fmt.Fprintf(out, "\n")
	checks := Preflight(root, opts.lookPath())
	var blocking []string
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			if c.Fatal {
				blocking = append(blocking, c.Name)
			}
		}
		fmt.Fprintf(out, "  [%s] %-16s %s\n", mark, c.Name, c.Detail)
	}

	if len(blocking) > 0 {
		fmt.Fprintf(out, "\nThe setup is written, but crew watch cannot run yet.\n")
		return fmt.Errorf("unmet requirements: %s", strings.Join(blocking, ", "))
	}

	fmt.Fprintf(out, `
Next:
  1. Describe this project in CLAUDE.md, and check the check_commands in
     .crew/config.json actually run here.
  2. crew watch          (leave it running)
  3. claude              (your first mate, in another terminal)
`)
	return nil
}

func resolveProfile(root, lang string) (scaffold.Profile, error) {
	if lang == "" {
		return scaffold.Detect(root)
	}
	return scaffold.ProfileFor(scaffold.Language(lang))
}

// Preflight reports whether crew watch could actually run in this project.
//
// Every one of these fails later and less clearly: a missing claude surfaces
// as a worker that never starts, missing dispatchers as a worker that cannot
// run a single check, and a missing tmux as no window to spawn into.
func Preflight(root string, lookPath func(string) (string, error)) []Check {
	repo := gitx.New(root)

	checks := []Check{gitCheck(repo, root)}
	checks = append(checks, mainBranchCheck(repo))

	for _, bin := range []string{"claude", "tmux"} {
		c := Check{Name: bin, Fatal: true}
		if path, err := lookPath(bin); err == nil {
			c.OK, c.Detail = true, path
		} else {
			c.Detail = "not on PATH; crew watch needs it to run workers"
			if bin == "tmux" {
				c.Detail = "not on PATH; worker windows live in tmux"
			}
		}
		checks = append(checks, c)
	}

	return append(checks, dispatcherCheck())
}

func gitCheck(repo gitx.Repo, root string) Check {
	c := Check{Name: "git repository", Fatal: true}
	if _, err := repo.Run("rev-parse", "--git-dir"); err != nil {
		c.Detail = root + " is not a git repository; crew drives one from the outside"
		return c
	}
	c.OK, c.Detail = true, root
	return c
}

// mainBranchCheck is deliberately not fatal. A repository with no commits has
// no branch yet, and that is a perfectly normal moment to run init.
func mainBranchCheck(repo gitx.Repo) Check {
	c := Check{Name: "main branch"}
	if ok, err := repo.BranchExists("main"); err == nil && ok {
		c.OK, c.Detail = true, "main"
		return c
	}
	c.Detail = "no main branch yet; create one, or set main_branch in .crew/config.json"
	return c
}

func dispatcherCheck() Check {
	c := Check{Name: "dispatchers", Fatal: true}
	dirs := DispatcherSearchPath()
	var missing []string
	for _, name := range []string{"crew-run", "crew-check"} {
		if _, err := findDispatcher(dirs, name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		c.Detail = fmt.Sprintf("%s not found in %s", strings.Join(missing, " and "),
			strings.Join(dirs, ", "))
		return c
	}
	c.OK, c.Detail = true, "crew-run and crew-check found"
	return c
}
