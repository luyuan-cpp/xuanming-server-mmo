package logic

import (
	"errors"
	"testing"
)

func viewPairs(gates ...string) []gateSlot {
	pairs := make([]gateSlot, 0, len(gates))
	for _, gate := range gates {
		pairs = append(pairs, gateSlot{gate: gate})
	}
	return pairs
}

func liveSet(gates ...string) map[string]struct{} {
	live := make(map[string]struct{}, len(gates))
	for _, gate := range gates {
		live[gate] = struct{}{}
	}
	return live
}

// etcd 回档、重建或连错时 Range 成功回空:不能据此判全部 gate 死亡。
func TestCheckLiveView_EmptySetIsAlwaysHeld(t *testing.T) {
	s := &gateSweeper{}
	for round := 0; round < liveViewDropHoldRounds+5; round++ {
		if err := s.checkLiveView(viewPairs("a"), liveSet()); !errors.Is(err, errLiveViewEmpty) {
			t.Fatalf("round %d: err = %v, want errLiveViewEmpty", round, err)
		}
	}
	if err := s.checkLiveView(nil, liveSet()); err != nil {
		t.Fatalf("没有登记 gate 时空集是正常的: %v", err)
	}
}

// 超过一半同时消失先压住,上限之后照常收尾:真有这么多 gate 死了,不能永远不释放会话。
func TestCheckLiveView_DropIsHeldThenAccepted(t *testing.T) {
	s := &gateSweeper{}
	pairs := viewPairs("a", "b", "c", "d")
	live := liveSet("a")
	for round := 0; round < liveViewDropHoldRounds; round++ {
		if err := s.checkLiveView(pairs, live); !errors.Is(err, errLiveViewDrop) {
			t.Fatalf("round %d: err = %v, want errLiveViewDrop", round, err)
		}
	}
	if err := s.checkLiveView(pairs, live); err != nil {
		t.Fatalf("压满之后应放行: %v", err)
	}
	if err := s.checkLiveView(pairs, liveSet("a", "b", "c", "d")); err != nil {
		t.Fatalf("恢复之后应放行: %v", err)
	}
	if err := s.checkLiveView(pairs, live); !errors.Is(err, errLiveViewDrop) {
		t.Fatalf("恢复之后再次骤降要重新压住: %v", err)
	}
}

// 小集群挂一台、大集群挂不到一半都是日常,不压。同一 gate 在多个 slot 上只算一次。
func TestCheckLiveView_OrdinaryLossPasses(t *testing.T) {
	s := &gateSweeper{}
	if err := s.checkLiveView(viewPairs("a", "b"), liveSet("a")); err != nil {
		t.Fatalf("两台挂一台: %v", err)
	}
	if err := s.checkLiveView(viewPairs("a", "b", "c", "d"), liveSet("a", "b")); err != nil {
		t.Fatalf("四台挂两台不超过一半: %v", err)
	}
	if err := s.checkLiveView(viewPairs("a", "a", "a", "a", "b", "c"), liveSet("a", "b")); err != nil {
		t.Fatalf("按 gate 去重后三台挂一台: %v", err)
	}
}
