package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault_IsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("内置默认配置应自洽: %v", err)
	}
}

func TestWatermarks(t *testing.T) {
	q := Default().Quota
	if got := q.TokenHard(); got != 4_500_000 {
		t.Errorf("Token 硬水位 = %d, 期望 450 万", got)
	}
	if got := q.TokenSoft(); got != 4_000_000 {
		t.Errorf("Token 软水位 = %d, 期望 400 万", got)
	}
	if got := q.CountHard(); got != 90 {
		t.Errorf("次数硬水位 = %d, 期望 90", got)
	}
	if got := q.CountSoft(); got != 80 {
		t.Errorf("次数软水位 = %d, 期望 80", got)
	}
	if q.TokenSoft() >= q.TokenHard() {
		t.Error("软水位必须低于硬水位")
	}
}

func TestValidate_RejectsInvalidCombinations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"软水位不低于硬水位", func(c *Config) {
			c.Quota.TokenSoftRatio = 0.95
			c.Quota.TokenHardRatio = 0.90
		}},
		{"次数型软水位不低于硬水位", func(c *Config) {
			c.Quota.CountSoftRatio = 0.95
		}},
		{"回收间隔不短于租约 TTL", func(c *Config) {
			c.Quota.ReapInterval = 5 * time.Minute
			c.Quota.LeaseTTL = time.Minute
		}},
		{"multi_ip 未配置出口 IP", func(c *Config) {
			c.Egress.Mode = "multi_ip"
			c.Egress.IPs = nil
		}},
		{"出口模式非法", func(c *Config) {
			c.Egress.Mode = "proxy"
		}},
		{"启用 fallback 但无预算上限", func(c *Config) {
			c.Fallback.Enabled = true
			c.Fallback.DailyBudgetCents = 0
		}},
		{"估算系数小于 1", func(c *Config) {
			c.Quota.EstimateMultiplier = 0.8
		}},
		{"监听地址为空", func(c *Config) {
			c.Server.Addr = ""
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			c.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("应校验失败，但通过了")
			}
		})
	}
}

// P1-10: 付费渠道 fallback 必须有预算护栏才允许启用。
func TestValidate_FallbackRequiresBudget(t *testing.T) {
	cfg := Default()
	if cfg.Fallback.Enabled {
		t.Error("fallback 默认必须关闭")
	}

	cfg.Fallback.Enabled = true
	cfg.Fallback.DailyBudgetCents = 50_000
	if err := cfg.Validate(); err != nil {
		t.Errorf("配了预算上限应允许启用: %v", err)
	}
}

// P0-2: 回收间隔必须短于租约 TTL，否则泄漏的预扣无法及时释放。
func TestValidate_ReapMustBeFasterThanLease(t *testing.T) {
	cfg := Default()
	if cfg.Quota.ReapInterval >= cfg.Quota.LeaseTTL {
		t.Fatal("默认配置的回收间隔应短于租约 TTL")
	}
}

func TestLoad_YAMLOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  addr: ":9999"
quota:
  token_limit: 1000000
egress:
  mode: multi_ip
  ips:
    - addr: "172.16.0.2"
      public_ip: "1.2.3.4"
      max_keys: 8
upstream:
  volc_base_url: "http://mockark:8081"
  model_mapping:
    deepseek-v3: deepseek-v3-241226
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %s", cfg.Server.Addr)
	}
	if cfg.Quota.TokenLimit != 1_000_000 {
		t.Errorf("token_limit = %d", cfg.Quota.TokenLimit)
	}
	if cfg.Egress.Mode != "multi_ip" || len(cfg.Egress.IPs) != 1 {
		t.Errorf("出口配置未生效: %+v", cfg.Egress)
	}
	if cfg.Egress.IPs[0].Addr != "172.16.0.2" || cfg.Egress.IPs[0].MaxKeys != 8 {
		t.Errorf("出口 IP 解析错误: %+v", cfg.Egress.IPs[0])
	}
	// 未覆盖的字段应保留默认值
	if cfg.Quota.LeaseTTL != 120*time.Second {
		t.Errorf("未指定的字段应保留默认值, got %s", cfg.Quota.LeaseTTL)
	}
	if got := cfg.UpstreamModel("deepseek-v3"); got != "deepseek-v3-241226" {
		t.Errorf("模型映射 = %s", got)
	}
	if got := cfg.UpstreamModel("unknown"); got != "unknown" {
		t.Errorf("未映射模型应原样返回, got %s", got)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":1111\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FLUXKEYS_ADDR", ":2222")
	t.Setenv("REDIS_ADDR", "redis-host:6380")
	t.Setenv("VOLC_BASE_URL", "http://mock:8081")
	t.Setenv("EGRESS_MODE", "multi_ip")
	t.Setenv("EGRESS_IPS", "172.16.0.2=1.2.3.4,172.16.0.3=1.2.3.5")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != ":2222" {
		t.Errorf("环境变量应覆盖 YAML, got %s", cfg.Server.Addr)
	}
	if cfg.Redis.Addr != "redis-host:6380" {
		t.Errorf("redis addr = %s", cfg.Redis.Addr)
	}
	if len(cfg.Egress.IPs) != 2 {
		t.Fatalf("应解析出 2 个出口 IP, got %d", len(cfg.Egress.IPs))
	}
	if cfg.Egress.IPs[1].Addr != "172.16.0.3" || cfg.Egress.IPs[1].PublicIP != "1.2.3.5" {
		t.Errorf("出口 IP 解析错误: %+v", cfg.Egress.IPs[1])
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Error("文件不存在应报错")
	}
}

