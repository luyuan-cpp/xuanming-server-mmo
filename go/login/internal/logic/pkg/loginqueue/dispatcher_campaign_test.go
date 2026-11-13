package loginqueue

import (
	"testing"
	"time"

	"shared/leader"
)

// 没当选时的竞选间隔不跟着锁 TTL 走(campaignRetry):缺省 30s 的 TTL 下,现任让位之后最迟 2s 就有新的放行者,
// 而不是 TTL/3 = 10s。毫秒级 TTL 的单测仍按 1s 下限,与任期内 panic 之后的退避同口径。
func TestCampaignRetryIsDecoupledFromTheLockTTL(t *testing.T) {
	cases := []struct {
		lockTTL time.Duration
		want    time.Duration
	}{
		{30 * time.Second, leader.DefaultRetryPeriod},
		{6 * time.Second, leader.DefaultRetryPeriod},
		{4500 * time.Millisecond, 1500 * time.Millisecond},
		{300 * time.Millisecond, time.Second},
	}
	for _, tc := range cases {
		if got := campaignRetry(tc.lockTTL); got != tc.want {
			t.Errorf("campaignRetry(%v) = %v, want %v", tc.lockTTL, got, tc.want)
		}
	}
	if leader.DefaultRetryPeriod != 2*time.Second {
		t.Fatalf("leader.DefaultRetryPeriod = %v; the 30s-TTL case above assumes 2s", leader.DefaultRetryPeriod)
	}
}
