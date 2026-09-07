/* FluxKeys 管理控制台通用 UI 层：图标引用、提示条、确认弹层、写请求封装。
 *
 * 无框架、无构建、无 npm、无 CDN —— 与 app.js 保持同一约束。
 * 依赖 app.js 先加载（复用其 esc / statusTag），index.html 的 script 顺序保证这一点。
 * 本文件不产出任何色值，配色全部由 admin.css 的 CSS 变量决定。
 *
 * 图标一律引用 index.html 底部的 Lucide sprite，禁止 emoji 与 Unicode 符号字符。
 */

'use strict';

window.FKAdmin = window.FKAdmin || {};

(function (A) {
  // ---------------------------------------------------------------- 图标

  /** 按功能名引用 sprite 中的 symbol。name 由调用方硬编码，不来自用户输入。 */
  A.icon = function (name, extraCls) {
    return (
      '<svg class="icon' + (extraCls ? ' ' + extraCls : '') + '" aria-hidden="true">' +
      '<use href="#icon-' + name + '"></use></svg>'
    );
  };

  // ---------------------------------------------------------------- 提示条

  /** kind: ok | warn | err。ok 自动消失，其余需手动关闭（错误文案往往要照着做处置）。 */
  A.notify = function (kind, msg, detail) {
    const root = document.getElementById('toast-root');
    if (!root) return null;
    const el = document.createElement('div');
    el.className = 'toast toast-' + kind;
    el.setAttribute('role', kind === 'ok' ? 'status' : 'alert');
    el.innerHTML =
      A.icon(kind === 'ok' ? 'check' : 'alert', 'toast-ico') +
      '<div class="toast-body"><p class="toast-msg">' + esc(msg) + '</p>' +
      (detail ? '<p class="toast-detail">' + esc(detail) + '</p>' : '') +
      '</div>' +
      '<button type="button" class="icon-btn toast-x" aria-label="关闭提示">' +
      A.icon('close') + '</button>';
    el.querySelector('.toast-x').addEventListener('click', () => el.remove());
    root.appendChild(el);
    if (kind === 'ok') setTimeout(() => el.remove(), 6000);
    return el;
  };

  // ---------------------------------------------------------------- 确认弹层

  function fieldsHtml(fields, uid) {
    if (!fields || !fields.length) return '';
    return '<div class="modal-fields">' + fields.map(function (f, i) {
      const id = 'fld-' + uid + '-' + i;
      if (f.type === 'checkbox') {
        return '<label class="fld-check" for="' + id + '">' +
          '<input type="checkbox" id="' + id + '" data-name="' + esc(f.name) + '"' +
          (f.required ? ' data-required="1"' : '') + '>' +
          '<span>' + esc(f.label) + '</span></label>';
      }
      if (f.type === 'select') {
        return '<label class="fld" for="' + id + '"><span class="fld-label">' + esc(f.label) + '</span>' +
          '<select id="' + id + '" data-name="' + esc(f.name) + '">' +
          (f.options || []).map(function (o) {
            return '<option value="' + esc(o.value) + '"' +
              (o.value === f.value ? ' selected' : '') + '>' + esc(o.text) + '</option>';
          }).join('') + '</select>' +
          (f.hint ? '<span class="fld-hint">' + esc(f.hint) + '</span>' : '') + '</label>';
      }
      return '<label class="fld" for="' + id + '"><span class="fld-label">' + esc(f.label) + '</span>' +
        '<input type="text" id="' + id + '" data-name="' + esc(f.name) + '"' +
        (f.placeholder ? ' placeholder="' + esc(f.placeholder) + '"' : '') +
        (f.maxlength ? ' maxlength="' + Number(f.maxlength) + '"' : '') +
        ' autocomplete="off" spellcheck="false">' +
        (f.hint ? '<span class="fld-hint">' + esc(f.hint) + '</span>' : '') + '</label>';
    }).join('') + '</div>';
  }

  /**
   * 危险操作二次确认（Spec 5.3）。刻意不用 window.confirm：它承载不了
   * 「哪个 Key、从什么改成什么、代价是什么」这三段上下文，且样式不受控。
   * 返回 Promise：确认得到 {values}，取消得到 null。
   */
  A.confirm = function (opts) {
    return new Promise(function (resolve) {
      const root = document.getElementById('modal-root');
      const restoreTo = document.activeElement;
      const uid = String(Date.now()) + String(Math.floor(Math.random() * 1000));
      const titleId = 'modal-t-' + uid;
      const mask = document.createElement('div');
      mask.className = 'modal-mask';
      mask.innerHTML =
        '<div class="modal" role="dialog" aria-modal="true" aria-labelledby="' + titleId + '">' +
        '<h3 class="modal-title" id="' + titleId + '">' + A.icon(opts.icon || 'alert') +
        '<span>' + esc(opts.title) + '</span></h3>' +
        (opts.rows && opts.rows.length
          ? '<dl class="kv">' + opts.rows.map(function (r) {
              return '<dt>' + esc(r.label) + '</dt><dd>' + (r.html || esc(r.value)) + '</dd>';
            }).join('') + '</dl>'
          : '') +
        (opts.warn
          ? '<p class="modal-warn">' + A.icon('alert') + '<span>' + esc(opts.warn) + '</span></p>'
          : '') +
        fieldsHtml(opts.fields, uid) +
        '<div class="modal-actions">' +
        '<button type="button" class="btn" data-role="cancel">取消</button>' +
        '<button type="button" class="btn ' + (opts.danger ? 'btn-danger' : 'btn-primary') +
        '" data-role="ok">' + esc(opts.confirmLabel || '确认') + '</button>' +
        '</div></div>';

      const dialog = mask.querySelector('.modal');
      const okBtn = mask.querySelector('[data-role="ok"]');
      const cancelBtn = mask.querySelector('[data-role="cancel"]');
      const gates = Array.prototype.slice.call(mask.querySelectorAll('input[data-required]'));

      function syncGate() {
        okBtn.disabled = gates.some(function (g) { return !g.checked; });
      }
      gates.forEach(function (g) { g.addEventListener('change', syncGate); });
      syncGate();

      function close(result) {
        document.removeEventListener('keydown', onKey, true);
        mask.remove();
        if (restoreTo && typeof restoreTo.focus === 'function') restoreTo.focus();
        resolve(result);
      }

      function focusables() {
        return Array.prototype.slice
          .call(dialog.querySelectorAll('button, input, select, textarea, [href]'))
          .filter(function (el) { return !el.disabled && el.offsetParent !== null; });
      }

      function onKey(ev) {
        if (ev.key === 'Escape') { ev.preventDefault(); close(null); return; }
        if (ev.key !== 'Tab') return;
        const list = focusables();
        if (!list.length) return;
        const first = list[0];
        const last = list[list.length - 1];
        if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
        else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
      }

      okBtn.addEventListener('click', function () {
        const values = {};
        dialog.querySelectorAll('[data-name]').forEach(function (el) {
          values[el.dataset.name] = el.type === 'checkbox' ? el.checked : el.value;
        });
        close({ values: values });
      });
      cancelBtn.addEventListener('click', function () { close(null); });
      mask.addEventListener('mousedown', function (ev) { if (ev.target === mask) close(null); });
      document.addEventListener('keydown', onKey, true);

      root.appendChild(mask);
      const firstField = dialog.querySelector('.modal-fields input, .modal-fields select');
      (firstField || cancelBtn).focus();
    });
  };

  // ---------------------------------------------------------------- 写请求

  A.toLogin = function () {
    // 不带 next 参数：避免开放重定向面，控制台本身就是单页，回到 / 即可。
    window.location.replace('/login');
  };

  /**
   * 写请求统一入口。抛出的 Error 带 status / gatewayCode，文案取 body.detail
   * （与 app.js 的 fetchJSON 同一约定，看板转发层已把网关错误转译成该结构）。
   *
   * opts.raw = true 时 body 按原样发送字符串，不做 JSON.stringify —— 导入清单
   * 必须保持运维粘贴的原文，重新序列化可能改变大整数与浮点的表示。
   */
  A.request = async function (method, url, body, opts) {
    const o = opts || {};
    const waitMs = o.timeoutMs || 20000;
    const ctrl = new AbortController();
    const timer = setTimeout(function () { ctrl.abort(); }, waitMs);
    let resp;
    try {
      resp = await fetch(url, {
        method: method,
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: body === undefined || body === null
          ? undefined
          : (o.raw ? String(body) : JSON.stringify(body)),
        credentials: 'same-origin',
        cache: 'no-store',
        signal: ctrl.signal,
      });
    } catch (e) {
      clearTimeout(timer);
      if (e && e.name === 'AbortError') {
        const t = new Error(
          '本地等待超过 ' + Math.round(waitMs / 1000) + ' 秒已中断。请求可能已到达网关，' +
          '请刷新后确认实际状态，不要直接重试。'
        );
        t.status = 0;
        throw t;
      }
      throw new Error('网络请求失败：' + ((e && e.message) || '未知原因'));
    }
    clearTimeout(timer);

    if (resp.status === 401) {
      // 会话过期。写请求的 401 不可能是「ADMIN_API_KEY 配错」——转发层已把网关 401 转成 500。
      A.toLogin();
      throw new Error('会话已过期，正在跳转登录');
    }

    let payload = null;
    if (resp.status !== 204) {
      try { payload = await resp.json(); } catch (e) { payload = null; }
    }
    if (!resp.ok) {
      const env = payload && payload.error;
      const err = new Error(
        (payload && payload.detail) ||
        (env && env.message) ||
        resp.statusText || ('HTTP ' + resp.status)
      );
      err.status = resp.status;
      err.gatewayCode = payload && payload.gateway_code;
      err.payload = payload;
      // provider 端点的四种 409 靠错误码分派文案，靠状态码分不出来。
      // 两种来源都要读：看板转发层（gateway/client.py）把网关的
      // {error:{code}} 信封压平成 {detail, gateway_code}，而网关被直连
      // 或将来若有端点原样透传信封时，code 仍在 error.code 里。
      err.code = err.gatewayCode || (env && env.code) || '';
      throw err;
    }
    return payload;
  };
})(window.FKAdmin);
