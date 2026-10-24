// Command ofa detects installed coding harnesses and applies their updates.
package main

import (
	"os"

	"ofa/internal/cli"
)

func main() {
	app := cli.New(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(int(app.Run()))
}