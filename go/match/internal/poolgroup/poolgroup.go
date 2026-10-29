// Package poolgroup 在一个匹配池（同 mode / battle_config_id / 人数档）的一页排队单位里，
// 一次切出尽可能多的组（docs/design/global-match-hierarchical-pools.md §4）。
//
// 它是纯函数：不读 SQL、不看时钟、不选 battle 节点，输入输出都是普通数据。池写者读出一页票据后
// 调用它得到候选组，再逐组交给提交点（authority.claimTx）用加锁读出的原票和数据库时钟复核。
// 这里挑错人不会成错局，只会让提交失败；正确性只在提交点。
//
// 挑人顺序（同一页内对每个锚点按排队序依次尝试）：
//  1. 先本机房：组内全员的首选机房相同；再跨机房：全员都等满 CrossRegionAfterMs（D-16 区域内优先）。
//  2. 每个机房阶段内，范围树从窄到宽：同 zone → 同大区 → 全服，锚点只走到它当前允许的层级；
//     候选也必须已经等到这一层（scope.Admit 的双向同意）。
//  3. 计分模式在容差内按评分就近，并要求组内极差不超过锚点容差；非计分模式按排队序装箱，预组队不拆。
package poolgroup

import (
	"math"
	"sort"

	"match/internal/scope"
)

// MaxTarget 是一组人数的上限（5V5 共 10 人）。
const MaxTarget = 10

const (
	defaultMaxAnchorsPerPass  = 256
	defaultMaxCandidates      = 64
	defaultMaxRatedCandidates = 512
)

// Unit 是一个不可拆的撮合单位：一张单排票，或一支完整的预组队。
type Unit struct {
	ID          uint64            // 单排 = player_id，整队 = 发起者 actor_id；只参与确定性排序
	Players     []uint64          // 与 Members 一一对应
	Members     []scope.Member    // 成员的范围树位置与原始入队时刻
	EnqueuedMs  uint64            // 排队序：单排 = 原票入队时刻，整队 = launch 入队时刻
	Preferred   uint32            // 全体成员共同的首选机房；成员不一致时为 0，只能参加跨机房阶段
	RTT         map[uint32]uint32 // 全体成员都在硬延迟内的机房 → 成员中的最大 RTT（毫秒）
	RatingCenti int64             // 计分模式的评分×100；计分模式只接受单排单位
}

// Policy 是一个池的成组规则。调用方从配置与模板目录取值，提交点用同一组值复核。
type Policy struct {
	Target             int            // 每组人数，1..MaxTarget
	Scope              scope.Schedule // 范围树逐级放宽阈值
	CrossRegionAfterMs uint64         // 跨机房前全员至少要等的时长（D-16）
	Rated              bool           // 计分模式（1V1 / 5V5）
	// ToleranceCenti 返回锚点等待 waitMs 时允许的组内评分极差（评分×100），须随等待单调不减。
	// +Inf 表示不限。只在 Rated 时调用；为 nil 视为不限。
	ToleranceCenti    func(anchorWaitMs uint64) float64
	MaxAnchorsPerPass int // 每个机房阶段最多真正展开搜索的锚点数；≤0 用默认值
	MaxCandidates     int // 每个机房每次尝试最多收集的候选单位数（先按机房过滤再计数）；≤0 用默认值
}

// Group 是一个候选组。Units 是输入切片的下标：Units[0] 是锚点，其余按排队序。
// 计分模式下组内评分极差不超过锚点容差；锚点不一定是组里最早入队的单位
// （之前没凑成的单位可以被后来的锚点当候选），提交点按组内最长等待者的容差复核，只会更宽、不会误拒。
type Group struct {
	Units  []int
	Region uint32      // 放置机房
	Level  scope.Level // 这一组跨到的范围层级
	Cross  bool        // 有成员的首选机房不是 Region
	MaxRTT uint32      // 组内成员到 Region 的最大 RTT
}

type unitState struct {
	valid     bool
	placeable bool // 硬延迟内至少有一个机房当前可分配
	allowed   scope.Level
	waitMs  uint64
	keys    [scope.LevelCount]uint32 // 单位在各层所属子树的键；0 表示这一层没有可共享的子树
}

