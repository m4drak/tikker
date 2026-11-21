#!/bin/sh
# SPDX-FileCopyrightText: 2026 The tikker Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Generate THIRD_PARTY_NOTICES: every third-party module linked into the binary,
# its license name and the full license text (needed for MIT/BSD/Apache redistribution).
# Usage: scripts/notices.sh <output-file>
set -eu

out=${1:-THIRD_PARTY_NOTICES}
tool=${GO_LICENSES:-github.com/google/go-licenses/v2@v2.0.1}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Tab-separated: module, version, license name, path of the license file.
CGO_ENABLED=0 go run "$tool" report ./cmd/tikker --ignore github.com/m4drak/tikker \
  --template scripts/notices.tpl > "$tmp.raw"
sort "$tmp.raw" > "$tmp"
rm -f "$tmp.raw"

tab=$(printf '\t')
{
  printf 'THIRD-PARTY NOTICES\n\n'
  printf 'tikker is licensed under GPL-3.0-or-later. It includes the following third-party\n'
  printf 'components, distributed under their own licenses (full texts below).\n\n'
  while IFS=$tab read -r name version license _path; do
    printf '%s %s (%s)\n' "$name" "$version" "$license"
  done < "$tmp"
  while IFS=$tab read -r name version license path; do
    printf '\n\n================================================================================\n'
    printf '%s %s - %s\n' "$name" "$version" "$license"
    printf '================================================================================\n\n'
    cat "$path"
  done < "$tmp"
} > "$out"
