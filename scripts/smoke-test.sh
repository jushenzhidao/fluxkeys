#!/usr/bin/env bash
# =============================================================================
# smoke-test.sh —— compose 起来之后的端到端冒烟测试
# =============================================================================
#
# 目的：确认「docker compose up 之后系统真的能用」，而不只是容器 Running。
# 容器健康检查只能说明进程活着，说明不了请求能不能跑通全链路
# （网关 → 调度 → 配额 Lua → Egress → mockark → 回写用量）。
#
# 用法:
#   bash scripts/smoke-test.sh                       # 用默认地址
#   GATEWAY_URL=http://1.2.3.4:8080 bash scripts/smoke-test.sh
#   SKIP_DASHBOARD=1 bash scripts/smoke-test.sh      # 跳过看板检查
#
# 退出码: 0 全部通过；1 有失败项。可直接用于 CI 门禁。
#
# 依赖: curl。jq 可选（有则做更严格的 JSON 断言，无则退化为字符串匹配）。
# =============================================================================

set -euo pipefail

# -----------------------------------------------------------------------------
# 禁用 HTTP 代理
#
# 冒烟测试打的都是本机/内网服务，走代理没有意义且会造成假失败：
# 很多环境（公司网络、开发机）设了全局 HTTP_PROXY，而代理无法访问
# 127.0.0.1 上的服务，curl 会收到 502 —— 看起来像"网关挂了"，
# 实际是代理拦了。实测踩过这个坑，所以在脚本入口直接清掉。
#
# 若确实需要通过代理访问远端被测环境，用 SMOKE_USE_PROXY=1 保留代理设置。
# -----------------------------------------------------------------------------
# CURL_NOPROXY 会拼进每个 curl 调用。用 --noproxy 而不是只 unset 环境变量，
# 是因为 ~/.curlrc 里也可能配了 proxy，那时 unset 无效。
CURL_NOPROXY=()
if [[ "${SMOKE_USE_PROXY:-0}" != "1" ]]; then
  unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
  CURL_NOPROXY=(--noproxy '*')
fi

# -----------------------------------------------------------------------------
# 配置（均可用环境变量覆盖，便于指向远端环境）
# -----------------------------------------------------------------------------
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8080}"
METRICS_URL="${METRICS_URL:-http://127.0.0.1:9090}"
DASHBOARD_URL="${DASHBOARD_URL:-http://127.0.0.1:8000}"

# 管理接口密钥。优先环境变量，其次从 .env 精确抓取。
# 不 source .env —— 那会把所有密钥灌进当前 shell 环境，是不必要的暴露面。
ADMIN_API_KEY="${ADMIN_API_KEY:-}"
if [[ -z "$ADMIN_API_KEY" ]]; then
  ENV_FILE="$(dirname "$0")/../.env"
  if [[ -f "$ENV_FILE" ]]; then
    ADMIN_API_KEY="$(awk -F= '/^[[:space:]]*ADMIN_API_KEY[[:space:]]*=/ {
                       sub(/^[^=]*=/, "");
                       gsub(/^[[:space:]]*|[[:space:]]*$/, "");
                       gsub(/^"|"$/, "");
                       print; exit
                     }' "$ENV_FILE")"
  fi
fi

# 测试用模型名。mockark 应当接受任意模型名，这里用架构里提到的默认探测模型。
TEST_MODEL="${TEST_MODEL:-deepseek-v3}"

# 单个请求的超时。流式请求给更长时间。
CURL_TIMEOUT="${CURL_TIMEOUT:-30}"
STREAM_TIMEOUT="${STREAM_TIMEOUT:-60}"

SKIP_DASHBOARD="${SKIP_DASHBOARD:-0}"
SKIP_METRICS="${SKIP_METRICS:-0}"

# -----------------------------------------------------------------------------
# 计数与输出
# -----------------------------------------------------------------------------
PASS=0
FAIL=0
SKIP=0
declare -a FAILED_CASES=()

pass() { printf '  \033[0;32m✓\033[0m %s\n' "$*"; PASS=$((PASS + 1)); }
skip() { printf '  \033[0;90m-\033[0m %s \033[0;90m(跳过: %s)\033[0m\n' "$1" "$2"; SKIP=$((SKIP + 1)); }
info() { printf '    \033[0;90m%s\033[0m\n' "$*"; }

