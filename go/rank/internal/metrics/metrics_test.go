package metrics

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func gatheredValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != len(labels) {
				continue
			}
			matched := true
			for _, label := range metric.Label {
				if labels[label.GetName()] != label.GetValue() {
					matched = false
				}
			}
			if matched {
				if metric.Counter != nil {
					return metric.Counter.GetValue(), true
				}
				return metric.Gauge.GetValue(), true
			}
		}
	}
	return 0, false
}

func TestDueDelayRequiresSuccessfulObservationAndPreservesLastOnError(t *testing.T) {
	c := newCollectors()
	registry := prometheus.NewRegistry()
	c.register(registry)
	if _, exists := gatheredValue(t, registry, "rank_reward_due_delay_seconds", nil); exists {
		t.Fatal("未查询前不能发布空队列0")
	}
	c.observeScan(10000, 0, false, errors.New("SQL不可用"))
	if _, exists := gatheredValue(t, registry, "rank_reward_due_delay_seconds", nil); exists {
		t.Fatal("首次查询失败不能伪造队列状态")
	}
	c.observeScan(10000, 2500, true, nil)
	if value, exists := gatheredValue(t, registry, "rank_reward_due_delay_seconds", nil); !exists || value != 7.5 {
		t.Fatalf("应观测真实到期延迟7.5秒，得到%v/%v", value, exists)
	}
	c.observeScan(11000, 0, false, errors.New("读超时"))
	c.observeScan(10000, 10001, true, nil)
	if value, _ := gatheredValue(t, registry, "rank_reward_due_delay_seconds", nil); value != 7.5 {
		t.Fatalf("错误或异常未来时间不得清旧值: %v", value)
	}
	if value, _ := gatheredValue(t, registry, "rank_reward_scan_total", map[string]string{"outcome": "ok"}); value != 1 {
		t.Fatalf("失败不能同时记成功: %v", value)
	}
	if value, _ := gatheredValue(t, registry, "rank_reward_scan_total", map[string]string{"outcome": "error"}); value != 3 {
		t.Fatalf("三个失败必须保留: %v", value)
	}
	c.observeScan(12000, 0, false, nil)
	if value, exists := gatheredValue(t, registry, "rank_reward_due_delay_seconds", nil); !exists || value != 0 {
		t.Fatalf("只有真实空结果才清0: %v/%v", value, exists)
	}
}

func TestMetricLabelsRejectUnboundedValues(t *testing.T) {
	c := newCollectors()
	registry := prometheus.NewRegistry()
	c.register(registry)
	if c.observeWrite("player-123", WriteOK) || c.observeWrite(OperationFreeze, "request-456") || c.observeDispatch("battle-789") {
		t.Fatal("动态身份不得成为指标label")
	}
	for _, outcome := range []string{WriteOK, WriteReplay, WriteRejected, WriteError} {
		if !c.observeWrite(OperationFreeze, outcome) {
			t.Fatal("封闭写结果被拒绝")
		}
		if value, exists := gatheredValue(t, registry, "rank_write_total", map[string]string{"operation": OperationFreeze, "outcome": outcome}); !exists || value != 1 {
			t.Fatalf("写结果%q应独立计数: %v/%v", outcome, value, exists)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		want := 0
		switch family.GetName() {
		case "rank_write_total":
			want = 20
		case "rank_reward_dispatch_total":
			want = 7
		case "rank_reward_scan_total":
			want = 2
		}
		if len(family.Metric) != want {
			t.Fatalf("%s出现意外series: %d want%d", family.GetName(), len(family.Metric), want)
		}
	}
}

// 放弃终态是封闭结果之一:注册即有 0 值序列,告警的 increase 窗口才看得到进程启动后的第一次增长。
func TestDispatchAbandonedIsPreRegisteredClosedOutcome(t *testing.T) {
	c := newCollectors()
	registry := prometheus.NewRegistry()
	c.register(registry)
	labels := map[string]string{"outcome": RewardAbandoned}
	if value, exists := gatheredValue(t, registry, "rank_reward_dispatch_total", labels); !exists || value != 0 {
		t.Fatalf("abandoned 应预建 0 值序列: %v/%v", value, exists)
	}
	if !c.observeDispatch(RewardAbandoned) {
		t.Fatal("abandoned 是封闭结果, 不得被拒")
	}
	if value, _ := gatheredValue(t, registry, "rank_reward_dispatch_total", labels); value != 1 {
		t.Fatalf("abandoned 应计 1 次: %v", value)
	}
}
