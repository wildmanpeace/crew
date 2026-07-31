package cli

import (
	"os"
)

// isTerminal reports whether f is an interactive terminal.
//
// crew approve depends on this: a first mate's session is not a terminal, so
// the check is what makes approval structurally captain-only rather than a
// convention anyone could follow or ignore.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
