// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Command tikker is an SMS gateway for MikroTik RouterOS devices.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/probes"
	"github.com/m4drak/tikker/internal/version"
)

// Exit codes. A configuration error is reported with exitConfig so that
// service managers can tell it apart from a runtime failure. exitFailure is
// used by `healthcheck` when the probe fails.
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

// healthcheckTimeout bounds the whole `healthcheck` request (SPEC §5).
const healthcheckTimeout = 3 * time.Second

const usage = `usage: tikker [--config FILE] <command>
       tikker [--config FILE] --check-config

commands:
  version         print build version information (also available as --version)
  healthcheck     probe the local /readyz (exit 0 when ready); --live probes /healthz

options:
  --config FILE   read the YAML configuration from FILE
                  (default: TIKKER_CONFIG, then $TIKKER_DATA_DIR/config.yaml)
  --check-config  validate the configuration, print the effective settings
                  with secrets redacted, and exit
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line in args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tikker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "configuration file")
	checkConfig := fs.Bool("check-config", false, "validate the configuration and exit")
	showVersion := fs.Bool("version", false, "print build version information")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "tikker: %v\n\n", err)
		printUsage(stderr)
		return exitConfig
	}
	rest := fs.Args()

	if *checkConfig {
		if len(rest) != 0 {
			printUsage(stderr)
			return exitConfig
		}
		return runCheckConfig(*configPath, stdout, stderr)
	}

	if *showVersion {
		if len(rest) != 0 {
			printUsage(stderr)
			return exitConfig
		}
		_, _ = fmt.Fprintln(stdout, version.String())
		return exitOK
	}

	if len(rest) == 0 {
		printUsage(stderr)
		return exitConfig
	}

	switch rest[0] {
	case "version":
		if len(rest) != 1 {
			printUsage(stderr)
			return exitConfig
		}
		_, _ = fmt.Fprintln(stdout, version.String())
		return exitOK
	case "healthcheck":
		return runHealthcheck(*configPath, rest[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "tikker: unknown command %q\n\n", rest[0])
		printUsage(stderr)
		return exitConfig
	}
}

// runCheckConfig loads and validates the configuration. On success it prints
// the effective settings; on failure it lists every problem and returns
// exitConfig.
func runCheckConfig(path string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(config.Options{Path: path})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "tikker: invalid configuration:")
		for _, line := range errorLines(err) {
			_, _ = fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return exitConfig
	}

	_, _ = fmt.Fprintln(stdout, "configuration OK, effective settings (secrets redacted):")
	_, _ = io.WriteString(stdout, cfg.String())
	return exitOK
}

// runHealthcheck implements `tikker healthcheck [--live]`, used by the image
// HEALTHCHECK. It loads the configuration to find the listen address, probes
// the local instance and returns exitOK only for 200 OK. Every failure,
// including an unreadable configuration, returns exitFailure.
func runHealthcheck(configPath string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tikker healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	live := fs.Bool("live", false, "probe /healthz (process liveness) instead of /readyz")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return exitOK
		}
		_, _ = fmt.Fprintf(stderr, "tikker healthcheck: %v\n", err)
		printUsage(stderr)
		return exitConfig
	}
	if fs.NArg() != 0 {
		printUsage(stderr)
		return exitConfig
	}

	cfg, err := config.Load(config.Options{Path: configPath})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "tikker healthcheck: invalid configuration:")
		for _, line := range errorLines(err) {
			_, _ = fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return exitFailure
	}

	target, err := probes.HealthcheckURL(cfg.Listen, *live)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tikker healthcheck: %v\n", err)
		return exitFailure
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := probes.Healthcheck(ctx, &http.Client{}, target); err != nil {
		_, _ = fmt.Fprintf(stderr, "tikker healthcheck: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// errorLines returns the messages of a (possibly nested) joined error, one
// per problem.
func errorLines(err error) []string {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		var lines []string
		for _, e := range multi.Unwrap() {
			lines = append(lines, errorLines(e)...)
		}
		return lines
	}
	return []string{err.Error()}
}

// printUsage writes the usage text. Write errors on the console are ignored
// deliberately: there is no better channel left to report them on.
func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, usage)
}
