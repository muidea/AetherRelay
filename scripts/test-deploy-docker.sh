#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
cat >"$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "compose" ]]; then
  if [[ "${FAKE_DOCKER_HEALTH_FAIL:-}" == "1" && " $* " == *" exec -T aetherrelay curl "* ]]; then
    exit 1
  fi
  exit 0
fi
echo "unexpected fake docker invocation: $*" >&2
exit 1
EOF
cat >"$TMP/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$TMP/bin/docker" "$TMP/bin/sleep"

PATH="$TMP/bin:$PATH" "$ROOT/scripts/deploy-docker.sh" \
  --dir "$TMP/deploy" \
  --skip-admin \
  --listen 0.0.0.0:9090 \
  >"$TMP/output" 2>"$TMP/error"

compose="$TMP/deploy/docker-compose.yml"
env_file="$TMP/deploy/.env"

test -d "$TMP/deploy/data"
grep -Fq -- '- "0.0.0.0:9090:8080"' "$compose"
grep -Fq -- '- ./data:/var/lib/aetherrelay' "$compose"
grep -Fq 'AETHERRELAY_IMAGE=' "$env_file"
grep -Eq '^AETHERRELAY_CREDENTIAL_KEY=.{40,}$' "$env_file"
grep -Fq 'AETHERRELAY_CREDENTIAL_KEY:' "$compose"
grep -Fq 'Provider:    登录管理台后在 Provider 管理中添加' "$TMP/output"
grep -Fq "持久化数据:  $TMP/deploy/data" "$TMP/output"

credential_key_before="$(sed -n "s/^AETHERRELAY_CREDENTIAL_KEY='\(.*\)'$/\1/p" "$env_file")"
printf 'UNMANAGED_VALUE=$(touch %s)\n' "$TMP/env-was-executed" >>"$env_file"
PATH="$TMP/bin:$PATH" "$ROOT/scripts/deploy-docker.sh" \
  --dir "$TMP/deploy" \
  --skip-admin \
  --listen 0.0.0.0:9090 \
  >"$TMP/output-repeat" 2>"$TMP/error-repeat"
credential_key_after="$(sed -n "s/^AETHERRELAY_CREDENTIAL_KEY='\(.*\)'$/\1/p" "$env_file")"
if [[ -z "$credential_key_before" || "$credential_key_after" != "$credential_key_before" ]]; then
  echo "repeated deployment changed AETHERRELAY_CREDENTIAL_KEY" >&2
  exit 1
fi
if [[ -e "$TMP/env-was-executed" ]]; then
  echo "deployment script executed content from .env" >&2
  exit 1
fi

if grep -Fq 'aetherrelay-state' "$compose"; then
  echo "deploy compose still contains a named state volume" >&2
  exit 1
fi

if grep -Eq 'OPENAI_API_KEY|DEEPSEEK_API_KEY|ANTHROPIC_API_KEY' "$compose" "$env_file"; then
  echo "deploy output still contains fixed Provider key variables" >&2
  exit 1
fi
if grep -Fq 'AETHERRELAY_ADMIN_PASSWORD_HASH=' "$env_file"; then
  echo "--skip-admin unexpectedly persisted an Admin password hash" >&2
  exit 1
fi

if FAKE_DOCKER_HEALTH_FAIL=1 PATH="$TMP/bin:$PATH" "$ROOT/scripts/deploy-docker.sh" \
  --dir "$TMP/deploy-health-fail" \
  --skip-admin \
  --listen 0.0.0.0:9090 \
  >"$TMP/output-health-fail" 2>"$TMP/error-health-fail"; then
  echo "deployment unexpectedly succeeded when health checks failed" >&2
  exit 1
fi
grep -Fq '服务未在预期时间内就绪' "$TMP/error-health-fail"
if grep -Fq '部署完成' "$TMP/output-health-fail"; then
  echo "failed deployment still reported completion" >&2
  exit 1
fi
