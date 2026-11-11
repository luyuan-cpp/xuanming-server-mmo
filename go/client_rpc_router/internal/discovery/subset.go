package discovery

import (
	"sort"

	"shared/releasetrack"
)

// 全服池目标的确定性子集(docs/design/connection-fanout-subsetting-20261106.md §3.3)。
//
// 路由服不分 zone:不取子集时,长期运行后每台路由服都连着全服每一台 match / chat / friend / …,连接数是
// 「路由服数 × 目标数」。这里让每台路由服只在约 k 台目标里挑(k = GlobalTargetSubsetSize),做法是确定性 aperture
// (Finagle 的 deterministic aperture;Google SRE 书「数据中心内的负载均衡」一章的确定性子集是同一类做法):
//
//   - 一条发布轨道的目标按 node_uuid 排成一个环,每台占一样长的一段;
//   - 同轨道的 R 台路由服按 node_uuid 排名,第 i 台的弧从环上 i/R 圈处起,长 m/R 圈;
//   - 路由服只在自己的弧上均匀取点,交给占着这个点的目标。
//
// 各路由服的弧均匀错开,环上每个点恰好落在 m 段弧里。所以各路由服负载相同时,每台目标收到的请求一样多,
// 也不会有目标一台路由服都不连。m = round(k × R / T)(至少 1),一段弧上约有 k 台目标。
//
// 不用 HRW「对 (本路由服, 目标) 打分取前 k 台」:HRW 分得匀靠的是键多。gate 侧按 zone 取路由服,键是成百上千个 zone,
// 分得匀;这里的键是路由服本身,只有几台到几十台,每台目标被几台路由服选中服从二项分布。R = 10、T = 8、k = 4 时,
// 少的目标只被 2 台路由服选中、多的被 8 台选中,负载差 4 倍;R × k 小于 T 时还会有目标一台都不连,加实例不加容量。
//
// 只用整数:环切成 R × T 格,目标 j 占第 [j×R, (j+1)×R) 格,路由服 i 的弧是第 [i×T, (i+m)×T) 格(超出部分绕回环头)。
// 任一格 c 写成 q×T + r(0 ≤ r < T),覆盖它的路由服恰好是 i ∈ {q, q-1, …, q-m+1}(模 R),共 m 台。
//
// 各路由服看到的成员短时不一致(某台刚上线、刚下线)只让这几秒分得不够匀:挑到的仍是镜像里的实例,不影响正确性。

// aperture 是本路由服在一条轨道的环上的弧。不取子集时 cellsPerTarget = 1、start = 0、width = 实例数,
// 即在全部实例里均匀挑。
type aperture struct {
	cellsPerTarget int // 每台目标在环上占几格,= 同轨道路由服数 R
	cells          int // 环的总格数,= R × 实例数
	start          int // 弧的起点(格)
	width          int // 弧长(格),> 0
}

// newAperture 算本路由服在一条有 targets 台实例(> 0)的轨道上的弧。rank 是本路由服在同轨道路由服里按 node_uuid 的名次
// (0 起),peers 是同轨道路由服数(含自己),size 是 GlobalTargetSubsetSize。以下情况在全部实例里挑:
// size <= 0(不取子集)、还不知道自己的名次(peers <= 1 或 rank 越界)、实例不多于 size、算出的弧已经盖满整圈(m >= peers)。
func newAperture(rank, peers, targets, size int) aperture {
	all := aperture{cellsPerTarget: 1, cells: targets, start: 0, width: targets}
	if size <= 0 || peers <= 1 || rank < 0 || rank >= peers || targets <= size {
		return all
	}
	// m = round(size × peers / targets),整数四舍五入;至少 1 段,保证每一格都有路由服覆盖。
	m := (2*size*peers + targets) / (2 * targets)
	if m < 1 {
		m = 1
	}
	if m >= peers {
		return all
	}
	return aperture{cellsPerTarget: peers, cells: peers * targets, start: rank * targets, width: m * targets}
}

// pick 在弧上均匀取一格,返回占这格的目标在环上的下标。intn 与 rand.Intn 同义(测试注入确定的序列)。
func (a aperture) pick(intn func(int) int) int {
	cell := (a.start + intn(a.width)) % a.cells
	return cell / a.cellsPerTarget
}

// sortRing 把一条轨道的实例按 node_uuid 排成环。各路由服必须排出同一个顺序,弧才能互相错开;
// node_uuid 唯一,endpoint 只在 uuid 缺失(测试里手造的条目)时让顺序仍然确定。
func sortRing(entries []NodeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].NodeUuid != entries[j].NodeUuid {
			return entries[i].NodeUuid < entries[j].NodeUuid
		}
		return entries[i].Endpoint < entries[j].Endpoint
	})
}

// PeerRank 算本路由服在同轨道路由服里的名次。peers 是路由服自己这类节点的发现镜像,self 是本路由服的 node_uuid,
// track 是本路由服的发布轨道。只数同轨道的:gate 按自己的轨道挑路由服,同一条轨道的路由服一起承担同一批会话。
// 按 node_uuid 去重、排序;镜像里还没有自己(注册事件还没到)时也把自己算进去。返回(名次, 同轨道路由服数)。
func PeerRank(peers []NodeEntry, self string, track releasetrack.Track) (rank, count int) {
	uuids := map[string]struct{}{self: {}}
	for _, peer := range peers {
		if peer.Track == track && peer.NodeUuid != "" {
			uuids[peer.NodeUuid] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(uuids))
	for uuid := range uuids {
		sorted = append(sorted, uuid)
	}
	sort.Strings(sorted)
	return sort.SearchStrings(sorted, self), len(sorted)
}