// Form 返回本页能切出的全部候选组，组与组之间不共用单位，也不共用玩家。
// regions 是当前至少有一个可分配 battle 节点的机房集合。
// 同一名玩家出现在页里两个单位中（读页时票据与整队状态不一致）时，这些单位本轮全部不参加。
func Form(units []Unit, regions map[uint32]bool, nowMs uint64, p Policy) []Group {
	if p.Target < 1 || p.Target > MaxTarget || len(units) == 0 || len(regions) == 0 {
		return nil
	}
	f := former{units: units, regions: regions, now: nowMs, p: p}
	f.prepare()
	maxAnchors := p.MaxAnchorsPerPass
	if maxAnchors <= 0 {
		maxAnchors = defaultMaxAnchorsPerPass
	}
	var out []Group
	for _, cross := range [2]bool{false, true} {
		tried := 0
		for _, a := range f.order {
			if tried >= maxAnchors {
				break
			}
			if !f.free(a) || !f.canAnchor(a, cross) {
				continue // 本阶段不可能成组的锚点不占搜索预算
			}
			tried++
			if g, ok := f.formAround(a, cross); ok {
				f.take(g)
				out = append(out, g)
			}
		}
	}
	return out
}

type former struct {
	units   []Unit
	regions map[uint32]bool
	now     uint64
	p       Policy
	order   []int // 排队序
	pos     []int // pos[i] = 单位 i 在排队序中的位置
	state   []unitState
	used    []bool
}

func (f *former) prepare() {
	n := len(f.units)
	f.order = make([]int, n)
	for i := range f.order {
		f.order[i] = i
	}
	sort.SliceStable(f.order, func(i, j int) bool {
		a, b := &f.units[f.order[i]], &f.units[f.order[j]]
		if a.EnqueuedMs != b.EnqueuedMs {
			return a.EnqueuedMs < b.EnqueuedMs
		}
		return a.ID < b.ID
	})
	f.pos = make([]int, n)
	for position, i := range f.order {
		f.pos[i] = position
	}
	f.state = make([]unitState, n)
	f.used = make([]bool, n)
	for i := range f.units {
		f.state[i] = f.inspect(&f.units[i])
	}
	// 跨单位共用玩家说明这一页读到的状态不一致：涉及的单位全部排除，下一轮重读。
	// 排除之后有效单位两两不共用玩家，组间与组内都不会出现同一玩家两次。
	owners := make(map[uint64]int, n)
	for i := range f.units {
		for _, player := range f.units[i].Players {
			owners[player]++
		}
	}
	for i := range f.units {
		for _, player := range f.units[i].Players {
			if owners[player] > 1 {
				f.state[i].valid = false
				break
			}
		}
	}
}

// inspect 判定单位能否参加撮合，并预算它的允许层级、等待与各层子树键。
func (f *former) inspect(u *Unit) unitState {
	var st unitState
	size := len(u.Players)
	if size == 0 || size > f.p.Target || len(u.Members) != size || len(u.RTT) == 0 || (f.p.Rated && size != 1) {
		return st
	}
	seen := make(map[uint64]bool, size)
	for _, player := range u.Players {
		if player == 0 || seen[player] {
			return st
		}
		seen[player] = true
	}
	allowed, err := scope.UnitAllowed(f.p.Scope, u.Members, f.now)
	if err != nil {
		return st
	}
	st.allowed = allowed
	st.waitMs = ^uint64(0)
	for _, member := range u.Members {
		st.waitMs = min(st.waitMs, scope.WaitMs(member.EnqueuedMs, f.now))
	}
	st.keys[scope.LevelZone] = sharedKey(u.Members, func(n scope.Node) uint32 { return n.Zone })
	st.keys[scope.LevelGroup] = sharedKey(u.Members, func(n scope.Node) uint32 { return n.Group })
	for region := range u.RTT {
		if f.regions[region] {
			st.placeable = true
			break
		}
	}
	st.valid = true
	return st
}

// sharedKey 返回全体成员在某层共同的子树键；不一致或缺失返回 0。
func sharedKey(members []scope.Member, key func(scope.Node) uint32) uint32 {
	first := key(members[0].Node)
	for _, member := range members[1:] {
		if key(member.Node) != first {
			return 0
		}
	}
	return first
}

