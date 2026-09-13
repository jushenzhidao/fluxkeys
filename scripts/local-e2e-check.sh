#!/usr/bin/env bash
# 本地端到端验证：真实网关二进制 + 真实 Redis + 真实 PostgreSQL + 假上游。
#
# 假上游的宿主是 test/mockark（测试夹具），由 test/mockark/cmd 以独立进程跑起来 ——
# 这里必须用独立进程而不是进程内启动: 被测对象是「独立进程的网关」，上游若在
# 同一进程内，就验不到网络层（连接、超时、流式 flush）。
#
# 覆盖: 配置加载 → 迁移 → provider seed → 出口 direct → 用户鉴权 →
#       调度 → 配额 Lua 预扣 → 上游转发 → 用量落库。
# 不产生的副作用: 不访问任何真实上游域名，用独立 Redis DB(14) 与独立端口。

set -uo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"

export PATH="$HOME/go/bin:$PATH"
# curl 不会自动绕过回环: 不设 NO_PROXY 时本地请求会被 HTTP_PROXY 代理走，
# 返回的是网关错误体而不是 connection refused，极具误导性。
export NO_PROXY="localhost,127.0.0.1,::1"
export no_proxy="$NO_PROXY"

# 端口必须动态取空闲值。
#
# 写死端口在本机实测直接失效: 127.0.0.1:18080 被另一个项目的容器占着，
# 网关 bind 失败退出，而脚本的「就绪探测」打到了那个容器上并拿到 200 ——
# 于是后续所有断言都在对着一个完全无关的服务执行。
pick_port() {
    python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}
GW_ADDR="127.0.0.1:$(pick_port)"
MOCK_ADDR="127.0.0.1:$(pick_port)"
METRICS_PORT="$(pick_port)"
GW_URL="http://${GW_ADDR}"
ADMIN_KEY="test-admin-key"
REDIS_DB=14
PG_PORT=15434

# 上游 Key 的密文存储需要 32 字节 hex 密钥；缺失时 store 会拒绝启动
# （这是刻意的：宁可起不来也不明文存密钥）。本地测试用一次性随机值即可。
export FLUXKEYS_ENCRYPTION_KEY="$(python3 -c 'import os;print(os.urandom(32).hex())')"

