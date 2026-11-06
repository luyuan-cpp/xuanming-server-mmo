package data

import (
	"math"
	"sort"
	"testing"
)

// 降序榜的排序键按分数单调递减,升序榜就是分数本身;整个 int64 都不溢出。
// 按 order_key 升序排出来的顺序,必须与「降序榜分数从高到低 / 升序榜分数从低到高」逐项相同。
func TestOrderKeyMatchesBoardDirection(t *testing.T) {
	scores := []int64{math.MinInt64, -100, -1, 0, 1, 42, 100, math.MaxInt64}
	for _, ascending := range []uint32{0, 1} {
		got := append([]int64(nil), scores...)
		sort.Slice(got, func(i, j int) bool { return orderKey(ascending, got[i]) < orderKey(ascending, got[j]) })
		want := append([]int64(nil), scores...)
		if ascending == 0 {
			sort.Slice(want, func(i, j int) bool { return want[i] > want[j] })
		} else {
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ascending=%d: order by order_key = %v, want %v", ascending, got, want)
			}
		}
	}
	if orderKey(0, math.MinInt64) != math.MaxInt64 || orderKey(0, math.MaxInt64) != math.MinInt64 {
		t.Fatalf("descending order_key must map MinInt64↔MaxInt64 without overflow")
	}
}

func TestPlaceholders(t *testing.T) {
	cases := map[int]string{0: "", 1: "?", 3: "?,?,?"}
	for n, want := range cases {
		if got := placeholders(n); got != want {
			t.Fatalf("placeholders(%d) = %q, want %q", n, got, want)
		}
	}
	if got := len(orderTupleArgs(5, 6, 7)); got != 6 {
		t.Fatalf("orderTupleArgs must return the 6 args the tuple predicates take, got %d", got)
	}
}
