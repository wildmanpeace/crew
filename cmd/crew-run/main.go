// Command crew-run is the implementer's dispatcher: test, lint, build, diff,
// and commit. It is the only shell access an implementer worker is given.
//
// The role is fixed here at compile time. A verifier never has this binary on
// its PATH.
package main

import (
	"github.com/wildmanpeace/crew/internal/config"
	"github.com/wildmanpeace/crew/internal/dispatch"
)

func main() { dispatch.Main(config.RoleImplementer) }
