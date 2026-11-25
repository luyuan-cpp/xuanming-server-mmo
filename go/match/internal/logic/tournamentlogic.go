package logic

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/playercontract"
	"match/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	cgpb "proto/content_guard"
	mailpb "proto/mail"
	matchpb "proto/match"
	"shared/contentguard"
	"shared/serviceauth"
)

const (
	tournamentMailSenderKind uint32 = 6 // 对应 go/mail SenderKind("match") = 6
	tournamentMailCaller            = "match"
	tournamentMailTTLMs      uint64 = 30 * 24 * 3600 * 1000
	tournamentLoopInterval          = 5 * time.Second
)

// TournamentLogic 实现全服赛季战队锦标赛《群仙问道赛》的 11 条客户端 RPC(465..475)。
type TournamentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTournamentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TournamentLogic {
	return &TournamentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func tournamentTip(id uint32) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id}
}

func tournamentErrorTip(err error) uint32 {
	switch {
	case errors.Is(err, authority.ErrTournamentNotOpen):
		return constants.ErrTournamentNotOpen
	case errors.Is(err, authority.ErrTournamentStageClosed):
		return constants.ErrTournamentStageClosed
	case errors.Is(err, authority.ErrTournamentSquadNameInvalid):
		return constants.ErrTournamentSquadNameInvalid
	case errors.Is(err, authority.ErrTournamentAlreadyInSquad):
		return constants.ErrTournamentAlreadyInSquad
	case errors.Is(err, authority.ErrTournamentSquadNotFound):
		return constants.ErrTournamentSquadNotFound
	case errors.Is(err, authority.ErrTournamentNotCaptain):
		return constants.ErrTournamentNotCaptain
	case errors.Is(err, authority.ErrTournamentSquadFull):
		return constants.ErrTournamentSquadFull
	case errors.Is(err, authority.ErrTournamentSquadLocked):
		return constants.ErrTournamentSquadLocked
	case errors.Is(err, authority.ErrTournamentInviteNotFound):
		return constants.ErrTournamentInviteNotFound
	case errors.Is(err, authority.ErrTournamentMemberNotEnough):
		return constants.ErrTournamentMemberNotEnough
	case errors.Is(err, authority.ErrTournamentLineupInvalid):
		return constants.ErrTournamentLineupInvalid
	case errors.Is(err, authority.ErrTournamentQualifierLimit):
		return constants.ErrTournamentQualifierLimit
	case errors.Is(err, authority.ErrTournamentMatchNotReady):
		return constants.ErrTournamentMatchNotReady
	case errors.Is(err, authority.ErrTournamentAlreadyCheered):
		return constants.ErrTournamentAlreadyCheered
	case errors.Is(err, authority.ErrTournamentRewardUnavailable):
		return constants.ErrTournamentRewardUnavailable
	default:
		return constants.ErrInternal
	}
}

func (l *TournamentLogic) lookupPlayerZone(pid uint64) uint32 {
	if l.svcCtx == nil {
		return 0
	}
	if loc, err := playercontract.LoadLocation(l.ctx, l.svcCtx, pid); err == nil && loc != nil {
		return loc.GetZoneId()
	}
	return 0
}

func (l *TournamentLogic) GetTournamentOverview(in *matchpb.GetTournamentOverviewRequest) (*matchpb.GetTournamentOverviewResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTournamentOverviewResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	resp, err := l.svcCtx.Authority.LoadTournamentOverview(l.ctx, playerID, in.GetIncludeHistory())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] GetTournamentOverview failed player=%d: %v", playerID, err)
		}
		return &matchpb.GetTournamentOverviewResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	resp.ErrorMessage = tournamentTip(0)
	return resp, nil
}

func (l *TournamentLogic) CreateTournamentSquad(in *matchpb.CreateTournamentSquadRequest) (*matchpb.CreateTournamentSquadResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	// UGC 队名先走 content_guard 内容安全审核(事务外执行,绝不放进 SQL 事务)。
	if l.svcCtx.ContentGuard != nil {
		res := l.svcCtx.ContentGuard.Check(l.ctx, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, playerID, in.GetSquadName())
		if res.Verdict != contentguard.Pass {
			return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrTournamentSquadNameInvalid)}, nil
		}
	}
	squadID, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		l.Errorf("[tournament] CreateTournamentSquad id generate failed player=%d: %v", playerID, err)
		return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	playerName := challengePublicName(l.ctx, l.svcCtx, playerID)
	zoneID := l.lookupPlayerZone(playerID)
	squad, err := l.svcCtx.Authority.CreateTournamentSquad(l.ctx, playerID, playerName, zoneID, in.GetSquadName(), squadID)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] CreateTournamentSquad failed player=%d: %v", playerID, err)
		}
		return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(0), Squad: squad}, nil
}

