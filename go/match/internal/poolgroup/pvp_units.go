package poolgroup

import (
	"math"
	"sort"
)

// 分边模式（PVP_TEAM，2V2～5V5）的选人与分边，规则见
// docs/design/global-match-hierarchical-pools.md 开头「计分组排」第 3～6 条。
//
// 一局的人数组成是 2N 的一种拆分：每份不超过 N，其中一份是锚点，5V5 最多 30 种。每种组成在每个人数桶里
// 取排队最早的单位；计分时再限定在一个宽度等于容差、包含锚点的评分窗口里。选出的单位最多 10 个，
// 枚举全部分边（锚点固定在 0 侧，最多 512 种）。规模都是常数级，用不着通用的装箱算法。

// maxPerSide 是每侧人数上限（引擎每队上限 5）。
const maxPerSide = 5

// maxSideUnits 是一局里单位数的上限：两侧都是单人。
const maxSideUnits = 2 * maxPerSide

// SideUnit 是一个不可拆单位在分边选人里用到的属性。
type SideUnit struct {
	Size        int    // 人数，1..每侧人数；越界的单位不参加
	RatingCenti int64  // 队伍分×100，只在计分时用
	WaitMs      uint64 // 单位里等待最短的那名成员的等待，用于车队差放宽
}

// SideRule 是分边选人的规则。
type SideRule struct {
	PerSide int  // 每侧人数，1..5
	Rated   bool // 计分：组内队伍分极差不超过 ToleranceCenti，分边时让两侧总分接近
	// ToleranceCenti 是组内队伍分极差上限（×100），+Inf 不限。只在 Rated 时用；NaN 或负数一律不成组。
	ToleranceCenti float64
	// GapStepMs 是车队差的放宽步长：单位能接受的车队差 = 1 + 等待 / 步长。0 = 不限车队差。
	GapStepMs uint64
}

// selection 是按一种人数组成、在一个评分窗口里选出的一组单位。
type selection struct {
	members []int // units 的下标：锚点 0 在前，其余升序（即排队序）
	last    int   // 用到的最晚排队位置；越小越先来先成局
	spread  int64 // 组内队伍分极差（×100）
}

// sideChoice 是一组单位的一种分边。
type sideChoice struct {
	members   []int
	sides     []uint32
	gap       int   // 两侧最大车队人数之差
	imbalance int64 // 两侧"队伍分 × 人数"之和的差（×100），只在计分时计
	spread    int64
}

// better：车队差更小优先，其次两侧分差，再次组内极差。都相同时保留先遇到的那个（调用方按确定的顺序遍历）。
func (c sideChoice) better(o sideChoice) bool {
	if c.gap != o.gap {
		return c.gap < o.gap
	}
	if c.imbalance != o.imbalance {
		return c.imbalance < o.imbalance
	}
	return c.spread < o.spread
}

// pickSides 为锚点 units[0] 选一局并分边，其余单位按排队序排列。返回选中单位的下标（锚点在前，其余升序）
// 与一一对应的阵营（锚点在 0 侧）；凑不出完整的两侧时返回 nil，单位绝不拆成玩家重新分边。
//
// 先来先成局：候选组按用到的最晚排队位置分批，同一批里取分边最好的一组；这一批都分不了边
// （人数凑不成两个 N，或吃亏一侧还没等够）才看下一批。
func pickSides(units []SideUnit, rule SideRule) ([]int, []uint32) {
	n := rule.PerSide
	if n < 1 || n > maxPerSide || len(units) == 0 || units[0].Size < 1 || units[0].Size > n {
		return nil, nil
	}
	tolerance := math.Inf(1)
	if rule.Rated {
		if math.IsNaN(rule.ToleranceCenti) || rule.ToleranceCenti < 0 {
			return nil, nil
		}
		tolerance = rule.ToleranceCenti
	}
	selections := collectSelections(units, n, rule.Rated, tolerance)
	for start := 0; start < len(selections); {
		end := start
		for end < len(selections) && selections[end].last == selections[start].last {
			end++
		}
		var best sideChoice
		found := false
		for _, sel := range selections[start:end] {
			if choice, ok := splitSides(units, sel, rule); ok && (!found || choice.better(best)) {
				best, found = choice, true
			}
		}
		if found {
			return append([]int(nil), best.members...), best.sides
		}
		start = end
	}
	return nil, nil
}

