/* FluxKeys 看板前端：原生 JS + Chart.js，无构建工具。
 *
 * 配色约定（中国用户直觉）：消耗上升/水位高用红色，余量充足用绿色。
 * 数字统一千分位；token 量大时压缩为「万 / 百万 / 亿」。
 */

'use strict';

// ------------------------------------------------------------ 格式化

/** 千分位整数。 */
function fmtInt(n) {
  const v = Number(n);
  if (!Number.isFinite(v)) return '0';
  return Math.round(v).toLocaleString('zh-CN');
}

/** token 量压缩显示：万 / 百万 / 亿。小于 1 万时保留千分位原值。 */
function fmtTokens(n) {
  const v = Number(n) || 0;
  const abs = Math.abs(v);
  if (abs >= 1e8) return (v / 1e8).toFixed(2) + ' 亿';
  if (abs >= 1e6) return (v / 1e6).toFixed(2) + ' 百万';
  if (abs >= 1e4) return (v / 1e4).toFixed(2) + ' 万';
  return fmtInt(v);
}

/** 比例转百分比字符串。 */
function fmtPct(x, digits) {
  const v = Number(x) || 0;
  return (v * 100).toFixed(digits === undefined ? 1 : digits) + '%';
}

function fmtTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('zh-CN', { hour12: false });
}

function fmtDay(s) {
  return s ? String(s).slice(5) : '—';
}

// 把分钟数写成人读得懂的时长。用于封禁冷却倒计时（退避后可达 24 小时），
// 「1440 分钟」这种写法运维得自己换算。
function fmtMinutes(mins) {
  const m = Math.max(0, Math.round(Number(mins) || 0));
  if (m < 60) return m + ' 分钟';
  const h = Math.floor(m / 60);
  const rest = m % 60;
  if (h < 24) return h + ' 小时' + (rest ? rest + ' 分钟' : '');
  const d = Math.floor(h / 24);
  const restH = h % 24;
  return d + ' 天' + (restH ? restH + ' 小时' : '');
}

function esc(s) {
  return String(s === null || s === undefined ? '' : s).replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])
  );
}

/** 按水位比例返回样式等级：越高越红。 */
function levelOf(ratio) {
  const v = Number(ratio) || 0;
  if (v >= 0.85) return 'lv-high';
  if (v >= 0.6) return 'lv-mid';
  return 'lv-ok';
}

/** 水位条 HTML。 */
function gaugeHtml(ratio) {
  const v = Math.min(1, Math.max(0, Number(ratio) || 0));
  return (
    '<div class="gauge ' + levelOf(v) + '">' +
    '<span class="bar"><i style="width:' + (v * 100).toFixed(1) + '%"></i></span>' +
    '<span class="pct">' + fmtPct(v) + '</span></div>'
  );
}

function statusTag(status) {
  const cls =
    status === 'active' ? 'tag-ok' :
    status === 'cooldown' ? 'tag-warn' :
    (status === 'banned' || status === 'invalid') ? 'tag-up' : 'tag-dim';
  return '<span class="tag ' + cls + '">' + esc(status || '—') + '</span>';
}

function refreshTag(state) {
  const cls =
    state === 'confirmed' ? 'tag-ok' :
    state === 'probing' ? 'tag-warn' :
    state === 'failed' ? 'tag-up' : 'tag-dim';
  return '<span class="tag ' + cls + '">' + esc(state || 'idle') + '</span>';
}

// ------------------------------------------------------------ 请求

async function fetchJSON(url) {
  const resp = await fetch(url, {
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  });
  // 会话过期或未登录：直接跳登录页。若只把 401 当普通错误铺在面板上，
  // 运维会看到 8 个「加载失败」而看不出真正原因是掉登录了。
  // 注意转发层已把网关 401 转成 500，故这里的 401 一定是看板会话问题。
  if (resp.status === 401) {
    window.location.replace('/login');
    throw new Error('会话已过期，正在跳转登录');
  }
  if (!resp.ok) {
    let detail = resp.statusText;
    try {
      const body = await resp.json();
      if (body && body.detail) detail = body.detail;
    } catch (e) { /* 响应非 JSON，沿用 statusText */ }
    throw new Error(detail);
  }
  return resp.json();
}

