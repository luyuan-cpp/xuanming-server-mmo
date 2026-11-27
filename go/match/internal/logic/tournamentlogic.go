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
	"match/internal/tournament"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	base "proto/common/base"
	cgpb "proto/content_guard"
	mailpb "proto/mail"
	matchpb "proto/match"
	"shared/contentguard"
	"shared/generated/pb/table"
	"shared/serviceauth"
)

const (
	tournamentMailSenderKind uint32 = 6 // 对应 go/mail SenderKind("match") = 6
	tournamentMailCaller            = "match"
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
	case errors.Is(err, authority.ErrTournamentEmblemInvalid):
		return constants.ErrTournamentEmblemInvalid
	case errors.Is(err, authority.ErrTournamentPredictionClosed):
		return constants.ErrTournamentPredictionClosed
	case errors.Is(err, authority.ErrTournamentPredictionTargetInvalid):
		return constants.ErrTournamentPredictionTargetInvalid
	case errors.Is(err, authority.ErrTournamentWarmupUnavailable):
		return constants.ErrTournamentWarmupUnavailable
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
		if res.Verdict == contentguard.Unavailable {
			// 内容安全不可达:没检过的名字不建队,但这不是名字的问题,回服务端故障让玩家稍后再试。
			return &matchpb.CreateTournamentSquadResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
		}
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
	squad, err := l.svcCtx.Authority.CreateTournamentSquad(l.ctx, playerID, playerName, zoneID, in.GetSquadName(), in.GetEmblemId(), squadID)
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

	battleID, launchErr := launchTournamentSpec(l.ctx, l.svcCtx, launch)
	if launchErr != nil {
		l.Errorf("[tournament] launchTournamentSpec failed match=%d game=%d: %v", launch.MatchID, launch.GameIndex, launchErr)
		return &matchpb.StartTournamentMatchResponse{ErrorMessage: tournamentTip(constants.ErrTournamentMatchNotReady), Match: matchView}, nil
	}
	matchView.BattleId = battleID
	matchView.State = matchpb.TournamentMatchState_TOURNAMENT_MATCH_STATE_IN_BATTLE
	matchView.FeaturedSpectate = launch.Featured
	matchView.IntermissionDeadlineMs = 0
	return &matchpb.StartTournamentMatchResponse{
		ErrorMessage: tournamentTip(0),
		Match:        matchView,
		BattleId:     battleID,
	}, nil
}

// launchTournamentSpec 复用既有直接开战管线(Kind=4 锦标赛直接成局)发起单局建房并绑定 battle_id。
// BO3 各小局以 RequestHigh=game_index 隔离直接开局幂等键,避免第 2/3 局复用第 1 局原件。
func launchTournamentSpec(ctx context.Context, svcCtx *svc.ServiceContext, launch *authority.TournamentLaunchSpec) (uint64, error) {
	members := make([]uint64, 0, len(launch.TeamA)+len(launch.TeamB))
	members = append(members, launch.TeamA...)
	members = append(members, launch.TeamB...)
	gameIdx := launch.GameIndex
	if gameIdx == 0 {
		gameIdx = 1
	}
	bestOf := launch.BestOf
	if bestOf == 0 {
		bestOf = 1
	}
	key := &matchpb.MatchDirectIntentRecord{
		Kind:        4,
		OwnerId:     launch.MatchID,
		RequestHigh: uint64(gameIdx),
		RequestLow:  launch.MatchID,
	}
	payload := encodeTournamentLaunchPayload(launch)
	tCtx := &battlepb.TournamentBattleContext{
		SeasonId:             launch.SeasonID,
		MatchId:              launch.MatchID,
		BestOf:               bestOf,
		GameIndex:            gameIdx,
		WinsA:                launch.WinsA,
		WinsB:                launch.WinsB,
		LeylineElement:       launch.LeylineElement,
		SquadAId:             launch.SquadAId,
		SquadAName:           launch.SquadAName,
		SquadBId:             launch.SquadBId,
		SquadBName:           launch.SquadBName,
		SquadAEmblemId:       launch.SquadAEmblemID,
		SquadBEmblemId:       launch.SquadBEmblemID,
		IsWarmup:             launch.IsWarmup,
		IsThirdPlace:         launch.IsThirdPlace,
		SkillBanSlotsPerTeam: svcCtx.Authority.TournamentCatalog().Rule().GetSkillBanSlotsPerTeam(),
	}
	formation, err := reserveGlobalDirect(ctx, svcCtx, key, payload, matchpb.MatchMode_MATCH_MODE_SEASON_TOURNAMENT, launch.BattleConfigID, members, directOptions{tournamentContext: tCtx})
	if err != nil {
		_ = svcCtx.Authority.BindTournamentMatchBattle(context.Background(), launch.MatchID, 0, false)
		return 0, err
	}
	if err := svcCtx.Authority.BindTournamentMatchBattle(ctx, launch.MatchID, formation.BattleId, launch.Featured); err != nil {
		logx.Errorf("[tournament] BindTournamentMatchBattle failed match=%d battle=%d: %v", launch.MatchID, formation.BattleId, err)
	}
	if formation.State < 4 {
		launchGlobalFormation(svcCtx, formation)
	}
	return formation.BattleId, nil
}