// collectSelections 列出全部候选组（去重），按用到的最晚排队位置、再按成员下标排序，结果确定。
func collectSelections(units []SideUnit, n int, rated bool, tolerance float64) []selection {
	var buckets [maxPerSide + 1][]int // 人数 → 候选下标（排队序）
	for i := 1; i < len(units); i++ {
		if size := units[i].Size; size >= 1 && size <= n {
			buckets[size] = append(buckets[size], i)
		}
	}
	compositions := compositionsFor(n, units[0].Size)
	seen := map[[maxSideUnits]int]bool{}
	var out []selection
	for _, window := range ratingWindows(units, rated, tolerance) {
		var eligible [maxPerSide + 1][]int
		for size := 1; size <= n; size++ {
			for _, i := range buckets[size] {
				if window.contains(units[i].RatingCenti) {
					eligible[size] = append(eligible[size], i)
				}
			}
		}
		for _, need := range compositions {
			sel, ok := takeEarliest(units, eligible, need)
			if !ok {
				continue
			}
			if key := sel.key(); !seen[key] {
				seen[key] = true
				out = append(out, sel)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].last != out[j].last {
			return out[i].last < out[j].last
		}
		return lessMembers(out[i].members, out[j].members)
	})
	return out
}

// compositionsFor 列出锚点之外还要凑的人数组成：need[k] 是要几个 k 人单位，Σ k·need[k] = 2N − 锚点人数，
// 每份不超过 N。能否分成两个 N 不在这里判断，分不了边的组成由 splitSides 排除。
func compositionsFor(n, anchorSize int) [][maxPerSide + 1]int {
	var out [][maxPerSide + 1]int
	var need [maxPerSide + 1]int
	var fill func(remaining, largest int)
	fill = func(remaining, largest int) {
		if remaining == 0 {
			out = append(out, need)
			return
		}
		for size := min(largest, remaining); size >= 1; size-- {
			need[size]++
			fill(remaining-size, size)
			need[size]--
		}
	}
	fill(2*n-anchorSize, n)
	return out
}

// ratingWindow 是评分闭区间（×100）。不计分时上下都不设限。
type ratingWindow struct {
	low, high float64
}

func (w ratingWindow) contains(rating int64) bool {
	value := float64(rating)
	return value >= w.low && value <= w.high
}

// ratingWindows 列出可能的评分窗口：宽度等于容差、包含锚点，下界取锚点与候选里落在 [锚点 − 容差, 锚点] 的
// 每个分值（组内最低分只能是其中之一），所以窗口里选出的组极差不超过容差。不计分或容差不限时只有一个不设限的窗口。
func ratingWindows(units []SideUnit, rated bool, tolerance float64) []ratingWindow {
	if !rated || math.IsInf(tolerance, 1) {
		return []ratingWindow{{low: math.Inf(-1), high: math.Inf(1)}}
	}
	anchor := units[0].RatingCenti
	lows := map[int64]bool{anchor: true}
	for _, u := range units[1:] {
		if u.RatingCenti <= anchor && float64(anchor-u.RatingCenti) <= tolerance {
			lows[u.RatingCenti] = true
		}
	}
	out := make([]ratingWindow, 0, len(lows))
	for low := range lows {
		out = append(out, ratingWindow{low: float64(low), high: float64(low) + tolerance})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].low < out[j].low })
	return out
}