// sameSubtree 报告两个单位在 level 层是否同属一棵子树（全服层恒为真）。
func (f *former) sameSubtree(a, b int, level scope.Level) bool {
	if level >= scope.LevelGlobal {
		return true
	}
	key := f.state[a].keys[level]
	return key != 0 && key == f.state[b].keys[level]
}

func (f *former) take(g Group) {
	for _, i := range g.Units {
		f.used[i] = true
	}
}

func (f *former) free(i int) bool {
	return !f.used[i] && f.state[i].valid
}

// canAnchor 是 O(1) 的前置判定：本阶段这个单位做锚点是否有可能成组。
func (f *former) canAnchor(a int, cross bool) bool {
	if !f.state[a].placeable {
		return false // 能连的机房当前都没有可分配节点
	}
	anchor := &f.units[a]
	if len(anchor.Players) == f.p.Target {
		return !cross // 满员单位只在本机房阶段放置一次
	}
	if cross {
		return f.state[a].waitMs >= f.p.CrossRegionAfterMs
	}
	_, reachable := anchor.RTT[anchor.Preferred]
	return anchor.Preferred != 0 && reachable && f.regions[anchor.Preferred]
}

func (f *former) formAround(a int, cross bool) (Group, bool) {
	anchor := &f.units[a]
	if len(anchor.Players) == f.p.Target {
		// 单个单位即满员：没有陌生人可等，不分层也不等跨机房，直接放到最合适的机房。
		region, rtt, ok := f.placeAlone(anchor)
		if !ok {
			return Group{}, false
		}
		level, admitted := f.admit([]int{a})
		if !admitted {
			return Group{}, false
		}
		return Group{Units: []int{a}, Region: region, Level: level, Cross: region != anchor.Preferred, MaxRTT: rtt}, true
	}
	tolerance := math.Inf(1)
	if f.p.Rated && f.p.ToleranceCenti != nil {
		tolerance = f.p.ToleranceCenti(f.state[a].waitMs)
		if math.IsNaN(tolerance) || tolerance < 0 {
			return Group{}, false
		}
	}
	for level := scope.LevelZone; level <= f.state[a].allowed; level++ {
		var best Group
		found := false
		for _, region := range f.regionsFor(anchor, cross) {
			members := f.pack(a, f.candidates(a, level, cross, tolerance, region), tolerance)
			if members == nil {
				continue
			}
			g := Group{Units: members, Region: region, MaxRTT: f.maxRTT(members, region)}
			if !found || better(g, best, f.units) {
				best, found = g, true
			}
		}
		if !found {
			continue
		}
		groupLevel, admitted := f.admit(best.Units)
		if !admitted || groupLevel > level {
			continue
		}
		best.Level = groupLevel
		for _, i := range best.Units {
			if f.units[i].Preferred != best.Region {
				best.Cross = true
			}
		}
		return best, true
	}
	return Group{}, false
}

// placeAlone 为没有陌生人的单位选机房：首选机房可用就用它，否则取硬延迟内 RTT 最小的可用机房。
func (f *former) placeAlone(u *Unit) (uint32, uint32, bool) {
	if rtt, ok := u.RTT[u.Preferred]; ok && f.regions[u.Preferred] {
		return u.Preferred, rtt, true
	}
	var region, best uint32
	for candidate, rtt := range u.RTT {
		if !f.regions[candidate] {
			continue
		}
		if region == 0 || rtt < best || (rtt == best && candidate < region) {
			region, best = candidate, rtt
		}
	}
	return region, best, region != 0
}

