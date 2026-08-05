#!/usr/bin/env bash
# 一键容器化部署脚本：生成配置、收集 Provider 密钥、初始化 Admin 凭据并启动容器。
# 产物全部落在部署目录（默认 ./deploy），不依赖仓库内的 compose.yaml。
#
# Admin 密码哈希只通过交互式 TTY 生成（密码不进入 argv / 环境变量 / 日志），
# 见 internal/services/aiproxy/password_hash.go；无交互场景请用
# --admin-password-hash 注入已生成的哈希（ai-proxy admin password-hash 生成）。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DEPLOY_DIR="${DEPLOY_DIR:-./deploy}"
IMAGE="${AI_PROXY_IMAGE:-ghcr.io/muidea/ai-proxy:latest}"
ADMIN_USERNAME="ops-admin"
ADMIN_PASSWORD_HASH=""
SKIP_ADMIN=0
LISTEN="127.0.0.1:8080"

if command -v docker >/dev/null 2>&1; then
  DOCKER=docker
elif command -v podman >/dev/null 2>&1; then
  DOCKER=podman
else
  echo "error: docker (或 podman) 不可用" >&2
  exit 2
fi

# docker compose 子命令（v2）与独立 docker-compose 二选一。
if "$DOCKER" compose version >/dev/null 2>&1; then
  COMPOSE=("$DOCKER" compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=(docker-compose)
else
  echo "error: 未找到 docker compose（v2 插件或 docker-compose）" >&2
  exit 2
fi

die() { echo "error: $*" >&2; exit 1; }
warn() { echo "warning: $*" >&2; }

usage() {
  cat <<EOF
usage: $0 [options]

一键容器化部署 ai-proxy。产物全部落在部署目录（默认 ./deploy）：
  config/config.yaml   运行配置（由 config.example.yaml 模板生成）
  docker-compose.yml   容器编排（自包含，不依赖仓库）
  .env                 Provider 密钥与 Admin 哈希（chmod 600）

options:
  --dir <path>                 部署目录（默认 ./deploy）
  --image <image>              镜像引用（默认 ghcr.io/muidea/ai-proxy:latest，
                               也可用环境变量 AI_PROXY_IMAGE 覆盖）
  --admin-username <name>      Admin 用户名（默认 ops-admin，仅限 [A-Za-z0-9._-]）
  --admin-password-hash <phc>  直接提供 Argon2id PHC 哈希（无交互/CI 场景；
                               省略时进入交互式生成）
  --skip-admin                 不启用 Admin 登录（仅本机可访问管理台）
  --listen <addr>              宿主机端口绑定（默认 127.0.0.1:8080，仅本机可访问；
                               改 0.0.0.0:8080 会放开到所有网卡并打印安全警告）。
                               容器内 listen_addr 恒为 0.0.0.0:8080，由端口映射控制暴露面）
  -h, --help
EOF
}

# 解析参数
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir) DEPLOY_DIR="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --admin-username) ADMIN_USERNAME="$2"; shift 2 ;;
    --admin-password-hash) ADMIN_PASSWORD_HASH="$2"; shift 2 ;;
    --skip-admin) SKIP_ADMIN=1; shift ;;
    --listen) LISTEN="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

[[ "$ADMIN_USERNAME" =~ ^[A-Za-z0-9._-]+$ ]] || die "admin 用户名仅限 [A-Za-z0-9._-]: $ADMIN_USERNAME"
[[ "$LISTEN" =~ ^[A-Za-z0-9.:-]+$ ]] || die "非法的 --listen 值: $LISTEN（仅允许字母、数字、点、冒号、短横线）"
if [[ -n "$ADMIN_PASSWORD_HASH" ]]; then
  [[ "$ADMIN_PASSWORD_HASH" =~ ^\$argon2id\$ ]] || die "--admin-password-hash 必须是 Argon2id PHC 哈希（以 \$argon2id\$ 开头）"
fi
if [[ "$SKIP_ADMIN" == 1 && -n "$ADMIN_PASSWORD_HASH" ]]; then
  warn "--skip-admin 与 --admin-password-hash 同时给出，将忽略哈希"
  ADMIN_PASSWORD_HASH=""
fi

DEPLOY_DIR="$(realpath "$DEPLOY_DIR")"
CONFIG_DIR="$DEPLOY_DIR/config"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
ENV_FILE="$DEPLOY_DIR/.env"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.yml"

mkdir -p "$CONFIG_DIR"

