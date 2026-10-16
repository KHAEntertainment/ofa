// Command ofa detects installed coding harnesses and applies their updates.
package main

import (
	"os"

	"ofa/internal/cli"
)

// version is the OFA build version. Bumped per release; a release step
// will stamp it via -ldflags.
const version = "0.1.0-dev"

func main() {
	app := cli.New(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(int(app.Run()))
}