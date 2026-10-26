// Command wslc wraps the real wslc CLI to add a `compose` subcommand like
// docker/podman: `wslc compose up -d` runs wslc-compose, anything else is
// passed through verbatim.
package main

import (
	"os"

	"wslc-go/internal/cli"
)

func main() {
	os.Exit(cli.ShimMain(os.Args[1:]))
}
