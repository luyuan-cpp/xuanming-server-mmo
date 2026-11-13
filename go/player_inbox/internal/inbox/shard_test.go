package inbox

import "testing"

// TestRowShard 钉住 shard = mix64(player_id) % 64(建表契约,设计 §4.2;评审 L5)。
// 期望值由 splitmix64 终结函数独立算出;改公式或模数等于改全部存量行的主键,这里必须红。
func TestRowShard(t *testing.T) {
	if ShardCount != 64 {
		t.Fatalf("ShardCount = %d;模数是建表契约,改它等于改全部存量行的主键", ShardCount)
	}
	cases := map[uint64]uint32{
		0:          0, // mix64(0) = 0;player_id=0 在各入口已被拒,这里只钉住公式
		1:          37,
		2:          10,
		63:         21,
		64:         19,
		65:         43,
		12345:      17,
		1<<63 + 5:  26,
		^uint64(0): 59,
	}
	for pid, want := range cases {
		if got := RowShard(pid); got != want {
			t.Errorf("RowShard(%d) = %d, want %d", pid, got, want)
		}
	}
}

// TestMix64MatchesSplitMix64 钉住 mix64 就是公开的 splitmix64 终结函数:splitmix64 以种子 0 起步时,
// 前两个输出是 mix64(γ) 与 mix64(2γ),γ = 0x9e3779b97f4a7c15。别处要镜像本算法时(merge_zone 等独立 module),
// 拿这两个参考值自检即可,不必信任本文件。
func TestMix64MatchesSplitMix64(t *testing.T) {
	gamma := uint64(0x9e3779b97f4a7c15) // 变量而不是常量:2γ 要在运行期按 2^64 回绕,常量表达式会溢出报错
	if got := mix64(gamma); got != 0xe220a8397b1dcdaf {
		t.Fatalf("mix64(γ) = %#x, want 0xe220a8397b1dcdaf(splitmix64 种子 0 的第 1 个输出)", got)
	}
	if got := mix64(2 * gamma); got != 0x6e789e6aa1b965f4 {
		t.Fatalf("mix64(2γ) = %#x, want 0x6e789e6aa1b965f4(splitmix64 种子 0 的第 2 个输出)", got)
	}
	if got := mix64(1); got != 0x5692161d100b05e5 {
		t.Fatalf("mix64(1) = %#x, want 0x5692161d100b05e5", got)
	}
}

// TestRowShardSpreadsBwmarrinIDs 是评审 L5 的回归:当时存量 player_id 来自 bwmarrin/snowflake(login 原布局
// 13-bit node / 9-bit step;库默认 10-bit node / 12-bit step;login 的这套号源 2026-11-12 已删,保留用例钉住公式与位布局无关)。同一毫秒只建一个角色时
// step = 0,低 9 / 12 位全 0,旧公式 player_id % 64 会把它们全部压进 shard 0。新公式必须把它们打散到多个分片。
// 输入是固定的一批 ID,结论确定、可重复。
func TestRowShardSpreadsBwmarrinIDs(t *testing.T) {
	layouts := []struct {
		name      string
		nodeShift uint // node 段的起始位 = step 位宽
	}{
		{"login 布局 13-bit node / 9-bit step", 9},
		{"bwmarrin 默认 10-bit node / 12-bit step", 12},
	}
	const n = 1024
	for _, l := range layouts {
		hits := make([]int, ShardCount)
		for i := uint64(0); i < n; i++ {
			ms := uint64(1_000_000_000) + i*37    // 自 epoch 起的毫秒数,取值任意
			id := ms<<22 | uint64(7)<<l.nodeShift // node = 7,step = 0
			if id%ShardCount != 0 {
				t.Fatalf("%s: 测试前提不成立:id=%d 按旧公式不在 shard 0", l.name, id)
			}
			hits[RowShard(id)]++
		}
		used, maxHit := 0, 0
		for _, h := range hits {
			if h > 0 {
				used++
			}
			maxHit = max(maxHit, h)
		}
		// 均匀时每片约 16 个;阈值留了余量(这批输入实测 64 片全用到,单片最多 27)。
		if used < 60 || maxHit > 3*n/ShardCount {
			t.Fatalf("%s: %d 个 step=0 的 ID 只落到 %d 个分片,单片最多 %d 个,want ≥ 60 片且单片 ≤ %d",
				l.name, n, used, maxHit, 3*n/ShardCount)
		}
	}
}

// TestCheckRowShard:shard 与 player_id 是否一致的纯判定(TiDB 不执行 CHECK)。
// 它只在「行已经读出来」时起作用:按旧公式写出的行对按玩家的读写根本不可见,走不到这里(评审 R3)——
// 真实路径(整库核对拒绝、按玩家读不到、按分片扫描整片报错)见 store_integration_test.go 的 TestIntegrationCheckRowShards。
func TestCheckRowShard(t *testing.T) {
	if err := checkRowShard(RowShard(12345), 12345); err != nil {
		t.Fatalf("一致的行不应报错: %v", err)
	}
	if err := checkRowShard((RowShard(12345)+1)%ShardCount, 12345); err == nil {
		t.Fatal("shard 与 player_id 不一致必须报错")
	}
	// 按旧公式 player_id % 64 算出的 shard 与新公式不同(这个 player_id 上),CheckRowShards 靠这一条判出旧数据。
	if err := checkRowShard(uint32(12345%ShardCount), 12345); err == nil {
		t.Fatal("按旧公式算出的 shard 必须判为不一致")
	}
}
