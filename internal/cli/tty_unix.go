//go:build unix

package cli

import (
	"os"

	"golang.org/x/term"
)

// isTerminal reports whether f is an interactive terminal.
//
// crew approve depends on this: a first mate's session is not a terminal, so
// the check is what makes approval structurally captain-only rather than a
// convention anyone could follow or ignore.
//
// The question has to be asked of the kernel, not of the file mode. /dev/null
// is itself a character device, so a mode test accepts it and the whole gate
// reduces to typing `< /dev/null`. term.IsTerminal issues the terminal-
// attributes ioctl, which no redirection can satisfy.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