fail() {
  printf '  \033[0;31m✗\033[0m %s\n' "$1"
  [[ $# -gt 1 ]] && printf '    \033[0;31m%s\033[0m\n' "$2"
  FAIL=$((FAIL + 1))
  FAILED_CASES+=("$1")
}

section() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# jq 可选
HAS_JQ=false
command -v jq >/dev/null 2>&1 && HAS_JQ=true

# -----------------------------------------------------------------------------
# 等待网关就绪
#
# CI 里 compose --wait 已经等过 healthcheck，但本地手工执行时用户可能刚
# up 完就跑脚本。多等一会儿比直接报错友好。
# -----------------------------------------------------------------------------
wait_for_gateway() {
  section "等待网关就绪"
  local i code
  for i in $(seq 1 60); do
    # 注意不能写成 `$(curl ... -w '%{http_code}' || echo 000)`：
    # 连接失败时 curl 仍会输出 "000" 再返回非零，两者拼成 "000000"。
    # 这里用 `|| true` 让 curl 自己的输出成为唯一来源。
    code="$(curl -sS "${CURL_NOPROXY[@]}" -o /dev/null -w '%{http_code}' --max-time 3 \
            "${GATEWAY_URL}/healthz" 2>/dev/null || true)"
    code="${code:-000}"
    if [[ "$code" == "200" ]]; then
      pass "网关已就绪（等待 ${i}s）"
      return 0
    fi
    sleep 1
  done
  fail "网关在 60s 内未就绪" "最后一次 /healthz 返回 $code"
  # 就绪失败后续用例没有意义，直接给出诊断建议后退出
  cat >&2 <<EOF

诊断:
  docker compose ps
  docker compose logs --tail=100 gateway

常见原因:
  - FLUXKEYS_ENCRYPTION_KEY 未设置或不是 32 字节 hex（网关会拒绝启动）
  - Postgres/Redis 未就绪，网关连接失败退出
  - 端口被占用，改 .env 里的 GATEWAY_HOST_PORT
EOF
  exit 1
}

# =============================================================================
# 1. 健康检查
# =============================================================================
test_health() {
  section "1. 健康与就绪检查"

  local code
  code="$(curl -sS "${CURL_NOPROXY[@]}" -o /dev/null -w '%{http_code}' --max-time 5 \
          "${GATEWAY_URL}/healthz" 2>/dev/null || true)"
  code="${code:-000}"
  if [[ "$code" == "200" ]]; then
    pass "GET /healthz -> 200"
  else
    fail "GET /healthz -> ${code}（期望 200）"
  fi

  # readyz 会检查 Redis + Postgres 可达。它和 healthz 的区别很关键：
  # healthz 200 但 readyz 503 说明进程活着但依赖断了 —— 这时网关不该收流量。
  local body
  body="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time 10 \
          "${GATEWAY_URL}/readyz" 2>/dev/null || true)"
  code="$(printf '%s' "$body" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000
  if [[ "$code" == "200" ]]; then
    pass "GET /readyz -> 200（Redis 与 Postgres 均可达）"
  else
    fail "GET /readyz -> ${code}（期望 200）" "$(printf '%s' "$body" | head -3)"
  fi
}

# =============================================================================
# 2. 非流式请求 —— 全链路最核心的一条
# =============================================================================
test_non_stream() {
  section "2. 非流式对话请求"

  local resp code body
  resp="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time "$CURL_TIMEOUT" \
          -X POST "${GATEWAY_URL}/v1/chat/completions" \
          -H 'Content-Type: application/json' \
          -H "Authorization: Bearer ${USER_API_KEY:-fk-smoke-test}" \
          -d "{
                \"model\": \"${TEST_MODEL}\",
                \"messages\": [{\"role\": \"user\", \"content\": \"ping\"}],
                \"max_tokens\": 16,
                \"stream\": false
              }" 2>/dev/null || true)"

  code="$(printf '%s' "$resp" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000
  body="$(printf '%s' "$resp" | sed '$d')"

  case "$code" in
    200)
      pass "POST /v1/chat/completions -> 200"
      # 断言响应结构符合 OpenAI 格式。只看 HTTP 200 不够 ——
      # 返回一个空壳 JSON 也是 200，但客户端会解析失败。
      if [[ "$HAS_JQ" == true ]]; then
        local content finish
        content="$(printf '%s' "$body" | jq -r '.choices[0].message.content // empty' 2>/dev/null)"
        finish="$(printf '%s' "$body" | jq -r '.choices[0].finish_reason // empty' 2>/dev/null)"
        if [[ -n "$content" ]]; then
          pass "响应含 choices[0].message.content"
          info "内容: $(printf '%s' "$content" | head -c 60)"
        else
          fail "响应缺少 choices[0].message.content" "$(printf '%s' "$body" | head -c 200)"
        fi
        [[ -n "$finish" ]] && info "finish_reason: $finish"

        # usage 是配额修正的依据（P0-2 的 quota_commit 用它）。
        # 缺失会导致预扣永远按估算值结算，配额统计逐渐失真。
        local total
        total="$(printf '%s' "$body" | jq -r '.usage.total_tokens // empty' 2>/dev/null)"
        if [[ -n "$total" && "$total" != "0" ]]; then
          pass "响应含 usage.total_tokens=$total"
        else
          fail "响应缺少有效 usage.total_tokens" "配额修正依赖此字段，缺失会导致预扣无法结算"
        fi
      else
        if printf '%s' "$body" | grep -q '"choices"'; then
          pass "响应含 choices 字段"
        else
          fail "响应不含 choices 字段" "$(printf '%s' "$body" | head -c 200)"
        fi
        skip "usage 字段严格校验" "未安装 jq"
      fi
      ;;
    401|403)
      # 鉴权失败说明链路是通的，只是没有有效用户 Key。
      # 这在全新部署时是预期情况（还没建用户），算通过但要提示。
      pass "POST /v1/chat/completions -> ${code}（鉴权生效，链路可达）"
      info "尚未创建用户 API Key。设置 USER_API_KEY 环境变量后重跑可验证完整链路"
      info "创建方式见 deploy/README.md「初始化用户」"
      ;;
    000)
      fail "POST /v1/chat/completions 连接失败" "网关不可达或超时（${CURL_TIMEOUT}s）"
      ;;
    *)
      fail "POST /v1/chat/completions -> $code" "$(printf '%s' "$body" | head -c 300)"
      ;;
  esac
}