func (l *TournamentLogic) InviteTournamentMember(in *matchpb.InviteTournamentMemberRequest) (*matchpb.InviteTournamentMemberResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.InviteTournamentMemberResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	inviterName := challengePublicName(l.ctx, l.svcCtx, playerID)
	inv, err := l.svcCtx.Authority.InviteTournamentMember(l.ctx, playerID, in.GetTargetPlayerId(), inviterName)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] InviteTournamentMember failed captain=%d target=%d: %v", playerID, in.GetTargetPlayerId(), err)
		}
		return &matchpb.InviteTournamentMemberResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.InviteTournamentMemberResponse{ErrorMessage: tournamentTip(0), Invite: inv}, nil
}

func (l *TournamentLogic) RespondTournamentInvite(in *matchpb.RespondTournamentInviteRequest) (*matchpb.RespondTournamentInviteResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.RespondTournamentInviteResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	playerName := challengePublicName(l.ctx, l.svcCtx, playerID)
	zoneID := l.lookupPlayerZone(playerID)
	squad, err := l.svcCtx.Authority.RespondTournamentInvite(l.ctx, playerID, playerName, zoneID, in.GetSquadId(), in.GetAccept())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] RespondTournamentInvite failed player=%d squad=%d: %v", playerID, in.GetSquadId(), err)
		}
		return &matchpb.RespondTournamentInviteResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.RespondTournamentInviteResponse{ErrorMessage: tournamentTip(0), Squad: squad}, nil
}

func (l *TournamentLogic) LeaveTournamentSquad(in *matchpb.LeaveTournamentSquadRequest) (*matchpb.LeaveTournamentSquadResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.LeaveTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	disbanded, squad, err := l.svcCtx.Authority.LeaveTournamentSquad(l.ctx, playerID, in.GetTargetPlayerId())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] LeaveTournamentSquad failed actor=%d target=%d: %v", playerID, in.GetTargetPlayerId(), err)
		}
		return &matchpb.LeaveTournamentSquadResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.LeaveTournamentSquadResponse{
		ErrorMessage:   tournamentTip(0),
		SquadDisbanded: disbanded,
		Squad:          squad,
	}, nil
}

func (l *TournamentLogic) UpdateTournamentLineup(in *matchpb.UpdateTournamentLineupRequest) (*matchpb.UpdateTournamentLineupResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.UpdateTournamentLineupResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	squad, err := l.svcCtx.Authority.UpdateTournamentLineup(l.ctx, playerID, in.GetStarterPlayerIds())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] UpdateTournamentLineup failed captain=%d: %v", playerID, err)
		}
		return &matchpb.UpdateTournamentLineupResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.UpdateTournamentLineupResponse{ErrorMessage: tournamentTip(0), Squad: squad}, nil
}

func (l *TournamentLogic) RegisterTournamentSquad(_ *matchpb.RegisterTournamentSquadRequest) (*matchpb.RegisterTournamentSquadResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.RegisterTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	squad, err := l.svcCtx.Authority.RegisterTournamentSquad(l.ctx, playerID)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] RegisterTournamentSquad failed captain=%d: %v", playerID, err)
		}
		return &matchpb.RegisterTournamentSquadResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.RegisterTournamentSquadResponse{ErrorMessage: tournamentTip(0), Squad: squad}, nil
}

func (l *TournamentLogic) ListTournamentStandings(in *matchpb.ListTournamentStandingsRequest) (*matchpb.ListTournamentStandingsResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.ListTournamentStandingsResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	resp, err := l.svcCtx.Authority.ListTournamentStandings(l.ctx, in.GetSeasonId(), playerID, in.GetPage(), in.GetPageSize())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] ListTournamentStandings failed season=%d: %v", in.GetSeasonId(), err)
		}
		return &matchpb.ListTournamentStandingsResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	resp.ErrorMessage = tournamentTip(0)
	return resp, nil
}

