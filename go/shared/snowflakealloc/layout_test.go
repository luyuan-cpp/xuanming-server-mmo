package snowflakealloc

import (
	"testing"

	"shared/snowflake"
)

// 17 位 worker 段切成 [cluster5][slot12] 后,常量必须自洽:NodeBits 不变(存量 id 布局不动),
// 两段之和恰好填满它。发号器不知道这个切分 —— 自洽性只能在分配器这边钉住。
func TestLayout_ClusterAndSlotFillNodeBits(t *testing.T) {
	if snowflake.NodeBits != 17 {
		t.Fatalf("NodeBits=%d, 存量 id 布局要求 17", snowflake.NodeBits)
	}
	if uint64(ClusterBits+SlotBits) != snowflake.NodeBits {
		t.Fatalf("ClusterBits(%d)+SlotBits(%d) != NodeBits(%d)", ClusterBits, SlotBits, snowflake.NodeBits)
	}
	if MaxCluster != 31 || MaxSlot != 4095 {
		t.Fatalf("MaxCluster=%d MaxSlot=%d, 期望 31 / 4095", MaxCluster, MaxSlot)
	}
	// login 变体:bwmarrin 的 13 位 node 段切 [cluster3][slot10]。
	if PlayerIDClusterBits != 3 || PlayerIDSlotBits != 10 {
		t.Fatalf("PlayerID 布局 cluster%d/slot%d, 期望 3/10", PlayerIDClusterBits, PlayerIDSlotBits)
	}
}

// (cluster, slot) → worker → (cluster, slot) 往返,含边界值。
// 与 C++ ParseGuid / ComposeID 用同一组向量对拍(设计稿 §5 / §3.7 第 6 条)。
func TestComposeDecodeWorkerID_RoundTrip(t *testing.T) {
	cases := []struct {
		cluster uint32
		slot    uint64
		worker  uint64
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, 4095, 4095},
		{1, 0, 4096},
		{1, 4095, 8191},
		{31, 0, 31 << 12},
		{31, 4095, snowflake.NodeMask},
		{7, 123, 7<<12 | 123},
	}
	for _, c := range cases {
		got, err := ComposeWorkerID(c.cluster, c.slot, ClusterBits, SlotBits)
		if err != nil {
			t.Fatalf("compose(%d,%d): %v", c.cluster, c.slot, err)
		}
		if got != c.worker {
			t.Fatalf("compose(%d,%d)=%d, 期望 %d", c.cluster, c.slot, got, c.worker)
		}
		if got > snowflake.NodeMask {
			t.Fatalf("compose(%d,%d)=%d 越过发号器的 NodeMask %d", c.cluster, c.slot, got, uint64(snowflake.NodeMask))
		}
		cl, sl := DecodeWorkerID(got, ClusterBits, SlotBits)
		if cl != c.cluster || sl != c.slot {
			t.Fatalf("decode(%d)=(%d,%d), 期望 (%d,%d)", got, cl, sl, c.cluster, c.slot)
		}
	}
}

// 越界必须报错而不是静默溢出到相邻位段。
func TestComposeWorkerID_RejectsOverflow(t *testing.T) {
	if _, err := ComposeWorkerID(32, 0, ClusterBits, SlotBits); err == nil {
		t.Fatal("cluster=32 越过 5 位必须报错")
	}
	if _, err := ComposeWorkerID(0, 4096, ClusterBits, SlotBits); err == nil {
		t.Fatal("slot=4096 越过 12 位必须报错")
	}
	// login 的 13 位布局 [cluster3][slot10]
	if _, err := ComposeWorkerID(8, 0, PlayerIDClusterBits, PlayerIDSlotBits); err == nil {
		t.Fatal("login 布局 cluster=8 越过 3 位必须报错")
	}
	if w, err := ComposeWorkerID(7, 1023, PlayerIDClusterBits, PlayerIDSlotBits); err != nil || w != 8191 {
		t.Fatalf("login 布局 (7,1023) 应合成 8191, got %d err=%v", w, err)
	}
	if cl, sl := DecodeWorkerID(8191, PlayerIDClusterBits, PlayerIDSlotBits); cl != 7 || sl != 1023 {
		t.Fatalf("login 布局 decode(8191)=(%d,%d), 期望 (7,1023)", cl, sl)
	}
	// 位宽本身非法(0 位 / 超过 63 位)也要拒绝。
	if _, err := ComposeWorkerID(0, 0, 0, 12); err == nil {
		t.Fatal("clusterBits=0 必须报错")
	}
	if _, err := ComposeWorkerID(0, 0, 5, 0); err == nil {
		t.Fatal("slotBits=0 必须报错")
	}
	if _, err := ComposeWorkerID(0, 0, 32, 32); err == nil {
		t.Fatal("cluster+slot=64 位必须报错")
	}
}

// 存量兼容:旧 id 的 worker17 就是新布局 cluster=0 的 slot,逐位不变。
// 一个 cluster≠0 的发号器发出的 id 必须能被 DecodeID 拆回来。
//
// 发号器在这里是黑盒(它不知道 cluster/slot),所以先用发号器的公开位宽手工拼一个
// 确定向量核对精确值,再用真实 Node 走一遍端到端。
func TestDecodeID_SplitsClusterAndSlot(t *testing.T) {
	worker, err := ComposeWorkerID(3, 77, ClusterBits, SlotBits)
	if err != nil {
		t.Fatal(err)
	}
	timeShift := snowflake.NodeBits + snowflake.StepBits
	id := (uint64(5001) << timeShift) | (worker << snowflake.StepBits) | 0
	sec, cluster, slot, step := DecodeID(id)
	if sec != 5001 || cluster != 3 || slot != 77 || step != 0 {
		t.Fatalf("DecodeID=(%d,%d,%d,%d), 期望 (5001,3,77,0)", sec, cluster, slot, step)
	}
	if WorkerOf(id) != worker {
		t.Fatalf("WorkerOf=%d, 期望 %d", WorkerOf(id), worker)
	}

	// cluster=0 的老式 worker id 走同一条解码路径,得到 slot=worker。
	oid := (uint64(5002) << timeShift) | (uint64(300) << snowflake.StepBits) | 5
	osec, cl0, sl0, st0 := DecodeID(oid)
	if osec != 5002 || cl0 != 0 || sl0 != 300 || st0 != 5 {
		t.Fatalf("存量 worker=300 应解成 (5002,0,300,5), got (%d,%d,%d,%d)", osec, cl0, sl0, st0)
	}

	// 端到端:真实发号器吃合成后的 worker,发出的 id 拆回来 cluster/slot 不变,
	// 首个号 step=0,时间段落在真实墙钟秒内(启动 guard 保证不早于构造秒)。
	before := snowflake.NowEpochSec()
	n := snowflake.NewNode(worker)
	gid, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	after := snowflake.NowEpochSec()
	gsec, gcl, gsl, gstep := DecodeID(gid)
	if gcl != 3 || gsl != 77 || gstep != 0 {
		t.Fatalf("真实发号 DecodeID=(%d,%d,%d,%d), 期望 cluster=3 slot=77 step=0", gsec, gcl, gsl, gstep)
	}
	if gsec < before || gsec > after+1 {
		t.Fatalf("真实发号的时间段 %d 不在 [%d, %d+1] 内", gsec, before, after)
	}
	if WorkerOf(gid) != worker {
		t.Fatalf("真实发号 WorkerOf=%d, 期望 %d", WorkerOf(gid), worker)
	}
}
