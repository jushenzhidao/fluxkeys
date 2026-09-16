# Provider 配置管理界面设计

> 设计范围：provider 配置的增删改 + 配置版本历史与回滚，落在既有看板 `dashboard/app/static/index.html` 内。
> 本文档只定义界面结构、交互与文案，不含实现代码。前端实现必须复用第 1 节的既有 token，禁止新造色值、字号、圆角。

## 0. 设计定位

这是**运维工具**，不是产品官网。三条基调：

- **信息密度优先**：provider 数量是个位数，但每个 provider 的配置项多且互相牵连（quota_kind 决定 quota_limit 的量纲，model_mapping 决定请求能不能路由成功）。一屏要能看到全部 provider 的关键配置 + 实时流量，而不是一个 provider 一张大卡片。
- **危险操作前置暴露后果**：阶段一的两起事故（provider 名写错导致配额记错主体、quota_kind 用错量纲导致刷满额度的 Key 被排到最优先）都属于"改错不报错、只是静默算错"。界面的核心职责是把这类静默错误在提交前变成可见的。
- **视觉语言完全沿用既有看板**：新增区块与「Key 配额水位」「出口 IP 状态」看起来是同一个产品的同一批次产物。不引入新配色、新字体、新圆角尺度。

三轴刻度：`DESIGN_VARIANCE=2`（运维表格类界面要可预测，不做非对称）/ `MOTION_INTENSITY=2`（只有 hover/focus 与展开折叠，无装饰动画）/ `VISUAL_DENSITY=8`（驾驶舱模式，紧凑 padding，表格为主，不用通用卡片容器包裹数据）。

---

## 1. 既有 Design Token 清单（如实提取，标注来源）

### 1.1 颜色（全部来自 `dashboard/app/static/app.css` L9-L27 的 `:root`）

| Token | 值 | 来源 | 语义（沿用既有约定） |
|---|---|---|---|
| `--bg` | `#0f1216` | app.css:10 | 页面底色 |
| `--panel` | `#171b21` | app.css:11 | 卡片 / 表格容器 / 弹层背景 |
| `--panel-2` | `#1d2229` | app.css:12 | 表头、输入框、按钮底、行 hover |
| `--border` | `#262c35` | app.css:13 | 唯一边框色 |
| `--text` | `#e6e9ee` | app.css:14 | 主文本 |
| `--text-dim` | `#939cab` | app.css:15 | 次级文本、label、表头文字、hint |
| `--text-faint` | `#6b7280` | app.css:16 | 三级文本（fld-hint、flow 图标） |
| `--up` | `#f04d4d` | app.css:19 | **危险 / 告警 / 消耗高**（不是"下跌"） |
| `--up-soft` | `#3a1d1f` | app.css:20 | 危险底色 |
| `--ok` | `#23b26d` | app.css:21 | 正常 / 余量足 / 成功 |
| `--ok-soft` | `#14301f` | app.css:22 | 成功底色 |
| `--warn` | `#e8a33d` | app.css:23 | 警告 / 中间水位 |
| `--warn-soft` | `#33270f` | app.css:24 | 警告底色 |
| `--info` | `#4b91f1` | app.css:25 | 链接色 |
| `--accent` | `#7c8cf8` | app.css:26 | 强调：section 左框、focus ring、primary 按钮描边 |

裸色值例外（既有代码已使用，新代码沿用同值不再新增）：`.gauge .bar` 底 `#2a3037`（app.css:197）、`.alert.critical` 底 `#1c1417`（app.css:243）、`tag` 描边 `#1f5c3c` / `#6d2b2e` / `#6a5220`（app.css:224-226）、`.err-banner` 文字 `#ffb4b4`（app.css:283）。

**配色语义有一条反直觉约定必须遵守**（app.css:1-7 注释明确）：红=消耗上升/水位高，绿=余量充足。与欧美「红跌绿涨」相反，这是刻意为中国用户的直觉设计的。provider 的配额水位条沿用同一套 `lv-ok / lv-mid / lv-high`（app.css:207-212）。

### 1.2 字体

| Token | 值 | 来源 |
|---|---|---|
| 正文字体栈 | `-apple-system, BlinkMacSystemFont, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Helvetica Neue", sans-serif` | app.css:35-36 |
| 等宽字体栈 | `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace` | admin.css:108 |
| 基准字号 / 行高 | `14px` / `1.5` | app.css:37-38 |

字号阶梯（既有实际使用值，共 6 级，**不新增**）：

| 字号 | 用途 | 来源 |
|---|---|---|
| 25px | 概览卡片数值 | app.css:106 |
| 17px | 页面 h1 | app.css:57 |
| 15px | section h2、弹层标题 | app.css:81 / admin.css:180 |
| 14px | 正文基准 | app.css:37 |
| 13px | 表格单元格、按钮、输入框、toast | app.css:152 / admin.css:37 |
| 12px | label、hint、meta、tag、等宽代码 | app.css:62 / admin.css:110 |

字重：`500`（表头 app.css:165、chart-box h3 app.css:137）/ `600`（h2 app.css:85、卡片数值 app.css:107、弹层标题 admin.css:181）。既有代码未用 400 以外的正文字重，正文保持默认。

字距：`header h1` 用 `letter-spacing: 0.5px`（app.css:59）。既有无 ALL CAPS 文本，本设计也不引入 ALL CAPS 标签（避免 AI 语法感）。

数字对齐：`font-variant-numeric: tabular-nums` 用于所有数值列（app.css:109、app.css:174、app.css:204）。provider 的 quota_limit、用量、版本号一律带上。

### 1.3 间距

既有实际使用值（4px 基准，**不新增非标值**）：

| 值 | 用途 | 来源 |
|---|---|---|
| 4px | 卡片 sub 上边距 | app.css:112 |
| 6px | 按钮 padding-y、gauge gap、kv gap | app.css:70 / admin.css:32 |
| 8px | table td padding-y、toolbar gap、alerts gap | app.css:155 / app.css:264 |
| 9px | h2 padding-left（与 3px 左框合成 12px 光学对齐） | app.css:83 |
| 10px | alert padding-y、hint 下边距 | app.css:238 / app.css:260 |
| 12px | table td padding-x、cards gap、section h2 下边距 | app.css:155 / app.css:93 |
| 14px | header padding-y、card padding-y、alert padding-x | app.css:47 / app.css:100 |
| 16px | 卡片 padding-x、header gap、chart-grid gap | app.css:100 / app.css:46 |
| 20px | main padding-top、弹层 padding-x | app.css:76 / admin.css:173 |
| 24px | main padding-x、header padding-x | app.css:76 / app.css:47 |
| 28px | section 下边距、空状态 padding | app.css:78 / app.css:180 |
| 48px | main padding-bottom | app.css:76 |

### 1.4 圆角

| Token | 值 | 用途 | 来源 |
|---|---|---|---|
| — | `4px` | gauge 条 | app.css:198 |
| — | `6px` | 按钮、输入框、select、textarea | app.css:69 / admin.css:36 |
| — | `8px` | alert、toast、err-banner、imp-result | app.css:237 / admin.css:267 |
| — | `10px` | 卡片、表格容器、弹层、imp-box、tag | app.css:99 / admin.css:172 |

**上限 10px**——既有代码最大圆角就是 10px，新增区块不得超过。

### 1.5 阴影与层级

既有代码**完全不用 box-shadow**，层级靠 `1px solid var(--border)` + 背景亮度递进表达（`--bg` → `--panel` → `--panel-2`）。这条必须延续：provider 卡片/表格不加阴影。

z-index 阶梯：`header` = 10（app.css:52）/ `th` sticky = 1（app.css:166）/ `.modal-mask` = 100（admin.css:156）/ `#toast-root` = 110（admin.css:255）。版本历史抽屉若做成浮层，取 100（与 modal 同层，但同一时刻只开一个）。

### 1.6 动效

| 值 | 用途 | 来源 |
|---|---|---|
| `150ms cubic-bezier(0.4, 0, 0.2, 1)` | 全部过渡，唯一时长 | admin.css:40-42 |
| `@media (prefers-reduced-motion: reduce)` → `transition: none` | 已覆盖 `.btn` / `.icon-btn` | admin.css:296-298 |

新增组件的 reduced-motion 分支必须同步补进 admin.css:296 那条已有规则的选择器列表，而不是另写一条媒体查询。

### 1.7 组件与图标

| 类 | 说明 | 来源 |
|---|---|---|
| `.btn` / `.btn-primary` / `.btn-danger` | 32px 最小高度，6px 圆角，hover 改 border-color | admin.css:27-52 |
| `.icon-btn` | 28×28 行内图标按钮，运维密度优先（admin.css:56-57 注释已说明取 28px 而非 44px 的理由） | admin.css:58-77 |
| `.icon` | 15×15，`stroke: currentColor`，`fill: none`，`stroke-width: 2` | admin.css:10-20 |
| `.tag` / `.tag-ok` / `.tag-up` / `.tag-warn` / `.tag-dim` | 状态徽章 | app.css:216-227 |
| `.gauge` / `.lv-ok` / `.lv-mid` / `.lv-high` | 水位条 | app.css:187-212 |
| `.table-wrap` + `table` | 表格容器，`max-height: 560px` 内滚动，表头 sticky | app.css:144-176 |
| `.modal` / `.modal-warn` / `.kv` / `.fld` / `.fld-check` | 危险确认弹层全套 | admin.css:153-247 |
| `.toast` / `.toast-ok` / `.toast-warn` / `.toast-err` | 提示条 | admin.css:251-288 |
| `.empty` | 表格空状态单元格 | app.css:178-183 |
| `.err-banner` | 区块级错误横幅 | app.css:280-288 |

**图标系统锁定：Lucide**（`dashboard/app/static/admin-icons.js`，ISC 许可，24×24 网格 stroke 风格，手工摘取 symbol 进 sprite，无 npm / 无 CDN）。现有 10 个：`import` `network` `ban` `restore` `layers` `alert` `check` `logout` `close` `arrow`。

本设计需新增 6 个 Lucide symbol，命名遵循 admin-icons.js:9 的约定（**按功能命名而非按外观命名**）：

| symbol id | Lucide 源图标 | 用途 |
|---|---|---|
| `icon-provider` | `server` | provider 实体 |
| `icon-add` | `plus` | 新增 provider |
| `icon-edit` | `pencil` | 编辑配置 |
| `icon-history` | `history` | 版本历史 |
| `icon-rollback` | `undo-2` | 回滚到指定版本 |
| `icon-lock` | `lock` | 只读字段标记 |

新增 symbol 必须插入 `admin-icons.js` 的同一个 SPRITE 字符串内（该文件 L11-L14 说明了它必须留在 `<body>` 首位的时序原因，不要另建 sprite 文件）。

### 1.8 既有 JS 工具（复用，不重写）

`app.js`：`esc()` HTML 转义、`fmtInt()` 千分位、`fmtPct()` 百分比、`fmtTime()`、`gaugeHtml(ratio)` 水位条、`statusTag(status)` 状态徽章、`emptyRow(tbody, cols, text)` 空状态行、`fetchJSON(url)` 只读请求（内含 401 跳登录）。

`admin-ui.js`：`FKAdmin.icon(name, extraCls)`、`FKAdmin.notify(kind, msg, detail)`、`FKAdmin.confirm(opts)` 危险确认弹层（支持 `rows` / `warn` / `fields` / `danger` / `confirmLabel`，`fields` 支持 `text` / `select` / `checkbox`，`checkbox` 带 `required` 时会 gate 住确认按钮）、`FKAdmin.request(method, url, body, opts)` 写请求（内含超时、401、错误转译）。

`admin.js`：`reasonField()` 变更原因字段（maxlength 500，进审计）、`transition(from, to)` 状态流转展示。**provider 的所有写操作都要带变更原因，与 Key 操作一致。**

写请求的错误语义约定（admin.js:86-93，新代码沿用同一映射）：**409 分四种按 `error.code` 区分** / 404 = 目标不存在，列表已过期 / 504 或 0 = **不要直接重试，先刷新确认是否已生效**。

架构师在 `docs/provider-config-hotreload.md` §4.1.1 定义四种 409，全走 `writeError(w, r, 409, code, msg)` — `type` 字段由 `errorTypeFor(status)` 从状态码派生为 `invalid_request_error`（server.go:542-553，调用方指定不了），`code` 参数自由取值：

| `error.code` | 场景 | 前端处置 |
|---|---|---|
| `version_conflict` | 乐观并发冲突 | 刷新重试 |
| `immutable_field` | 跨量纲回滚 / 改 provider 名或 quota_kind | 改用新建 provider + 重新导入 Key |
| `quota_kind_mismatch` | 回滚目标版本的 quota_kind 与当前不同 | 改用编辑表单单改 quota_limit，或新建 provider |
| `invalid_state_transition` | 停用默认 provider / 设已停用项为默认 | 先改默认再停用 / 先启用再设默认 |

**前端错误分派必须按 `error.code` 而非状态码** — 四种同为 409，只看状态码退回模糊提示。`admin-ui.js` 的 `request()` 返回的 error 需扩展：`err.status`（已有）+ `err.code`（新增，从 `payload.error.code` 读，网关侧的 `writeError` 写的 JSON 结构见 server.go:519-526）。

---

## 2. 界面总体结构

### 2.1 落位

provider 管理插入 `index.html` 的位置：**「配额刷新状态」之后、「行为相似度自检」之前**。理由：provider 配置是配额口径的上游定义，看完概览与配额健康后紧接着看 provider 配置是自然的排障顺序；放到页面最末会让运维在出事时需要滚很久。

新增两个 `<section>`：

```html
<!-- Provider 配置 -->
<section>
  <h2>Provider 配置</h2>
  <p class="hint">…（见 2.2 说明文案）</p>
  <div class="toolbar">…</div>
  <div class="table-wrap"><table id="table-providers">…</table></div>
</section>

<!-- 配置版本历史 -->
<section>
  <h2>配置版本历史</h2>
  <p class="hint">…</p>
  <div class="table-wrap"><table id="table-config-versions">…</table></div>
</section>
```

