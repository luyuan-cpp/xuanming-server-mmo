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
// 公会段进配表之后,serverbase.TipVerdict 已经认得这些码(不再判 VerdictUnknown),
// 所以这里只剩一件事:声明本域里哪些码算「服务端故障」。
// 这是**码的属性**,理应和码定义在一起(Tip.xlsx 加一列 fault 就能收口),
// 目前仍散在各服务 —— 与刚修掉的「段和发号分家」是同一类病,见
// docs/design/tip-code-axis.md 的「已知残留」。
func TipClassifier() serverbase.Classifier {
	return func(code uint32) serverbase.Verdict {
		if _, ok := faultCodes[code]; ok {
			return serverbase.VerdictFault
		}
		return serverbase.TipVerdict(code)
	}
}
