package serverbase

import (
	"shared/generated/pb/table"
)

// Verdict 是对一个业务错误码的定性。
type Verdict uint8

const (
	// VerdictOK 成功(码 0,或 tip 码表里的 kSuccess=1)。
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

// tipCodeRange 是 tip 码表的一个域段。
//
// tip 码表是**单一扁平命名空间**:所有 *_error_tip.proto 里的枚举共用
// 0..129 这一条数轴,每个 proto 占一段连续区间,互不重叠
// (0 是各枚举各自的 OK 值,不属于任何段)。
type tipCodeRange struct {
	Domain string
	Lo, Hi uint32
}

// tipDomains 与 generated/code/proto/tip/*.proto 的实际取值一一对应。
// 新增 tip proto / 扩段时必须同步这里,否则新码会落进 VerdictUnknown。
var tipDomains = []tipCodeRange{
	{"common", 1, 19},
	{"login", 20, 51},
	{"scene", 52, 75},
	{"team", 76, 93},
	{"mission", 94, 100},
	{"bag", 101, 115},
	{"skill", 116, 122},
	{"buff", 123, 124},
	{"entity", 125, 125},
	{"actor_action", 126, 126},
	{"mount", 127, 127},
	{"reward", 128, 128},
	{"cross_server", 129, 129},
}

// TipMaxKnownCode 是 tip 码表当前的最大已知码。超过它一律 VerdictUnknown。
const TipMaxKnownCode uint32 = uint32(table.CrossServerError_kSceneTransferInProgress)

// TipDomain 返回 tip 码所属的域名(用作日志字段;不当 Prometheus label 也行,
// 但它是低基数的,想当 label 也安全)。
func TipDomain(code uint32) string {
	if code == 0 {
		return "ok"
	}
	for _, r := range tipDomains {
		if code >= r.Lo && code <= r.Hi {
			return r.Domain
		}
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
	//   kInvalidTableId(2)      查表 id 非法,多半是请求带进来的
	//   kInvalidParameter(6)    客户端参数
	//   kFeatureUnavailable(7)  功能未开放,产品行为
	//   kRateLimitExceeded(9)   限流,是保护生效不是故障
	//   kMessageSizeExceeded(11) / kMessageIdNotFound(14) /
	//   kRequestMessageParseError(15) / kArraySizeTooLargeInMessage(16) /
	//   kNegativeValueInMessage(18)  全是客户端上行侧的问题

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
	// 不算故障:kLoginAccountNotFound(20) / kLoginAccountPlayerFull(21) /
	// kLoginInProgress(25) / kLoginEnteringGame(27) / kLoginPlaying(28) /
	// kTooManyDevices(44) / kLoginBeKickByAnOtherAccount(37) /
	// kLoginSessionDisconnect(36) —— 都是正常的登录态与业务规则。

	// ---- scene 段 ----
	uint32(table.SceneError_kEnterNodeUnavailable):                  {}, // 没有可用场景节点
	uint32(table.SceneError_kEnterSceneGsInfoNull):                  {}, // 服务端 GS 信息缺失
	uint32(table.SceneError_kEnterSceneYourSceneIsNull):             {}, // 服务端场景状态缺失
	uint32(table.SceneError_kChangeScenePlayerQueueNotFound):        {}, // 换场队列状态缺失
	uint32(table.SceneError_kChangeScenePlayerQueueComponentGsNull): {},
	uint32(table.SceneError_kChangeScenePlayerQueueComponentEmpty):  {},
	uint32(table.SceneError_kEnterSceneFailed):                      {}, // 泛化的进场失败
	//
	// 不算故障:kEnterSceneSceneFull(58) / kEnterSceneMainFull(54) /
	// kEnterSceneGsFull(63) / kChangeScenePlayerQueueFull(69) 都是容量拒绝;
	// kEnterSceneYouInCurrentScene(60) / kEnterSceneChangingScene(66) 是状态拒绝;
	// kInvalidEnterSceneParameters(73) / kEnterSceneParamError(57) 是参数问题。

	// ---- 其余域:几乎全是游戏规则拒绝,只挑出"服务端组件缺失"这一类 ----
	uint32(table.MissionError_kPlayerMissionComponentNotFound): {},
	uint32(table.BagError_kBagAddItemHasNotBaseComponent):      {},
	uint32(table.EntityError_kEntityTransformNotFound):         {},
}

// TipVerdict 按 tip 码表给一个码定性。
//
// 注意:只能用于确实来自 tip 码表的码(即 SourceTipInfo)。
// 拿它去判 data_service / scene_manager 的 error_code 会得到垃圾
// —— 那两套码表里的 1、4、8 与 tip 的 1、4、8 完全不是一回事。
func TipVerdict(code uint32) Verdict {
	switch {
	case code == uint32(table.CommonError_kCommon_errorOK),
		code == uint32(table.CommonError_kSuccess):
		return VerdictOK
	case code > TipMaxKnownCode:
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
