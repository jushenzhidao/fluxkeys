package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// 本文件用真实 Postgres 验证 PatchVolcKeyState。
//
// 为什么不能只靠 gateway 侧的 fake: fake 是按预期语义手写的，若我对
// UPDATE ... FROM 同表的 pre-image 语义理解有误，fake 和实现会一起错，
// 测试全绿而线上返回错误的旧值。这里断言的是数据库的真实行为。

func strPtr(s string) *string { return &s }

// seedPatchKey 预置一个待更新的 Key。
func seedPatchKey(t *testing.T, s *Store, ctx context.Context, keyID, status, pool, persona string) {
	t.Helper()
	if _, err := s.UpsertVolcKey(ctx, &VolcKey{
		KeyID: keyID, Secret: "sk-" + keyID,
		Status: status, Pool: pool, PersonaID: persona,
	}); err != nil {
		t.Fatalf("预置 Key %s: %v", keyID, err)
	}
}

func TestPatchVolcKeyState_回显新旧值且只改指定列(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "p_day")

	res, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Status: strPtr("banned"),
	})
	if err != nil {
		t.Fatalf("PatchVolcKeyState: %v", err)
	}

	// pre-image 必须是本语句执行前的值
	if res.PrevStatus != "active" {
		t.Errorf("PrevStatus = %q, 期望 active", res.PrevStatus)
	}
	if res.NewStatus != "banned" {
		t.Errorf("NewStatus = %q, 期望 banned", res.NewStatus)
	}
	// nil 字段两侧都应是原值
	if res.PrevPool != "hot" || res.NewPool != "hot" {
		t.Errorf("pool 被误改: %q → %q", res.PrevPool, res.NewPool)
	}
	if res.PrevPersonaID != "p_day" || res.NewPersonaID != "p_day" {
		t.Errorf("persona_id 被误改: %q → %q", res.PrevPersonaID, res.NewPersonaID)
	}

	// 落库核对，防止只有 RETURNING 对而写入错
	k, err := s.GetVolcKey(ctx, "volc_001")
	if err != nil {
		t.Fatalf("GetVolcKey: %v", err)
	}
	if k.Status != "banned" || k.Pool != "hot" || k.PersonaID != "p_day" {
		t.Errorf("落库值不符: status=%q pool=%q persona=%q", k.Status, k.Pool, k.PersonaID)
	}
}

func TestPatchVolcKeyState_nil字段绝不参与更新(t *testing.T) {
	// 这条对应 EXCLUDED 被 VALUES 兜底污染那类缺陷的同型形态:
	// 无法区分「未提供」与「空值」，后果是静默改写不该改的列。
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "banned", "cold", "p_night")

	// 只改 pool，其余全 nil
	if _, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Pool: strPtr("warm"),
	}); err != nil {
		t.Fatalf("PatchVolcKeyState: %v", err)
	}

	k, _ := s.GetVolcKey(ctx, "volc_001")
	if k.Status != "banned" {
		t.Errorf("status 被改成 %q，期望仍是 banned（封禁不该被一次改池操作抹掉）", k.Status)
	}
	if k.PersonaID != "p_night" {
		t.Errorf("persona_id 被改成 %q，期望仍是 p_night", k.PersonaID)
	}
	if k.EgressIP != "" && k.HealthScore == 0 {
		t.Errorf("本端点不该触碰 egress_ip / health_score")
	}
	if k.Pool != "warm" {
		t.Errorf("pool = %q, 期望 warm", k.Pool)
	}
}

func TestPatchVolcKeyState_显式空串会真的清空(t *testing.T) {
	// 与上一条互为对照: 同一个字段，nil 保留、空串清空。
	// 两条必须同时成立，否则「区分未提供与空值」只是半个实现。
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "p_day")

	if _, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		PersonaID: strPtr(""),
	}); err != nil {
		t.Fatalf("PatchVolcKeyState: %v", err)
	}
	k, _ := s.GetVolcKey(ctx, "volc_001")
	if k.PersonaID != "" {
		t.Errorf("persona_id = %q, 期望被清空", k.PersonaID)
	}
	if k.Status != "active" || k.Pool != "hot" {
		t.Errorf("其他列被误改: status=%q pool=%q", k.Status, k.Pool)
	}
}