func encodeTournamentLaunchPayload(spec *authority.TournamentLaunchSpec) []byte {
	buf := make([]byte, 24+8*(len(spec.TeamA)+len(spec.TeamB)))
	binary.LittleEndian.PutUint64(buf[0:], spec.MatchID)
	binary.LittleEndian.PutUint32(buf[8:], spec.SeasonID)
	binary.LittleEndian.PutUint32(buf[12:], spec.BattleConfigID)
	binary.LittleEndian.PutUint32(buf[16:], spec.GameIndex)
	binary.LittleEndian.PutUint32(buf[20:], uint32(spec.LeylineElement))
	offset := 24
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
	return claimTournamentRewards(l.ctx, l.svcCtx.Authority, l.svcCtx.MailAdmin, l.svcCtx.MailSecret, playerID, in.GetSeasonId()), nil
}

// tournamentRewardStore 是领奖用到的两条权威读写,生产由 *authority.Store 实现;抽出来只为单测换内存替身。
type tournamentRewardStore interface {
	ClaimableTournamentRewards(ctx context.Context, playerID uint64, seasonID uint32) ([]*matchpb.MatchTournamentRewardRecord, error)
	MarkTournamentRewardClaimed(ctx context.Context, seasonID uint32, playerID uint64, tierKind uint32, mailID uint64) (*matchpb.TournamentPlayerRewardView, error)
}

// claimTournamentRewards 逐条领取玩家待领的锦标赛奖励(群仙问道赛设计 V1 第七节,数据覆写审计 r2 第 4 轮):
//   - 带附件的奖励只经邮件发:发件没装配就回内部错误、行保持可领取,不把附件吞成「已领取」;不带附件的荣誉奖励直接落已领取;
//   - 邮件请求只由奖励行决定(tournamentRewardMailRequest),重领逐字节同一请求,mail 判重回原 mail_id;
//   - 一条失败不挡其余:成功的照常落已领取,响应带回已领取的部分与第一条失败的提示。
func claimTournamentRewards(ctx context.Context, store tournamentRewardStore, mail mailpb.MailAdminClient, mailSecret string, playerID uint64, seasonID uint32) *matchpb.ClaimTournamentRewardResponse {
	pending, err := store.ClaimableTournamentRewards(ctx, playerID, seasonID)
	if err != nil {
		logx.WithContext(ctx).Errorf("[tournament] ClaimableTournamentRewards failed player=%d: %v", playerID, err)
		return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}
	}
	if len(pending) == 0 {
		return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(constants.ErrTournamentRewardUnavailable)}
	}
	claimed := make([]*matchpb.TournamentPlayerRewardView, 0, len(pending))
	failedTip := uint32(0)
	for _, r := range pending {
		view, tip := claimOneTournamentReward(ctx, store, mail, mailSecret, playerID, r)
		if tip != 0 {
			if failedTip == 0 {
				failedTip = tip
			}
			continue
		}
		claimed = append(claimed, view)
	}
	return &matchpb.ClaimTournamentRewardResponse{ErrorMessage: tournamentTip(failedTip), ClaimedRewards: claimed}
}