function showError(msg) {
  const box = document.getElementById('global-error');
  if (!msg) {
    box.style.display = 'none';
    return;
  }
  box.style.display = 'block';
  box.textContent = '数据加载异常：' + msg;
}

function emptyRow(tbody, cols, text) {
  tbody.innerHTML =
    '<tr><td class="empty" colspan="' + cols + '">' + esc(text || '暂无数据') + '</td></tr>';
}

// ------------------------------------------------------------ 图表

const CHARTS = {};
const GRID = 'rgba(255,255,255,0.06)';
const TICK = '#939cab';

Chart.defaults.color = TICK;
Chart.defaults.font.family =
  '-apple-system, BlinkMacSystemFont, "PingFang SC", "Microsoft YaHei", sans-serif';

function upsertChart(id, config) {
  if (CHARTS[id]) {
    CHARTS[id].destroy();
  }
  const el = document.getElementById(id);
  if (!el) return;
  CHARTS[id] = new Chart(el, config);
}

function baseScales(yTickFmt) {
  return {
    x: { grid: { color: GRID }, ticks: { color: TICK } },
    y: {
      beginAtZero: true,
      grid: { color: GRID },
      ticks: { color: TICK, callback: yTickFmt },
    },
  };
}

// ------------------------------------------------------------ 概览

function renderCards(o) {
  const remainRatio = o.capacity_tokens > 0 ? o.remaining_tokens / o.capacity_tokens : 0;
  // 余量充足 → 绿；余量偏低 → 红（中国习惯）
  const remainCls = remainRatio >= 0.4 ? 'ok' : remainRatio >= 0.15 ? 'warn' : 'up';
  const waterCls = o.avg_quota_ratio >= 0.85 ? 'up' : o.avg_quota_ratio >= 0.6 ? 'warn' : 'ok';
  const errCls = o.error_rate >= 0.05 ? 'up' : o.error_rate >= 0.01 ? 'warn' : 'ok';
  const refreshCls = o.unconfirmed_refresh > 0 ? 'up' : 'ok';
  const alertCls = o.alert_count > 0 ? 'up' : 'ok';

  const cards = [
    { cls: 'up', label: '今日总用量', value: fmtTokens(o.total_tokens),
      sub: '输入 ' + fmtTokens(o.prompt_tokens) + ' / 输出 ' + fmtTokens(o.completion_tokens) },
    { cls: '', label: '今日请求数', value: fmtInt(o.total_requests),
      sub: '平均延迟 ' + fmtInt(o.avg_latency_ms) + ' ms' },
    { cls: 'ok', label: '活跃 Key', value: fmtInt(o.active_keys) + ' / ' + fmtInt(o.total_keys),
      sub: '今日有用量 ' + fmtInt(o.used_keys) + ' 个' },
    { cls: waterCls, label: '平均配额水位', value: fmtPct(o.avg_quota_ratio),
      sub: '最高单 Key ' + fmtPct(o.max_quota_ratio) },
    { cls: remainCls, label: '全池剩余额度', value: fmtTokens(o.remaining_tokens),
      sub: '总容量 ' + fmtTokens(o.capacity_tokens) + '（' + fmtPct(remainRatio) + '）' },
    { cls: errCls, label: '今日错误率', value: fmtPct(o.error_rate, 2),
      sub: '错误请求 ' + fmtInt(o.error_requests) + ' 次' },
    { cls: refreshCls, label: '刷新未确认', value: fmtInt(o.unconfirmed_refresh),
      sub: o.unconfirmed_refresh > 0 ? '存在超刷风险' : '全部已确认' },
    { cls: alertCls, label: '配额告警', value: fmtInt(o.alert_count),
      sub: '配额日进度 ' + fmtPct(o.quota_day_progress) },
  ];

  document.getElementById('cards').innerHTML = cards
    .map(
      (c) =>
        '<div class="card ' + c.cls + '">' +
        '<div class="label">' + esc(c.label) + '</div>' +
        '<div class="value">' + esc(c.value) + '</div>' +
        '<div class="sub">' + esc(c.sub) + '</div></div>'
    )
    .join('');

  document.getElementById('meta-quota-day').textContent = '配额日 ' + (o.quota_day || '—');
  document.getElementById('meta-updated').textContent = '更新于 ' + fmtTime(o.generated_at);

  renderPoolChart(o.pools || []);
}

