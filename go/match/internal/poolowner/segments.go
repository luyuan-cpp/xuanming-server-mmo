package poolowner

import (
	"sort"
	"strconv"

	"match/internal/catalog"
)

// 热池拆分（docs/design/global-match-hierarchical-pools.md §5.2.1）：一个池的撮合工作切成若干撮合段，
// 每段一个归属键，照旧交给 Owner / Owns（生产经 svc.OwnsWork）在 match 成员表上分工。
// 段只决定谁读哪一段排队单位、跑哪个机房阶段；成局仍由提交点（authority.claimTx）在锁内复核原票。

// Role 是撮合段的角色，也是指标的 role 标签。
type Role uint8

const (
	// RolePool：整池段。每组只有 1 人的池不拆，一段读全池、两个机房阶段都跑（拆分前的行为）。
	RolePool Role = iota + 1
	// RoleRegion：机房段。(池, 首选机房) 的本机房阶段；模板配了子池时只读等满合并时长的单位。
	RoleRegion
	// RoleSub：子池段。(池, 首选机房, 子池号) 的本机房阶段，只读入队不满合并时长的单位。
	RoleSub
	// RoleCross：跨机房段。每个拆开的池一个，只读等满跨机房阈值的单位，只提交跨机房阶段的组。
	RoleCross
)

// Roles 是全部角色，按指标输出的顺序。
var Roles = [...]Role{RolePool, RoleRegion, RoleSub, RoleCross}

func (r Role) String() string {
	switch r {
	case RolePool:
		return "pool"
	case RoleRegion:
		return "region"
	case RoleSub:
		return "sub"
	case RoleCross:
		return "cross"
	}
	return "unknown"
}

// Segment 是一个撮合段。
type Segment struct {
	Pool catalog.Pool
	Role Role
	// Region 是机房段、子池段的首选机房；整池段、跨机房段为 0。
	Region uint32
	// SubPools 是机房段、子池段所在模板的子池数 K（≥ 1，1 = 不拆）；整池段、跨机房段为 0。
	SubPools uint32
	// Sub 是子池段的子池号 0..K-1。
	Sub uint32
}

// Key 是撮合段的归属键。都以 "pool:" 开头，与后台工作按片分工的键（authority/work_owner.go）分开；
// 带上人数档：同一模板的各人数档可以归不同副本（热池常常就是一个模板的一个人数档）。
func (g Segment) Key() string {
	key := "pool:" + strconv.FormatUint(uint64(g.Pool.Mode), 10) + ":" + strconv.FormatUint(uint64(g.Pool.ConfigId), 10) + ":" + strconv.FormatUint(uint64(g.Pool.TeamSize), 10)
	switch g.Role {
	case RoleRegion:
		return key + ":region:" + strconv.FormatUint(uint64(g.Region), 10)
	case RoleSub:
		return key + ":region:" + strconv.FormatUint(uint64(g.Region), 10) + ":sub:" + strconv.FormatUint(uint64(g.Sub), 10)
	case RoleCross:
		return key + ":cross"
	}
	return key
}

// Split 报告池要不要拆成机房段、子池段与跨机房段。每组只有 1 人的池不拆：组里没有陌生人，写者只是入队即时成局的兜底；
// 按机房拆反而会让首选机房整体下线时没有段读这些票，满员单位「首选机房没节点就立即换机房」（D3）就没人做了。
func Split(pool catalog.Pool) bool { return pool.Players() > 1 }

// Segments 枚举全部撮合段：不拆的池一个整池段；拆开的池每个机房一个机房段（模板子池数 K > 1 时再加 K 个子池段），
// 最后一个跨机房段。regions 是机房列表（各副本看同一份 etcd 注册，见设计 §5.2.1 第 2 条），0 与重复会被忽略；
// subPools 返回模板的子池数，nil 或返回 ≤ 1 都是不拆。结果只取决于输入，顺序确定：按 pools 的顺序，池内按机房号、子池号，跨机房段在最后。
func Segments(pools []catalog.Pool, regions []uint32, subPools func(catalog.Pool) uint32) []Segment {
	list := distinctRegions(regions)
	var out []Segment
	for _, pool := range pools {
		if !Split(pool) {
			out = append(out, Segment{Pool: pool, Role: RolePool})
			continue
		}
		k := uint32(1)
		if subPools != nil {
			k = max(subPools(pool), 1)
		}
		for _, region := range list {
			out = append(out, Segment{Pool: pool, Role: RoleRegion, Region: region, SubPools: k})
			if k <= 1 {
				continue
			}
			for sub := uint32(0); sub < k; sub++ {
				out = append(out, Segment{Pool: pool, Role: RoleSub, Region: region, SubPools: k, Sub: sub})
			}
		}
		out = append(out, Segment{Pool: pool, Role: RoleCross})
	}
	return out
}

// Window 返回这一段在数据库时刻 now 要读的原始入队时刻区间：只读 after < enqueued_ms ≤ until，0 表示这一端不限。
// mergeMs 是子池合并时长，crossMs 是跨机房阈值（CrossRegionWaitSeconds）。
// ok 为 false 表示这一段本轮读不到任何单位（时钟还没走过阈值，按「0 = 不限」读会读反），调用方跳过这一段。
func (g Segment) Window(now, mergeMs, crossMs uint64) (after, until uint64, ok bool) {
	switch g.Role {
	case RoleRegion:
		if g.SubPools <= 1 {
			return 0, 0, true
		}
		if now <= mergeMs {
			return 0, 0, false
		}
		return 0, now - mergeMs, true
	case RoleSub:
		if now <= mergeMs {
			return 0, 0, true // 全部单位都还在子池阶段
		}
		return now - mergeMs, 0, true
	case RoleCross:
		if now <= crossMs {
			return 0, 0, false
		}
		return 0, now - crossMs, true
	}
	return 0, 0, true
}

// distinctRegions 去掉 0 与重复，按机房号升序。
func distinctRegions(regions []uint32) []uint32 {
	seen := make(map[uint32]bool, len(regions))
	out := make([]uint32, 0, len(regions))
	for _, region := range regions {
		if region == 0 || seen[region] {
			continue
		}
		seen[region] = true
		out = append(out, region)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
