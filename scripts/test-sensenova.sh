#!/bin/bash
set -e

# SenseNova 真实上游测试脚本
# 用途：导入真实的 SenseNova keys 并测试完整调用链路
#
# 位置说明：keys 文件原本取自本仓的 real_upstream_test/keys.txt。
# 该目录已于 2026-09-13 迁出本仓，现位于同级的 livetest-ai 项目，
# 故此处按「脚本位置相对解析」，不再依赖调用方的当前工作目录。
# 需要改指别处时：export FLUXKEYS_KEYS_FILE=/path/to/keys.txt

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

KEYS_FILE="${FLUXKEYS_KEYS_FILE:-$REPO_ROOT/../livetest-ai/real_upstream_test/keys.txt}"
if [ ! -f "$KEYS_FILE" ]; then
  echo "❌ 找不到 keys 文件：$KEYS_FILE"
  echo "   该文件原属本仓 real_upstream_test/，现已迁至 livetest-ai 项目。"
  echo "   请确认同级存在 livetest-ai/real_upstream_test/keys.txt，"
  echo "   或用 FLUXKEYS_KEYS_FILE 显式指定路径。"
  exit 1
fi
echo "keys 文件: $KEYS_FILE"

GATEWAY_URL="${FLUXKEYS_GATEWAY_URL:-http://localhost:8080}"
ADMIN_KEY="${FLUXKEYS_ADMIN_KEY:-test-admin-key}"

echo "==> 1. 批量导入 SenseNova keys"
KEYS_JSON="["
first=true
while IFS= read -r key || [ -n "$key" ]; do
  # 跳过空行
  [ -z "$key" ] && continue

  # 跳过已处理的最后一行（可能没有换行符）
  key=$(echo "$key" | tr -d '\r\n')
  [ -z "$key" ] && continue

  # 生成 key_id（使用前8位作为标识）
  prefix=$(echo "$key" | cut -c 1-8)
  key_id="sense_${prefix}"

  if [ "$first" = true ]; then
    first=false
  else
    KEYS_JSON+=","
  fi

  KEYS_JSON+="{\"provider\":\"sensenova\",\"key_id\":\"$key_id\",\"secret\":\"$key\",\"pool\":\"hot\"}"
done < "$KEYS_FILE"
KEYS_JSON+="]"

echo "导入 JSON:"
echo "$KEYS_JSON" | jq .

curl -X POST "$GATEWAY_URL/admin/keys/import" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d "$KEYS_JSON" \
  | jq .

echo ""
echo "==> 2. 查看已导入的 keys"
curl -s "$GATEWAY_URL/admin/keys" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  | jq '.keys[] | select(.provider == "sensenova")'

echo ""
echo "==> 3. 发起测试请求（非流式）"
curl -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer test-user-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-r1",
    "messages": [{"role": "user", "content": "1+1=?"}],
    "max_tokens": 50
  }' \
  | jq .

echo ""
echo "==> 4. 发起流式请求测试"
curl -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer test-user-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-r1",
    "messages": [{"role": "user", "content": "2+2=?"}],
    "max_tokens": 50,
    "stream": true
  }'

echo ""
echo "==> 5. 检查配额使用情况"
curl -s "$GATEWAY_URL/admin/keys" \
  -H "X-Admin-Key: $ADMIN_KEY" \
  | jq '.keys[] | select(.provider == "sensenova") | {key_id, quota_used, quota_limit, last_used_at}'

echo ""
echo "✅ 测试完成"
