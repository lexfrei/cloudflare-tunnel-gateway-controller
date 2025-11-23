package main

import (
	"os"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/cmd/controller/cmd"
)

//nolint:gochecknoglobals // set by ldflags at build time
var (
	Version = "development"
	Gitsha  = "development"
)

//nolint:noinlineerr // inline error handling is standard for main
func main() {
	cmd.SetVersion(Version, Gitsha)

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
