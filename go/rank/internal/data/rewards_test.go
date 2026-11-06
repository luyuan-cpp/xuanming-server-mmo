package data

import (
	"testing"
	"time"
)

// 奖励邮件可投窗口 = min(30 天, 邮件个人邮件最大有效期);有效期按邮件自己的允许范围 1~365 天校验,越界即拒。
// 窗口还要不短于 2 天:发件时至少剩 24 小时可领(MinRewardMailClaimWindow),截止点之前再留出同样长的发件期。
// 邮件有效期配成 1 天时截止点就是封榜时刻,一封都发不出,必须拒启。
func TestRewardMailWindowCapsAtThirtyDaysAndRejectsOutOfRangeTtl(t *testing.T) {
	const day = 24 * time.Hour
	for _, c := range []struct {
		days uint32
		want time.Duration
	}{{2, 2 * day}, {7, 7 * day}, {30, 30 * day}, {31, 30 * day}, {365, 30 * day}} {
		got, err := RewardMailWindow(c.days)
		if err != nil || got != c.want {
			t.Fatalf("有效期 %d 天: 窗口 %v err=%v, want %v", c.days, got, err, c.want)
		}
	}
	for _, days := range []uint32{0, 1, 366} {
		if _, err := RewardMailWindow(days); err == nil {
			t.Fatalf("有效期 %d 天超出允许范围(邮件 1~365 天, 奖励窗口不短于 2 天), 必须拒绝", days)
		}
	}
	if MinRewardMailClaimWindow != day || minRewardMailWindow != 2*day {
		t.Fatalf("最短可领时长 %v、窗口下限 %v, want 24h 与 48h", MinRewardMailClaimWindow, minRewardMailWindow)
	}
}

// 没配置的仓储取产品上限 30 天(与邮件缺省有效期相同);配置只接受 [2 天, 30 天],拒绝的值不生效。
func TestConfigureRewardMailWindowKeepsOnlyValidWindow(t *testing.T) {
	r := New(nil, nil, nil, nil, nil)
	if r.rewardMailWindow != MaxRewardMailWindow {
		t.Fatalf("缺省窗口 %v, want %v", r.rewardMailWindow, MaxRewardMailWindow)
	}
	for _, bad := range []time.Duration{0, -time.Hour, minRewardMailWindow - time.Millisecond, MaxRewardMailWindow + time.Millisecond} {
		if err := r.ConfigureRewardMailWindow(bad); err == nil {
			t.Fatalf("窗口 %v 必须拒绝", bad)
		}
	}
	if r.rewardMailWindow != MaxRewardMailWindow {
		t.Fatalf("拒绝的配置不得生效: %v", r.rewardMailWindow)
	}
	if err := r.ConfigureRewardMailWindow(7 * 24 * time.Hour); err != nil || r.rewardMailWindow != 7*24*time.Hour {
		t.Fatalf("合法窗口应生效: %v err=%v", r.rewardMailWindow, err)
	}
	if err := r.ConfigureRewardMailWindow(minRewardMailWindow); err != nil || r.rewardMailWindow != minRewardMailWindow {
		t.Fatalf("下限本身合法: %v err=%v", r.rewardMailWindow, err)
	}
}
