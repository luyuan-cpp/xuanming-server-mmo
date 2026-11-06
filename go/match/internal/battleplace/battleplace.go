// Package battleplace 在一个物理机房里为成局选一台 battle 节点
// （docs/design/global-match-hierarchical-pools.md §1 现状缺口、§5.1、R1、R6）。
//
// 原先每个机房固定选 NodeId 最小的一台，同机房其它节点拿不到排队成局的流量，那一台满了或排空时
// 整个机房的成局都失败。现在用"随机两选一取较闲"（power of two choices）：从机房里可选的节点中随机挑两台，
// 取负载较低的那台；节点拒绝分配（battle_not_allocatable）或连不上时本地熔断一段时间。
//
// 可选 = 注册完整（机房、实例 UUID、gRPC 地址）、注册信息里的可分配状态（NodeInfo.admission）不是 CLOSED、
// 未被本副本熔断。battle 只在可分配状态变化时写 etcd（开闸、Agones 排空与解除、停机关闸），不周期写。
// UNSPECIFIED 视为可选，这是有意的 fail-open：新版 battle 在 etcd 首次发布前就填 CLOSED、开闸后才改 OPEN，
// UNSPECIFIED 只来自不认识该字段的旧版本；滚动升级时把旧节点判成不可选，会让整个机房没有节点、排队成局全部失败。
// 误选的代价有界：不接房间的节点在任何副作用之前就以 battle_not_allocatable 拒绝（建房预检或 CreateBattle），
// 本副本据此熔断它，成局照常收尾。
//
// 负载 = 新鲜上报的房间数 + 本副本在途数，只用来比较同机房的两台候选，不是容量判定：
//   - 房间数由 battle 随 CreateBattle 成功应答捎回（Observe），不另发上报流量。它是节点的全局量（含其它副本建的房间），
//     但只在本副本选中这台建房时才刷新；超过 DefaultReportTTL 的读数按未知处理、只看在途，否则一台节点会因为一次
//     旧的高读数被一直避开，永远等不到刷新读数的机会。代价：未知的节点显得更闲，会先被多选几次（选中即刷新读数）；
//     混版本期间不上报的旧节点一直显得更闲、多分到成局。
//   - 在途数只统计本副本发出、尚未结束的备战，覆盖"已选中、房间还没建成、读数还没回来"的那一段，把同一副本的突发成局摊开。
//
// 选点不承担正确性：选到的节点在提交点固化进 formation，之后的创建与恢复都只认它；
// 能不能建房只由 battle 的建房准入闸与 Agones 分配许可判定。
package battleplace

import (
	"math/rand/v2"
	"sync"
	"time"

	"match/internal/discovery"

	base "proto/common/base"
)

// DefaultCooldown 是节点被判不可分配后的本地熔断时长。
const DefaultCooldown = 30 * time.Second

// DefaultReportTTL 是节点房间数读数的有效期；更早的读数按未知处理（只看在途）。
const DefaultReportTTL = 60 * time.Second

// 节点的选点状态，也是 match_battle_nodes{state} 指标的取值（低基数）。每台节点只归入一种，按下列顺序判定。
const (
	StateInvalid     = "invalid"     // 注册不完整（缺机房、实例 UUID 或 gRPC 地址），不可选
	StateClosed      = "closed"      // 节点自报不接新房间（启动未完成、Agones 排空或未就绪、停机或维护），不可选
	StateCooldown    = "cooldown"    // 本副本熔断中（拒绝分配或连不上之后的熔断期内），不可选
	StateOpen        = "open"        // 节点自报接新房间，可选
	StateUnspecified = "unspecified" // 节点没上报可分配状态（旧版本），按可选处理（理由见包注释）
)

// roomReport 是一台节点最近一次随建房成功应答捎回的房间数。
type roomReport struct {
	rooms uint32
	at    time.Time
}

// Picker 记录本副本对各 battle 节点（按实例 UUID）的在途成局数、熔断截止时刻与最近一次房间数读数。并发安全。
// 已下线实例的熔断与读数条目不主动清理（每个实例至多各一条）；读数过期后在下一次比较时删掉。
type Picker struct {
	mu          sync.Mutex
	inflight    map[string]int
	brokenUntil map[string]time.Time
	reports     map[string]roomReport
	now         func() time.Time
	intn        func(n int) int
	cooldown    time.Duration
}

// New 返回使用真实时钟与随机数的 Picker。
func New() *Picker {
	return NewWith(time.Now, rand.IntN, DefaultCooldown)
}

// NewWith 用于单测注入时钟与随机数。
func NewWith(now func() time.Time, intn func(n int) int, cooldown time.Duration) *Picker {
	return &Picker{inflight: map[string]int{}, brokenUntil: map[string]time.Time{}, reports: map[string]roomReport{},
		now: now, intn: intn, cooldown: cooldown}
}