function renderPoolChart(pools) {
  if (!pools.length) {
    upsertChart('chart-pool', {
      type: 'bar',
      data: { labels: ['暂无数据'], datasets: [{ data: [0], backgroundColor: '#2a3037' }] },
      options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } } },
    });
    return;
  }
  const palette = { hot: '#f04d4d', warm: '#e8a33d', cold: '#23b26d' };
  upsertChart('chart-pool', {
    type: 'doughnut',
    data: {
      labels: pools.map((p) => p.name),
      datasets: [{
        data: pools.map((p) => p.count),
        backgroundColor: pools.map((p) => palette[p.name] || '#4b91f1'),
        borderColor: '#171b21',
        borderWidth: 2,
      }],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      plugins: { legend: { position: 'right' } },
    },
  });
}

// ------------------------------------------------------------ 趋势

function renderTrend(data) {
  const points = data.points || [];
  if (!points.length) {
    upsertChart('chart-trend', {
      type: 'line',
      data: { labels: ['暂无数据'], datasets: [{ data: [0], borderColor: '#2a3037' }] },
      options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } } },
    });
    return;
  }
  upsertChart('chart-trend', {
    type: 'line',
    data: {
      labels: points.map((p) => fmtDay(p.quota_day)),
      datasets: [
        {
          label: 'Token 用量',
          data: points.map((p) => p.total_tokens),
          borderColor: '#f04d4d',
          backgroundColor: 'rgba(240,77,77,0.14)',
          fill: true,
          tension: 0.3,
          yAxisID: 'y',
        },
        {
          label: '请求数',
          data: points.map((p) => p.requests),
          borderColor: '#4b91f1',
          backgroundColor: 'transparent',
          tension: 0.3,
          yAxisID: 'y1',
        },
      ],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { position: 'top' },
        tooltip: {
          callbacks: {
            label: (ctx) =>
              ctx.dataset.label +
              '：' +
              (ctx.datasetIndex === 0 ? fmtTokens(ctx.parsed.y) : fmtInt(ctx.parsed.y)),
          },
        },
      },
      scales: {
        x: { grid: { color: GRID }, ticks: { color: TICK } },
        y: {
          beginAtZero: true,
          position: 'left',
          grid: { color: GRID },
          ticks: { color: TICK, callback: (v) => fmtTokens(v) },
        },
        y1: {
          beginAtZero: true,
          position: 'right',
          grid: { display: false },
          ticks: { color: TICK, callback: (v) => fmtInt(v) },
        },
      },
    },
  });
}

// ------------------------------------------------------------ 错误

