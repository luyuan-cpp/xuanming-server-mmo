// Package scope 定义匹配范围树与逐级放宽规则（docs/design/global-match-hierarchical-pools.md §3）。
//
// 范围树只有三层：玩家的 home zone 是叶子，大区（ZoneGroup 表）是中间层，全服是根。
// 一张票入队后先只和同 zone 的人凑；等满 GroupAfterMs 才放宽到同大区；等满 GlobalAfterMs 才放宽到全服。
// 放宽是双向同意的：一组人跨了哪一层，组里每个单位都必须已经等到允许这一层。
// 刚入队的人不会被别人的长等待拖进更大的范围，他自己的同 zone 优先不被打破。
//
// 本包只做纯计算，不读表、不碰 SQL、不看时钟。撮合（poolgroup）与提交点（authority.claimTx）
// 用同一组函数判定，两处结论不会分叉。
package scope

import (
	"errors"
	"fmt"
	"sort"
)

// Level 是范围树上的层级，值越大范围越宽。
type Level uint8

const (
	LevelZone   Level = 0 // 同一个 home zone
	LevelGroup  Level = 1 // 同一个大区
	LevelGlobal Level = 2 // 全服（根）
	LevelCount        = 3
)

var kLevelNames = [LevelCount]string{"zone", "group", "global"}

// String 用作低基数指标 label 与日志字段。
func (l Level) String() string {
	if int(l) < LevelCount {
		return kLevelNames[l]
	}
	return "invalid"
}

// Node 是一名玩家在范围树上的位置。Group==0 表示该 zone 没有登记到任何大区：
// 它在大区层不与任何其它 zone 同属一棵子树，放宽时从 zone 层直接到全服。
type Node struct {
	Zone  uint32
	Group uint32
}

// keyAt 返回节点在 level 层所属子树的键；ok=false 表示这一层没有可共享的子树。
func (n Node) keyAt(level Level) (uint32, bool) {
	switch level {
	case LevelZone:
		return n.Zone, n.Zone != 0
	case LevelGroup:
		return n.Group, n.Group != 0
	case LevelGlobal:
		return 0, true
	}
	return 0, false
}

// ErrInvalidNode 表示有成员没有 home zone。入口必须先拒绝这类票，撮合与提交点见到它一律不成组。
var ErrInvalidNode = errors.New("scope: 成员缺少 home zone")

// CommonLevel 返回一组节点的最近公共祖先层级：所有节点同属一棵子树的最低层。
// 空集合或有节点缺 home zone 时返回 ErrInvalidNode。
func CommonLevel(nodes []Node) (Level, error) {
	if len(nodes) == 0 {
		return 0, ErrInvalidNode
	}
	for _, node := range nodes {
		if node.Zone == 0 {
			return 0, ErrInvalidNode
		}
	}
	for level := LevelZone; level < LevelGlobal; level++ {
		first, ok := nodes[0].keyAt(level)
		shared := ok
		for _, node := range nodes[1:] {
			if !shared {
				break
			}
			key, ok := node.keyAt(level)
			shared = ok && key == first
		}
		if shared {
			return level, nil
		}
	}
	return LevelGlobal, nil
}

// Schedule 是逐级放宽的等待阈值（毫秒，从票据原始入队时刻起算，重试和回队不重置）。
// 两个值都为 0 等于入队即全服，用于需要关闭分层的测试或玩法。
type Schedule struct {
	GroupAfterMs  uint64 // 等满这么久才允许同大区
	GlobalAfterMs uint64 // 等满这么久才允许全服
}

// Validate 要求阈值单调不减：先放宽到大区，再放宽到全服。
func (s Schedule) Validate() error {
	if s.GroupAfterMs > s.GlobalAfterMs {
		return fmt.Errorf("scope: GroupAfterMs(%d) 不能大于 GlobalAfterMs(%d)", s.GroupAfterMs, s.GlobalAfterMs)
	}
	return nil
}

// MinWaitMs 返回放宽到 level 至少要等多久。
// 只能用来预过滤**单排票**（SQL 侧 enqueued_ms <= now - MinWaitMs(level)）：单排票自身跨度恒为 zone 层，
// 等待就是它的入队时长。整队单位的允许层级还取决于成员自身跨度与最短成员等待（UnitAllowed），
// 不能按 launch 入队时刻做层级预过滤，否则会排除掉 Admit 本来接受的组。
func (s Schedule) MinWaitMs(level Level) uint64 {
	switch level {
	case LevelZone:
		return 0
	case LevelGroup:
		return s.GroupAfterMs
	default:
		return s.GlobalAfterMs
	}
}

// AllowedByWait 返回等待 waitMs 之后允许的最宽层级。
func (s Schedule) AllowedByWait(waitMs uint64) Level {
	switch {
	case waitMs >= s.GlobalAfterMs:
		return LevelGlobal
	case waitMs >= s.GroupAfterMs:
		return LevelGroup
	default:
		return LevelZone
	}
}

