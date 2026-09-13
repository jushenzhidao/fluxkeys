# 发版模型

本文说明本仓的发布流程：什么时候发、版本号怎么来、以及出问题时怎么补。

## 三个入口

`.github/workflows/release.yml` 是唯一的发布入口，按触发方式分三种模式：

| 入口 | 触发 | 模式 | 版本号来源 | 用途 |
|---|---|---|---|---|
| ① 自动 | push 到 `main` | `auto` | 上一个版本 tag 自增 patch | 日常路径，零人工 |
| ② 人工 tag | push `v*` tag | `tag` | 就是该 tag，不递增 | minor / major，以及对外契约不向后兼容的变更 |
| ③ 补发 | `workflow_dispatch` + `tag` 输入 | `tag` | 输入的 tag，**必须已存在** | 构建失败重跑、改了 workflow 后补跑 |

三个入口最终都汇到同一个 `resolve` job 解析出唯一的版本号，其余 job 一律依赖它的输出。
**不要**在别处再推导一次版本号 —— push 到 main 时根本没有 tag 可读，分散推导必然漂移。

### 为什么自增逻辑不能拆成独立 workflow

`GITHUB_TOKEN` 推送的 tag **不会触发**任何其他 workflow。所以「先由 A workflow 打 tag、
再由 B workflow 监听 tag 触发发布」这种拆法在本仓不成立，自增必须与发布在同一次 run 内完成。

反过来说，`auto` 模式自己 `git push` 的 tag 也不会把自己再触发一遍，不会自激。

## 版本号规则

- 版本号只存于 git tag。**不写回任何 manifest** —— 写回要向主干提交，会自触发本 workflow，
  且 `GITHUB_TOKEN` 推送的提交不触发任何 workflow，会静默破坏依赖该提交的其他流水线。
- **git tag 带 `v` 前缀，镜像 tag 不带**。`v0.1.1` → `ghcr.io/<owner>/fluxkeys:0.1.1`。
  核验时写成 `manifests/v0.1.1` 会拿到 404 的 error JSON。
- 镜像每次发布推三个 tag：`<版本>`、`<major>.<minor>`、`latest`。
  预发布（含 `-`，如 `v1.0.0-rc1`）**不**更新 `latest`。
- `auto` 模式的基线取「最新的稳定版本 tag」，排序用 `--sort=-v:refname`。
  用 `git describe --tags` 会取「最近可达的 tag」，在有合并历史时挑错版本
  （`v1.9.0` 会被字典序判成早于 `v1.10.0`）。
- 一个稳定 tag 都没有时（例如只发过 `v1.0.0-rc1`），基线回落到最新的预发布 tag：
  此时若仍从 `v0.0.0` 起算会得到 `v0.0.1`，**低于已发布的 rc**，版本号倒退。
- 零 tag 的冷启动从 `v0.0.0` 起算，首个自动版本是 `v0.0.1`。

## 逃生阀

commit message 里含 `[skip release]` 即整条流水线跳过（`resolve` job 的 `if` 条件，
其余 job 全靠 `needs` 传递跳过）。纯文档、纯 CI 改动走这条路，避免产出无意义的版本。

两个必须知道的点：

1. **它匹配的是整条 message，包括标题。** 所以「给逃生阀写说明」的那次提交自己也会被拦住 ——
   写文档描述这个标记时，要么别写全字面量，要么接受那次不发版。
2. **跳过之后要补跑，只能用 `workflow_dispatch`。** 该路径下 `github.event.head_commit`
   为 null（表达式已用 `|| ''` 兜底，不会误判成"含标记"）。

## 补发（workflow_dispatch）

- 输入的 tag **必须已存在**，不存在会直接失败，不会替你造新版本 ——
  避免"以为在补发、实际在发新版本"。
- 补发是**真实发布**：会覆盖该 tag 对应的镜像并更新 Release。它**不是演练**。
- checkout 显式指定 `ref: ${{ inputs.tag || github.ref }}`。少了这一行，默认检出的是
  默认分支，会用主干代码去构建旧 tag 的镜像，并把 `:<version>` 与 `:latest` 覆盖过去 ——
  镜像与 tag 的承诺就脱钩了。
- 同一个 tag 重跑是幂等的：镜像同 tag 覆盖推，Release 走更新而非报错。

## 并发

单一全局并发组 `release`，且 `cancel-in-progress: false`：

- 两次快速 push main 会被串行化，后一次能看到前一次刚打的 tag，版本号不会撞车；
- 人工 tag 与自动 patch 不会并行抢版本线（否则可能出现 `v0.1.1` 晚于 `v0.2.0` 发布，
  导致 Release 列表的 "Latest" 指错）。

代价是一次发布（多架构，约十几分钟）会挡住后一次。发版频率下可以接受。

## 权限

仓库的 Actions 默认权限是 **read**，因此本 workflow 在 job 内显式声明所需的写权限：

- workflow 级：`packages: write`（推 ghcr.io）、`id-token: write`（备用，供将来接 sigstore 签名）
- `release` job：`contents: write`（打 tag、建 Release）

这一点有实证：`v0.1.0` 的 Release 就是本工作流创建的。若将来把仓库默认权限进一步收紧到
组织级策略，需重新核验 job 级声明是否仍被授予。

## 发布顺序

`resolve` → `verify`（`go vet` + `go test -race`）→ `build-push`（多架构镜像）→ `release`。

自动模式下 **tag 在镜像推送之后才打**：镜像先推、tag 后打，构建失败就不会留下
「有 tag、没镜像」的半成品 —— tag 一旦存在就是对外承诺。

## 本地验证（不推任何东西）

改完 workflow 后，不要靠「推一版试试」来验证。版本解析逻辑可以从 YAML 里抽出来直接跑：

```bash
# 抽出 resolve 步骤的 shell，替换掉 ${{ inputs.tag }} 与 ${{ github.ref }} 后：
#   - 模拟 push main：  inputs.tag=''            github.ref=refs/heads/main
#   - 模拟人工 tag：    inputs.tag=''            github.ref=refs/tags/v0.1.0
#   - 模拟补发：        inputs.tag='v0.1.0'      github.ref=refs/heads/main
# GITHUB_OUTPUT 指向一个临时文件，读回 mode / version / image_version / is_prerelease 断言
```

关键覆盖点：冷启动（零 tag）、只有预发布 tag、`v0.9.0` 与 `v0.10.0` 的排序陷阱、
补发不存在的 tag、非法输入（如 `latest`）。这些都在本地跑得出来，不必消耗一次真实发布。

## 有意不做

- **不提供 `dry_run` 演练开关。** 演练需求由上面的本地干跑覆盖；真要在 CI 里演练，
  得同时跳过镜像推送与 manifest 校验，多出来的分支比它挡掉的风险更容易出错。
- **不把版本号写回 manifest**，理由见上文。
- **不做「移动 `latest` git tag」**：`latest` 只是镜像 tag，仓内不存在同名 git tag 是正常的，
  核验时别去找 `refs/tags/latest`。
