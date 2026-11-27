#!/usr/bin/env bash
# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Writes THIRD_PARTY_NOTICES.txt: the licence, NOTICE and PATENTS files of every third-party Go
# module compiled into the operator and CSI driver binaries. The images ship it under /licenses.
# Run by `make third-party-notices`; `make license-check` fails if the committed file is stale.
set -euo pipefail

GO_LICENSES="${GO_LICENSES:-go-licenses}"
OUT="${1:-THIRD_PARTY_NOTICES.txt}"
MODULE="$(go list -m)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$GO_LICENSES" save ./cmd/... --save_path="$tmp/save" --ignore "$MODULE" 2>/dev/null
"$GO_LICENSES" report ./cmd/... --ignore "$MODULE" 2>/dev/null | sort > "$tmp/report.csv"

{
  echo "Third-party software notices for the Ksolves Operator for Apache NiFi"
  echo
  echo "The operator and CSI driver binaries include the Go modules below. Each is listed with"
  echo "its licence type, followed by the licence, NOTICE and PATENTS files it ships."
  while IFS=, read -r pkg url licence; do
    echo
    echo "================================================================================"
    echo "$pkg"
    echo "Licence: $licence"
    echo "Source: $url"
    echo "================================================================================"
    dir="$tmp/save/$pkg"
    while IFS= read -r f; do
      echo
      echo "--- $(basename "$f") ---"
      echo
      cat "$f"
    done < <(find "$dir" -maxdepth 1 -type f \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'PATENTS*' \) | sort)
  done < "$tmp/report.csv"
} > "$OUT"
