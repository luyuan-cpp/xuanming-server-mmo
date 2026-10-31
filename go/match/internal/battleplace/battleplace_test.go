package battleplace

import (
	"testing"
	"time"

	"match/internal/discovery"
)

func node(id uint32, uuid string, region uint32) discovery.NodeEntry {
	return discovery.NodeEntry{NodeId: id, NodeUuid: uuid, PhysicalRegion: region, Endpoint: uuid + ":1"}
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
