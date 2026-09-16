package config

import (
	"strings"
	"testing"
)

// 本文件覆盖「出口地址自动发现」（egress.ips_source=scan）的配置校验。
//
// 扫描本身只决定「有哪些地址」，**分档计划仍由人给** —— 那些数字来自反封禁的
// 容量算术（撤离余量、单 IP 等效密度）。所以校验的重点全在「计划写错了会静默
// 失效」的那几处: 档位名拼错、count=0 放错位置、档位漏配、CIDR 写错、
// 以及两种地址来源同时生效。

func scanCfg(t *testing.T, mutate func(*Config)) *Config {
	t.Helper()
	cfg := Default()
	cfg.Egress.Mode = "multi_ip"
	cfg.Egress.IPsSource = EgressSourceScan
	cfg.Egress.Scan.Tiers = []EgressScanTier{
		{Pool: "hot", Count: 2, MaxKeys: 10},
		{Pool: "", Count: 0, MaxKeys: 10}, // 通用档接住剩余
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func TestValidate_扫描模式的合法配置(t *testing.T) {
	if err := scanCfg(t, nil).Validate(); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	// 只有通用档一个 catch-all 也是合法的：等价于「不过滤分层」，与
	// 现在 EGRESS_IPS 全通用档的形态一致。
	one := scanCfg(t, func(c *Config) {
		c.Egress.Scan.Tiers = []EgressScanTier{{Pool: "", Count: 0, MaxKeys: 10}}
	})
	if err := one.Validate(); err != nil {
		t.Errorf("单一通用档应合法: %v", err)
	}
}

func TestValidate_扫描模式必须给出分档计划(t *testing.T) {
	cfg := scanCfg(t, func(c *Config) { c.Egress.Scan.Tiers = nil })
	err := cfg.Validate()
	if err == nil {
		t.Fatal("没有 tiers 时应拒绝启动 —— 扫描无法自行决定分几档、每档多少容量")
	}
	// 报错要指向该改的地方，并说明为什么不能自动决定。
	for _, want := range []string{"tiers", "scan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错应含 %q，实际: %v", want, err)
		}
	}
}

