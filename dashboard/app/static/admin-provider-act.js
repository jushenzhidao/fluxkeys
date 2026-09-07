/* FluxKeys 管理控制台：provider 写操作的执行与两步提交编排。
 *
 * 两步提交（契约 §4.5）不是 UI 礼貌，是这组操作唯一的安全边界：dry-run 与真实
 * 生效在网关侧走同一条构造路径，所以「校验通过」这件事是可信的；而不先校验就
 * 提交，配置错误的第一个可见症状会是线上请求全部 401 或水位失真。
 *
 * 因此本文件有一条不可放宽的规则：任何字段改动都会作废上一次的校验结果并禁用
 * 「确认生效」。拿一份过期的校验通过去提交，等于没校验。
 */

'use strict';

(function (A) {
  const P = (A.prov = A.prov || {});

  function out() {
    return document.getElementById('pf-out');
  }

  function setOut(cls, html) {
    const box = out();
    if (!box) return;
    box.className = 'imp-result ' + cls;
    box.innerHTML = html;
  }

  function failList(items) {
    return '<ul class="imp-fails">' + items.map(function (f) {
        return '<li>' + (f.field ? '<code>' + esc(f.field) + '</code> ' : '') +
        esc(f.message) + '</li>';
    }).join('') + '</ul>';
  }

  function kindFlow(now, then) {
    return now + ' 改为 ' + then;
  }

  function commitBtn() {
    return document.querySelector('[data-pact="form-commit"]');
  }

  /** 作废上一次校验结果。字段一改，之前的「通过」就不再代表待提交的内容。 */
  function invalidate() {
    const f = P.getForm();
    if (!f || !f.checked) return;
    f.checked = false;
    const btn = commitBtn();
    if (btn) btn.disabled = true;
    setOut('', '<p class="imp-line">配置已改动，请重新校验后再提交。</p>');
  }

  // ---------------------------------------------------------------- 表单开合

  function openForm(p, isEdit) {
    const h = P.host();
    if (!h) return;
    P.setForm({
      isEdit: isEdit,
      name: isEdit ? p.name : '',
      // enabled 不在表单里（启停走独立动作），但 PUT 是全量替换 ——
      // 不带上当前值就会把一个启用中的 provider 顺手停掉。
      enabled: isEdit ? !!p.enabled : false,
      // 乐观锁基准是该 provider 这一行自己的 version，取自打开表单那一刻的
      // 真实响应值，不由前端推算、也不用全局版本号顶替。
      expectedVersion: isEdit ? p.version : undefined,
      checked: false,
    });
    h.innerHTML = P.formHtml(p, isEdit);
    const first = h.querySelector('input:not([readonly]), select:not([readonly])');
    if (first) first.focus();
    h.scrollIntoView({ block: 'nearest' });
  }

  function closeForm() {
    const h = P.host();
    if (h) h.innerHTML = '';
    P.setForm(null);
  }

  // ---------------------------------------------------------------- 两步提交

  async function onCheck() {
    const f = P.getForm();
    if (!f) return;
    const body = P.readForm();
    const local = P.localFails(body);
    if (local.length) {
      setOut('is-err', '<p class="imp-line">有 ' + local.length +
        ' 项需要先修正，未提交校验：</p>' + failList(local));
      return;
    }
    setOut('is-busy', '<p class="imp-line">正在校验…</p>');
    // 不带 target 字段：网关按「该 provider 是否已存在」自行判定 is_create
    // （admin_provider.go:566），且 providerBody 里没有 target 这一键 ——
    // 多发一个未知字段会让整个请求以 400 被拒。
    const payload = Object.assign({}, body);
    if (f.isEdit) payload.expected_version = f.expectedVersion;
    try {
      // 预演挂在具体 provider 下（网关无 /providers/validate）。新建时该
      // provider 还不存在，网关按 ErrProviderNotFound 判定 is_create，
      // 走的仍是这条路径 —— 不需要前端分流。
      const res = await A.request(
        'POST',
        '/api/admin/providers/' + encodeURIComponent(f.name) + '/dry-run',
        payload
      );
      renderCheck(res);
    } catch (e) {
      f.checked = false;
      const btn = commitBtn();
      if (btn) btn.disabled = true;
      setOut('is-err', '<p class="imp-line">校验请求失败：' + esc(e.message) + '</p>');
      P.notifyFail('配置校验失败', e);
    }
  }

  function renderCheck(res) {
    const f = P.getForm();
    const diff = (res && res.diff) || [];
    const warns = (res && res.warnings) || [];
    const fails = (res && res.failures) || [];
    let html = '';
    if (diff.length) {
      html += '<p class="imp-line">将变更 ' + diff.length + ' 项：</p><ul class="imp-fails">' +
        diff.map(function (d) {
          return '<li><code>' + esc(d.field) + '</code> <span class="flow">' +
            esc(String(d.before)) + A.icon('arrow') + esc(String(d.after)) +
            '</span></li>';
        }).join('') + '</ul>';
    }
    // 网关的字段名是 valid，不是 ok。严格判 !== true 而非 falsy：
    // 字段缺失（例如打到了旧版网关）也必须当作未通过，绝不能因为
    // undefined 被当成「没报错就是通过」而放开提交按钮。
    if (!res || res.valid !== true) {
      f.checked = false;
      const btn = commitBtn();
      if (btn) btn.disabled = true;
      setOut('is-err',
        '<p class="imp-line">校验未通过，配置未提交。以下 ' + fails.length +
        ' 项需要修正：</p>' + failList(fails) + html);
      fails.forEach(function (x) {
        if (x.field) {
          const el = document.querySelector('[data-pf="' + x.field.split('.')[0] + '"]');
          if (el) el.setAttribute('aria-invalid', 'true');
        }
      });
      return;
    }
    f.checked = true;
    const btn = commitBtn();
    if (btn) btn.disabled = false;
    if (warns.length) {
      html += '<p class="imp-line">有 ' + warns.length + ' 项需要你确认后果：</p>' +
        '<ul class="imp-fails">' + warns.map(function (w) {
          return '<li>' + esc(w.message) + '</li>';
        }).join('') + '</ul>';
    }
    setOut('is-ok', '<p class="imp-line">校验通过，尚未生效。' +
      '确认上面的变更后点「确认生效」。</p>' + html);
  }

  async function onCommit() {
    const f = P.getForm();
    if (!f || !f.checked) return;
    const body = P.readForm();
    const btn = commitBtn();
    lock(true);
    setOut('is-busy', '<p class="imp-line">正在提交…</p>');
    try {
      let res;
      if (f.isEdit) {
        body.expected_version = f.expectedVersion;
        // PUT 是全量替换，不是 PATCH 合并。表单本来就渲染并回读了全部字段，
        // 所以这里提交的是一份完整配置 —— 但正因为是全量，任何被表单漏掉的
        // 字段都会被写成零值，而不是保持原样。新增字段时必须同步进表单。
        //
        // name 保留在请求体里：网关拿它与路径比对，不一致就报 name_immutable
        // （admin_provider.go:255）。删掉它等于放弃这道校验。
        res = await A.request('PUT', '/api/admin/providers/' + encodeURIComponent(f.name), body);
      } else {
        res = await A.request('POST', '/api/admin/providers', body);
      }
      const name = (res && res.name) || f.name;
      A.notify(
        'ok',
        f.isEdit ? name + ' 配置已更新' : name + ' 已创建（默认停用）',
        reloadNote(res) +
          (f.isEdit ? '' : '请先确认凭据与模型映射无误，再用列表里的启用按钮启用它。')
      );
      closeForm();
      P.reload();
    } catch (e) {
      lock(false);
      setOut('is-err', '<p class="imp-line">提交失败，配置未变更：' + esc(e.message) + '</p>');
      P.notifyFail(f.isEdit ? '更新配置失败' : '创建 provider 失败', e);
    }
    if (btn && !P.getForm()) return;
  }

  function reloadNote(res) {
    if (res && res.reloaded === false) {
      return '配置已落库，但本实例热加载未成功' +
        (res.reload_error ? '（' + res.reload_error + '）' : '') +
        '：该实例仍在用旧配置，请查它的日志。';
    }
    return '已即时生效，无需重启。';
  }

  function lock(on) {
    const h = P.host();
    if (!h) return;
    h.querySelectorAll('button').forEach((b) => { b.disabled = on; });
    h.querySelectorAll('input, select').forEach(function (el) {
      if (on) el.setAttribute('data-was-ro', el.readOnly ? '1' : '0');
      el.readOnly = on ? true : el.getAttribute('data-was-ro') === '1';
    });
  }

  // ---------------------------------------------------------------- 行内操作

  function reasonField(placeholder) {
    return { name: 'reason', label: '变更原因（进审计，便于日后回溯）',
             placeholder: placeholder, maxlength: 500, required: true };
  }

  async function onDisable(name) {
    const p = P.find(name);
    if (!p) return;
    const rows = [{ k: 'provider', v: name }, { k: '当前版本', v: '#' + p.version }];
    const last = P.enabledCount() <= 1;
    const r = await A.confirm({
      title: '停用 ' + name,
      icon: 'icon-ban',
      danger: true,
      rows: rows,
      warn: '停用后它不再出现在 /v1/models、不接受新 Key 导入、不再被调度派发。' +
        '在途请求仍会完成，租约由 reap 正常回收。历史用量与版本记录完整保留。' +
        (last ? '这是当前唯一启用的 provider，停用它会让所有请求无处派发 —— 网关会拒绝这次操作。' : ''),
      fields: [reasonField('为什么要停用这个 provider')],
      confirmLabel: '停用',
    });
    if (!r) return;
    // 停用走 DELETE（网关侧是软删除，无 /disable 端点）。语义仍是停用：
    // 配置行、历史版本、归档用量都保留，只是 enabled 置否。
    await writeAct('DELETE', '/api/admin/providers/' + encodeURIComponent(name),
      { expected_version: p.version, reason: r.values.reason },
      name + ' 已停用', '停用 ' + name + ' 失败');
  }

  async function onEnable(name) {
    const p = P.find(name);
    if (!p) return;
    const r = await A.confirm({
      title: '启用 ' + name,
      icon: 'icon-restore',
      rows: [{ k: 'provider', v: name }, { k: '凭据', v: p.credential_present ? '已配置' : '缺失' }],
      warn: p.credential_present
        ? '启用后它立刻开始承接流量。'
        : '该 provider 的凭据环境变量在网关进程里没有值，启用后派往它的请求会全部 401。' +
          '请先在部署环境设置该变量并重启网关。',
      fields: [reasonField('为什么要启用这个 provider')],
      confirmLabel: '启用',
    });
    if (!r) return;
    // 网关没有 /enable 端点，启用是「把 enabled 置真的一次全量更新」。
    //
    // 必须先 GET 拿完整当前配置再 PUT：PUT 是全量替换，只发
    // {enabled, expected_version} 会把 base_url、quota_limit、模型映射
    // 全部写成零值 —— 一次「启用」变成一次静默的配置清空。
    // 列表接口返回的是精简视图，字段不全，不能拿它当 PUT 的基底。
    let full;
    try {
      const got = await A.request('GET', '/api/admin/providers/' + encodeURIComponent(name));
      full = (got && got.provider) || got;
    } catch (e) {
      P.notifyFail('读取 ' + name + ' 当前配置失败', e);
      return;
    }
    if (!full || !full.base_url) {
      A.notify('err', '启用 ' + name + ' 已中止',
        '未能取到该 provider 的完整配置，继续提交会把现有配置覆盖成空值。请刷新后重试。');
      return;
    }
    const body = Object.assign({}, full, {
      enabled: true,
      reason: r.values.reason,
      expected_version: p.version,
    });
    await writeAct('PUT', '/api/admin/providers/' + encodeURIComponent(name), body,
      name + ' 已启用', '启用 ' + name + ' 失败');
  }

  // 「设为默认」已整条移除，不是漏做。
  //
  // default_provider 只是 YAML 里的一个键（internal/config/config.go:40），
  // 不在 provider_configs 表里，也没有任何管理端点能改它 —— 改它要编辑
  // 配置文件并重启网关，本页的热生效范围覆盖不到。
  // 留一个按钮在这儿只会稳定地 404，而运维会以为是权限或网络问题。

  async function writeAct(method, url, body, okMsg, failPrefix) {
    try {
      const res = await A.request(method, url, body);
      A.notify('ok', okMsg, reloadNote(res));
      P.reload();
    } catch (e) {
      P.notifyFail(failPrefix, e);
    }
  }

  // ---------------------------------------------------------------- 版本快照

  // 版本详情挂在 provider 下。版本号虽然全局单调，但网关会校验该版本是否
  // 属于路径里的 provider，不属于就回 404（admin_provider.go:455）——
  // 所以这里必须带上当前正在看谁的历史，不能只凭版本号去查。
  function versionUrl(version) {
    return '/api/admin/providers/' +
      encodeURIComponent(P.state.versionsProvider) +
      '/versions/' + encodeURIComponent(version);
  }

  async function onViewVersion(version) {
    try {
      const got = await fetchJSON(versionUrl(version));
      // 网关把它包在 {version: ...} 里；展示快照本体而不是这层信封。
      showSnapshot(version, (got && got.version) || got);
    } catch (e) {
      A.notify('err', '读取版本 #' + version + ' 失败：' + e.message, '');
    }
  }

  function showSnapshot(version, snap) {
    const root = document.getElementById('modal-root');
    if (!root) return;
    root.innerHTML =
      '<div class="modal-mask"><div class="modal modal-wide" role="dialog" aria-modal="true" ' +
      'aria-label="版本 ' + esc(version) + ' 的配置快照">' +
      '<h3 class="modal-title-plain">' +
      '<svg class="icon" aria-hidden="true"><use href="#icon-layers"></use></svg>' +
      '版本 #' + esc(version) + ' 配置快照（只读）</h3>' +
      '<pre class="snap-body">' + esc(JSON.stringify(snap, null, 2)) + '</pre>' +
      '<div class="modal-actions">' +
      '<button class="btn" type="button" data-pact="snap-close">关闭</button></div>' +
      '</div></div>';
    const btn = root.querySelector('[data-pact="snap-close"]');
    btn.focus();
    function close() {
      root.innerHTML = '';
      document.removeEventListener('keydown', onKey);
    }
    function onKey(ev) {
      if (ev.key === 'Escape') close();
    }
    btn.addEventListener('click', close);
    document.addEventListener('keydown', onKey);
  }

  async function onRollback(version) {
    const target = Number(version);
    const pname = P.state.versionsProvider;
    let snap;
    try {
      const got = await fetchJSON(versionUrl(target));
      const v = (got && got.version) || got;
      // 列表接口不带 snapshot，只有单版本详情才填（provider_deps.go:72）。
      // 取不到快照就无法判断量纲是否一致 —— 此时必须中止，不能默认放行。
      snap = v && v.snapshot;
    } catch (e) {
      A.notify('err', '读取目标版本失败：' + e.message, '回滚未执行。');
      return;
    }
    if (!snap) {
      A.notify('err', '回滚到 #' + target + ' 已中止',
        '目标版本没有返回配置快照，无法确认它的配额量纲与当前是否一致。' +
        '请刷新版本历史后重试。');
      return;
    }
    // 跨量纲回滚在这里就被拦下，请求不会发出。这不是一个「确认后放行」的分支：
    // 次数与 token 数之间没有确定的换算关系，Redis 里已累加的计数无法换算，
    // 所以不存在一个能让这次回滚变安全的确认动作。
    const cur = P.find(pname);
    if (cur && snap.quota_kind && snap.quota_kind !== cur.quota_kind) {
      A.notify(
        'err',
        '回滚到 #' + target + ' 被拒绝：配额量纲不一致',
        pname + ' 的量纲会从 ' + kindFlow(cur.quota_kind, snap.quota_kind) +
        '。回滚它会让 Redis 中已按当前量纲累加的计数被按目标量纲解读，水位整体失真且不报错。' +
        '若你只是想恢复某个 quota_limit，请用编辑表单单独改那一项。'
      );
      return;
    }
    const r = await A.confirm({
      title: '回滚到版本 #' + target,
      icon: 'icon-rollback',
      danger: true,
      rows: [
        { k: 'provider', v: pname },
        // 该 provider 自己的当前版本号；state.activeVersion 是全局热加载
        // 计数，与 provider_versions.id 不同号段，显示它会误导操作人。
        { k: '当前版本', v: cur ? '#' + cur.version : '未知' },
        { k: '回滚到', v: '#' + target },
      ],
      warn: '回滚会生成一个新版本，不会抹掉历史。若目标版本引用的环境变量现在已不存在，' +
        '或它的 adapter 已不被支持，网关会拒绝这次回滚而不是回滚出一份跑不起来的配置。',
      fields: [reasonField('为什么要回滚到这个版本')],
      confirmLabel: '回滚',
    });
    if (!r) return;
    // 乐观锁基准取该 provider 自己的版本，不是全局 activeVersion ——
    // 网关比对的是 provider_configs 里这一行的版本号。
    //
    // 请求体里不含 confirm_quota_kind_change：界面上没有任何入口能置真，
    // 它在网关侧是一道默认拒绝的安全网，不是本页的确认位。
    await writeAct('POST',
      '/api/admin/providers/' + encodeURIComponent(pname) + '/rollback',
      {
        target_version_id: target,
        expected_version: cur ? cur.version : undefined,
        reason: r.values.reason,
      },
      pname + ' 已回滚到 #' + target, '回滚到 #' + target + ' 失败');
  }

  // ---------------------------------------------------------------- 绑定

  P.handlers = {
    'prov-edit': function (name) {
      const p = P.find(name);
      if (p) openForm(p, true);
    },
    'prov-disable': onDisable,
    'prov-enable': onEnable,
    // 无 prov-default：默认 provider 只能改 YAML，见上方说明。
    'cfg-view': onViewVersion,
    'cfg-rollback': onRollback,
  };

  function init() {
    const h = P.host();
    if (h) {
      h.addEventListener('click', function (ev) {
        const btn = ev.target.closest('button[data-pact]');
        if (!btn || btn.disabled) return;
        const act = btn.dataset.pact;
        if (act === 'form-cancel') closeForm();
        else if (act === 'form-check') onCheck();
        else if (act === 'form-commit') onCommit();
        else if (act === 'map-add') {
          const rows = document.getElementById('pf-map');
          if (rows) rows.insertAdjacentHTML('beforeend', mapRow());
          invalidate();
        } else if (act === 'map-del') {
          const row = btn.closest('.map-row');
          if (row) row.remove();
          invalidate();
        }
      });
      // 任何输入都作废上一次校验结果，「确认生效」随之禁用。
      h.addEventListener('input', invalidate);
      h.addEventListener('change', invalidate);
    }

    const add = document.getElementById('btn-prov-add');
    if (add) add.addEventListener('click', () => openForm(null, false));
  }

  function mapRow() {
    return '<div class="map-row">' +
      '<input type="text" data-map="pub" placeholder="对外模型名" aria-label="对外模型名">' +
      '<svg class="icon" aria-hidden="true"><use href="#icon-arrow"></use></svg>' +
      '<input type="text" data-map="up" placeholder="上游模型名" aria-label="上游模型名">' +
      '<button class="icon-btn" type="button" data-pact="map-del" title="删除这条映射" aria-label="删除这条映射">' +
      '<svg class="icon" aria-hidden="true"><use href="#icon-close"></use></svg></button></div>';
  }

  document.addEventListener('DOMContentLoaded', init);
})(window.FKAdmin);
