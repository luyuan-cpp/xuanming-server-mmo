package logic

import (
	"context"

	"match/internal/svc"

	matchpb "proto/match"
)

// 整队开战的票据域实现(设计文档 docs/design/team-system.md §E.1)。
//
// 分工(依赖方向 team → 接口 ← logic):
//   - team 包(internal/team)持有队伍记录与编排:读记录、惰性转让、队长校验、roster 排序、
//     会话 / 战斗锁 / 位置预检、持久 launch 原件、推送;
//   - 本文件与 global_team_launch.go 只做"票据领域"的几件事,方法集满足 team.BattleStarter。
//     logic 不 import team:team 依赖 proto/team 生成物,logic 也就不认识 TeamRecord,只拿 team 给的原件。
//
// 线程安全:TeamBattleStarter 无可变状态,可并发调用。

// teamMatchLockSlackSeconds 开战锁时长在"备战预算 + 补偿窗口"之上的余量(§E.1 第 6 步)。
const teamMatchLockSlackSeconds = 10

// TeamBattleStarter 整队开战的票据域端口实现(team.BattleStarter),match_service.go 装配时注入
// team.NewService。
type TeamBattleStarter struct {
	svcCtx *svc.ServiceContext
}

// NewTeamBattleStarter svcCtx 需要 Authority(票据与成局)、Catalog(组队人数上限)与 Config(备战预算)。
func NewTeamBattleStarter(svcCtx *svc.ServiceContext) *TeamBattleStarter {
	return &TeamBattleStarter{svcCtx: svcCtx}
}

// TeamSizeFor 模式 mode 在模板 battleConfigId 上的每队人数上限,与 JoinQueue 同一口径:
// PlayMode 表的人数上限,按 kMaxBattleTeamSize 收口;PVP_TEAM 是每侧人数。
// 0 = 该模板没有开放这个模式(或不是组队模式),组队入口据此拒绝。
func (s *TeamBattleStarter) TeamSizeFor(mode, battleConfigId uint32) uint32 {
	switch matchpb.MatchMode(mode) {
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		return min(s.svcCtx.Catalog.MaxTeamSize(matchpb.MatchMode(mode), battleConfigId), kMaxBattleTeamSize)
	default:
		return 0
	}
}

// MatchLockTTLSeconds memberCount 人的开战锁时长(秒,§E.1 第 6 步):备战预算(gather 最坏链路)
// + 补偿窗口(逐人 CancelBattlePrepare)+ 余量。5 人 = 53 + 25 + 10 = 88s。
func (s *TeamBattleStarter) MatchLockTTLSeconds(memberCount int) int {
	return matchedTicketTTLFor(s.svcCtx, uint32(memberCount)) + compensationTicketTTLFor(memberCount) + teamMatchLockSlackSeconds
}

// TicketBlocked 一名成员的票据预检(§E.1 第 5 步最后一项):持有未结束的票据 → true。err 非 nil 时返回值无意义,调用方 fail-closed。
func (s *TeamBattleStarter) TicketBlocked(ctx context.Context, playerId uint64) (bool, error) {
	return s.svcCtx.Authority.TeamMemberHasTicket(ctx, playerId)
}

// PartyQueueMode 视图模式来自本人当前票据;不在队伍记录里制造第二份匹配状态。
func (s *TeamBattleStarter) PartyQueueMode(ctx context.Context, playerId, teamId uint64) (uint32, error) {
	ticket, err := s.svcCtx.Authority.Ticket(ctx, playerId)
	if err != nil || ticket == nil || ticket.TeamId != teamId {
		return 0, err
	}
	return ticket.Mode, nil
}

// PartyQueueState playerId 是否正随 teamId 在排队(team-backfill-matching.md §4.5,team 用它装饰队伍视图)。
// 返回 (constants.PartyQueue*, 目标人数, 副本, 入队时刻毫秒, err);后三项只在 Queued 时有值。
func (s *TeamBattleStarter) PartyQueueState(ctx context.Context, playerId, teamId uint64) (uint32, uint32, uint32, uint64, error) {
	return s.svcCtx.Authority.TeamQueueState(ctx, playerId, teamId)
}
