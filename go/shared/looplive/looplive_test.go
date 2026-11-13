package looplive

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestGauge() (*Gauge, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	return NewRegistered(reg, prometheus.GaugeOpts{
		Name:        "test_loop_last_run_unixtime",
		Help:        "test",
		ConstLabels: prometheus.Labels{"service": "test"},
	}), reg
}

// exportedSeries 数注册表里这个 gauge 实际暴露了几条序列(/metrics 上看到的就是这个)。
func exportedSeries(t *testing.T, reg *prometheus.Registry) int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == "test_loop_last_run_unixtime" {
			return len(family.GetMetric())
		}
	}
	return 0
}

// 序列只在 Mark 与 Stop 之间存在:注册之后、Mark 之前没有序列(不是 0),Stop 之后又没有。没起循环的副本因此不会被告警当成卡住。
func TestSeriesExistsOnlyBetweenMarkAndStop(t *testing.T) {
	live, reg := newTestGauge()
	if n := exportedSeries(t, reg); n != 0 {
		t.Fatalf("Mark 之前不该有序列,实际 %d 条", n)
	}
	if _, ok := live.Value(); ok {
		t.Fatal("Mark 之前 Value 应回没有序列")
	}

	live.Mark(time.Unix(1234, 0))
	if n := exportedSeries(t, reg); n != 1 {
		t.Fatalf("Mark 之后应恰好一条序列,实际 %d 条", n)
	}
	if got := testutil.ToFloat64(live.Collector()); got != 1234 {
		t.Fatalf("暴露的值应是写入的 Unix 秒 1234,实际 %v", got)
	}
	if got, ok := live.Value(); !ok || got != 1234 {
		t.Fatalf("Value 应读到 1234,实际 %v(ok=%t)", got, ok)
	}

	live.Stop()
	if n := exportedSeries(t, reg); n != 0 {
		t.Fatalf("Stop 之后不该有序列,实际 %d 条", n)
	}
	if _, ok := live.Value(); ok {
		t.Fatal("Stop 之后 Value 应回没有序列")
	}
	if n := exportedSeries(t, reg); n != 0 {
		t.Fatalf("Value 不得顺手造出序列,实际 %d 条", n)
	}
}

// Round:正常返回的一轮写这一刻(这一轮自己处理过的失败也算跑完);panic 的一轮被兜住、不写,时刻停在上一轮。
func TestRoundMarksOnlyRoundsThatReturn(t *testing.T) {
	live, _ := newTestGauge()
	live.Mark(time.Unix(1000, 0))

	ran := false
	before := time.Now().Unix()
	if !live.Round("looplive.test", func() { ran = true }) {
		t.Fatal("正常返回的一轮应回 true")
	}
	got, ok := live.Value()
	if !ran || !ok || got < float64(before) || got > float64(time.Now().Unix()) {
		t.Fatalf("正常返回的一轮应把时刻写成现在:ran=%t ok=%t 值=%v", ran, ok, got)
	}

	live.Mark(time.Unix(1000, 0))
	if live.Round("looplive.test", func() { panic("test: poisoned row") }) {
		t.Fatal("panic 的一轮应回 false")
	}
	if got, ok := live.Value(); !ok || got != 1000 {
		t.Fatalf("panic 的一轮不写时刻,值应停在 1000,实际 %v(ok=%t)", got, ok)
	}
	if !live.Round("looplive.test", func() {}) {
		t.Fatal("panic 之后下一轮照常跑")
	}
	if got, _ := live.Value(); got <= 1000 {
		t.Fatalf("panic 之后跑完的一轮应把时刻往前写,实际 %v", got)
	}
}

// 不接指标的调用方拿到的是 nil:Mark、Stop 是空操作,Round 照常跑这一轮并兜住 panic。
func TestNilGaugeStillRunsRounds(t *testing.T) {
	var live *Gauge
	live.Mark(time.Now())
	live.Stop()
	if _, ok := live.Value(); ok {
		t.Fatal("nil gauge 没有序列")
	}
	ran := false
	if !live.Round("looplive.test", func() { ran = true }) || !ran {
		t.Fatal("nil gauge 的 Round 也要跑这一轮")
	}
	if live.Round("looplive.test", func() { panic("test") }) {
		t.Fatal("nil gauge 的 Round 同样兜住 panic 并回 false")
	}
}

// reg 为 nil 时只构造不注册;同名注册两次是装配错误,当场 panic。
func TestNewRegistered(t *testing.T) {
	unregistered := NewRegistered(nil, prometheus.GaugeOpts{Name: "test_unregistered_last_run_unixtime", Help: "test"})
	unregistered.Mark(time.Unix(7, 0))
	if got, ok := unregistered.Value(); !ok || got != 7 {
		t.Fatalf("没注册的 gauge 照样能写能读,实际 %v(ok=%t)", got, ok)
	}

	_, reg := newTestGauge()
	defer func() {
		if recover() == nil {
			t.Fatal("同名注册两次应 panic")
		}
	}()
	NewRegistered(reg, prometheus.GaugeOpts{Name: "test_loop_last_run_unixtime", Help: "test", ConstLabels: prometheus.Labels{"service": "test"}})
}