// regionsFor 返回锚点在本阶段可尝试的机房：本机房阶段只有首选机房，跨机房阶段是全部可达且可分配的机房。
func (f *former) regionsFor(anchor *Unit, cross bool) []uint32 {
	if !cross {
		return []uint32{anchor.Preferred} // canAnchor 已保证可达且可分配
	}
	out := make([]uint32, 0, len(anchor.RTT))
	for region := range anchor.RTT {
		if f.regions[region] {
			out = append(out, region)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// candidates 按排队序收集锚点在 level 层、本阶段、能到达 region 的合格候选单位。
// 上限按机房分别计数：先过滤机房再截断，不会因为排在前面的候选到不了某机房而漏掉它。
func (f *former) candidates(a int, level scope.Level, cross bool, tolerance float64, region uint32) []int {
	limit := f.p.MaxCandidates
	if limit <= 0 {
		limit = defaultMaxCandidates
		if f.p.Rated {
			limit = defaultMaxRatedCandidates
		}
	}
	anchor := &f.units[a]
	room := f.p.Target - len(anchor.Players)
	var out []int
	for _, b := range f.order {
		if len(out) >= limit {
			break
		}
		if b == a || !f.free(b) || len(f.units[b].Players) > room {
			continue
		}
		st := &f.state[b]
		if st.allowed < level || !f.sameSubtree(a, b, level) {
			continue
		}
		peer := &f.units[b]
		if _, ok := peer.RTT[region]; !ok {
			continue
		}
		if cross {
			if st.waitMs < f.p.CrossRegionAfterMs {
				continue
			}
		} else if peer.Preferred != anchor.Preferred {
			continue
		}
		if f.p.Rated && math.Abs(float64(peer.RatingCenti)-float64(anchor.RatingCenti)) > tolerance {
			continue
		}
		out = append(out, b)
	}
	return out
}

// pack 从候选里为锚点凑满 Target 人；凑不满返回 nil。
func (f *former) pack(a int, peers []int, tolerance float64) []int {
	if f.p.Rated {
		return f.packRated(a, peers, tolerance)
	}
	return f.packFIFO(a, peers)
}

// packFIFO 按排队序做精确装箱：预组队整体进出，优先排在前面的单位。人数 ≤10，候选有上限，O(候选×人数)。
func (f *former) packFIFO(a int, peers []int) []int {
	target := f.p.Target
	start := len(f.units[a].Players)
	solutions := make([][]int, target+1)
	solutions[start] = []int{a}
	for _, b := range peers {
		size := len(f.units[b].Players)
		for count := target; count >= start+size; count-- {
			if solutions[count] != nil || solutions[count-size] == nil {
				continue
			}
			solutions[count] = append(append([]int(nil), solutions[count-size]...), b)
		}
		if solutions[target] != nil {
			break
		}
	}
	return solutions[target]
}

// packRated 在候选里找包含锚点、人数为 Target、评分极差最小且不超过容差的一组；同极差取靠前的窗口。
func (f *former) packRated(a int, peers []int, tolerance float64) []int {
	target := f.p.Target
	if len(peers)+1 < target {
		return nil
	}
	pool := append([]int{a}, peers...)
	sort.SliceStable(pool, func(i, j int) bool {
		return f.units[pool[i]].RatingCenti < f.units[pool[j]].RatingCenti
	})
	at := 0
	for i, u := range pool {
		if u == a {
			at = i
			break
		}
	}
	bestStart, bestSpread := -1, math.Inf(1)
	for start := max(0, at-target+1); start <= at && start+target <= len(pool); start++ {
		spread := float64(f.units[pool[start+target-1]].RatingCenti) - float64(f.units[pool[start]].RatingCenti)
		if spread <= tolerance && spread < bestSpread {
			bestStart, bestSpread = start, spread
		}
	}
	if bestStart < 0 {
		return nil
	}
	members := []int{a}
	for _, u := range pool[bestStart : bestStart+target] {
		if u != a {
			members = append(members, u)
		}
	}
	others := members[1:]
	sort.SliceStable(others, func(i, j int) bool { return f.pos[others[i]] < f.pos[others[j]] })
	return members
}

func (f *former) maxRTT(members []int, region uint32) uint32 {
	var worst uint32
	for _, i := range members {
		worst = max(worst, f.units[i].RTT[region])
	}
	return worst
}

func (f *former) admit(members []int) (scope.Level, bool) {
	units := make([][]scope.Member, len(members))
	for i, idx := range members {
		units[i] = f.units[idx].Members
	}
	return scope.Admit(f.p.Scope, units, f.now)
}

// better 跨机房时在多个可行机房里挑一个：组内最大 RTT 更小者优先，其次 RTT 之和，再次机房号。
func better(g, best Group, units []Unit) bool {
	if g.MaxRTT != best.MaxRTT {
		return g.MaxRTT < best.MaxRTT
	}
	gs, bs := sumRTT(g, units), sumRTT(best, units)
	if gs != bs {
		return gs < bs
	}
	return g.Region < best.Region
}

func sumRTT(g Group, units []Unit) uint64 {
	var sum uint64
	for _, i := range g.Units {
		sum += uint64(units[i].RTT[g.Region])
	}
	return sum
}