// claimOneTournamentReward 领一条奖励,失败回 tip(行保持可领取,玩家重领即补)。只有确认邮件已建(拿到 mail_id)才落已领取:
// 超时、断连、被拒、请求冲突一律不落,不把「结果不明」当「已发送」。
func claimOneTournamentReward(ctx context.Context, store tournamentRewardStore, mail mailpb.MailAdminClient, mailSecret string, playerID uint64, r *matchpb.MatchTournamentRewardRecord) (*matchpb.TournamentPlayerRewardView, uint32) {
	logger := logx.WithContext(ctx)
	var atts mailpb.MailAttachments
	if err := proto.Unmarshal(r.Attachments, &atts); err != nil {
		logger.Errorf("[tournament] Corrupt attachments for reward=%d: %v", r.RewardId, err)
		return nil, constants.ErrInternal
	}
	mailID := uint64(0)
	if len(atts.GetCurrencies())+len(atts.GetItems()) > 0 {
		if mail == nil || mailSecret == "" {
			logger.Errorf("[tournament] Mail delivery is not configured (MailRpc / MailSecretEnv); reward=%d stays claimable", r.RewardId)
			return nil, constants.ErrInternal
		}
		mailReq := tournamentRewardMailRequest(r, &atts)
		nowMs := uint64(time.Now().UnixMilli())
		if err := serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, mailReq, tournamentMailCaller, mailSecret, nowMs); err != nil {
			logger.Errorf("[tournament] Sign SendPersonalMail failed reward=%d: %v", r.RewardId, err)
			return nil, constants.ErrInternal
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		mailResp, callErr := mail.SendPersonalMail(callCtx, mailReq)
		cancel()
		// 收件箱满是玩家自己能处理的:原样回 mail 的满箱提示(tip 全仓单轴),行保持可领取,清出空位再领即补。
		if fullTip := uint32(table.MailError_kMailRecipientBoxFull); callErr == nil && mailResp.GetErrorMessage().GetId() == fullTip {
			logger.Infof("[tournament] Recipient mailbox is full for reward=%d; reward stays claimable", r.RewardId)
			return nil, fullTip
		}
		if callErr != nil || mailResp.GetErrorMessage().GetId() != 0 || mailResp.GetMailId() == 0 {
			logger.Errorf("[tournament] SendPersonalMail failed reward=%d tip=%d err=%v; reward stays claimable", r.RewardId, mailResp.GetErrorMessage().GetId(), callErr)
			return nil, constants.ErrInternal
		}
		mailID = mailResp.GetMailId()
	}
	view, err := store.MarkTournamentRewardClaimed(ctx, r.SeasonId, playerID, r.TierKind, mailID)
	if err != nil {
		logger.Errorf("[tournament] MarkTournamentRewardClaimed failed reward=%d mail=%d: %v", r.RewardId, mailID, err)
		return nil, constants.ErrInternal
	}
	return view, 0
}

// tournamentRewardMailRequest 只由奖励行构造领奖邮件请求:文案取结算时冻结在行里的值(不读当前配表),ExpireMs 不填
// (mail 首次落库时按 MaxPersonalMailTtlDays 算)。同一条奖励每次重领逐字节相同,mail 的 (sender_kind, request_id) 判重
// 先于一切依赖当前时间的校验,邮件已建而已领取没落定时重领回原 mail_id,不会撞成请求冲突。
func tournamentRewardMailRequest(r *matchpb.MatchTournamentRewardRecord, atts *mailpb.MailAttachments) *mailpb.SendMailRequest {
	return &mailpb.SendMailRequest{
		SenderKind:        tournamentMailSenderKind,
		RequestId:         r.RewardId,
		RecipientPlayerId: r.PlayerId,
		Title:             r.MailTitle,
		Body:              r.MailBody,
		Attachments:       atts,
	}
}

// SendTournamentSpectateCheer V2 观战底部预设喝彩(严格白名单 1..6 + 5 秒冷却 + 全 L0/L1/L2 观众通用)。
func (l *TournamentLogic) SendTournamentSpectateCheer(in *matchpb.SendTournamentSpectateCheerRequest) (*matchpb.SendTournamentSpectateCheerResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil {
		return &matchpb.SendTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	battleID := in.GetBattleId()
	if battleID == 0 || !tournament.IsValidSpectateCheerPhrase(in.GetPhraseId()) || !tournament.IsValidSpectateCheerSide(in.GetSide()) {
		return &matchpb.SendTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrTournamentSpectateCheerInvalid)}, nil
	}
	if l.svcCtx.Authority != nil && !l.svcCtx.Authority.IsTournamentBattle(l.ctx, battleID) {
		return &matchpb.SendTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrTournamentSpectateCheerInvalid)}, nil
	}
	nowMs := uint64(time.Now().UnixMilli())
	senderName := challengePublicName(l.ctx, l.svcCtx, playerID)
	snap, cooldownHit, invalidArg := l.svcCtx.ActiveTournamentCheers().Send(battleID, playerID, senderName, in.GetPhraseId(), in.GetSide(), nowMs)
	if invalidArg {
		return &matchpb.SendTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrTournamentSpectateCheerInvalid)}, nil
	}
	tipCode := uint32(0)
	if cooldownHit {
		tipCode = constants.ErrTournamentSpectateCheerCooldown
	}
	return &matchpb.SendTournamentSpectateCheerResponse{
		ErrorMessage:    tournamentTip(tipCode),
		BattleId:        snap.BattleID,
		CountA:          snap.CountA,
		CountB:          snap.CountB,
		CooldownUntilMs: snap.CooldownUntilMs,
		RecentCheers:    snap.RecentCheers,
		ServerTimeMs:    snap.ServerTimeMs,
	}, nil
}

