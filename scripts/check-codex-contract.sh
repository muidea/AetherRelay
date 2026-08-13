#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

go test ./internal/modules/application/proxyapi/service/proxy ./internal/modules/application/proxyapi/biz ./internal/modules/blocks/codexaccountpool/biz ./internal/modules/blocks/codexupstream/biz
go test -race ./internal/modules/application/proxyapi/service/proxy ./internal/modules/application/proxyapi/biz ./internal/modules/blocks/codexaccountpool/biz ./internal/modules/blocks/codexupstream/biz
go vet ./...
git diff --check

tracked_files="$(mktemp)"
findings="$(mktemp)"
trap 'rm -f "$tracked_files" "$findings"' EXIT
while IFS= read -r -d '' path; do
  case "$path" in
    vendor/*|*_test.go|*/testdata/*|scripts/check-codex-contract.sh) continue ;;
  esac
  printf '%s\0' "$path"
done < <(git ls-files --cached --others --exclude-standard -z) >"$tracked_files"
pattern='(sk-[A-Za-z0-9_-]{20,}|Bearer[[:space:]]+[A-Za-z0-9._-]{24,}|-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----)'
xargs -0 rg -n --no-heading "$pattern" <"$tracked_files" >"$findings" 2>/dev/null || true
if [[ -s "$findings" ]]; then
  cat "$findings"
  echo "credential-like material found in tracked files" >&2
  exit 1
fi