FAILS=0
ok()   { echo "✅ $*"; }
bad()  { echo "❌ $*"; FAILS=$((FAILS+1)); }
chk()  { if [ "$2" = "0" ]; then ok "$1"; else bad "$1"; fi; }
cleanup() {
    [ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
    [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null
    wait 2>/dev/null
}
trap cleanup EXIT

echo "=== 0. 构建 ==="
go1.25.0 build -o /tmp/fk-mockark ./test/mockark/cmd || exit 1
go1.25.0 build -o /tmp/fk-gateway ./cmd/gateway || exit 1
ok "已构建假上游（test/mockark/cmd）与 gateway"

echo
echo "=== 1. 启动假上游（test/mockark/cmd）==="
/tmp/fk-mockark -addr "$MOCK_ADDR" -token-limit 100000 -count-limit 1000 >/tmp/fk-mockark.log 2>&1 &
MOCK_PID=$!
for i in $(seq 1 20); do
    curl -fsS -m 2 "http://${MOCK_ADDR}/_mock/stats" >/dev/null 2>&1 && break
    sleep 0.3
done
curl -fsS -m 2 "http://${MOCK_ADDR}/_mock/stats" >/dev/null 2>&1 \
    && ok "假上游已就绪 ${MOCK_ADDR}" || { bad "假上游未就绪"; cat /tmp/fk-mockark.log; exit 1; }

echo
echo "=== 2. 准备配置 ==="
cat > /tmp/fk-e2e.yaml <<EOF
server:
  addr: "${GW_ADDR}"
  metrics_addr: "127.0.0.1:${METRICS_PORT}"
redis:
  addr: "127.0.0.1:6379"
  db: ${REDIS_DB}
postgres:
  dsn: "host=127.0.0.1 port=${PG_PORT} user=fluxkeys password=fluxkeys dbname=fluxkeys sslmode=disable"
  auto_migrate: true
providers:
  volc:
    base_url: "http://${MOCK_ADDR}"
    quota_kind: "token"
    quota_limit: 100000
    quota_window: 24h
    refresh_hour: 12
    model_mapping:
      "gpt-4": "mock-model"
    count_models:
      - "mock-count-model"
    reasoning_models: []
upstream:
  max_retries: 2
  retry_base_delay: 100ms
  retry_jitter: 50ms
scheduler:
  weight_quota: 35
  weight_history: 25
  weight_persona: 20
  weight_health: 15
  soft_penalty: 20
  active_pool_size: 100
  enable_persona: false
  min_request_interval: 0s
  pool_shares: {}
egress:
  mode: "direct"
  verify_on_start: false
refresh:
  enabled: false
admin:
  api_key: "${ADMIN_KEY}"
EOF
ok "已生成 /tmp/fk-e2e.yaml"

echo
echo "=== 3. 启动网关 ==="
/tmp/fk-gateway -config /tmp/fk-e2e.yaml >/tmp/fk-gateway.log 2>&1 &
GW_PID=$!
ready=0
for i in $(seq 1 40); do
    # 必须校验响应体形状，不能只看「有 200」。
    # 端口被别的服务占用时，健康探测会对着那个服务拿到 200，
    # 于是整轮断言都在测一个与本次部署无关的进程。
    if curl -fsS -m 2 "${GW_URL}/readyz" 2>/dev/null | grep -q '"ready"'; then
        ready=1; break
    fi
    kill -0 "$GW_PID" 2>/dev/null || break
    sleep 0.5
done
if [ "$ready" != "1" ]; then
    bad "网关未就绪"
    echo "--- 日志 ---"; tail -25 /tmp/fk-gateway.log
    exit 1
fi
ok "网关已就绪（配置加载 + 迁移 + provider seed 全部通过）"
curl -sS -m 5 "${GW_URL}/readyz" | python3 -m json.tool | sed 's/^/    /'

echo
echo "=== 4. 导入上游 Key ==="
imp=$(curl -sS -m 20 -X POST "${GW_URL}/admin/keys" \
    -H "Authorization: Bearer ${ADMIN_KEY}" -H "Content-Type: application/json" \
    -d '[{"provider":"volc","key_id":"k1","secret":"sk-local-e2e","pool":"hot","status":"active"}]')
echo "$imp" | python3 -m json.tool | sed 's/^/    /'
[ "$(echo "$imp" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("imported_count"))')" = "1" ] \
    && ok "Key 已导入" || bad "Key 导入失败"

echo
echo "=== 5. 签发用户 API Key ==="
u=$(curl -sS -m 15 -X POST "${GW_URL}/admin/users" -H "Authorization: Bearer ${ADMIN_KEY}" \
    -H "Content-Type: application/json" -d '{"name":"local-e2e"}')
UID_=$(echo "$u" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
k=$(curl -sS -m 15 -X POST "${GW_URL}/admin/users/${UID_}/keys" -H "Authorization: Bearer ${ADMIN_KEY}" \
    -H "Content-Type: application/json" -d '{}')
UKEY=$(echo "$k" | python3 -c 'import sys,json;print(json.load(sys.stdin)["api_key"])')
[ -n "$UKEY" ] && ok "已签发用户 Key (user_id=${UID_})" || { bad "签发失败"; echo "$k"; exit 1; }

echo
echo "=== 6. 非流式请求（经网关 → 假上游）==="
resp=$(curl -sS -m 60 -w '\n%{http_code}' -X POST "${GW_URL}/v1/chat/completions" \
    -H "Authorization: Bearer ${UKEY}" -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"你好"}],"stream":false}')
code=$(echo "$resp" | tail -n1)
body=$(echo "$resp" | sed '$d')
chk "HTTP 200（实际 ${code}）" "$([ "$code" = "200" ] && echo 0 || echo 1)"
echo "$body" | python3 -m json.tool 2>/dev/null | head -12 | sed 's/^/    /'
chk "响应含 content" "$(echo "$body" | python3 -c 'import sys,json;d=json.load(sys.stdin);sys.exit(0 if d["choices"][0]["message"].get("content") else 1)' && echo 0 || echo 1)"
chk "model 回显对外别名" "$(echo "$body" | python3 -c 'import sys,json;sys.exit(0 if json.load(sys.stdin)["model"]=="gpt-4" else 1)' && echo 0 || echo 1)"

echo
echo "=== 7. 流式请求 ==="
sout=$(curl -sS -N -m 60 -X POST "${GW_URL}/v1/chat/completions" \
    -H "Authorization: Bearer ${UKEY}" -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"数到3"}],"stream":true}' 2>&1)
