package cli

import (
	"errors"
	"fmt"
	"os"

	"wslc-go/internal/engine"
)

// SplitArgv returns (composeArgs, passthroughArgs) for the `wslc` wrapper;
// composeArgs is nil unless argv starts with the `compose` subcommand.
func SplitArgv(argv []string) (composeArgs, passthrough []string) {
	if len(argv) > 0 && argv[0] == "compose" {
		return append([]string{}, argv[1:]...), []string{}
	}
	return nil, argv
}

// ShimMain implements the `wslc` wrapper: `wslc compose ...` runs
// wslc-compose, anything else is passed through verbatim to the real wslc CLI.
func ShimMain(argv []string) int {
	engine.HandleInterrupts()
	composeArgs, passthrough := SplitArgv(argv)
	if composeArgs != nil {
		if len(composeArgs) == 0 {
			composeArgs = []string{"--help"}
		}
		return Main(composeArgs)
	}
	code, err := engine.RunInteractive(passthrough, false)
	if err != nil && !errors.Is(err, engine.ErrInterrupted) {
		fmt.Fprintf(os.Stderr, "wslc: %v\n", err)
	}
	return code
}
