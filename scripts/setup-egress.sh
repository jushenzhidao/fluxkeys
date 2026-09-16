#!/usr/bin/env bash
# =============================================================================
# setup-egress.sh —— 生产环境多 EIP 出口的策略路由配置与验证
# =============================================================================
#
# 这个脚本解决的是架构评审 P1-6 的问题，值得先说清楚为什么必须有它：
#
#   云厂商控制台上给弹性网卡「绑定辅助私网 IP」并关联 EIP 之后，**操作系统
#   内部什么都不会发生**：辅助 IP 不会出现在网卡上，也不会有对应的策略路由。
#   此时应用调用 net.Dialer.LocalAddr 绑定该地址，结果分两种：
#     - 地址根本不在网卡上 → bind 报 EADDRNOTAVAIL（这种反而是好事，能发现）
#     - 地址在网卡上但无策略路由 → **请求照常成功，但源地址被内核改回主 IP**
#
#   第二种是真正危险的：没有任何报错，日志一切正常，但所有 Key 实际共用同一个
#   出口 IP。「按 Key 绑定出口」这个反作弊的核心隔离手段完全失效，而你不会知道。
#   等到 Key 被批量封禁才发现，为时已晚。
#
#   所以本脚本做两件事，且第二件和第一件一样重要：
#     1. 配置：网卡加辅助 IP + 每 IP 独立路由表 + ip rule
#     2. 验证：用 curl --interface <ip> 逐个探测，打印每个 IP 实际出口的公网
#              地址。只有当各 IP 打印出**互不相同**的公网地址时，配置才算生效。
#
# 用法:
#   sudo bash scripts/setup-egress.sh                    # 从 .env 读 EGRESS_IPS 并配置+验证
#   sudo bash scripts/setup-egress.sh --verify-only      # 只验证，不改动任何配置
#   sudo bash scripts/setup-egress.sh --dry-run          # 只打印将执行的命令
#   sudo bash scripts/setup-egress.sh --ips '172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12'
#   sudo bash scripts/setup-egress.sh --persist          # 额外写入 systemd unit 使配置重启后仍生效
#
# 幂等性: 可重复执行。已存在的地址/路由/规则会被跳过而非报错。
#
# 环境要求: Linux，iproute2，curl，root 权限。
# =============================================================================

set -euo pipefail

# -----------------------------------------------------------------------------
# 常量
# -----------------------------------------------------------------------------

# 策略路由表号的起始值。100 以下常被系统与其他工具占用（main=254, default=253,
# local=255），从 100 开始并按 IP 顺序递增。
readonly TABLE_BASE=100

# ip rule 的优先级起始值。数字越小优先级越高；main 表默认在 32766，
# 我们的规则必须排在它之前才能生效。
readonly RULE_PRIO_BASE=10000

# 路由表名持久化位置。写成独立文件而非追加 /etc/iproute2/rt_tables，
# 避免重复执行时把同一行追加多次。
readonly RT_TABLES_DIR=/etc/iproute2/rt_tables.d
readonly RT_TABLES_FILE="${RT_TABLES_DIR}/fluxkeys.conf"

# 查询出口公网 IP 的服务。用多个是为了容错 —— 单个服务不可用不该让验证失败。
# 这些服务只返回调用方的公网 IP，不接收任何业务数据。
readonly IP_ECHO_SERVICES=(
  "https://api.ipify.org"
  "https://ifconfig.me/ip"
  "https://icanhazip.com"
)

readonly SYSTEMD_UNIT=/etc/systemd/system/fluxkeys-egress.service

# -----------------------------------------------------------------------------
# 全局状态
# -----------------------------------------------------------------------------
DRY_RUN=false
VERIFY_ONLY=false
PERSIST=false
IPS_ARG=""
IFACE=""
declare -a ADDRS=()      # 本机绑定地址（TCP 源地址）
declare -a PUBLIC_IPS=() # 期望的对应 EIP，仅用于比对展示