// takeEarliest 按组成在每个人数桶里取排队最早的单位；有一个桶不够就返回 false。
func takeEarliest(units []SideUnit, eligible [maxPerSide + 1][]int, need [maxPerSide + 1]int) (selection, bool) {
	sel := selection{members: []int{0}}
	for size, count := range need {
		if count == 0 {
			continue
		}
		if len(eligible[size]) < count {
			return selection{}, false
		}
		sel.members = append(sel.members, eligible[size][:count]...)
		sel.last = max(sel.last, eligible[size][count-1])
	}
	sort.Ints(sel.members[1:])
	low, high := units[0].RatingCenti, units[0].RatingCenti
	for _, i := range sel.members[1:] {
		low, high = min(low, units[i].RatingCenti), max(high, units[i].RatingCenti)
	}
	sel.spread = high - low
	return sel, true
}

func (s selection) key() [maxSideUnits]int {
	var key [maxSideUnits]int
	for i := range key {
		key[i] = -1
	}
	copy(key[:], s.members)
	return key
}

func lessMembers(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// splitSides 枚举 sel 的全部分边（锚点固定在 0 侧），返回两侧各 N 人、满足车队对等的分法里最好的一种。
// 掩码第 i 位是 sel.members[i+1] 的阵营；按掩码升序遍历，同样好的分法保留掩码最小的，结果确定。
func splitSides(units []SideUnit, sel selection, rule SideRule) (sideChoice, bool) {
	others := len(sel.members) - 1
	var best sideChoice
	found := false
	for mask := 0; mask < 1<<others; mask++ {
		var people, largest [2]int
		var total [2]int64
		for i, index := range sel.members {
			side := sideOf(i, mask)
			unit := units[index]
			people[side] += unit.Size
			largest[side] = max(largest[side], unit.Size)
			total[side] += unit.RatingCenti * int64(unit.Size)
		}
		if people[0] != rule.PerSide || people[1] != rule.PerSide {
			continue
		}
		gap := abs(largest[0] - largest[1])
		if !premadeConsent(units, sel.members, mask, largest, gap, rule.GapStepMs) {
			continue
		}
		choice := sideChoice{gap: gap, spread: sel.spread}
		if rule.Rated {
			choice.imbalance = abs(total[0] - total[1])
		}
		if found && !choice.better(best) {
			continue
		}
		choice.members = sel.members
		choice.sides = make([]uint32, len(sel.members))
		for i := range sel.members {
			choice.sides[i] = uint32(sideOf(i, mask))
		}
		best, found = choice, true
	}
	return best, found
}

// sideOf 是 sel.members[i] 在掩码 mask 下的阵营：锚点（i = 0）固定在 0 侧。
func sideOf(i, mask int) int {
	if i == 0 {
		return 0
	}
	return (mask >> (i - 1)) & 1
}

// premadeConsent 判定车队对等：车队差不超过 1 立即可成；更大的差要吃亏一侧（最大车队较小的一侧）
// 每个单位都等到能接受这个差。步长为 0 不限车队差。
func premadeConsent(units []SideUnit, members []int, mask int, largest [2]int, gap int, stepMs uint64) bool {
	if gap <= 1 || stepMs == 0 {
		return true
	}
	weaker := 0
	if largest[1] < largest[0] {
		weaker = 1
	}
	for i, index := range members {
		if sideOf(i, mask) == weaker && acceptableGap(units[index].WaitMs, stepMs) < gap {
			return false
		}
	}
	return true
}

// acceptableGap 是等待 waitMs 的单位能接受的车队差：1 + 等待 / 步长。车队差最大是每侧人数 − 1，
// 封顶在 maxPerSide 只是为了不溢出。
func acceptableGap(waitMs, stepMs uint64) int {
	steps := waitMs / stepMs
	if steps >= maxPerSide {
		return maxPerSide
	}
	return 1 + int(steps)
}

func abs[T int | int64](v T) T {
	if v < 0 {
		return -v
	}
	return v
}
