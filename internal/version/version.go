// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package version exposes build information injected at link time.
package version

import "fmt"

// The variables below are overridden at build time with -ldflags -X
// (see the Makefile). The defaults identify untagged, local builds.
var (
	// Version is the release version, or "dev" for local builds.
	Version = "dev"
	// Commit is the git revision the binary was built from.
	Commit = "none"
	// Date is the build timestamp in RFC 3339 format.
	Date = "unknown"
)

// String returns a one-line, human-readable description of the build.
func String() string {
	return fmt.Sprintf("tikker %s (commit %s, built %s)", Version, Commit, Date)
}
