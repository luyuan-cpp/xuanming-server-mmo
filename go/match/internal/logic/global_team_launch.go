package logic

import (
	"context"
	"errors"

	"match/internal/authority"

	matchpb "proto/match"
	teampb "proto/team"
)

// 首次请求和后台恢复共用持久launch原件；后台不拼造或重新捕获会话。
func (s *TeamBattleStarter) ReserveSQLTeamMatch(ctx context.Context, launch *teampb.TeamLaunchRecord) (*matchpb.MatchFormationRecord, error) {
	data, err := authority.DecodeTeamLaunch(launch)
	if err != nil {
		return nil, err
	}
	if launch.BattleId != 0 {
		return s.svcCtx.Authority.Formation(ctx, launch.BattleId)
	}
	if launch.State != 1 {
		return nil, errors.New("team: 原launch不在待提交态")
	}
	for _, exit := range data.SpectatorExits {
		if err = closeCapturedSpectatorExit(ctx, s.svcCtx, exit); err != nil {
			return nil, err
		}
	}
	if authority.TeamLaunchMode(launch) == uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM) || launch.TargetSize > launch.MemberCount {
		if err = s.svcCtx.Authority.EnqueuePreparedTeamLaunch(ctx, launch); err != nil {
			return nil, err
		}
		return nil, nil
	}
	roster := make([]uint64, 0, len(data.Members))
	tickets := map[uint64]string{}
	for _, member := range data.Members {
		roster = append(roster, member.Original.PlayerId)
		tickets[member.Original.PlayerId] = member.Ticket
	}
	formation, group, err := buildGlobalDirect(ctx, s.svcCtx, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, launch.ConfigId, roster, tickets, gatherOptions{directTeamID: launch.TeamId})
	if err != nil {
		return nil, err
	}
	formation, err = s.svcCtx.Authority.ReserveTeamLaunch(ctx, launch, formation, group)
	if err != nil {
		return nil, err
	}
	launchGlobalFormation(s.svcCtx, formation)
	return formation, nil
}
