#!/usr/bin/env bash
# =============================================================================
# 生成生产部署 .env —— 一条命令补齐全部密钥（强随机），零手工编辑即可部署
#
# 用法:
#   bash scripts/gen-prod-env.sh            # .env 不存在时生成
#   bash scripts/gen-prod-env.sh --force    # 覆盖已有 .env（危险，见下）
#
# 生成后唯一需要人工填的是 EGRESS_IPS（与机器网卡绑定，无法代填）。
#
# ⚠️ --force 会重写全部密钥:
#   - POSTGRES_PASSWORD 换掉后，旧数据卷的账号认证立即失效
#   - FLUXKEYS_ENCRYPTION_KEY 换掉后，已导入的火山 Key 全部解不开（不可恢复）
#   确需重来请先备份数据卷，或删卷重建: docker compose down -v
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -f .env && "${1:-}" != "--force" ]]; then
  echo "错误: .env 已存在，拒绝覆盖。" >&2
  echo "  - 想改个别参数: 直接编辑 .env（改 REDIS_PASSWORD 等启动参数后需 down 再 up）" >&2
  echo "  - 确要全部重新生成（会作废旧密钥）: bash scripts/gen-prod-env.sh --force" >&2
  exit 1
fi

command -v openssl >/dev/null 2>&1 || { echo "错误: 需要 openssl" >&2; exit 1; }

# 密钥文件权限收紧到 600；umask 保证后续 shell 追加内容也不会意外放宽
umask 077

cat > .env <<EOF
# FluxKeys 生产环境变量 —— 由 scripts/gen-prod-env.sh 生成于 $(date '+%Y-%m-%d %H:%M:%S')
#
# 警告:
#   1. FLUXKEYS_ENCRYPTION_KEY 用于加密火山 Key，一旦导入 Key 后不可更换
#      （需全量解密重加密）。请把本文件备份到密钥管理系统。
#   2. .env 在 .gitignore 中，绝不提交。
#   3. 改 REDIS_PASSWORD / POSTGRES_PASSWORD 等启动参数后，需要
#      docker compose down && up 才生效。

# ---- 密钥（强随机，勿手工替换为弱口令）----
POSTGRES_PASSWORD=$(openssl rand -hex 24)
REDIS_PASSWORD=$(openssl rand -hex 24)
ADMIN_API_KEY=$(openssl rand -hex 32)
DASHBOARD_PASSWORD=$(openssl rand -hex 16)
DASHBOARD_SESSION_SECRET=$(openssl rand -hex 32)
# 32 字节 hex，加密 volc_keys.secret_enc。丢失 = 库中所有火山 Key 永久解不开
FLUXKEYS_ENCRYPTION_KEY=$(openssl rand -hex 32)
GRAFANA_ADMIN_PASSWORD=$(openssl rand -hex 12)

# ---- 镜像与配额 ----
# 升级时改为具体版本号（如 v1.0.0），并 docker pull + tag 后 up（见 compose 文件头）
FLUXKEYS_IMAGE_TAG=dev
QUOTA_TOKEN_LIMIT=5000000
QUOTA_TOKEN_HARD=4500000
QUOTA_COUNT_HARD=90

# ---- 启用的 profile: 监控 + 每日备份 ----
# 需要自动 HTTPS 再加 tls，并在下方填 CADDY_DOMAIN
COMPOSE_PROFILES=monitoring,backup

# ==== 以下必须按机器实际情况填写（无法代填）====

# 出口 IP 列表，格式: 本机地址=公网EIP 或 本机地址|档位|max_keys，逗号分隔。
# 生成方法与分层建议见 deploy/README.md 第 1 步。
EGRESS_IPS=

# 对外服务域名（仅启用 tls profile 时需要，ACME 签发用）
# CADDY_DOMAIN=
# ACME_EMAIL=
EOF

chmod 600 .env

echo "已生成 .env（权限 600）。下一步:"
echo "  1. 编辑 .env 填 EGRESS_IPS（机器相关，无法代填；生成方法见 deploy/README.md）"
echo "  2. sudo bash scripts/setup-egress.sh --persist --ips '<同 EGRESS_IPS>'"
echo "  3. docker compose up -d"
echo "  4. bash scripts/smoke-test.sh"
