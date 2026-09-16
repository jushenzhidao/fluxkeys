# FluxKeys Go 侧镜像 —— 多阶段构建，产出 gateway 一个 target。
#
#   docker build --target gateway -t fluxkeys/gateway:dev .
#
# 注：假上游（原 mockark）已不再作为可发布服务存在，改为 test/mockark 下的
# 测试夹具，由集成测试在进程内启动，因此镜像里不再有对应的 target。
#
# 带版本信息的本地构建（注意 git rev-parse 在无提交的仓库会失败，用 || echo 兜底）：
#   docker build --target gateway -t fluxkeys/gateway:dev \
#     --build-arg VERSION=dev \
#     --build-arg GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
#     --build-arg BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" .
#
# 设计取舍说明：
#   1. runtime 选 alpine 而非 distroless。distroless 里没有 shell 也没有 wget/curl，
#      无法实现镜像内置 HEALTHCHECK（任务明确要求带 HEALTHCHECK）。alpine 3.20
#      带 busybox wget，加上静态二进制后整体仍在 ~20MB 量级，代价可接受。
#   2. CGO_ENABLED=0 + netgo：产出纯静态二进制，不依赖 glibc/musl 的 NSS，
#      DNS 解析走 Go 自带 resolver，避免容器内 /etc/nsswitch.conf 差异导致的解析问题。
#   3. 版本信息通过 -ldflags -X 注入。若 main 包里没有对应变量，Go 链接器会静默
#      忽略，不会导致构建失败，因此这里可以先行注入、等 cmd/ 侧补上变量即生效。
#   4. 调优档配置（configs/*.yml）在 gateway stage 里 COPY 到 /etc/fluxkeys/，
#      随镜像分发而非挂宿主机目录。代价是改配置必须重建镜像 —— 换来的是配置与
#      二进制版本同步演进、容器零宿主机目录依赖（与 read_only 基线一致）。

# ============================================================================
# Stage 1: builder
# ============================================================================
FROM golang:1.23.4-alpine3.20 AS builder

# git 用于构建期读取版本信息；ca-certificates 供 go mod download 走 HTTPS。
# 这里不钉 apk 包的精确版本号：alpine 仓库会移除旧补丁版本，钉死后过几周
# 就会 "unable to select packages" 构建失败。可复现性由基础镜像 tag 保证
# （golang:1.23.4-alpine3.20 已锁定发行版快照），这是更稳的锚点。
RUN apk add --no-cache git ca-certificates tzdata

# 国内网络：走 goproxy.cn 镜像，direct 兜底。
# GOSUMDB 保持默认 sum.golang.org（goproxy.cn 会代理校验和数据库），
# 不设 GONOSUMDB/GOFLAGS=-mod=mod，保证依赖完整性校验不被绕过。
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=sum.golang.org \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# 先只拷 go.mod/go.sum 下载依赖 —— 这一层只在依赖变更时失效，
# 后续改业务代码可以直接命中缓存。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 再拷源码
COPY . .

# 由 CI 通过 --build-arg 注入；本地构建时用 dev 占位。
# TARGETARCH 由 buildx 自动提供，多架构构建时用它交叉编译。
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG TARGETARCH

# -s -w 去掉符号表与 DWARF 调试信息，二进制可缩小 ~30%。
# -trimpath 去掉本机绝对路径，让构建结果可复现（同输入同输出）。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    export GOARCH="${TARGETARCH:-amd64}"; \
    LDFLAGS="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${GIT_COMMIT} \
      -X main.buildTime=${BUILD_TIME}"; \
    go build -trimpath -tags netgo,osusergo \
        -ldflags "${LDFLAGS}" -o /out/gateway ./cmd/gateway

# ============================================================================
# Stage 2: 公共 runtime 基座
# ============================================================================
FROM alpine:3.20.3 AS runtime-base

# ca-certificates: 访问火山 Ark HTTPS 必需
# tzdata:          配额日/刷新窗口逻辑依赖 Asia/Shanghai 时区，缺了会按 UTC 算，
#                  12:00 刷新窗口会整体偏移 8 小时 —— 这是会直接导致超刷的坑
# wget:            busybox 自带，用于 HEALTHCHECK
RUN apk add --no-cache ca-certificates tzdata

ENV TZ=Asia/Shanghai

# 非 root 运行。uid/gid 固定为 65532（沿用 distroless nonroot 约定），
# 便于将来切换 runtime 基础镜像时挂载卷的属主不变。
RUN addgroup -g 65532 -S nonroot \
 && adduser  -u 65532 -S nonroot -G nonroot -h /home/nonroot

# ============================================================================
# Stage 3: gateway
# ============================================================================
FROM runtime-base AS gateway

COPY --from=builder /out/gateway /usr/local/bin/gateway

# 调优档配置随镜像分发，**不挂宿主机目录**。
#
# 为什么放在 configs/ 而不是 deploy/: 构建上下文按 .dockerignore 排除了
# deploy/（那是「Go 镜像不需要文档与监控配置」的有意裁剪）。把档位文件挪回
# deploy/ 会让下面这行直接构建失败 —— 这是刻意的：失败可见，好过镜像里悄悄
# 少一个档位文件、运行时才发现。
#
# 内置的好处: 镜像即「二进制 + 配置」的完整交付物，版本与配置一起演进；
# 容器不需要任何宿主机目录挂载，与 read_only / cap_drop: ALL 的安全基线一致。
COPY --chown=65532:65532 configs/ /etc/fluxkeys/

# 不带参数直接 docker run 时用生产档。用 CMD 而非 ENTRYPOINT 的固定参数，
# 才能让 compose 的 command（切档位）与手工 `-config X` 都覆盖得掉。
CMD ["-config", "/etc/fluxkeys/config.prod.yml"]

USER 65532:65532
WORKDIR /home/nonroot

# 8080 业务端口，9090 指标端口（对应 FLUXKEYS_ADDR / FLUXKEYS_METRICS_ADDR 默认值）
EXPOSE 8080 9090

# 探活打 /healthz（不做依赖检查，只判进程是否还在服务）。
# 就绪探针 /readyz 会检查 Redis/Postgres，那属于 compose/编排层的职责，
# 不放进镜像 HEALTHCHECK —— 否则依赖抖动会让容器被误判为 unhealthy 而重启。
# start-period 给 40s：首次启动要跑 migration + 全量 lease 扫描（P0-2 崩溃恢复）。
HEALTHCHECK --interval=15s --timeout=3s --start-period=40s --retries=3 \
    CMD wget -q -O /dev/null --tries=1 --timeout=2 http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/gateway"]
