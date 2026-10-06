// Command agentgw is a self-hosted MCP/API gateway: sources -> rules -> actions.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = `usage: agentgw <command> [flags]

commands:
  validate   check a config file
  rules      test rules against a saved event
  run-once   poll every source once and run matching actions
  version    print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "validate", "rules", "run-once":
		fmt.Fprintf(os.Stderr, "agentgw %s: not implemented yet\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
