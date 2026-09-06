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
const (
	ErrCannotAddSelf        = uint32(table.FriendError_kFriendCannotAddSelf)
	ErrAlreadyFriends       = uint32(table.FriendError_kFriendAlreadyFriends)
	ErrFriendListFull       = uint32(table.FriendError_kFriendListFull)
	ErrRequestAlreadySent   = uint32(table.FriendError_kFriendRequestAlreadySent)
	ErrTargetFriendListFull = uint32(table.FriendError_kFriendTargetListFull)
	ErrNoPendingRequest     = uint32(table.FriendError_kFriendNoPendingRequest)
	ErrTooManyPending       = uint32(table.FriendError_kFriendTooManyPending)
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// 好友域的码**全部**是游戏规则拒绝:加自己、已是好友、列表满、重复申请、
// 没有待处理申请、待处理申请过多。没有一条指向服务端自身或依赖出错
// —— 真正的故障(Redis / MySQL 挂了)在 friend_logic 里是 `return nil, err`,
// 走的是 gRPC status 而不是 in-band 码,拦截器会记成 transport_error。
//
// 好友段进配表之后,serverbase.TipVerdict 已经认得这些码(不再判 VerdictUnknown),
// 且本域没有故障码,所以这里直接交回全局判定。将来若真加了故障码,
// 在 Tip.xlsx 那一行的 fault 列标 1 即可(生成到 tip.Faults,全局判定直接认得),
// **不要**在这里包一层本地 map —— 那就是把「码的属性和码定义分家」重新制造出来。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}