# =============================================================================
# 3. 流式请求
#
# 流式是最容易出问题的路径：SSE 需要正确的 chunked 传输、及时 flush、
# 以及在客户端断连时释放租约（P0-2 的泄漏来源就在这里）。
# =============================================================================
test_stream() {
  section "3. 流式对话请求（SSE）"

  local tmp
  tmp="$(mktemp)"
  # shellcheck disable=SC2064
  # 立即展开 tmp 的值，确保 trap 在变量被覆盖后仍能清理正确的文件
  trap "rm -f '$tmp'" RETURN

  local code
  code="$(curl -sS "${CURL_NOPROXY[@]}" -o "$tmp" -w '%{http_code}' --max-time "$STREAM_TIMEOUT" \
          -X POST "${GATEWAY_URL}/v1/chat/completions" \
          -H 'Content-Type: application/json' \
          -H 'Accept: text/event-stream' \
          -H "Authorization: Bearer ${USER_API_KEY:-fk-smoke-test}" \
          -d "{
                \"model\": \"${TEST_MODEL}\",
                \"messages\": [{\"role\": \"user\", \"content\": \"count to three\"}],
                \"max_tokens\": 32,
                \"stream\": true
              }" 2>/dev/null || true)"
  code="${code:-000}"

  case "$code" in
    200)
      pass "POST /v1/chat/completions (stream) -> 200"

      # SSE 格式断言：必须有 data: 行
      local data_lines
      data_lines="$(grep -c '^data: ' "$tmp" 2>/dev/null || echo 0)"
      if [[ "$data_lines" -gt 0 ]]; then
        pass "收到 $data_lines 个 SSE data 帧"
      else
        fail "未收到任何 SSE data 帧" "响应前 200 字节: $(head -c 200 "$tmp")"
      fi

      # [DONE] 终止标记。缺它客户端会一直等，表现为"卡住不返回"。
      if grep -q '^data: \[DONE\]' "$tmp" 2>/dev/null; then
        pass "收到 data: [DONE] 终止标记"
      else
        fail "缺少 data: [DONE] 终止标记" "OpenAI 客户端会因此一直等待流结束"
      fi

      # 至少要有一个 chunk 带内容增量
      if grep -q '"delta"' "$tmp" 2>/dev/null; then
        pass "SSE 帧含 delta 增量字段"
      else
        fail "SSE 帧不含 delta 字段" "格式与 OpenAI 流式协议不兼容"
      fi
      ;;
    401|403)
      pass "POST /v1/chat/completions (stream) -> ${code}（鉴权生效）"
      info "设置 USER_API_KEY 后重跑可验证完整流式链路"
      ;;
    000)
      fail "流式请求连接失败或超时" "超时 ${STREAM_TIMEOUT}s"
      ;;
    *)
      fail "流式请求 -> $code" "$(head -c 300 "$tmp")"
      ;;
  esac
}

