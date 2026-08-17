package constants

import "shared/serverbase"

// Guild member roles.
const (
	RoleMember  uint32 = 0
	RoleOfficer uint32 = 1
	RoleLeader  uint32 = 3
)

// 公会服务独占的 tip 码段 [200, 219]。
//
// # 为什么必须开号段
//
// 下面的 Err* 常量最终写进 TipInfoMessage.id,而 TipInfoMessage 承载的是**全仓
// 唯一一条扁平数轴**:generated/code/proto/tip/*.proto 里所有枚举共用它,
// common 占 1-19、login 20-51、scene 52-75、team 76-93 …… 一直到 cross_server 129。
// 历史实现绕开这条数轴、从 1 开始手写自己的常量,于是
// ErrGuildNotFound=2 与 common 段的 kInvalidTableId=2 完全撞号 —— 客户端按 id
// 查提示表,拿到的是「无效表 ID」而不是「公会不存在」。
//
// 号段划分:130-199 留给未来新增的 *_error_tip.proto(即配表生成链自己的扩展空间),
// 200 起给各 Go 服务的私有段 —— guild 200-219 / friend 220-239 /
// player_locator 240-259(后者当前无 tip 码,只做预留)。
// 「本文件里每个 Err* uint32 常量都落在本段内且互不相同」由 constants_test.go 强制,
// 该测试同时扫描它们与 common 段(以及整条已生成的 tip 数轴)是否重叠。
//
// # 上线影响(必须知情)
//
// 新 id 目前**不在 data/tip/Tip.xlsx 里**,客户端查不到对应文案。
// tipErr() 仍会把英文短语放进 Parameters[0],不至于变成完全空白的错误;
// 真正的中文文案需要后续把 200-219 补进配表并重新生成 tip proto。
// 在补齐之前,这些 id 至少不再冒充 common 段的其它含义。
const (
	GuildTipCodeLo uint32 = 200
	GuildTipCodeHi uint32 = 219
)

// Business error IDs returned in TipInfoMessage.Id.
// 本组常量全部是 tip 码,取值必须落在 [GuildTipCodeLo, GuildTipCodeHi] 内。
const (
	ErrAlreadyInGuild  uint32 = 200
	ErrGuildNotFound   uint32 = 201
	ErrNotInGuild      uint32 = 202
	ErrGuildFull       uint32 = 203
	ErrLeaderCantLeave uint32 = 204
	ErrNotLeader       uint32 = 205
	ErrNoPermission    uint32 = 206
	ErrNotRanked       uint32 = 207
	// ErrIDGenUnavailable:发号器已被 fence(worker id 的 etcd 租约丢了),
	// 本次建帮整体失败。客户端重试即可 —— 进程会退出并由编排重拉、重新拿号。
	ErrIDGenUnavailable uint32 = 208
)

// Default limits.
const (
	DefaultMaxMembers uint32 = 50
	DefaultInitLevel  uint32 = 1
)

// faultCodes 是本段里判定为「服务端内部故障」的码 —— 需要打 Error 日志 + 配告警。
//
// 判定原则照抄 shared/serverbase 的那条线:码明确指向服务端自身或其依赖出错的才算
// 故障;一切游戏规则拒绝(已在公会、公会满、不是会长、没权限、未上榜)都不算。
// 拿不准的一律不放进来 —— 漏报只是维持现状,误报会把「公会满」刷成告警。
var faultCodes = map[uint32]struct{}{
	// 发号器被 fence:本进程已不是该 worker id 的合法持有者,建帮整体失败。
	// 这是服务端自身状态问题,且随后进程会主动退出,必须能被看见。
	ErrIDGenUnavailable: {},
}

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// 为什么不能直接用 serverbase.TipVerdict:它只认已生成的 tip 数轴(上限 129),
// 200-219 会被判成 VerdictUnknown 并刷 rpc_inband_unknown_code 日志。
// 本函数只接管公会自己的号段,段外的码(例如上游透传的 common 段)仍交回全局判定,
// 这样 serverbase 的「码表漂移」告警语义不受影响。
func TipClassifier() serverbase.Classifier {
	return func(code uint32) serverbase.Verdict {
		if code < GuildTipCodeLo || code > GuildTipCodeHi {
			return serverbase.TipVerdict(code)
		}
		if _, ok := faultCodes[code]; ok {
			return serverbase.VerdictFault
		}
		return serverbase.VerdictBizReject
	}
}
