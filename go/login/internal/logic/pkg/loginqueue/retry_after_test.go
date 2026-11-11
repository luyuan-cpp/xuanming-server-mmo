package loginqueue

import (
	"testing"
	"time"
)

func TestRetryAfterForRank_ScalesWithRankAndCaps(t *testing.T) {
	const base = uint32(2000)
	admitTTL := 60 * time.Second
	cases := []struct {
		rank uint32
		want uint32
	}{
		{0, base},
		{RetryAfterFullRateRanks - 1, base},
		{RetryAfterFullRateRanks, 2001}, // 2000 × 2001 / 2000:接上前一段,不跳变
		{3999, 4000},
		{9999, 10000},
		{29999, MaxRetryAfterMs},
		{100000, MaxRetryAfterMs},
		{^uint32(0), MaxRetryAfterMs}, // 乘法在 uint64 里做,不溢出
	}
	for _, c := range cases {
		if got := RetryAfterForRank(base, c.rank, admitTTL); got != c.want {
			t.Errorf("RetryAfterForRank(%d, rank=%d, %v) = %d, want %d", base, c.rank, admitTTL, got, c.want)
		}
	}
}

func TestRetryAfterForRank_NeverReachesAdmitTTL(t *testing.T) {
	// 放行槽只等 AdmitTTL:上限取它的一半,最坏情况下客户端还剩一半时间去取。
	for _, ttl := range []time.Duration{10 * time.Second, 20 * time.Second, 60 * time.Second, 5 * time.Minute} {
		got := RetryAfterForRank(2000, 1_000_000, ttl)
		limit := uint32(ttl/time.Millisecond) / 2
		if limit > MaxRetryAfterMs {
			limit = MaxRetryAfterMs
		}
		if limit < 2000 {
			limit = 2000
		}
		if got != limit {
			t.Errorf("AdmitTTL=%v: got %dms, want %dms", ttl, got, limit)
		}
		if time.Duration(got)*time.Millisecond > ttl/2 && got != 2000 {
			t.Errorf("AdmitTTL=%v: interval %dms is more than half of AdmitTTL", ttl, got)
		}
	}
}

func TestRetryAfterForRank_NeverBelowBase(t *testing.T) {
	// AdmitTTL 配得比 base 还短(1ms 是 ValidateTTLs 允许的下限)时,不把间隔压到 base 以下。
	if got := RetryAfterForRank(2000, 50000, time.Millisecond); got != 2000 {
		t.Errorf("tiny AdmitTTL: got %d, want base 2000", got)
	}
	// base 为 0(配置没写)原样回 0,客户端按自己的缺省间隔。
	if got := RetryAfterForRank(0, 50000, time.Minute); got != 0 {
		t.Errorf("zero base: got %d, want 0", got)
	}
}