# =============================================================================
# 4. 模型列表
# =============================================================================
test_models() {
  section "4. 模型列表"

  local resp code
  resp="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time 10 \
          -H "Authorization: Bearer ${USER_API_KEY:-fk-smoke-test}" \
          "${GATEWAY_URL}/v1/models" 2>/dev/null || true)"
  code="$(printf '%s' "$resp" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000

  if [[ "$code" == "200" ]]; then
    pass "GET /v1/models -> 200"
    if [[ "$HAS_JQ" == true ]]; then
      local n
      n="$(printf '%s' "$resp" | sed '$d' | jq -r '.data | length' 2>/dev/null || echo "?")"
      info "可用模型数: $n"
    fi
  elif [[ "$code" == "401" || "$code" == "403" ]]; then
    pass "GET /v1/models -> ${code}（鉴权生效）"
  else
    fail "GET /v1/models -> $code"
  fi
}

# =============================================================================
# 5. 管理接口
# =============================================================================
test_admin() {
  section "5. 管理接口"

  if [[ -z "$ADMIN_API_KEY" ]]; then
    skip "GET /admin/keys" "未找到 ADMIN_API_KEY"
    return 0
  fi

  local resp code body
  resp="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time 10 \
          -H "Authorization: Bearer ${ADMIN_API_KEY}" \
          "${GATEWAY_URL}/admin/keys" 2>/dev/null || true)"
  code="$(printf '%s' "$resp" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000
  body="$(printf '%s' "$resp" | sed '$d')"

  if [[ "$code" == "200" ]]; then
    pass "GET /admin/keys -> 200"
    if [[ "$HAS_JQ" == true ]]; then
      local n
      n="$(printf '%s' "$body" | jq -r 'if type=="array" then length elif .keys then (.keys|length) elif .data then (.data|length) else "?" end' 2>/dev/null || echo "?")"
      info "池中 Key 数: $n"
      if [[ "$n" == "0" ]]; then
        info "Key 池为空。导入火山 Key 后系统才能真正处理请求"
      fi
    fi
  else
    fail "GET /admin/keys -> $code" "$(printf '%s' "$body" | head -c 200)"
  fi

  # 未带凭证必须被拒。管理接口能匿名访问等于把 Key 池管理权开放出去，
  # 这是必须验证的安全边界，不是可选项。
  local anon
  anon="$(curl -sS "${CURL_NOPROXY[@]}" -o /dev/null -w '%{http_code}' --max-time 10 \
          "${GATEWAY_URL}/admin/keys" 2>/dev/null || true)"
  anon="${anon:-000}"
  if [[ "$anon" == "401" || "$anon" == "403" ]]; then
    pass "无凭证访问 /admin/keys -> ${anon}（正确拒绝）"
  else
    fail "无凭证访问 /admin/keys -> $anon" "管理接口未鉴权，Key 池管理权暴露"
  fi
}

