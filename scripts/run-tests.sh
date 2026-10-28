#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

output_file="$(mktemp)"
stage="test runner regression"
cleanup() {
	status=$?
	if [[ $status -ne 0 ]]; then
		printf 'Test stage failed: %s (exit %s)\n' "$stage" "$status" >&2
		cat "$output_file" >&2
	fi
	rm -f "$output_file"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

scripts/test-run-tests.sh >"$output_file" 2>&1
stage="deployment script tests"
scripts/test-deploy-docker.sh >"$output_file" 2>&1
stage="Go tests"
go test ./... -count=1 >"$output_file" 2>&1
