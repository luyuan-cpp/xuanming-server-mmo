package battleplace

import (
	"maps"
	"testing"
	"time"

	"match/internal/discovery"

	base "proto/common/base"
)

// node 造一条没上报可分配状态（UNSPECIFIED）的登记，与旧版本 battle 的注册值相同。
func node(id uint32, uuid string, region uint32) discovery.NodeEntry {
	return discovery.NodeEntry{NodeId: id, NodeUuid: uuid, PhysicalRegion: region, Endpoint: uuid + ":1"}
}

// withAdmission 给登记填上节点自报的可分配状态。
func withAdmission(entry discovery.NodeEntry, admission base.NodeAdmission) discovery.NodeEntry {
	entry.Admission = admission
	return entry
}

func openNode(id uint32, uuid string) discovery.NodeEntry {
	return withAdmission(node(id, uuid, 1), base.NodeAdmission_NODE_ADMISSION_OPEN)
}

func closedNode(id uint32, uuid string) discovery.NodeEntry {
	return withAdmission(node(id, uuid, 1), base.NodeAdmission_NODE_ADMISSION_CLOSED)
}

// fixedPicks 依次返回预设的随机数，便于确定性地覆盖两选一。
func fixedPicks(values ...int) func(int) int {
	i := 0
	return func(n int) int {
		v := values[i%len(values)] % n
		i++
		return v
	}
}

func TestPickPrefersLessLoadedOfTwo(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := NewWith(func() time.Time { return clock }, fixedPicks(0, 0), time.Minute)
	nodes := []discovery.NodeEntry{node(1, "a", 1), node(2, "b", 1), node(3, "c", 2)}
	p.Started("a")
	p.Started("a")
	got, ok := p.Pick(nodes, 1)
	if !ok || got.NodeUuid != "b" {
		t.Fatalf("应选在途较少的 b, got %+v", got)
	}
	p.Finished("a")
	p.Finished("a")
	got, _ = p.Pick(nodes, 1)
	if got.NodeUuid != "a" {
		t.Fatalf("在途相同取 NodeId 较小者, got %+v", got)
	}
}

func TestPickOnlyWithinRegionAndSkipsBroken(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := NewWith(func() time.Time { return clock }, fixedPicks(0, 0), time.Minute)
	nodes := []discovery.NodeEntry{node(1, "a", 1), node(2, "b", 1), node(3, "c", 2)}
	if got, ok := p.Pick(nodes, 2); !ok || got.NodeUuid != "c" {
		t.Fatalf("只能在目标机房里选, got %+v", got)
	}
	p.MarkUnavailable("a")
	for i := 0; i < 5; i++ {
		if got, _ := p.Pick(nodes, 1); got.NodeUuid != "b" {
			t.Fatalf("熔断中的节点不应被选中, got %+v", got)
		}
	}
	p.MarkUnavailable("b")
	if _, ok := p.Pick(nodes, 1); ok {
		t.Fatal("机房内全部熔断时应选不出节点")
	}
	if regions := p.Regions(nodes); regions[1] || !regions[2] {
		t.Fatalf("全部熔断的机房不应可放置: %v", regions)
	}
	clock = clock.Add(time.Minute)
	if regions := p.Regions(nodes); !regions[1] {
		t.Fatal("熔断到期后应恢复")
	}
}

func TestPickSkipsIncompleteRegistrations(t *testing.T) {
	p := New()
	nodes := []discovery.NodeEntry{{NodeId: 1, PhysicalRegion: 1, Endpoint: "x:1"}, {NodeId: 2, NodeUuid: "b", Endpoint: "y:1"}}
	if _, ok := p.Pick(nodes, 1); ok {
		t.Fatal("缺 UUID 或机房的注册不能参与选点")
	}
	if len(p.Regions(nodes)) != 0 {
		t.Fatal("缺 UUID 或机房的注册不能算作可放置机房")
	}
}

func TestFinishedNeverGoesNegative(t *testing.T) {
	p := New()
	p.Finished("ghost")
	p.Started("a")
	p.Finished("a")
	p.Finished("a")
	if len(p.inflight) != 0 {
		t.Fatalf("在途计数不应残留或为负: %v", p.inflight)
	}
}

