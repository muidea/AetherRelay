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
  exit 0
fi
echo "unexpected fake docker invocation: $*" >&2
exit 1
EOF
chmod +x "$TMP/bin/docker"

PATH="$TMP/bin:$PATH" "$ROOT/scripts/deploy-docker.sh" \
  --dir "$TMP/deploy" \
  --skip-admin \
  --listen 0.0.0.0:9090 \
  >"$TMP/output" 2>"$TMP/error"

compose="$TMP/deploy/docker-compose.yml"
env_file="$TMP/deploy/.env"

grep -Fq -- '- "0.0.0.0:9090:8080"' "$compose"
grep -Fq 'AI_PROXY_IMAGE=' "$env_file"
grep -Fq 'Provider:    登录管理台后在 Provider 管理中添加' "$TMP/output"

if grep -Eq 'OPENAI_API_KEY|DEEPSEEK_API_KEY|ANTHROPIC_API_KEY' "$compose" "$env_file"; then
  echo "deploy output still contains fixed Provider key variables" >&2
  exit 1
fi
if grep -Fq 'AI_PROXY_ADMIN_PASSWORD_HASH=' "$env_file"; then
  echo "--skip-admin unexpectedly persisted an Admin password hash" >&2
  exit 1
fi

