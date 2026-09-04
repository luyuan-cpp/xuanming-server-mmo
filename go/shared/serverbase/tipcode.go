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

// tipFaultCodes 是 tip 码表里判定为"服务端内部故障"的码。
//
// 判定原则(照着这条线加码,别凭感觉):
//
//	是故障 —— 码明确指向服务端自身或其依赖出错:存储/序列化失败、
//	          状态机走死、超时、服务端会话/组件/场景状态缺失、无可用节点。
//	不是故障 —— 客户端传错参数、协议层拒绝、以及一切游戏规则拒绝
//	          (满、重复、冷却、权限、已领取)。容量满属于规则拒绝,不是故障。
//
// 拿不准的一律**不放进来**:漏报只是维持现状(本来就看不见),
// 误报会把"背包满"刷成 Error 告警,那比没有更糟。
// 各服务如需微调,自己包一层 Classifier 即可,不必改本表。
var tipFaultCodes = map[uint32]struct{}{
	// ---- common 段 ----
	// 配置表数据本身坏了 —— 服务端加载/生成侧的问题。
	uint32(table.CommonError_kInvalidTableData): {},
	// 依赖服务不可用。
	uint32(table.CommonError_kServiceUnavailable): {},
	// 服务端手上的实体是空的 —— 状态缺失。
	uint32(table.CommonError_kEntityIsNull): {},
	// 服务端自己越界。
	uint32(table.CommonError_kIndexOutOfRange): {},
	// 实体存在但无效 —— 同样是服务端状态问题。
	uint32(table.CommonError_kThisEntityIsInvalid): {},
	// 会话不在了:玩家还在发请求,服务端却查不到会话。
	uint32(table.CommonError_kSessionNotFound):         {},
	uint32(table.CommonError_kPlayerNotFoundInSession): {},
	// 服务端产出的响应连自己都解不开。
	uint32(table.CommonError_kResponseMessageParseError): {},
	// 节点注册失败 —— 直接影响服务发现。
	uint32(table.CommonError_kFailedToRegisterTheNode): {},
	//
	// 刻意**不算**故障的 common 码,记在这里免得下个人反复纠结:
	//   kInvalidTableId / kInvalidParameter      请求参数不合法
	//   kFeatureUnavailable                     功能未开放,产品行为
	//   kRateLimitExceeded                      限流,是保护生效不是故障
	//   kMessageSizeExceeded / kMessageIdNotFound /
	//   kRequestMessageParseError / kArraySizeTooLargeInMessage /
	//   kNegativeValueInMessage                 全是客户端上行侧的问题

	// ---- login 段 ----
	uint32(table.LoginError_kLoginUnknownError):          {}, // 兜底未知错误
	uint32(table.LoginError_kLoginSessionIdNotFound):     {}, // 会话状态缺失
	uint32(table.LoginError_kLoginSessionNotFound):       {},
	uint32(table.LoginError_kLoginFsmFailed):             {}, // 登录状态机走死
	uint32(table.LoginError_kLoginFSMLoadFailed):         {},
	uint32(table.LoginError_kLoginFSMEventFailed):        {},
	uint32(table.LoginError_kLoginFsmInvalidEvent):       {},
	uint32(table.LoginError_kLoginDataSerializeFailed):   {}, // 序列化/反序列化
	uint32(table.LoginError_kLoginDataParseFailed):       {},
	uint32(table.LoginError_kLoginRedisError):            {}, // 存储依赖
	uint32(table.LoginError_kLoginRedisSetFailed):        {},
	uint32(table.LoginError_kLoginAccountDataLoadFaile):  {}, // 拼写错的历史码,数值真实存在
	uint32(table.LoginError_kLoginAccountDataLoadFailed): {},
	uint32(table.LoginError_kLoginTimeout):               {}, // 超时
	//
	// 不算故障:kLoginAccountNotFound / kLoginAccountPlayerFull /
	// kLoginInProgress / kLoginEnteringGame / kLoginPlaying /
	// kTooManyDevices / kLoginBeKickByAnOtherAccount /
	// kLoginSessionDisconnect —— 都是正常的登录态与业务规则。

	// ---- scene 段 ----
	uint32(table.SceneError_kEnterNodeUnavailable):                  {}, // 没有可用场景节点
	uint32(table.SceneError_kEnterSceneGsInfoNull):                  {}, // 服务端 GS 信息缺失
	uint32(table.SceneError_kEnterSceneYourSceneIsNull):             {}, // 服务端场景状态缺失
	uint32(table.SceneError_kChangeScenePlayerQueueNotFound):        {}, // 换场队列状态缺失
	uint32(table.SceneError_kChangeScenePlayerQueueComponentGsNull): {},
	uint32(table.SceneError_kChangeScenePlayerQueueComponentEmpty):  {},
	uint32(table.SceneError_kEnterSceneFailed):                      {}, // 泛化的进场失败
	//
	// 不算故障:kEnterSceneSceneFull / kEnterSceneMainFull /
	// kEnterSceneGsFull / kChangeScenePlayerQueueFull 都是容量拒绝;
	// kEnterSceneYouInCurrentScene / kEnterSceneChangingScene 是状态拒绝;
	// kInvalidEnterSceneParameters / kEnterSceneParamError 是参数问题。

	// ---- 其余域:几乎全是游戏规则拒绝,只挑出"服务端组件缺失"这一类 ----
	uint32(table.MissionError_kPlayerMissionComponentNotFound): {},
	uint32(table.BagError_kBagAddItemHasNotBaseComponent):      {},
	uint32(table.EntityError_kEntityTransformNotFound):         {},
}

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
	}
	if _, ok := tipFaultCodes[code]; ok {
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
