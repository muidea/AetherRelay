#!/usr/bin/env bash
# 一键容器化部署脚本：生成配置、初始化 Admin 凭据并启动容器。
# 产物全部落在部署目录（默认 ./deploy），不依赖仓库内的 compose.yaml。
#
# Admin 密码哈希只通过交互式 TTY 生成（密码不进入 argv / 环境变量 / 日志），
# 见 internal/services/aetherrelay/password_hash.go；无交互场景请用
# --admin-password-hash 注入已生成的哈希（AetherRelay admin password-hash 生成）。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

DEPLOY_DIR="${DEPLOY_DIR:-./deploy}"
IMAGE="${AETHERRELAY_IMAGE:-ghcr.io/muidea/aetherrelay:latest}"
ADMIN_USERNAME="ops-admin"
ADMIN_PASSWORD_HASH=""
SKIP_ADMIN=0
LISTEN="127.0.0.1:8080"
HASH_DIR=""

cleanup() {
  if [[ -n "$HASH_DIR" && -d "$HASH_DIR" ]]; then
    rm -rf "$HASH_DIR"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

一键容器化部署 AetherRelay。产物全部落在部署目录（默认 ./deploy）：
  config/config.yaml   运行配置（由 config.example.yaml 模板生成）
  data/                持久化数据（DuckDB、图片、交互归档与账号池数据）
  docker-compose.yml   容器编排（自包含，不依赖仓库）
  .env                 镜像引用与 Admin 哈希（chmod 600）

options:
  --dir <path>                 部署目录（默认 ./deploy）
  --image <image>              镜像引用（默认 ghcr.io/muidea/aetherrelay:latest，
                               也可用环境变量 AETHERRELAY_IMAGE 覆盖）
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
if [[ ! "$LISTEN" =~ ^([A-Za-z0-9.-]*):([0-9]{1,5})$ ]]; then
  die "非法的 --listen 值: $LISTEN（格式应为 host:port，例如 127.0.0.1:8080）"
fi
LISTEN_HOST="${BASH_REMATCH[1]}"
LISTEN_PORT="${BASH_REMATCH[2]}"
if (( 10#$LISTEN_PORT < 1 || 10#$LISTEN_PORT > 65535 )); then
  die "非法的 --listen 端口: $LISTEN_PORT（范围 1-65535）"
fi
if [[ -z "$LISTEN_HOST" || "$LISTEN_HOST" == "localhost" ]]; then
  LISTEN_HOST="127.0.0.1"
fi
if [[ -n "$ADMIN_PASSWORD_HASH" ]]; then
  [[ "$ADMIN_PASSWORD_HASH" =~ ^\$argon2id\$ ]] || die "--admin-password-hash 必须是 Argon2id PHC 哈希（以 \$argon2id\$ 开头）"
fi
if [[ "$SKIP_ADMIN" == 1 && -n "$ADMIN_PASSWORD_HASH" ]]; then
  warn "--skip-admin 与 --admin-password-hash 同时给出，将忽略哈希"
  ADMIN_PASSWORD_HASH=""
fi

DEPLOY_DIR="$(realpath "$DEPLOY_DIR")"
CONFIG_DIR="$DEPLOY_DIR/config"
DATA_DIR="$DEPLOY_DIR/data"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
ENV_FILE="$DEPLOY_DIR/.env"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.yml"

mkdir -p "$CONFIG_DIR" "$DATA_DIR"

# 只读取脚本自己管理的白名单字段；不得 source 可被外部修改的 .env。
read_env_value() {
  local wanted="$1" line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" == *=* ]] || continue
    key="${line%%=*}"
    [[ "$key" == "$wanted" ]] || continue
    value="${line#*=}"
    if [[ ${#value} -ge 2 && "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    elif [[ ${#value} -ge 2 && "$value" == \"*\" ]]; then
      value="${value:1:${#value}-2}"
    fi
    printf '%s' "$value"
    return 0
  done <"$ENV_FILE"
  return 1
}

if [[ -f "$ENV_FILE" ]]; then
  stored_credential_key="$(read_env_value AETHERRELAY_CREDENTIAL_KEY || true)"
  if [[ -n "$stored_credential_key" ]]; then
    AETHERRELAY_CREDENTIAL_KEY="$stored_credential_key"
  fi
  if [[ -z "$ADMIN_PASSWORD_HASH" && "$SKIP_ADMIN" != 1 ]]; then
    ADMIN_PASSWORD_HASH="$(read_env_value AETHERRELAY_ADMIN_PASSWORD_HASH || true)"
  fi
fi
if [[ -n "$ADMIN_PASSWORD_HASH" ]]; then
  [[ "$ADMIN_PASSWORD_HASH" =~ ^\$argon2id\$ ]] || die "$ENV_FILE 中的 AETHERRELAY_ADMIN_PASSWORD_HASH 不是有效的 Argon2id PHC 哈希"
fi

echo "==> 部署目录: $DEPLOY_DIR"
echo "==> 数据目录: $DATA_DIR"
echo "==> 镜像: $IMAGE"

# Provider 与账号池凭据使用该外部主密钥加密后写入 DuckDB。已有部署保留
# .env 中的密钥；新部署只生成一次，密钥本身不进入 config.yaml 或数据库。
if [[ -z "${AETHERRELAY_CREDENTIAL_KEY:-}" ]]; then
  command -v base64 >/dev/null 2>&1 || die "生成凭据主密钥需要 base64 命令"
  AETHERRELAY_CREDENTIAL_KEY="$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n')"
  [[ -n "$AETHERRELAY_CREDENTIAL_KEY" ]] || die "生成凭据主密钥失败"
  echo "    -> 已生成 DuckDB 凭据加密主密钥"
fi

# 1. 生成 config.yaml（已有则保留，不覆盖用户改动）
if [[ -f "$CONFIG_FILE" ]]; then
  warn "检测到已有 $CONFIG_FILE，保留原样"
  warn "若未配置 Admin 登录，可运行: ${COMPOSE[*]} exec aetherrelay admin set-credentials --username ops-admin --config /etc/aetherrelay/config.yaml"
else
  if [[ -f "$REPO_ROOT/config.example.yaml" ]]; then
    cp "$REPO_ROOT/config.example.yaml" "$CONFIG_FILE"
  else
    # 脚本脱离仓库使用时，从镜像内模板生成。
    "$DOCKER" run --rm "$IMAGE" cat /usr/share/aetherrelay/config.example.yaml >"$CONFIG_FILE"
  fi
  # 容器内固定监听全部网卡（由宿主机端口映射控制暴露面），状态落到宿主机数据目录映射点。
  sed -i 's|^  listen_addr:.*|  listen_addr: 0.0.0.0:8080|' "$CONFIG_FILE"
  sed -i 's|^  dir: .*|  dir: /var/lib/aetherrelay|' "$CONFIG_FILE"
  if [[ "$SKIP_ADMIN" != 1 ]]; then
    # 在 server 段末尾（slo_violation_webhook 之后）插入 Admin 登录配置。
    # 哈希经 ${AETHERRELAY_ADMIN_PASSWORD_HASH} 由 .env 注入，不写入 YAML 明文。
    sed -i "/^  slo_violation_webhook:/a\\
  admin_auth_enabled: true\\
  admin_username: \"$ADMIN_USERNAME\"\\
  admin_password_hash: \${AETHERRELAY_ADMIN_PASSWORD_HASH}" "$CONFIG_FILE"
    echo "    -> 已启用 Admin 登录（用户名: $ADMIN_USERNAME）"
  fi
  echo "    -> 已生成 $CONFIG_FILE"
fi

# 2. Admin 密码哈希：优先 --admin-password-hash，其次交互式生成。
# stdout 在容器内写入仅当前用户可读的临时文件，stderr 的密码提示则直接显示
# 在当前 TTY；避免用命令替换吞掉提示，让用户误以为脚本卡住。
if [[ "$SKIP_ADMIN" != 1 && -z "$ADMIN_PASSWORD_HASH" ]]; then
  if [[ -n "${AETHERRELAY_ADMIN_PASSWORD_HASH:-}" ]]; then
    ADMIN_PASSWORD_HASH="$AETHERRELAY_ADMIN_PASSWORD_HASH"
  elif [[ -t 0 && -t 1 ]]; then
    HASH_DIR="$(mktemp -d)"
    hash_file="$HASH_DIR/value"
    chmod 700 "$HASH_DIR"
    echo "==> 设置 Admin 登录密码（输入不回显，请输入两次）"
    if ! "$DOCKER" run --rm -it \
      --user "$(id -u):$(id -g)" \
      --entrypoint /bin/sh \
      -v "$HASH_DIR:/run/aetherrelay-password-hash:rw" \
      "$IMAGE" -c \
      'umask 077; exec /usr/local/bin/AetherRelay admin password-hash > /run/aetherrelay-password-hash/value'; then
      die "生成 Admin 密码哈希失败"
    fi
    ADMIN_PASSWORD_HASH="$(tr -d '\r\n' <"$hash_file")"
    rm -rf "$HASH_DIR"
    HASH_DIR=""
    [[ "$ADMIN_PASSWORD_HASH" =~ ^\$argon2id\$ ]] || die "password-hash 未生成有效的 Argon2id 哈希"
    echo "    -> Admin 密码已设置"
  else
    die "非交互环境无法生成 Admin 哈希；请用 --admin-password-hash 提供，或 --skip-admin 跳过"
  fi
fi

# 3. 写入 .env（chmod 600；值加单引号供 Compose 插值读取）
{
  echo "AETHERRELAY_IMAGE='$IMAGE'"
  echo "AETHERRELAY_CREDENTIAL_KEY='$AETHERRELAY_CREDENTIAL_KEY'"
  if [[ "$SKIP_ADMIN" != 1 ]]; then
    echo "AETHERRELAY_ADMIN_PASSWORD_HASH='$ADMIN_PASSWORD_HASH'"
  fi
} >"$ENV_FILE"
chmod 600 "$ENV_FILE"
echo "    -> 已写入 $ENV_FILE (chmod 600)"

# 4. 生成 docker-compose.yml（自包含）
BIND="$LISTEN_HOST:$LISTEN_PORT:8080"
if [[ "$LISTEN_HOST" == 127.* ]]; then
  ACCESS_HOST="$LISTEN_HOST"
else
  ACCESS_HOST="127.0.0.1"
  warn "非 loopback 监听：请确认已启用 Admin 登录（否则管理台仅 loopback），并由防火墙/反向代理保护数据面"
fi

cat >"$COMPOSE_FILE" <<EOF
services:
  aetherrelay:
    image: \${AETHERRELAY_IMAGE:-$IMAGE}
    restart: unless-stopped
    ports:
      - "$BIND"
    environment:
      AETHERRELAY_CONFIG: /etc/aetherrelay/config.yaml
      AETHERRELAY_CREDENTIAL_KEY: \${AETHERRELAY_CREDENTIAL_KEY:?set AETHERRELAY_CREDENTIAL_KEY in .env}
      AETHERRELAY_ADMIN_PASSWORD_HASH: \${AETHERRELAY_ADMIN_PASSWORD_HASH:-}
    volumes:
      # 目录挂载（而非单文件）：管理页保存配置时通过临时文件原子替换。
      - ./config:/etc/aetherrelay
      - ./data:/var/lib/aetherrelay
EOF
echo "    -> 已生成 $COMPOSE_FILE"

# 5. 持久化目录交由容器内 UID/GID 10001（aetherrelay 用户）持有（尽力而为）
if [[ -w "$DEPLOY_DIR" ]]; then
  chown -R 10001:10001 "$CONFIG_DIR" "$DATA_DIR" 2>/dev/null \
    || warn "无法 chown 配置与数据目录为 10001:10001；请手动修正目录权限"
fi

# 6. 拉取镜像、启动并等待就绪
echo "==> 拉取镜像并启动容器"
"${COMPOSE[@]}" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" pull
"${COMPOSE[@]}" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" up -d

echo "==> 等待服务就绪"
READY=0
for _ in $(seq 1 30); do
  if "${COMPOSE[@]}" -f "$COMPOSE_FILE" exec -T aetherrelay curl --fail -s http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done
if [[ "$READY" != 1 ]]; then
  warn "服务未在预期时间内就绪，请检查: ${COMPOSE[*]} -f $COMPOSE_FILE logs -f"
fi

# 7. 结果提示
echo ""
echo "=================================================="
echo "部署完成"
echo "  管理台:      http://$ACCESS_HOST:$LISTEN_PORT/admin/"
echo "  健康检查:    curl http://$ACCESS_HOST:$LISTEN_PORT/healthz"
echo "  查看日志:    ${COMPOSE[*]} -f $COMPOSE_FILE logs -f"
echo "  持久化数据:  $DATA_DIR"
echo "  Provider:    登录管理台后在 Provider 管理中添加；账号池凭据在账号池页面导入"
if [[ "$SKIP_ADMIN" != 1 ]]; then
  echo "  Admin 账号:  $ADMIN_USERNAME（默认仅本机可登录；经 HTTPS 反向代理部署时，"
  echo "                请在 config.yaml 开启 admin_session_cookie_secure: true）"
else
  echo "  注意:        未启用 Admin 登录（--skip-admin）；如需启用请重跑本脚本，或"
  echo "                ${COMPOSE[*]} exec aetherrelay admin set-credentials --config /etc/aetherrelay/config.yaml"
fi
echo "  变更密码:    重跑本脚本（交互生成新哈希）后重启容器，或更新 $ENV_FILE 中的"
echo "                AETHERRELAY_ADMIN_PASSWORD_HASH 后 docker compose up -d"
echo "=================================================="