// 可分配状态与负载（R6）。fixedPicks(0, 0) 让两选一恒比较机房里前两台可选节点（按 nodes 的顺序）。
func TestPickByAdmissionAndLoad(t *testing.T) {
	cases := []struct {
		name    string
		nodes   []discovery.NodeEntry
		prepare func(p *Picker, advance func(time.Duration))
		want    string // 期望选中的实例；空 = 应当选不出、机房也不可放置
	}{
		{
			name:  "CLOSED 不可选",
			nodes: []discovery.NodeEntry{closedNode(1, "a"), openNode(2, "b")},
			want:  "b",
		},
		{
			name:  "UNSPECIFIED 视为可选",
			nodes: []discovery.NodeEntry{node(1, "a", 1), closedNode(2, "b")},
			want:  "a",
		},
		{
			name:  "全部 CLOSED 选不出",
			nodes: []discovery.NodeEntry{closedNode(1, "a"), closedNode(2, "b")},
		},
		{
			name:  "新鲜读数的房间数少者优先",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.Observe("a", 10)
				p.Observe("b", 3)
			},
			want: "b",
		},
		{
			name:  "房间数与本副本在途相加比较",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.Observe("a", 2)
				p.Observe("b", 1)
				p.Started("b")
				p.Started("b")
			},
			want: "a",
		},
		{
			name:  "读数过期前仍按读数",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, advance func(time.Duration)) {
				p.Observe("a", 50)
				advance(DefaultReportTTL - time.Second)
				p.Started("b")
			},
			want: "b",
		},
		{
			name:  "读数过期回落在途，旧的高读数不让节点一直被避开",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, advance func(time.Duration)) {
				p.Observe("a", 50)
				advance(DefaultReportTTL)
				p.Started("b")
			},
			want: "a",
		},
		{
			name:  "同负载取 NodeId 小者",
			nodes: []discovery.NodeEntry{openNode(5, "a"), openNode(3, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.Observe("a", 4)
				p.Observe("b", 4)
			},
			want: "b",
		},
		{
			name:  "未上报的 0 不覆盖旧读数",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.Observe("a", 5)
				p.Observe("a", 0)
				p.Observe("b", 3)
			},
			want: "b",
		},
		{
			name:  "熔断仍生效，读数再低也不选",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.Observe("a", 1)
				p.Observe("b", 100)
				p.MarkUnavailable("a")
			},
			want: "b",
		},
		{
			name:  "熔断到期后恢复参与比较",
			nodes: []discovery.NodeEntry{openNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, advance func(time.Duration)) {
				p.MarkUnavailable("a")
				advance(time.Minute)
				p.Observe("b", 100)
			},
			want: "a",
		},
		{
			name:  "CLOSED 与熔断叠加选不出",
			nodes: []discovery.NodeEntry{closedNode(1, "a"), openNode(2, "b")},
			prepare: func(p *Picker, _ func(time.Duration)) {
				p.MarkUnavailable("b")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Unix(1000, 0)
			p := NewWith(func() time.Time { return clock }, fixedPicks(0, 0), time.Minute)
			if tc.prepare != nil {
				tc.prepare(p, func(d time.Duration) { clock = clock.Add(d) })
			}
			got, ok := p.Pick(tc.nodes, 1)
			placeable := p.Regions(tc.nodes)[1]
			if tc.want == "" {
				if ok || placeable {
					t.Fatalf("应当选不出节点且机房不可放置, got %+v ok=%v placeable=%v", got, ok, placeable)
				}
				return
			}
			if !ok || got.NodeUuid != tc.want {
				t.Fatalf("应选 %s, got %+v ok=%v", tc.want, got, ok)
			}
			if !placeable {
				t.Fatal("有可选节点的机房应可放置")
			}
		})
	}
}

func TestCensusCountsEveryState(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := NewWith(func() time.Time { return clock }, fixedPicks(0), time.Minute)
	nodes := []discovery.NodeEntry{
		openNode(1, "open"),
		closedNode(2, "closed"),
		openNode(3, "cooling"),
		closedNode(4, "closed-and-cooling"), // 自报 CLOSED 优先于本地熔断
		node(5, "unspecified", 2),
		{NodeId: 6, PhysicalRegion: 1, Endpoint: "x:1"}, // 缺实例 UUID
	}
	p.MarkUnavailable("cooling")
	p.MarkUnavailable("closed-and-cooling")
	want := map[string]int{StateOpen: 1, StateClosed: 2, StateCooldown: 1, StateUnspecified: 1, StateInvalid: 1}
	if got := p.Census(nodes); !maps.Equal(got, want) {
		t.Fatalf("按状态计数不符: got %v want %v", got, want)
	}
	// 没有节点时也要带全部状态（全 0），否则归零的状态在指标里停在旧值。
	empty := map[string]int{StateOpen: 0, StateClosed: 0, StateCooldown: 0, StateUnspecified: 0, StateInvalid: 0}
	if got := p.Census(nil); !maps.Equal(got, empty) {
		t.Fatalf("空列表应返回全部状态且为 0: got %v", got)
	}
	// 熔断到期后回到自报状态。
	clock = clock.Add(time.Minute)
	if got := p.Census(nodes)[StateCooldown]; got != 0 {
		t.Fatalf("熔断到期后不应再计入 cooldown: got %d", got)
	}
}
