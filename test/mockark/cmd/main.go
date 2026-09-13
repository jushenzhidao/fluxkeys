// Command mockrunner 把 test/mockark 夹具作为一个独立进程跑起来。
//
// 存在理由: 假上游的宿主已经并入 test/mockark（不再是 internal/mockark +
// cmd/mockark 那套「可发布的服务」形态）。但有两个场景需要它作为**独立进程**
// 存在，而不是被测试在进程内启动:
//
//  1. scripts/local-e2e-check.sh —— 真实网关二进制 + 真实 Redis/PostgreSQL +
//     真实上游地址的端到端验证。这里的被测对象是「独立进程的网关」，上游必须
//     也是独立进程，否则验不到网络层（连接、超时、流式 flush）。
//  2. 排查问题时手工起一个假上游观察流量。
//
// 它**不是产品的一部分**: 不被任何 Dockerfile target 构建、不发 GHCR 镜像、
// 不出现在任何 compose 里 —— 那些都已随 mockark 一起移除。CI 也不跑它
// （集成测试在进程内启动夹具，更快且无需端口）。
//
// 示例:
//
//	go run ./test/mockark/cmd -addr :18080 -token-limit 10000 -stream-delay 20ms
//	curl -s localhost:18080/_mock/stats | jq
//	curl -sX POST localhost:18080/_mock/inject -d '{"kind":"429_quota","key_id":"volc_001","remaining":3}'
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fluxkeys/fluxkeys/test/mockark"
)

func main() {
	var (
		addr        = flag.String("addr", ":18080", "监听地址")
		tokenLimit  = flag.Int64("token-limit", 0, "每 Key 默认 Token 额度（0 = 不限）")
		countLimit  = flag.Int64("count-limit", 0, "每 Key 默认次数额度（0 = 不限）")
		streamDelay = flag.Duration("stream-delay", 0, "流式 chunk 间隔")
		chunks      = flag.Int("chunks", 5, "每次流式响应的内容 chunk 数")
		models      = flag.String("models", "", "模型列表，逗号分隔（留空用内置默认）")
		seed        = flag.Int64("seed", 42, "随机种子，固定后 completion_tokens 可复现")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opt := mockark.Options{
		DefaultTokenLimit: *tokenLimit,
		DefaultCountLimit: *countLimit,
		StreamChunkDelay:  *streamDelay,
		StreamChunks:      *chunks,
		Seed:              *seed,
	}
	if *models != "" {
		for _, m := range strings.Split(*models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				opt.Models = append(opt.Models, m)
			}
		}
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: mockark.NewServer(opt),
		// 流式响应可能长时间持续，不设写超时
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("假上游（test/mockark）启动",
			"addr", *addr, "token_limit", *tokenLimit, "count_limit", *countLimit,
			"stream_delay", streamDelay.String(), "chunks", *chunks)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("假上游监听失败", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("收到退出信号，关闭假上游")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("假上游关闭超时", "err", err)
	}
}
