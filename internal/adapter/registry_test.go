package adapter

import (
	"sort"
	"testing"
)

// 本组用例原先只存在于 test/multi_provider_test.go，且打的是 internal/gateway 里
// 一份劣质副本（无互斥锁、无 nil 校验、无 provider 一致性校验）。副本已删除，
// 覆盖迁移到真实类型上 —— 顺带补上副本从未校验过的两个 panic 守卫。

func TestRegistry_Get返回已注册适配器(t *testing.T) {
	reg := NewRegistry()
	volc := NewVolc(map[string]string{"ep-20241226": "ep-20241226-xxxxx"})
	sense := NewSenseNova(map[string]string{"SenseChat-5": "SenseChat-5"})

	reg.Register("volc", volc)
	reg.Register("sensenova", sense)

	for _, provider := range []string{"volc", "sensenova"} {
		ad, err := reg.Get(provider)
		if err != nil {
			t.Fatalf("Get(%q) 返回错误: %v", provider, err)
		}
		if ad == nil {
			t.Fatalf("Get(%q) 返回 nil 适配器", provider)
		}
		if string(ad.Provider()) != provider {
			t.Errorf("Get(%q) 返回的适配器 Provider() = %q", provider, ad.Provider())
		}
	}
}

func TestRegistry_未注册provider返回错误(t *testing.T) {
	reg := NewRegistry()
	ad, err := reg.Get("nonexistent")
	if err == nil {
		t.Fatal("未注册的 provider 应返回错误")
	}
	if ad != nil {
		t.Errorf("出错时应同时返回 nil 适配器，实际 = %v", ad)
	}
}

// MustGet 是启动期校验用的，失败必须 panic 而不是静默返回零值 ——
// 返回 nil 会让调用方在运行时的请求路径上才炸。
func TestRegistry_MustGet未注册时panic(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet 对未注册 provider 应 panic")
		}
	}()
	reg.MustGet("nonexistent")
}

// 注册 nil 适配器必须启动即失败。放到运行时就是每个请求都拿到 nil 适配器，
// 表现成 500 而不是「配置写错了」。
func TestRegistry_注册nil适配器panic(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("注册 nil 适配器应 panic")
		}
	}()
	reg.Register("volc", nil)
}

// provider 名与适配器自报的 Provider() 不一致时必须启动即失败。
// 静默接受会让 key 与适配器错配 —— 请求带着 volc 的凭证发往 SenseNova 的
// 路径与响应解析，表现为一连串难以归因的 4xx。
func TestRegistry_provider不匹配时panic(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("provider 名与适配器 Provider() 不一致应 panic")
		}
	}()
	reg.Register("volc", NewSenseNova(map[string]string{}))
}

func TestRegistry_重复注册覆盖旧适配器(t *testing.T) {
	reg := NewRegistry()
	first := NewVolc(map[string]string{"a": "a"})
	second := NewVolc(map[string]string{"b": "b"})

	reg.Register("volc", first)
	reg.Register("volc", second)

	got, err := reg.Get("volc")
	if err != nil {
		t.Fatalf("Get 返回错误: %v", err)
	}
	if got != Adapter(second) {
		t.Error("重复注册应覆盖旧适配器")
	}
}

func TestRegistry_Providers返回全部已注册(t *testing.T) {
	reg := NewRegistry()
	if got := reg.Providers(); len(got) != 0 {
		t.Errorf("空注册表 Providers() = %v，期望空", got)
	}

	reg.Register("volc", NewVolc(map[string]string{}))
	reg.Register("sensenova", NewSenseNova(map[string]string{}))

	got := reg.Providers()
	sort.Strings(got)
	want := []string{"sensenova", "volc"}
	if len(got) != len(want) {
		t.Fatalf("Providers() = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Providers()[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}
