package metrics

import (
	"testing"

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
