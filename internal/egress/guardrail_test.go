package egress

import (
	"testing"
	"time"
)

// 本文件守「封禁/撤离前的护栏」：不许把最后一个可用出口封掉。
//
// 来源是一次实测（2026-09-14，node-064 两个出口）：两个出口同时达到封禁阈值时，
// EvacuateIP 照样先 MarkBanned 再迁移，而外面没有接收方 ⇒ active=0 +
// moved=0 / failed=3，整池不可用。调用方必须先问 OtherAssignable。

func TestOtherAssignable_两个出口都可用时为真(t *testing.T) {
	p, err := NewPool(ModeMultiIP, []*IP{
		NewIP("10.0.0.1", "", 10),
		NewIP("10.0.0.2", "", 10),
	}, time.Second)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	if !p.OtherAssignable("10.0.0.1") {
		t.Error("另一个出口可用，应返回 true（可以封禁 10.0.0.1）")
	}
}

func TestOtherAssignable_只剩自己时为假(t *testing.T) {
	other := NewIP("10.0.0.2", "", 10)
	other.MarkBanned() // 另一个出口已经不可用

	p, err := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.1", "", 10), other}, time.Second)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	if p.OtherAssignable("10.0.0.1") {
		t.Error("除自己外没有可用出口，必须是 false —— 否则会封光整池")
	}
}

func TestOtherAssignable_信誉跌破线也算不可用(t *testing.T) {
	// Assignable 判的是「active 且信誉 >= 50」，不是「非 banned」:
	// 一个信誉跌破线的出口同样接不住迁过来的 Key，不能算退路。
	weak := NewIP("10.0.0.2", "", 10)
	for i := 0; i < 60; i++ {
		weak.MarkFailure()
	}
	if weak.Assignable() {
		t.Fatalf("前置不成立：该出口信誉应已跌破线（reputation=%d）", weak.Reputation())
	}
	p, err := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.1", "", 10), weak}, time.Second)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	if p.OtherAssignable("10.0.0.1") {
		t.Error("信誉跌破线的出口不算退路，应返回 false")
	}
}

func TestOtherAssignable_单出口时永远为假(t *testing.T) {
	p, err := NewPool(ModeMultiIP, []*IP{NewIP("10.0.0.1", "", 10)}, time.Second)
	if err != nil {
		t.Fatalf("建池: %v", err)
	}
	if p.OtherAssignable("10.0.0.1") {
		t.Error("池里只有一个出口时，封它没有退路 ⇒ 必须 false")
	}
}