func (l *TournamentLogic) StartTournamentMatch(in *matchpb.StartTournamentMatchRequest) (*matchpb.StartTournamentMatchResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	// 海选可能新建等待单,先在事务外预分配 match_id(绝不在 SQL 事务内调发号器)。
	var preMatchID uint64
	if in.GetMatchId() == 0 {
		var err error
		preMatchID, err = l.svcCtx.BattleIDGen.Generate()
		if err != nil {
			l.Errorf("[tournament] StartTournamentMatch id generate failed player=%d: %v", playerID, err)
			return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
		}
	}
	matchView, launch, err := l.svcCtx.Authority.PrepareTournamentMatch(l.ctx, playerID, in.GetMatchId(), in.GetLineupPlayerIds(), preMatchID)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] StartTournamentMatch prepare failed captain=%d: %v", playerID, err)
		}
		return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	if launch == nil {
		return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(0), Match: matchView}, nil
	}

	// 双方就绪:沿用既有直接开战管线(Kind=4 锦标赛直接成局)与 gather 建房。
	members := make([]uint64, 0, len(launch.TeamA)+len(launch.TeamB))
	members = append(members, launch.TeamA...)
	members = append(members, launch.TeamB...)
	key := &matchpb.MatchDirectIntentRecord{
		Kind:       4,
		OwnerId:    launch.MatchID,
		RequestLow: launch.MatchID,
	}
	payload := encodeTournamentLaunchPayload(launch)
	formation, err := reserveGlobalDirect(l.ctx, l.svcCtx, key, payload, matchpb.MatchMode_MATCH_MODE_SEASON_TOURNAMENT, launch.BattleConfigID, members, directOptions{})
	if err != nil {
		l.Errorf("[tournament] reserveGlobalDirect failed match=%d: %v", launch.MatchID, err)
		_ = l.svcCtx.Authority.BindTournamentMatchBattle(context.Background(), launch.MatchID, 0, false)
		return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(constants.ErrTournamentMatchNotReady), Match: matchView}, nil
	}
	if err := l.svcCtx.Authority.BindTournamentMatchBattle(l.ctx, launch.MatchID, formation.BattleId, launch.Featured); err != nil {
		l.Errorf("[tournament] BindTournamentMatchBattle failed match=%d battle=%d: %v", launch.MatchID, formation.BattleId, err)
	}
	if formation.State < 4 {
		launchGlobalFormation(l.svcCtx, formation)
	}
	matchView.BattleId = formation.BattleId
	matchView.State = matchpb.TournamentMatchState_TOURNAMENT_MATCH_STATE_IN_BATTLE
	matchView.FeaturedSpectate = launch.Featured
	return &matchpb.StartTournamentMatchResponse{
		ErrorMessage: tournamentTip(0),
		Match:        matchView,
		BattleId:     formation.BattleId,
	}, nil
}

func encodeTournamentLaunchPayload(spec *authority.TournamentLaunchSpec) []byte {
	buf := make([]byte, 16+8*(len(spec.TeamA)+len(spec.TeamB)))
	binary.LittleEndian.PutUint64(buf[0:], spec.MatchID)
	binary.LittleEndian.PutUint32(buf[8:], spec.SeasonID)
	binary.LittleEndian.PutUint32(buf[12:], spec.BattleConfigID)
	offset := 16
	for _, pid := range spec.TeamA {
		binary.LittleEndian.PutUint64(buf[offset:], pid)
		offset += 8
	}
	for _, pid := range spec.TeamB {
		binary.LittleEndian.PutUint64(buf[offset:], pid)
		offset += 8
	}
	return buf
}

func (l *TournamentLogic) CheerTournamentSquad(in *matchpb.CheerTournamentSquadRequest) (*matchpb.CheerTournamentSquadResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.CheerTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	count, err := l.svcCtx.Authority.CheerTournamentSquad(l.ctx, playerID, in.GetSquadId())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] CheerTournamentSquad failed player=%d squad=%d: %v", playerID, in.GetSquadId(), err)
		}
		return &matchpb.CheerTournamentSquadResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.CheerTournamentSquadResponse{
		ErrorMessage:    tournamentTip(0),
		SquadId:         in.GetSquadId(),
		SquadCheerCount: count,
	}, nil
}