func TestLoad_EmptyPathUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("空路径应回落到默认配置: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %s", cfg.Server.Addr)
	}
}

func TestIsCountModel(t *testing.T) {
	cfg := Default()
	if !cfg.IsCountModel("seedream") {
		t.Error("seedream 应为次数型")
	}
	if !cfg.IsCountModel("SeeDream") {
		t.Error("模型名匹配应忽略大小写")
	}
	if cfg.IsCountModel("deepseek-v3") {
		t.Error("deepseek-v3 应为 Token 型")
	}
}

func TestParseEgressIPs(t *testing.T) {
	got := parseEgressIPs("172.16.0.2=1.2.3.4, 172.16.0.3 ,")
	if len(got) != 2 {
		t.Fatalf("应解析出 2 条, got %d: %+v", len(got), got)
	}
	if got[0].PublicIP != "1.2.3.4" {
		t.Errorf("公网 IP = %s", got[0].PublicIP)
	}
	if got[1].Addr != "172.16.0.3" || got[1].PublicIP != "" {
		t.Errorf("缺省公网 IP 应为空: %+v", got[1])
	}
	if got[0].MaxKeys != 10 {
		t.Errorf("MaxKeys 默认值 = %d, 期望 10", got[0].MaxKeys)
	}
}

// 默认必须能离线跑通: direct 模式不需要任何出口 IP 配置。
func TestDefault_RunsOfflineFriendly(t *testing.T) {
	cfg := Default()
	if cfg.Egress.Mode != "direct" {
		t.Errorf("默认出口模式应为 direct, got %s", cfg.Egress.Mode)
	}
	if cfg.Admin.APIKey != "" {
		t.Error("管理密钥默认应为空（禁用 admin 路由）")
	}
}

// EGRESS_IPS 需支持档位与容量，同时保持旧语法可用。
func TestParseEgressIPs_档位与容量(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []EgressIP
	}{
		{
			name: "旧语法仅地址",
			in:   "172.16.0.11",
			want: []EgressIP{{Addr: "172.16.0.11", MaxKeys: 10}},
		},
		{
			name: "旧语法带公网地址",
			in:   "172.16.0.11=203.0.113.11",
			want: []EgressIP{{Addr: "172.16.0.11", PublicIP: "203.0.113.11", MaxKeys: 10}},
		},
		{
			name: "限定档位",
			in:   "172.16.0.11=203.0.113.11|hot",
			want: []EgressIP{{Addr: "172.16.0.11", PublicIP: "203.0.113.11", MaxKeys: 10, Pool: "hot"}},
		},
		{
			name: "档位与容量",
			in:   "172.16.0.11=203.0.113.11|cold|100",
			want: []EgressIP{{Addr: "172.16.0.11", PublicIP: "203.0.113.11", MaxKeys: 100, Pool: "cold"}},
		},
		{
			name: "无公网地址但有档位",
			in:   "172.16.0.11|warm|30",
			want: []EgressIP{{Addr: "172.16.0.11", MaxKeys: 30, Pool: "warm"}},
		},
		{
			name: "多项混合",
			in:   "172.16.0.11|hot|10, 172.16.0.12|cold|100 ,172.16.0.13",
			want: []EgressIP{
				{Addr: "172.16.0.11", MaxKeys: 10, Pool: "hot"},
				{Addr: "172.16.0.12", MaxKeys: 100, Pool: "cold"},
				{Addr: "172.16.0.13", MaxKeys: 10},
			},
		},
		{
			name: "非法容量回落到默认值",
			in:   "172.16.0.11|hot|abc",
			want: []EgressIP{{Addr: "172.16.0.11", MaxKeys: 10, Pool: "hot"}},
		},
		{
			name: "空地址项被跳过",
			in:   "172.16.0.11,,  ",
			want: []EgressIP{{Addr: "172.16.0.11", MaxKeys: 10}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseEgressIPs(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("解析出 %d 项，期望 %d 项: %+v", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("第 %d 项 = %+v, 期望 %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}
