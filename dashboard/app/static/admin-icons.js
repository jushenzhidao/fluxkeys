/* FluxKeys 图标 sprite：来自 Lucide（ISC 许可，24×24 网格，stroke 风格）。
 *
 * 手工摘取所需的 10 个图标 path，不引 npm、不引 CDN、无构建步骤。
 * 独立成文件而非内联进 index.html，是为了让 index.html 保持在 300 行以内。
 *
 * 三条约定：
 * - stroke 与 fill 不在 symbol 上声明，统一由 admin.css 的 .icon 类设置为
 *   currentColor，图标颜色因此自动跟随 --up / --ok / --warn 等既有变量。
 * - 按功能命名（icon-ban）而非按外观命名（icon-circle-slash），换图标时引用处不用改。
 * - 本脚本必须放在 <body> 的第一个位置，不能放到 body 末尾与其他脚本合并。
 *   <use href="#icon-*"> 在元素首次布局时解析目标 symbol，若那时 sprite 还没插入，
 *   浏览器会静默渲染空白且不抛错 —— 语法检查、构建、单测都发现不了这类缺陷。
 *   放在 body 开头则 sprite 先于任何 <use> 进入文档，时序依赖被彻底消除。
 *   （不内联进 index.html 的原因：会让该文件超过 300 行的单文件上限。）
 */

'use strict';

(function () {
  const SPRITE =
    '<svg width="0" height="0" style="position:absolute" aria-hidden="true" focusable="false">' +
    '<symbol id="icon-import" viewBox="0 0 24 24">' +
    '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/>' +
    '<polyline points="17 8 12 3 7 8"/><line x1="12" x2="12" y1="3" y2="15"/></symbol>' +
    '<symbol id="icon-network" viewBox="0 0 24 24">' +
    '<rect x="16" y="16" width="6" height="6" rx="1"/>' +
    '<rect x="2" y="16" width="6" height="6" rx="1"/>' +
    '<rect x="9" y="2" width="6" height="6" rx="1"/>' +
    '<path d="M5 16v-3a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v3"/><path d="M12 12V8"/></symbol>' +
    '<symbol id="icon-ban" viewBox="0 0 24 24">' +
    '<circle cx="12" cy="12" r="10"/><path d="m4.9 4.9 14.2 14.2"/></symbol>' +
    '<symbol id="icon-restore" viewBox="0 0 24 24">' +
    '<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/>' +
    '<path d="M3 3v5h5"/></symbol>' +
    '<symbol id="icon-layers" viewBox="0 0 24 24">' +
    '<path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83z"/>' +
    '<path d="M2 12a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 12"/>' +
    '<path d="M2 17a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 17"/></symbol>' +
    '<symbol id="icon-alert" viewBox="0 0 24 24">' +
    '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/>' +
    '<path d="M12 9v4"/><path d="M12 17h.01"/></symbol>' +
    '<symbol id="icon-check" viewBox="0 0 24 24">' +
    '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></symbol>' +
    '<symbol id="icon-logout" viewBox="0 0 24 24">' +
    '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/>' +
    '<polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/></symbol>' +
    '<symbol id="icon-close" viewBox="0 0 24 24">' +
    '<path d="M18 6 6 18"/><path d="m6 6 12 12"/></symbol>' +
    '<symbol id="icon-arrow" viewBox="0 0 24 24">' +
    '<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/></symbol>' +
    '</svg>';

  // 按 HTML 规范，解析器一遇到 <body> 开标签就把 body 元素插入 DOM，之后才解析其
  // 子节点，因此本脚本执行时 document.body 必然已存在。仍保留 documentElement 兜底：
  // 万一将来有人把这个 script 移进 <head>，图标应当照常可用而不是集体空白。
  const host = document.body || document.documentElement;
  host.insertAdjacentHTML('afterbegin', SPRITE);
})();
