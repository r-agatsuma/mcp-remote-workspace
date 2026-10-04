// mcp-control must be built with CGO_ENABLED=0 and installed at the daemon's
// fixed host path. It is bind-mounted read-only, never copied into the rootfs.
package main

import (
	"fmt"
	"os"

	"github.com/r-agatsuma/mcp-remote-workspace/internal/control"
)

func main() {
	var err error
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		err = control.Init()
	case "operate":
		err = control.Serve("/workspace", os.Stdin, os.Stdout)
	default:
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "control runtime failed")
		os.Exit(1)
	}
}
