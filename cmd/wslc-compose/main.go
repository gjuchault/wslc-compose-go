// Command wslc-compose provides docker-compose style orchestration for WSL containers (wslc).
package main

import (
	"os"

	"wslc-go/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