# 已有 .env 先载入作为默认值（值为我们生成时加引号的形式，source 安全）。
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  set -a; source "$ENV_FILE"; set +a
fi

echo "==> 部署目录: $DEPLOY_DIR"
echo "==> 镜像: $IMAGE"

# 1. 生成 config.yaml（已有则保留，不覆盖用户改动）
if [[ -f "$CONFIG_FILE" ]]; then
  warn "检测到已有 $CONFIG_FILE，保留原样"
  warn "若未配置 Admin 登录，可运行: ${COMPOSE[*]} exec ai-proxy admin set-credentials --username ops-admin --config /etc/ai-proxy/config.yaml"
else
  if [[ -f "$REPO_ROOT/config.example.yaml" ]]; then
    cp "$REPO_ROOT/config.example.yaml" "$CONFIG_FILE"
  else
    # 脚本脱离仓库使用时，从镜像内模板生成。
    "$DOCKER" run --rm "$IMAGE" cat /usr/share/ai-proxy/config.example.yaml >"$CONFIG_FILE"
  fi
  # 容器内固定监听全部网卡（由宿主机端口映射控制暴露面），状态落到命名卷挂载点。
  sed -i 's|^  listen_addr:.*|  listen_addr: 0.0.0.0:8080|' "$CONFIG_FILE"
  sed -i 's|^  dir: .*|  dir: /var/lib/ai-proxy|' "$CONFIG_FILE"
  if [[ "$SKIP_ADMIN" != 1 ]]; then
    # 在 server 段末尾（slo_violation_webhook 之后）插入 Admin 登录配置。
    # 哈希经 ${AI_PROXY_ADMIN_PASSWORD_HASH} 由 .env 注入，不写入 YAML 明文。
    sed -i "/^  slo_violation_webhook:/a\\
  admin_auth_enabled: true\\
  admin_username: \"$ADMIN_USERNAME\"\\
  admin_password_hash: \${AI_PROXY_ADMIN_PASSWORD_HASH}" "$CONFIG_FILE"
    echo "    -> 已启用 Admin 登录（用户名: $ADMIN_USERNAME）"
  fi
  echo "    -> 已生成 $CONFIG_FILE"
fi

# 2. 收集 Provider 密钥（交互或复用 .env）
ask_secret() {
  local name="$1" current="${!1:-}" v
  local prompt
  if [[ -n "$current" ]]; then
    prompt="$name [已配置，回车保留]: "
  else
    prompt="$name [留空跳过]: "
  fi
  read -r -p "$prompt" v || true
  if [[ -n "$v" ]]; then
    printf '%s' "$v"
  else
    printf '%s' "$current"
  fi
}

if [[ -t 0 ]]; then
  OPENAI_API_KEY="$(ask_secret OPENAI_API_KEY)"
  DEEPSEEK_API_KEY="$(ask_secret DEEPSEEK_API_KEY)"
  ANTHROPIC_API_KEY="$(ask_secret ANTHROPIC_API_KEY)"
else
  warn "非交互环境：Provider 密钥沿用环境变量或已有 .env 的值"
fi

# 3. Admin 密码哈希：优先 --admin-password-hash，其次交互式生成
if [[ "$SKIP_ADMIN" != 1 && -z "$ADMIN_PASSWORD_HASH" ]]; then
  if [[ -n "${AI_PROXY_ADMIN_PASSWORD_HASH:-}" ]]; then
    ADMIN_PASSWORD_HASH="$AI_PROXY_ADMIN_PASSWORD_HASH"
  elif [[ -t 0 && -t 1 ]]; then
    echo "==> 生成 Admin 密码哈希（密码在容器内交互输入，不进入参数/日志）"
    hash_output="$("$DOCKER" run --rm -it "$IMAGE" ai-proxy admin password-hash 2>&1)" \
      || die "生成密码哈希失败，docker 输出: $(printf '%s' "$hash_output" | tail -n 3 | tr '\n' ' ')"
    # pty 模式下提示与哈希混在同一输出流，提取 PHC 行。
    ADMIN_PASSWORD_HASH="$(printf '%s' "$hash_output" | grep -o '\$argon2id\$[^[:space:]]*' | tail -n 1 | tr -d '\r')"
    [[ "$ADMIN_PASSWORD_HASH" =~ ^\$argon2id\$ ]] || die "password-hash 输出异常: $(printf '%s' "$hash_output" | tail -n 3 | tr '\n' ' ')"
  else
    die "非交互环境无法生成 Admin 哈希；请用 --admin-password-hash 提供，或 --skip-admin 跳过"
  fi
