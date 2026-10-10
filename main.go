// dymploy receives repository events, builds in Docker and deploys the artifact.
package main

import (
	"context"
	"os"

	"github.com/dymmer-code/dymploy/cmd"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	if err := cmd.NewRootCmd(version).ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
