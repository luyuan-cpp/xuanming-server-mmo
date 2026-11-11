package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

// 2026-10-30(评审 B-4 / B-6):全球终局恢复存量与终局发件箱的指标都挂进 register(),存量 / 积压按 kind 覆盖写
// (处置之后能回落),投递结局按 kind / outcome 累加;指标名带 match_ 前缀。
func TestOutcomeMetricsRegisteredAndWritten(t *testing.T) {
	register()
	SetGlobalExpiredStranded(3)
	if got := gaugeValue(t, globalExpiredStranded); got != 3 {
		t.Fatalf("stranded = %v, want 3", got)
	}
	SetGlobalExpiredStranded(0)
	if got := gaugeValue(t, globalExpiredStranded); got != 0 {
		t.Fatalf("处置之后读数应回落到 0, got %v", got)
	}
	SetOutcomeOutboxBacklog("waiting", 40, 86400)
	if got := gaugeValue(t, outcomeOutboxBacklogRows.WithLabelValues("waiting")); got != 40 {
		t.Fatalf("waiting rows = %v, want 40", got)
	}
	if got := gaugeValue(t, outcomeOutboxOldestAge.WithLabelValues("waiting")); got != 86400 {
		t.Fatalf("waiting oldest = %v, want 86400", got)
	}
	before := counterVecValue(outcomeOutboxDeliveryTotal, "first", "pending")
	ObserveOutcomeOutboxDelivery("first", "pending")
	if got := counterVecValue(outcomeOutboxDeliveryTotal, "first", "pending") - before; got != 1 {
		t.Fatalf("delivery_total{first,pending} 增量 = %v, want 1", got)
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	for _, want := range []string{
		"match_global_expired_stranded_formations", "match_outcome_outbox_backlog_rows",
		"match_outcome_outbox_oldest_pending_age_seconds", "match_outcome_outbox_delivery_total",
	} {
		if !names[want] {
			t.Errorf("默认注册表里没有 %s", want)
		}
	}
}

// outcomeOutboxLastRun 读默认注册表里的 match_outcome_outbox_last_run_unixtime;没有序列时 ok=false。
func outcomeOutboxLastRun(t *testing.T) (value float64, ok bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "match_outcome_outbox_last_run_unixtime" && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// 缺口清单第 25 条第五批:终局发件箱投递循环的活性 gauge 不预建,循环写过才有序列、值就是写入的时刻,循环退出删掉序列
// (canary 与停机中的副本没有序列,告警按副本判不会误报)。
func TestOutcomeOutboxLastRunExistsOnlyWhileTheLoopRuns(t *testing.T) {
	register()
	ClearOutcomeOutboxRun()
	if _, ok := outcomeOutboxLastRun(t); ok {
		t.Fatal("循环没起来(或已退出)时不该有活性序列")
	}
	MarkOutcomeOutboxRun(time.Unix(1234, 0))
	if got, ok := outcomeOutboxLastRun(t); !ok || got != 1234 {
		t.Fatalf("写入之后应有序列且值为 1234,实际 ok=%t 值=%v", ok, got)
	}
	ClearOutcomeOutboxRun()
	if _, ok := outcomeOutboxLastRun(t); ok {
		t.Fatal("循环退出之后应删掉活性序列")
	}
}
