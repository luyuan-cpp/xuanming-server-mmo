// Package liveview 是「存活集合理性闸门」的状态机(决策清单 B1-1,2026-11-05 定,
// docs/handoff/launch-readiness-20261013/decisions_launch_line_20261020.md)。
//
// 消费方从 etcd Range 出一份存活集(某个前缀下注册着的节点或 gate),和自己认为还活着的那份(基线)比,把缺席的当成
// 消失,做不可逆的收尾:player_locator 把死 gate 上的在线会话转 DISCONNECTING,scene_manager 摘负载集、拆实例、
// 改派世界频道。etcd 回档、重建、连错或租约大面积过期时 Range 照样成功,只是回空或残缺,照单全收就会把一大批
// 活着的成员同时判死。C++ 节点失租后会马上重新拿租约、按原 node_id 重新注册(etcd_service.cpp
// OnKeepAliveResponse),这类残缺通常几秒到几十秒就自愈。
//
// 规则:
//   - 这次的存活集为空、基线不空:压住(HoldEmpty)。为空期间计时一直从头算,所以永远不会自动放行;
//   - 基线不少于 MinKnown 且缺席超过一半:进入压住(HoldDrop);
//   - 压住期间缺席数一变(有成员回来,或又少了),计时从头算:视图还在动,不下结论。所以恢复到一半以下也继续压住,
//     直到缺席降到 0 或稳定下来;
//   - 缺席数连续 hold 时长不变才放行(Release,每次压住只返回一次)。放行之后 hold 时长内照常放行(Accept),
//     让调用方把这批缺席收完;放行期过了仍超过一半就重新压住;
//   - 缺席降到 0 就复位;
//   - 基线为空(这次没有可比的)不改变状态。
//
// 阈值:MinKnown 与「超过一半」由两个消费方共用,在这里改。hold 时长按压住的代价由消费方各自定:scene_manager 压住时
// 整条 load reporter 停摆,取 60s;player_locator 压住只推迟死 gate 清扫,取 30 分钟,留出人工介入的时间。
//
// 线程模型:Gate 不并发安全,一个 Gate 只给一个 goroutine 用。时刻由调用方传入,传 time.Now() 时计时走单调读数,
// 不受墙钟回拨影响。
package liveview

import "time"

// MinKnown:基线少于这么多成员时不判骤降。小集群挂一台就可能超过一半,那是日常,不是视图出错。
const MinKnown = 4

// Verdict 是一次观察的结论。
type Verdict int

const (
	// Accept:视图可用,调用方照常处理缺席的成员。
	Accept Verdict = iota
	// HoldEmpty:存活集为空而基线不空。这次不处理任何缺席,也不换掉旧视图。
	HoldEmpty
	// HoldDrop:骤降,或骤降之后视图还在变。这次不处理任何缺席,也不换掉旧视图。
	HoldDrop
	// Release:骤降之后缺席数连续 hold 时长没变,这次起照常处理。每次压住只返回一次,调用方据此记一次放行。
	Release
)

// String 给日志用。
func (v Verdict) String() string {
	switch v {
	case Accept:
		return "accept"
	case HoldEmpty:
		return "held_empty"
	case HoldDrop:
		return "held_drop"
	case Release:
		return "released"
	default:
		return "unknown"
	}
}

// Held 报告这次是不是压住。
func (v Verdict) Held() bool {
	return v == HoldEmpty || v == HoldDrop
}

// Observation 是一次观察。Known、Missing 按调用方自己的去重口径数:scene_manager 按 (zone,node),player_locator 按 gate 实例。
type Observation struct {
	// Known 是基线成员数。
	Known int
	// Missing 是基线里这次不在存活集中的成员数。
	Missing int
	// Empty 表示这次的存活集一个成员都没有(不只是基线成员都缺席)。
	Empty bool
}

// Gate 是一份视图的闸门状态。用 NewGate 构造,零值的 hold 为 0。
type Gate struct {
	hold time.Duration

	holding     bool
	lastMissing int
	// calmSince:缺席数最近一次变化、进入压住或看到空集的时刻。
	calmSince time.Time
	// releasedAt:最近一次放行的时刻;零值表示不在放行期。
	releasedAt time.Time
}

// NewGate 返回一个缺席数稳定满 hold 才放行的闸门。
func NewGate(hold time.Duration) *Gate {
	return &Gate{hold: hold}
}

// Observe 记一次观察,返回这次该不该处理缺席的成员。
func (g *Gate) Observe(o Observation, now time.Time) Verdict {
	if o.Known <= 0 {
		// 没有可比的(本副本这一拍没分到 slot、zone 里还没有已知节点):不是视图恢复的证据,状态不动。
		return Accept
	}
	if o.Missing <= 0 {
		g.reset()
		return Accept
	}
	if !g.releasedAt.IsZero() {
		if !o.Empty && now.Sub(g.releasedAt) < g.hold {
			return Accept
		}
		g.reset()
	}
	if o.Empty {
		g.restart(o.Missing, now)
		return HoldEmpty
	}
	if !g.holding {
		if o.Known < MinKnown || o.Missing*2 <= o.Known {
			return Accept
		}
		g.restart(o.Missing, now)
		return HoldDrop
	}
	if o.Missing != g.lastMissing {
		g.restart(o.Missing, now)
		return HoldDrop
	}
	if now.Sub(g.calmSince) < g.hold {
		return HoldDrop
	}
	g.holding = false
	g.releasedAt = now
	return Release
}

// restart 进入压住(或留在压住里),从 now 起重新计时。
func (g *Gate) restart(missing int, now time.Time) {
	g.holding = true
	g.lastMissing = missing
	g.calmSince = now
}

func (g *Gate) reset() {
	*g = Gate{hold: g.hold}
}
