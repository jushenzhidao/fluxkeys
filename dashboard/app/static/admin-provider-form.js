/* FluxKeys 管理控制台：provider 配置的写操作（新增 / 编辑 / 两步提交）。
 *
 * 与 admin-provider.js 分文件：那边只读，改错了页面显示不对；这边落库并即时热
 * 生效，改错了线上配置就错了。两者的失效代价不同，不该住在同一个文件里。
 *
 * 本文件里三处不是风格偏好而是硬约束：
 * 1. provider 名与 quota_kind 在编辑态 readonly（不是 disabled —— disabled 元素
 *    不进 tab 序、读屏也读不到，而这两个字段恰恰最需要被读到并解释清楚）。
 * 2. 必须先 dry-run 才允许提交，且 dry-run 结果失效后要重新校验：拿一份过期的
 *    「校验通过」提交，等于没校验。
 * 3. credential_env 疑似填了密钥原文时判 error 并阻断，不给旁路 —— 密钥一旦进
 *    版本历史就无法收回。提示里只给正确形态的例子，不写判据。
 */

'use strict';

(function (A) {
  const P = (A.prov = A.prov || {});

  const QUOTA_KINDS = ['token', 'count'];
  const ADAPTER_KINDS = ['openai_compatible', 'volc', 'sensenova'];

  // 编辑中的表单。dryRun 存最近一次校验通过的结果；任何字段改动都会作废它。
  let form = null;

  function host() {
    return document.getElementById('prov-form-host');
  }

  // ---------------------------------------------------------------- 表单结构

  function fieldRow(f) {
    const id = 'pf-' + f.name;
    const ro = f.readonly ? ' readonly' : '';
    const lock = f.readonly
      ? '<svg class="icon" aria-hidden="true"><use href="#icon-lock"></use></svg>'
      : '';
    let input;
    if (f.type === 'select' && f.readonly) {
      // readonly 对 <select> 不是有效属性 —— 浏览器会忽略它，下拉照样能改。
      // 锁图标在旁边而字段实际可改，是最坏的组合：看起来受保护，其实不受。
      // 所以禁改的枚举字段渲染成 readonly 文本框：真的改不动，且仍在 tab 序里、
      // 读屏能读到，禁改理由才有机会被看见（这也是不用 disabled 的原因）。
      input = '<input type="text" id="' + id + '" data-pf="' + f.name +
        '" value="' + esc(f.value) + '" readonly>';
    } else if (f.type === 'select') {
      input = '<select id="' + id + '" data-pf="' + f.name + '">' +
        f.options.map((o) => '<option value="' + esc(o) + '"' +
          (String(f.value) === o ? ' selected' : '') + '>' + esc(o) + '</option>').join('') +
        '</select>';
    } else if (f.type === 'checkbox') {
      input = '<input type="checkbox" id="' + id + '" data-pf="' + f.name + '"' +
        (f.value ? ' checked' : '') + '>';
    } else {
      input = '<input type="' + (f.type || 'text') + '" id="' + id + '" data-pf="' + f.name +
        '" value="' + esc(f.value === null || f.value === undefined ? '' : f.value) + '"' +
        (f.placeholder ? ' placeholder="' + esc(f.placeholder) + '"' : '') +
        (f.maxlength ? ' maxlength="' + f.maxlength + '"' : '') + ro + '>';
    }
    return (
      '<div class="fld">' +
      '<label class="fld-label" for="' + id + '">' + lock + esc(f.label) + '</label>' +
      input +
      (f.hint ? '<p class="fld-hint">' + f.hint + '</p>' : '') +
      '</div>'
    );
  }

  function mapRowsHtml(mapping) {
    const keys = Object.keys(mapping || {});
    const rows = keys.length ? keys : [''];
    return rows.map(function (k) {
      return (
        '<div class="map-row">' +
        '<input type="text" data-map="pub" value="' + esc(k) + '" placeholder="对外模型名" aria-label="对外模型名">' +
        '<svg class="icon" aria-hidden="true"><use href="#icon-arrow"></use></svg>' +
        '<input type="text" data-map="up" value="' + esc(k ? mapping[k] : '') +
        '" placeholder="上游模型名" aria-label="上游模型名">' +
        '<button class="icon-btn" type="button" data-pact="map-del" title="删除这条映射" aria-label="删除这条映射">' +
        '<svg class="icon" aria-hidden="true"><use href="#icon-close"></use></svg></button>' +
        '</div>'
      );
    }).join('');
  }

  function formHtml(p, isEdit) {
    const v = p || {};
    const lockHint = '该字段是 Redis 配额 key <code>{provider}:quota:{kind}:{key_id}:{day}</code> ' +
      '的组成部分，建成后不可改：改动会让已按旧值累加的计数被按新值解读，' +
      '水位整体失真且不报任何错。需要换值请新建 provider 并重新导入 Key。';
    const fields = [
      { name: 'name', label: 'provider 名', value: v.name || '', readonly: isEdit,
        placeholder: '小写字母开头，可含数字与下划线',
        hint: isEdit ? lockHint : '建成后不可改，且不得与任何已存在（含已停用）的 provider 重名。' },
      { name: 'base_url', label: 'base_url', value: v.base_url || '',
        placeholder: 'https://…',
        hint: '改动后在途请求（含重试）仍走旧 URL 直至完成，新 URL 从下一个请求起生效。' },
      { name: 'quota_kind', label: '配额量纲 quota_kind', type: 'select',
        options: QUOTA_KINDS, value: v.quota_kind || 'token', readonly: isEdit,
        hint: isEdit ? lockHint : 'token 按 token 数计，count 按调用次数计。建成后不可改。' },
      { name: 'quota_limit', label: '单 Key 配额上限', type: 'number',
        value: v.quota_limit === undefined ? '' : v.quota_limit,
        hint: '<span class="limit-ref">上调不会追溯已扣减的用量，当日水位按新上限重算；' +
          '下调可能让当前用量已超限的 Key 立即退出派发。</span>' },
      // 界面收 "24h" 这种可读写法，提交前换算成纳秒（网关字段是
      // quota_window_nanos，见 provider_deps.go:32）。回填时反向换算。
      { name: 'quota_window', label: '配额窗口', value: nanosToDur(v.quota_window_nanos),
        placeholder: '如 24h、5h',
        hint: '影响配额 key 的分桶周期。改动后当前窗口内的计数不会重新分桶。' },
      { name: 'refresh_hour', label: '刷新时刻（0-23，留空表示不按小时刷新）',
        type: 'number', value: v.refresh_hour === null || v.refresh_hour === undefined ? '' : v.refresh_hour,
        hint: '留空即清空该设置。填一个已过去的小时数会让本日不再刷新。' },
      { name: 'adapter_kind', label: 'adapter_kind', type: 'select',
        options: ADAPTER_KINDS, value: v.adapter_kind || 'openai_compatible',
        hint: '有流量后不可改：运行中切换会让在途请求的响应用错的 adapter 解析。' },
      { name: 'credential_env', label: '凭据环境变量名 credential_env',
        value: v.credential_env || '', placeholder: 'SENSENOVA_API_KEY',
        hint: '填<strong>环境变量名</strong>而非密钥值，例如 <code>SENSENOVA_API_KEY</code>。' +
          '变量的值须在部署环境设置并重启网关，不在本页热生效范围内。' },
      { name: 'count_models', label: '计次模型（逗号分隔）',
        value: (v.count_models || []).join(', '),
        hint: '每一项都必须出现在下方模型映射的对外模型名中，否则永远匹配不到。' },
      { name: 'reasoning_models', label: '推理模型（逗号分隔）',
        value: (v.reasoning_models || []).join(', '),
        hint: '同上：必须出现在模型映射的对外模型名中。' },
    ];
    return (
      '<div class="imp-box prov-form">' +
      '<h3 class="modal-title-plain">' + (isEdit ? '编辑 ' + esc(v.name) : '新增 provider') + '</h3>' +
      (isEdit ? '' : '<p class="fld-hint">新建的 provider 默认停用，' +
        '可先验证凭据与模型映射再显式启用。</p>') +
      '<div class="modal-fields">' + fields.map(fieldRow).join('') + '</div>' +
      '<div class="fld">' +
      '<span class="fld-label">模型映射</span>' +
      '<p class="fld-hint">提交时<strong>整体替换</strong>而非合并 —— 这里列出的就是生效后的全部映射。' +
      '移除一条会让该对外模型名返回 404。</p>' +
      '<div class="map-rows" id="pf-map">' + mapRowsHtml(v.model_mapping) + '</div>' +
      '<button class="btn" type="button" data-pact="map-add">' +
      '<svg class="icon" aria-hidden="true"><use href="#icon-add"></use></svg>添加一条映射</button>' +
      '</div>' +
      fieldRow({ name: 'reason', label: '变更原因（进审计，便于日后回溯）',
        value: '', maxlength: 500, placeholder: '为什么要做这次改动' }) +
      '<div class="modal-actions">' +
      '<button class="btn" type="button" data-pact="form-cancel">取消</button>' +
      '<button class="btn btn-primary" type="button" data-pact="form-check">校验配置</button>' +
      '<button class="btn btn-primary" type="button" data-pact="form-commit" disabled>确认生效</button>' +
      '</div>' +
      '<div class="imp-result" id="pf-out" role="status" aria-live="polite"></div>' +
      '</div>'
    );
  }

  // ---------------------------------------------------------------- 取值

  function readMapping() {
    const out = {};
    const rows = document.querySelectorAll('#pf-map .map-row');
    for (let i = 0; i < rows.length; i++) {
      const pub = rows[i].querySelector('[data-map="pub"]').value.trim();
      const up = rows[i].querySelector('[data-map="up"]').value.trim();
      if (!pub && !up) continue;
      out[pub] = up;
    }
    return out;
  }

  function csv(s) {
    return String(s || '').split(',').map((x) => x.trim()).filter(Boolean);
  }

  function val(name) {
    const el = document.querySelector('[data-pf="' + name + '"]');
    return el ? el.value.trim() : '';
  }

  // 「24h」「90m」「30s」解析为纳秒。无法解析时返回 0，由 localFails 拦下 ——
  // 不要在这里回退成默认 24h：把无法理解的输入静默改成一个合法值，
  // 等于替运维决定了配额周期，而他看到的表单里写的是别的东西。
  const DUR_UNITS = { h: 3600e9, m: 60e9, s: 1e9 };
  function durToNanos(s) {
    const m = /^(\d+(?:\.\d+)?)(h|m|s)$/.exec(String(s || '').trim());
    if (!m) return 0;
    return Math.round(parseFloat(m[1]) * DUR_UNITS[m[2]]);
  }

  function nanosToDur(n) {
    const v = Number(n) || 0;
    if (!v) return '24h';
    if (v % DUR_UNITS.h === 0) return v / DUR_UNITS.h + 'h';
    if (v % DUR_UNITS.m === 0) return v / DUR_UNITS.m + 'm';
    return Math.round(v / DUR_UNITS.s) + 's';
  }
  P.durToNanos = durToNanos;

  // 网关的 decoder 开了 DisallowUnknownFields（admin_provider.go:481），
  // 任何多余键都会让整个请求以 400 被拒。所以这里只发它认得的字段，
  // 且字段名必须与 ProviderConfigView 的 json tag 逐一对应。
  function readForm() {
    const body = {
      base_url: val('base_url'),
      quota_limit: Number(val('quota_limit')),
      quota_window_nanos: durToNanos(val('quota_window')),
      adapter_kind: val('adapter_kind'),
      credential_env: val('credential_env'),
      model_mapping: readMapping(),
      count_models: csv(val('count_models')),
      reasoning_models: csv(val('reasoning_models')),
      reason: val('reason'),
    };
    const rh = val('refresh_hour');
    // 不传 = 不改，传 null = 清空。留空意味着「不按小时刷新」，
    // 是一个明确的取值，所以送 null 而不是省略字段。
    body.refresh_hour = rh === '' ? null : Number(rh);
    // name 与 quota_kind 编辑态也要发：PUT 是全量替换，且网关拿 name
    // 与路径比对来兜住改名（admin_provider.go:255）。省掉它们等于把
    // 「沿用当前值」的判断推给网关，而 PUT 的语义里没有这一步。
    body.name = form.isEdit ? form.name : val('name');
    body.quota_kind = val('quota_kind');
    body.enabled = form.isEdit ? !!form.enabled : false;
    return body;
  }

  // ---------------------------------------------------------------- 本地校验

  function markInvalid(name, bad) {
    const el = document.querySelector('[data-pf="' + name + '"]');
    if (!el) return;
    if (bad) el.setAttribute('aria-invalid', 'true');
    else el.removeAttribute('aria-invalid');
  }

  // 只做「送出去必然被拒」的判定，不复制网关的业务规则：两套规则各自演化，
  // 最终会对同一份配置给出不同结论。
  function localFails(body) {
    const fails = [];
    ['name', 'base_url', 'credential_env', 'quota_limit', 'reason', 'quota_window']
      .forEach((f) => markInvalid(f, false));

    if (!form.isEdit && !/^[a-z][a-z0-9_]{1,31}$/.test(body.name || '')) {
      fails.push({ field: 'name', message: 'provider 名应为小写字母开头的 2-32 位小写字母、数字或下划线' });
    }
    if (!/^https:\/\/.+/.test(body.base_url)) {
      fails.push({ field: 'base_url', message: 'base_url 必须是 https 开头的合法地址' });
    }
    if (!(body.quota_limit > 0)) {
      fails.push({ field: 'quota_limit', message: '配额上限须为大于 0 的数字' });
    }
    if (!body.quota_window_nanos) {
      fails.push({ field: 'quota_window', message: '配额窗口需形如 24h / 90m / 30s，且大于 0' });
    }
    if (body.refresh_hour !== null &&
        (!Number.isInteger(body.refresh_hour) || body.refresh_hour < 0 || body.refresh_hour > 23)) {
      fails.push({ field: 'refresh_hour', message: '刷新时刻须为 0-23 的整数，或留空' });
    }
    if (!body.reason) {
      fails.push({ field: 'reason', message: '变更原因必填，它会进审计供日后回溯' });
    }
    if (!body.credential_env) {
      fails.push({ field: 'credential_env', message: '凭据环境变量名必填' });
    } else if (looksLikeSecret(body.credential_env)) {
      // error 而非 warning，且不提供旁路：这个字段会进 provider_config_versions，
      // 历史只读，密钥一旦写进去就无法收回。文案只给正确形态的例子 ——
      // 写出判据会引导人去规避判据。
      fails.push({
        field: 'credential_env',
        message: '这里填的像是密钥值本身。该字段填环境变量名，如 SENSENOVA_API_KEY；' +
          '密钥的值须在部署环境设置，不要填进配置',
      });
    }
    Object.keys(body.model_mapping).forEach(function (k) {
      if (!k) fails.push({ field: 'model_mapping', message: '对外模型名不能为空' });
      else if (!body.model_mapping[k]) {
        fails.push({ field: 'model_mapping', message: '「' + k + '」的上游模型名不能为空' });
      }
    });
    const pub = Object.keys(body.model_mapping);
    body.count_models.concat(body.reasoning_models).forEach(function (m) {
      if (pub.indexOf(m) < 0) {
        fails.push({ field: 'model_mapping', message: '「' + m + '」不在模型映射里，永远不会被匹配到' });
      }
    });
    fails.forEach((f) => markInvalid(f.field, true));
    return fails;
  }

  function looksLikeSecret(s) {
    return s.length > 40 || /[a-z]/.test(s);
  }

  P.form = form;
  P.readForm = readForm;
  P.localFails = localFails;
  P.formHtml = formHtml;
  P.host = host;
  P.getForm = function () { return form; };
  P.setForm = function (f) { form = f; };
})(window.FKAdmin);