// GetTournamentSpectateCheer V2 拉取指定锦标赛对局最新双边人气计数与增量预设喝彩。
func (l *TournamentLogic) GetTournamentSpectateCheer(in *matchpb.GetTournamentSpectateCheerRequest) (*matchpb.GetTournamentSpectateCheerResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if l.svcCtx == nil {
		return &matchpb.GetTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	battleID := in.GetBattleId()
	if battleID == 0 {
		return &matchpb.GetTournamentSpectateCheerResponse{ErrorMessage: tournamentTip(constants.ErrTournamentSpectateCheerInvalid)}, nil
	}
	nowMs := uint64(time.Now().UnixMilli())
	snap := l.svcCtx.ActiveTournamentCheers().Snapshot(battleID, playerID, in.GetAfterSeq(), nowMs)
	return &matchpb.GetTournamentSpectateCheerResponse{
		ErrorMessage:    tournamentTip(0),
		BattleId:        snap.BattleID,
		CountA:          snap.CountA,
		CountB:          snap.CountB,
		CooldownUntilMs: snap.CooldownUntilMs,
		RecentCheers:    snap.RecentCheers,
		ServerTimeMs:    snap.ServerTimeMs,
	}, nil
}

// ---- 《群仙问道赛》V3 增量 RPC：队徽更换 / 赛前零积分热身约战 / 神机问道免费竞猜 ----

func (l *TournamentLogic) UpdateTournamentSquadEmblem(in *matchpb.UpdateTournamentSquadEmblemRequest) (*matchpb.UpdateTournamentSquadEmblemResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.UpdateTournamentSquadEmblemResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	squad, err := l.svcCtx.Authority.UpdateTournamentSquadEmblem(l.ctx, playerID, in.GetEmblemId())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] UpdateTournamentSquadEmblem failed captain=%d emblem=%d: %v", playerID, in.GetEmblemId(), err)
		}
		return &matchpb.UpdateTournamentSquadEmblemResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.UpdateTournamentSquadEmblemResponse{ErrorMessage: tournamentTip(0), Squad: squad}, nil
}

func (l *TournamentLogic) StartTournamentWarmup(in *matchpb.StartTournamentWarmupRequest) (*matchpb.StartTournamentWarmupResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.StartTournamentWarmupResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	preMatchID, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		l.Errorf("[tournament] StartTournamentWarmup id generate failed player=%d: %v", playerID, err)
		return &matchpb.StartTournamentWarmupResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	matchView, launch, err := l.svcCtx.Authority.PrepareTournamentWarmup(l.ctx, playerID, in.GetTargetSquadId(), in.GetLeylineElement(), in.GetLineupPlayerIds(), preMatchID)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] StartTournamentWarmup prepare failed captain=%d: %v", playerID, err)
		}
		return &matchpb.StartTournamentWarmupResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	if launch == nil {
		return &matchpb.StartTournamentWarmupResponse{ErrorMessage: tournamentTip(0), Match: matchView}, nil
	}
	battleID, launchErr := launchTournamentSpec(l.ctx, l.svcCtx, launch)
	if launchErr != nil {
		l.Errorf("[tournament] StartTournamentWarmup launch failed match=%d: %v", launch.MatchID, launchErr)
		return &matchpb.StartTournamentWarmupResponse{ErrorMessage: tournamentTip(constants.ErrTournamentMatchNotReady), Match: matchView}, nil
	}
	matchView.BattleId = battleID
	matchView.State = matchpb.TournamentMatchState_TOURNAMENT_MATCH_STATE_IN_BATTLE
	return &matchpb.StartTournamentWarmupResponse{
		ErrorMessage: tournamentTip(0),
		Match:        matchView,
		BattleId:     battleID,
	}, nil
}

