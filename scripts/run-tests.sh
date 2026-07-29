#!/usr/bin/env bash

set -euo pipefail

output_file="$(mktemp)"
trap 'rm -f "$output_file"' EXIT

if ! go test ./... -count=1 >"$output_file" 2>&1; then
  cat "$output_file"
  exit 1
fi
