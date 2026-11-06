package logic

import matchpb "proto/match"

// 战报回放的录制策略(docs/design/battle-report-replay.md BR-8、§7.1):「这局录不录回放」只在本文件决定。
// gather 建房时调用一次 recordReplayFor,结果写进 CreateBattleRequest.record_replay,随原件冻结
// (随 formation 的 create_request 落库)。之后只读冻结值:battle 按它录,match 终局事务按它写战报与列表。
// 负责人 2026-11-06 定:只录 PVP;每名玩家保留最近 100 场(authority.BattleReportsKeptPerPlayer)。
// 活动局(帮会历练等)不单独判断,跟着自己的模式走。

// recordReplayFor 是「这局录不录回放」的唯一权威。enabled 是全局开关 BattleReport.Enabled,
// 关掉后新成局一律不录;表外的模式不录。
func recordReplayFor(enabled bool, mode matchpb.MatchMode) bool {
	if !enabled {
		return false
	}
	record, decided := replayPolicyOfMode(mode)
	return decided && record
}

// replayPolicyOfMode 是按对局模式的显式决策表:每个 MatchMode 枚举值各占一行、各写理由。decided=false
// 只给表外的值(此时不录)。TestReplayPolicyDecidesEveryMatchMode 遍历 MatchMode_name 守住「每个枚举值都在表里」:
// proto 新增模式而这里没加一行,测试即红,不会被 default 静默归到哪一边。
func replayPolicyOfMode(mode matchpb.MatchMode) (record bool, decided bool) {
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_1V1:
		return true, true // 计分 PVP:玩家之间的对局,最需要复盘和对证
	case matchpb.MatchMode_MATCH_MODE_5V5:
		return true, true // 计分 PVP(同上)
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		return true, true // 真人人数档 PVP
	case matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		return true, true // 点名切磋:也是玩家之间的对局
	case matchpb.MatchMode_MATCH_MODE_3V3:
		return true, true // 未开放;开放后按 PVP 录
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		return false, true // 刷怪局最多、最频繁,不录(2026-11-06 定)
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		return false, true // 同上;帮会历练等活动局也走这个模式
	case matchpb.MatchMode_MATCH_MODE_UNSPECIFIED:
		return false, true // 漏填模式
	default:
		return false, false
	}
}
