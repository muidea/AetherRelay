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
  printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
  if [[ "${FAKE_DOCKER_HEALTH_FAIL:-}" == "1" && " $* " == *" exec -T aetherrelay curl "* ]]; then
    exit 1
  fi
  exit 0
fi
if [[ "${1:-}" == "pull" ]]; then
  printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
  exit 0
fi
if [[ "${1:-}" == "run" ]]; then
  printf '%s\n' "$*" >>"${FAKE_DOCKER_LOG:?}"
  if [[ " $* " == *" cat /usr/share/aetherrelay/config.example.yaml "* ]]; then
    cat "${FAKE_DOCKER_TEMPLATE:?}"
    exit 0
  fi
fi
echo "unexpected fake docker invocation: $*" >&2
exit 1
EOF
cat >"$TMP/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$TMP/bin/docker" "$TMP/bin/sleep"
export FAKE_DOCKER_LOG="$TMP/docker.log"
export FAKE_DOCKER_TEMPLATE="$ROOT/config.example.yaml"

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

pull_line="$(grep -n -m1 '^pull ' "$FAKE_DOCKER_LOG" | cut -d: -f1)"
template_line="$(grep -n -m1 ' cat /usr/share/aetherrelay/config.example.yaml$' "$FAKE_DOCKER_LOG" | cut -d: -f1)"
if [[ -z "$pull_line" || -z "$template_line" || "$pull_line" -ge "$template_line" ]]; then
  echo "deployment did not pull the image before extracting its template" >&2
  exit 1
fi
if grep -Eq '^compose .* pull$' "$FAKE_DOCKER_LOG"; then
  echo "deployment pulled the mutable image tag again after extracting its template" >&2
  exit 1
fi

credential_key_before="$(sed -n "s/^AETHERRELAY_CREDENTIAL_KEY='\(.*\)'$/\1/p" "$env_file")"
printf 'UNMANAGED_VALUE=$(touch %s)\n' "$TMP/env-was-executed" >>"$env_file"
config="$TMP/deploy/config/config.yaml"
cat >"$config" <<'EOF'
server:
  listen_addr: 0.0.0.0:8080

state:
  dir: /var/lib/aetherrelay

chatgpt_web:
  provider_enabled: true
  temporary_chat:
    enabled: true

codex_oauth:
  provider_enabled: true
  websocket_max_sessions: 99
  websocket_max_message_bytes: 1234
  websocket_idle_timeout_seconds: 45
  websocket_max_lifetime_seconds: 67
EOF
chmod 0644 "$config"
config_before="$(sha256sum "$config" | cut -d' ' -f1)"
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
config_after="$(sha256sum "$config" | cut -d' ' -f1)"
if [[ "$config_after" != "$config_before" ]]; then
  echo "repeated deployment changed the existing final config" >&2
  exit 1
fi
grep -Fq '  websocket_max_sessions: 99' "$config"
grep -Fq '  websocket_max_message_bytes: 1234' "$config"
grep -Fq '  websocket_idle_timeout_seconds: 45' "$config"
grep -Fq '  websocket_max_lifetime_seconds: 67' "$config"

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
