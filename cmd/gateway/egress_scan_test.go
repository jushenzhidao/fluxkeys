package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fluxkeys/fluxkeys/internal/config"
	"github.com/fluxkeys/fluxkeys/internal/egress"
)

// 本文件守「扫描本机地址 → 分档填充 → 出口池」这条装配路径。
//
// 断言对象是**真实网卡**上跑出来的结果，因此所有期望值都由本机实际发现的地址数
// 推出，不写死任何数量 —— 换机器、换 CI 都不会假红。反过来，「地址不足要拒绝
// 启动」「份额密度校验不能因切到扫描而失效」这两条则与机器无关，写死断言。

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// hostAddrs 用与装配层相同的过滤条件取本机可用出口地址。
func hostAddrs(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	addrs, err := egress.DiscoverAddrs(egress.ScanOptions{
		IfaceDeny:   cfg.Egress.Scan.IfaceDeny,
		PrefixAllow: cfg.Egress.Scan.PrefixAllow,
		PrefixDeny:  cfg.Egress.Scan.PrefixDeny,
	})
	if err != nil {
		t.Fatalf("枚举本机地址: %v", err)
	}
	return addrs
}

func scanTestCfg(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Egress.Mode = "multi_ip"
	cfg.Egress.IPsSource = config.EgressSourceScan
	cfg.Egress.VerifyOnStart = false
	cfg.Egress.RequestTimeout = 5 * time.Second
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func TestBuildEgressPool_扫描本机地址填充通用档(t *testing.T) {
	cfg := scanTestCfg(t, func(c *config.Config) {
		c.Egress.Scan.Tiers = []config.EgressScanTier{{Pool: "", Count: 0, MaxKeys: 7}}
	})
	addrs := hostAddrs(t, cfg)
	if len(addrs) == 0 {
		t.Skip("本机没有可作出口的地址（只有回环或被黑名单覆盖），无法验证扫描装配")
	}

	pool, err := buildEgressPool(cfg, discardLogger())
	if err != nil {
		t.Fatalf("扫描装配失败: %v", err)
	}
	if pool.Mode() != egress.ModeMultiIP {
		t.Fatalf("出口池模式 = %s，期望 multi_ip", pool.Mode())
	}
	st := pool.Stats()
	if st.Total != len(addrs) {
		t.Errorf("入口出口数 = %d，期望本机扫到的 %d 个（%v）", st.Total, len(addrs), addrs)
	}
	// 每个出口都必须带上分档计划里的 max_keys 与档位 —— 漏掉任何一个都意味着
	// 那个出口的承载能力与风控假设不符。
	for _, ip := range st.PerIP {
		if ip.MaxKeys != 7 {
			t.Errorf("出口 %s 的 max_keys = %d，期望 7", ip.Addr, ip.MaxKeys)
		}
		if ip.Pool != egress.PoolAny {
			t.Errorf("出口 %s 落在档位 %q，期望通用档", ip.Addr, ip.Pool)
		}
	}
}

func TestBuildEgressPool_扫描模式忽略yaml里的显式清单(t *testing.T) {
	cfg := scanTestCfg(t, func(c *config.Config) {
		// 档位配置随镜像分发（改它要重建镜像），所以「在某台机器上试一次扫描」
		// 不该被旧清单挡住 —— 这条路径必须放行，由装配层告警说明忽略了什么。
		c.Egress.IPs = []config.EgressIP{{Addr: "203.0.113.99", MaxKeys: 3, Pool: "hot"}}
		c.Egress.Scan.Tiers = []config.EgressScanTier{{Pool: "", Count: 0, MaxKeys: 5}}
	})
	if len(hostAddrs(t, cfg)) == 0 {
		t.Skip("本机没有可作出口的地址，无法验证扫描装配")
	}

	pool, err := buildEgressPool(cfg, discardLogger())
	if err != nil {
		t.Fatalf("扫描模式不应因 YAML 里另有 ips 而失败: %v", err)
	}
	// 显式清单里的那个地址绝不能进池 —— 否则「两个来源同时生效」就成了事实上的
	// 静默合并，而运维以为自己只选了一个。
	for _, ip := range pool.Stats().PerIP {
		if ip.Addr == "203.0.113.99" {
			t.Error("扫描模式下 YAML 里的 egress.ips 必须被忽略，不能合并进池")
		}
	}
}

func TestBuildEgressPool_地址不足时拒绝启动(t *testing.T) {
	cfg := scanTestCfg(t, nil)
	n := len(hostAddrs(t, cfg))
	// 要求本机扫到的地址数 + 1 个 ⇒ 必然不足，且不依赖机器规模。
	cfg.Egress.Scan.Tiers = []config.EgressScanTier{{Pool: "hot", Count: n + 1, MaxKeys: 10}}

	_, err := buildEgressPool(cfg, discardLogger())
	if err == nil {
		t.Fatal("地址不足时必须拒绝启动 —— 缩水运行会静默丢掉撤离余量")
	}
	// 报错要给出两个可核对的数字与两条出路，否则运维只能猜该改哪里。
	for _, want := range []string{"tiers", "egress.ips"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错应含 %q，实际: %v", want, err)
		}
	}
}

func TestBuildEgressPool_扫描模式下份额密度校验仍生效(t *testing.T) {
	cfg := scanTestCfg(t, func(c *config.Config) {
		// 只有一个 hot 通用之外的档位、且出口极少，配上高份额与高峰值 QPS
		// ⇒ 密度必然超标。这条校验在扫描模式下必须仍然生效: 它在配置校验阶段
		// 算不出来（「接住剩余」那一档要等装配完成才知道有几个地址），
		// 若这里也不补，开启扫描就等于丢掉防超密度的唯一自动闸门。
		c.Egress.Scan.Tiers = []config.EgressScanTier{{Pool: "hot", Count: 1, MaxKeys: 10}}
		c.Scheduler.PoolShares = map[string]float64{"hot": 1}
		c.Scheduler.ExpectedPeakQPS = 500
	})
	if len(hostAddrs(t, cfg)) == 0 {
		t.Skip("本机没有可作出口的地址，无法验证密度校验")
	}

	_, err := buildEgressPool(cfg, discardLogger())
	if err == nil {
		t.Fatal("每出口请求量远超上限时应拒绝启动")
	}
	if !strings.Contains(err.Error(), "pool_shares") {
		t.Errorf("报错应指向 pool_shares，实际: %v", err)
	}
}

func TestBuildEgressPool_direct模式不扫描(t *testing.T) {
	cfg := scanTestCfg(t, func(c *config.Config) {
		c.Egress.Mode = "direct"
		// direct 形态下 tiers 留空 —— 若实现里把扫描的校验放错位置，
		// 这里会因为「tiers 为空」而失败。
	})
	pool, err := buildEgressPool(cfg, discardLogger())
	if err != nil {
		t.Fatalf("direct 模式不应受扫描配置影响: %v", err)
	}
	if pool.Mode() != egress.ModeDirect {
		t.Errorf("模式 = %s，期望 direct", pool.Mode())
	}
}