func TestPatchVolcKeyState_expectedStatus不匹配时不写入(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "")

	res, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Status:         strPtr("banned"),
		ExpectedStatus: strPtr("cooldown"),
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("err = %v, 期望 ErrPreconditionFailed", err)
	}
	// 必须同时返回当前值，供调用方写出可自解释的 409 文案
	if res == nil {
		t.Fatal("前置条件失败时应返回当前值快照")
	}
	if res.PrevStatus != "active" {
		t.Errorf("快照 PrevStatus = %q, 期望 active", res.PrevStatus)
	}

	k, _ := s.GetVolcKey(ctx, "volc_001")
	if k.Status != "active" {
		t.Errorf("条件不匹配却写入了，status = %q", k.Status)
	}
}

func TestPatchVolcKeyState_expectedStatus匹配时正常写入(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "")

	res, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Status:         strPtr("cooldown"),
		ExpectedStatus: strPtr("active"),
	})
	if err != nil {
		t.Fatalf("PatchVolcKeyState: %v", err)
	}
	if res.PrevStatus != "active" || res.NewStatus != "cooldown" {
		t.Errorf("新旧值 = %q → %q", res.PrevStatus, res.NewStatus)
	}
}

func TestPatchVolcKeyState_RejectStatusFrom拦截终态(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_banned", "banned", "cold", "")
	seedPatchKey(t, s, ctx, "volc_invalid", "invalid", "cold", "")

	for _, keyID := range []string{"volc_banned", "volc_invalid"} {
		_, err := s.PatchVolcKeyState(ctx, keyID, VolcKeyPatch{
			Status:           strPtr("active"),
			RejectStatusFrom: []string{"banned", "invalid"},
		})
		if !errors.Is(err, ErrPreconditionFailed) {
			t.Errorf("%s err = %v, 期望 ErrPreconditionFailed", keyID, err)
		}
		k, _ := s.GetVolcKey(ctx, keyID)
		if k.Status == "active" {
			t.Errorf("%s 被拒却仍改成了 active", keyID)
		}
	}
}

func TestPatchVolcKeyState_守卫为空时不拦截(t *testing.T) {
	// RejectStatusFrom 为 nil / 空切片都表示「无守卫」。
	// 空切片若被编码成空数组而非 NULL，ANY 比较的结果需要额外推理，
	// 这条测试钉住两种写法行为一致。
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "banned", "cold", "")

	if _, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Status: strPtr("active"), RejectStatusFrom: []string{},
	}); err != nil {
		t.Fatalf("空守卫不应拦截: %v", err)
	}
	k, _ := s.GetVolcKey(ctx, "volc_001")
	if k.Status != "active" {
		t.Errorf("status = %q, 期望 active", k.Status)
	}
}

func TestPatchVolcKeyState_Key不存在返回ErrNotFound(t *testing.T) {
	// 与 ErrPreconditionFailed 必须可区分: 两者在 UPDATE 层面都是影响 0 行，
	// 对外一个 404 一个 409。
	s, ctx := newTestStore(t)
	res, err := s.PatchVolcKeyState(ctx, "volc_nope", VolcKeyPatch{Status: strPtr("banned")})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, 期望 ErrNotFound", err)
	}
	if res != nil {
		t.Errorf("Key 不存在时不应返回快照: %+v", res)
	}
}

func TestPatchVolcKeyState_同值重放幂等(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "p_day")

	for i := 0; i < 3; i++ {
		res, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{Pool: strPtr("cold")})
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		if res.NewPool != "cold" {
			t.Errorf("第 %d 次 NewPool = %q", i+1, res.NewPool)
		}
		// 第二次起新旧值应相同，调用方据此判定「无实际变更」
		if i > 0 && res.PrevPool != res.NewPool {
			t.Errorf("第 %d 次重放的新旧值不一致: %q → %q", i+1, res.PrevPool, res.NewPool)
		}
	}
}

