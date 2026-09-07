#!/bin/bash
set -e

# SenseNova 真实上游测试脚本
# 用途：导入真实的 SenseNova keys 并测试完整调用链路

GATEWAY_URL="http://localhost:8080"
ADMIN_KEY="test-admin-key"

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
done < real_upstream_test/keys.txt
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
