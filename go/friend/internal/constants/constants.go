package constants

import "shared/serverbase"

// 好友服务独占的 tip 码段 [220, 239]。
//
// # 为什么必须开号段
//
// 下面的 Err* 常量最终写进 TipInfoMessage.id,而 TipInfoMessage 承载的是**全仓
// 唯一一条扁平数轴**:generated/code/proto/tip/*.proto 里所有枚举共用它,
// common 占 1-19、login 20-51、scene 52-75、team 76-93 …… 一直到 cross_server 129。
// 历史实现绕开这条数轴、从 1 开始手写自己的常量,于是
// ErrFriendListFull=3 与 common 段的 kInvalidTableData=3 完全撞号 —— 客户端按 id
// 查提示表,拿到的是「无效表数据」而不是「好友列表已满」。
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
// 真正的中文文案需要后续把 220-239 补进配表并重新生成 tip proto。
// 在补齐之前,这些 id 至少不再冒充 common 段的其它含义。
const (
	FriendTipCodeLo uint32 = 220
	FriendTipCodeHi uint32 = 239
)

// Tip error IDs returned to client via TipInfoMessage.
// 本组常量全部是 tip 码,取值必须落在 [FriendTipCodeLo, FriendTipCodeHi] 内。
const (
	ErrCannotAddSelf        uint32 = 220
	ErrAlreadyFriends       uint32 = 221
	ErrFriendListFull       uint32 = 222
	ErrRequestAlreadySent   uint32 = 223
	ErrTargetFriendListFull uint32 = 224
	ErrNoPendingRequest     uint32 = 225
	ErrTooManyPending       uint32 = 226
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// 好友域的码**全部**是游戏规则拒绝:加自己、已是好友、列表满、重复申请、
// 没有待处理申请、待处理申请过多。没有一条指向服务端自身或依赖出错
// —— 真正的故障(Redis / MySQL 挂了)在 friend_logic 里是 `return nil, err`,
// 走的是 gRPC status 而不是 in-band 码,拦截器会记成 transport_error。
// 所以这里没有故障码集合;将来若真加了故障码,在下面补一个 map 即可。
//
// 为什么不能直接用 serverbase.TipVerdict:它只认已生成的 tip 数轴(上限 129),
// 220-239 会被判成 VerdictUnknown 并刷 rpc_inband_unknown_code 日志。
// 本函数只接管好友自己的号段,段外的码(例如上游透传的 common 段)仍交回全局判定,
// 这样 serverbase 的「码表漂移」告警语义不受影响。
func TipClassifier() serverbase.Classifier {
	return func(code uint32) serverbase.Verdict {
		if code < FriendTipCodeLo || code > FriendTipCodeHi {
			return serverbase.TipVerdict(code)
		}
		return serverbase.VerdictBizReject
	}
}
