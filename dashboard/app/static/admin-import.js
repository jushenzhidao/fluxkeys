/* FluxKeys 管理控制台：Key 清单导入。
 *
 * 与 admin.js 分文件的原因不只是行数：导入是唯一的长耗时写操作（最长 90 秒），
 * 它的禁重复提交与超时文案自成一套规则，混在状态操作里容易被后人误改。
 *
 * 两条不可省的语义（Spec 2.4）：
 * 1. 网关逐条 upsert，不是单个事务 —— 超时时可能已写入部分 Key。
 * 2. 因此超时文案必须引导「先刷新核对」，而不是「请重试」：整份重提虽因 upsert
 *    幂等而不会写坏数据，但会掩盖上次实际成功了多少条。
 */

'use strict';

(function (A) {
  // 服务端 read 超时 90s（GATEWAY_TIMEOUT_IMPORT），本地留 15s 余量，
  // 让 504 由服务端判定并给出统一文案，而非前端先行中断。
  const IMPORT_WAIT_MS = 105000;
  const MAX_FAIL_ROWS = 50;
  // 与网关 http.MaxBytesReader(8<<20) 及看板转发层上限对齐。提前在本地拦掉，
  // 免得白传一次 8MB 才被拒。按 UTF-8 实际字节数判断，不用字符数。
  const MAX_BYTES = 8 * 1024 * 1024;

  function summaryHtml(r) {
    const fails = (r && r.failures) || [];
    const line = [
      '成功 ' + fmtInt(r.imported_count || 0) + ' 条',
      '新建 ' + fmtInt(r.created_count || 0),
      '更新 ' + fmtInt(r.updated_count || 0),
      '失败 ' + fmtInt(r.failed_count || 0),
    ].join(' ｜ ');
    let html = '<p class="imp-line">' + esc(line) + '</p>';
    if (fails.length) {
      html += '<ul class="imp-fails">' + fails.slice(0, MAX_FAIL_ROWS).map(function (x) {
        return '<li><code>' + esc(x.key_id || '（无 key_id）') + '</code> ' +
          esc(x.reason || '') + '</li>';
      }).join('') + '</ul>';
      if (fails.length > MAX_FAIL_ROWS) {
        html += '<p class="imp-line">另有 ' + fmtInt(fails.length - MAX_FAIL_ROWS) +
          ' 条失败未展开，详见网关日志。</p>';
      }
    }
    return html;
  }

  function setOut(out, cls, html) {
    out.className = 'imp-result ' + cls;
    out.innerHTML = html;
  }

  /** 本地校验：返回条数，或返回 null 并已写好错误提示。 */
  function precheck(raw, out) {
    const bytes = new TextEncoder().encode(raw).length;
    if (bytes > MAX_BYTES) {
      setOut(out, 'is-err', '<p class="imp-line">清单 ' + (bytes / 1048576).toFixed(2) +
        ' MB，超过 8 MB 上限，未提交。请拆成多批导入。</p>');
      return null;
    }
    let parsed;
    try {
      parsed = JSON.parse(raw);
    } catch (e) {
      setOut(out, 'is-err', '<p class="imp-line">JSON 解析失败：' + esc(e.message) +
        '。清单未提交，网关侧无任何写入，修正后可重新提交。</p>');
      return null;
    }
    const list = Array.isArray(parsed) ? parsed : [parsed];
    if (!list.length) {
      setOut(out, 'is-err', '<p class="imp-line">清单为空数组，未提交。</p>');
      return null;
    }
    const bad = list.findIndex(function (x) {
      return !x || typeof x !== 'object' || Array.isArray(x) || !x.key_id;
    });
    if (bad >= 0) {
      setOut(out, 'is-err', '<p class="imp-line">第 ' + (bad + 1) +
        ' 条缺少必填的 key_id 或不是对象，整份清单未提交。</p>');
      return null;
    }
    return list.length;
  }

  async function onImport() {
    const ta = document.getElementById('imp-input');
    const btn = document.getElementById('btn-import');
    const out = document.getElementById('imp-result');
    const raw = ta.value.trim();

    out.className = 'imp-result';
    if (!raw) {
      setOut(out, 'is-err', '<p class="imp-line">请先粘贴 Key 清单 JSON。</p>');
      ta.focus();
      return;
    }
    const count = precheck(raw, out);
    if (count === null) return;

    // 禁止重复提交：单次导入最长 90 秒，重复点击会让运维无法判断看到的是哪一次的
    // 结果 —— upsert 的幂等性只保证最终状态一致，不保证计数可读。
    btn.disabled = true;
    btn.classList.add('is-busy');
    ta.readOnly = true;
    const started = Date.now();
    setOut(out, 'is-busy', '<p class="imp-line">正在导入 ' + fmtInt(count) +
      ' 条。网关逐条写入，约 20-50 毫秒/条，最长可能等待 90 秒，请不要刷新或关闭页面。</p>');

    try {
      // raw:true —— 原样透传运维粘贴的文本，不做 parse 后再 stringify：
      // 重新序列化可能改变大整数与浮点的表示，让网关收到的内容与提交的不完全一致。
      const r = await A.request('POST', '/api/admin/keys/import', raw, {
        raw: true,
        timeoutMs: IMPORT_WAIT_MS,
      });
      setOut(out, 'is-ok', '<p class="imp-line">导入完成，耗时 ' +
        ((Date.now() - started) / 1000).toFixed(1) + ' 秒。</p>' + summaryHtml(r || {}));
      A.notify('ok', '导入完成', '成功 ' + fmtInt((r && r.imported_count) || 0) + ' 条');
      loadKeys();
    } catch (e) {
      const mayPartial = e.status === 504 || e.status === 0;
      let tail;
      if (mayPartial) {
        tail = '<p class="imp-line">网关是逐条写入、非单个事务，超时时可能已写入部分 Key。' +
          '请先刷新下方 Key 列表核对实际条数，确认缺哪些再补，不要整份重新提交 —— ' +
          '重提虽然安全，但会掩盖上一次实际成功了多少。</p>';
      } else if (e.status === 400 && e.payload && e.payload.failures) {
        // 网关对「全部失败」返回 400，响应体仍是 ImportResult，逐条原因要展示出来。
        tail = summaryHtml(e.payload);
      } else if (e.status === 413) {
        tail = '<p class="imp-line">请拆成多批提交，单批不超过 8 MB。</p>';
      } else {
        tail = '<p class="imp-line">清单未生效，修正后可重新提交。</p>';
      }
      setOut(out, 'is-err', '<p class="imp-line">导入未正常结束：' + esc(e.message) + '</p>' + tail);
      if (mayPartial) loadKeys();
    } finally {
      btn.disabled = false;
      btn.classList.remove('is-busy');
      ta.readOnly = false;
    }
  }

  document.addEventListener('DOMContentLoaded', function () {
    const btn = document.getElementById('btn-import');
    if (btn) btn.addEventListener('click', onImport);
  });
})(window.FKAdmin);
