package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/adapter"
)

// SSE 流式转发。
//
// 三个必须满足的约束:
//
//  1. 真流式。每读到一个 chunk 立即转换并 Flush，绝不缓冲完整响应。缓冲会
//     让首字节延迟等于整个生成耗时（可达数十秒），是不可接受的体验回退。
//
//  2. 解析末尾 chunk 的 usage。流式的真实用量只出现在最后一个 chunk 里，
//     漏掉它就只能按预扣量 Commit，误差会随流量累积成系统性偏差。
//
//  3. 客户端断连必须被感知并向上传播。断连时上游连接需关闭、租约需结束。
//     单纯依赖租约 TTL 回收会让被占用的额度悬置最长 600 秒，在高并发下
//     足以把整个 Key 池的可用额度耗光。

// maxSSELineBytes 限制单行 SSE 数据的长度。
//
// 正常 chunk 在 KB 量级，但工具调用的参数或推理内容可能很长，
// 故给到 1MB —— 既能容纳异常大的 chunk，又不至于被恶意上游打爆内存。
const maxSSELineBytes = 1 << 20

// streamResponse 将上游 SSE 流转发给客户端，并返回解析到的用量。
//
// 返回非 nil 错误表示流未正常结束（客户端断开或上游中断）。此时 usage
// 可能为空，调用方须按预扣量保守 Commit。
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request,
	upResp *http.Response, plan *requestPlan, ad adapter.Adapter, start time.Time) (adapter.Usage, *adapter.UpstreamError) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		// 理论上不会发生（net/http 的 ResponseWriter 都实现了 Flusher），
		// 但绝不能默默降级为缓冲模式 —— 那会让流式请求的延迟劣化数十倍。
		return adapter.Usage{}, &adapter.UpstreamError{
			Class:        adapter.ErrClassServer,
			ClientStatus: http.StatusInternalServerError,
			Code:         "internal_error",
			Message:      "ResponseWriter 不支持流式刷出",
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 关闭中间层缓冲。Nginx 默认会缓冲 proxy 响应，那会抵消所有流式努力。
	w.Header().Set("X-Accel-Buffering", "no")
	if v := upResp.Header.Get("X-Request-Id"); v != "" {
		w.Header().Set("X-Request-Id", v)
	} else {
		w.Header().Set("X-Request-Id", plan.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var (
		usage      adapter.Usage
		gotUsage   bool
		firstChunk = true
		ctx        = r.Context()
	)

	reader := bufio.NewReaderSize(upResp.Body, 16<<10)

	for {
		// 每轮循环开头检查断连。上游可能持续推送而客户端早已离开，
		// 不检查会让这个 goroutine 一直跑到上游结束。
		select {
		case <-ctx.Done():
			return usage, adapter.NewNetworkError("客户端断开连接")
		default:
		}

		line, err := readSSELine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 上游正常结束。缺 usage 说明上游未按 include_usage 返回，
				// 由调用方按预扣量兜底。
				if !gotUsage {
					s.log.WarnContext(ctx, "流式响应未包含 usage，将按预扣量修正配额",
						"request_id", plan.RequestID, "model", plan.Model)
				}
				return usage, nil
			}
			if ctx.Err() != nil {
				return usage, adapter.NewNetworkError("客户端断开连接")
			}
			return usage, adapter.NewNetworkError("读取上游流失败: " + err.Error())
		}

		// 空行是 SSE 的事件分隔符，原样转发以保持协议正确
		if len(bytes.TrimSpace(line)) == 0 {
			if _, werr := w.Write([]byte("\n")); werr != nil {
				return usage, adapter.NewNetworkError("客户端写入失败: " + werr.Error())
			}
			flusher.Flush()
			continue
		}

		data, isData := bytes.CutPrefix(line, []byte("data:"))
		if !isData {
			// 注释行（": ping"）与其他字段（event:、id:）原样转发
			if _, werr := w.Write(append(line, '\n')); werr != nil {
				return usage, adapter.NewNetworkError("客户端写入失败: " + werr.Error())
			}
			flusher.Flush()
			continue
		}
		data = bytes.TrimSpace(data)

		if firstChunk {
			s.metrics.ObserveTTFB(plan.Model, time.Since(start))
			firstChunk = false
		}

		// 结束哨兵
		if bytes.Equal(data, []byte("[DONE]")) {
			if _, werr := fmt.Fprint(w, "data: [DONE]\n\n"); werr != nil {
				return usage, adapter.NewNetworkError("客户端写入失败: " + werr.Error())
			}
			flusher.Flush()
			if !gotUsage {
				s.log.WarnContext(ctx, "流式响应未包含 usage，将按预扣量修正配额",
					"request_id", plan.RequestID, "model", plan.Model)
			}
			return usage, nil
		}

		// 解析用量。usage 只出现在末尾 chunk，但不能假设它一定是最后一条 ——
		// 部分实现会在 usage chunk 之后再发 [DONE]，也有实现把 usage 附在
		// finish_reason chunk 上。故每个 chunk 都尝试解析，取到即记录。
		if u, ok := ad.ParseUsage(plan.Endpoint, data); ok && !u.Empty() {
			usage = u
			gotUsage = true
		}

		out, terr := ad.TransformStreamChunk(data)
		if terr != nil {
			// 转换失败时透传原始内容: 丢一个 chunk 会让客户端收到不完整的
			// 回答，比透传一个格式略有偏差的 chunk 严重得多。
			out = data
		}

		if _, werr := fmt.Fprintf(w, "data: %s\n\n", out); werr != nil {
			return usage, adapter.NewNetworkError("客户端写入失败: " + werr.Error())
		}
		flusher.Flush()
	}
}

// readSSELine 读取一行（不含行尾换行符），并限制单行长度。
//
// 不用 bufio.Scanner 是因为它在超过 buffer 上限时返回的错误难以与 EOF
// 区分，而这里必须精确区分「上游正常结束」和「行太长」两种情况。
func readSSELine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				// 最后一行没有换行符结尾
				return buf, nil
			}
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxSSELineBytes {
			return nil, fmt.Errorf("SSE 单行超过 %d 字节上限", maxSSELineBytes)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}
