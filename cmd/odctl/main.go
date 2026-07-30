// odctl is the command line for the OpenDrive Bridge (whitepaper §4.6).
package main

import (
	"os"

	"github.com/StormRealm/opendrive-bridge/internal/cli"
)

var version = "0.0.0-dev"

func main() {
	cli.Version = version
	os.Exit(cli.Execute(os.Args[1:], &cli.Options{}))
}
