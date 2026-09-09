package constants

import (
	"shared/generated/pb/table"
	"shared/serverbase"
)

// Guild member roles.
const (
	RoleMember  uint32 = 0
	RoleOfficer uint32 = 1
	RoleLeader  uint32 = 3
)

// 公会的 tip 码现在由配表统一发号,不再手写。
//
// # 这里为什么变了
//
// 这些常量最终写进 TipInfoMessage.id,而 TipInfoMessage 承载的是**全仓唯一一条
// 数轴**,客户端拿 id 去查文案。历史上这一段是手写的 [200,219] 私有段 ——
// 一个「约定」:它防不住别人(go/match 就从 1 开始重新数了一遍,ErrInBattle=1
// 直接压在 common 段上),也拿不到中文文案(Tip.xlsx 的文案列当时根本没有出口)。
//
// 2026-09-02 起改成:段由 data/tip/Tip.xlsx 的组头行声明,发号器按段分配,
// 段表与文案表都是生成产物。公会段是 guild_error base=14000。
// 于是「防重叠」从人的自觉变成发号器的机械保证,文案也随表一起发。
//
// 加一个新码 = 往 Tip.xlsx 的 //guild_error 组里加一行(名字 + 中文),
// 重跑导表器,然后在下面加一行引用。不要再手写数字。
const (
	ErrAlreadyInGuild  = uint32(table.GuildError_kGuildAlreadyInGuild)
	ErrGuildNotFound   = uint32(table.GuildError_kGuildNotFound)
	ErrNotInGuild      = uint32(table.GuildError_kGuildNotInGuild)
	ErrGuildFull       = uint32(table.GuildError_kGuildFull)
	ErrLeaderCantLeave = uint32(table.GuildError_kGuildLeaderCantLeave)
	ErrNotLeader       = uint32(table.GuildError_kGuildNotLeader)
	ErrNoPermission    = uint32(table.GuildError_kGuildNoPermission)
	ErrNotRanked       = uint32(table.GuildError_kGuildNotRanked)
	// ErrIDGenUnavailable:发号器已被 fence(worker id 的 etcd 租约丢了),
	// 本次建帮整体失败。客户端重试即可 —— 进程会退出并由编排重拉、重新拿号。
	ErrIDGenUnavailable = uint32(table.GuildError_kGuildIdGenUnavailable)
)

// 合服闸门(logic.checkMergeFence)刻意**没有**在这里加码。
//
// 它拒绝时走 gRPC status(FailedPrecondition),不是 TipInfoMessage。理由是本文件
// 顶上那条规矩:tip 码只能由 data/tip/Tip.xlsx 的 //guild_error 组发号,
// 而 Tip.xlsx 与生成产物不在本次改动的范围内;借用别的段(common 的
// kFeatureUnavailable 之类)会当场撞上 TestNoHandWrittenTipCodes 的护栏 ——
// 那条护栏正是为了防止"随手挪一个别处的码"这种做法,不能为了省事把它绕开。
//
// 后果是玩家侧看到的是一条通用错误而不是定制文案。要补文案:往 Tip.xlsx 的
// //guild_error 组加一行(例如 kGuildZoneMerging),重跑导表器,在上面的 const 块
// 加一行引用,然后把 checkMergeFence 的返回改回 tipErr —— 调用点只有一处。

// Default limits.
const (
	DefaultMaxMembers uint32 = 50
	DefaultInitLevel  uint32 = 1
)

// TipClassifier 返回本服务的 in-band 业务码定性函数,供 serverbase.UnaryInterceptor 使用。
//
// 公会段进配表之后,serverbase.TipVerdict 已经认得这些码(不再判 VerdictUnknown);
// 「哪个码算服务端故障」也进了表 —— Tip.xlsx 的 fault 列,ErrIDGenUnavailable
// (发号器被 fence:本进程已不是该 worker id 的合法持有者,随后会主动退出,必须能被看见)
// 就是在那里标的 1。以前这里有一张本地 faultCodes map 专门补这一条,
// 那正是「码的属性和码定义分家」的病灶,2026-09-05 随 fault 列一起删掉了。
//
// 所以这里直接交回全局判定。**不要再往这里加本地 map** —— 要改某个码的分类,改表。
func TipClassifier() serverbase.Classifier {
	return serverbase.TipVerdict
}
