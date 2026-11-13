package data

// asset_cleanup_liveness_test.go —— 清理循环的活性 gauge guild_asset_cleanup_last_run_unixtime(CleanupLive;
// 缺口清单第 25 条第五批,docs/design/assetop-redelivery-sharding-20261110.md §14.8)。口径在 shared/looplive,
// 这里只测 guild 一侧的接线。纯单测,不连库。

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// CleanupLive 注册在默认注册表上,名字是 guild_asset_cleanup_last_run_unixtime:写过才有序列、值就是写入的时刻,
// 循环有意停止后删掉(没开清理、或已经停掉清理的副本不会被当成卡住)。告警 AssetOpCleanupLoopStalled 认的就是这个名字。
func TestCleanupLiveIsRegisteredUnderTheAlertedName(t *testing.T) {
	exported := func() (float64, bool) {
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		for _, mf := range families {
			if mf.GetName() == "guild_asset_cleanup_last_run_unixtime" && len(mf.GetMetric()) == 1 {
				return mf.GetMetric()[0].GetGauge().GetValue(), true
			}
		}
		return 0, false
	}
	CleanupLive.Stop()
	if _, ok := exported(); ok {
		t.Fatal("没写过时不该有活性序列")
	}
	CleanupLive.Mark(time.Unix(1234, 0))
	if got, ok := exported(); !ok || got != 1234 {
		t.Fatalf("写入之后默认注册表里应有序列且值为 1234,实际 ok=%t 值=%v", ok, got)
	}
	CleanupLive.Stop()
	if _, ok := exported(); ok {
		t.Fatal("停止之后不该有活性序列")
	}
}

// RunCleanup 一起来就写一次(首轮之前有至多一个间隔的随机延迟,不能等第一轮),ctx 取消、有意停止时删掉序列;
// 间隔非法不起循环,也不写。间隔取一小时,首轮在用例结束前不会到点,store 为 nil 也碰不到库。
func TestRunCleanupMarksLivenessAtStartAndStopsItOnExit(t *testing.T) {
	CleanupLive.Stop()
	var store *GuildAssetStore
	store.RunCleanup(context.Background(), CleanupConf{})
	if _, ok := CleanupLive.Value(); ok {
		t.Fatal("间隔非法、循环没起来时不该写活性时刻")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunCleanup(ctx, CleanupConf{Interval: time.Hour, TerminalRetention: time.Hour, CounterRetention: time.Hour})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := CleanupLive.Value(); ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("RunCleanup 起来之后应先写一次活性时刻")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if _, ok := CleanupLive.Value(); ok {
		t.Fatal("RunCleanup 停止之后应删掉活性序列")
	}
}