# =============================================================================
# 6. 指标端点
# =============================================================================
test_metrics() {
  section "6. 指标端点"

  if [[ "$SKIP_METRICS" == "1" ]]; then
    skip "指标检查" "SKIP_METRICS=1"
    return 0
  fi

  local resp code body
  resp="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time 10 \
          "${METRICS_URL}/metrics" 2>/dev/null || true)"
  code="$(printf '%s' "$resp" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000
  body="$(printf '%s' "$resp" | sed '$d')"

  if [[ "$code" != "200" ]]; then
    fail "GET ${METRICS_URL}/metrics -> $code" "Prometheus 抓不到指标时所有告警都会静默失效"
    return 0
  fi
  pass "GET /metrics -> 200"

  # 逐个检查告警规则依赖的指标是否存在。
  # 缺失不算 fail —— 指标由 go-gateway 实现，可能还没做完；
  # 但必须显式列出来，否则会出现「告警规则写好了但永远不触发」的静默盲区。
  # 与 internal/metrics/metrics.go 核对过的实际指标名。
  # 注意 leases_active / keys_by_status 是复数/带后缀形式，别写成单数。
  local -a expected=(
    fluxkeys_requests_total
    fluxkeys_request_duration_seconds
    fluxkeys_quota_used_ratio
    fluxkeys_quota_acquire_total
    fluxkeys_keys_by_status
    fluxkeys_leases_active
    fluxkeys_leases_reaped_total
    fluxkeys_quota_drift_total
    fluxkeys_egress_ips_by_state
    fluxkeys_dependency_up
  )
  local m absent=0
  for m in "${expected[@]}"; do
    if printf '%s' "$body" | grep -q "^# *\(HELP\|TYPE\) *${m}\|^${m}[ {]"; then
      pass "指标已导出: $m"
    else
      info "指标未出现: ${m}"
      absent=$((absent + 1))
    fi
  done

  # 重要：不把「未出现」判为失败。
  # Prometheus 的 *Vec 类型（CounterVec/GaugeVec/HistogramVec）在没有任何
  # 样本时不会输出 # TYPE 行 —— 全新启动、零流量的实例上 requests_total 等
  # 指标本来就看不到，这不代表未实现。有流量后自然出现。
  # 无标签的 Gauge/Counter（如 leases_active）会立即出现。
  if [[ "$absent" -gt 0 ]]; then
    info "$absent 个指标当前无样本。若已产生过流量仍缺失，才需核对"
    info "internal/metrics/metrics.go 与 deploy/README.md「指标契约」是否一致"
  fi
  return 0
}

# =============================================================================
# 7. 看板首页
# =============================================================================
test_dashboard() {
  section "7. 统计看板"

  if [[ "$SKIP_DASHBOARD" == "1" ]]; then
    skip "看板检查" "SKIP_DASHBOARD=1"
    return 0
  fi

  local code
  code="$(curl -sS "${CURL_NOPROXY[@]}" -o /dev/null -w '%{http_code}' --max-time 10 \
          "${DASHBOARD_URL}/healthz" 2>/dev/null || true)"
  code="${code:-000}"
  if [[ "$code" == "200" ]]; then
    pass "GET 看板 /healthz -> 200"
  else
    fail "GET 看板 /healthz -> $code"
  fi

  local resp body
  resp="$(curl -sS "${CURL_NOPROXY[@]}" -w '\n%{http_code}' --max-time 15 "${DASHBOARD_URL}/" 2>/dev/null || true)"
  code="$(printf '%s' "$resp" | tail -1)"
  code="${code:-000}"   # curl 完全失败时无输出，兜底为 000
  body="$(printf '%s' "$resp" | sed '$d')"

  if [[ "$code" == "200" ]]; then
    pass "GET 看板首页 -> 200"
    if printf '%s' "$body" | grep -qi '<html\|<!doctype'; then
      pass "首页返回 HTML"
    else
      info "首页未返回 HTML（可能是 JSON API 形式，非错误）"
    fi
  elif [[ "$code" == "401" || "$code" == "403" ]]; then
    pass "GET 看板首页 -> ${code}（已启用鉴权）"
  else
    fail "GET 看板首页 -> $code"
  fi
}

