// Command gitlet coordinates AI coding agents working on one git repository.
package main

import (
	"os"

	"github.com/crewlet/gitlet/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
