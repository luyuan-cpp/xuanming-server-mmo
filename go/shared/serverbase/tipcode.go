package serverbase

import (
	"shared/generated/pb/table"
	"shared/generated/tip"
)

// Verdict 是对一个业务错误码的定性。
type Verdict uint8

const (
	// VerdictOK 成功(码 0,或 tip 码表里的 kSuccess=1000)。
	VerdictOK Verdict = iota
	// VerdictBizReject 正常业务拒绝:背包满、队伍满、冷却未到、参数非法……
	// 这类不是故障,只计数,不打 Error 日志、不该配告警。
	VerdictBizReject
	// VerdictFault 服务端内部故障:依赖挂了、服务端自身状态缺失、序列化失败、
	// 状态机走死、超时。这类必须打日志 + 计数 + 配告警。
	VerdictFault
	// VerdictUnknown 码不在已知码表范围内 —— 说明码表长了新段而本包没跟上。
	// 单独计一个数,免得新码被默默归进"业务拒绝"从此隐身。
	VerdictUnknown
)

func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictBizReject:
		return "biz_reject"
	case VerdictFault:
		return "fault"
	default:
		return "unknown_code"
	}
}

// Classifier 把**某一套码表**里的一个错误码定性。
// 不同码表必须用不同的 Classifier,理由见包注释。
type Classifier func(code uint32) Verdict

// ---------------------------------------------------------------------------
// tip 码表
// ---------------------------------------------------------------------------

// tip 码轴的段表已改为**生成产物**:shared/generated/tip。
//
// 这里以前是一张手抄的 tipDomains,而发号在 tools/data_table_exporter 里 ——
// 两边互不知道对方存在,于是往任何一组加码都会拿到全局队尾的号、落在自己段外,
// 且全程零报错(往 common 加第 20 个码,它拿到的是 130 而不是 20)。
//
// 现在段由 data/tip/Tip.xlsx 的组头行声明、由发号器按段分配、
// 并生成到 shared/generated/tip。本文件只消费,不再维护副本。
//
// 段之间**不再连续**:每组各占 1000 号(common 1000、login 2000 ……),
// 留出扩容余量。因此判定一个码在不在轴上要用 tip.InKnownSegment,
// 不能再用「小于某个最大值」——段之间的空洞不属于任何域。

// TipDomain 返回 tip 码所属的域名(用作日志字段;低基数,想当 Prometheus label 也安全)。
func TipDomain(code uint32) string {
	if code == 0 {
		return "ok"
	}
	if d := tip.DomainOf(code); d != "" {
		return d
	}
	return "unknown"
}

// 「哪些 tip 码算服务端内部故障」也已改为**生成产物**:shared/generated/tip 的
// Faults / IsFault,源头是 data/tip/Tip.xlsx 的 fault 列(标 1 的码是故障)。
//
// 这里以前是一张手写的 tipFaultCodes(33 条),guild 还另有一张本地 faultCodes ——
// 「码算不算故障」是码的属性,却和码定义分家,加码的人不知道要去另一个文件登记,
// 与「段和发号分家」是同一类病。
// 现在分类和定义在 Tip.xlsx 同一行上,本文件只消费,不再维护副本。
//
// 判定原则(改 fault 列时照着这条线,别凭感觉;完整清单与「刻意不算故障」的
// 码见 docs/design/tip-code-axis.md「故障分类」一节):
//
//	是故障 —— 码明确指向服务端自身或其依赖出错:存储/序列化失败、
//	          状态机走死、超时、服务端会话/组件/场景状态缺失、无可用节点。
//	不是故障 —— 客户端传错参数、协议层拒绝、以及一切游戏规则拒绝
//	          (满、重复、冷却、权限、已领取)。容量满属于规则拒绝,不是故障。
//
// 拿不准的一律**不标**:漏报只是维持现状(本来就看不见),
// 误报会把"背包满"刷成 Error 告警,那比没有更糟。
// 不要再在服务里包一层本地 map 去"微调"——那就是把分家重新制造出来;
// 要改分类,改表。

// TipVerdict 按 tip 码表给一个码定性。
//
// 注意:只能用于确实来自 tip 码表的码(即 SourceTipInfo)。
// 拿它去判 data_service / scene_manager 的 error_code 会得到垃圾：
// 那两套轴仍从低位独立发号，而 tip 已迁到 1000 起的分段轴，语义并不相通。
func TipVerdict(code uint32) Verdict {
	switch {
	case code == uint32(table.CommonError_kCommon_errorOK),
		code == uint32(table.CommonError_kSuccess):
		return VerdictOK
	case !tip.InAllocatedRange(code):
		// 「码表漂移」:码落在所有段之外,或者高于本二进制编译时该段的已分配上界
		// —— 后者说明对端跑的是更新的码表而本进程没跟上。两种都单独计数,
		// 免得新码被默默归进「业务拒绝」从此隐身。
		return VerdictUnknown
	case tip.IsFault(code):
		return VerdictFault
	}
	return VerdictBizReject
}

// FaultCodeSet 给"自带私有码表"的服务用:列出该码表里属于服务端内部故障的码,
// 其余非 0 码一律当正常业务拒绝,0 当成功。
//
// 例(scene_manager):
//
//	serverbase.FaultCodeSet(
//	    constants.ErrNoAvailableNode,
//	    constants.ErrRedis,
//	    constants.ErrKafkaRoute,
//	    constants.ErrEncodeEvent,
//	)
func FaultCodeSet(faults ...uint32) Classifier {
	set := make(map[uint32]struct{}, len(faults))
	for _, c := range faults {
		set[c] = struct{}{}
	}
	return func(code uint32) Verdict {
		if code == 0 {
			return VerdictOK
		}
		if _, ok := set[code]; ok {
			return VerdictFault
		}
		return VerdictBizReject
	}
}