func TestPatchVolcKeyState_无待更新字段直接报错(t *testing.T) {
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "")
	if _, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{}); err == nil {
		t.Error("三个字段全 nil 时应报错，否则调用方无法区分「改了」与「什么都没改」")
	}
	if _, err := s.PatchVolcKeyState(ctx, "", VolcKeyPatch{Status: strPtr("banned")}); err == nil {
		t.Error("空 key_id 应报错")
	}
}

func TestPatchVolcKeyState_并发下乐观并发控制严格成立(t *testing.T) {
	// 这条是 expected_status 的核心价值验证。
	//
	// 若实现退化成「先 SELECT 校验再 UPDATE」，两个并发请求会各自读到
	// active、各自通过校验、再互相覆盖，于是两者都成功 —— 顺序调用的
	// 测试完全看不出这个缺陷。单条 SQL 的 CAS 保证有且仅有一个成功。
	s, ctx := newTestStore(t)
	seedPatchKey(t, s, ctx, "volc_001", "active", "hot", "")

	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		okCount  int
		conflict int
		others   []error
	)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// 全部声称「我看到的是 active」，都想改成各自的目标状态
			target := "banned"
			if i%2 == 1 {
				target = "cooldown"
			}
			_, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
				Status:         strPtr(target),
				ExpectedStatus: strPtr("active"),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrPreconditionFailed):
				conflict++
			default:
				others = append(others, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range others {
		t.Errorf("意外错误: %v", err)
	}
	if okCount != 1 {
		t.Errorf("成功数 = %d, 期望恰好 1（乐观并发控制必须只放行一个）", okCount)
	}
	if conflict != n-1 {
		t.Errorf("冲突数 = %d, 期望 %d", conflict, n-1)
	}

	// 最终状态必须是某个成功者写入的值，不能是 active
	k, _ := s.GetVolcKey(ctx, "volc_001")
	if k.Status != "banned" && k.Status != "cooldown" {
		t.Errorf("最终 status = %q, 期望 banned 或 cooldown", k.Status)
	}
}

func TestPatchVolcKeyState_不触碰密文与出口绑定(t *testing.T) {
	// egress_ip 是终身绑定，secret_enc 是密钥。本端点绝不应改动它们 ——
	// 换出口等于把一个有历史的老账号变成「换了地址的账号」，
	// 这是风控最敏感的信号。
	s, ctx := newTestStore(t)
	if _, err := s.UpsertVolcKey(ctx, &VolcKey{
		KeyID: "volc_001", Secret: "sk-original", Status: "active",
		Pool: "hot", EgressIP: "10.0.0.7", PersonaID: "p_day",
	}); err != nil {
		t.Fatalf("预置: %v", err)
	}
	before, _ := s.GetVolcKey(ctx, "volc_001")

	if _, err := s.PatchVolcKeyState(ctx, "volc_001", VolcKeyPatch{
		Status: strPtr("banned"), Pool: strPtr("cold"), PersonaID: strPtr("p_night"),
	}); err != nil {
		t.Fatalf("PatchVolcKeyState: %v", err)
	}

	after, _ := s.GetVolcKey(ctx, "volc_001")
	if after.EgressIP != before.EgressIP {
		t.Errorf("egress_ip 被改动: %q → %q", before.EgressIP, after.EgressIP)
	}
	if after.SecretEnc != before.SecretEnc {
		t.Error("secret_enc 被改动")
	}
	if after.Secret != "sk-original" {
		t.Errorf("密钥解密结果变了: %q", after.Secret)
	}
	if after.HealthScore != before.HealthScore {
		t.Errorf("health_score 被改动: %d → %d", before.HealthScore, after.HealthScore)
	}
}
