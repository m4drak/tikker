// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Command tikker is an SMS gateway for MikroTik RouterOS devices.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/m4drak/tikker/internal/version"
)

const usage = `usage: tikker <command>

commands:
  version   print build version information (also available as --version)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command given in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tikker: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// printUsage writes the usage text. Write errors on the console are ignored
// deliberately: there is no better channel left to report them on.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usage)
}
