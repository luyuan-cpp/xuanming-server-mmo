// Package constants 是 friend 写进 TipInfoMessage.id 的 tip 码表。
//
// friend 引用**两条**码轴前缀(constants_test.go 的 TestNoHandWrittenTipCodes 机械守住):
//   - FriendError_:Tip.xlsx 的 //friend_error base=15000 段,好友域专用业务码;
//   - CommonError_:参数非法 / 限流 / 存储故障这类跨域语义,与 trade、chat、路由服同一口径。
package constants

import (
	"shared/generated/pb/table"
	"shared/serverbase"
)

// 好友的 tip 码现在由配表统一发号,不再手写。
//
// # 这里为什么变了
//
// 这些常量最终写进 TipInfoMessage.id,而 TipInfoMessage 承载的是**全仓唯一一条
// 数轴**,客户端拿 id 去查文案。历史上这一段是手写的 [220,239] 私有段 ——
// 一个「约定」:它防不住别人(go/match 就从 1 开始重新数了一遍),
// 也拿不到中文文案(Tip.xlsx 的文案列当时根本没有出口)。
//
// 2026-09-02 起改成:段由 data/tip/Tip.xlsx 的组头行声明,发号器按段分配,
// 段表与文案表都是生成产物。好友段是 friend_error base=15000。
// 于是「防重叠」从人的自觉变成发号器的机械保证,文案也随表一起发。
//
// 加一个新码 = 往 Tip.xlsx 的 //friend_error 组里加一行(名字 + 中文),
// 重跑导表器,然后在下面加一行引用。不要再手写数字。
//
// ⚠ F2 批的已知前置(规格 §1.2):下面最后三个码(ErrBlocked / ErrBlockListFull /
// ErrTargetInboxFull)引用的枚举常量**现在还不存在** —— 它们对应的 Tip.xlsx 行要等本分支
// 合回 main 时与 regen 在同一个串行窗口里添加(xlsx 是二进制、无法 3-way 合并,而主工作区
// 有并行会话在改同一个文件)。在导表器跑之前 go/friend 编译不过,这是本仓既有流程
// (guild 的 4 个新码同样如此),不是缺陷。要补的三行(A=名字,B=中文文案,fault 列**留空**,
// 因为三者都是业务拒绝而非服务端故障):
//
//	FriendBlocked          对方已将你拉黑 / 你已将对方拉黑
//	FriendBlockListFull    黑名单已满
//	FriendTargetInboxFull  对方的好友申请已满
//
// 名字按本段既有 7 行的规律推导:xlsx 里写 `FriendXxx`,导表器加 `k` 前缀生成
// `FriendError_kFriendXxx`;"对方的……"用 `FriendTarget` 开头(见 FriendTargetListFull)。
// **合并后必须核对生成的枚举名与这里逐字一致**,不一致就改这里、别改 xlsx 去将就代码。
const (
	ErrCannotAddSelf        = uint32(table.FriendError_kFriendCannotAddSelf)
	ErrAlreadyFriends       = uint32(table.FriendError_kFriendAlreadyFriends)
	ErrFriendListFull       = uint32(table.FriendError_kFriendListFull)
	ErrRequestAlreadySent   = uint32(table.FriendError_kFriendRequestAlreadySent)
	ErrTargetFriendListFull = uint32(table.FriendError_kFriendTargetListFull)
	ErrNoPendingRequest     = uint32(table.FriendError_kFriendNoPendingRequest)
	ErrTooManyPending       = uint32(table.FriendError_kFriendTooManyPending)

	// ErrBlocked:两人之间存在拉黑关系,不许发起好友申请(AddFriend)、也不许通过申请
	// (AcceptFriend —— 拉黑之后对方的旧申请可能还躺在收件箱里)。
	//
	// **两个方向共用这一个码,文案也不区分方向**,是刻意的:回"对方把你拉黑了"等于把别人的
	// 黑名单状态变成一个可探测的接口(挨个发申请就能反查出谁拉黑了自己)。中性的拒绝
	// 既不泄露状态,也足够让客户端停下来 —— 这是 §11.3「敏感信息最少暴露」在这里的具体形态。
	ErrBlocked = uint32(table.FriendError_kFriendBlocked)

	// ErrBlockListFull:自己的黑名单条数已达 Friend.MaxBlocks。
	//
	// 上限本身不是"产品觉得够了",而是 ListBlocks 要一次回给客户端(gate 单包有上限),
	// 且每次 AddFriend 都要按黑名单做判定 —— 无上界的黑名单会让这两处都慢慢退化。
	ErrBlockListFull = uint32(table.FriendError_kFriendBlockListFull)

	// ErrTargetInboxFull:对方的入站待处理申请已达 Friend.MaxIncomingRequests。
	//
	// 与 ErrTooManyPending(我自己挂着的出站申请太多)是两个方向,不能合并成一个码:
	// 撞 ErrTooManyPending 的玩家撤掉自己几条申请就能继续,撞本码的玩家**自己什么都做不了**
	// (要等对方去清收件箱),客户端该给的引导完全不同 —— 同一个码会让前一种情况的玩家
	// 也看到"等对方处理",然后一直等下去。
	//
	// 入站上限存在的理由是防骚扰:出站上限只管得住"单个刷子",100 个小号各发 1 条一样能把
	// 一个玩家的收件箱刷爆,而那是被骚扰者自己清不动的。
	ErrTargetInboxFull = uint32(table.FriendError_kFriendTargetInboxFull)
)

