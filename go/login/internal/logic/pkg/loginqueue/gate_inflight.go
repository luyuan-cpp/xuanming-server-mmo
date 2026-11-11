// gate_inflight.go — 本进程交出去、还没反映进 gate 上报人数的票,按 gate 记。
//
// gate 人数每 10s 才上报一次,login 看到的 PlayerCount 最多晚约 15s。只按它挑,两条挑法都会在一个上报周期里往同一台
// 最闲的 gate 上堆人:快路径每个请求各挑各的,随机二选一只能摊开、不知道本周期已分给某台多少人;dispatcher 一拍之内有
// gatePicker 记账,跨拍却又从同一份滞后人数起算,连续几拍都先填同一台。这里把本进程最近交出去的票按 gate 记下来,
// 挑的时候按「上报人数 + 本进程在途」比负载(GateCandidate.Inflight,两条挑法同一口径)。
//
// 在途窗口取排队的交票后宽限 defaultHandoffGrace(20s):区级容量(FreeSlots)按同一个窗口把交出去的票算作在途占位,
// 这里只是按 gate 拆开记。窗口内已经连上 gate、又被下一次上报算进 PlayerCount 的人会被多算一次,只让那台 gate 显得
// 更忙一点、挑选时往别的 gate 摊;放多少人仍由 FreeSlots 管,不会因此多放。满载判定(gatepick 的水位)仍只看上报人数。
// 只记本进程的:多副本时各记各的,看不到别的副本交出去的票,仍比不记好;不跨进程同步,不引入共享存储。
//
// 记的时刻:快路径在签票成功之后(PickAndSignGateToken);排队在 dispatcher 挑定之后(recordPicks),即放行时选定、
// 取走时才签票的那台。放行槽没写成(PopAdmit 在写槽之前丢掉的条目)或取走时复核改挑了别的 gate,这张票会在原来那台上
// 多算到窗口结束,只影响挑选的摊法,不影响任何人能否进入。
package loginqueue

import (
	"sync"
	"time"
)

// GateInflight 是按 gate 记的在途票。零值不可用,用 NewGateInflight 构造;nil 指针表示不记账:Record 什么都不做,
// Annotate 原样返回。并发安全,一把锁,临界区只有切片操作;内存与窗口内交出去的票数同阶,每次读写顺手清掉过期的。
type GateInflight struct {
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
	// handedOut[nodeID] 是交给这台 gate 的票的交出时刻,按先后追加。
	handedOut map[uint32][]time.Time
}

// NewGateInflight 构造在途记账,窗口取 defaultHandoffGrace。now 为 nil 时用 time.Now
// (带单调时钟读数,墙钟回拨不影响窗口);单测传手动时钟。
func NewGateInflight(now func() time.Time) *GateInflight {
	if now == nil {
		now = time.Now
	}
	return &GateInflight{window: defaultHandoffGrace, now: now, handedOut: make(map[uint32][]time.Time)}
}

// Record 记一张交给 nodeID 的票。
func (g *GateInflight) Record(nodeID uint32) {
	if g == nil {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.handedOut[nodeID] = append(g.pruneLocked(nodeID, now), now)
}

// Annotate 返回候选的副本,每台填上 Inflight = 本进程窗口内交给它的票数;不改调用方的切片。
// 顺手清掉所有 gate 的过期记录,下线的 gate 不会在表里一直留着。
func (g *GateInflight) Annotate(candidates []GateCandidate) []GateCandidate {
	if g == nil || len(candidates) == 0 {
		return candidates
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for nodeID := range g.handedOut {
		g.pruneLocked(nodeID, now)
	}
	out := make([]GateCandidate, len(candidates))
	for i, c := range candidates {
		c.Inflight = uint32(len(g.handedOut[c.NodeID]))
		out[i] = c
	}
	return out
}

// pruneLocked 丢掉 nodeID 窗口外的记录并返回剩下的;剩下为空时从表里删掉这台。调用方持有锁。
// 记录按交出时刻先后追加,过期的都在前面。
func (g *GateInflight) pruneLocked(nodeID uint32, now time.Time) []time.Time {
	times := g.handedOut[nodeID]
	keep := 0
	for keep < len(times) && now.Sub(times[keep]) >= g.window {
		keep++
	}
	times = times[keep:]
	if len(times) == 0 {
		delete(g.handedOut, nodeID)
		return nil
	}
	g.handedOut[nodeID] = times
	return times
}

// recordPicks 包一层 dispatcher 一拍的挑 gate 函数:每挑定一台就记一张在途票。inflight 为 nil 时原样返回。
func recordPicks(pick PickAdmitSlotFn, inflight *GateInflight) PickAdmitSlotFn {
	if inflight == nil {
		return pick
	}
	return func(zoneID uint32, queueID string) (*AdmitSlot, error) {
		slot, err := pick(zoneID, queueID)
		if err == nil && slot != nil {
			inflight.Record(slot.NodeID)
		}
		return slot, err
	}
}
