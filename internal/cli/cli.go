// Package cli implements the d256-node command line.
package cli

import (
	"fmt"
	"io"
	"runtime"

	nodelinkv1 "github.com/dest1256/d256-panel/proto/nodelink/v1"

	"github.com/dest1256/d256-node/internal/version"
)

var errNotImplemented = fmt.Errorf("not implemented yet")

const usage = `d256-node - Xray node for d256 panel

Usage:
  d256-node <command> [arguments]

Commands:
  run       start the node daemon
  enroll    enroll this node: enroll <panel-url> <token>
  status    show panel link, config version, Xray state, journal size
  logs      show node logs
  reload    ask the running daemon to reload the config
  reset     delete keys and cache
  version   print versions
`

// Run executes the command and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "d256-node %s (%s/%s, go %s, protocol %d)\n",
			version.Version, runtime.GOOS, runtime.GOARCH, runtime.Version(),
			nodelinkv1.ProtocolVersion_PROTOCOL_VERSION_1)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "run", "enroll", "status", "logs", "reload", "reset":
		fmt.Fprintf(stderr, "d256-node %s: %v\n", args[0], errNotImplemented)
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
