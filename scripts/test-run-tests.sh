#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/scripts" "$fixture/bin"
cp "$root/scripts/run-tests.sh" "$fixture/scripts/run-tests.sh"

# Stub all stages so this regression never runs Go, Docker or itself recursively.
cat >"$fixture/scripts/test-run-tests.sh" <<'EOF'
#!/usr/bin/env bash
echo 'runner-marker'
exit "${TEST_RUNNER_EXIT:-0}"
EOF
cat >"$fixture/scripts/test-deploy-docker.sh" <<'EOF'
#!/usr/bin/env bash
echo deployment >>"$TEST_STAGE_LOG"
echo 'deployment-marker'
exit "${TEST_DEPLOY_EXIT:-0}"
EOF
cat >"$fixture/bin/go" <<'EOF'
#!/usr/bin/env bash
echo go >>"$TEST_STAGE_LOG"
echo 'go-marker'
exit "${TEST_GO_EXIT:-0}"
EOF
chmod +x "$fixture/scripts/"*.sh "$fixture/bin/go"
export PATH="$fixture/bin:$PATH" TEST_STAGE_LOG="$fixture/stages"

for scenario in runner deployment go success; do
  export TEST_RUNNER_EXIT=0 TEST_DEPLOY_EXIT=0 TEST_GO_EXIT=0
  : >"$TEST_STAGE_LOG"
  expected=0
  case "$scenario" in
    runner) TEST_RUNNER_EXIT=7; expected=7; stage='test runner regression'; marker=runner-marker ;;
    deployment) TEST_DEPLOY_EXIT=8; expected=8; stage='deployment script tests'; marker=deployment-marker ;;
    go) TEST_GO_EXIT=9; expected=9; stage='Go tests'; marker=go-marker ;;
  esac
  actual=0
  "$fixture/scripts/run-tests.sh" >"$fixture/output" 2>&1 || actual=$?
  if [[ "$actual" != "$expected" ]]; then
    echo "runner status mismatch: $scenario expected=$expected actual=$actual" >&2
    cat "$fixture/output" >&2
    exit 1
  fi
  if [[ "$expected" != 0 ]]; then
    grep -Fq "Test stage failed: $stage (exit $expected)" "$fixture/output"
    grep -Fq "$marker" "$fixture/output"
    if [[ "$scenario" != go ]] && grep -Fq go "$TEST_STAGE_LOG"; then
      echo "runner continued to Go after $scenario failure" >&2
      exit 1
    fi
    if [[ "$scenario" == runner && -s "$TEST_STAGE_LOG" ]]; then
      echo 'runner continued after its first failing stage' >&2
      exit 1
    fi
  else
    [[ "$(cat "$TEST_STAGE_LOG")" == $'deployment\ngo' ]]
    [[ ! -s "$fixture/output" ]]
  fi
done
