package clientplayerloginlogic

import (
	"context"
	"testing"

	"github.com/panjf2000/ants/v2"
	"github.com/prometheus/client_golang/prometheus"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"

	pbbase "proto/common/base"
	loginpb "proto/login"
	"shared/generated/pb/table"
)

// EnterGame 池满背压(PreloadPool 满、链未启动)的指标用例。
// 入场链整段跑在 PreloadPool 里,池满要有背压。
// 开服首日新号的常规背压出口(docs/design/new-player-first-enter-no-cold-read.md §6 第 21 行、R8);
// entergame_total{result="pool_saturated"} 是它唯一进 Prometheus 的信号,
// tools/scripts/stress_summarize.ps1 的 pool_saturated 列读的就是这条序列。

// enterGameSeries 在默认 registry 里找 family{result=...} 这条序列。found=false 表示序列还不存在
// (带 label 的向量在某个 label 组合第一次写入之前不输出它);value 只对计数器有意义,直方图恒为 0。
// 先 Enable:单测里 go-zero 的指标开关默认是关的,之前的写入都被丢弃(理由见 createPlayerCounterValue)。
// result 一律传字面量而不是 Result* 常量:label 值是告警表达式与汇总脚本的契约,要单独钉住。
func enterGameSeries(t *testing.T, family, result string) (value float64, found bool) {
	t.Helper()
	gzprom.Enable()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == result {
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// 池满:链根本没启动。EnterGame 回 kLoginInProgress,玩家锁由外层 defer 释放;
// entergame_total{result="pool_saturated"} +1,而且**不进** entergame_total_seconds 直方图(没有耗时可记)。
//
// 池用与生产相同的非阻塞模式(svc.NewServiceContext 里的 ants.WithNonblocking(true)),容量 1,
// 唯一的 worker 被一个等信号的任务占住。占住(等 started)与放开(close(unblock))都由用例显式控制,不靠时序。
func TestEnterGame_PoolSaturatedCountsWithoutLatencySample(t *testing.T) {
	h := newEnterWiringHarness(t, []*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}, nil)

	pool, err := ants.NewPool(1, ants.WithNonblocking(true))
	if err != nil {
		t.Fatalf("saturated preload pool: %v", err)
	}
	t.Cleanup(pool.Release)
	started := make(chan struct{})
	unblock := make(chan struct{})
	defer close(unblock)
	if err := pool.Submit(func() {
		close(started)
		<-unblock
	}); err != nil {
		t.Fatalf("occupy the only pool worker: %v", err)
	}
	<-started
	h.l.svcCtx.PreloadPool = pool

	before, _ := enterGameSeries(t, "entergame_total", "pool_saturated")

	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
	if err != nil {
		t.Fatalf("EnterGame: %v", err)
	}
	if got := resp.GetErrorMessage().GetId(); got != uint32(table.LoginError_kLoginInProgress) {
		t.Fatalf("tip = %d, want kLoginInProgress(%d) as back-pressure when the preload pool is full",
			got, uint32(table.LoginError_kLoginInProgress))
	}

	after, found := enterGameSeries(t, "entergame_total", "pool_saturated")
	if !found || after-before != 1 {
		t.Fatalf("entergame_total{result=pool_saturated}: found=%v delta=%v, want the series present with delta 1",
			found, after-before)
	}
	if _, found := enterGameSeries(t, "entergame_total_seconds", "pool_saturated"); found {
		t.Fatal("entergame_total_seconds has a pool_saturated series: a chain that never started has no latency to record")
	}
	if n, err := h.rdb.Exists(context.Background(), identityLockKey).Result(); err != nil || n != 0 {
		t.Fatalf("player lock present = %d / err %v after the pool rejected the submit, want it released", n, err)
	}
	if got := h.locator.setSessionCalls.Load(); got != 0 {
		t.Fatalf("SetSession calls = %d, want 0 (the chain must not run when the pool rejects the submit)", got)
	}
}

// 预置:PrimeEnterGameMetrics 之后 pool_saturated 这条序列必须已经存在(哪怕一次池满都没发生过)。
// 汇总脚本靠「序列在不在」区分「测到 0」与「这个 login 不导出该取值」,告警的 increase(...) 也靠它不漏第一次。
// 只断言存在、不断言等于 0:计数是进程级累加的,同包别的用例可能已经让它涨过。
func TestPrimeEnterGameMetrics_ExposesPoolSaturatedSeries(t *testing.T) {
	gzprom.Enable()
	PrimeEnterGameMetrics()

	if _, found := enterGameSeries(t, "entergame_total", "pool_saturated"); !found {
		t.Fatal("entergame_total{result=pool_saturated} missing after PrimeEnterGameMetrics")
	}
	if _, found := enterGameSeries(t, "entergame_total_seconds", "pool_saturated"); found {
		t.Fatal("priming must not create a pool_saturated series in the entergame_total_seconds histogram")
	}
}