fi

# 4. 写入 .env（chmod 600；值加单引号，source 与 compose 插值均安全）
{
  echo "AI_PROXY_IMAGE='$IMAGE'"
  echo "OPENAI_API_KEY='${OPENAI_API_KEY:-}'"
  echo "DEEPSEEK_API_KEY='${DEEPSEEK_API_KEY:-}'"
  echo "ANTHROPIC_API_KEY='${ANTHROPIC_API_KEY:-}'"
  if [[ "$SKIP_ADMIN" != 1 ]]; then
    echo "AI_PROXY_ADMIN_PASSWORD_HASH='$ADMIN_PASSWORD_HASH'"
  fi
} >"$ENV_FILE"
chmod 600 "$ENV_FILE"
echo "    -> 已写入 $ENV_FILE (chmod 600)"

# 5. 生成 docker-compose.yml（自包含）
if [[ "$LISTEN" == "127.0.0.1:"* || "$LISTEN" == "localhost:"* || "$LISTEN" == ":"* ]]; then
  BIND="127.0.0.1:8080:8080"
else
  BIND="8080:8080"
  warn "非 loopback 监听：请确认已启用 Admin 登录（否则管理台仅 loopback），并由防火墙/反向代理保护数据面"
fi

cat >"$COMPOSE_FILE" <<EOF
services:
  ai-proxy:
    image: \${AI_PROXY_IMAGE:-$IMAGE}
    restart: unless-stopped
    ports:
      - "$BIND"
    environment:
      AI_PROXY_CONFIG: /etc/ai-proxy/config.yaml
      OPENAI_API_KEY: \${OPENAI_API_KEY:-}
      DEEPSEEK_API_KEY: \${DEEPSEEK_API_KEY:-}
      ANTHROPIC_API_KEY: \${ANTHROPIC_API_KEY:-}
      AI_PROXY_ADMIN_PASSWORD_HASH: \${AI_PROXY_ADMIN_PASSWORD_HASH:-}
    volumes:
      # 目录挂载（而非单文件）：管理页保存配置时通过临时文件原子替换。
      - ./config:/etc/ai-proxy
      - ai-proxy-state:/var/lib/ai-proxy

volumes:
  ai-proxy-state:
EOF
echo "    -> 已生成 $COMPOSE_FILE"

# 6. 配置目录交由容器内 UID 10001（ai-proxy 用户）持有（尽力而为）
if [[ -w "$CONFIG_DIR" ]]; then
  chown -R 10001:10001 "$CONFIG_DIR" 2>/dev/null \
    || warn "无法 chown 配置目录为 10001；管理页保存配置可能不可写（只读挂载仍可运行）"
fi

# 7. 启动并等待就绪
echo "==> 拉取镜像并启动容器"
"${COMPOSE[@]}" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" up -d

echo "==> 等待服务就绪"
READY=0
for _ in $(seq 1 30); do
  if "${COMPOSE[@]}" -f "$COMPOSE_FILE" exec -T ai-proxy curl --fail -s http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done
if [[ "$READY" != 1 ]]; then
  warn "服务未在预期时间内就绪，请检查: ${COMPOSE[*]} -f $COMPOSE_FILE logs -f"
fi

# 8. 结果提示
echo ""
echo "=================================================="
echo "部署完成"
echo "  管理台:      http://127.0.0.1:8080/admin/"
echo "  健康检查:    curl http://127.0.0.1:8080/healthz"
echo "  查看日志:    ${COMPOSE[*]} -f $COMPOSE_FILE logs -f"
if [[ "$SKIP_ADMIN" != 1 ]]; then
  echo "  Admin 账号:  $ADMIN_USERNAME（默认仅本机可登录；经 HTTPS 反向代理部署时，"
  echo "                请在 config.yaml 开启 admin_session_cookie_secure: true）"
else
  echo "  注意:        未启用 Admin 登录（--skip-admin）；如需启用请重跑本脚本，或"
  echo "                ${COMPOSE[*]} exec ai-proxy admin set-credentials --config /etc/ai-proxy/config.yaml"
fi
echo "  变更密码:    重跑本脚本（交互生成新哈希）后重启容器，或更新 $ENV_FILE 中的"
echo "                AI_PROXY_ADMIN_PASSWORD_HASH 后 docker compose up -d"
echo "=================================================="
