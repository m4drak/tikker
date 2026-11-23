// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package metrics exposes Prometheus-format metrics at /metrics (backlog §5 #3).
//
// It intentionally has no third-party dependency: the Prometheus text
// exposition format (a name, optional labels in braces and a float value per
// line) is simple enough to render directly, and the project avoids adding
// dependencies without a reason (SPEC §11).
package metrics