// friend 复用的 common 段码。
//
// # 为什么不在好友段再发三个码
//
// 这三件事都不是好友域特有的语义:客户端没带会话身份、请求过快、存储挂了。
// 每个服务各发一个自己的「参数非法」,客户端就要背 N 份同义文案,
// 告警规则也没法按码跨服务聚合。common 段就是为这类跨域语义留的
// —— 复用 common 码不算「借别人的段」,借段是指把自己的新语义写到别人段的号上。
const (
	// ErrInvalidParameter:取不到会话身份(session.ClientPlayerID 返回 ok=false),
	// 以及目标 player_id 为 0、exclude 条数越界、拉黑自己这类客户端输入问题。
	// (「加自己为好友」有专用码 ErrCannotAddSelf,不走这里;「拉黑自己」没有专用码 ——
	// 它只可能来自客户端 bug 或手拼请求,不需要一条给玩家看的文案。)
	//
	// 身份缺失刻意**不用** kPlayerNotFoundInSession:后者 Tip.xlsx fault 列为 1,语义是
	// 「服务端在自己的会话表里找不到这名玩家」——那是服务端状态缺失。而这里的情况是请求
	// 根本没带会话元数据(或路由服没透传),属于客户端 / 链路问题;用故障码会把它刷成
	// rpc_inband_fault 并触发告警,而这类请求在被扫描、被旧客户端重放时本来就会出现。
	ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)

	// ErrRateLimited:好友申请频率超过 Friend.RequestQuotaPerMinute。
	//
	// 限流是规则拒绝,不是故障:它命中恰恰说明防刷生效了,标成故障等于把「被刷」
	// 记成「我方出错」。配额本体在 internal/logic/rate_quota.go(Redis 计数器),
	// Redis 自己出错时是 fail-open(放行 + 计 metrics.ObserveRateQuota),
	// 所以这个码只在「真的超了」时出现,永远不代表依赖故障。
	ErrRateLimited = uint32(table.CommonError_kRateLimitExceeded)

	// ErrStorage:MySQL / Redis 真故障。**本包唯一允许当 fault 的码**
	// (Tip.xlsx fault 列为 1,serverbase 记成 rpc_inband_fault 并配告警)。
	//
	// 正因为它会告警,绝不能拿它当「功能未实现」或「查不到」的占位 —— 那会让告警长期
	// 常亮,真故障就淹没在噪音里。F1 批新增的四个 RPC 当时因此选择**不实现**
	// (继承 UnimplementedClientPlayerFriendServer)而不是回一个假的 ErrStorage;
	// 这条纪律对将来任何新 RPC 继续有效。
	//
	// F2 批起 friend_logic 的存储故障是 in-band 的(`return resp, nil` + 本码),
	// 不再 `return nil, err`:后者会被路由服翻成信封级错误,客户端根本收不到
	// TipInfoMessage,只能看到一个没有文案的连接失败。定性由 serverbase 那层按
	// Tip.xlsx 的 fault 列做,所以这里只管「回哪个码」。
	ErrStorage = uint32(table.CommonError_kServiceUnavailable)
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// **好友段**的 10 个码全部是游戏规则拒绝:加自己、已是好友、双方列表满、重复申请、
// 没有待处理申请、出站/入站申请过多、存在拉黑关系、黑名单满。没有一条指向服务端自身
// 或依赖出错 —— 拉黑与配额这类「被拒绝」尤其不是故障,它们命中说明规则在生效。
// 本包唯一的故障码是复用 common 段的 ErrStorage(kServiceUnavailable);
// 而它「算故障」这件事是 Tip.xlsx 的 fault 列说的,不是这个函数说的。
//
// 好友段进配表之后,serverbase.TipVerdict 已经认得这些码(不再判 VerdictUnknown),
// 且两条轴的定性都已在表里,所以这里直接交回全局判定。将来好友段真加了故障码,
// 在 Tip.xlsx 那一行的 fault 列标 1 即可(生成到 tip.Faults,全局判定直接认得),
// **不要**在这里包一层本地 map —— 那就是把「码的属性和码定义分家」重新制造出来。
//
// F2 批把存储故障改成 in-band 之后,ErrStorage 真的会经这个定性函数走到
// rpc_inband_fault(F1 批它走的是 gRPC status / transport_error)。也就是说
// 「friend 有多少真故障」这条曲线从现在起才有意义,之前恒为 0 并不代表没出过错。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}