function renderErrors(data) {
  const timeline = data.timeline || [];
  if (timeline.length) {
    upsertChart('chart-errors', {
      type: 'line',
      data: {
        labels: timeline.map((p) =>
          new Date(p.hour).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
        ),
        datasets: [{
          label: '错误率',
          data: timeline.map((p) => p.error_rate),
          borderColor: '#f04d4d',
          backgroundColor: 'rgba(240,77,77,0.14)',
          fill: true,
          tension: 0.3,
        }],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
          tooltip: { callbacks: { label: (ctx) => '错误率 ' + fmtPct(ctx.parsed.y, 2) } },
        },
        scales: baseScales((v) => fmtPct(v, 0)),
      },
    });
  } else {
    upsertChart('chart-errors', {
      type: 'line',
      data: { labels: ['暂无数据'], datasets: [{ data: [0], borderColor: '#2a3037' }] },
      options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } } },
    });
  }

  const codes = data.by_code || [];
  upsertChart('chart-error-code', {
    type: 'bar',
    data: {
      labels: codes.length ? codes.map((c) => c.label) : ['暂无错误'],
      datasets: [{
        label: '次数',
        data: codes.length ? codes.map((c) => c.count) : [0],
        backgroundColor: '#f04d4d',
      }],
    },
    options: {
      indexAxis: 'y',
      responsive: true,
      maintainAspectRatio: false,
      plugins: { legend: { display: false } },
      scales: {
        x: { beginAtZero: true, grid: { color: GRID }, ticks: { color: TICK, callback: (v) => fmtInt(v) } },
        y: { grid: { display: false }, ticks: { color: TICK } },
      },
    },
  });

  const tbody = document.getElementById('tbody-error-keys');
  const rows = data.by_key || [];
  if (!rows.length) {
    emptyRow(tbody, 7, '近 24 小时无错误记录');
    return;
  }
  tbody.innerHTML = rows
    .map(
      (r) =>
        '<tr><td>' + esc(r.key_id) + '</td>' +
        '<td>' + esc(r.pool || '—') + '</td>' +
        '<td>' + statusTag(r.status) + '</td>' +
        '<td class="num">' + fmtInt(r.requests) + '</td>' +
        '<td class="num" style="color:var(--up)">' + fmtInt(r.errors) + '</td>' +
        '<td class="num" style="color:var(--up)">' + fmtPct(r.error_rate, 2) + '</td>' +
        '<td>' + esc(r.top_error || '—') + '</td></tr>'
    )
    .join('');
}

// ------------------------------------------------------------ Key 表格

const keyState = { sort: 'ratio', order: 'desc', status: '', pool: '', limit: 100 };

function renderKeys(data) {
  const tbody = document.getElementById('tbody-keys');
  document.getElementById('keys-total').textContent =
    '共 ' + fmtInt(data.total) + ' 个 Key，当前显示 ' + fmtInt((data.items || []).length) + ' 个';

  const items = data.items || [];
  if (!items.length) {
    emptyRow(tbody, 13, '暂无 Key 数据（volc_keys 表为空或筛选无结果）');
    return;
  }

  // 操作列由 admin.js 提供。它未加载时（例如将来剥离出只读部署）此处退化为空单元格，
  // 表头列数不变，不会出现错位。
  const rowOps = (window.FKAdmin && window.FKAdmin.rowActions) || (() => '<td></td>');

  tbody.innerHTML = items
    .map((k) => {
      const t = k.token || {};
      return (
        '<tr>' +
        '<td>' + esc(k.key_id) + (t.hot ? '' : ' <span class="tag tag-dim">无热态</span>') + '</td>' +
        '<td>' + esc(k.pool || '—') + '</td>' +
        '<td>' + statusTag(k.status) + '</td>' +
        '<td>' + gaugeHtml(t.ratio) + '</td>' +
        '<td class="num">' + fmtTokens(t.used) + '</td>' +
        '<td class="num">' + fmtTokens(t.prededuct) + '</td>' +
        '<td class="num" style="color:var(--ok)">' + fmtTokens(t.remaining) + '</td>' +
        '<td class="num">' + fmtInt(k.health_score) + '</td>' +
        '<td>' + esc(k.egress_ip || '—') + '</td>' +
        '<td>' + refreshTag(k.refresh_state) + '</td>' +
        '<td class="num">' + fmtTokens(k.today_tokens) + '</td>' +
        '<td class="num"' + (k.today_errors > 0 ? ' style="color:var(--up)"' : '') + '>' +
        fmtInt(k.today_errors) + '</td>' +
        rowOps(k) +
        '</tr>'
      );
    })
    .join('');
}

