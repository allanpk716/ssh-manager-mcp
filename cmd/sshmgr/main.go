package main

import (
	"os"

	"ssh-manager-mcp/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(cli.ExitCodeFor(err))
	}
}