沿用既有 `section > h2` 的样式（左侧 3px `--accent` 竖线 + 9px padding-left，app.css:80-86），与其他 9 个区块完全一致。

### 2.2 区块说明文案

Provider 配置区块的 `.hint`：

> provider 名是 Redis 配额 key 的前缀与用量归档维度，创建后不可改。quota_kind 决定预扣量纲（token 计 token 数，count 计调用次数），配错会让配额水位与健康分全部失真。配置变更走校验后热生效，无需重启网关。

版本历史区块的 `.hint`：

> 每次保存生成一个版本号，含变更前后的完整配置快照。回滚是「以历史版本内容创建一个新版本」，不删除中间版本，因此回滚本身也留痕。

两段都是具体信息，不是"请谨慎操作"这类空话。

---

## 3. 场景一：provider 列表

### 3.1 布局决策

**用表格，不用卡片网格。** provider 数量是个位数，但每个有 9+ 个配置维度需要横向比对（"volc 的 quota_limit 是 500 万，sensenova 是 500，这俩量纲不同吗"这类问题必须能横着扫一眼得出结论）。卡片网格会把同名字段拆到不同视觉列，横向比对失效。

沿用 `.table-wrap` + `table`（表头 sticky、容器内滚动、hover 高亮行）。

### 3.2 列定义

| 列 | 对齐 | 内容 | 说明 |
|---|---|---|---|
| Provider | 左 | 等宽字体的 provider 名 + `.tag` 启用状态 + 默认标记 + 凭据缺失标记 | 名字用 `--font-mono` 栈：它是 Redis key 前缀，视觉上就该像标识符而不像散文。徽章见 3.3 与 3.7 |
| 状态 | 左 | `.tag-ok` 启用 / `.tag-dim` 已停用 | 停用行整体 `opacity: 0.62` |
| base_url | 左 | 截断显示，`title` 属性给全值 | 超长 URL 不撑破表格 |
| 量纲 | 左 | `.tag-dim` token / count | 与 quota_limit 相邻，读的时候量纲和数字在一起 |
| quota_limit | 右 | `fmtInt()` 千分位 + tabular-nums | |
| 今日水位 | 左 | `gaugeHtml(ratio)` | ratio = 该 provider 全部 Key 的今日已用 / (quota_limit × Key 数)。**这是"哪个 provider 有流量在跑"的主判据** |
| 可用 Key | 右 | `active_keys / total_keys` | 分母为 0 时显示 `0 / 0` 并给 `--warn` 色：配了 provider 但没有 Key，请求会全部落空 |
| 今日请求 | 右 | `fmtInt()` | 0 请求时用 `--text-faint` 显示 `0`，与有流量的行形成明显区分 |
| 模型映射 | 右 | `N 条` + hover `title` 展开前 5 条 | 0 条时 `--warn`：无映射意味着对外模型名直接透传，多数情况是漏配 |
| 计次模型 | 右 | `N 个` | |
| 推理模型 | 右 | `N 个` | |
| 操作 | 左 | `.icon-btn` × 3：编辑 / 停用或启用 / 历史 | `td.ops`（admin.css:88） |

### 3.3 「一眼看出哪个 provider 有流量」的处理

不靠额外装饰，靠**三个既有信号叠加**：

1. 今日水位列的 `gaugeHtml`——有流量的行有彩色条，零流量的行是空条（灰底），扫一眼就能分辨。
2. 今日请求列——零流量用 `--text-faint`，与主文本的 `--text` 形成对比降级。
3. 有流量的 provider 在 Provider 列的 provider 名后追加一个 `.tag-ok` 徽章文案 `在跑`，零流量追加 `.tag-dim` 文案 `无流量`。

**不给每个数字配彩色圆形图标底**，不加进度环，不加迷你折线图。运维要的是可比对的数字，不是可视化演示。

### 3.4 工具栏

```html
<div class="toolbar">
  <select id="sel-provider-state">
    <option value="">全部 provider</option>
    <option value="enabled">仅启用</option>
    <option value="disabled">仅已停用</option>
  </select>
  <span class="meta" id="providers-total">—</span>
  <span class="spacer"></span>
  <button type="button" class="btn btn-primary" id="btn-provider-add">
    <svg class="icon" aria-hidden="true"><use href="#icon-add"></use></svg>
    <span>新增 Provider</span>
  </button>
</div>
```

沿用 `.toolbar` 的 flex + 8px gap（app.css:262-268）与 `.toolbar select` 样式（app.css:270-278）。新增按钮靠右，与既有「导入火山 Key」的 `.btn-primary` 视觉一致。

### 3.5 行结构草稿

```html
<tr data-provider="volc">
  <td>
    <code class="prov-name">volc</code>
    <span class="tag tag-ok">在跑</span>
  </td>
  <td><span class="tag tag-ok">启用</span></td>
  <td title="https://ark.cn-beijing.volces.com"><span class="ellip">https://ark.cn-beijing.volces.com</span></td>
  <td><span class="tag tag-dim">token</span></td>
  <td class="num">5,000,000</td>
  <td><!-- gaugeHtml(0.42) --></td>
  <td class="num">8 / 10</td>
  <td class="num">12,847</td>
  <td class="num" title="deepseek-v3 → deepseek-v3-241226 …">3 条</td>
  <td class="num">0 个</td>
  <td class="num">1 个</td>
  <td class="ops">
    <button type="button" class="icon-btn" data-act="prov-edit" data-provider="volc"
      title="编辑配置" aria-label="编辑 volc 的配置">…</button>
    <button type="button" class="icon-btn danger" data-act="prov-disable" data-provider="volc"
      title="停用该 provider" aria-label="停用 volc">…</button>
    <button type="button" class="icon-btn" data-act="prov-history" data-provider="volc"
      title="查看该 provider 的变更历史" aria-label="查看 volc 的变更历史">…</button>
  </td>
</tr>
```

`.prov-name` 与 `.ellip` 是本次需新增的两条 CSS，均只用既有 token：

```css
.prov-name { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
             font-size: 13px; color: var(--text); }
.ellip { display: inline-block; max-width: 260px; overflow: hidden;
         text-overflow: ellipsis; vertical-align: bottom; }
```

### 3.6 删除按钮：不提供

列表操作里**没有删除**。有流量的 provider 删掉会造成用量归档断层，无流量的 provider 删掉也会让历史版本里的引用变成悬空。统一只提供停用。若运维追问，工具栏 hint 已说明：停用后调度器不再向该 provider 派发，配置与历史保留。

### 3.7 default_provider：界面上要有，作为独立操作而非表单字段

**回答架构师的缺口确认：需要 `PATCH /admin/config/default-provider`。** 理由不是"顺手加个开关"，而是缺了它会在界面上造出一个自己修不了的坏状态：

本项目的目标是把 provider 配置从 `config.prod.yaml` 搬到页面上热生效。若 `default_provider` 仍只存在于 yaml，那么运维在页面上停用了恰好是默认 provider 的那一个之后，兜底路径就指向一个已停用的 provider——请求会落到没有候选 Key 的分支上。这个坏状态是页面制造的，却只能改 yaml 加重启才能修复，而页面上看不出任何异常。这正是本设计要消灭的那一类缺陷（操作成功、无报错、状态与心智模型不符）。

**载体：不做成 provider 表单里的 checkbox。** `default_provider` 是全局单值字段，具有互斥语义——把 A 设为默认会隐式地把 B 取消默认。做成每行一个 checkbox 会把它误表达为各行独立的状态，运维勾上 A 之后不会预期 B 被改动。改为：

1. **列表 Provider 列**：当前默认 provider 的名字后追加一枚 `.tag`（`--info` 色，文案 `默认`）。只有一行会有，互斥关系在视觉上自明。
2. **操作列**：非默认且处于启用状态的行，`.icon-btn` 追加一个「设为默认」；默认行本身不渲染此按钮（位置留空，理由同 §7.1）。
3. **停用行不提供此按钮**——把已停用的 provider 设为默认是直接制造上述坏状态。

**设为默认走 `FKAdmin.confirm`**，非 danger，`rows` 给 `默认 provider` 的流转（`volc → sensenova`）。成品文案：

> 未指定 provider 的请求将改为落到 sensenova。volc 不再是默认，但它的配置、Key 与派发都不受影响——只是不再接收未指定 provider 的那部分流量。此项改动同样进配置版本历史，可回滚。

**停用默认 provider：后端已定为硬拒绝，界面跟随。** 我原先设计的是 `required` checkbox（gate 确认按钮）放行，理由是"停用默认造成的是立即报错的显性故障，显性故障配摩擦"。架构师在 §4.6 把它做成了停用的前置检查——不能停用默认 provider，要停先改默认，违反返回 409 + `invalid_state_transition`。

**我接受这个更严的处理，并撤回原先的分级论证。** 原论证有个缺口：显性故障确实秒级可见，但"可见"不等于"可修复"。运维停用默认 provider 之后要修复，得先想到问题出在默认指向上——而报错本身是"无可用上游"，指向的是 Key 或 provider 而不是这个全局字段。这条修复路径比故障本身隐蔽。何况这里的替代动作只有两步（先改默认、再停用），拒绝的成本极低，没有理由用摩擦换。

因此界面上：目标是当前默认 provider 时，**停用按钮不渲染**（位置留空，理由同 §7.1），hover 该行的默认徽章给 `title`：

> volc 是当前的默认 provider，不能直接停用。请先把默认指向另一个启用中的 provider。

若用户绕过界面（旧页面缓存、并发改动导致界面状态过期）触发了后端拒绝，走 §8.5 的 `invalid_state_transition` 文案。

这样一来 §5 开头那条"显性故障配摩擦、静默错误配禁止"的判据要修正为：**静默错误一律禁止；显性故障看修复路径是否自明——自明的可以配摩擦，不自明的同样禁止。** 已同步改到 §5 与 §5.4。

### 3.8 `credential_present`：列表里显示凭据缺失

架构师提供的 `credential_present` 布尔（`os.Getenv` 是否非空）在界面上落在两处：

1. **列表 Provider 列**：为 `false` 时追加一枚 `.tag`（`--up` 色，文案 `凭据未设置`）。这是运行时故障，不是配置瑕疵——该 provider 的请求会全部失败，级别应等同于"可用 Key 为 0"。
2. **编辑表单的凭据来源字段**：`false` 时在 `.fld-hint` 上方加一行 `--up` 色说明：

> 环境变量 `SENSENOVA_API_KEY` 在网关进程中为空。该 provider 的请求会全部失败。变量名本身可以在这里改，但设置变量的值需要在部署环境操作并重启网关进程——这一项不在本页面的热生效范围内。

最后一句是必要的：`credential_present` 为 false 时运维的第一反应会是"在这个页面上把值填进去"，而页面结构上做不到（§4.2 已确认不存原文）。不写清"哪里能改"，这个红色徽章就只是报警而不给出路。

---

## 4. 场景二：新增 provider

### 4.1 载体选择

**用 `<section>` 内联展开的表单面板，不用 modal。** 理由：字段有 8 项且 model_mapping 是可增删的动态行列表，modal 的 `max-width: 520px`（admin.css:167）与 `max-height: calc(100vh - 40px)`（admin.css:168）会让动态行列表陷入嵌套滚动。`FKAdmin.confirm` 那套 modal 留给"一句话说清后果 + 一两个字段"的危险确认，用在这里是错的工具。

面板沿用 `.imp-box`（admin.css:92-97：`--panel` 底 + 1px border + 10px 圆角 + 14/16px padding），与「导入火山 Key」面板同一视觉。默认折叠，点「新增 Provider」展开并把焦点移到第一个输入框。

### 4.2 字段清单与布局

两列网格（`grid-template-columns: repeat(auto-fit, minmax(300px, 1fr))`，gap 16px，与 `.chart-grid` 同一手法 app.css:120-124），model_mapping 与两个模型列表跨满行。

| # | 字段 | 控件 | 必填 | 新增时 | 编辑时 | hint 文案 |
|---|---|---|---|---|---|---|
| 1 | provider 名 | **select（受限）** | 是 | 从已实现 adapter 中选 | **只读** | 见 5.1 与 4.2.1 |
| 2 | base_url | text | 是 | 可填 | 可改 | `上游 API 根地址，不含 /v1 之后的路径。写错会让该 provider 全部请求失败——这类错会立即报错，不属于静默错误。` |
| 3 | 凭据来源 | text | 是 | 可填 | 可改 | `填环境变量名（如 SENSENOVA_API_KEY），不要在此粘贴密钥原文：本页提交内容会进配置版本历史，历史对所有持看板口令的人可见。疑似密钥原文的输入会被校验直接拒绝。` |
| 4 | quota_kind | select | 是 | 可填 | **只读** | 见 5.2 |
| 5 | quota_limit | number | 是 | 可填 | 可改 + 显示水位 | 见 5.3 |
| 6 | quota_window | select（24h / 5h） | 是 | 可填 | 可改 | `配额周期。24h 配合固定刷新点，5h 是滑动窗口。改动会立即改变配额日的归属计算。` |
| 7 | refresh_hour | number（0-23，可空） | 否 | 可填 | 可改 | `固定刷新点的小时数，留空表示无固定刷新点（滑动窗口 provider 填空）。填 12 表示每日 12:00 前的用量计入前一配额日。` |
| 8 | model_mapping | 动态键值行 | 否 | 可填 | 可改 | `对外模型名 → 上游模型名。留空表示不转换、按原名透传给上游。` |
| 9 | count_models | 多值 text（逗号分隔） | 否 | 可填 | 可改 | `按调用次数计费的模型名，逗号分隔。列在这里的模型不按 token 预扣。` |
| 10 | reasoning_models | 多值 text（逗号分隔） | 否 | 可填 | 可改 | `推理类模型名，逗号分隔。影响 token 用量的估算方式。` |
| 11 | 启用 | checkbox | — | 默认勾选 | 走独立操作 | `不勾选则创建后处于停用状态，调度器不会向它派发请求。` |
| 12 | 变更原因 | text（maxlength 500） | 是 | 必填 | 必填 | `进配置版本历史，回滚时靠它判断该版本改了什么、为什么改。` |

