package cli

import (
	"flag"
	"reflect"
	"testing"
)

// Every crew command reads as "<verb> <task-id> --flag", which is exactly the
// ordering Go's flag package stops parsing at. These tests pin both orderings.

func TestFlagAfterPositionalIsParsed(t *testing.T) {
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	rm := fs.Bool("remove-worktree", false, "")

	pos, err := ParseArgs(fs, []string{"my-task", "--remove-worktree"})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if !*rm {
		t.Fatal("--remove-worktree after the task id was silently ignored")
	}
	if !reflect.DeepEqual(pos, []string{"my-task"}) {
		t.Fatalf("positionals = %q", pos)
	}
}

func TestFlagBeforePositionalStillWorks(t *testing.T) {
	fs := flag.NewFlagSet("teardown", flag.ContinueOnError)
	rm := fs.Bool("remove-worktree", false, "")

	pos, err := ParseArgs(fs, []string{"--remove-worktree", "my-task"})
	if err != nil {
		t.Fatal(err)
	}
	if !*rm || !reflect.DeepEqual(pos, []string{"my-task"}) {
		t.Fatalf("rm = %v, positionals = %q", *rm, pos)
	}
}

// A value flag must carry its value with it when permuted, or the value
// becomes a positional and the flag silently takes the next thing along.
func TestValueFlagAfterPositionalKeepsItsValue(t *testing.T) {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	head := fs.String("head", "", "")

	pos, err := ParseArgs(fs, []string{"my-task", "--head", "abc123def456"})
	if err != nil {
		t.Fatal(err)
	}
	if *head != "abc123def456" {
		t.Fatalf("--head = %q, want the sha", *head)
	}
	if !reflect.DeepEqual(pos, []string{"my-task"}) {
		t.Fatalf("positionals = %q; the sha leaked into them", pos)
	}
}

func TestInlineValueForm(t *testing.T) {
	fs := flag.NewFlagSet("peek", flag.ContinueOnError)
	lines := fs.Int("lines", 200, "")

	pos, err := ParseArgs(fs, []string{"my-task", "--lines=50"})
	if err != nil {
		t.Fatal(err)
	}
	if *lines != 50 {
		t.Fatalf("--lines = %d", *lines)
	}
	if !reflect.DeepEqual(pos, []string{"my-task"}) {
		t.Fatalf("positionals = %q", pos)
	}
}

func TestSingleDashFormIsAccepted(t *testing.T) {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	if _, err := ParseArgs(fs, []string{"my-task", "-force"}); err != nil {
		t.Fatal(err)
	}
	if !*force {
		t.Fatal("-force was ignored")
	}
}

// An unknown flag must be an error rather than a silently swallowed
// positional, or a typo reads as success.
func TestUnknownFlagIsAnError(t *testing.T) {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	fs.Bool("force", false, "")
	if _, err := ParseArgs(fs, []string{"my-task", "--frce"}); err == nil {
		t.Fatal("a misspelled flag was accepted")
	}
}

func TestMissingValueIsAnError(t *testing.T) {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.String("head", "", "")
	if _, err := ParseArgs(fs, []string{"my-task", "--head"}); err == nil {
		t.Fatal("a value flag with no value was accepted")
	}
}

func TestDoubleDashEndsFlagParsing(t *testing.T) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.Bool("force", false, "")

	pos, err := ParseArgs(fs, []string{"my-task", "--", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"my-task", "--not-a-flag"}) {
		t.Fatalf("positionals = %q", pos)
	}
}

func TestNoFlagsAtAll(t *testing.T) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	pos, err := ParseArgs(fs, []string{"my-task"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"my-task"}) {
		t.Fatalf("positionals = %q", pos)
	}
}
