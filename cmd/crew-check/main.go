// Command crew-check is the verifier's dispatcher: test, lint, and build
// only. It exposes no commit and no diff verb, so a verifier has no path to
// either — not because a check catches the attempt, but because nothing on
// its PATH implements them.
//
// The role is fixed here at compile time.
package main

import (
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/dispatch"
)

func main() { dispatch.Main(config.RoleVerifier) }