字段容器沿用 `.fld` / `.fld-label` / `.fld-hint`（admin.css:217-219），输入框沿用 `.modal input[type="text"]` 的样式规则（admin.css:221-230）——需把选择器扩展为 `.modal input, .prov-form input` 之类的共用形态，而不是复制一份色值。

### 4.2.1 provider 名改为受限选择器（原设计是自由输入，此处修正）

架构师在通则里提到"新建表单的受限选择器"，**我文档里原本没有这东西**——`#1 provider 名` 上一版是 `text` 自由输入，只在提交后靠 `unsupported_provider` 拦。这是我漏的，现在改掉：新增态用 `<select>`，选项只有已实现 adapter 的名字。

依据是硬编码的：`newAdapterFor`（confsnap/snapshot.go）的 `switch name` 只有 `case "volc"` 与 `case "sensenova"` 两支，`default` 返回 `不支持的 provider=%s`。所以 provider 名不是一个"填什么都行、语义由你定"的标识符，**它必须恰好等于某个已实现 adapter 的名字**。自由输入把这个事实藏起来了：控件形态在说"你可以起任意名字"，而代码只认两个值。

选项来源不硬编码在前端。架构师已把这个 switch 导出为 `confsnap.SupportedProviders()`（hotreload §3.2.1），**由 `GET /admin/providers` 响应的顶层字段 `supported_providers` 返回**（team-lead 最终裁决，见下），前端打开管理页拉列表的那一次请求里就带回来了，选择器与 §8.5 的 `unsupported_provider` 文案共用这一份。前端写死 `['volc','sensenova']` 的代价是下次加 adapter 要改两个地方，而漏改的表现是新 adapter 上线了但页面上选不到——**功能齐备却不可达，且没有任何报错**。

**为什么挂列表响应顶层而不是独立端点**（此处经过两次修正：我原提议挂列表 → team-lead 改独立端点 → team-lead 撤回，定为列表顶层字段。理由按强弱排，最强的在前）：

1. **时序一致性（正确性理由，决定性的一条）**：分两个端点则前端要发两次请求，两次之间可能发生一次配置热加载，于是选项与列表数据来自**不同快照**。这正是本设计用「请求内单快照」要根除的那类不一致——选择器里还留着一个刚被这次热加载移除的 adapter，或者列表里已出现一个选择器还不认识的 provider。挂顶层字段则一次请求、一个快照，必然自洽。
2. **整洁性（team-lead 原来的二分，现降为次要）**：列表接口返回的是**数据**（当前配了哪些 provider），`supported_providers` 是**代码能力**（这个二进制实现了哪几家），两者变更频率与生命周期不同——前者运维每天改，后者只在发版时变。这条是整洁性考虑，抵不过上面那条正确性考虑，所以让位。
3. **我原先的空状态顾虑不成立**，理由比之前更简单：顶层字段在 `providers` 为空数组时照样返回。"第一次新建 provider"发生在列表为空时，那正是选择器必须已有选项的时刻，顶层字段天然满足。

这条修正本身也印证了 §10.1 的记法——**理由的强弱会随认识变化而重排，所以文档里要记住理由而不只是结论**。三次变更的结论各不相同，但只有第一次是我凭顾虑提的，后两次都是有更强理由压过来的。

架构师侧同时有一条反向验证：`SupportedProviders()` 必须由 `newAdapterFor` 的 `switch` 导出，禁止在 handler 里另抄字面量，加一个 `case` 后不改 handler 即应生效。界面侧同理——文案与选择器都从这一份来，前端不得再抄第二份。

**受限选择器不取代 `unsupported_provider` 文案。** 选择器让正常路径下选不出非法值，但那个 code 仍会出现在三种情况里：接口被直接调用（curl / 脚本）、前端选项列表因缓存过期而含已下线的 adapter、以及回滚到一个引用了当时存在、现已删除的 adapter 的旧版本。控件约束和服务端校验各自覆盖不同的入口，这也是架构师说"两侧文案必须同进同退"的前提——两侧都得有文案。

配套：既然新增态能选的就是那几个，§8.2 空状态的引导文案不该写"填入 provider 名"，要写"选择上游厂商"。

**凭据来源字段用 `type="text"`，不用 `type="password"`，不加显示/隐藏切换。** 它存的是环境变量名（如 `SENSENOVA_API_KEY`），本身不是秘密——给它加遮罩会传达错误信号：让运维以为这里可以放密钥原文，恰好是要防的行为。后端表设计已确认不得有任何列存放凭据原文（team-lead 定论），密钥只存在于部署环境的环境变量中。

**疑似密钥原文按 error 处理，不是 warning。** 架构师在网关侧把"长度 > 40 或含小写字母"判定为 error、dry-run 直接拒绝提交，我认同并跟进到界面：这一项不进 §8.3 的 warning 分支，而是走错误分支，`aria-invalid="true"` + 焦点定位到该字段，不提供"我知道风险，继续提交"的旁路。理由与 §7.4 同源——密钥一旦进了版本历史就无法收回（历史是只读的、且会被 `audit_logs` 的 detail JSONB 二次留存），这类不可逆的错误不该由一次勾选来把关。误拒一个合法但形态奇怪的变量名，代价是运维改个名字重提一次；放过一次，代价是一条上游密钥永久留在所有持看板口令的人可以翻阅的表里。

对应的成品拒绝文案（走 §8.3 的错误列表）：

> 凭据来源：这看起来是密钥原文而不是环境变量名。本字段只接受环境变量名（通常是全大写加下划线，如 `SENSENOVA_API_KEY`）。密钥请设置在部署环境的环境变量里，不要经过本页面——本页提交内容会写入配置版本历史，历史记录无法删除。

文案不写"疑似"两个字对运维解释算法（长度 > 40 或含小写字母），因为讲清判据反而会引导人去规避判据。给出正确形态的例子比给出判据更有用。

### 4.3 model_mapping 动态行

```html
<div class="fld map-fld">
  <span class="fld-label">模型映射（对外名 → 上游名）</span>
  <div class="map-rows" id="map-rows">
    <div class="map-row">
      <input type="text" data-map="from" placeholder="deepseek-v3" autocomplete="off" spellcheck="false">
      <svg class="icon" aria-hidden="true"><use href="#icon-arrow"></use></svg>
      <input type="text" data-map="to" placeholder="deepseek-v3-241226" autocomplete="off" spellcheck="false">
      <button type="button" class="icon-btn danger" data-act="map-del"
        title="删除该行映射" aria-label="删除这行模型映射">
        <svg class="icon" aria-hidden="true"><use href="#icon-close"></use></svg>
      </button>
    </div>
  </div>
  <button type="button" class="btn" id="btn-map-add">
    <svg class="icon" aria-hidden="true"><use href="#icon-add"></use></svg>
    <span>添加一行映射</span>
  </button>
  <span class="fld-hint">对外模型名 → 上游模型名。留空表示不转换、按原名透传给上游。</span>
</div>
```

`.map-row` 用 `display: flex; gap: 8px; align-items: center;`，中间的 `#icon-arrow` 复用既有 sprite（admin.css:198 已定义 `.flow .icon` 为 `--text-faint`，这里同色）。

行内 `--text-faint` 箭头图标在 `--panel-2` 底上的对比度约 3.1:1，属非文本图形（WCAG 1.4.11 要求 3:1）达标；它只是方向指示，语义由两侧输入框的 placeholder 与 label 承载，不是唯一信息载体。

删除行按钮用 `.icon-btn.danger`（admin.css:76）。最后一行不允许删除，改为清空两个输入框（避免出现零行时用户不知道怎么加回来）。

### 4.4 提交流程：先 dry-run，再确认

**保存不直接生效。** 两步：

```
[保存配置] → POST 校验端点（dry-run）→ 展示校验结果 → [确认生效] → POST 提交
```

第一步按钮文案是「校验配置」而不是「保存」——文案要如实反映这次点击的后果。校验通过后按钮变为「确认生效」。

校验结果展示区沿用 `.imp-result`（admin.css:123-135）的三态：`.is-busy`（`--warn` 左框）/ `.is-ok`（`--ok` 左框）/ `.is-err`（`--up` 左框）。详见第 7 节状态设计。

---

## 5. 危险字段的交互设计与成品文案

分级判据（**已按 §3.7 的讨论修正过一次**，原判据是"改错会不会当场报错"，现为两层）：

1. **静默算错的 → 一律物理禁止修改。** 不给二次确认的放行通道——摩擦拦不住认为自己有正当理由的人，只会训练他学会怎么点过去。
2. **会当场报错的 → 再看修复路径是否自明。** 自明的（改错就报错、报错直指该字段）提供参照数据让用户自己发现填错，不替他否决；**不自明的（报错指向别处、运维得先推理才知道问题出在哪）同样禁止**。

按此归类：

- **物理禁止修改**：`provider 名`（§5.1）与 `quota_kind`（§5.2）——它们进了 Redis 配额 key 结构，改动等于让配额计数换命名空间，而请求全程返回 200，错误要几天后才在对账里浮现，属第 1 类。回滚是这两个字段唯一的旁路入口，一并堵上（§7.4）。另有 `credential_env` 填入疑似密钥原文（§4.2）——它不属静默算错，但错误不可逆（密钥进版本历史无法收回），按不可逆归入禁止。
- **提供参照，不替用户否决**：`quota_limit`（§5.3）显示实际用量水位，`quota_window` 给内联警告，`base_url` 不拦——写错立即报错，且报错直指该 provider 全部请求失败，修复路径自明。
- **第 2 类中修复路径不自明因而也禁止的**：停用当前默认 provider（§3.7）。故障是"无可用上游"的显性报错，但它指向 Key 与 provider，不指向 `default_provider` 这个全局字段，运维得先想到问题出在默认指向上才能修。

### 5.1 provider 名 — 编辑时物理只读

**交互**：编辑态渲染为 `readonly` 的输入框，不是 `disabled`（`disabled` 无法被键盘 focus，屏幕阅读器会跳过，运维读不到旁边的解释）。`readonly` 保留 tab 可达与可选中复制。

视觉：`--panel` 底（比可编辑字段的 `--panel-2` 更暗，沿用 admin.css:113 `textarea:read-only` 的降级思路）+ `--text-dim` 文字 + 字段名后跟一枚 `#icon-lock` 图标（`--text-faint` 色，14px），`aria-label="不可修改"`。

**成品文案**（`.fld-hint` 位置）：

> provider 名创建后不可修改。它是 Redis 配额 key 的前缀（`volc:quota:token:<key_id>:<日期>`）、用量记录的归档维度，以及调度器筛选 Key 的匹配值。改名会让新名下的配额从零起算、旧名下已记录的用量与历史成为无人认领的孤儿数据，而请求全程返回 200——错误不会当场暴露，通常要到几天后配额对不上账时才被发现。需要换名请新建一个 provider，用「导入 Key」把该批 Key 在新 provider 下重新导入，再停用旧的。**重新导入时每条要用新的 `key_id`**（如 `volc_001` → `volc_v2_001`）：`key_id` 全局唯一，沿用原来的 key_id 会被拒绝——旧 Key 要带着已记录的用量留在旧 provider 下停用，这是这条路径成立的前提。旧 provider 的历史用量按旧名留在归档里，这是对的——那些请求确实是用旧配置发出去的。

### 5.2 quota_kind — 编辑时物理只读

**交互**：与 provider 名同级处理。编辑态渲染为 `readonly`（不是 `disabled`，理由同 5.1：保留 tab 可达与屏幕阅读器可读，运维要能读到旁边的解释）。视觉同 5.1：`--panel` 底 + `--text-dim` 文字 + 字段名后跟 `#icon-lock`，`aria-label="不可修改"`。

编辑态渲染为只读的 text 输入框而非 disabled 的 select——`select` 即使 `disabled` 也仍带下拉箭头，视觉上像"暂时不能改"，而实情是永久不可改。用只读文本框 + lock 图标传达的是"这不是一个可选项"。

**为什么禁改而不是二次确认**：Redis 配额 key 的形态是 `{provider}:quota:{kind}:{key_id}:{day}`——`kind` 是 key 本身的一部分。改 kind 不是"改一个字段"，而是让整批 key 换命名空间：旧 kind 的 key 带着当日已用量继续躺在 Redis 里等 TTL 过期，同时归档表 `key_daily_history` 当天那批行的量纲已经写死。要做对就得跨 Redis 与 Postgres 两个存储保证原子性，代价远超收益（team-lead 与 architect 已就此定论）。既然做不对，就不给这条路。

**成品说明文案**（`.fld-hint` 位置，解释禁改的理由而非只声明禁改）：

> quota_kind 创建后不可修改。它决定预扣与计数的单位（token 记 token 数，量级百万；count 记调用次数，量级百千），并且是 Redis 配额 key 的组成部分：`volc:quota:token:<key_id>:<日期>`。改动它不是换一个字段值，而是让该 provider 的全部配额计数换到一个新命名空间——旧量纲的计数带着当日已用量继续留在 Redis 里，而当天已写入归档表的行量纲已固定，同一 provider 同一天会出现两种量纲的记录。更要紧的是改错之后系统不会报错：把 token 当成 count 解读，一个已用 500 万 token 的 Key 会被读成"已用 500 万次调用、远超 500 次上限"；反向解读则一个刷满 500 次的 Key 会被读成"只用了 500 token、额度几乎全新"，于是被排到最优先派发——与本项目防封禁的意图完全反向。阶段一实测中，这一项配错让历史打分 ratio 虚高 33 倍，而请求全程返回 200。
>
> 需要更换量纲的正确路径：新建一个 provider（用新的 provider 名），用「导入 Key」把该批 Key 在新 provider 下重新导入，再停用旧的。**重新导入时每条要用新的 `key_id`**：`key_id` 全局唯一，沿用原来的会被拒绝——旧 Key 要带着旧量纲已记录的用量留在旧 provider 下停用，否则那批历史就没有归属了。这样两套量纲各自占一个 Redis 命名空间，归档表也不会出现同一 provider 同一天两种量纲。

