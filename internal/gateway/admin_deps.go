package gateway

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/fluxkeys/fluxkeys/internal/confsnap"
	"github.com/fluxkeys/fluxkeys/internal/egress"
	"github.com/fluxkeys/fluxkeys/internal/gateway/adminapi"
	"github.com/fluxkeys/fluxkeys/internal/metrics"
)

// 本文件让 *Server 满足 adminapi.Deps，从而把管理面挂到网关主体上。
//
// 全部是「取出并交出」的一行访问器，刻意不含任何逻辑: 一旦这里出现分支，
// 管理面看到的状态与热路径看到的状态就可能不同，而两者读的本该是同一份。
//
// 依赖方向是 gateway → adminapi（单向）。adminapi 不 import 本包，
// 因此管理面可以脱离网关主体单独测试。

var _ adminapi.Deps = (*Server)(nil)

func (s *Server) Log() *slog.Logger { return s.log }

// Store 交出的是**收窄后**的接口类型: adminapi.Store 只含管理面用到的
// 7 个方法，而 s.store 的静态类型是热路径的 Store（11 个方法）。
// 直接返回 s.store 也编译得过（超集可赋给子集），但那样 adminapi
// 里就能拿到 RevokeUserAPIKey 之外的鉴权/流水方法 —— 接口收窄的意义
// 在于让「管理面能碰什么」在类型上就写死。
func (s *Server) Store() adminapi.Store { return s.store }

func (s *Server) Scheduler() adminapi.Scheduler { return s.sched }

func (s *Server) Egress() *egress.Pool { return s.egress }

func (s *Server) Metrics() *metrics.Metrics { return s.metrics }

func (s *Server) Snaps() *confsnap.Holder { return s.snaps }

func (s *Server) AdminChain(h http.HandlerFunc) http.Handler { return s.adminChain(h) }

// InvalidateAuthCache 把吊销的生效延迟从缓存的 TTL 压到零。
func (s *Server) InvalidateAuthCache() { s.authCache.invalidate() }

// CreateUser 创建用户并把存储层回填后的限额搬进 adminapi 的结果视图。
//
// 返回值的每个字段都必须来自存储层的返回而非请求入参: 调用方没传限额时
// 存储层会填默认值，回显请求值会让调用方以为「不限」而实际上有默认上限。
func (s *Server) CreateUser(ctx context.Context, in adminapi.NewUser) (adminapi.CreatedUser, error) {
	uc, err := s.store.CreateUser(ctx, in)
	if err != nil {
		return adminapi.CreatedUser{}, err
	}
	return adminapi.CreatedUser{
		UserID:          uc.UserID,
		Name:            uc.Name,
		RPMLimit:        uc.RPMLimit,
		TPMLimit:        uc.TPMLimit,
		DailyTokenLimit: uc.DailyTokenLimit,
	}, nil
}
