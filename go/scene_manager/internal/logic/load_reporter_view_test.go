package logic

import "testing"

func TestCheckNodeView(t *testing.T) {
	if err := checkNodeView(0, 3); err == nil {
		t.Fatal("已知 3 个节点时 etcd 回空,不能当成全部消失")
	}
	for _, c := range []struct{ next, prev int }{{0, 0}, {1, 3}, {3, 0}, {3, 3}} {
		if err := checkNodeView(c.next, c.prev); err != nil {
			t.Fatalf("next=%d prev=%d: %v", c.next, c.prev, err)
		}
	}
}
