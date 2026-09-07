/* FluxKeys 管理控制台：provider 配置的读取与渲染。
 *
 * 与 admin.js 分文件的原因见 index.html 的 300 行约定；与 admin-provider-form.js
 * 分文件的原因是两者的失效方式不同：本文件只读，写错了页面显示不对；表单文件
 * 会落库，写错了配置就错了。混在一起会让「只是改个渲染」的改动落在写路径旁边。
 *
 * 三条本页面特有的语义（契约 §4.1.1 / §4.2 / §6.1）：
 * 1. 四种 409 靠 err.code 分派文案，不靠状态码 —— 处置动作各不相同。
 * 2. provider 名与 quota_kind 是 Redis 配额 key `{provider}:quota:{kind}:{key_id}:{day}`
 *    的组成部分，建成后物理禁改：改了会让历史计数被按新维度解读，水位失真且不报错。
 * 3. 没有删除。退役走停用，历史用量与版本记录完整保留。
 */

'use strict';

(function (A) {
  const P = (A.prov = A.prov || {});

  const VERSION_PAGE = 50;

  // 列表与版本历史的当前状态。乐观锁基准是**每个 provider 自己的**
  // row.version（网关按行比对），不存在全局版本号 —— 所以这里不缓存
  // activeVersion，每次写操作都从对应那行取，且必须来自真实响应。
  const state = {
    providers: [],
    stateFilter: '',
    versions: [],
    // 版本历史按 provider 分组，故必须记住「当前在看谁的历史」。
    versionsProvider: '',
    // 游标翻页用网关返回的 next_before，不用最后一行的版本号自行推算 ——
    // 版本号全局单调，同一 provider 的相邻两条之间可以夹着别人的版本。
    versionsCursor: 0,
    versionsHasMore: false,
    loading: false,
  };
  P.state = state;

  // ---------------------------------------------------------------- 错误分派

  // 按 code 分派而非按状态码：处置动作完全不同，共用一句「操作冲突」等于不给下一步。
  //
  // 这些 code 取自网关实际返回值（internal/gateway/admin_provider.go:86-104
  // 的 writeProviderStoreErr），**不是** docs/provider-config-hotreload.md
  // §4.1.1 那四个。设计文档写的 immutable_field / quota_kind_mismatch /
  // invalid_state_transition 在网关里根本不存在，照文档写这张表的结果是
  // 每次都落到兜底分支 —— 而兜底只会显示网关那句话，不给「怎么办」。
  const CONFLICT_HINT = {
    version_conflict:
      '配置在你编辑期间已被其他人改动。请刷新列表后基于最新配置重新编辑 —— ' +
      '直接重试会用你看到的旧值覆盖掉对方的改动。',
    name_immutable:
      'provider 名建成后不可改。正确做法：新建一个 provider 写入目标配置，' +
      '用「导入 Key」在新 provider 下重新导入 Key，再停用旧的。' +
      '旧 provider 的历史用量按旧维度留在归档里是正确的。',
    quota_kind_immutable:
      '配额量纲不可修改。换量纲请新建 provider 再停用旧的 —— ' +
      '若这次是回滚，说明目标版本的量纲与当前不同，没有可用的回滚路径；' +
      '只想调额度请用编辑表单单独改 quota_limit。',
    provider_exists:
      'provider 名已被占用，含已停用的。名字进入配额 key 前缀与归档维度，' +
      '停用后也不释放。请换一个名字。',
    not_implemented:
      '当前部署未启用配置管理（网关未挂载 provider 存储）。' +
      '这不是权限问题，重试无效，需要运维确认网关的部署形态。',
    provider_not_found:
      'provider 不存在，可能已被其他人停用或改名。请刷新列表确认。',
    version_not_found:
      '目标版本不属于当前 provider。版本号在全局单调递增，' +
      '别的 provider 的版本号在这里查不到 —— 请刷新版本历史后重新选择。',
    provider_invalid:
      '配置未通过网关校验，逐条原因见下方列表。',
  };

  /** 把一次写操作的失败转成贴合场景的提示。 */
  P.notifyFail = function (prefix, e) {
    const hint = CONFLICT_HINT[e && e.code];
    let detail = hint || '';
    if (!detail) {
      if (e && e.status === 0) {
        detail = '请勿直接重试，先刷新列表确认这次操作是否已生效。';
      } else if (e && e.status === 504) {
        detail = '请求已发出但未在超时内返回。刷新列表确认实际状态后再决定是否重试。';
      } else if (e && e.status === 404) {
        detail = '该 provider 已不存在，可能已被其他人改动。请刷新列表。';
      } else if (e && e.status >= 500) {
        detail = '网关侧异常，配置未变更。请检查 gateway 容器状态与日志。';
      }
    }
    A.notify('err', prefix + '：' + ((e && e.message) || '未知原因'), detail);
    // 版本冲突后本地缓存的那行 version 已过期，继续用它提交只会再撞一次 409。
    if (e && e.code === 'version_conflict') P.load();
  };

  // ---------------------------------------------------------------- 单元格

  function trafficTag(p) {
    return p.has_traffic
      ? '<span class="tag tag-ok">在跑</span>'
      : '<span class="tag tag-dim">无流量</span>';
  }

  function credTag(p) {
    if (p.credential_present) return '<span class="tag tag-ok">已配置</span>';
    return (
      '<span class="tag tag-up" title="须在部署环境设置该变量的值并重启网关，' +
      '不在本页热生效范围内">凭据缺失</span>'
    );
  }

  function nameCell(p) {
    return (
      // 无「默认」标记：default_provider 只存在于 YAML（config.go:40），
      // 列表接口不回这个字段，也没有任何端点能改它。凭 undefined 渲染标记
      // 会让所有 provider 都显示成非默认，等于给出一个恒假的事实。
      '<div class="prov-name"><code>' + esc(p.name) + '</code>' +
      trafficTag(p) +
      '</div>' +
      '<div class="limit-ref">' + esc(p.credential_env || '（未设 credential_env）') + '</div>'
    );
  }

  // 水位 = 本配额日已用 / 上限。字段名是 today_used（ProviderListEntry
  // 定义在 internal/gateway/provider_deps.go:54）。
  //
  // 它的量纲随 quota_kind 变化：按次计费回调用次数，token 计费回 token 数。
  // 所以这一格只做除法、不追加单位 —— 写死任一单位都会让另一种 provider
  // 的水位读起来是错的。
  //
  // 字段缺失时显示占位符而不是把 0 当成「空闲」：把未知渲染成 0% 会让运维
  // 以为额度充足，而这恰恰是最危险的误读。
  function quotaCell(p) {
    const used = p.today_used;
    const limit = Number(p.quota_limit) || 0;
    if (used === undefined || used === null || !limit) {
      return '<span class="limit-ref">用量未返回</span>';
    }
    return gaugeHtml(Number(used) / limit);
  }

  function opsCell(p) {
    const n = esc(p.name);
    let html = '<td class="ops">';
    html += iconBtn('prov-edit', n, 'icon-edit', '编辑 ' + p.name + ' 的配置');
    if (p.enabled) {
      html += iconBtn('prov-disable', n, 'icon-ban', '停用 ' + p.name);
    } else {
      html += iconBtn('prov-enable', n, 'icon-restore', '启用 ' + p.name);
    }
    return html + '</td>';
  }

  function iconBtn(act, name, icon, label) {
    return (
      '<button class="icon-btn" type="button" data-pact="' + act + '" data-name="' + name +
      '" title="' + esc(label) + '" aria-label="' + esc(label) + '">' +
      '<svg class="icon" aria-hidden="true"><use href="#' + icon + '"></use></svg></button>'
    );
  }

  function mapCount(p) {
    const keys = Object.keys(p.model_mapping || {});
    const cls = keys.length ? '' : ' class="limit-ref"';
    const tip = keys.slice(0, 5).join('、') + (keys.length > 5 ? ' 等' : '');
    return '<span' + cls + (tip ? ' title="' + esc(tip) + '"' : '') + '>' +
      keys.length + ' 条</span>';
  }

  function rowHtml(p) {
    const cells = [
      '<td>' + nameCell(p) + '</td>',
      '<td>' + (p.enabled
        ? '<span class="tag tag-ok">启用</span>'
        : '<span class="tag tag-dim">已停用</span>') + '</td>',
      '<td class="ellip" title="' + esc(p.base_url) + '">' + esc(p.base_url) + '</td>',
      '<td><span class="tag tag-dim">' + esc(p.quota_kind) + '</span></td>',
      '<td class="num">' + fmtInt(p.quota_limit) + '</td>',
      '<td>' + esc(p.quota_window || '—') + '</td>',
      '<td>' + quotaCell(p) + '</td>',
      '<td>' + credTag(p) + '</td>',
      '<td>' + esc(p.adapter_kind || '—') + '</td>',
      '<td class="num">' + fmtInt(p.version) + '</td>',
      '<td>' + fmtTime(p.updated_at) + '</td>',
      opsCell(p),
    ].join('');
    return '<tr' + (p.enabled ? '' : ' class="prov-off"') + '>' + cells + '</tr>';
  }

  // ---------------------------------------------------------------- 列表

  function visibleProviders() {
    if (state.stateFilter === 'enabled') return state.providers.filter((p) => p.enabled);
    if (state.stateFilter === 'disabled') return state.providers.filter((p) => !p.enabled);
    return state.providers;
  }

  P.render = function () {
    const tbody = document.getElementById('tbody-providers');
    if (!tbody) return;
    const rows = visibleProviders();
    if (!rows.length) {
      emptyRow(
        tbody, 12,
        state.providers.length
          ? '当前筛选下没有 provider'
          : '尚未配置任何 provider。点「新增 provider」创建第一个 —— 新建后默认停用，可先验证凭据与模型映射再启用。'
      );
    } else {
      tbody.innerHTML = rows.map(rowHtml).join('');
    }
    const ver = document.getElementById('prov-version');
    if (ver) {
      // 列表接口只回 {providers, count}，没有全局 active_version
      // （admin_provider.go:134）。版本是 per-provider 的，所以这里显示
      // 「共几个 / 启用几个」，不编造一个全局版本号。
      ver.textContent = state.providers.length
        ? '共 ' + state.providers.length + ' 个，启用 ' + P.enabledCount() + ' 个'
        : '';
    }
  };

  /** 把 provider 名灌进版本历史的选择器；保留用户已选项。 */
  function syncVersionPicker() {
    const sel = document.getElementById('sel-cfg-provider');
    if (!sel) return;
    const names = state.providers.map((p) => p.name);
    if (!names.length) {
      sel.innerHTML = '<option value="">（暂无 provider）</option>';
      state.versionsProvider = '';
      return;
    }
    // 已选项仍存在则保留，否则落到第一个 —— 否则每次列表刷新都会把
    // 正在查看的历史跳回第一个 provider。
    if (names.indexOf(state.versionsProvider) === -1) {
      state.versionsProvider = names[0];
    }
    sel.innerHTML = names
      .map(
        (n) =>
          '<option value="' + esc(n) + '"' +
          (n === state.versionsProvider ? ' selected' : '') +
          '>' + esc(n) + '</option>'
      )
      .join('');
  }

  P.load = async function () {
    const tbody = document.getElementById('tbody-providers');
    if (!tbody || state.loading) return;
    state.loading = true;
    if (!state.providers.length) emptyRow(tbody, 12, '加载中…');
    try {
      const data = await fetchJSON('/api/admin/providers');
      state.providers = (data && data.providers) || [];
      syncVersionPicker();
      P.render();
    } catch (e) {
      emptyRow(tbody, 12, '加载失败：' + e.message);
    } finally {
      state.loading = false;
    }
  };

  /** 供表单模块取乐观锁基准与当前行数据。 */
  P.find = function (name) {
    return state.providers.filter((p) => p.name === name)[0] || null;
  };
  P.enabledCount = function () {
    return state.providers.filter((p) => p.enabled).length;
  };

  // ---------------------------------------------------------------- 版本历史

  const ACTION_TEXT = {
    seed: '初始化', create: '新增', update: '修改', enable: '启用',
    disable: '停用', rollback: '回滚', set_default: '设为默认',
  };
  const ACTION_CLS = {
    create: 'tag-ok', enable: 'tag-ok', update: 'tag-dim', seed: 'tag-dim',
    disable: 'tag-warn', rollback: 'tag-up', set_default: 'tag-dim',
  };

  // 字段名对齐 gateway.ProviderVersionView（provider_deps.go:62）：
  // 版本号是 id（不是 version）、对象是 provider_name（不是 target）、
  // 操作人是 created_by（不是 actor）。读错任一项都不会报错，只会静默
  // 渲染成 #0 / — ，且 #0 会被当成回滚目标发出去。
  function versionRow(v) {
    const vid = v.id;
    // 与该 provider 自己的 version 比，不与 state.activeVersion 比：
    // 后者是全局配置热加载计数，和 provider_versions.id 不是同一个号段，
    // 拿来比会把「当前」标到错误的行上，或一行都不标。
    const own = P.find(state.versionsProvider);
    const isActive = !!own && vid === own.version;
    const fields = (v.changed_fields || []).join('、');
    return (
      '<tr>' +
      '<td class="num"><code>#' + fmtInt(vid) + '</code>' +
      (isActive ? ' <span class="tag tag-ok">当前</span>' : '') + '</td>' +
      '<td><span class="tag ' + (ACTION_CLS[v.action] || 'tag-dim') + '">' +
      esc(ACTION_TEXT[v.action] || v.action) + '</span>' +
      (v.rolled_back_from ? ' <span class="limit-ref">自 #' +
        fmtInt(v.rolled_back_from) + '</span>' : '') + '</td>' +
      '<td><code>' + esc(v.provider_name || '—') + '</code></td>' +
      '<td class="ellip" title="' + esc(fields) + '">' + esc(fields || '—') + '</td>' +
      '<td class="ellip" title="' + esc(v.reason) + '">' + esc(v.reason || '—') + '</td>' +
      '<td>' + esc(v.created_by || '—') + '</td>' +
      '<td>' + fmtTime(v.created_at) + '</td>' +
      '<td class="ops">' +
      iconBtn('cfg-view', String(vid), 'icon-layers', '查看版本 #' + vid + ' 的完整快照') +
      (isActive ? '' : iconBtn('cfg-rollback', String(vid), 'icon-rollback',
        '回滚到版本 #' + vid)) +
      '</td></tr>'
    );
  }

  P.renderVersions = function () {
    const tbody = document.getElementById('tbody-cfg-versions');
    if (!tbody) return;
    if (!state.versions.length) {
      emptyRow(tbody, 8, '暂无配置变更记录');
    } else {
      tbody.innerHTML = state.versions.map(versionRow).join('');
    }
    const more = document.getElementById('btn-cfg-more');
    if (more) more.hidden = !state.versionsHasMore;
  };

  // 版本历史按 provider 分组，没有全局视图（网关只有
  // GET /admin/providers/{name}/versions）。所以这张表必须先选中一个
  // provider 才有内容 —— 空着时给的是「选一个」而不是「暂无记录」，
  // 后者会让人以为系统没记变更。
  P.loadVersions = async function (append) {
    const tbody = document.getElementById('tbody-cfg-versions');
    if (!tbody) return;
    const name = state.versionsProvider;
    if (!name) {
      state.versions = [];
      state.versionsHasMore = false;
      emptyRow(tbody, 8, '点击某个 provider 的「历史」查看它的配置变更记录');
      const more0 = document.getElementById('btn-cfg-more');
      if (more0) more0.hidden = true;
      return;
    }
    let url =
      '/api/admin/providers/' + encodeURIComponent(name) + '/versions?limit=' + VERSION_PAGE;
    if (append && state.versionsCursor) url += '&before=' + state.versionsCursor;
    if (!append) emptyRow(tbody, 8, '加载中…');
    try {
      const data = await fetchJSON(url);
      // 网关返回 {provider_name, versions, next_before}，不是 {items, has_more}。
      const items = (data && data.versions) || [];
      state.versions = append ? state.versions.concat(items) : items;
      // next_before 为 0 表示没有下一页（网关只在满页时才填它）。
      state.versionsCursor = (data && data.next_before) || 0;
      state.versionsHasMore = !!state.versionsCursor;
      P.renderVersions();
    } catch (e) {
      if (append) {
        A.notify('err', '加载更多版本失败：' + e.message, '');
      } else {
        emptyRow(tbody, 8, '加载失败：' + e.message);
      }
    }
  };

  // 必须先 await P.load()：版本历史是 per-provider 的，要等列表回来、
  // 选择器填好 versionsProvider 之后才知道该查谁的历史。并发跑会让
  // loadVersions 拿着空名字直接返回，历史表永远是空的。
  P.reload = async function () {
    await P.load();
    await P.loadVersions(false);
  };

  // ---------------------------------------------------------------- 绑定

  function init() {
    // 事件委托绑到 tbody：两张表每次刷新都重建行，逐行绑定会随刷新丢失。
    ['tbody-providers', 'tbody-cfg-versions'].forEach(function (id) {
      const tbody = document.getElementById(id);
      if (!tbody) return;
      tbody.addEventListener('click', function (ev) {
        const btn = ev.target.closest('button[data-pact]');
        if (!btn || btn.disabled) return;
        const fn = P.handlers && P.handlers[btn.dataset.pact];
        if (fn) fn(btn.dataset.name);
      });
    });

    const sel = document.getElementById('sel-prov-state');
    if (sel) {
      sel.addEventListener('change', function (ev) {
        state.stateFilter = ev.target.value;
        P.render();
      });
    }
    const vsel = document.getElementById('sel-cfg-provider');
    if (vsel) {
      vsel.addEventListener('change', function (ev) {
        state.versionsProvider = ev.target.value;
        // 换 provider 必须清空游标：沿用上一个 provider 的 next_before
        // 会拿 A 的版本号去翻 B 的历史，翻出一页空结果。
        state.versionsCursor = 0;
        P.loadVersions(false);
      });
    }
    const more = document.getElementById('btn-cfg-more');
    if (more) more.addEventListener('click', () => P.loadVersions(true));

    if (document.getElementById('tbody-providers')) P.reload();
  }

  document.addEventListener('DOMContentLoaded', init);
})(window.FKAdmin);
