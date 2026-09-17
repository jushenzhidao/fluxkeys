package adminapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// 本文件的用例钉住 KI-034 在管理面的接线: PATCH 改完库后必须调用
// Scheduler.RefreshKey，否则「重启前被 ban 的 Key 被 force 复活」只会改
// health，进不了活跃池 s.keys，于是永远不被 Select 选中。

// patchStore 是 Store 的最小可控实现，PATCH 路径之外的 метод均返回零值。
type patchStore struct {
	patchRes *KeyPatchResult
	patchErr error
	audits   []string
}

func (s *patchStore) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	s.audits = append(s.audits, action)
	return nil
}
func (s *patchStore) UpsertUpstreamKey(ctx context.Context, in NewUpstreamKey) (bool, error) {
	return false, nil
}
func (s *patchStore) RevokeUserAPIKey(ctx context.Context, userID, keyID int64) error { return nil }
func (s *patchStore) PatchUpstreamKeyState(ctx context.Context, keyID string, p KeyPatch) (*KeyPatchResult, error) {
	return s.patchRes, s.patchErr
}
func (s *patchStore) AssignShard(ctx context.Context, shard string, keyIDs []string) (int64, error) {
	return 0, nil
}
func (s *patchStore) SetVolcKeyEgressIP(ctx context.Context, keyID, egressIP string) error {
	return nil
}
func (s *patchStore) DeleteUpstreamKey(ctx context.Context, keyID string) error { return nil }
func (s *patchStore) CreateUserAPIKey(ctx context.Context, userID int64, name string) (string, IssuedKey, error) {
	return "", IssuedKey{}, nil
}

// patchScheduler 记录 SetKeyStatus 与 RefreshKey 的调用。
type patchScheduler struct {
	mu         sync.Mutex
	statuses   map[string]string
	refreshIDs []string
	refreshErr error
}

func (s *patchScheduler) SetKeyStatus(keyID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses == nil {
		s.statuses = map[string]string{}
	}
	s.statuses[keyID] = status
}
func (s *patchScheduler) KeyStates(ctx context.Context) ([]KeyState, error) { return nil, nil }
func (s *patchScheduler) Reload(ctx context.Context) error                  { return nil }
func (s *patchScheduler) RefreshKey(ctx context.Context, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshIDs = append(s.refreshIDs, keyID)
	return s.refreshErr
}

// patchDeps 实现 Deps，Egress 为 nil（本用例 pool 不变，不触发出口迁移）。
type patchDeps struct {
	store *patchStore
	sched *patchScheduler
}

func (d *patchDeps) Log() *slog.Logger         { return slog.Default() }
func (d *patchDeps) Store() Store              { return d.store }
func (d *patchDeps) Scheduler() Scheduler      { return d.sched }
func (d *patchDeps) Egress() *egress.Pool      { return nil }
func (d *patchDeps) Metrics() *metrics.Metrics { return nil }
func (d *patchDeps) Snaps() *confsnap.Holder   { return nil }
func (d *patchDeps) InvalidateAuthCache()      {}
func (d *patchDeps) CreateUser(ctx context.Context, in NewUser) (CreatedUser, error) {
	return CreatedUser{}, nil
}
func (d *patchDeps) AdminChain(h http.HandlerFunc) http.Handler { return h }

func TestPatch_force复活后触发单键刷新_KI034(t *testing.T) {
	st := &patchStore{patchRes: &KeyPatchResult{
		PrevStatus: "banned", NewStatus: "active",
		PrevPool: "hot", NewPool: "hot",
		PrevPersonaID: "p_01", NewPersonaID: "p_01",
	}}
	sch := &patchScheduler{}
	a := &API{deps: &patchDeps{store: st, sched: sch}}

	req := httptest.NewRequest(http.MethodPatch, "/admin/keys/volc_001",
		strings.NewReader(`{"status":"active","force":true}`))
	req.SetPathValue("key_id", "volc_001")
	rec := httptest.NewRecorder()
	a.handleAdminKeyPatch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}
	// KI-034 的核心: 复活必须触发 RefreshKey 把 Key 拉回活跃池，光改 health 不够。
	if len(sch.refreshIDs) != 1 || sch.refreshIDs[0] != "volc_001" {
		t.Fatalf("应触发一次 RefreshKey(volc_001)，实际 %v", sch.refreshIDs)
	}
	// health 状态也要置回 active（封禁/复活的状态机仍由 SetKeyStatus 驱动）。
	if sch.statuses["volc_001"] != "active" {
		t.Fatalf("SetKeyStatus 应为 active，实际 %q", sch.statuses["volc_001"])
	}
	// 操作要留审计痕。
	if len(st.audits) != 1 || st.audits[0] != "patch_volc_key" {
		t.Fatalf("应记一次 patch_volc_key 审计，实际 %v", st.audits)
	}
}

func TestPatch_单键刷新失败仍返回成功但降级(t *testing.T) {
	st := &patchStore{patchRes: &KeyPatchResult{
		PrevStatus: "banned", NewStatus: "active",
		PrevPool: "hot", NewPool: "hot",
	}}
	sch := &patchScheduler{refreshErr: errors.New("boom")}
	a := &API{deps: &patchDeps{store: st, sched: sch}}

	req := httptest.NewRequest(http.MethodPatch, "/admin/keys/volc_001",
		strings.NewReader(`{"status":"active","force":true}`))
	req.SetPathValue("key_id", "volc_001")
	rec := httptest.NewRecorder()
	a.handleAdminKeyPatch(rec, req)

	// 刷新失败不回滚已生效的写库与内存状态，仍 200 —— 下一个周期 key_reload
	// 会兜底对齐池归属，返回错误只会让运维误以为复活失败而反复重试。
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}
}
