// Command git-feedback is the forge-neutral feedback CLI. It is also
// installed as a Git subcommand, where `git feedback ...` strips the leading
// "feedback" token before dispatch.
package main

import (
	"fmt"
	"os"

	"github.com/sdougbrown/git-feedback/internal/cli"
)

// version is overridden at build time with -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	argv := os.Args[1:]
	if len(argv) > 0 && argv[0] == "feedback" {
		argv = argv[1:]
	}
	if len(argv) > 0 && argv[0] == "--version" {
		fmt.Printf("git-feedback %s\n", version)
		return
	}
	os.Exit(cli.Run(argv, os.Stdout, os.Stderr))
}