function bindKeySorting() {
  document.querySelectorAll('#table-keys th.sortable').forEach((th) => {
    th.addEventListener('click', () => {
      const field = th.dataset.sort;
      if (keyState.sort === field) {
        keyState.order = keyState.order === 'desc' ? 'asc' : 'desc';
      } else {
        keyState.sort = field;
        keyState.order = 'desc';
      }
      updateSortIndicators();
      loadKeys();
    });
  });
  updateSortIndicators();
}

function updateSortIndicators() {
  document.querySelectorAll('#table-keys th.sortable').forEach((th) => {
    const old = th.querySelector('.arrow');
    if (old) old.remove();
    if (th.dataset.sort === keyState.sort) {
      const span = document.createElement('span');
      span.className = 'arrow';
      span.textContent = keyState.order === 'desc' ? '↓' : '↑';
      th.appendChild(span);
    }
  });
}

async function loadKeys() {
  const params = new URLSearchParams({
    sort: keyState.sort,
    order: keyState.order,
    limit: String(keyState.limit),
  });
  if (keyState.status) params.set('status', keyState.status);
  if (keyState.pool) params.set('pool', keyState.pool);
  try {
    renderKeys(await fetchJSON('/api/keys?' + params.toString()));
  } catch (e) {
    emptyRow(document.getElementById('tbody-keys'), 13, '加载失败：' + e.message);
  }
}

// ------------------------------------------------------------ 配额健康

function renderQuotaHealth(data) {
  const box = document.getElementById('quota-alerts');
  const alerts = data.alerts || [];
  if (!alerts.length) {
    box.innerHTML =
      '<div class="ok-banner">配额状态正常：已检查 ' + fmtInt(data.checked_keys) +
      ' 个 Key，未发现租约泄漏或水位告警' +
      (data.lease_zset_size ? '，当前租约 ' + fmtInt(data.lease_zset_size) + ' 条' : '') +
      '</div>';
    return;
  }
  const kindText = {
    lease_leak: '租约泄漏',
    near_hard: '接近硬水位',
    drift: '对账偏差',
    refresh: '刷新异常',
  };
  box.innerHTML = alerts
    .map(
      (a) =>
        '<div class="alert ' + (a.level === 'critical' ? 'critical' : '') + '">' +
        '<span class="who">' + esc(kindText[a.kind] || a.kind) +
        (a.key_id ? ' · ' + esc(a.key_id) : '') + '</span>' +
        '<span class="msg">' + esc(a.message) + '</span></div>'
    )
    .join('');
}

// ------------------------------------------------------------ 刷新状态

function renderRefresh(data) {
  const stateBody = document.getElementById('tbody-refresh-states');
  const states = data.states || [];
  if (!states.length) {
    emptyRow(stateBody, 2, '暂无 Key 数据');
  } else {
    stateBody.innerHTML = states
      .map(
        (s) =>
          '<tr><td>' + refreshTag(s.name) + '</td>' +
          '<td class="num">' + fmtInt(s.count) + '</td></tr>'
      )
      .join('');
  }

  const riskyBody = document.getElementById('tbody-refresh-risky');
  const risky = data.risky_keys || [];
  if (!risky.length) {
    emptyRow(
      riskyBody,
      5,
      data.unconfirmed > 0
        ? '有 ' + fmtInt(data.unconfirmed) + ' 个 Key 未确认刷新，但均无用量，暂无超刷风险'
        : '全部 Key 刷新已确认'
    );
    return;
  }
  riskyBody.innerHTML = risky
    .map(
      (r) =>
        '<tr><td style="color:var(--up)">' + esc(r.key_id) + '</td>' +
        '<td>' + esc(r.pool || '—') + '</td>' +
        '<td>' + refreshTag(r.refresh_state) + '</td>' +
        '<td class="num">' + fmtTokens(r.used_after_refresh) + '</td>' +
        '<td class="num">' + fmtTokens(r.hot_used) + '</td></tr>'
    )
    .join('');
}

