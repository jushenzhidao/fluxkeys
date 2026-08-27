"""登录页 HTML。

样式内联而非引用 /app.css，这样静态资源无需任何鉴权豁免。登录页样式量很小，
内联的代价远低于多开一个豁免口子。

配色全部复用 app.css 既有的 :root 变量语义，不新增颜色、不使用渐变。
"""

from __future__ import annotations

from typing import Final

# 与 app.css 的 :root 保持同名同值，避免登录页与主界面观感割裂。
# 这里必须写死一份是因为登录页不加载外部 CSS。
_LOGIN_HTML: Final[
    str
] = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FluxKeys 管理控制台 · 登录</title>
<style>
:root {
  --bg: #0f1115; --panel: #161a20; --panel-2: #1c2128; --border: #262c36;
  --text: #e6e9ef; --text-dim: #9aa4b2; --text-faint: #6b7280;
  --up: #ef4444; --ok: #22c55e; --accent: #3b82f6;
}
* { box-sizing: border-box; }
body {
  margin: 0; min-height: 100vh; display: flex; align-items: center;
  justify-content: center; background: var(--bg); color: var(--text);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
               "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
}
.card {
  width: 100%; max-width: 360px; padding: 32px;
  background: var(--panel); border: 1px solid var(--border); border-radius: 8px;
}
h1 { margin: 0 0 4px; font-size: 18px; font-weight: 600; }
.sub { margin: 0 0 24px; font-size: 13px; color: var(--text-dim); }
label { display: block; margin-bottom: 8px; font-size: 13px; color: var(--text-dim); }
input {
  width: 100%; padding: 10px 12px; margin-bottom: 16px; font-size: 14px;
  color: var(--text); background: var(--panel-2);
  border: 1px solid var(--border); border-radius: 6px;
}
input:focus { outline: none; border-color: var(--accent); }
button {
  width: 100%; padding: 10px; font-size: 14px; font-weight: 500;
  color: var(--text); background: var(--accent);
  border: none; border-radius: 6px; cursor: pointer;
}
button:disabled { opacity: .6; cursor: default; }
.err {
  margin-top: 14px; padding: 10px 12px; font-size: 13px;
  color: var(--up); background: var(--panel-2);
  border: 1px solid var(--border); border-radius: 6px;
}
.hidden { display: none; }
</style>
</head>
<body>
<main class="card">
  <h1>FluxKeys 管理控制台</h1>
  <p class="sub">请输入访问口令</p>
  <form id="f" autocomplete="off">
    <label for="pw">口令</label>
    <input id="pw" name="password" type="password" required autofocus>
    <button id="btn" type="submit">登录</button>
  </form>
  <div id="err" class="err hidden"></div>
</main>
<script>
const form = document.getElementById('f');
const btn = document.getElementById('btn');
const errBox = document.getElementById('err');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errBox.classList.add('hidden');
  btn.disabled = true;
  try {
    const body = new URLSearchParams();
    body.set('password', document.getElementById('pw').value);
    const resp = await fetch('/login', {
      method: 'POST',
      body,
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    });
    if (resp.status === 204) {
      window.location.replace('/');
      return;
    }
    let detail = '登录失败';
    try {
      const parsed = await resp.json();
      if (parsed && parsed.detail) { detail = parsed.detail; }
    } catch (_) { /* 保留默认文案 */ }
    errBox.textContent = detail;
    errBox.classList.remove('hidden');
  } catch (_) {
    errBox.textContent = '无法连接服务，请确认看板进程状态';
    errBox.classList.remove('hidden');
  } finally {
    btn.disabled = false;
  }
});
</script>
</body>
</html>
"""


def render_login_page() -> str:
    """返回登录页 HTML。"""
    return _LOGIN_HTML