# -----------------------------------------------------------------------------
# 输出辅助
# -----------------------------------------------------------------------------
log()  { printf '\033[0;34m[INFO]\033[0m  %s\n' "$*"; }
ok()   { printf '\033[0;32m[ OK ]\033[0m  %s\n' "$*"; }
warn() { printf '\033[0;33m[WARN]\033[0m  %s\n' "$*" >&2; }
err()  { printf '\033[0;31m[FAIL]\033[0m  %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

section() {
  printf '\n\033[1m==> %s\033[0m\n' "$*"
}

# run 执行命令；--dry-run 下只打印。
# 所有会改变系统状态的操作都必须走这个函数，否则 --dry-run 会失去意义。
run() {
  if [[ "$DRY_RUN" == true ]]; then
    printf '  \033[0;90m[dry-run]\033[0m %s\n' "$*"
    return 0
  fi
  "$@"
}

# -----------------------------------------------------------------------------
# 参数解析
# -----------------------------------------------------------------------------
usage() {
  sed -n '2,40p' "$0" | sed 's/^# \?//'
  exit 0
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run)     DRY_RUN=true; shift ;;
      --verify-only) VERIFY_ONLY=true; shift ;;
      --persist)     PERSIST=true; shift ;;
      --ips)
        [[ $# -ge 2 ]] || die "--ips 需要参数值"
        IPS_ARG="$2"; shift 2 ;;
      --iface)
        [[ $# -ge 2 ]] || die "--iface 需要参数值"
        IFACE="$2"; shift 2 ;;
      -h|--help)     usage ;;
      *)             die "未知参数: $1（用 --help 查看用法）" ;;
    esac
  done
}

# -----------------------------------------------------------------------------
# 前置检查
# -----------------------------------------------------------------------------
check_prerequisites() {
  section "前置检查"

  # 这个脚本操作的是 Linux 内核的策略路由，在 macOS/BSD 上完全不适用。
  # 明确报错比让 ip 命令 not found 更有帮助。
  [[ "$(uname -s)" == "Linux" ]] \
    || die "本脚本仅支持 Linux（当前: $(uname -s)）。macOS 开发机上无法配置策略路由，请在目标服务器执行。"

  for cmd in ip curl awk; do
    command -v "$cmd" >/dev/null 2>&1 || die "缺少命令: ${cmd}（Debian/Ubuntu: apt install iproute2 curl）"
  done
  ok "依赖命令齐备"

  # --dry-run 和 --verify-only 不改系统状态，无需 root。
  # 但 verify 需要读 ip rule，非 root 通常也能读，所以只在真正要改时强制。
  if [[ "$DRY_RUN" == false && "$VERIFY_ONLY" == false && "$EUID" -ne 0 ]]; then
    die "需要 root 权限来配置网卡与路由（请用 sudo 执行）"
  fi
  if [[ "$EUID" -eq 0 ]]; then
    ok "以 root 运行"
  else
    log "非 root（仅验证/演练模式，可接受）"
  fi
}

