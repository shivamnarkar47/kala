// Command kaal is the Go host of the kaal agent harness.
package main

import (
	"os"

	"github.com/kaal/kaal/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
