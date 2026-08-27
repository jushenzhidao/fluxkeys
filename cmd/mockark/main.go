// Command mockark 启动可控的火山 Ark Mock 上游。
//
// 用途: 离线跑通网关全链路，并制造真实环境难以复现的异常（额度耗尽、429、
// 流式中断），从而把「安心模式熔断」「换 Key 重试」「租约回收」变成
// 可重复的确定性测试。
//
// 示例:
//
//	mockark -addr :18080 -token-limit 10000 -stream-delay 20ms
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

	"github.com/fluxkeys/fluxkeys/internal/mockark"
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
		logger.Info("mockark 启动",
			"addr", *addr, "token_limit", *tokenLimit, "count_limit", *countLimit,
			"stream_delay", streamDelay.String(), "chunks", *chunks)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("mockark 监听失败", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("收到退出信号，关闭 mockark")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("mockark 关闭超时", "err", err)
	}
}
