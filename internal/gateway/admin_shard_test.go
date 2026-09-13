package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// POST /admin/keys/shard 的端点测试。
//
// 契约: 批量指派 Key 的机器归属，affected < requested 时如实透出
// （漏指派的 Key 会静默地不被任何实例装载，这个差额是唯一的信号）。

func shardPost(t *testing.T, env *testEnv, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys/shard",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp
}

func TestAdminShard_批量指派并回报命中数(t *testing.T) {
	env := newTestEnv(t)
	// 预置两个 Key，第三个故意不存在
	env.store.upstreamKeys = map[string]NewUpstreamKey{
		"volc_001": {KeyID: "volc_001"},
		"volc_002": {KeyID: "volc_002"},
	}

	resp := shardPost(t, env,
		`{"shard":"node-a","key_ids":["volc_001","volc_002","volc_missing"],"reason":"扩容机器A"}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}

	// affected 必须是真实命中数（2），requested 是请求数（3）——
	// 差额是「有 key_id 没匹配上」的唯一信号，不能被吞成全量成功。
	if !strings.Contains(body, `"affected":2`) {
		t.Errorf("affected 应为 2: %s", body)
	}
	if !strings.Contains(body, `"requested":3`) {
		t.Errorf("requested 应为 3: %s", body)
	}

	// 存储层收到的就是去重后的原始清单
	env.store.mu.Lock()
	calls := env.store.shardCalls
	env.store.mu.Unlock()
	if len(calls) != 1 || calls[0].Shard != "node-a" || len(calls[0].KeyIDs) != 3 {
		t.Fatalf("AssignShard 调用不符: %+v", calls)
	}

	// 审计必须落一条 assign_key_shard
	audits := env.store.auditRecords()
	found := false
	for _, a := range audits {
		if a.Action == "assign_key_shard" {
			found = true
		}
	}
	if !found {
		t.Error("分片指派未写审计日志")
	}
}

func TestAdminShard_重复ID去重后指派(t *testing.T) {
	env := newTestEnv(t)
	env.store.upstreamKeys = map[string]NewUpstreamKey{"volc_001": {KeyID: "volc_001"}}

	resp := shardPost(t, env,
		`{"shard":"node-a","key_ids":["volc_001","volc_001","volc_001"]}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}
	// 去重后只剩 1 个
	if !strings.Contains(body, `"requested":1`) {
		t.Errorf("重复 ID 应去重, requested 应为 1: %s", body)
	}
}

func TestAdminShard_参数校验(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name string
		body string
	}{
		{"空key_ids", `{"shard":"node-a","key_ids":[]}`},
		{"含空白项", `{"shard":"node-a","key_ids":["volc_001",""]}`},
		{"未知字段", `{"shard":"node-a","key_ids":["k"],"oops":1}`},
		{"非法JSON", `{shard}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := shardPost(t, env, tc.body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("状态码 = %d, 期望 400", resp.StatusCode)
			}
		})
	}

	// 无管理密钥必须 401 —— 归属指派能把账号从机器上摘掉，绝不能裸奔
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/admin/keys/shard",
		strings.NewReader(`{"shard":"x","key_ids":["k"]}`))
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未鉴权状态码 = %d, 期望 401", resp.StatusCode)
	}
}

func TestAdminShard_允许空shard解除归属(t *testing.T) {
	// shard 传空串是合法操作: 语义是「解除归属」，用于机器下线前摘流。
	// 若把它当参数缺失拒绝，下线流程就没有 API 可走。
	env := newTestEnv(t)
	env.store.upstreamKeys = map[string]NewUpstreamKey{"volc_001": {KeyID: "volc_001"}}

	resp := shardPost(t, env, `{"shard":"","key_ids":["volc_001"],"reason":"机器A下线摘流"}`)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
	}
	env.store.mu.Lock()
	calls := env.store.shardCalls
	env.store.mu.Unlock()
	if len(calls) != 1 || calls[0].Shard != "" {
		t.Fatalf("解除归属未透传空 shard: %+v", calls)
	}
}