### 5.3 quota_limit — 显示实际用量水位作参照

**交互**：可自由编辑，但字段下方常驻一块参照信息（不是警告，是数据），编辑态才渲染：

```html
<div class="fld">
  <label class="fld-label" for="prov-quota-limit">配额上限（quota_limit）</label>
  <input type="number" id="prov-quota-limit" min="1" step="1" value="5000000">
  <div class="limit-ref" id="limit-ref">
    <dl class="kv">
      <dt>当前配置上限</dt><dd class="num">5,000,000 token</dd>
      <dt>今日单 Key 最高已用</dt><dd class="num">4,120,338 token（volc_007，水位 82.4%）</dd>
      <dt>近 7 配额日单 Key 峰值</dt><dd class="num">4,880,201 token（volc_003，2026-08-29）</dd>
    </dl>
  </div>
  <span class="fld-hint">…</span>
</div>
```

`.kv` 是既有类（admin.css:186-195：两列 grid，dt 用 `--text-dim`）。`.limit-ref` 只需一条新 CSS：`padding: 10px 12px; border-radius: 8px; background: var(--panel-2); border-left: 3px solid var(--border);`——与 `.imp-result` 的中性态同构。

**实时校验**：输入值低于「近 7 配额日单 Key 峰值」时，把 `.limit-ref` 的左框色改为 `--warn` 并追加一行：

> 新上限低于近 7 日实测峰值 4,880,201。该 provider 下已有 Key 的今日已用量若超过新上限，它们会在下一次预扣时被判定为额度耗尽并退出派发，直到下个配额周期。这不会报错，表现为可用 Key 数突然减少。

输入值低于「今日单 Key 最高已用」时升级为 `--up` 左框：

> 新上限 3,000,000 低于该 provider 今日已有的最高用量 4,120,338。保存后 volc_007 会立即被判定为额度耗尽、退出派发，本配额周期内不再恢复。若这是预期行为（例如上游确实下调了额度），继续；若只是想调整上限，请填写不低于 4,120,338 的值。

`--up` 态**不阻止提交**——上游真的下调额度时这就是正确操作。它的职责是让用户知道自己在做什么，而不是替用户否决。

### 5.4 危险字段汇总表

| 字段 | 编辑态 | 拦截强度 | 是否阻止提交 |
|---|---|---|---|
| provider 名 | `readonly` + lock 图标 | 物理禁止 | 无法输入，不存在提交 |
| quota_kind | `readonly` + lock 图标 | 物理禁止；回滚入口也一并拒绝（§7.4） | 无法输入；跨量纲回滚被拒 |
| quota_limit | number 可改 | 常驻参照数据 + 越界升级警告 | 不阻止 |
| quota_window | select 可改 | 内联警告（改动配额日归属） | 不阻止 |
| base_url | text 可改 | 无（错误会立即报错且直指该字段） | 不阻止 |
| credential_env | text 可改 | 疑似密钥原文 → error 阻断，无旁路（§4.2） | 阻止 |
| 停用默认 provider | 停用按钮不渲染（§3.7） | 物理禁止；后端亦拒（409 `invalid_state_transition`） | 无入口 |

两个 `readonly` 字段的共同点：它们都是 Redis 配额 key 的组成部分（`{provider}:quota:{kind}:{key_id}:{day}`）。**凡是进了 Redis key 结构的字段，界面上一律禁改**——这是一条可以直接套用到未来新字段的判据，比逐个字段讨论要稳。

表里另两项禁止不来自 Redis key 结构，各有独立理由，一并记下以免未来被误当成例外：`credential_env` 的疑似密钥原文是**不可逆**（进了版本历史收不回来）；停用默认 provider 是**修复路径不自明**（报错指向 Key 与 provider，不指向 `default_provider`）。所以完整判据是三条：进 Redis key 结构的、不可逆的、修复路径不自明的——都禁止。

**文案原则**（本节所有文案已遵守）：说清「会发生什么」，不写「此操作有风险请谨慎」。每段警告都包含：改动了什么 → 系统内部会如何解读 → 运维会观察到什么现象 → 该怎么做。

---

## 6. 场景三～四：编辑与停用/启用

### 6.1 编辑

复用 4.2 的同一套表单，差异只在字段的可编辑性（4.2 表格的「编辑时」列）与三条：

1. 面板顶部加一行 `.kv` 摘要，展示不可改的事实：provider 名、创建时间、当前版本号、上次修改人与时间。
2. 表单初始值来自当前生效配置。字段被改动后，label 后追加一枚 `--warn` 色小圆点（`.fld-dirty`，4px 圆点，纯 CSS 无图标），让运维在提交前扫一眼就知道自己改了哪几项。
3. 「校验配置」的 dry-run 结果里必须列出**变更 diff**（见 6.3）。

### 6.2 停用 / 启用

不进表单，走 `FKAdmin.confirm` 弹层，与 Key 的封禁操作同一模式。

**停用弹层**：标题 `停用 Provider`，`danger: true`，`icon: 'ban'`，确认按钮 `确认停用`。

`rows`：provider 名 / 状态流转（`启用 → 已停用`）/ 当前在跑的 Key 数 / 今日请求数。

**成品警告文案**：

> 停用后调度器立即停止向该 provider 的 Key 派发请求，进行中的请求不受影响。指向该 provider 模型名的请求将没有候选 Key 可用，会返回无可用上游的错误——不会静默回退到别的 provider。该 provider 现有 8 个在跑的 Key 会全部退出派发。配置、Key 与历史用量都保留，随时可重新启用。

若该 provider 今日请求数 > 0，追加一个 `required` checkbox（gate 住确认按钮）：

> 我确认该 provider 今日仍有 12,847 次请求在跑，停用会中断这部分流量的后续派发

若今日请求数为 0，不加 checkbox——对零流量 provider 强制勾选是无意义的摩擦，会训练运维养成不看内容就勾的习惯，等真正危险时也照勾。

**启用弹层**：标题 `启用 Provider`，非 danger，`icon: 'restore'`，确认按钮 `确认启用`。

> 启用后调度器立即开始向该 provider 下状态为 active 的 Key 派发请求。当前该 provider 有 8 个 active Key。配额计数从 Redis 中的现有值继续累加，不会重置——若停用期间跨过了配额刷新点，第一批请求可能基于过期的计数做预扣判断，建议启用后观察一个刷新周期。

两个弹层都带 `reasonField()` 变更原因（复用 admin.js:62-69）。

**默认 provider 不进这个弹层。** 目标是当前默认 provider 时停用按钮不渲染（§3.7），运维压根到不了这里。上面那条"今日请求数 > 0 就加 gate checkbox"的规则只适用于非默认 provider——两者是不同性质：有流量是**中断已知流量**（运维知道自己在中断什么，勾选是让他确认规模），是默认则是**移除兜底路径**（故障会落到他没预期的请求上，且报错不指向这个字段）。前者配摩擦，后者禁止。

### 6.3 dry-run 校验结果的 diff 展示

编辑态校验通过后，`.imp-result.is-ok` 内展示变更清单，用既有 `.kv` 两列布局，每行是 `字段名` → `旧值 → 新值`（`#icon-arrow` 分隔，复用 admin.js 的 `transition()` 手法）。

```html
<div class="imp-result is-ok" role="status" aria-live="polite">
  <p class="imp-line">校验通过。以下 3 项将变更，确认后热生效，无需重启网关。</p>
  <dl class="kv">
    <dt>quota_limit</dt>
    <dd><span class="flow"><span class="num">5,000,000</span>
      <svg class="icon" aria-hidden="true"><use href="#icon-arrow"></use></svg>
      <span class="num">6,000,000</span></span></dd>
    <dt>model_mapping</dt>
    <dd>新增 1 条：<code>deepseek-v4</code> → <code>deepseek-v4-flash</code></dd>
    <dt>reasoning_models</dt>
    <dd>移除 1 项：<code>deepseek-v3</code></dd>
  </dl>
</div>
```

**未变更的字段不列出。** 全表列出会让真正的改动淹没在十几行"无变化"里——这正是运维漏看关键变更的成因。

---

## 7. 场景五：配置版本历史与回滚

### 7.1 列表结构

第二个 `<section>`，`.table-wrap` + table，`max-height: 560px` 内滚动（沿用 app.css:149）。默认展示全部 provider 的变更，从列表某行点「历史」图标时按该 provider 过滤（工具栏的 provider 选择器同步选中）。

| 列 | 对齐 | 内容 |
|---|---|---|
| 版本 | 右 | `#128` 等宽 + tabular-nums。当前生效版本追加 `.tag-ok` 徽章 `生效中` |
| 时间 | 左 | `fmtTime()` |
| 操作人 | 左 | `dashboard`（admin_proxy.py:44 已说明共享口令下如实上报为 dashboard，不编造人名）。历史列表同样如实显示，不美化 |
| Provider | 左 | 等宽 provider 名 |
| 动作 | 左 | `.tag` 徽章：`新增` `修改` `停用` `启用` `回滚` |
| 改动摘要 | 左 | 变更字段名列表，如 `quota_limit、model_mapping`。回滚行显示 `回滚至 #124` |
| 变更原因 | 左 | 提交时填的原因，截断 + `title` 全文 |
| 操作 | 左 | `.icon-btn` × 2：查看完整快照 / 回滚到此版本 |

**当前生效版本那一行不提供回滚按钮**（回滚到自己是空操作），该位置留空而不是渲染一个 disabled 按钮——disabled 按钮会让人以为暂时不可用、稍后可用。

### 7.2 查看历史版本快照

用 `FKAdmin.confirm` 的只读形态（无 `fields`，`confirmLabel` 改为 `关闭`，无取消按钮）承载不合适——它是确认弹层。这里用一个独立的只读 modal，复用 `.modal-mask` / `.modal` 样式，内容区是等宽字体的配置快照。

```html
<div class="modal-mask">
  <div class="modal modal-wide" role="dialog" aria-modal="true" aria-labelledby="snap-t">
    <h3 class="modal-title modal-title-plain" id="snap-t">
      <svg class="icon" aria-hidden="true"><use href="#icon-history"></use></svg>
      <span>版本 #124 · volc · 2026-08-29 14:22</span>
    </h3>
    <dl class="kv">
      <dt>操作人</dt><dd>dashboard</dd>
      <dt>动作</dt><dd>修改</dd>
      <dt>变更原因</dt><dd>上游通知额度上调至 600 万</dd>
      <dt>该版本状态</dt><dd>已被 #128 覆盖</dd>
    </dl>
    <pre class="snap-body">…该版本的完整配置，YAML 形态…</pre>
    <div class="modal-actions">
      <button type="button" class="btn" data-role="close">关闭</button>
      <button type="button" class="btn btn-danger" data-role="rollback">回滚到此版本</button>
    </div>
  </div>
</div>
```

三条新增 CSS，全部只用既有 token：

```css
.modal-wide { max-width: 720px; }
.modal-title-plain { color: var(--text); }   /* 既有 .modal-title 是 --up，只读查看不该是红的 */
.snap-body { margin: 0 0 14px; padding: 12px; border-radius: 8px;
             background: var(--bg); border: 1px solid var(--border);
             color: var(--text); font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
             font-size: 12px; line-height: 1.6; max-height: 320px; overflow: auto;
             white-space: pre-wrap; word-break: break-all; }
```

`.snap-body` 用 `--bg` 底（比 modal 的 `--panel` 更暗），与 `.imp-box textarea` 的处理一致（admin.css:103）。

**快照弹层里的「回滚到此版本」按钮同样受 §7.4 约束**：若该版本的 `quota_kind` 与当前生效版本不同，此按钮不渲染，位置改为一行 `--up` 色的说明文字，点击不可用（不渲染 disabled 按钮，理由同 §7.1：disabled 会让人以为稍后可用）：

> 该版本量纲为 token，与当前生效的 count 不同，无法回滚。原因与替代路径见下方说明。

配套的完整拒绝文案与替代路径（§7.4）在按钮位置下方直接展开，不要求运维关掉弹层再去列表里点一次才看到理由。

**快照里的凭据字段只显示环境变量名，不显示解析后的值。** 若历史版本中存有疑似密钥原文，渲染为 `••••（已隐去，历史记录中不回显凭据原文）`。

**脱敏在网关侧也做一遍，前端这层不是唯一防线**（架构师已在网关侧实现）。这个分工是对的：前端脱敏只挡住了从这个页面看的人，挡不住直接调 API 的人，而看板口令是全员共用的（login.html:130-131）。界面这层保留的价值在于，即使后端漏了某个字段，页面上也不会把它显示出来——两层各自独立生效，不互为前提。

### 7.3 回滚确认

`FKAdmin.confirm`，`danger: true`，`icon: 'rollback'`，标题 `回滚配置`，确认按钮 `确认回滚`。

`rows`：Provider / 版本流转（`#128 生效中` → `#124`）/ 目标版本时间 / 目标版本的变更原因。

**成品警告文案**：

> 回滚会以 #124 的配置内容创建一个新版本 #129 并立即热生效，#125 到 #128 之间的 4 次变更全部被覆盖——它们不会被删除，仍可再回滚回去。

`fields` 固定带 `reasonField('例：#128 的上限调整导致 Key 大批退出派发，先回滚')`。

