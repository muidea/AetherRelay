#!/usr/bin/env bash

set -uo pipefail

output_file="$(mktemp)"
cleanup() {
	status=$?
	if [[ $status -ne 0 ]]; then
		cat "$output_file"
	fi
	rm -f "$output_file"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

scripts/test-deploy-docker.sh >"$output_file" 2>&1
go test ./... -count=1 >"$output_file" 2>&1
