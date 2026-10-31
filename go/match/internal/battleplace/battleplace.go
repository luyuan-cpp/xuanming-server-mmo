// Package battleplace 在一个物理机房里为成局选一台 battle 节点
// （docs/design/global-match-hierarchical-pools.md §1 现状缺口、R1）。
//
// 原先每个机房固定选 NodeId 最小的一台，同机房其它节点拿不到排队成局的流量，那一台满了或排空时
// 整个机房的成局都失败。现在用"随机两选一取较闲"（power of two choices）：从机房里健康节点中随机挑两台，
// 取本副本在途成局较少的那台；节点拒绝分配（battle_not_allocatable）或连不上时本地熔断一段时间。
//
// 在途数只统计本副本发出、尚未结束的备战，不是全局负载；它的作用是把同一副本的突发成局摊开。
// 节点负载上报（battle 在注册信息里报 accepting / room_count）是后续项，接上后只改 healthy 的判定。
// 选点不承担正确性：选到的节点在提交点固化进 formation，之后的创建与恢复都只认它。
package battleplace

import (
	"math/rand/v2"
	"sync"
	"time"

	"match/internal/discovery"
)

// DefaultCooldown 是节点被判不可分配后的本地熔断时长。
const DefaultCooldown = 30 * time.Second

// Picker 记录本副本对各 battle 节点（按实例 UUID）的在途成局数与熔断截止时刻。并发安全。
type Picker struct {
	mu          sync.Mutex
	inflight    map[string]int
	brokenUntil map[string]time.Time
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
	return &Picker{inflight: map[string]int{}, brokenUntil: map[string]time.Time{}, now: now, intn: intn, cooldown: cooldown}
}

func (p *Picker) healthyLocked(node discovery.NodeEntry, at time.Time) bool {
	if node.PhysicalRegion == 0 || node.NodeUuid == "" || node.Endpoint == "" {
		return false
	}
	until, broken := p.brokenUntil[node.NodeUuid]
	return !broken || !at.Before(until)
}

// Regions 返回当前至少有一台健康节点的机房集合，供撮合判断哪些机房可放置。
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

// Pick 在 region 的健康节点里随机挑两台，返回本副本在途较少的那台；在途相同取 NodeId 较小者，结果可复现。
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
	la, lb := p.inflight[a.NodeUuid], p.inflight[b.NodeUuid]
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