### 7.4 跨量纲回滚：直接拒绝，不提供打字确认

`quota_kind` 在编辑路径上已物理只读（§5.2），**回滚因此成为它唯一可能被改动的入口**。这条路必须一起堵上，否则禁改就只是个摆设——运维想换量纲时会发现"编辑改不了，但回滚到一个老版本就能改"，于是这个后门变成常规操作，而它恰好绕过了 §5.2 论证的全部风险。

我的判断是**直接拒绝，不给打字确认的放行通道**。理由三条：

1. **一致性**：同一个字段，编辑不让改、回滚让改（哪怕加了摩擦），policy 就是自相矛盾的。运维会正确地推断出"这个限制是可以绕的"，进而对界面上其他警告也打折扣。摩擦挡不住一个认为自己有正当理由的人，只会训练他学会怎么点过去。
2. **回滚的语义决定了它不该承载危险变更**：运维点回滚时的心理模型是"退回到一个已知正常的状态"，而不是"做一次配置变更"。在这个动线上他的注意力是最低的——他觉得自己在撤销，不是在改动。把项目最危险的字段放在注意力最低的动线上用打字确认拦，是把拦截放错了位置。
3. **拒绝之后有明确的替代路径可走**，不是把人堵死（见下方文案）。

**不做「部分回滚」**（回滚除 quota_kind 外的所有字段）。它会产出一个与任何历史版本都不相同的新版本，而运维以为自己回到了 #124。这正是本项目要消灭的那类缺陷：操作成功、无报错、但实际状态与用户心智模型不符。

**交互**：拒绝在点击回滚按钮时立即发生，不进 confirm 弹层——弹层的存在本身暗示"确认就能过"。改为在版本历史表格下方展示 `.err-banner`（app.css:280-288），并把该行的回滚按钮置为不可用（此处用 `disabled` 是对的：它确实是这一行特有的、状态性的不可用，与 §5.2 那种永久性禁改不同）。

**成品拒绝文案**：

> 无法回滚到 #124：该版本的 quota_kind 是 token，当前生效的是 count。量纲一经使用即不可切换——Redis 中现有的配额计数是按 count 累加的，回滚不会换算它们，归档表里今天已写入的行量纲也已固定。系统不会因此报错，表现是配额水位与健康分整体失真（阶段一实测为 ratio 虚高 33 倍，刷满额度的 Key 反而被排到最优先派发）。
>
> 若你的目标是回滚 quota_limit 或其他字段：#124 与当前版本共有 3 项差异，除 quota_kind 外为 quota_limit（6,000,000 → 5,000,000）与 model_mapping（少 1 条 deepseek-v4 映射）。请用编辑表单逐项改回。
>
> 若你的目标确实是换量纲：新建一个 provider 用 token 量纲，用「导入 Key」把该批 Key 在新 provider 下重新导入（每条要用新的 `key_id`，沿用原来的会被拒绝——旧 Key 要带着已记录的用量留在当前 provider 下停用），再停用当前这个。

第二段把差异字段与具体值直接列出来，运维不需要再去打开 #124 的快照人工比对——拒绝一个操作的同时要把替代路径铺到可以直接执行的程度，否则拒绝就变成了刁难。

`quota_kind` 一致时，回滚按行为正常放行，不加任何额外摩擦。

**与后端 `confirm_quota_kind_change` 标志位的关系**（架构师在 `docs/provider-config-hotreload.md` §4.8 定义：跨量纲回滚不带该字段即 409 + `quota_kind_mismatch` 终态，不是 200 + `applied:false` 的中间态）：

界面**永远不发送 `true`**。这个标志位在本页面上没有任何入口可以把它置真——跨量纲回滚在前端就被拒绝，请求不会发出。它因此退化为一道服务端的默认安全网：任何不带该字段的跨量纲回滚请求都会被 409 拦下，包括未来某个新客户端、某个手写 curl、或本页面因回归而漏掉这道检查的情形。

这是好设计，不需要改。但请把它理解为**默认拒绝**而非**待确认**：不要在服务端把它做成"前端确认过就放行"的语义并据此放宽其他校验。如果将来有别的调用方需要真正执行跨量纲切换（例如一次性的数据迁移脚本），那条路径应当自己独立论证 Redis 命名空间与归档量纲怎么处理，而不是靠置一个前端本就不会置的标志位来获得放行。

**关于状态码：我原先提的"422 在本项目表示请求体形态不合法"这个前提不成立**，架构师 grep 确认 422 在网关侧全代码库零命中（`dashboard/tests/test_api.py` 里那几处是 FastAPI 对 query 参数的内建校验，不是本项目写的）。我独立复核了这一点：`grep 422` 在 `.go` 文件中无命中，`errorTypeFor`（server.go:542-553）只区分 401/403、429、5xx 与默认四档，没有 422 分支。所以那条既成直觉是我凭空假设的。

结论仍然成立但理由要换：**不该引入 422，因为它会成为网关里独一无二的状态码，谁都没有直觉**——不是因为它已有别的含义，而是因为它一个含义都没有。

采纳的实现是代码库已有的机制：四种拒绝全用 409，靠 `error.code` 分派（表见 §1.8）。跨量纲回滚对应 `quota_kind_mismatch`。这比新造状态码好，因为 `writeError` 的 `code` 参数本来就是为这个设计的，而 `type` 由状态码派生、调用方指定不了——沿用既有机制而不是在它旁边加一套。

---

## 8. 状态设计（5 态全覆盖）

### 8.1 加载中

**provider 列表**：首屏用 `emptyRow(tbody, 12, '正在加载 provider 配置…')`（复用 app.js 既有函数，`.empty` 样式 app.css:178-183）。不做骨架屏——既有 9 个区块全部用这一种加载表达，单独给 provider 加骨架屏会显得是外来的。

**刷新按钮**：沿用 `.btn.is-busy`（admin.css:54，文字转 `--text-dim`）。

**提交中**：`.imp-result.is-busy`（`--warn` 左框）+ 按钮 `disabled`（`.btn:disabled` opacity 0.5，admin.css:46）。文案：

> 正在校验配置…

或提交阶段：

> 正在提交并热加载配置，请勿刷新页面。

表单所有输入框在提交期间转 `readonly`（沿用 admin.css:113 的 `textarea:read-only` 降级为 `--text-dim` 的手法），与导入面板的锁定行为一致（index.html:153 的提示文案已确立这一约定）。

### 8.2 空状态（还没有任何 provider）

不是简单的「暂无数据」。这个空状态出现在系统刚部署完、`providers` 配置段还没填的时候，运维需要知道下一步做什么。

```html
<tr>
  <td class="empty" colspan="12">
    还没有配置任何 provider。网关当前无可用上游，所有请求会返回无可用上游的错误。<br>
    点击右上角「新增 Provider」创建第一个，或确认网关是否仍在读取 config.prod.yaml 的 providers 段。
  </td>
</tr>
```

`.empty` 是既有类。**不放居中大图标 + 大标题 + CTA 按钮那套空状态插画**——运维工具的空状态是一条故障线索，不是引导页。CTA 已经在工具栏，不需要在表格里再放一个。

**筛选无结果**（选了「仅已停用」但没有停用的 provider）是另一条文案，不要复用上面那条：

> 当前筛选条件下没有 provider。已配置 2 个 provider，均为启用状态。

### 8.3 校验失败

`.imp-result.is-err`（`--up` 左框）。逐项列出，不合并成一句。沿用 `.imp-fails`（admin.css:140-147：`--up` 文字，内嵌 `code` 转 `--text`，`max-height: 180px` 内滚动）。

```html
<div class="imp-result is-err" role="alert">
  <p class="imp-line">校验未通过，共 3 项不合法。配置未提交，网关仍在使用当前生效版本。</p>
  <ul class="imp-fails">
    <li>provider 名 <code>Volc Engine</code> 含空格与大写字母。它会成为 Redis key 前缀，只允许小写字母、数字与下划线。</li>
    <li><code>base_url</code> 无法解析：缺少 scheme。应形如 <code>https://ark.cn-beijing.volces.com</code>。</li>
    <li><code>quota_window</code> 为 5h 但 <code>refresh_hour</code> 填了 12。滑动窗口 provider 没有固定刷新点，二者只能配置其一。</li>
  </ul>
</div>
```

每条错误必须包含**哪一项 + 为什么不合法 + 期望形态**。校验失败时焦点移到第一个出错的输入框，并给该输入框加 `aria-invalid="true"` 与 `--up` 边框（新增一条 `.fld input[aria-invalid="true"] { border-color: var(--up); }`）。

**逐字段的即时校验也要有**，不能全等到提交：base_url 的 scheme 校验在 `blur` 时执行。这一项在提交前就能确定对错，攒到最后一次性报三条错是浪费运维一个来回。

（上一版这里还写了"provider 名的字符集校验在 `blur` 时执行"，随 §4.2.1 改受限选择器一并删掉——`<select>` 选不出非法字符，留着这条会让实现者去写一段永远不触发的校验，而它的存在又暗示 provider 名是自由输入的。）

### 8.4 保存成功

`FKAdmin.notify('ok', ...)`（6 秒自动消失，admin-ui.js:43）+ `.imp-result.is-ok` 常驻本次结果 + 列表自动刷新。

> 已提交 · volc 配置已热生效

detail：

> 版本 #129，变更 quota_limit、model_mapping 共 2 项。网关已重新加载，无需重启。

**成功即确定性成功，不做"可能部分生效"的暗示。** 部署拓扑是单实例（已核实：`docker-compose.yml` 的 gateway 无 `replicas` / `scale` 指令，主机侧由 systemd 单实例托管），热加载 API 同步返回已生效与新版本号。因此不存在"多实例部分生效"这一状态，界面不为它预留文案——预留一段永远不会触发的警告，只会让运维怀疑每次成功是否真的成功。

若将来改为多实例部署，这里需要同步补回一个 `notify('warn', ...)` 分支，且**前提是热加载 API 能返回各实例的重载确认**。在接口给不出这个信息之前，界面无法如实区分"全部生效"与"部分生效"，写出来就是空头承诺。

网关侧返回非 2xx 时按 §8.3 / §8.5 的错误路径处理，不落到成功态。**不显示假的成功**——这条与 app.js 对出口 IP 表格的处理原则一致（index.html:212-213 明确：网关不可达时显示"未知"而不是退化成看起来正常的旧值）。

### 8.5 Edge（边界）

| 边界 | 处理 |
|---|---|
| provider 名超长（> 40 字符） | 新增态已是受限 `<select>`（§4.2.1），运维无法输入超长值，`maxlength` 那条约束随之作废。**但列表侧的 `.ellip` 截断 + `title` 全值要保留**——旧版本快照、回滚 diff 与版本历史里可能存在早年从 YAML 直接写入的长名字，展示路径仍要能容纳它 |
| base_url 超长 | `.ellip` 限宽 260px + `title` |
| model_mapping 行数极多（> 50） | `.map-rows` 加 `max-height: 320px; overflow: auto`，列表列只显示条数 |
| quota_limit 填 0 或负数 | `min="1"`，校验端点也必须拦（前端校验不能是唯一防线） |
| 版本历史极长（> 500 条） | 后端分页，表格容器内滚动 + 「加载更多」按钮。不做无限滚动（运维要能定位到具体版本号，无限滚动破坏可寻址性） |
| 两人同时改同一 provider | 提交带 `expected_version`，冲突返回 409 + `version_conflict`。文案见下 |
| 网关不可达 | 校验端点返回 503 时，`.imp-result.is-err`：`网关不可达，无法校验配置。配置未提交。请先确认网关容器状态——在网关恢复前，配置变更无法热生效。` |
| 请求超时 | 复用 admin-ui.js:195-198 的既有文案约定：`本地等待超过 20 秒已中断。请求可能已到达网关，请刷新后确认实际状态，不要直接重试。` |

**四种 409 的文案，按 `error.code` 分派**（不按状态码——四种同为 409，见 §1.8）：

`version_conflict`：

> 提交失败：该 provider 的配置在你编辑期间已被改动，当前版本已是 #130，而你基于 #128 编辑。你的改动未提交。请刷新后重新打开编辑表单——直接重试会用你的旧视图覆盖掉别人刚做的变更。

`immutable_field`：

> 提交失败：provider 名与 quota_kind 创建后不可修改，本次提交试图改动其中一项。你的改动未提交。要更换这两项中的任何一个，正确路径是新建一个 provider，用「导入 Key」把该批 Key 在新 provider 下重新导入（每条要用新的 `key_id`，沿用原来的会被拒绝——旧 Key 要带着已记录的用量留在当前 provider 下停用），再停用当前这个。

**注意 `immutable_field` 现在有两个来源**，文案要按来源分支：改 provider 名 / quota_kind 是一个来源（上面这条），**导入 Key 时试图给已存在的 `key_id` 换 provider 是另一个来源**（§11 第 9 项的 handler 检查）。后者的文案见 ⑪-d。同一个 code 两种场景，前端按提交入口分派——编辑表单提交后收到它显示上面这条，导入表单提交后收到它显示 ⑪-d 那条。不能共用一条泛化措辞，否则运维在导入表单里读到"provider 名与 quota_kind 不可修改"会完全对不上他刚做的事。

`quota_kind_mismatch`（前端已在点击时拦下、正常不该到这里，但作为后端独立防线的兜底文案仍需存在）：

> 回滚失败：目标版本的 quota_kind 与当前生效版本不同，量纲一经使用即不可切换。未做任何改动。若你要回滚的是 quota_limit 或其他字段，请用编辑表单逐项改回；若确实要换量纲，新建一个 provider 再停用当前这个。

`invalid_state_transition`：

> 操作失败：{具体原因，二选一}
> · 停用被拒时：volc 是当前的默认 provider，不能直接停用。请先把默认指向另一个启用中的 provider，再回来停用它。
> · 设默认被拒时：sensenova 当前处于停用状态，不能设为默认——未指定 provider 的请求会没有兜底路径。请先启用它。

