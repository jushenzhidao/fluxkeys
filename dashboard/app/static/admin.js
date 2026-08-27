/* FluxKeys 管理控制台：Key 状态操作与出口 IP 变更。
 *
 * 本次只做三项写操作，导入在 admin-import.js。建用户与签发 API Key 仍走直连，
 * 不在本界面内。依赖 admin-ui.js（弹层/提示/写请求）与 app.js（esc / statusTag / loadKeys）。
 *
 * 两条与后端语义强耦合的约定，改动前务必先读 docs/dashboard-admin-console-spec.md：
 * 1. banned / invalid 是调度器眼中的终态，转回 active 必须显式 force（Spec 3.3）。
 * 2. 出口 IP 变更走独立端点，因为它需要 Rebind 丢弃旧连接这一副作用（Spec 3.2）；
 *    该绑定是终身关系，界面必须让运维意识到变更代价。
 */

'use strict';

(function (A) {
  const STATUS_LABEL = {
    active: '在用 active',
    cooldown: '冷却 cooldown',
    banned: '封禁 banned',
    invalid: '失效 invalid',
  };
  const TERMINAL = { banned: 1, invalid: 1 };
  const POOLS = [
    { value: 'hot', text: 'hot' },
    { value: 'warm', text: 'warm' },
    { value: 'cold', text: 'cold' },
  ];

  function label(s) {
    return STATUS_LABEL[s] || (s || '未知');
  }

  function transition(from, to) {
    return '<span class="flow">' + statusTag(from) + A.icon('arrow') + statusTag(to) + '</span>';
  }

  // ---------------------------------------------------------------- 行内操作

  /** 由 app.js 的 renderKeys 调用，为每行 Key 追加操作单元格。 */
  A.rowActions = function (k) {
    const id = esc(k.key_id);
    const st = k.status || '';
    const pool = esc(k.pool || '');
    const ip = esc(k.egress_ip || '');
    const base = ' data-key="' + id + '" data-status="' + esc(st) + '" data-pool="' + pool + '"';
    const btns = [];
    if (TERMINAL[st]) {
      btns.push('<button type="button" class="icon-btn" data-act="restore"' + base +
        ' title="恢复为 active（终态需强制确认）" aria-label="恢复 ' + id + '">' +
        A.icon('restore') + '</button>');
    } else {
      btns.push('<button type="button" class="icon-btn danger" data-act="ban"' + base +
        ' title="封禁该 Key" aria-label="封禁 ' + id + '">' + A.icon('ban') + '</button>');
    }
    btns.push('<button type="button" class="icon-btn" data-act="pool"' + base +
      ' title="调整所属池" aria-label="调整 ' + id + ' 的池">' + A.icon('layers') + '</button>');
    btns.push('<button type="button" class="icon-btn" data-act="ip"' + base +
      ' data-ip="' + ip + '" title="变更出口 IP（终身绑定，慎用）" aria-label="变更 ' + id +
      ' 的出口 IP">' + A.icon('network') + '</button>');
    return '<td class="ops">' + btns.join('') + '</td>';
  };

  function reasonField(placeholder) {
    return {
      name: 'reason',
      label: '变更原因（进审计，便于日后回溯）',
      placeholder: placeholder,
      maxlength: 500,
    };
  }

  // ---------------------------------------------------------------- 状态操作

  async function patchKey(keyId, body, okMsg) {
    try {
      const r = await A.request('PATCH', '/api/admin/keys/' + encodeURIComponent(keyId), body);
      const changed = (r && r.changed) || [];
      A.notify(
        'ok',
        okMsg,
        changed.length
          ? '已变更字段：' + changed.join('、')
          : '目标值与当前值一致，未产生字段变更（审计已记录本次操作）'
      );
      loadKeys();
    } catch (e) {
      const hint =
        e.status === 409
          ? '状态在你操作期间已被改动，或终态恢复缺少强制确认。请刷新后重新确认当前状态。'
          : e.status === 404
            ? '该 Key 在库中不存在，列表可能已过期，请刷新。'
            : e.status === 504 || e.status === 0
              ? '请勿直接重试，先刷新列表确认是否已生效。'
              : '';
      A.notify('err', '操作失败：' + e.message, hint);
    }
  }

  async function onBan(btn) {
    const keyId = btn.dataset.key;
    const cur = btn.dataset.status;
    const r = await A.confirm({
      title: '封禁 Key',
      danger: true,
      icon: 'ban',
      confirmLabel: '确认封禁',
      rows: [
        { label: 'Key ID', value: keyId },
        { label: '状态变更', html: transition(cur, 'banned') },
        { label: '所属池', value: btn.dataset.pool || '—' },
      ],
      warn:
        '封禁后调度器立即停止向该 Key 派发请求，进行中的请求不受影响。banned 是终态，' +
        '不会自动恢复，之后要放回流量必须显式强制确认。',
      fields: [reasonField('例：火山侧提示异常，先隔离观察')],
    });
    if (!r) return;
    await patchKey(
      keyId,
      // expected_status 带上界面看到的状态：若期间已被他人改动则返回 409，
      // 避免把一个刚被别人恢复的 Key 又按旧视图封掉。
      { status: 'banned', expected_status: cur, reason: r.values.reason || undefined },
      '已封禁 ' + keyId
    );
  }

  async function onRestore(btn) {
    const keyId = btn.dataset.key;
    const cur = btn.dataset.status;
    const r = await A.confirm({
      title: '强制恢复终态 Key',
      danger: true,
      icon: 'restore',
      confirmLabel: '确认恢复',
      rows: [
        { label: 'Key ID', value: keyId },
        { label: '状态变更', html: transition(cur, 'active') },
        { label: '当前状态含义', value: cur === 'invalid' ? '上游已拒绝该 Key 的鉴权' : '已被人工或系统封禁' },
      ],
      warn:
        '该 Key 处于终态，通常意味着上游已拒绝其鉴权或已被封禁。若根因未排查，恢复后会立即' +
        '再次失败，并向上游多贡献一次异常请求 —— 异常请求正是本项目最敏感的风控信号。',
      fields: [
        {
          type: 'checkbox',
          name: 'force',
          required: true,
          label: '我已排查根因，确认可以放回流量',
        },
        reasonField('例：已确认鉴权问题已修复'),
      ],
    });
    if (!r) return;
    await patchKey(
      keyId,
      {
        status: 'active',
        force: true,
        expected_status: cur,
        reason: r.values.reason || undefined,
      },
      '已恢复 ' + keyId + ' 为 active'
    );
  }

  async function onPool(btn) {
    const keyId = btn.dataset.key;
    const cur = btn.dataset.pool;
    const r = await A.confirm({
      title: '调整所属池',
      icon: 'layers',
      confirmLabel: '确认调整',
      rows: [
        { label: 'Key ID', value: keyId },
        { label: '当前池', value: cur || '—' },
        { label: '当前状态', value: label(btn.dataset.status) },
      ],
      fields: [
        {
          type: 'select',
          name: 'pool',
          label: '目标池',
          value: cur,
          options: POOLS,
          hint: 'pool 目前是纯标签：调度器读入后未参与打分或过滤，改它不会立即改变派发行为。',
        },
        reasonField('例：降级到 cold 观察一周'),
      ],
    });
    if (!r) return;
    if (r.values.pool === cur) {
      A.notify('warn', '目标池与当前池相同，未提交', '如需变更请选择其他池。');
      return;
    }
    await patchKey(
      keyId,
      { pool: r.values.pool, reason: r.values.reason || undefined },
      keyId + ' 已移入 ' + r.values.pool + ' 池'
    );
  }

  // ---------------------------------------------------------------- 出口 IP

  async function onIp(btn) {
    const keyId = btn.dataset.key;
    const curIp = btn.dataset.ip;
    const r = await A.confirm({
      title: '变更出口 IP',
      danger: true,
      icon: 'network',
      confirmLabel: '确认变更并重新绑定',
      rows: [
        { label: 'Key ID', value: keyId },
        { label: '当前出口 IP', value: curIp || '（未绑定，由出口池按哈希分配）' },
        { label: '当前状态', value: label(btn.dataset.status) },
      ],
      warn:
        '出口 IP 是终身绑定关系。变更等于把一个有完整历史的老账号变成「换了地址的账号」，' +
        '这是上游风控最敏感的信号之一，可能直接触发对该 Key 的复核。同时旧连接会被立即丢弃，' +
        '正在进行的请求会失败一次。仅在旧 IP 确认不可用时才做。',
      fields: [
        {
          type: 'checkbox',
          name: 'ack',
          required: true,
          label: '我理解此变更不可撤销，且会成为风控可见的地址变更记录',
        },
        {
          name: 'egress_ip',
          label: '期望的出口 IP（可留空，留空则由出口池分配；此值仅进审计）',
          placeholder: '例：203.0.113.24',
        },
      ],
    });
    if (!r) return;
    const body = {};
    const want = (r.values.egress_ip || '').trim();
    if (want) body.egress_ip = want;
    try {
      const res = await A.request('PUT', '/api/admin/keys/' + encodeURIComponent(keyId) + '/ip', body);
      A.notify(
        'ok',
        keyId + ' 出口 IP 已重新绑定',
        (res && res.old_ip ? '原 ' + res.old_ip + '，新 ' + (res.new_ip || '未返回') : '') +
          (res && res.switch_status ? '（' + res.switch_status + '）' : '')
      );
      loadKeys();
    } catch (e) {
      A.notify(
        'err',
        '出口 IP 变更失败：' + e.message,
        e.status === 503
          ? '出口池当前无可用 IP，请先检查 egress_ips 表与出口机状态。'
          : e.status === 504 || e.status === 0
            ? '请勿直接重试，先刷新列表确认绑定是否已切换。'
            : ''
      );
    }
  }

  // ---------------------------------------------------------------- 绑定

  const HANDLERS = { ban: onBan, restore: onRestore, pool: onPool, ip: onIp };

  function init() {
    // 事件委托绑到 tbody：Key 列表每次刷新都会重建行，逐行绑定会随刷新丢失。
    const tbody = document.getElementById('tbody-keys');
    if (tbody) {
      tbody.addEventListener('click', function (ev) {
        const btn = ev.target.closest('button[data-act]');
        if (!btn || btn.disabled) return;
        const fn = HANDLERS[btn.dataset.act];
        if (fn) fn(btn);
      });
    }
    // 导入按钮由 admin-import.js 自行绑定。

    const logout = document.getElementById('btn-logout');
    if (logout) {
      logout.addEventListener('click', async function () {
        try {
          await A.request('POST', '/logout');
        } catch (e) { /* 无论成功与否都回登录页：本地已无可用会话 */ }
        A.toLogin();
      });
    }
  }

  document.addEventListener('DOMContentLoaded', init);
})(window.FKAdmin);
