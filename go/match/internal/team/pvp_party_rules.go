package team

import matchpb "proto/match"

// CheckPvpPartySize 是人数档真人 PVP 的组队人数规则(docs/design/global-match-hierarchical-pools.md 开头第 9 条,
// 借鉴 LoL 灵活组排):每侧 5 人的池不收 4 人队,免得一个单人被塞进 4 黑。4 人队可以打每侧 4 人的池,
// 或者等第 5 人进队;其余人数档、其余队伍人数不受限。perSide 是开战请求的每侧人数,members 是队伍人数。
// 返回 0 表示通过。StartTeamMatch 入口先判,开战原件的提交事务里按锁住的名单再判一次。
func CheckPvpPartySize(mode matchpb.MatchMode, perSide, members uint32) uint32 {
	if mode == matchpb.MatchMode_MATCH_MODE_PVP_TEAM && perSide == 5 && members == 4 {
		return ErrPvpPartySize
	}
	return 0
}
