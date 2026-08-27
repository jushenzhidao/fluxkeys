package quota

import (
	"testing"
	"time"
)

// P0-3 回归测试: 配额日必须与火山 12:00 刷新对齐，而非自然日 00:00。
func TestQuotaDay_AlignsToRefreshHour(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"刷新前一刻仍属前一配额日", time.Date(2026, 8, 23, 11, 59, 59, 0, loc), "20260822"},
		{"刷新时刻进入新配额日", time.Date(2026, 8, 23, 12, 0, 0, 0, loc), "20260823"},
		{"下午属当日", time.Date(2026, 8, 23, 18, 30, 0, 0, loc), "20260823"},
		{"午夜后仍属前一配额日", time.Date(2026, 8, 24, 0, 0, 1, 0, loc), "20260823"},
		{"凌晨仍属前一配额日", time.Date(2026, 8, 24, 6, 0, 0, 0, loc), "20260823"},
		{"跨月边界", time.Date(2026, 9, 1, 3, 0, 0, 0, loc), "20260831"},
		{"跨年边界", time.Date(2026, 1, 1, 2, 0, 0, 0, loc), "20251231"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := QuotaDay(c.at); got != c.want {
				t.Errorf("QuotaDay(%s) = %s, want %s", c.at.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

// 这是 V3 缺陷的直接复现: 若按自然日切分，00:00-12:00 会误判为新配额日。
func TestQuotaDay_DiffersFromCalendarDayInMorningWindow(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 8, 24, 9, 0, 0, 0, loc)

	calendar := at.Format("20060102") // V3 的错误做法
	quota := QuotaDay(at)

	if calendar == quota {
		t.Fatal("上午 9 点配额日不应等于自然日，否则会按满额度调度导致超刷")
	}
	if quota != "20260823" {
		t.Errorf("QuotaDay = %s, want 20260823", quota)
	}
}

func TestQuotaDayBoundaries(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 8, 24, 3, 0, 0, 0, loc)

	start := QuotaDayStart(at)
	end := QuotaDayEnd(at)

	wantStart := time.Date(2026, 8, 23, 12, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 8, 24, 12, 0, 0, 0, loc)

	if !start.Equal(wantStart) {
		t.Errorf("start = %s, want %s", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %s, want %s", end, wantEnd)
	}
	if !at.After(start) || !at.Before(end) {
		t.Error("当前时刻应落在配额日区间内")
	}
}

// TTL 必须覆盖到配额日结束后 24h，消除 V3「次日 12:30」的两义性。
func TestKeyTTL_CoversQuotaDayPlusGrace(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 8, 23, 13, 0, 0, 0, loc)

	ttl := KeyTTL(at)
	expiry := at.Add(ttl)
	want := time.Date(2026, 8, 25, 12, 0, 0, 0, loc)

	if !expiry.Equal(want) {
		t.Errorf("过期时刻 = %s, want %s", expiry, want)
	}
	if ttl <= 24*time.Hour {
		t.Errorf("TTL 应长于一个配额日, got %s", ttl)
	}
}
