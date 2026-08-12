//go:build !unix

package cli

import "os"

// isTerminal fails closed off unix.
//
// crew's worker plumbing is unix-only in practice — tmux windows, a shell
// dispatcher, POSIX file locking — so this build exists to keep the package
// compiling, not to be run. Approval is a security gate, and a gate that
// cannot verify its precondition must refuse rather than guess: on this build
// crew approve always reports that it needs a terminal.
func isTerminal(f *os.File) bool { return false }
