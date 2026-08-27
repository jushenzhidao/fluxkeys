package quota

import "time"

// RefreshHour 是火山侧配额刷新的起始小时（12:00-13:00 刷新）。
const RefreshHour = 12

// QuotaDay 返回时刻 t 所属的「配额日」。
//
// P0-3: 火山配额在 12:00 刷新，而自然日在 00:00 翻新。若直接用自然日做
// 存储后缀，00:00-12:00 这 12 小时系统会写入一个「全新的满额度」记录，
// 但火山侧此时仍是昨日剩余额度 —— 调度器按满额度选 Key 必然超刷。
//
// 因此 12:00 之前的时刻归属于前一个自然日。
func QuotaDay(t time.Time) string {
	return QuotaDayTime(t).Format("20060102")
}

// QuotaDayTime 返回配额日对应的日期（当地时区零点）。
func QuotaDayTime(t time.Time) time.Time {
	if t.Hour() < RefreshHour {
		t = t.AddDate(0, 0, -1)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// QuotaDayStart 返回配额日的起始时刻（该自然日 12:00）。
func QuotaDayStart(t time.Time) time.Time {
	d := QuotaDayTime(t)
	return time.Date(d.Year(), d.Month(), d.Day(), RefreshHour, 0, 0, 0, d.Location())
}

// QuotaDayEnd 返回配额日的结束时刻（次日 12:00）。
func QuotaDayEnd(t time.Time) time.Time {
	return QuotaDayStart(t).AddDate(0, 0, 1)
}

// KeyTTL 返回配额 Key 应设置的存活时长。
//
// P0-3: V3 中「TTL 次日 12:30」存在两义性。此处钉死为
// 「该配额日结束后再保留 24h」，用于刷新窗口内的对账与回溯。
func KeyTTL(now time.Time) time.Duration {
	return QuotaDayEnd(now).Add(24 * time.Hour).Sub(now)
}
