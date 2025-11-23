package main

import (
	"fmt"
	"os"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/cmd/controller/cmd"
)

//nolint:gochecknoglobals // set by ldflags at build time
var (
	Version = "development"
	Gitsha  = "development"
)

func main() {
	cmd.SetVersion(Version, Gitsha)

	err := cmd.Execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}
