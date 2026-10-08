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
	if len(argv) > 0 && (argv[0] == "--help" || argv[0] == "-h") {
		printUsage()
		return
	}
	os.Exit(cli.Run(argv, os.Stdout, os.Stderr))
}

func printUsage() {
	fmt.Print(`git-feedback observes GitHub review feedback and delivers replayable events.

Usage:
  git-feedback reconcile <URL> [--head <SHA>] [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback snapshot <URL> --snapshot <ID> --output <FILE> [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback inbox <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--limit 50] [--after <CURSOR>] [--ids-only] [--exclude-self] [--json]
  git-feedback ack <URL> --consumer <NAME> --event=<ID> [--event=<ID>...] [--account <LOGIN>] [--state-dir <DIR>] [--json]
  git-feedback wait <URL> --consumer <NAME> [--account <LOGIN>] [--state-dir <DIR>] [--timeout 30m] [--head <SHA>] [--limit 50] [--exclude-self] [--json]

Flags:
  --version    print version and exit
  --help       show this help
`)
}
