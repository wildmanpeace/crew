package cli

import (
	"flag"
	"fmt"
	"strings"
)

// ParseArgs parses flags that may appear after positional arguments and
// returns the positionals.
//
// Go's flag package stops at the first non-flag argument, so
// "crew teardown my-task --remove-worktree" leaves the flag unparsed and
// silently false. Every crew command reads as "<verb> <task-id> --flag", which
// is the shape that breaks, and the failure is quiet: --force appears to be
// ignored, --remove-worktree appears not to work, and --head looks missing.
//
// This permutes flags ahead of positionals before parsing, so both orderings
// behave the same.
func ParseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	flagArgs, positionals, err := partitionArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	// Anything Parse left over is a positional too.
	return append(positionals, fs.Args()...), nil
}

// partitionArgs splits args into flags (with their values) and positionals.
func partitionArgs(fs *flag.FlagSet, args []string) (flagArgs, positionals []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]

		// "--" ends flag parsing; everything after it is positional.
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			return flagArgs, positionals, nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}

		name := strings.TrimLeft(a, "-")
		// An inline value needs no lookahead.
		if strings.Contains(name, "=") {
			flagArgs = append(flagArgs, a)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			return nil, nil, fmt.Errorf("unknown flag %q", a)
		}
		flagArgs = append(flagArgs, a)

		// A non-boolean flag takes the next argument as its value, so that
		// argument must travel with it rather than becoming a positional.
		if !isBoolFlag(f) {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %q needs a value", a)
			}
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positionals, nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