func (l *TournamentLogic) PredictTournamentMatch(in *matchpb.PredictTournamentMatchRequest) (*matchpb.PredictTournamentMatchResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.PredictTournamentMatchResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	playerName := challengePublicName(l.ctx, l.svcCtx, playerID)
	countA, countB, err := l.svcCtx.Authority.PredictTournamentMatch(l.ctx, playerID, playerName, in.GetMatchId(), in.GetPredictedSquadId())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] PredictTournamentMatch failed player=%d match=%d: %v", playerID, in.GetMatchId(), err)
		}
		return &matchpb.PredictTournamentMatchResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.PredictTournamentMatchResponse{
		ErrorMessage:     tournamentTip(0),
		MatchId:          in.GetMatchId(),
		PredictedSquadId: in.GetPredictedSquadId(),
		PredictCountA:    countA,
		PredictCountB:    countB,
	}, nil
}

func (l *TournamentLogic) GetTournamentPredictionBoard(in *matchpb.GetTournamentPredictionBoardRequest) (*matchpb.GetTournamentPredictionBoardResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTournamentPredictionBoardResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	resp, err := l.svcCtx.Authority.LoadTournamentPredictionBoard(l.ctx, in.GetSeasonId(), playerID)
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] GetTournamentPredictionBoard failed season=%d: %v", in.GetSeasonId(), err)
		}
		return &matchpb.GetTournamentPredictionBoardResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	resp.ErrorMessage = tournamentTip(0)
	return resp, nil
}

// ---- 《群仙问道赛》V4 增量 RPC：BO3 五行灵脉禁选 + 本季名局录 ----

func (l *TournamentLogic) BanTournamentLeyline(in *matchpb.BanTournamentLeylineRequest) (*matchpb.BanTournamentLeylineResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.BanTournamentLeylineResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	if in.GetMatchId() == 0 || !tournament.IsValidLeylineElement(in.GetLeylineElement()) {
		return &matchpb.BanTournamentLeylineResponse{ErrorMessage: tournamentTip(constants.ErrTournamentMatchNotReady)}, nil
	}
	matchView, err := l.svcCtx.Authority.BanTournamentLeyline(l.ctx, playerID, in.GetMatchId(), in.GetLeylineElement())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] BanTournamentLeyline failed captain=%d match=%d: %v", playerID, in.GetMatchId(), err)
		}
		return &matchpb.BanTournamentLeylineResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	return &matchpb.BanTournamentLeylineResponse{ErrorMessage: tournamentTip(0), Match: matchView}, nil
}

func (l *TournamentLogic) GetTournamentHighlights(in *matchpb.GetTournamentHighlightsRequest) (*matchpb.GetTournamentHighlightsResponse, error) {
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTournamentHighlightsResponse{ErrorMessage: tournamentTip(constants.ErrInternal)}, nil
	}
	resp, err := l.svcCtx.Authority.LoadTournamentHighlights(l.ctx, in.GetSeasonId())
	if err != nil {
		code := tournamentErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[tournament] GetTournamentHighlights failed season=%d: %v", in.GetSeasonId(), err)
		}
		return &matchpb.GetTournamentHighlightsResponse{ErrorMessage: tournamentTip(code)}, nil
	}
	resp.ErrorMessage = tournamentTip(0)
	return resp, nil
}

// StartTournamentLoop 启动全服赛季锦标赛阶段推进与 BO3 局间到期自动开局循环(按 OwnsWork("tournament:driver") 选主,SQL 提交点兜底幂等)。
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
				if err != nil && ctx.Err() == nil {
					logx.Errorf("[tournament] season advance tick failed: %v", err)
				}
				dueSpecs, dueErr := svcCtx.Authority.TakeDueIntermissionLaunches(tickCtx, 0)
				if dueErr != nil && ctx.Err() == nil {
					logx.Errorf("[tournament] due intermission scan failed: %v", dueErr)
				}
				for _, spec := range dueSpecs {
					if _, lErr := launchTournamentSpec(tickCtx, svcCtx, spec); lErr != nil && ctx.Err() == nil {
						logx.Errorf("[tournament] auto-launch BO3 game failed match=%d game=%d: %v", spec.MatchID, spec.GameIndex, lErr)
					}
				}
				cancel()
			}
		}
	})
}
