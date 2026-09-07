package gateway

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

// 鉴权结果的进程内缓存。
//
// 动机: 鉴权是每个请求的必经路径，而它的数据源是 Postgres 的一次 JOIN
// 查询。用户 Key 的变更频率是「天」级，每请求查一次库纯属浪费，且把
// Postgres 的可用性直接串进了所有请求的关键路径 —— PG 抖动时全站 5xx。
//
// 两个刻意的设计取舍:
//
//  1. 只缓存成功结果。失败不缓存 —— 负缓存会让「刚创建的 Key 因主从延迟
//     查不到」被放大成 TTL 级的持续 401；同时撞库请求不会污染缓存，
//     条目数天然被真实用户数封顶。
//
//  2. TTL 内吊销不生效（上限 authCacheTTL）。吊销是低频管理操作，
//     30 秒的生效延迟可接受；换来的是热路径上 Postgres 读放大直接归零。
//
// 缓存键是 token 的 SHA-256 而非明文: 进程内存的 heap dump 不应包含
// 可直接使用的用户凭证。

// authCacheTTL 是鉴权缓存的有效期。
const authCacheTTL = 30 * time.Second

// authCacheMaxEntries 是条目数上限的保险丝。只缓存成功结果时条目数被
// 真实 Key 数封顶，正常永远到不了这个值；到了说明有异常（比如误缓存了
// 失败结果的回归），一次性清空比 LRU 简单且足够。
const authCacheMaxEntries = 16 << 10

type authEntry struct {
	uc        UserContext
	expiresAt time.Time
}

// authCall 是一次在途的回源查询，用于并发去重。
type authCall struct {
	done chan struct{}
	uc   *UserContext
	err  error
}

type authCache struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	entries  map[[32]byte]authEntry
	inflight map[[32]byte]*authCall
}

func newAuthCache(ttl time.Duration) *authCache {
	return &authCache{
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[[32]byte]authEntry),
		inflight: make(map[[32]byte]*authCall),
	}
}

// authenticate 返回缓存的鉴权结果，未命中时经 fetch 回源。
//
// 返回值的第三项 fromCache 标明是否命中缓存，调用方据此决定是否要
// 顺带更新 last_used_at（命中时跳过，把落库频率钳到每 Key 每 TTL 一次）。
//
// 并发去重: 同一 token 的并发未命中只放一个进 Postgres，其余等待其结果。
// 缓存过期瞬间的高并发场景下，没有去重就是每过期一次打一排重复查询。
func (c *authCache) authenticate(ctx context.Context, token string,
	fetch func(ctx context.Context) (*UserContext, error)) (uc *UserContext, fromCache bool, err error) {

	key := sha256.Sum256([]byte(token))
	now := c.now()

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		cp := e.uc
		return &cp, true, nil
	}
	// 已有同 token 的在途查询: 挂上去等它的结果
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, false, call.err
			}
			cp := *call.uc
			return &cp, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	call := &authCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	call.uc, call.err = fetch(ctx)
	close(call.done)

	c.mu.Lock()
	delete(c.inflight, key)
	if call.err == nil && call.uc != nil {
		if len(c.entries) >= authCacheMaxEntries {
			c.entries = make(map[[32]byte]authEntry)
		}
		c.entries[key] = authEntry{uc: *call.uc, expiresAt: now.Add(c.ttl)}
	}
	c.mu.Unlock()

	if call.err != nil {
		return nil, false, call.err
	}
	cp := *call.uc
	return &cp, false, nil
}

// invalidate 清空全部缓存。供管理接口在吊销 Key / 停用用户后调用，
// 把吊销的生效延迟从 TTL 压到零。全清而非按 token 清: 管理操作拿到的是
// key_id 而非明文 token，无法定位到单个条目，而全清的代价只是一轮回源。
func (c *authCache) invalidate() {
	c.mu.Lock()
	c.entries = make(map[[32]byte]authEntry)
	c.mu.Unlock()
}

// keyToucher 是存储层的可选能力: 记录 API Key 最近使用时间。
//
// 定义为独立接口而非并入 Store: last_used_at 是锦上添花的观测数据，
// 不应强迫每个 Store 实现（尤其是测试替身）都写一个空方法。
type keyToucher interface {
	TouchUserAPIKey(ctx context.Context, keyID int64) error
}
