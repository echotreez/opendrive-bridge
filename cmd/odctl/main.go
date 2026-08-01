// odctl is the command line for the OpenDrive Bridge (whitepaper §4.6).
package main

import (
	"fmt"
	"os"

	"github.com/echotreez/opendrive-bridge/internal/cli"
)

// Set by the linker at release time (see .goreleaser.yaml) so that a binary
// can be traced back to the commit it came from.
var (
	version   = "0.0.0-dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	cli.Version = fmt.Sprintf("%s (commit %s, built %s)", version, commit, buildDate)
	os.Exit(cli.Execute(os.Args[1:], &cli.Options{}))
}