func TestValidate_扫描模式拦下静默失效的几种写法(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string // 报错里必须出现的关键词
	}{
		{
			// 拼错的档位名不会报错，只是永远不匹配任何 Key —— 该档等于不存在。
			name:   "档位名拼错",
			mutate: func(c *Config) { c.Egress.Scan.Tiers[0].Pool = "Hot" },
			want:   "pool",
		},
		{
			// count=0 是「接住剩余全部」，放在中间会让它之后的项拿不到地址。
			name: "count=0 出现在中间项",
			mutate: func(c *Config) {
				c.Egress.Scan.Tiers = []EgressScanTier{
					{Pool: "hot", Count: 0, MaxKeys: 10},
					{Pool: "cold", Count: 3, MaxKeys: 100},
				}
			},
			want: "最后一项",
		},
		{
			name:   "count 为负",
			mutate: func(c *Config) { c.Egress.Scan.Tiers[0].Count = -1 },
			want:   "count",
		},
		{
			name:   "max_keys 为负",
			mutate: func(c *Config) { c.Egress.Scan.Tiers[0].MaxKeys = -5 },
			want:   "max_keys",
		},
		{
			name:   "prefix_allow 不是合法 CIDR",
			mutate: func(c *Config) { c.Egress.Scan.PrefixAllow = []string{"172.16.0.0/33"} },
			want:   "CIDR",
		},
		{
			name:   "prefix_deny 不是合法 CIDR",
			mutate: func(c *Config) { c.Egress.Scan.PrefixDeny = []string{"172.16.0.11"} },
			want:   "CIDR",
		},
		{
			// pool_shares 里分了 Key 却没有对应档位的出口 ⇒ 这些 Key 找不到出口。
			name: "档位漏配",
			mutate: func(c *Config) {
				c.Egress.Scan.Tiers = []EgressScanTier{
					{Pool: "hot", Count: 2, MaxKeys: 10},
					{Pool: "warm", Count: 1, MaxKeys: 50},
				}
			},
			want: "cold",
		},
		{
			name:   "ips_source 取值非法",
			mutate: func(c *Config) { c.Egress.IPsSource = "auto" },
			want:   "ips_source",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := scanCfg(t, tc.mutate)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("该写法会让某档静默无出口/容量为 0，必须被拦下")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错应含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

func TestValidate_EGRESS_IPS与扫描互斥(t *testing.T) {
	// EGRESS_IPS 是运维显式给出的清单，与「扫描本机」互斥: 静默择一的结果是
	// 运维以为生效的那条没生效（出口集合多几个或少几个），却没有任何提示。
	cfg := scanCfg(t, func(c *Config) {
		c.Egress.IPs = []EgressIP{{Addr: "172.16.0.11", MaxKeys: 10}}
		c.Egress.ipsFromEnv = true
	})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("两者同时设置时必须拒绝启动，让人明确选一个")
	}
	if !strings.Contains(err.Error(), "EGRESS_IPS") {
		t.Errorf("报错应点名 EGRESS_IPS，实际: %v", err)
	}

	// YAML 里的 ips 不在此列: 档位配置随镜像分发，改它要重建镜像，而「在某台
	// 机器上试一次扫描」不该被旧清单挡住。装配层会 WARN 报出被忽略的条数。
	yamlOnly := scanCfg(t, func(c *Config) {
		c.Egress.IPs = []EgressIP{{Addr: "172.16.0.11", MaxKeys: 10}}
	})
	if err := yamlOnly.Validate(); err != nil {
		t.Errorf("YAML 里的 ips 与扫描并存应放行（仅忽略并告警）: %v", err)
	}
}

func TestValidate_multi_ip未配任何地址来源的报错指向两条路(t *testing.T) {
	cfg := Default()
	cfg.Egress.Mode = "multi_ip"
	cfg.Egress.IPsSource = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("multi_ip 既没有 ips 也没开扫描时应拒绝启动")
	}
	// 报错要同时给出两条出路，否则运维只会看到「必须配置 ips」而不知道
	// 现在还可以让网关自己扫。
	for _, want := range []string{"egress.ips", "ips_source=scan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错应含 %q，实际: %v", want, err)
		}
	}
}

func TestApplyEnv_可切换出口地址来源(t *testing.T) {
	cfg := Default()
	t.Setenv("EGRESS_IPS_SOURCE", "scan")
	applyEnv(cfg)
	if !cfg.Egress.Scanning() {
		t.Errorf("EGRESS_IPS_SOURCE=scan 应切换到扫描模式，实际 %q", cfg.Egress.IPsSource)
	}
}

// 用独立函数而不是接在上一个用例后面: t.Setenv 的作用域是整个测试函数，
// 两个场景写在一起会让第二个场景继承第一个的 EGRESS_IPS_SOURCE（实测踩过）。
func TestApplyEnv_EGRESS_IPS记录来源并解析档位(t *testing.T) {
	// IPs 来自环境变量这件事必须被记下来，否则无法拦下「既有 EGRESS_IPS
	// 又开了扫描」这种冲突配置。
	cfg := Default()
	t.Setenv("EGRESS_IPS", "172.16.0.11|hot|10,172.16.0.12")
	applyEnv(cfg)
	if !cfg.Egress.ipsFromEnv {
		t.Error("EGRESS_IPS 生效时应置 ipsFromEnv，供 Validate 判冲突")
	}
	if len(cfg.Egress.IPs) != 2 || cfg.Egress.IPs[0].Pool != "hot" {
		t.Errorf("EGRESS_IPS 解析结果不对: %+v", cfg.Egress.IPs)
	}
	if cfg.Egress.Scanning() {
		t.Error("只设 EGRESS_IPS 时不应进入扫描模式")
	}
}
