// eligibility.go — 挑 gate 之前先给候选分档:哪些能接新玩家,一台都没有时退而求其次用哪些。
//
// 规则只有这一份,用它的有四处:login 的快路径 PickGate、排队 dispatcher 的 newBatchPickFn、排队领票时的复核
// (go/login/internal/logic/pkg/loginqueue),以及 scene_manager 的跨 zone 重定向(gate_redirect.go)。
// 三种不合格各自怎么判出来由调用方负责:停写 login 读 watch 镜像(node_cache.go)、scene_manager 每次直读 etcd 用
// RevisionTracker;排空读运维打的 gate:<id>:draining 标记;满载看 gate 自报的 max_connections。
// 指标与日志由各调用方按自己的名字记。设计与取舍见 docs/design/gate-load-balancing-design.md「满载与停写的 gate」。
package gatepick

import "time"

// ReportPeriod 是 C++ gate 往 etcd 重写自己 NodeInfo 的周期(cpp/nodes/gate/main.cpp 的 playerCountReportTimer,
// 每次无条件 Put:值不变也是一次新写,ModRevision 照样前进)。停写判定按它推;login gate 目录镜像的两条时限
// (node_cache.go 的 cacheResyncAfter、cacheStaleAfter)也按它推。改 gate 上报周期、或者 gate 不再定期重写记录,
// 必须同步改这里,否则健康的 gate 会被判停写;report_period_crosslang_test.go 逐字核对 C++ 源码钉住这条前提。
const ReportPeriod = 10 * time.Second

// StaleAfter:一条 gate 注册记录超过这么久没被重写(ModRevision 没变),就判这台 gate 停写。
// 取三个周期:连续漏两次还不算,第三次也没来才算,网络抖动、GC 停顿不至于把一台好 gate 判成停写。
// 停写多半是 gate 的事件循环卡住了:上报定时器不走了,etcd 租约却可能还在续,记录要等租约到期(NodeTTL,默认 180s)
// 才消失,这期间不剔除就会一直往它身上分人(容量模型 A0-7 的「黑洞」)。
// 每种读法都要保证健康 gate 的停写时长小于它:login 镜像的上界见 node_cache.go 的 nodeDataStaleAfter,直读见 RevisionTracker。
const StaleAfter = 3 * ReportPeriod

// FullPercent:人数达到自报连接上限的这个百分比,这台 gate 就算到水位。
// 余量给人数上报滞后:挑 gate 时看到的人数最多晚约 15s(gate 每 10s 上报一次,login 镜像判过期前再晚约 5s),
// 这段时间里它还在收人。余量不保证不超:这 15s 里分给它的人超过上限的 10% 时仍会顶满,由 gate 按 GateMaxConnections 拒连兜底。
const FullPercent = 90

// 不合格的原因,也是调用方指标的 reason 标签值(封闭集合)。
const (
	ReasonStale    = "stale"    // 注册记录已超过 StaleAfter 没被重写:多半卡住了,票签过去多半连不上
	ReasonDraining = "draining" // 运维标了排空(计划缩容):连得上,只是不该再接新玩家
	ReasonFull     = "full"     // 人数到自报上限的 FullPercent:连得上,余量只剩上报滞后那一点
	ReasonMixed    = "mixed"    // 只用于回落:连得上的候选里既有排空中的,也有到水位的
)

// State 是判一台 gate 合不合格时看的四项。
type State struct {
	PlayerCount uint32
	// MaxConnections 是 gate 自报的客户端连接上限(NodeInfo.max_connections,即它的 GateMaxConnections),
	// 与 PlayerCount 同一单位;0 = 没声明(dev 下不设上限的 gate、旧版 gate),不按上限判。
	MaxConnections uint32
	// Stale:注册记录已超过 StaleAfter 没被重写。判得出的调用方才填(login 镜像、RevisionTracker),判不出时为 false。
	Stale bool
	// Draining:运维标了排空(gate:<id>:draining)。排空中的 gate 照常服务已经在线的玩家,只是不再分新玩家;
	// 读不到标记时为 false(放行)。
	Draining bool
}

// Full 报告人数是否已到水位:playerCount*100 >= maxConnections*FullPercent。按整数算,不引入浮点舍入,
// 上限接近 uint32 也不溢出。maxConnections 为 0 时不按上限判。
func Full(playerCount, maxConnections uint32) bool {
	return maxConnections > 0 && uint64(playerCount)*100 >= uint64(maxConnections)*FullPercent
}

// Verdict 返回一台 gate 不合格的原因:合格返回 "";否则按停写、排空、满载的先后取第一条成立的。
// 停写排最前:它多半连不上;排空与满载都连得上。
func Verdict(s State) string {
	switch {
	case s.Stale:
		return ReasonStale
	case s.Draining:
		return ReasonDraining
	case Full(s.PlayerCount, s.MaxConnections):
		return ReasonFull
	default:
		return ""
	}
}

// Fallback 说明一次 Eligible 的结果取自哪一档。
type Fallback uint8

const (
	// FallbackNone:有完全合格的候选(没停写、没排空、没到水位),结果就是它们。
	FallbackNone Fallback = iota
	// FallbackReachable:没有完全合格的,结果是连得上的那些(在排空或到水位,但没停写)。
	FallbackReachable
	// FallbackAll:连得上的也没有(全部停写),结果是全部候选。
	FallbackAll
)

// Exclusion 是一次 Eligible 的分档情况,调用方据此记指标、打日志。
// Stale、Draining、Full 按 Verdict 归类,互不重叠:既停写又排空或满载的只计 Stale,既排空又满载的只计 Draining。
type Exclusion struct {
	Stale    int
	Draining int
	Full     int
	Fallback Fallback
}