// stateLocked 给出 node 此刻的选点状态（取值与判定顺序见 State* 常量）。调用方持有 p.mu。
func (p *Picker) stateLocked(node discovery.NodeEntry, at time.Time) string {
	if node.PhysicalRegion == 0 || node.NodeUuid == "" || node.Endpoint == "" {
		return StateInvalid
	}
	if node.Admission == base.NodeAdmission_NODE_ADMISSION_CLOSED {
		return StateClosed
	}
	if until, broken := p.brokenUntil[node.NodeUuid]; broken && at.Before(until) {
		return StateCooldown
	}
	if node.Admission == base.NodeAdmission_NODE_ADMISSION_OPEN {
		return StateOpen
	}
	// UNSPECIFIED，以及本副本不认识的新取值（更新版本的 battle）：都按"没上报"放行。
	return StateUnspecified
}

func (p *Picker) healthyLocked(node discovery.NodeEntry, at time.Time) bool {
	switch p.stateLocked(node, at) {
	case StateOpen, StateUnspecified:
		return true
	default:
		return false
	}
}

// loadLocked 是比较用的负载估计：新鲜读数的房间数 + 本副本在途数。读数过期按未知计（只剩在途），并顺手删掉。
// 调用方持有 p.mu。
func (p *Picker) loadLocked(instance string, at time.Time) uint64 {
	load := uint64(p.inflight[instance])
	report, ok := p.reports[instance]
	if !ok {
		return load
	}
	if at.Sub(report.at) >= DefaultReportTTL {
		delete(p.reports, instance)
		return load
	}
	return load + uint64(report.rooms)
}

// Regions 返回当前至少有一台可选节点的机房集合，供撮合判断哪些机房可放置。
func (p *Picker) Regions(nodes []discovery.NodeEntry) map[uint32]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.now()
	out := map[uint32]bool{}
	for _, node := range nodes {
		if p.healthyLocked(node, at) {
			out[node.PhysicalRegion] = true
		}
	}
	return out
}

// Pick 在 region 的可选节点里随机挑两台，返回负载（新鲜读数的房间数 + 本副本在途数）较低的那台；
// 负载相同取 NodeId 较小者，结果可复现。
func (p *Picker) Pick(nodes []discovery.NodeEntry, region uint32) (discovery.NodeEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.now()
	var healthy []discovery.NodeEntry
	for _, node := range nodes {
		if node.PhysicalRegion == region && p.healthyLocked(node, at) {
			healthy = append(healthy, node)
		}
	}
	switch len(healthy) {
	case 0:
		return discovery.NodeEntry{}, false
	case 1:
		return healthy[0], true
	}
	first := p.intn(len(healthy))
	second := p.intn(len(healthy) - 1)
	if second >= first {
		second++
	}
	a, b := healthy[first], healthy[second]
	la, lb := p.loadLocked(a.NodeUuid, at), p.loadLocked(b.NodeUuid, at)
	if lb < la || (lb == la && b.NodeId < a.NodeId) {
		return b, true
	}
	return a, true
}

// Started 记录一局开始在 instance 上备战；必须与 Finished 成对调用。
func (p *Picker) Started(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight[instance]++
}

// Finished 记录一局备战结束（无论成败）。
func (p *Picker) Finished(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight[instance] <= 1 {
		delete(p.inflight, instance)
		return
	}
	p.inflight[instance]--
}

// MarkUnavailable 在节点拒绝分配或连不上时本地熔断它 cooldown 时长；到期后自动恢复参与选点。
func (p *Picker) MarkUnavailable(instance string) {
	if instance == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.brokenUntil[instance] = p.now().Add(p.cooldown)
}

// Observe 记下 instance 随 CreateBattle 成功应答捎回的房间数（含刚建成的这一间，R6）。
// 所有 CreateBattle 成功应答处都要调（logic/gather.go 的开局与 logic/global_match.go 的恢复重放）。
// rooms 为 0 表示节点没上报（旧版本），不记：不能当成"空闲"写进去，也不覆盖之前的读数。
func (p *Picker) Observe(instance string, rooms uint32) {
	if instance == "" || rooms == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports[instance] = roomReport{rooms: rooms, at: p.now()}
}

// Census 把 nodes 按选点状态计数，供 match_battle_nodes{state} 指标。返回值总含全部状态（没有的计 0），
// 调用方逐个写 gauge，归零的状态也会被刷新。
func (p *Picker) Census(nodes []discovery.NodeEntry) map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.now()
	counts := map[string]int{StateInvalid: 0, StateClosed: 0, StateCooldown: 0, StateOpen: 0, StateUnspecified: 0}
	for _, node := range nodes {
		counts[p.stateLocked(node, at)]++
	}
	return counts
}