// ------------------------------------------------------------ 相似度

function renderSimilarity(data) {
  const summary = document.getElementById('similarity-summary');
  if (data.note) {
    summary.innerHTML = '<div class="ok-banner" style="border-left-color:var(--warn);color:var(--warn)">' +
      esc(data.note) + '</div>';
  } else if (data.alert_pairs > 0) {
    summary.innerHTML =
      '<div class="alert critical"><span class="who">相似度告警</span>' +
      '<span class="msg">在 ' + fmtInt(data.sampled_keys) + ' 个 Key 的 ' +
      fmtInt(data.compared_pairs) + ' 组比较中，发现 ' + fmtInt(data.alert_pairs) +
      ' 组相似度超过阈值 ' + fmtPct(data.threshold, 0) +
      '（最高 ' + fmtPct(data.max_similarity) + '，平均 ' + fmtPct(data.avg_similarity) +
      '）。建议调整对应 Key 的 persona 以拉开行为差异。</span></div>';
  } else {
    summary.innerHTML =
      '<div class="ok-banner">行为差异度良好：' + fmtInt(data.compared_pairs) +
      ' 组比较中无超过阈值 ' + fmtPct(data.threshold, 0) + ' 的 Key 对，最高相似度 ' +
      fmtPct(data.max_similarity) + '</div>';
  }

  const tbody = document.getElementById('tbody-similarity');
  const pairs = data.pairs || [];
  if (!pairs.length) {
    emptyRow(tbody, 7, '无超过阈值的 Key 对');
    return;
  }
  tbody.innerHTML = pairs
    .map((p) => {
      const egress = p.same_egress
        ? '<span class="tag tag-up">同一出口 ' + esc(p.egress_a) + '</span>'
        : esc((p.egress_a || '—') + ' / ' + (p.egress_b || '—'));
      return (
        '<tr><td>' + esc(p.key_a) + '</td><td>' + esc(p.key_b) + '</td>' +
        '<td class="num" style="color:var(--up)">' + fmtPct(p.similarity) + '</td>' +
        '<td class="num">' + fmtPct(p.hour_similarity) + '</td>' +
        '<td class="num">' + fmtPct(p.model_similarity) + '</td>' +
        '<td>' + egress + '</td>' +
        '<td><span class="tag ' + (p.level === 'critical' ? 'tag-up' : 'tag-warn') + '">' +
        (p.level === 'critical' ? '严重' : '警告') + '</span></td></tr>'
      );
    })
    .join('');
}

// ------------------------------------------------------------ 出口 IP

