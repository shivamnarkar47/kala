// Command kaal is the Go host of the kaal agent harness.
//
// P0 skeleton (Chakra vyuha, the wheel): the version probe only. The
// run/sessions/doctor/update/diagrams subcommands land in P4 (Suchi vyuha)
// per docs/go-migration-plan.md. Exit codes mirror the Python CLI from day
// one: 0 = answer produced / help / version, 1 = config-key-gateway error
// class, 2 = argument or loop error.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version mirrors pyproject.toml and harness/__init__.py. The parity gate
// (P7) asserts `kaal --version` prints exactly this string.
const version = "0.3"

func main() {
	fs := flag.NewFlagSet("kaal", flag.ExitOnError) // -h exits 0, bad flags exit 2 — argparse parity
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "usage: kaal [--version]\n\n")
		fmt.Fprintf(out, "kaal %s — DeepSeek V4 Flash agent harness (Go host, P0 skeleton)\n", version)
		fmt.Fprintf(out, "subcommands (run, sessions, doctor, update, diagrams) arrive in P4\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("kaal %s\n", version)
		return
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "kaal: unknown command %q — subcommands arrive in P4\n", fs.Arg(0))
		os.Exit(2) // argparse exits 2 on an invalid choice
	}
	fs.Usage()
	os.Exit(1) // no-args in Python launches the TUI, which exits 1 without a key
}
