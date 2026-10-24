// Command ember rents a GPU node, runs a container on it, forwards its ports
// home over SSH, and destroys the node.
package main

import (
	"fmt"
	"os"

	"github.com/Legion112/ember/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ember: %v\n", err)
		os.Exit(cli.ExitCode(err))
	}
}