function renderEgress(data) {
  const tbody = document.getElementById('tbody-egress');
  const items = data.items || [];
  const note = [];
  // stale 必须最先、最显眼地说明: 此时状态列全是「未知」而非真实值，
  // 把它当实时数据看会得出「出口都健康」的错误结论。
  if (data.stale) {
    note.push(
      '<span style="color:var(--up)">⚠ 无法从网关取到实时状态' +
        (data.stale_reason ? '（' + esc(data.stale_reason) + '）' : '') +
        '，状态与信誉列不可用。用量数据仍来自数据库，准确。</span>'
    );
  }
  if (data.unbound_keys > 0) note.push('未绑定出口 IP 的 Key：' + fmtInt(data.unbound_keys) + ' 个');
  if ((data.orphan_ips || []).length) {
    note.push(
      '有流水但已不在出口池中的 IP：' + data.orphan_ips.map(esc).join('、') +
        '（配置里已移除，其上 Key 需重新绑定）'
    );
  }
  document.getElementById('egress-note').innerHTML = note.join(' ｜ ');

  if (!items.length) {
    emptyRow(tbody, 10, '出口池为空（direct 模式下属正常）');
    return;
  }
  tbody.innerHTML = items
    .map((e) => {
      // banned 是其上 Key 全部不可用的状态；suspect / cooldown 只是观察中，
      // 用不同颜色区分紧急程度。
      let stateCls = 'tag-dim';
      if (e.state === 'active') stateCls = 'tag-ok';
      else if (e.state === 'banned') stateCls = 'tag-up';
      else if (e.state) stateCls = 'tag-warn';

      // banned 分两种，处置完全不同，必须让运维一眼分清:
      //   有 unban_at → 冷却期届满会自动转 cooldown 重新探测，等着就行
      //   无 unban_at → 未配 ban_cooldown，不会自愈，得人工介入
      // 只显示「banned」而不区分，运维会对着永久封禁的出口干等。
      let banNote = '';
      if (e.state === 'banned') {
        if (e.unban_at) {
          const mins = Math.round((new Date(e.unban_at) - Date.now()) / 60000);
          // 已过期但状态仍是 banned，说明健康探测这一轮还没跑到（间隔 15 秒）
          const when = mins > 0 ? '约 ' + fmtMinutes(mins) + '后' : '即将';
          banNote =
            '<div class="meta" title="预计 ' + esc(e.unban_at) + ' 转入 cooldown 重新探测">' +
            when + '重试' +
            (e.ban_count > 1 ? '（第 ' + fmtInt(e.ban_count) + ' 次被封）' : '') +
            '</div>';
        } else {
          banNote =
            '<div class="meta" style="color:var(--up)" ' +
            'title="未配置 ban_cooldown，该出口不会自动恢复">需人工介入</div>';
        }
      }

      // 库里记的绑定数与网关内存里的不一致，说明两者已漂移 ——
      // 通常是网关重启后重新分配了绑定但没落库，需要运维知道。
      const drift =
        e.db_bound_keys != null && e.bound_keys != null && e.db_bound_keys !== e.bound_keys
          ? ' <span class="meta" style="color:var(--warn)" title="库中记录 ' +
            fmtInt(e.db_bound_keys) + ' 个，与网关内存不一致">⚠</span>'
          : '';

      // 差值 = 占着出口容量但不可用的 Key。它们仍算进 max_keys 配额，
      // 是容量吃紧时第一批该清理的对象。
      const dead = Math.max(0, (e.bound_keys || 0) - (e.active_keys || 0));
      const activeCell =
        fmtInt(e.active_keys) +
        (dead > 0
          ? ' <span class="meta" style="color:var(--warn)">(' + fmtInt(dead) + ' 不可用)</span>'
          : '');

      return (
        '<tr><td>' + esc(e.addr) + '</td>' +
        '<td>' + esc(e.public_ip || '—') + '</td>' +
        '<td><span class="tag tag-dim">' + esc(e.pool || 'any') + '</span></td>' +
        '<td><span class="tag ' + stateCls + '">' + esc(e.state || '未知') + '</span>' +
        banNote + '</td>' +
        '<td class="num">' + fmtInt(e.reputation) + '</td>' +
        '<td>' + gaugeHtml(e.load_ratio) +
        '<span class="meta">' + fmtInt(e.bound_keys) + ' / ' + fmtInt(e.max_keys) + '</span>' +
        drift + '</td>' +
        '<td class="num">' + activeCell + '</td>' +
        '<td class="num">' + fmtTokens(e.today_tokens) + '</td>' +
        '<td class="num">' + fmtInt(e.today_requests) + '</td>' +
        '<td class="num"' + (e.error_rate >= 0.05 ? ' style="color:var(--up)"' : '') + '>' +
        fmtPct(e.error_rate, 2) + '</td></tr>'
      );
    })
    .join('');
}

// ------------------------------------------------------------ 用户用量