**400 + `unsupported_provider`**（新建时 provider 名不在已实现 adapter 列表内）：

> 提交失败：provider 名 "openai" 当前不受支持。你的改动未提交。本页面目前支持的上游厂商是：volc、sensenova。若要接入其他厂商，需先为它实现一个 adapter 并发一次版本——这不是配置问题，而是代码能力边界。

**写清判据，因为运维绕不过它。** 架构师在拒绝文案通则（hotreload §4.1.1 通则表）里定的："支持哪几家由代码决定，含糊只会让他把厂商名反复试一遍"。provider 名不是一个"填什么都行、语义由你定"的标识符，它必须恰好等于某个已实现 adapter 的硬编码名（`newAdapterFor` 的 `switch name`，confsnap/snapshot.go）。文案不写支持列表，运维得去试 "qwen" / "doubao" / "claude" 各一遍，最后才确认"不是我打错字，是真的没有"。

**列表从哪来**：`GET /admin/providers` 响应顶层的 `supported_providers`（同 §4.2.1，与受限选择器共用页面初次拉列表的那一次请求，不各拉一遍——分开拉会让文案里的支持列表与选择器的选项落在两个快照上，见 §4.2.1 的时序一致性理由）。文案里的 `volc、sensenova` 这一串不能硬编码——下次加 adapter 会失效，而失效的表现是文案少列一家、运维以为不支持，刚好是这条文案本要防的后果。措辞形态是「本页面目前支持的上游厂商是：{joined_names}」，`{joined_names}` 为该数组用顿号拼接的结果。

**新增态的受限选择器不取代这条文案。** 选择器让正常路径下选不出非法值，但这个 code 仍会出现在三种情况：接口被直接调用（curl / 脚本）、前端选项列表因缓存过期而含已下线的 adapter、以及回滚到一个引用了当时存在、现已删除的 adapter 的旧版本。服务端 message 与界面文案对同一个 code 给出的判据披露程度要一致（架构师通则最后一段），所以两侧都写。

**未识别的 `error.code`**：显示后端返回的 `message` 原文 + 一句 `该错误界面未做专门处理，请把上面这句连同你刚做的操作报给维护者。` 不要静默降级成一句通用失败——`error.code` 是会随后端演进增加的，界面认不出的那些，原文比编造的解释有用。

---

## 9. 响应式与无障碍

### 9.1 响应式

看板是桌面运维工具，既有代码只有一条移动端断点（admin.css:290-294，`max-width: 640px` 处理 toast 与 `.kv`）。本设计沿用同一取舍：

- provider 表格在窄屏下横向滚动（`.table-wrap` 已是 `overflow: auto`），不做卡片化重排——重排会破坏列对齐，而列对齐是这张表的核心价值。
- 表单两列网格用 `minmax(300px, 1fr)` 自动降为单列，无需额外断点。
- `.modal-wide` 的 `max-width: 720px` 在窄屏由 `.modal` 既有的 `width: 100%` + `padding: 20px`（admin.css:159）自然收敛。
- `< 640px` 时 `.kv` 已由既有规则转单列（admin.css:292）。

### 9.2 无障碍

| 项 | 处理 |
|---|---|
| focus 可见 | 沿用 `outline: 2px solid var(--accent); outline-offset: 1px`（admin.css:79-86）。新增的 `input[type="number"]`、`.prov-form input`、`.snap-body` 需加入该选择器列表 |
| 键盘可达 | 表单全程 tab 可达；只读字段用 `readonly` 而非 `disabled` 保留可达性；modal 焦点陷阱由 `FKAdmin.confirm` 既有实现提供（admin-ui.js:127-142），只读快照 modal 需复用同一套 `onKey` 逻辑而不是重写 |
| Esc 关闭 | 既有 modal 已支持（admin-ui.js:134）；只读快照 modal 同样支持 |
| 图标按钮标签 | 每个 `.icon-btn` 必须有 `title` + `aria-label`，且 `aria-label` 含具体 provider 名（沿用 admin.js:48 的写法：`aria-label="恢复 volc_001"`） |
| 状态不只靠颜色 | `.tag` 徽章始终带文字（`启用` / `已停用` / `在跑` / `无流量`），水位条旁始终有百分比数字（app.css:204 `.pct`）；错误项除 `--up` 色外带 `aria-invalid` 与文字说明 |
| 动态区域播报 | `.imp-result` 带 `role="status" aria-live="polite"`（校验中/成功）或 `role="alert"`（失败），沿用 index.html:156 的既有写法 |
| 触摸目标 | `.icon-btn` 是 28×28，低于 44×44。这是既有代码的**已知取舍**，admin.css:56-57 注释已记录理由（桌面运维工具密度优先，44px 由 padding + 单元格行距共同满足）。本设计沿用同一取舍，不为 provider 单独破例——同一张表里两种尺寸的按钮更糟 |
| reduced-motion | 新增组件的 transition 一律加进 admin.css:296 已有那条规则的选择器列表 |
| 对比度 | 所有文字组合沿用既有 token 搭配，未新造：`--text` on `--panel` ≈ 12.6:1；`--text-dim` on `--panel` ≈ 5.6:1；`--text-faint` on `--panel` ≈ 3.5:1（仅用于 `.fld-hint` 等 12px 辅助文字，**这一项低于 4.5:1**，属既有代码已有问题，本设计不引入新的 faint 文字承载关键信息，关键说明一律用 `--text-dim` 或 `--text`） |

---

## 10. 自检清单（13 点交付自检 + 2 条通用判定）

| # | 检查项 | 结果 | 依据 |
|---|---|---|---|
| 1 | 无 emoji 作为功能图标 | **通过** | 全文档零 emoji；图标一律 Lucide inline SVG，新增 6 个 symbol 进既有 sprite（§1.7）。既有 admin-ui.js:7 已明文禁止 emoji 与 Unicode 符号字符，本设计延续 |
| 2 | 无紫→粉渐变 | **通过** | 全设计零 `linear-gradient`。既有代码同样零渐变。`--accent: #7c8cf8` 是纯色单用，未参与任何渐变，也未与粉色配对 |
| 3 | 无 AI 模板味 | **通过** | 无大标题+三卡片+居中 CTA；无营销文案；无彩色圆形图标底（§3.3 明确拒绝）；空状态是故障线索而非引导插画（§8.2）；无 ALL CAPS 小标签、无编号 section 标记 |
| 4 | 所有颜色走既有 token | **通过** | 新增 CSS 共 8 条（`.prov-name` `.ellip` `.limit-ref` `.modal-wide` `.modal-title-plain` `.snap-body` `.fld-dirty` `aria-invalid` 边框），全部引用 `var(--*)`，零裸色值 |
| 5 | 间距为 4px 网格 | **通过** | 只用 §1.3 已提取的既有值（4/6/8/10/12/14/16/20/24/28）。9px 那一处是既有 h2 的光学对齐，本设计不新增非标值 |
| 6 | 字体与字号不新造 | **通过** | 沿用 §1.2 的两条字体栈与 6 级字号。provider 名、快照、映射用等宽栈（标识符该像标识符），其余用正文栈 |
| 7 | 信息密度优先，不是 landing page | **通过** | 表格为主，`VISUAL_DENSITY=8`；列表 12 列一屏可比对；未用卡片包裹数据；§3.1 已论证为何不用卡片网格 |
| 8 | 危险字段有分级拦截 | **通过** | 判据经 §3.7 讨论后修正为三条——进 Redis key 结构的、不可逆的、修复路径不自明的，一律物理禁止：provider 名与 quota_kind 只读（§5.1 / §5.2）+ 回滚旁路一并拒绝（§7.4）；疑似密钥原文 error 阻断无旁路（§4.2）；停用默认 provider 按钮不渲染（§3.7）。只有"改错立即报错且报错直指该字段"的才放行并给参照数据（quota_limit / quota_window / base_url，§5.3）。判据与归类写在 §5 开头与 §5.4，可套用到未来新字段 |
| 9 | 警告文案说清后果，非空话 | **通过** | §3.7 / §3.8 / §5.1 / §5.2 / §6.2 / §7.3 / §7.4 每段均含：改了什么 → 系统如何解读 → 会观察到什么现象 → 该怎么做。无"请谨慎操作"类表述。§3.8 特别补了"哪里才能改"，避免报警而不给出路 |
| 10 | 5 态全覆盖 | **通过** | Loading §8.1 / Empty §8.2（含空库与筛选无结果两条不同文案）/ Error §8.3 / Populated §3.5 / Edge §8.5（8 项边界） |
| 11 | 保存前 dry-run 且结果可读 | **通过** | 两步提交，第一步按钮文案是「校验配置」而非「保存」（§4.4）；结果三态复用 `.imp-result`；编辑态列 diff 且不列未变更字段（§6.3） |
| 12 | 键盘可达 + focus 可见 + reduced-motion | **通过** | §9.2。只读字段用 `readonly` 而非 `disabled` 以保留可达性；modal 焦点陷阱复用 admin-ui.js 既有实现而非重写 |
| 13 | 未改动 dashboard 下任何代码文件 | **通过** | 本轮仅新建 `docs/provider-config-ui.md`。§1.7 / §9.2 中提到的 admin-icons.js 与 admin.css 改动是**给 Phase 3 前端的实现指引**，本轮未执行 |
| 14 | 每个控件的**形态**与事实相符 | **通过（本轮新增，见 10.1）** | provider 名从自由输入改受限 `<select>`（§4.2.1）；导入 hint 的 provider 说明改条件式而非"必填"（§11 ⑪-b）；upsert 语义写明 `key_id` 全局唯一且换 provider 会被拒（§11 ⑪-c 定稿版）。三处原本都不是校验规则写错，是形态在承诺一件事实不支持的事 |
| 16 | 临时实现在**形态上可识别** | **通过（新增，见 10.1）** | 本轮一度需要给受限选择器留硬编码占位（`capabilities` 端点尚不存在），后因改挂列表顶层字段而消解。判定保留：临时代码与最终代码长得一样时，没有任何机制提醒它是临时的 |
| 15 | 拒绝文案的判据披露程度与"能否绕过"匹配 | **通过（本轮新增，见 10.1）** | 绕不过的写全合法取值：`unsupported_provider` 列出支持厂商（§8.5）。能绕过的不写判据：疑似密钥原文只说被拒 + 正路（§4.2）。两侧服务端 message 与界面文案对同一 code 披露程度一致 |

### 10.1 三条通用判定（供后续新场景直接套用）

前 13 条是对本文档的逐项检查，下面三条是**判定方法**，新增字段、控件、拒绝文案时直接套，不必每次重新讨论。

**判定控件该长什么样：问这个控件的形态本身在向运维承诺什么。**

校验规则对不对是第二个问题。第一个问题是控件不说话时运维会推断出什么——文本输入框在说"这个字段的值由你定"，`maxlength="40"` 在说"约束是长度"，"必填"在说"无条件必填"，"同 `key_id` 重复提交是更新"在说"key_id 是唯一维度"。四句里有三句与代码事实不符，而且不符时**不会有任何报错**：运维照形态推断，推错了也只是拿到一个自己不知道错在哪的结果。

本轮三处缺口全是这一类，没有一处是校验规则写错。这也是为什么"加一条前端校验"通常不是修复：正则拦住非法字符，可 `openai` 是合法字符组成的非法值，形态仍在说谎。修复是换形态——`<select>` 一旦只列已实现 adapter，就不必再解释"为什么我填的名字不行"。

顺带一条推论：**永不触发的校验比没有校验更糟。** §8.3 删掉的那句 provider 名字符集校验，留着会让后来的人以为字符集是真实约束维度，而真实维度是"必须等于某个已实现 adapter 的名字"。

**判定拒绝文案该说多少：假设文案已贴在运维面前，问他看完能不能在本页把这次提交塞过去。**

（architect 的通则，hotreload §4.1.1 之后。）

- **能塞过去** → 判据本身是防线，写出来等于自废。只说被拒绝 + 该走哪条正路。实例：疑似密钥原文（§4.2 检查 9b）——把"长度 > 40 或含小写字母"讲清楚，运维为了过检查会去改凭据的写法，而不是改用环境变量名。
- **塞不过去** → 判据是事实说明，藏起来只制造无效重试。必须写清并列全合法取值。实例：`unsupported_provider`（§8.5）——支持哪几家由代码决定，不列出来运维会把 `qwen` / `doubao` / `claude` 各试一遍。

这两条是一对：一条判定文案该说多少，一条判定控件该长什么样。都指向同一件事——**界面提供的信息量要匹配运维实际能做的事**，多了变成绕过路径，少了变成无效重试。

**判定临时实现能不能就这么放进去：问它在形态上跟最终实现有没有区别。**

（第三条，与上面第一条同源：那条说控件形态在向运维承诺什么，这条说代码形态在向后来的维护者承诺什么。）

**临时代码必须在形态上可识别，否则它就是永久代码。** 本轮的实例：受限选择器一度要依赖一个还不存在的能力端点，可选方案是先在前端写死 `['volc','sensenova']` 作占位。这个占位的问题不是它错——它当时是对的——而是**占位代码和最终代码长得一模一样**，没有任何机制提醒下一个读到它的人这是临时的。它会一直留着，直到某天新 adapter 上线、页面上选不到、而且不报错。

这次侥幸不需要占位（改挂列表顶层字段后，数据来自页面本来就要拉的请求），但判定要留下。真需要临时实现时，形态上至少要有一处与最终实现不同：显式的 `TODO` 加上失效条件（"加第三个 adapter 时此处会静默漏项"）、或一条断言在数据源可用后立刻失败、或干脆把顺序调开让它不必存在——本轮选的是第三种，也是最干净的一种。