# -----------------------------------------------------------------------------
# validate_ipv4 校验字符串是合法的 IPv4 点分十进制地址。
#
# 分两步：先匹配形状，再逐段检查 0-255 范围。只做形状匹配会放过 999.1.2.3。
# 返回 0 表示合法，非 0 表示非法。
# -----------------------------------------------------------------------------
validate_ipv4() {
  local ip="$1"
  [[ "$ip" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1

  local seg
  for seg in "${BASH_REMATCH[@]:1:4}"; do
    # 去前导零后比较，避免 010 被当成八进制
    seg=$((10#$seg))
    (( seg >= 0 && seg <= 255 )) || return 1
  done
  return 0
}

# -----------------------------------------------------------------------------
# 读取 IP 列表
#
# 优先级: --ips 参数 > 环境变量 EGRESS_IPS > .env 文件里的 EGRESS_IPS
# 格式与 internal/config/config.go 的 parseEgressIPs 完全一致：
#   "172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12"
# 等号后的公网 IP 可省略（仅用于展示核对）。
# -----------------------------------------------------------------------------
load_ips() {
  section "读取出口 IP 配置"

  local raw="$IPS_ARG"

  if [[ -z "$raw" ]]; then
    raw="${EGRESS_IPS:-}"
    [[ -n "$raw" ]] && log "来源: 环境变量 EGRESS_IPS"
  else
    log "来源: --ips 参数"
  fi

  # 从 .env 兜底读取。注意不 source .env —— 那会把密码等变量一并带进当前 shell
  # 环境，之后任何子进程都能读到，是不必要的暴露。这里只精确抓一行。
  if [[ -z "$raw" ]]; then
    local env_file
    env_file="$(dirname "$0")/../.env"
    if [[ -f "$env_file" ]]; then
      raw="$(awk -F= '/^[[:space:]]*EGRESS_IPS[[:space:]]*=/ {
                        sub(/^[^=]*=/, "");
                        gsub(/^[[:space:]]*|[[:space:]]*$/, "");
                        gsub(/^"|"$/, "");
                        print; exit
                      }' "$env_file")"
      [[ -n "$raw" ]] && log "来源: $env_file 中的 EGRESS_IPS"
    fi
  fi

  [[ -n "$raw" ]] || die "未找到出口 IP 配置。请设置 EGRESS_IPS 环境变量、在 .env 中配置，或用 --ips 传入。
格式: --ips '172.16.0.11=203.0.113.11,172.16.0.12=203.0.113.12'"

  # 逐项解析
  local item addr public
  local IFS=','
  for item in $raw; do
    # 去空白
    item="${item#"${item%%[![:space:]]*}"}"
    item="${item%"${item##*[![:space:]]}"}"
    [[ -n "$item" ]] || continue

    if [[ "$item" == *"="* ]]; then
      addr="${item%%=*}"
      public="${item#*=}"
    else
      addr="$item"
      public=""
    fi

    # 校验是合法 IPv4。配错一个字符就会在网卡上加出一个错误地址，
    # 之后所有绑定该地址的请求都会失败，早点拦住。
    #
    # 注意不能只用 ^([0-9]{1,3}\.){3}[0-9]{1,3}$ —— 那会放过 999.1.2.3
    # 这种每段超出 0-255 的地址（实测踩过）。所以形状匹配之后还要逐段查范围。
    validate_ipv4 "$addr" || die "非法的 IPv4 地址: '$addr'"

    ADDRS+=("$addr")
    PUBLIC_IPS+=("$public")
  done

  [[ ${#ADDRS[@]} -gt 0 ]] || die "解析后出口 IP 列表为空"

  ok "共 ${#ADDRS[@]} 个出口 IP:"
  local i
  for i in "${!ADDRS[@]}"; do
    printf '     %-16s -> EIP %s\n' "${ADDRS[$i]}" "${PUBLIC_IPS[$i]:-（未声明）}"
  done
}

# -----------------------------------------------------------------------------
# 探测主网卡与网关
# -----------------------------------------------------------------------------
detect_iface() {
  section "探测网卡与默认网关"

  if [[ -z "$IFACE" ]]; then
    # 从默认路由反查出口网卡，比硬编码 eth0 可靠
    IFACE="$(ip -4 route show default | awk '/default/ {for(i=1;i<=NF;i++) if($i=="dev") {print $(i+1); exit}}')"
    [[ -n "$IFACE" ]] || die "无法自动探测默认网卡，请用 --iface 指定"
  fi
  ip link show "$IFACE" >/dev/null 2>&1 || die "网卡不存在: $IFACE"
  ok "网卡: $IFACE"

  GATEWAY="$(ip -4 route show default | awk '/default/ {for(i=1;i<=NF;i++) if($i=="via") {print $(i+1); exit}}')"
  [[ -n "$GATEWAY" ]] || die "无法探测默认网关"
  ok "默认网关: $GATEWAY"

  # 主 IP：用于在验证阶段做对照。若某个辅助 IP 的出口公网地址与主 IP 相同，
  # 就说明策略路由没生效 —— 这是本脚本要抓的核心失效模式。
  PRIMARY_ADDR="$(ip -4 -o addr show dev "$IFACE" scope global \
                  | awk '{print $4}' | cut -d/ -f1 | head -1)"
  ok "主地址: ${PRIMARY_ADDR:-未知}"

  # 子网前缀长度，加辅助 IP 时要保持一致
  PREFIX_LEN="$(ip -4 -o addr show dev "$IFACE" scope global \
                | awk '{print $4}' | cut -d/ -f2 | head -1)"
  PREFIX_LEN="${PREFIX_LEN:-24}"

  # 所在子网，用于给每个路由表加本地网段直连路由
  SUBNET="$(ip -4 route show dev "$IFACE" proto kernel scope link \
            | awk '{print $1; exit}')"
  ok "子网: ${SUBNET:-未知} (/$PREFIX_LEN)"
}

# -----------------------------------------------------------------------------
# 放宽反向路径过滤
#
# 为什么需要：rp_filter=1（严格模式）时，内核会校验「回包的入站网卡是否与
# 该源地址的出站路由一致」。多 IP 单网卡场景下这个校验经常判失败，导致回包
# 被静默丢弃 —— 表现为「请求发出去了但永远收不到响应」，非常难排查。
# 设为 2（松散模式）：只要该源地址在任意网卡上可达即通过，安全性足够。
# -----------------------------------------------------------------------------
relax_rp_filter() {
  section "调整反向路径过滤 (rp_filter)"

  local current
  current="$(cat "/proc/sys/net/ipv4/conf/${IFACE}/rp_filter" 2>/dev/null || echo "?")"
  log "当前 ${IFACE} rp_filter = $current"

  if [[ "$current" != "1" ]]; then
    ok "rp_filter 无需调整"
    return 0
  fi

  warn "严格模式会导致多 IP 场景回包被丢弃，调整为松散模式 (2)"

  # 这里刻意不让失败中止整个脚本。
  # 在容器内或部分受限环境中 /proc/sys 是只读的，sysctl 会失败；
  # 但策略路由本身仍然可以配置和验证。真正的判定标准是后面 verify_egress
  # 的实测结果 —— 如果 rp_filter 确实造成了回包被丢，那里会明确报出来。
  # 反过来说，因为一个可能无关的 sysctl 失败就拒绝配置路由，是过度反应。
  local sysctl_ok=true
  run sysctl -qw "net.ipv4.conf.${IFACE}.rp_filter=2" 2>/dev/null || sysctl_ok=false
  run sysctl -qw "net.ipv4.conf.all.rp_filter=2" 2>/dev/null || sysctl_ok=false

  if [[ "$sysctl_ok" == false ]]; then
    warn "无法修改 rp_filter（只读 /proc/sys，常见于容器内运行）"
    warn "若后续验证出现「能发包但收不到响应」，需在宿主机执行:"
    warn "  sysctl -w net.ipv4.conf.all.rp_filter=2"
    return 0
  fi

  # 持久化，否则重启失效
  if [[ "$DRY_RUN" == false ]]; then
    printf 'net.ipv4.conf.%s.rp_filter = 2\nnet.ipv4.conf.all.rp_filter = 2\n' "$IFACE" \
      > /etc/sysctl.d/99-fluxkeys-egress.conf 2>/dev/null \
      || warn "无法写入 /etc/sysctl.d/99-fluxkeys-egress.conf，重启后 rp_filter 会恢复"
  else
    printf '  \033[0;90m[dry-run]\033[0m 写入 /etc/sysctl.d/99-fluxkeys-egress.conf\n'
  fi
  ok "rp_filter 已设为 2"
}

# -----------------------------------------------------------------------------
# 注册路由表名
# -----------------------------------------------------------------------------
register_table_names() {
  section "注册路由表名"

  run mkdir -p "$RT_TABLES_DIR"

  # 整个文件重写而不是追加 —— 这是幂等的关键。
  # 追加的话重复执行会产生重复条目，ip 命令会告警且行为不确定。
  local content=""
  local i table_id
  for i in "${!ADDRS[@]}"; do
    table_id=$((TABLE_BASE + i))
    content+="${table_id} fluxkeys_eg${i}"$'\n'
  done

  if [[ "$DRY_RUN" == true ]]; then
    printf '  \033[0;90m[dry-run]\033[0m 写入 %s:\n' "$RT_TABLES_FILE"
    printf '%s' "$content" | sed 's/^/      /'
  else
    printf '%s' "$content" > "$RT_TABLES_FILE"
    ok "已写入 ${RT_TABLES_FILE}（${#ADDRS[@]} 个表）"
  fi
}

# -----------------------------------------------------------------------------
# 在网卡上添加辅助 IP
#
# 幂等实现：先查地址是否已存在，存在则跳过。
# 直接 `ip addr add` 已存在的地址会返回 EEXIST 并因 set -e 中止脚本。
# -----------------------------------------------------------------------------
configure_addresses() {
  section "配置网卡辅助 IP"

  local addr
  for addr in "${ADDRS[@]}"; do
    if ip -4 -o addr show dev "$IFACE" | grep -qw "$addr"; then
      ok "$addr 已在 $IFACE 上，跳过"
      continue
    fi
    log "添加 $addr/$PREFIX_LEN 到 $IFACE"
    # label 便于 `ip addr` 输出中辨认来源；noprefixroute 避免内核自动添加
    # 一条与主地址重复的子网路由（那会干扰策略路由的判定）
    run ip addr add "${addr}/${PREFIX_LEN}" dev "$IFACE" noprefixroute
    ok "$addr 已添加"
  done

  if [[ "$DRY_RUN" == false ]]; then
    log "当前 $IFACE 上的地址:"
    ip -4 -o addr show dev "$IFACE" | awk '{printf "     %s\n", $4}'
  fi
}

# -----------------------------------------------------------------------------
# 为每个 IP 建立独立路由表与策略规则
#
# 这是整个脚本的核心。每个出口 IP 需要三样东西：
#   1. 该表内的本地网段直连路由（否则同网段流量走不通）
#   2. 该表内的默认路由，且 src 指定为本 IP
#   3. 一条 `from <ip> lookup <table>` 的规则，让源地址为该 IP 的流量查这张表
# 缺任何一条，绑定就会退回主 IP —— 也就是静默失效。
# -----------------------------------------------------------------------------
configure_routes() {
  section "配置策略路由"

  local i addr table_id table_name prio
  for i in "${!ADDRS[@]}"; do
    addr="${ADDRS[$i]}"
    table_id=$((TABLE_BASE + i))
    table_name="fluxkeys_eg${i}"
    prio=$((RULE_PRIO_BASE + i))

    log "[$addr] 表 $table_id ($table_name)，规则优先级 $prio"

    # --- 1. 本地网段直连路由 ---
    if [[ -n "$SUBNET" ]]; then
      if ip route show table "$table_id" 2>/dev/null | grep -q "^${SUBNET} "; then
        printf '     子网路由已存在，跳过\n'
      else
        run ip route add "$SUBNET" dev "$IFACE" src "$addr" table "$table_id"
        printf '     已加子网路由 %s src %s\n' "$SUBNET" "$addr"
      fi
    fi

    # --- 2. 默认路由（带 src，确保出包源地址就是本 IP）---
    if ip route show table "$table_id" 2>/dev/null | grep -q "^default "; then
      printf '     默认路由已存在，跳过\n'
    else
      run ip route add default via "$GATEWAY" dev "$IFACE" src "$addr" table "$table_id"
      printf '     已加默认路由 via %s src %s\n' "$GATEWAY" "$addr"
    fi

    # --- 3. 策略规则 ---
    # 幂等判断：匹配 "from <addr> lookup <table>"。
    # 注意 ip rule 的输出里表名可能显示为数字或名称，两种都要匹配。
    if ip rule show | grep -qE "from ${addr}(/32)? +lookup (${table_id}|${table_name})"; then
      printf '     ip rule 已存在，跳过\n'
    else
      run ip rule add from "$addr" lookup "$table_id" priority "$prio"
      printf '     已加 ip rule from %s lookup %s\n' "$addr" "$table_id"
    fi
  done

  # 路由缓存刷新。Linux 3.6+ 已移除路由缓存，这条基本是历史遗留的保险措施，
  # 失败不影响任何功能（容器内 /proc/sys 只读时会失败），所以忽略错误。
  run ip route flush cache 2>/dev/null || true

  ok "策略路由配置完成"

  if [[ "$DRY_RUN" == false ]]; then
    log "当前 FluxKeys 相关规则:"
    ip rule show | grep -E "fluxkeys|lookup 1[0-9][0-9]" | sed 's/^/     /' || true
  fi
}

# -----------------------------------------------------------------------------
# 查询单个源 IP 的实际出口公网地址
#
# curl --interface 可以接受 IP 地址（不只是网卡名），效果等同于绑定源地址，
# 与 Go 侧 net.Dialer.LocalAddr 的行为一致 —— 这正是我们要验证的路径。
# -----------------------------------------------------------------------------
probe_public_ip() {
  local src="$1"
  local svc result

  for svc in "${IP_ECHO_SERVICES[@]}"; do
    # --max-time 10 防止单个服务卡住整个验证流程
    # -sS 静默但保留错误输出，-4 强制 IPv4
    if result="$(curl -4 -sS --max-time 10 --interface "$src" "$svc" 2>/dev/null)"; then
      # 去掉可能的空白与换行
      result="$(printf '%s' "$result" | tr -d '[:space:]')"
      if [[ "$result" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
        printf '%s' "$result"
        return 0
      fi
    fi
  done
  return 1
}

# -----------------------------------------------------------------------------
# 验证 —— 脚本最重要的部分
#
# 判定逻辑（三重）：
#   1. 每个 IP 都必须能成功出网
#   2. 各 IP 的出口公网地址必须**互不相同**（相同 = 策略路由未生效）
#   3. 若 .env 里声明了期望 EIP，实际值必须与声明一致
# -----------------------------------------------------------------------------
verify_egress() {
  section "验证出口 IP 实际生效情况"

  if [[ "$DRY_RUN" == true ]]; then
    warn "--dry-run 模式跳过验证（未实际配置，验证无意义）"
    return 0
  fi

  log "逐个 IP 探测真实出口公网地址（curl --interface）..."
  echo

  # 先取主 IP 的出口作为对照基准
  local primary_public=""
  if [[ -n "$PRIMARY_ADDR" ]]; then
    if primary_public="$(probe_public_ip "$PRIMARY_ADDR")"; then
      printf '  %-16s -> %-16s \033[0;90m(主地址，对照基准)\033[0m\n' "$PRIMARY_ADDR" "$primary_public"
    else
      warn "主地址 $PRIMARY_ADDR 出网探测失败，无法建立对照基准"
      primary_public=""
    fi
  fi

  local -a results=()
  local failed=0
  local i addr expect actual mark

  for i in "${!ADDRS[@]}"; do
    addr="${ADDRS[$i]}"
    expect="${PUBLIC_IPS[$i]}"

    if ! actual="$(probe_public_ip "$addr")"; then
      printf '  %-16s -> \033[0;31m探测失败\033[0m（无法出网）\n' "$addr"
      results+=("FAIL")
      failed=$((failed + 1))
      continue
    fi

    results+=("$actual")

    # 与主 IP 相同 = 策略路由未生效，这是最需要抓的情况
    if [[ -n "$primary_public" && "$actual" == "$primary_public" ]]; then
      printf '  %-16s -> \033[0;31m%-16s\033[0m \033[0;31m✗ 与主地址出口相同，策略路由未生效！\033[0m\n' \
        "$addr" "$actual"
      failed=$((failed + 1))
      continue
    fi

    # 与声明的 EIP 比对
    if [[ -n "$expect" ]]; then
      if [[ "$actual" == "$expect" ]]; then
        mark=$'\033[0;32m✓ 与声明一致\033[0m'
      else
        mark=$'\033[0;31m✗ 声明为 '"$expect"$'\033[0m'
        failed=$((failed + 1))
      fi
    else
      mark=$'\033[0;90m（未声明期望值）\033[0m'
    fi
    printf '  %-16s -> %-16s %b\n' "$addr" "$actual" "$mark"
  done

  echo

  # 唯一性检查：即便每个都能出网、都与主 IP 不同，若两个辅助 IP 之间
  # 出口相同，隔离依然是破的。
  local uniq_count total_count
  total_count=0
  for r in "${results[@]}"; do
    [[ "$r" != "FAIL" ]] && total_count=$((total_count + 1))
  done
  uniq_count="$(printf '%s\n' "${results[@]}" | grep -v '^FAIL$' | sort -u | wc -l | tr -d ' ')"

  if [[ "$total_count" -gt 0 && "$uniq_count" -ne "$total_count" ]]; then
    err "出口公网地址不唯一：${total_count} 个 IP 只对应 ${uniq_count} 个不同出口"
    err "说明部分 IP 共用了同一出口，按 Key 绑定出口的隔离效果是假的"
    failed=$((failed + 1))
  fi

  if [[ "$failed" -gt 0 ]]; then
    err "验证未通过（$failed 项异常）"
    cat >&2 <<'EOF'

排查建议:
  1. 确认云厂商控制台已给弹性网卡绑定这些辅助私网 IP，且各自关联了独立 EIP
     —— 控制台没绑，OS 侧配置得再对也出不去。
  2. 检查规则与路由是否存在:
       ip rule show
       ip route show table 100
  3. 检查 rp_filter（严格模式会丢回包）:
       sysctl net.ipv4.conf.all.rp_filter
  4. 逐个手工复现:
       curl -4 --interface 172.16.0.11 https://api.ipify.org
  5. 若出口公网地址全部相同，几乎可以确定是 ip rule 缺失或优先级
     排在 main 表（32766）之后导致未命中。

详细排查步骤见 deploy/README.md「出口 IP 不生效」一节。
EOF
    return 1
  fi

  ok "验证通过：${total_count} 个出口 IP 各自使用独立公网出口"
  return 0
}

# -----------------------------------------------------------------------------
# 持久化到 systemd
#
# ip addr / ip rule 都是内存态，重启即失效。而这个配置一旦在重启后丢失，
# 系统会退回「所有 Key 共用主 IP」的静默失效状态 —— 危害等同于从未配置。
# 因此强烈建议 --persist。
# -----------------------------------------------------------------------------
persist_config() {
  section "持久化配置（systemd）"

  local script_path
  script_path="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

  local unit_content
  unit_content="$(cat <<EOF
[Unit]
Description=FluxKeys 多 EIP 出口策略路由
# 必须等网络就绪后再配置，否则网卡还没起来，ip addr add 会失败
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
# 复用同一个脚本，保证开机配置与手工执行的逻辑完全一致
ExecStart=/usr/bin/env bash ${script_path}
# 出口配置失败不应让整机启动流程卡死，但会在 systemctl status 里留下失败记录
# （配合 FluxKeysEgressIPDown 告警可以及时发现）
SuccessExitStatus=0
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
)"

  if [[ "$DRY_RUN" == true ]]; then
    printf '  \033[0;90m[dry-run]\033[0m 写入 %s\n' "$SYSTEMD_UNIT"
    printf '%s\n' "$unit_content" | sed 's/^/      /'
    return 0
  fi

  printf '%s\n' "$unit_content" > "$SYSTEMD_UNIT"
  systemctl daemon-reload
  systemctl enable fluxkeys-egress.service >/dev/null 2>&1
  ok "已写入 $SYSTEMD_UNIT 并设为开机启动"
  log "验证: systemctl status fluxkeys-egress.service"
}

# -----------------------------------------------------------------------------
# 主流程
# -----------------------------------------------------------------------------
main() {
  parse_args "$@"

  printf '\033[1mFluxKeys 出口 IP 配置工具\033[0m\n'
  [[ "$DRY_RUN" == true ]]     && warn "演练模式：不会改动任何系统配置"
  [[ "$VERIFY_ONLY" == true ]] && log "仅验证模式：不会改动任何系统配置"

  check_prerequisites
  load_ips
  detect_iface

  if [[ "$VERIFY_ONLY" == false ]]; then
    relax_rp_filter
    register_table_names
    configure_addresses
    configure_routes
    [[ "$PERSIST" == true ]] && persist_config
  else
    log "跳过配置步骤"
  fi

  # 验证失败要用非零退出码，这样 systemd 与 CI 才能感知
  if ! verify_egress; then
    exit 1
  fi

  section "完成"
  cat <<EOF
下一步:
  1. 在 .env 中确认:
       EGRESS_MODE=multi_ip
       EGRESS_IPS=$(IFS=,; printf '%s' "${ADDRS[*]}")
       EGRESS_VERIFY_ON_START=true
  2. 启动（仓库只有一份编排文件，gateway 走 host 网络）:
       docker compose up -d
  3. 网关启动后核对指标 fluxkeys_egress_ip_up 是否全为 1

EOF
  [[ "$PERSIST" == false && "$VERIFY_ONLY" == false ]] && warn "配置未持久化，重启后会丢失。建议重新执行并加 --persist"
  return 0
}

main "$@"