# =============================================================================
# 8. 配额隔离抽查
#
# 这不是完整的并发不超刷测试（那属于集成测试范畴），只是确认
# 连续请求之后配额指标确实在变 —— 即配额写路径真的被走到了。
# 如果打了一堆请求配额却毫无变化，说明预扣压根没生效。
# =============================================================================
test_quota_moves() {
  section "8. 配额写路径抽查"

  if [[ "$SKIP_METRICS" == "1" ]]; then
    skip "配额变化检查" "SKIP_METRICS=1"
    return 0
  fi

  # 取当前配额指标之和作为基线
  local before after
  before="$(curl -sS "${CURL_NOPROXY[@]}" --max-time 10 "${METRICS_URL}/metrics" 2>/dev/null \
            | awk '/^fluxkeys_quota_used_ratio\{/ {s+=$NF} END {printf "%.6f", s+0}')"

  if [[ -z "$before" ]]; then
    skip "配额变化检查" "未导出 fluxkeys_quota_used_ratio"
    return 0
  fi
  info "基线配额使用率之和: $before"

  # 打几个请求。用 || true 忽略失败 —— 这里关心的是配额是否变化，
  # 请求本身成功与否已在前面的用例覆盖。
  local i
  for i in 1 2 3; do
    curl -sS "${CURL_NOPROXY[@]}" -o /dev/null --max-time "$CURL_TIMEOUT" \
      -X POST "${GATEWAY_URL}/v1/chat/completions" \
      -H 'Content-Type: application/json' \
      -H "Authorization: Bearer ${USER_API_KEY:-fk-smoke-test}" \
      -d "{\"model\":\"${TEST_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}" \
      2>/dev/null || true
  done

  # 快照刷新间隔默认 1s，等 3s 足够
  sleep 3

  after="$(curl -sS "${CURL_NOPROXY[@]}" --max-time 10 "${METRICS_URL}/metrics" 2>/dev/null \
           | awk '/^fluxkeys_quota_used_ratio\{/ {s+=$NF} END {printf "%.6f", s+0}')"
  info "请求后配额使用率之和: $after"

  if [[ "$before" == "$after" ]]; then
    # 没变化的合理解释：请求全部被鉴权拒绝（没到配额环节）。
    # 所以这里只提示不判失败，避免在"尚未创建用户 Key"的新部署上误报。
    info "配额未变化。若请求均被 401 拒绝则属预期；否则需检查配额写路径是否生效"
    skip "配额变化断言" "无法区分「鉴权拒绝」与「配额未生效」"
  else
    pass "配额指标随请求变化（$before -> ${after}），写路径生效"
  fi
  return 0
}

# =============================================================================
# 汇总
# =============================================================================
summary() {
  section "测试汇总"
  printf '  通过: \033[0;32m%d\033[0m   失败: \033[0;31m%d\033[0m   跳过: \033[0;90m%d\033[0m\n' \
    "$PASS" "$FAIL" "$SKIP"

  if [[ "$FAIL" -gt 0 ]]; then
    printf '\n  \033[0;31m失败项:\033[0m\n'
    local c
    for c in "${FAILED_CASES[@]}"; do
      printf '    - %s\n' "$c"
    done
    cat >&2 <<EOF

诊断命令:
  docker compose ps
  docker compose logs --tail=100 gateway
  docker compose logs --tail=50 mockark

EOF
    return 1
  fi

  printf '\n  \033[0;32m冒烟测试全部通过\033[0m\n\n'
  return 0
}

# -----------------------------------------------------------------------------
# 主流程
# -----------------------------------------------------------------------------
main() {
  printf '\033[1mFluxKeys 冒烟测试\033[0m\n'
  printf '  网关:   %s\n' "$GATEWAY_URL"
  printf '  指标:   %s\n' "$METRICS_URL"
  printf '  看板:   %s\n' "$DASHBOARD_URL"
  [[ "$HAS_JQ" == false ]] && printf '  \033[0;33m提示: 未安装 jq，JSON 断言将退化为字符串匹配\033[0m\n'

  wait_for_gateway
  test_health
  test_non_stream
  test_stream
  test_models
  test_admin
  test_metrics
  test_dashboard
  test_quota_moves

  summary
}

main "$@"