// FellBack 报告结果是否没能取到完全合格的那一档。
func (e Exclusion) FellBack() bool {
	return e.Fallback != FallbackNone
}

// Excluded 返回结果里因 reason 被剔掉的台数。退到哪一档,那一档里用上的就不算剔掉:退到「连得上的」时
// 排空中与到水位的都用上了,只剔停写的;退到「全部」时一台都没剔。reason 不是三种不合格原因之一时返回 0。
func (e Exclusion) Excluded(reason string) int {
	switch {
	case reason == ReasonStale && e.Fallback != FallbackAll:
		return e.Stale
	case reason == ReasonDraining && e.Fallback == FallbackNone:
		return e.Draining
	case reason == ReasonFull && e.Fallback == FallbackNone:
		return e.Full
	default:
		return 0
	}
}

// EachExcluded 按停写、排空、满载的顺序,对结果里被剔掉的每一类调用一次 f(原因, 台数);台数为 0 的跳过。
func (e Exclusion) EachExcluded(f func(reason string, count int)) {
	for _, reason := range [...]string{ReasonStale, ReasonDraining, ReasonFull} {
		if n := e.Excluded(reason); n > 0 {
			f(reason, n)
		}
	}
}

// FallbackReason 返回回落的原因,即调用方 fallback 指标的 reason;没回落时返回 ""。
//   - 退到「全部」:ReasonStale,全部停写,票多半落在卡住的 gate 上;
//   - 退到「连得上的」:看那些候选,全在排空是 ReasonDraining,全到水位是 ReasonFull,两种都有是 ReasonMixed。
func (e Exclusion) FallbackReason() string {
	switch {
	case e.Fallback == FallbackAll:
		return ReasonStale
	case e.Fallback != FallbackReachable:
		return ""
	case e.Full == 0:
		return ReasonDraining
	case e.Draining == 0:
		return ReasonFull
	default:
		return ReasonMixed
	}
}

// Eligible 给候选分档,返回最高的非空一档,保持原顺序:
//  1. 完全合格:没停写、没排空、没到水位(Verdict 为空);
//  2. 连得上的:在排空或到水位,但没停写。没有完全合格的才用这一档(Fallback = FallbackReachable);
//  3. 全部:全部停写时才用(Fallback = FallbackAll)。
//
// 调用方在返回的这一档里按负载挑(TwoChoices)。state 是从候选取出 State 的投影,必须没有副作用
// (每个候选会被调用不止一次)。结果就是全部候选时原样返回 items,不复制;调用方不得修改返回的切片(它可能就是 items)。
//
// 为什么一次分档,而不是各条件各自过滤、各自回落(2026-11-06 起;此前排空在调用方先剔,停写与满载再剔,两层各自回落):
//   - 停写的 gate 多半连不上,排空中与到水位的连得上。分层回落时,「没排空的全部停写、只有排空中的健康」会退到卡住的
//     gate 上:排空那层已经把健康的剔掉,停写那层只能在卡住的里面回落。跨 zone 传送时票签出去源实体就销毁了,连不上只能断线重登。
//   - 调用方先剔排空,候选集里就没有排空 gate 上的在线人数,login 的区级准入因此少算在线、排空期间多放人。
//     并进这一次判定之后,候选集只标记、不剔除,容量记账照数全部 gate。
//
// 总会给出非空的一档,不拒绝:
//   - 全部停写:要么这一区的 gate 全卡住了,分过去照样失败,与没有这层判定时一样;要么是调用方的视图有问题,
//     一律拒绝会把一次可能的误判放大成整区进不去。
//   - 连得上的全在排空或到水位:整区都标了排空多半是误操作,拒绝比分到待缩容的 gate 更糟(后者稍后随排空改派);
//     全部到水位说明有人没经过区级准入就来了:login 排队的区容量已按 AdmissionCapacity 封顶(capacity.go),
//     只剩排队关着、重连豁免、人数上报滞后、跨 zone 传送这几条路,gate 按 GateMaxConnections 拒连兜底。
//
// items 为空时返回空,由调用方报「没有候选」。只影响挑哪台,不影响容量记账:调用方数在线人数时仍要数全部候选。
func Eligible[T any](items []T, state func(T) State) ([]T, Exclusion) {
	var ex Exclusion
	for _, it := range items {
		switch Verdict(state(it)) {
		case ReasonStale:
			ex.Stale++
		case ReasonDraining:
			ex.Draining++
		case ReasonFull:
			ex.Full++
		}
	}
	healthy := len(items) - ex.Stale - ex.Draining - ex.Full
	switch {
	case healthy == len(items):
		return items, ex
	case healthy > 0:
		return keep(items, healthy, state, func(s State) bool { return Verdict(s) == "" }), ex
	case ex.Draining+ex.Full > 0:
		ex.Fallback = FallbackReachable
		if ex.Stale == 0 {
			return items, ex
		}
		return keep(items, ex.Draining+ex.Full, state, func(s State) bool { return !s.Stale }), ex
	default:
		ex.Fallback = FallbackAll
		return items, ex
	}
}

// keep 返回 items 里 ok 成立的元素,保持原顺序;n 是事先数好的个数,只用来分配容量。
func keep[T any](items []T, n int, state func(T) State, ok func(State) bool) []T {
	kept := make([]T, 0, n)
	for _, it := range items {
		if ok(state(it)) {
			kept = append(kept, it)
		}
	}
	return kept
}
