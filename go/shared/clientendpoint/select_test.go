package clientendpoint

import (
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// 计数器是进程级全局量,本文件的用例一律不开 t.Parallel,只断言前后差值。
type counterSnapshot struct {
	client, fallback, rejected, deduped float64
}

func snapshotCounters() counterSnapshot {
	return counterSnapshot{
		client:   testutil.ToFloat64(clientCounter),
		fallback: testutil.ToFloat64(fallbackCounter),
		rejected: testutil.ToFloat64(rejectedCounter),
		deduped:  testutil.ToFloat64(dedupedCounter),
	}
}

func (s counterSnapshot) delta(before counterSnapshot) counterSnapshot {
	return counterSnapshot{
		client:   s.client - before.client,
		fallback: s.fallback - before.fallback,
		rejected: s.rejected - before.rejected,
		deduped:  s.deduped - before.deduped,
	}
}

// ep 是测试里的一对 host/port;零值 = 未填。
type ep struct {
	host string
	port uint32
}

var (
	// podEP 是集群内身份(PodIP:RPC_PORT),只在回落时才会被下发。
	podEP       = ep{"10.244.0.5", 18000}
	noEP        = ep{}
	oneClient   = counterSnapshot{client: 1}
	oneFallback = counterSnapshot{fallback: 1}
	oneRejected = counterSnapshot{rejected: 1}
)

func TestSelect(t *testing.T) {
	tests := []struct {
		name     string
		client   ep
		endpoint ep
		require  bool
		want     ep
		wantOK   bool
		metric   counterSnapshot
	}{
		{"有客户端地址:用它(不要求)", ep{"127.0.0.1", 30000}, podEP, false, ep{"127.0.0.1", 30000}, true, oneClient},
		{"有客户端地址:用它(要求)", ep{"gate-1.z1.example.com", 18000}, podEP, true, ep{"gate-1.z1.example.com", 18000}, true, oneClient},
		{"缺客户端地址且不要求:回落 endpoint", noEP, podEP, false, podEP, true, oneFallback},
		{"缺客户端地址且要求:跳过,不回落 PodIP", noEP, podEP, true, noEP, false, oneRejected},
		{"半填(只有 host)视为缺失:回落", ep{"127.0.0.1", 0}, podEP, false, podEP, true, oneFallback},
		{"半填(只有 port)视为缺失:回落", ep{"", 30000}, podEP, false, podEP, true, oneFallback},
		{"半填且要求:跳过", ep{"127.0.0.1", 0}, podEP, true, noEP, false, oneRejected},
		{"端口越界(65536)视为缺失:回落", ep{"127.0.0.1", 65536}, podEP, false, podEP, true, oneFallback},
		{"端口上界 65535 可用", ep{"127.0.0.1", 65535}, podEP, false, ep{"127.0.0.1", 65535}, true, oneClient},
		{"回落时 endpoint 也不可用:跳过", noEP, ep{"", 18000}, false, noEP, false, oneRejected},
		{"两个地址都空:跳过", noEP, noEP, false, noEP, false, oneRejected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotCounters()
			host, port, ok := Select(tt.client.host, tt.client.port, tt.endpoint.host, tt.endpoint.port, tt.require)
			if host != tt.want.host || port != tt.want.port || ok != tt.wantOK {
				t.Fatalf("Select() = (%q, %d, %v), want (%q, %d, %v)",
					host, port, ok, tt.want.host, tt.want.port, tt.wantOK)
			}
			if got := snapshotCounters().delta(before); got != tt.metric {
				t.Fatalf("计数器增量 = %+v, want %+v", got, tt.metric)
			}
		})
	}
}

type record struct {
	name   string
	addr   string
	launch uint64
}

func dedupe(items []record) []record {
	return DedupeNewest(items,
		func(r record) string { return r.addr },
		func(r record) uint64 { return r.launch })
}

func names(items []record) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.name)
	}
	return out
}

func TestDedupeNewest(t *testing.T) {
	tests := []struct {
		name        string
		items       []record
		wantNames   []string
		wantDeduped float64
	}{
		{
			name:      "空集合",
			items:     nil,
			wantNames: []string{},
		},
		{
			name:      "单条",
			items:     []record{{"a", "h:1", 1}},
			wantNames: []string{"a"},
		},
		{
			name:      "无重复:原样保留顺序",
			items:     []record{{"a", "h:1", 5}, {"b", "h:2", 1}, {"c", "h:3", 9}},
			wantNames: []string{"a", "b", "c"},
		},
		{
			name: "崩溃 gate 的陈旧记录:只留 launch_time 最大者(新记录在后)",
			items: []record{
				{"old-gate1", "127.0.0.1:30001", 100},
				{"gate0", "127.0.0.1:30000", 150},
				{"new-gate1", "127.0.0.1:30001", 200},
			},
			wantNames:   []string{"gate0", "new-gate1"},
			wantDeduped: 1,
		},
		{
			name: "新记录在前:仍留最大者,且保持它原来的位置",
			items: []record{
				{"new-gate1", "127.0.0.1:30001", 200},
				{"gate0", "127.0.0.1:30000", 150},
				{"old-gate1", "127.0.0.1:30001", 100},
			},
			wantNames:   []string{"new-gate1", "gate0"},
			wantDeduped: 1,
		},
		{
			name: "launch_time 并列:留先出现的",
			items: []record{
				{"first", "h:1", 7},
				{"second", "h:1", 7},
			},
			wantNames:   []string{"first"},
			wantDeduped: 1,
		},
		{
			name: "同一地址三条:只留一条,计 2",
			items: []record{
				{"x1", "h:1", 1},
				{"y", "h:2", 1},
				{"x3", "h:1", 3},
				{"x2", "h:1", 2},
			},
			wantNames:   []string{"y", "x3"},
			wantDeduped: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]record(nil), tt.items...)
			before := snapshotCounters()

			got := dedupe(input)

			if gotNames := names(got); !reflect.DeepEqual(gotNames, tt.wantNames) {
				t.Fatalf("DedupeNewest() = %v, want %v", gotNames, tt.wantNames)
			}
			if !reflect.DeepEqual(input, tt.items) {
				t.Fatalf("入参被修改: %v, want %v", input, tt.items)
			}
			if d := snapshotCounters().delta(before); d != (counterSnapshot{deduped: tt.wantDeduped}) {
				t.Fatalf("计数器增量 = %+v, want 仅 deduped=%v", d, tt.wantDeduped)
			}
		})
	}
}

// 指标名与 label 是运维 / 告警规则的对外契约(ingress 设计 §3「低基数指标」),改名要同步告警。
func TestSelectMetricContract(t *testing.T) {
	Select("127.0.0.1", 30000, "", 0, false) // 触发惰性注册

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "mmorpg_client_endpoint_select_total" {
			continue
		}
		results := map[string]bool{}
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != 1 || m.GetLabel()[0].GetName() != "result" {
				t.Fatalf("label 集合应只有 result,实际 %v", m.GetLabel())
			}
			results[m.GetLabel()[0].GetValue()] = true
		}
		for _, want := range []string{"client", "fallback", "rejected", "deduped"} {
			if !results[want] {
				t.Errorf("缺少 result=%q 的序列(已有 %v)", want, results)
			}
		}
		if len(results) != 4 {
			t.Errorf("result 取值应恰好 4 个,实际 %v", results)
		}
		return
	}
	t.Fatal("默认注册表里没有 mmorpg_client_endpoint_select_total")
}