// Member 是参与范围判定的一名成员。
type Member struct {
	Node       Node
	EnqueuedMs uint64
}

// WaitMs 用调用方给定的同一时刻算等待；入队时刻晚于 now（时钟偏差）按 0 计。
func WaitMs(enqueuedMs, nowMs uint64) uint64 {
	if enqueuedMs >= nowMs {
		return 0
	}
	return nowMs - enqueuedMs
}

// UnitAllowed 返回一个不可拆单位（单排票或完整预组队）当前允许的最宽层级。
//   - 等待取成员中最短的一个（最保守）。
//   - 预组队成员本来就跨了若干层（社交规则允许跨 zone 组队时），这个跨度是它自身的范围，
//     不需要再等：允许层级至少是成员自身的公共层级。
func UnitAllowed(s Schedule, members []Member, nowMs uint64) (Level, error) {
	if len(members) == 0 {
		return 0, ErrInvalidNode
	}
	nodes := make([]Node, len(members))
	minWait := ^uint64(0)
	for i, member := range members {
		nodes[i] = member.Node
		if wait := WaitMs(member.EnqueuedMs, nowMs); wait < minWait {
			minWait = wait
		}
	}
	own, err := CommonLevel(nodes)
	if err != nil {
		return 0, err
	}
	return max(own, s.AllowedByWait(minWait)), nil
}

// Admit 判定若干不可拆单位能否组成一局，并返回这一局跨到的层级。
//   - 只有一个单位时没有陌生人，不受范围约束（满员预组队、单人档）。
//   - 多个单位时，层级是全部成员的最近公共祖先；每个单位的允许层级都必须不低于它。
//
// 撮合挑人与提交点复核都调用它；提交点传入的是加锁读出的原票与数据库时钟。
func Admit(s Schedule, units [][]Member, nowMs uint64) (Level, bool) {
	if len(units) == 0 {
		return 0, false
	}
	var nodes []Node
	for _, unit := range units {
		if len(unit) == 0 {
			return 0, false
		}
		for _, member := range unit {
			nodes = append(nodes, member.Node)
		}
	}
	level, err := CommonLevel(nodes)
	if err != nil {
		return 0, false
	}
	if len(units) == 1 {
		return level, true
	}
	for _, unit := range units {
		allowed, err := UnitAllowed(s, unit, nowMs)
		if err != nil || allowed < level {
			return 0, false
		}
	}
	return level, true
}

// Range 是 ZoneGroup 表的一行：[ZoneBegin, ZoneEnd] 闭区间内的 zone 归属大区 GroupID。
// 同一个大区可以有多行（合服或扩容后号段不连续）。
type Range struct {
	GroupID   uint32
	ZoneBegin uint32
	ZoneEnd   uint32
}

// Directory 是 zone → 大区 的不可变查表结果，按 ZoneBegin 升序存放。
// 只在入队时用它给票据写入大区号；之后撮合与提交点都读票上冻结的值，不再查表，
// 所以滚动换表期间各副本表版本不同，也不会让同一张票在两个副本上算出不同的大区。
type Directory struct {
	ranges []Range
}

// NewDirectory 校验并建表：大区号与 zone 号非 0、区间不倒置、任意两行不重叠。
func NewDirectory(rows []Range) (*Directory, error) {
	sorted := append([]Range(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ZoneBegin < sorted[j].ZoneBegin })
	for i, row := range sorted {
		if row.GroupID == 0 || row.ZoneBegin == 0 || row.ZoneBegin > row.ZoneEnd {
			return nil, fmt.Errorf("scope: 大区行无效 group=%d zone=[%d,%d]", row.GroupID, row.ZoneBegin, row.ZoneEnd)
		}
		if i > 0 && sorted[i-1].ZoneEnd >= row.ZoneBegin {
			prev := sorted[i-1]
			return nil, fmt.Errorf("scope: 大区行重叠 group=%d zone=[%d,%d] 与 group=%d zone=[%d,%d]",
				prev.GroupID, prev.ZoneBegin, prev.ZoneEnd, row.GroupID, row.ZoneBegin, row.ZoneEnd)
		}
	}
	return &Directory{ranges: sorted}, nil
}

// GroupOf 返回 zone 所属大区；没有登记时返回 0（该 zone 放宽时跳过大区层）。
func (d *Directory) GroupOf(zone uint32) uint32 {
	if d == nil || zone == 0 {
		return 0
	}
	i := sort.Search(len(d.ranges), func(i int) bool { return d.ranges[i].ZoneEnd >= zone })
	if i < len(d.ranges) && d.ranges[i].ZoneBegin <= zone {
		return d.ranges[i].GroupID
	}
	return 0
}

// NodeOf 返回 zone 在范围树上的位置。
func (d *Directory) NodeOf(zone uint32) Node {
	return Node{Zone: zone, Group: d.GroupOf(zone)}
}