三条判定同源于一个观察：**形态是一种承诺，而承诺与事实不符时通常不会有任何报错。** 控件形态骗运维，代码形态骗维护者，文案的披露程度骗的是运维对"我还能不能试一次"的判断。三处都没有报错机制兜底，所以只能靠设计时问一句。

## 11. 已定论的后端契约（本文档已按定论修订）

初稿基于推断做了若干假定，team-lead 与架构师已逐条核实定论，文档相应章节已改。此处记录结论与影响范围，供 Phase 3 前端与 Phase 4 验收对照。

**① 凭据字段只存环境变量名**（维持初稿假定）。后端表设计已明确不得有任何列存放凭据原文——配置进版本历史表且任意历史版本可查，凭据原文进去会被 `audit_logs` 的 detail JSONB 二次留存。界面上是普通 `type="text"` 输入框，**不加密码遮罩、不加显示/隐藏切换**：它不是秘密，加遮罩反而暗示这里可以放密钥原文，恰好是要防的行为。→ §4.2 字段 3 与其后的说明段。

**② quota_kind 由「二次确认」降级为物理只读**（推翻初稿设计）。不做原子的「改量纲 + 清计数」：Redis key 是 `{provider}:quota:{kind}:{key_id}:{day}`，`kind` 是 key 本身的一部分，改它是整批 key 换命名空间而非清计数，且归档表当天的行量纲已写死，跨 Redis 与 Postgres 保证原子性代价远超收益。→ §5.2 全节重写为 readonly，初稿那套「内联警告 + 弹层 + 打字确认」已删；33 倍 ratio 虚高的后果文案保留并移入 readonly 字段的说明，因为它解释了为什么禁改，比"此字段不可修改"有信息量；§5 开头的分级判据、§5.4 汇总表同步改。

**③ 跨量纲回滚：由「打字确认后放行」改为直接拒绝**（设计师判断，team-lead 授权自行定夺）。②使 quota_kind 在编辑路径上禁改后，回滚成为它唯一的旁路入口。选择拒绝而非加摩擦的三条理由见 §7.4：policy 自相矛盾会让运维对界面其他警告一并打折扣；回滚动线上运维的心理模型是"撤销"而非"变更"，注意力最低，把最危险的字段放这里拦是拦错了位置；拒绝的同时已把替代路径铺到可直接执行（列出差异字段与具体值 + 指向新建 provider）。同时明确不做「部分回滚」——它会产出与任何历史版本都不同的新版本而运维以为回到了 #124，正是本项目要消灭的那类"操作成功但状态与心智模型不符"的缺陷。

**④ 热加载无多实例语义，「部分生效」文案删除**。已独立核实部署拓扑：`docker-compose.yml` 的 gateway 无 `replicas` / `scale` 指令，主机侧由 systemd 单实例托管，生产是单实例。热加载 API 同步返回已生效与新版本号，界面按确定性成功处理。→ §8.4 删去 warn 分支，并留下改为多实例时需要补回的前提条件（接口须能返回各实例重载确认，否则界面无法如实区分）。

**⑤ 疑似密钥原文按 error 拒绝，不按 warning 放行**（架构师定，比初稿更严，界面已跟进）。网关侧判据是长度 > 40 或含小写字母，dry-run 直接拒绝提交。界面不提供"我知道风险，继续提交"的旁路——密钥一旦进版本历史就无法收回（历史只读且被 `audit_logs` 二次留存），这类不可逆错误不该由一次勾选把关。→ §4.2 字段 3 hint 与其后的说明段、成品拒绝文案。

**⑥ 历史快照脱敏在网关侧与前端各做一遍**（架构师已在网关侧实现）。分工正确：前端脱敏挡不住直接调 API 的人，而看板口令全员共用（login.html:130-131）。界面这层保留的价值是即使后端漏了某个字段页面也不显示——两层独立生效，不互为前提。→ §7.2。

**⑦ `credential_present` 布尔落地到两处界面**（架构师提供，`os.Getenv` 是否非空，不回值）。列表 Provider 列加 `--up` 色 `凭据未设置` 徽章（级别等同「可用 Key 为 0」，是运行时故障不是配置瑕疵）；编辑表单该字段上方加说明，并**明确写出"设置变量的值要在部署环境操作并重启，不在本页热生效范围内"**——不写清哪里能改，红色徽章就只是报警而不给出路。→ §3.8。

**⑧ `default_provider` 已补独立端点 `PATCH /admin/config/default-provider`**（架构师提问，设计师答需要，架构师已在 hotreload §4.1.2 落地）。缺了它会由页面制造一个页面自己修不了的坏状态：运维在页面上停用了恰好是默认的那个 provider，兜底路径就指向已停用项，而修复只能改 yaml 加重启。载体不做成表单 checkbox，因为它是全局互斥单值。→ §3.7。

**⑧-b 停用默认 provider 从「gate checkbox 放行」改为硬拒绝**（架构师在 §4.6 做成停用前置检查，设计师撤回原分级论证）。我原先按"显性故障配摩擦"给了勾选放行，缺口在于**可见不等于可修复**：故障是"无可用上游"，指向 Key 与 provider 而非 `default_provider` 这个全局字段，修复路径比故障本身隐蔽；而替代动作只有两步，拒绝成本极低。→ §3.7 停用按钮不渲染；§5 与 §5.4 的分级判据相应修正为三条（进 Redis key 结构的、不可逆的、修复路径不自明的，都禁止）。

**⑨ `confirm_quota_kind_change: true` 界面永不发送**（架构师定义的标志位，界面侧的定位说明）。跨量纲回滚在前端已被拒绝，请求不会发出，该字段在本页面没有入口可置真。它退化为服务端的默认安全网。架构师已按**默认拒绝**实现：不带字段即 409 终态，不是 200 + `applied:false` 的中间态，且该标志位只放行 `quota_kind` 一项检查，不放宽 credential_env 缺失 / adapter_kind 不支持 / 版本冲突任何一条。→ §7.4 末段。

**⑩ 我提的「422 在本项目表示请求体形态不合法」这个前提不成立，但不引入 422 的结论保留**。架构师 grep 确认 422 在网关侧零命中，我独立复核过：`errorTypeFor`（server.go:542-553）只有 401/403、429、5xx 与默认四档，无 422 分支。所以那是我凭空假设的既成直觉。理由换成：引入 422 会造一个网关里独一无二的状态码，谁都没有直觉——不是因为它已有别的含义，而是因为它一个含义都没有。**采纳的实现是四种 409 靠 `error.code` 分派**（`version_conflict` / `immutable_field` / `quota_kind_mismatch` / `invalid_state_transition`），沿用 `writeError` 既有机制而不是在它旁边加一套。→ §1.8 分派表、§8.5 四种文案、§7.4。

**前端实现的硬要求**：错误分派按 `error.code` 而非 `status`。四种同为 409，只看状态码会退回模糊提示。`admin-ui.js` 的 `request()` 现在只挂 `err.status`（admin-ui.js:218），需扩展 `err.code`——从响应体 `error.code` 读（网关的 `errorEnvelope` 结构见 server.go:519-526）。注意既有 dashboard 端点走的是 FastAPI 的 `detail` 字段（admin-ui.js:217），两种形态并存，取值要兼容。

**⑪ 换 provider 名 / 换量纲的替代路径措辞改为「重新导入 Key」**（team-lead 裁决不做 Key 迁移端点，架构师同步口径）。裁决理由界面侧完全认同，而且它让文案更好写：迁移的措辞暗示存在一个"把 Key 搬过去"的动作，运维会去界面上找它、找不到就以为功能缺失；重新导入指向的是页面上已经存在的东西。四处文案已改（§1.8 分派表、§5.1、§5.2、§7.4、§8.5 的 `immutable_field`），措辞统一为「用「导入 Key」把该批 Key 在新 provider 下重新导入」——**带上功能名，因为那个区块的实际标题是「导入火山 Key」**（index.html:137），不写清运维会在页面上找一个叫"导入 Key"的区块而看到的是"导入火山 Key"。

我在 §5.1 顺手补了一句"旧 provider 的历史用量按旧名留在归档里，这是对的——那些请求确实是用旧配置发出去的"。原文案只说旧数据会成为"无人认领的孤儿数据"（那是在说改名场景），不说清新建场景下旧归档留着是**正确**的，运维走完正确路径反而会怀疑自己漏了收尾步骤。

**⑪-b 一个界面前置缺口，需要 fe-provider 在 Phase 3 一并处理。** 我核实了重新导入这条路径在既有功能上是否真的走得通：网关侧 `handleAdminKeysImport` 支持每条 item 带 `provider` 字段，并会校验它在配置中存在（admin.go:290-313），多上游时必须显式指定否则报错——**所以后端能力是完整的**。但既有导入表单的 placeholder 只给了 `{"key_id":"volc_001","secret":"...","pool":"hot","persona_id":"p_day"}`（index.html:146），**没有 `provider` 字段**，hint 文案（index.html:139-141）也只说"每条必须含 `key_id`"。单上游时代这样是对的，多 provider 之后运维照 placeholder 填就会撞上"provider 不能为空"。建议 Phase 3 在 placeholder 补 `"provider":"volc"`——这不是新功能，是让既有表单的示例跟上后端已有的校验。

hint 里的 provider 说明**写成条件式，不写成"必填"**：「配置多个上游时必填；单上游可省略，由默认上游推导」。依据是 `it.Provider = snap.Cfg.ResolveProvider(strings.TrimSpace(it.Provider))`（admin.go:292）——单上游会自动补全，只有多上游且未设默认才落到 `:294` 报空。写成无条件"必填"的代价是：单上游部署的运维看到它会以为自己一直漏了字段，回头去翻自己那份跑了很久的导入清单找不存在的问题。条件式措辞同时告诉他"你现在这样是对的"和"哪天加了第二个上游就要补"。

**⑪-c upsert 语义文案：按 key_id 全局唯一写（约束变更方案已撤回，此处是第三版，也是定稿）。**

这一处经过三次改口，把过程记全，因为最后的结论与第一版最接近但理由完全不同：

| 版本 | 结论 | 理由 | 状态 |
|---|---|---|---|
| 一 | 不写"两条独立记录"，因为持久层是单列唯一 | 读 `schema.sql:38` + `upstreamkeys.go` 的 `ON CONFLICT (key_id)` | 结论对，但只当成"待 architect 定夺的分歧" |
| 二 | 改约束成 `(provider, key_id)` 复合，hint 写组合判据 | team-lead 裁决，用 `schema.sql` 内部矛盾作证据 | **已撤回** |
| 三 | 保持单列唯一，hint 写明 provider 不可变，handler 层 409 硬堵 | architect 用 `upstreamkeys.go:90` 反推运行事实 | **定稿** |

**撤回第二版的决定性证据是一条运行事实，不是文本比对**：`upstreamkeys.go:90` 写的是 `ON CONFLICT (key_id) DO UPDATE`。这个冲突目标要求 `key_id` 上存在单列唯一约束——若生产真是 `(provider, key_id)` 复合唯一，这条语句会直接报"没有匹配的唯一约束"，导入功能整体不可用。既然导入一直在用，**生产必然是单列唯一**。我复核确认 `schema.sql:38` 仍是 `key_id TEXT NOT NULL UNIQUE`，与之一致。

**而且第二版的迁移会在启动期打死生产。** `Migrate` 由 `auto_migrate` 启动钩子自动执行（`store.go` 内 `if cfg.AutoMigrate { s.Migrate(ctx) }`，`cmd/gateway/main.go` 同样有一处），线上若已存在同 `(provider, key_id)` 的重复行，`ADD CONSTRAINT` 会失败 → `Migrate` 报错 → **网关起不来**。给运维一份检测重复行的 SQL 兜不住这个，因为检测是人工的、迁移是自动的。这类"数据修正塞进启动期自动迁移"的形状本身要避开。

**推理错在哪，这个边界要写清，否则后来人会重走一遍**：`key_daily_history` 是**归档表**，天然按 (key, provider, day) 分维度，所以它的主键必须含 provider；`upstream_keys` 是**实体表**，`key_id` 单列唯一就是这把 Key 的身份定义。两张表维度不同，`schema.sql:118-119` 那条注释（我核实过，原话确实是「provider 必须进主键: 同一 Key 迁到别的上游后，两段历史属于不同的额度体系（水位不同），合并成一行会让 token_ratio 失去意义」）证明的是**归档表需要 provider 进主键**，推不出实体表要复合唯一。第二版是拿归档表的维度推实体表的身份。

### 定稿文案

导入表单 hint（index.html:139-141）：

> 网关逐条 upsert，同 `key_id` 重复提交是更新。`key_id` 全局唯一，**换 provider 重新提交会被拒绝**——一把 Key 不能改上游归属。上限 8MB。

**为什么必须写"会被拒绝"这半句**：`key_id` 全局唯一这个事实单独摆出来不足以让运维预判后果。他会以为"全局唯一"意味着换 provider 重导会报重复，实际现行代码是 `provider = EXCLUDED.provider` 无守卫（`upstreamkeys.go:96`）——**静默改写这把 Key 的归属并报告成功**。所以文案要说的不是唯一性，是拒绝行为。

**本期处置：handler 层前置检查 → 409 + `immutable_field`，不动 SQL。** `ON CONFLICT DO UPDATE` 语法上表达不了"冲突即拒绝"，硬要在 SQL 层做只能改成 `DO NOTHING` 再判受影响行数，那会把"更新"这个正常路径也一起废掉。前置检查（查一次现存 provider，不同则拒）语义直白且不影响正常 upsert。

**落地顺序约束依然成立，标的从 SQL 约束换成 handler 检查**：文案不能先于 handler 层的 409 检查上线。文案承诺一个尚未实施的拒绝，运维照文案预期"会被拦住"，实际被静默改写——那个中间版本比两头任何一头都糟，因为它把运维的警惕心也一起消掉了。逻辑与第二版下的判断完全一样，只是依赖对象变了。

下面保留原始核实过程，它是三次改口的共同依据。

