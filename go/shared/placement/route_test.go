package placement

import (
	"math"
	"testing"
)

// TestRouteRowShard 钉住 player_route 的行分片:模数是聚簇主键的一部分,任何环境建过表之后改它,
// 存量行的 route_shard 全部与新算法不一致(按新片点查读不到旧行,CHECK 也会拒绝新写)。
func TestRouteRowShard(t *testing.T) {
	if RouteRowShards != 64 {
		t.Fatalf("RouteRowShards = %d, want 64:改它会让存量行的主键全部失效(player_route 聚簇主键首列 = player_id %% 64,"+
			"schema.go 的 chk_route_shard 写死 64),只能新表迁移", RouteRowShards)
	}
	for _, tc := range []struct {
		id   uint64
		want uint32
	}{
		{0, 0},
		{1, 1},
		{15, 15},
		{16, 16}, // 模数还是 16 的话这里是 0
		{63, 63}, // 最后一片
		{64, 0},  // 回绕
		{65, 1},
		// 4242 = 66×64 + 18;后两个的低 6 位全 1。
		{4242, 18},
		{1<<55 - 1, 63},
		{math.MaxUint64, 63},
	} {
		if got := RouteRowShard(tc.id); got != tc.want {
			t.Fatalf("RouteRowShard(%d) = %d, want %d", tc.id, got, tc.want)
		}
	}
}
