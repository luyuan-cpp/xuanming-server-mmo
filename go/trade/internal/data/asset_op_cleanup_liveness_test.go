package data

// asset_op_cleanup_liveness_test.go —— 终态清理循环的活性 gauge trade_assetop_cleanup_last_run_unixtime
// (缺口清单第 25 条第五批,docs/design/assetop-redelivery-sharding-20261110.md §14.8)。纯单测,不连库。

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// cleanupLastRunValue 读默认注册表里的活性 gauge;没有序列(或读不出)时 ok=false。
func cleanupLastRunValue() (float64, bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range families {
		if mf.GetName() == "trade_assetop_cleanup_last_run_unixtime" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// 不预建:写过才有序列、值就是写入的时刻;循环退出删掉序列(没开清理、或已经停掉清理的副本不会被当成卡住)。
func TestCleanupLastRunExistsOnlyWhileMarked(t *testing.T) {
	ClearCleanupRun()
	if _, ok := cleanupLastRunValue(); ok {
		t.Fatal("没写过时不该有活性序列")
	}
	MarkCleanupRun(time.Unix(1234, 0))
	if got, ok := cleanupLastRunValue(); !ok || got != 1234 {
		t.Fatalf("写入之后应有序列且值为 1234,实际 ok=%t 值=%v", ok, got)
	}
	ClearCleanupRun()
	if _, ok := cleanupLastRunValue(); ok {
		t.Fatal("清掉之后不该有活性序列")
	}
}

// RunCleanup 一起来就写一次(首轮之前有至多一个间隔的随机延迟,不能等第一轮),ctx 取消、循环退出时删掉序列;
// 配置非法不起循环,也不写。间隔取一小时,首轮在用例结束前不会到点,repo 为 nil 也碰不到库。
func TestRunCleanupMarksLivenessAtStartAndClearsItOnExit(t *testing.T) {
	ClearCleanupRun()
	var repo *AssetOpRepo
	repo.RunCleanup(context.Background(), CleanupConf{})
	if _, ok := cleanupLastRunValue(); ok {
		t.Fatal("配置非法、循环没起来时不该写活性时刻")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		repo.RunCleanup(ctx, CleanupConf{Interval: time.Hour, TerminalRetention: time.Hour})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := cleanupLastRunValue(); ok {
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
	if _, ok := cleanupLastRunValue(); ok {
		t.Fatal("RunCleanup 退出之后应删掉活性序列")
	}
}