ndata=$(echo "$sout" | grep -c '^data:')
chk "收到 SSE data 行（${ndata} 行）" "$([ "${ndata:-0}" -gt 0 ] && echo 0 || echo 1)"
chk "以 [DONE] 收尾" "$(echo "$sout" | grep -q '\[DONE\]' && echo 0 || echo 1)"

echo
echo "=== 8. 按次计费闭环（Redis 实扣 == 流水 count_units）==="
cresp=$(curl -sS -m 60 -w '\n%{http_code}' -X POST "${GW_URL}/v1/chat/completions" \
    -H "Authorization: Bearer ${UKEY}" -H "Content-Type: application/json" \
    -d '{"model":"mock-count-model","messages":[{"role":"user","content":"hi"}],"stream":false}')
ccode=$(echo "$cresp" | tail -n1)
chk "按次模型请求 200（实际 ${ccode}）" "$([ "$ccode" = "200" ] && echo 0 || echo 1)"

# Redis 侧实扣
rused=$(docker exec fluxkeys-test-postgres-1 true 2>/dev/null; \
    python3 - <<'PY'
import socket
s=socket.create_connection(("127.0.0.1",6379),timeout=3)
def cmd(*a):
    out=b"*%d\r\n"%len(a)
    for x in a:
        x=x.encode() if isinstance(x,str) else x
        out+=b"$%d\r\n%s\r\n"%(len(x),x)
    s.sendall(out); import time; time.sleep(0.15)
    return s.recv(65536).decode(errors="replace")
cmd("SELECT",str(14))
keys=cmd("KEYS","*quota:count:k1*")
print(keys.strip().splitlines()[-1] if keys.strip() else "")
PY
)
echo "    Redis count 配额键: ${rused:-（未找到）}"
if [ -n "$rused" ]; then
    h=$(python3 - <<PY
import socket,time
s=socket.create_connection(("127.0.0.1",6379),timeout=3)
def cmd(*a):
    out=b"*%d\r\n"%len(a)
    for x in a:
        x=x.encode() if isinstance(x,str) else x
        out+=b"$%d\r\n%s\r\n"%(len(x),x)
    s.sendall(out); time.sleep(0.15)
    return s.recv(65536).decode(errors="replace")
cmd("SELECT","14")
print(cmd("HGET","${rused}","used").strip().splitlines()[-1])
PY
)
    echo "    Redis used = ${h}"
    # 流水侧
    recs=$(docker exec fluxkeys-test-postgres-1 psql -U fluxkeys -d fluxkeys -tAc \
        "select count_units, status_code, provider, model from usage_records where billing_kind='count' order by id desc limit 5" 2>/dev/null)
    echo "    流水 (count_units|status|provider|model):"
    echo "$recs" | sed 's/^/      /'
    sum=$(echo "$recs" | awk -F'|' '{s+=$1} END {print s+0}')
    chk "按次流水 count_units 合计 = ${sum} > 0（不再恒为 0）" "$([ "${sum:-0}" -gt 0 ] && echo 0 || echo 1)"
    chk "Redis 实扣(${h}) == 流水合计(${sum})" "$([ "${h:-x}" = "${sum:-y}" ] && echo 0 || echo 1)"
else
    bad "未找到按次配额键"
fi

echo
echo "=== 9. 用量流水（token 型）==="
docker exec fluxkeys-test-postgres-1 psql -U fluxkeys -d fluxkeys -tAc \
    "select provider, model, billing_kind, count_units, total_tokens, status_code, retry_count from usage_records order by id desc limit 6" 2>/dev/null | sed 's/^/    /'

echo
echo "=== 10. 调度分布 ==="
curl -sS -m 15 -H "Authorization: Bearer ${ADMIN_KEY}" "${GW_URL}/admin/keys" \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print("   ",{"total":d["total"],"active":d["active"],"providers":sorted({k.get("provider") for k in d["keys"]}),"first":{kk:d["keys"][0].get(kk) for kk in ("key_id","provider","status","quota_token_used","quota_count_used")} if d["keys"] else None})'

echo
if [ "$FAILS" -eq 0 ]; then echo "✅ 本地端到端全部通过"; else echo "❌ ${FAILS} 项失败"; fi
exit "$FAILS"
