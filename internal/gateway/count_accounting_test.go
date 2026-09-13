package gateway

import (
	"strings"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/quota"
)

// countUsedTotal 汇总**所有 Key** 的按次实扣量。
//
// 必须汇总而不能只看单个 Key: 「响应体超限」属于可重试错误，重试会经 exclude
// 换到另一个 Key，实扣量因此散落在多个 Key 上。
// 只看 volc_001 会把「3 次重试各扣 1」读成「只扣了 1」，从而错误地把
// 「流水多记」当成缺陷。
func countUsedTotal(t *testing.T, f *fakeQuota) int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	suffix := "|" + string(quota.KindCount)
	for k, v := range f.used {
		if strings.HasSuffix(k, suffix) {
			total += v
		}
	}
	return total
}

// 按次计费的「已实扣但对外非 200」路径，流水必须记下实扣量。
//
// 这条路径是: 上游已经返回了 200 响应头，但响应体超过 server.max_body_bytes，
// 于是 attempt() 显式按预扣量 Commit —— 上游确实消耗了额度，不能白送。
// 而对外的状态码是 502。
//
// 旧实现把落库判据写成 `status == http.StatusOK`，于是这条路径上
// Redis 实扣 1、流水 count_units 记 0，偏差方向是少记。
// 与真实环境暴露过的「按次流水恒为 0」属同一类缺陷，只是换了条路径。
func TestCount_响应超限时流水仍记实扣量(t *testing.T) {
	// 请求体要小于 512，响应体要大于 512 —— 否则走不到「响应体超限」分支
	env := newTestEnv(t, func(c *config.Config) { c.Server.MaxBodyBytes = 512 })

	env.upstream.setScript(stubResponse{
		Status: 200,
		Body: `{"model":"seedream-3.0","data":[{"url":"` +
			strings.Repeat("a", 800) + `"}]}`,
	})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"model":"seedream-3.0","messages":[{"role":"user","content":"hi"}]}`)
	code := resp.StatusCode
	resp.Body.Close()

	// 前提守卫: 必须真的走到了这条「已实扣、非 200」的路径。
	// 少了它，若上游行为变化导致请求变成 200，用例会静默变成另一个场景的重复。
	if code != 502 {
		t.Fatalf("对外状态码 = %d, 期望 502（本次用例前提是走响应体超限分支）", code)
	}

	used := countUsedTotal(t, env.quota)
	if used == 0 {
		t.Fatalf("count 配额未扣减 —— 用例前提不成立，无法验证账面一致性")
	}

	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(recs))
	}
	if got := recs[0].CountUnits; int64(got) != used {
		t.Errorf("流水 count_units = %d, Redis 实扣 = %d —— 账面与配额不一致（偏差方向=少记）",
			got, used)
	}
}

// 成功路径的实扣量同样要与流水一致（这是既有行为的守卫，防止修复引入回归）。
func TestCount_成功路径流水与实扣一致(t *testing.T) {
	env := newTestEnv(t)
	env.upstream.setScript(stubResponse{
		Status: 200,
		Body:   `{"model":"seedream-3.0","data":[{"url":"https://x/1.png"},{"url":"https://x/2.png"}]}`,
	})

	resp := env.post(t, "/v1/images/generations", "user-key-ok",
		`{"model":"seedream-3.0","prompt":"一只猫","n":2}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()

	used := countUsedTotal(t, env.quota)
	recs := env.store.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("流水条数 = %d, 期望 1", len(recs))
	}
	if got := recs[0].CountUnits; int64(got) != used {
		t.Errorf("流水 count_units = %d, Redis 实扣 = %d", got, used)
	}
}

// 失败且未 Commit（走 Release）的请求，流水不得凭空记一次用量。
//
// 反向守卫: 上一条用例把判据从「状态码」换成「是否 Commit」，容易顺手写成
// 「只要不是 200 就记预扣量」—— 那会让所有失败请求都记一笔不存在的用量。
func TestCount_失败未Commit时流水不记用量(t *testing.T) {
	env := newTestEnv(t)
	// 上游 500: attempt 走错误分支，不设置 commitActual，租约以 Release 结束
	env.upstream.setScript(stubResponse{Status: 500, Body: `{"error":"boom"}`})

	resp := env.post(t, "/v1/chat/completions", "user-key-ok",
		`{"model":"seedream-3.0","messages":[{"role":"user","content":"hi"}]}`)
	resp.Body.Close()

	if used := env.quota.usedFor("volc_001", quota.KindCount); used != 0 {
		t.Errorf("失败请求不应扣减配额，实际 = %d", used)
	}
	recs := env.store.usageRecords()
	if len(recs) == 0 {
		t.Fatalf("失败请求也应有流水（用于排障定位问题 Key）")
	}
	for i, r := range recs {
		if r.CountUnits != 0 {
			t.Errorf("第 %d 条流水 count_units = %d, 期望 0（未 Commit 就不该记用量）",
				i, r.CountUnits)
		}
	}
	env.quota.assertClean(t)
}
