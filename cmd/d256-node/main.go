package main

import (
	"os"

	"github.com/dest1256/d256-node/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