function renderUsers(data) {
  const tbody = document.getElementById('tbody-users');
  const items = data.items || [];
  if (!items.length) {
    emptyRow(tbody, 10, '暂无用量记录');
    return;
  }
  tbody.innerHTML = items
    .map((u) => {
      const overLimit =
        u.daily_token_limit > 0 && u.total_tokens / Math.max(1, u.active_days) > u.daily_token_limit;
      return (
        '<tr><td>' + esc(u.user_name) + (u.user_id ? ' <span class="meta">#' + u.user_id + '</span>' : '') + '</td>' +
        '<td>' + esc(u.email || '—') + '</td>' +
        '<td>' + statusTag(u.status || '—') + '</td>' +
        '<td class="num" style="color:var(--up)">' + fmtTokens(u.total_tokens) + '</td>' +
        '<td class="num">' + fmtTokens(u.prompt_tokens) + '</td>' +
        '<td class="num">' + fmtTokens(u.completion_tokens) + '</td>' +
        '<td class="num">' + fmtInt(u.requests) + '</td>' +
        '<td class="num"' + (u.error_rate >= 0.05 ? ' style="color:var(--up)"' : '') + '>' +
        fmtPct(u.error_rate, 2) + '</td>' +
        '<td class="num"' + (overLimit ? ' style="color:var(--up)"' : '') + '>' +
        (u.daily_token_limit > 0 ? fmtTokens(u.daily_token_limit) : '不限') + '</td>' +
        '<td class="num">' + fmtInt(u.active_days) + '</td></tr>'
      );
    })
    .join('');
}

// ------------------------------------------------------------ 加载编排

/** 逐个加载，单个接口失败不影响其他面板。 */
async function loadOne(url, render, onFail) {
  try {
    render(await fetchJSON(url));
    return true;
  } catch (e) {
    if (onFail) onFail(e);
    return false;
  }
}

async function loadAll() {
  const days = document.getElementById('sel-days').value;
  const results = await Promise.all([
    loadOne('/api/overview', renderCards),
    loadOne('/api/usage/trend?days=' + days, renderTrend),
    loadOne('/api/errors?hours=24', renderErrors, () =>
      emptyRow(document.getElementById('tbody-error-keys'), 7, '加载失败')
    ),
    loadOne('/api/quota/health', renderQuotaHealth, (e) => {
      document.getElementById('quota-alerts').innerHTML =
        '<div class="alert critical"><span class="msg">配额健康数据加载失败：' + esc(e.message) + '</span></div>';
    }),
    loadOne('/api/refresh/status', renderRefresh, () =>
      emptyRow(document.getElementById('tbody-refresh-risky'), 5, '加载失败')
    ),
    loadOne('/api/behavior/similarity?days=' + days, renderSimilarity, (e) => {
      document.getElementById('similarity-summary').innerHTML =
        '<div class="alert"><span class="msg">相似度报告加载失败：' + esc(e.message) + '</span></div>';
    }),
    loadOne('/api/egress', renderEgress, () =>
      emptyRow(document.getElementById('tbody-egress'), 10, '加载失败')
    ),
    loadOne('/api/usage/by-user?days=30', renderUsers, () =>
      emptyRow(document.getElementById('tbody-users'), 10, '加载失败')
    ),
    loadKeys(),
  ]);
  const failed = results.filter((ok) => ok === false).length;
  showError(failed ? '有 ' + failed + ' 个数据源加载失败，请检查 /healthz' : '');
}

function init() {
  bindKeySorting();
  document.getElementById('btn-refresh').addEventListener('click', loadAll);
  document.getElementById('sel-days').addEventListener('change', loadAll);
  document.getElementById('sel-status').addEventListener('change', (e) => {
    keyState.status = e.target.value;
    loadKeys();
  });
  document.getElementById('sel-pool').addEventListener('change', (e) => {
    keyState.pool = e.target.value;
    loadKeys();
  });
  document.getElementById('sel-limit').addEventListener('change', (e) => {
    keyState.limit = Number(e.target.value) || 100;
    loadKeys();
  });
  loadAll();
  // 30 秒自动刷新。看板 QPS 极低，对网关无压力。
  setInterval(loadAll, 30000);
}

document.addEventListener('DOMContentLoaded', init);
