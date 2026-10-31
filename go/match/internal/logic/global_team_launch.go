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
	formation, group, err := buildGlobalDirect(ctx, s.svcCtx, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, launch.ConfigId, roster, tickets, directOptions{teamID: launch.TeamId})
	if err != nil {
		return nil, err
	}
	applyLaunchHomes(group, data)
	formation, err = s.svcCtx.Authority.ReserveTeamLaunch(ctx, launch, formation, group)
	if err != nil {
		return nil, err
	}
	launchGlobalFormation(s.svcCtx, formation)
	return formation, nil
}

// applyLaunchHomes 把原件记的归属抄到票上。buildGlobalDirect 给点名成局建票不填 home(切磋、活动不看区),
// 整队直接开战的提交点(ReserveTeamLaunch)要逐项和原件核对,缺了就是 ErrTeamRoute,这一单永远提交不了。
func applyLaunchHomes(group []*matchpb.MatchAuthorityTicketRecord, data *teampb.TeamLaunchData) {
	for _, ticket := range group {
		ticket.ZoneId = data.GetOriginal().GetZones()[ticket.PlayerId]
	}
}
