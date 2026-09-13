// Package httpcore 承载 HTTP 层的最小公共件：统一错误响应、请求 ID 的
// context 传递与生成。
//
// 存在理由: 网关主体与它的管理面子包都要写同一种错误响应、读同一个请求 ID。
// 把这几个符号留在任一包里，另一包就得反向依赖它（管理面依赖网关主体在方向
// 上是成立的，但那会让「管理面只是个消费者」这层边界消失，也让网关主体无法
// 在不引入管理代码的前提下被单独理解）。
//
// 本包刻意不依赖任何 internal 包，也不持有状态 —— 只有函数与一个错误信封类型。
package httpcore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorEnvelope 是 OpenAI 风格的错误响应。
//
// 字段层级（error.message / error.type / error.code）是对外契约的一部分，
// 客户端 SDK 按此结构解析，不可改动。
type ErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// ErrorTypeFor 把 HTTP 状态码映射为 OpenAI 风格的错误类型。
//
// 未知状态码一律归入 invalid_request_error: 该字段用于客户端分支，
// 归错类只会让客户端选错重试策略，而多造一个类型会让未知类型在客户端
// 落到 default 分支 —— 与归入 invalid_request_error 的结果相同，但多一处
// 需要维护的枚举。
func ErrorTypeFor(status int) string {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// WriteError 写出统一格式的错误响应。
//
// log 为 nil 时不记录编码失败（测试便利），生产路径应传入 logger。
func WriteError(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, code, msg string) {
	var e ErrorEnvelope
	e.Error.Message = msg
	e.Error.Code = code
	e.Error.Type = ErrorTypeFor(status)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(e); err != nil && log != nil {
		log.WarnContext(r.Context(), "写出错误响应失败", "err", err)
	}
}

// WriteJSON 写出 JSON 响应。
//
// 编码失败无补救手段（响应头已写出），故刻意忽略 —— 调用方不应依赖
// 本函数的返回值判断成功与否。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ctxKey 是本包 context 键的私有类型。
//
// 私有类型可避免与其他包的键冲突；调用方无法构造同类型的键，
// 也就无法伪造请求 ID。
type ctxKey int

const ctxKeyRequestID ctxKey = iota

// WithRequestID 把请求 ID 放进 context，并回写到响应头。
//
// 复用上游传入的 X-Request-Id 以保证跨系统链路可关联；缺失时生成一个。
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext 取出请求 ID。缺失时返回空串。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// NewRequestID 生成一个 16 字节的随机 ID，编码为 32 字符十六进制串。
//
// 旧设计用 8 字节纳秒时间戳 + 4 字节随机数，在负载远低于生日悖论阈值时
// 也会因「时间戳包含内部序列号字段，恰好撞、随机数也撞」而冲突（已实测
// 观察到）。改用 crypto/rand.Read —— 它取默认 Reader（unix 上是
// /dev/urandom，密码学级熵源），且无需 import 外部 UUID 库。
//
// 16 字节即 128 比特，生日悖论 1% 冲突概率的阈值是 2^64（约 1800 万亿次）；
// 单进程 5000 QPS 全年也才 1500 亿次，实际冲突率 < 1e-10。
func NewRequestID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "req_" + hex.EncodeToString(b[:])
}