func (l *TournamentLogic) ClaimTournamentReward(in *matchpb.ClaimTournamentRewardRequest) (*matchpb.ClaimTournamentRewardResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	pending, err := l.svcCtx.Authority.ClaimableTournamentRewards(l.ctx, playerID, in.GetSeasonId())
	if err != nil {
		l.Errorf("[tournament] ClaimableTournamentRewards failed player=%d: %v", playerID, err)
		return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	if len(pending) == 0 {
		return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrTournamentRewardUnavailable)}, nil
	}
	cat := l.svcCtx.ActiveTournament()
	claimed := make([]*matchpb.TournamentPlayerRewardView, 0, len(pending))
	for _, r := range pending {
		mailID := uint64(0)
		// 若配置了 MailAdminClient,走既有幂等个人邮件投递附件(request_id = reward_id 绝不重发);
		// 未配置时仍幂等落账已领取状态供本地/测试与荣誉展示。
		if l.svcCtx.MailAdmin != nil && l.svcCtx.MailSecret != "" {
			var atts mailpb.MailAttachments
			if len(r.Attachments) > 0 {
				if err := proto.Unmarshal(r.Attachments, &atts); err != nil {
					l.Errorf("[tournament] Corrupt attachments for reward=%d: %v", r.RewardId, err)
					return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
				}
			}
			title := "群仙问道赛赛季奖励"
			body := "本赛季群仙问道赛奖励已结算，请查收附件。"
			if tierRow, ok := cat.RewardByID(r.TierId); ok {
				if tierRow.GetMailTitle() != "" {
					title = tierRow.GetMailTitle()
				}
				if tierRow.GetMailBody() != "" {
					body = tierRow.GetMailBody()
				}
			}
			nowMs := uint64(time.Now().UnixMilli())
			mailReq := &mailpb.SendMailRequest{
				SenderKind:        tournamentMailSenderKind,
				RequestId:         r.RewardId,
				RecipientPlayerId: playerID,
				Title:             title,
				Body:              body,
				Attachments:       &atts,
				ExpireMs:          nowMs + tournamentMailTTLMs,
			}
			if err := serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, mailReq, tournamentMailCaller, l.svcCtx.MailSecret, nowMs); err != nil {
				l.Errorf("[tournament] Sign SendPersonalMail failed reward=%d: %v", r.RewardId, err)
				return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
			}
			callCtx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
			mailResp, callErr := l.svcCtx.MailAdmin.SendPersonalMail(callCtx, mailReq)
			cancel()
			if callErr != nil || mailResp.GetErrorMessage().GetId() != 0 || mailResp.GetMailId() == 0 {
				l.Errorf("[tournament] SendPersonalMail failed reward=%d tip=%d err=%v", r.RewardId, mailResp.GetErrorMessage().GetId(), callErr)
				return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
			}
			mailID = mailResp.GetMailId()
		}
		v, err := l.svcCtx.Authority.MarkTournamentRewardClaimed(l.ctx, r.SeasonId, playerID, r.TierKind, mailID)
		if err != nil {
			l.Errorf("[tournament] MarkTournamentRewardClaimed failed reward=%d: %v", r.RewardId, err)
			return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
		}
		claimed = append(claimed, v)
	}
	return &matchpb.ClaimTournamentRewardResponse{
		ErrorMessage:   tournamentTip(0),
		ClaimedRewards: claimed,
	}, nil
}

// StartTournamentLoop 启动全服赛季锦标赛阶段推进循环(按 OwnsWork("tournament:driver") 选主,SQL 提交点兜底幂等)。
func StartTournamentLoop(ctx context.Context, svcCtx *svc.ServiceContext) {
	if svcCtx == nil || svcCtx.Authority == nil || svcCtx.BattleIDGen == nil {
		return
	}
	svcCtx.LaunchRelayTask("match.tournament.driver", func() {
		ticker := time.NewTicker(tournamentLoopInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !svcCtx.OwnsWork("tournament:driver") {
					continue
				}
				tickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := svcCtx.Authority.AdvanceTournamentSeason(tickCtx, 0, func(count int) ([]uint64, error) {
					ids := make([]uint64, 0, count)
					for i := 0; i < count; i++ {
						id, e := svcCtx.BattleIDGen.Generate()
						if e != nil {
							return nil, fmt.Errorf("allocate tournament id %d/%d: %w", i+1, count, e)
						}
						ids = append(ids, id)
					}
					return ids, nil
				})
				cancel()
				if err != nil && ctx.Err() == nil {
					logx.Errorf("[tournament] season advance tick failed: %v", err)
				}
			}
		}
	})
}