我最初没写这句，因为核实到的持久层行为与「组合唯一」相反。team-lead 引的 `admin.go:313 附近`实际是 L315-321，`compositeKey := it.Provider + ":" + it.KeyID` 配一个 `seen` map——那是**同一批请求内**的去重口径，不是数据库的唯一约束。当时决定"重复提交是更新还是新建"的两处是：

- `internal/store/schema.sql`：`key_id TEXT NOT NULL UNIQUE` —— **key_id 单列唯一**，无 `(provider, key_id)` 复合约束
- `internal/store/upstreamkeys.go`：`ON CONFLICT (key_id) DO UPDATE SET ... provider = EXCLUDED.provider` —— 冲突目标是 key_id 单列，且命中后**无条件覆盖 provider 列**

这两处是同一份 schema 在跑：`docker-compose.yml` 把 `internal/store/schema.sql` 挂到 `docker-entrypoint-initdb.d`。（原先另有三个部署脚本打包这一份，它们已于 2026-09-13 随 real_upstream_test/ 移除，现在在跑的只有这一份。）

`deploy/init.sql` 里确实有 `CONSTRAINT uq_provider_key UNIQUE (provider, key_id)`，看着支持你的说法，但那份**不是在跑的那份**：它没有 key_id 单列 UNIQUE，列结构也对不上（`secret_enc BYTEA` 而非 `TEXT`，缺 `persona_id` / `health_score` / `refresh_state` / `last_error` / `refresh_confirmed_at`），现已无任何消费者（原先只被两个遗留脚本手工调用，二者已于 2026-09-13 移除）。两份 schema 对同一张表的唯一性给了相反答案，这本身要请 architect 定夺哪份是准的。

**按现行实现，实际会发生的事**：在 sensenova 下导入一个 key_id 已存在于 volc 的 Key，不会得到两条记录，而是那条 volc 记录的 provider 被改写成 sensenova。它的 `egress_ip`、`health_score`、`persona_id`、封禁状态全部原样留着——**只是换了 provider**。volc 侧从此少一把可用 Key，而导入结果会报告"成功"。

这比你描述的"多一份影子记录"更糟：影子记录是多出来的，这个是原地搬走的。而且它撞上一条已裁决的设计前提——你和 architect 定过不做 Key 迁移端点，理由是"一把 volc 的 Key 在语义上不可能变成 sensenova 的 Key"。现行 upsert 恰好提供了一条隐式的迁移路径，无提示、无二次确认、结果报成功。按本文档 §5 的三条判据（进 Redis key 结构的 / 不可逆的 / 修复路径不自明的），provider 列被静默改写三条全中。

**这反噬到本设计的核心替代路径，不只是一句 hint。** §5.1 / §5.2 / §7.4 / §8.5 都把运维指向"新建 provider + 用「导入 Key」在新 provider 下重新导入"。如果 key_id 全局唯一且 provider 被覆盖，运维照这条路径用同一批 key_id 导入，得到的不是"新 provider 有了自己的 Key"，而是"旧 provider 的 Key 被搬到了新 provider"——旧 provider 当场没有可用 Key，而这个设计的全部前提是旧 provider 继续带着历史用量停用。

**~~裁决（team-lead 第二版）：改约束~~ —— 已撤回，撤回理由见本节开头。** 下面这组证据本身是真的，但它证明的范围比第二版用它证明的窄，保留下来是为了标明边界：

- `:38` `key_id TEXT NOT NULL UNIQUE` —— 实体表：Key 身份 = key_id 单列
- `:120` `PRIMARY KEY (upstream_key_id, provider, quota_day)` —— 归档表：一行 = (Key, 上游, 日)
- `:118-119` 注释原话：`-- provider 必须进主键: 同一 Key 迁到别的上游后，两段历史属于不同的额度体系（水位不同），合并成一行会让 token_ratio 失去意义。`
- `:160-163` 迁移注释：旧主键 `(upstream_key_id, quota_day)` 会把同一 Key 在不同上游的用量挤成一行，`provider 列由最后一次 upsert 决定，历史打分因此读到错误上游的水位`，故 DROP 旧主键改三列

**这不是"内部矛盾"，是两张表各自维度正确。** 后两条说的是归档表按 (key, provider, day) 分维度——这是归档表的正确形态，与实体表 `key_id` 单列唯一并不冲突。把它读成矛盾，是把归档粒度当成了实体身份。`key_daily_history` 承认的是"同一 key_id 的用量历史可以跨越多个 provider"（因为 Key 确实可能被迁过），不是"同一 key_id 可以同时属于多个 provider"。

**这条观察独立于约束方案的存废，它是本节最该留下的东西**（team-lead 认为是本轮最有价值的输出）：`:118-119` 那条注释描述的失效链路（provider 由最后一次 upsert 决定 → 历史打分读到错误上游的水位）与本文档开头记的 quota_kind 事故（ratio 虚高 33 倍 → 刷满额度的 Key 反被排最优先）是同一条链路的两个入口。一个是量纲配错，一个是 provider 列被静默改写，都终结于**调度按错误水位打分**。

**所以 provider 归属值得在 handler 层用 409 硬堵 + 交付一份排查 SQL，而不是写一句提醒。** 判据不是"数据整洁"——如果只是整洁问题，一句 hint 就够了。它是一条**已经出过事的链路的第二个入口**，而第一个入口（quota_kind）我们的处置是物理禁止编辑。同一条链路的两个入口用两种严格度，弱的那个就是实际防线。排查 SQL 给运维用于确认现存数据里有没有已经被搬走过的 Key（历史遗留的静默改写不会有任何记录，只能靠 `key_daily_history` 里同一 key_id 出现过多个 provider 反查），这部分请 be-api 出，属于交付物而非启动期自动执行的迁移——检测是人工的，迁移是自动的，两者不能混在一起。

`deploy/init.sql` 是死文件这点已确认（现已无任何消费者 —— 原先只被两个遗留脚本手工调用，二者已于 2026-09-13 移除；在跑的是 `store.go:26` 的 `//go:embed schema.sql` 与 `ci.yml:167`），team-lead 已要求记进「有意不做」并注明正确处置是删除而非同步维护。这条现在有**双重实证**：一是我核实唯一性时先读到了 `deploy/init.sql` 的 `uq_provider_key` 并因此得出相反结论；二是 team-lead 据同一处分歧作出了第二版裁决，最后要靠 architect 从 `upstreamkeys.go:90` 的 `ON CONFLICT` 反推运行事实才纠正回来。同一份错文件连续误导了两个人，这比"容易混淆"具体得多。

**落地顺序对界面有约束（标的已更新）**：handler 层的 provider 不可变检查到位之前，界面不能照定稿文案推荐「重新导入」，也不能把 hint 里那句"会被拒绝"上线。§5.1 / §5.2 / §7.4 / §8.5 四处替代路径都依赖"重新导入不会伤到旧 provider"这个前提，而现在这个前提由 handler 检查提供，不再由 SQL 约束提供。fe-provider 的文案与 be-api 的 409 检查要同版本上线。

**⑪-d 重新导入必须换 `key_id`——这是硬要求，不是建议。**

架构师与 team-lead 都点出了同一件事，我复核后同意，并认为它是本设计最容易自伤的一处：**我们为了避免隐式迁移而不做 Key 迁移端点，却在文档里手把手把运维引到了唯一那条隐式迁移路径上。**

§5.1 / §5.2 / §7.4 / §8.5 四处都把运维指向「新建 provider + 重新导入 Key」，那是我们为禁改 name / quota_kind 铺的**唯一出路**。运维照做时最自然的动作是打开原来那份导入清单原样重导——`key_id` 一个都没变。于是每一把都撞上"已存在且 provider 不同"，现在会被 409 拦住（不再静默改写，这是改进），但**他会卡在那里**：文档告诉他走这条路，走到一半被拒绝，而拒绝文案只说"不能改上游归属"——他要的正是把 Key 用在新 provider 上，读完不知道下一步该做什么。

所以四处引导文案都已补上新 `key_id` 的要求**与理由**（见 §5.1、§5.2、§7.4、§8.5 的改动）。只说"会被拒绝"不构成可执行的下一步，必须同时给出：新 key_id 要与旧的不同，以及为什么——**旧 Key 要带着已记录的历史用量留在旧 provider 下停用，这是这条替代路径成立的设计前提**，不是一个可以绕过的麻烦。运维理解了这一点才不会去试"那我先删掉旧的再导入"（那会连历史一起删掉，正是要防的）。

导入表单收到 409 + `immutable_field` 时的成品文案（与 §8.5 编辑表单那条区分开，按提交入口分派）：

> 导入失败：`volc_001` 已存在，当前挂在 provider `volc` 下。你的这批 Key 未导入。一把 Key 不能改上游归属——`volc_001` 在 volc 下已记录的用量与健康分属于 volc 这套额度体系，搬到别的上游后两段历史会混在一起，调度会按错误的水位打分。
>
> 若你正在做「新建 provider + 重新导入」：请给这批 Key 起新的 `key_id`（如 `volc_001` → `{新provider名}_001`），旧的那批留在 `volc` 下停用即可，它们要带着历史用量留在原处。
>
> 若你只是想更新 `volc_001` 的密钥或池位：把 `provider` 改回 `volc`（或删掉该字段，单上游时会自动推导），这样就是正常的更新。

第一段说清被拒的是什么与后果；第二段给正在走替代路径的人下一步；第三段给"我其实只想改密钥、`provider` 字段是顺手填错的"那种情况一条出路——这两种意图在提交内容上完全一样，界面分不出来，所以两条都给。

### 仍需 Phase 2 后端提供的接口能力

1. **独立的校验端点**：校验不落库、不生效（§4.4 两步提交依赖它）。校验结果须**逐项返回**（哪一项不合法 + 期望形态），不要合并成一句错误串——§8.3 的逐条错误列表依赖此结构。
2. **编辑提交带 `expected_version`**，冲突返回 409 + `error.code = version_conflict`（§8.5 的并发冲突文案依赖它）。
3. **回滚接口须返回目标版本与当前版本的字段级差异**，供 §7.4 的拒绝文案直接列出差异字段与具体值；同时后端须独立拒绝跨量纲回滚——前端校验不能是唯一防线。
4. **provider 列表接口须带今日用量与请求数**（§3.2 的水位列与「在跑 / 无流量」徽章依赖它），否则「一眼看出哪个 provider 有流量」这个核心需求无法满足。
5. ~~**`PATCH /admin/config/default-provider`**~~ — **已由架构师落地**（hotreload §4.1.2）：`expected_version` 走同一套乐观锁；进版本历史，`provider_config_versions.action` 的 CHECK 已补 `'set_default'`（对应 §7.1 的 `设为默认` 徽章值）；服务端独立校验目标存在且 enabled，违反返回 409 + `invalid_state_transition`；并与 §4.6 停用前置检查闭环。**界面侧待办**：§7.1 版本历史的动作徽章需要认识 `set_default` 这个值，Phase 3 实现时别漏。
6. **列表接口带 `credential_present` 与 `is_default`**（§3.7 / §3.8 的两枚徽章依赖它们）。`credential_present` 按架构师定义只回布尔不回值。
7. ~~**跨量纲回滚的拒绝用可区分的错误码**~~ — **已定为四种 409 靠 `error.code` 分派**（hotreload §4.1.1），不新造状态码。见 §1.8 分派表与本节第 ⑩ 条。
8. **`GET /admin/providers` 响应顶层加 `supported_providers` 数组** — team-lead 最终裁决为列表响应的顶层字段，**不单开端点**（独立端点方案已撤回，理由见 §4.2.1：分两次请求则选项与列表数据可能落在两个快照上，中间夹一次热加载就自相矛盾）。**§4.2.1 的受限选择器与 §8.5 的 `unsupported_provider` 文案都依赖它**，缺了它两处只能退回硬编码，而硬编码的失效方式是静默的：新 adapter 上线后选不到、文案少列一家。值必须由 `confsnap.SupportedProviders()` 导出，禁止在 handler 里另抄字面量。`providers` 为空数组时此字段照样返回。
9. **`upstream_keys` 的 provider 不可变检查（handler 层，409 + `immutable_field`）** — ~~改 `(provider, key_id)` 复合唯一~~ 已撤回（理由见 ⑪-c：与 `ON CONFLICT (key_id)` 不兼容，且 `ADD CONSTRAINT` 遇既存重复行会让 `auto_migrate` 在启动钩子里失败、网关起不来）。改为**不动 SQL**，在导入 handler 里前置检查：`key_id` 已存在且现存 provider 与本次提交不同则拒绝，返回 409 + `error.code = immutable_field`。**这是界面契约而不只是后端内务**：§5.1 / §5.2 / §7.4 / §8.5 四处都把运维指向「新建 provider + 重新导入 Key」，这条路径成立的前提是重新导入不会把旧 provider 的 Key 搬走。检查到位前那四处文案不能上线。另需一份**排查 SQL** 作为交付物（不是启动期迁移），用于确认现存数据里有没有历史遗留的已被搬走的 Key。
11. **重新导入必须使用新 `key_id` 的引导文案** — 与第 9 项同源，见 §5.1 / §5.2 / §7.4 / §8.5 与下方 ⑪-d。四处替代路径是我们为禁改 name / quota_kind 铺的唯一出路，而运维照做时最自然的动作是拿原来那批 Key 原样重导，每一把都会撞上 409。所以引导文案必须同时给出新 key_id 的要求与理由，只说"会被拒绝"不构成可执行的下一步。
10. **`deploy/init.sql` 记入「有意不做」并删除** — 它与在跑的 `internal/store/schema.sql` 对同一张表的唯一性给相反答案（`uq_provider_key` 复合 vs `key_id` 单列），列结构也已对不上。正确处置是删除而非同步维护：留着它，下一个查唯一性的人可能先读到错的那份——本轮我就是这样先得出了相反结论。

