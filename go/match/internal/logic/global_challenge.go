package logic

import (
	"context"
	"errors"
	"time"

	"match/generated/pb/game"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"
	dspb "proto/data_service"
	pb "proto/match"
)

func (l *ChallengeLogic) createGlobalChallenge(in *pb.ChallengePlayerRequest, challenger uint64) (*pb.ChallengePlayerResponse, error) {
	for _, player := range []uint64{challenger, in.TargetPlayerId} {
		locked, err := isPlayerBattleLocked(l.svcCtx, player)
		if err != nil {
			return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrInternal, "战斗状态暂不可用")}, nil
		}
		if locked {
			code := constants.ErrChallengeSelfBusy
			if player != challenger {
				code = constants.ErrChallengeTargetBusy
			}
			return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(code, "成员仍在其它战斗中")}, nil
		}
	}
	target, err := loadPlayerSession(l.svcCtx, in.TargetPlayerId)
	if err != nil {
		return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrInternal, "在线状态暂不可用")}, nil
	}
	if !isSessionOnline(target) {
		return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrChallengeTargetOffline, "对方不在线")}, nil
	}
	id, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙，请稍后再试")}, nil
	}
	invitation, err := l.svcCtx.Authority.CreateChallenge(l.ctx, id, challenger, in.TargetPlayerId, in.BattleConfigId, uint64(l.challengeTTL())*1000)
	if err != nil {
		if errors.Is(err, authority.ErrChallengePending) {
			return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrChallengePending, "对方已有待处理的切磋邀请")}, nil
		}
		if errors.Is(err, authority.ErrOriginalPlayer) {
			return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrChallengeExpired, "原角色归属已变化，请重新发起")}, nil
		}
		l.Error("全球切磋邀请原件提交未确认")
		return &pb.ChallengePlayerResponse{ErrorMessage: tipErr(constants.ErrInternal, "邀请结果尚未确认，请重试原请求")}, nil
	}
	challengerName := challengePublicName(l.ctx, l.svcCtx, challenger)
	notification := globalChallengeInvitationNotice(invitation, challengerName)
	if err = pushToPlayer(l.svcCtx, invitation.TargetPlayerId, uint32(game.MatchServiceNotifyChallengeInviteMessageId), notification); err != nil {
		// 通知失败不撤销已提交原件；同正文重试会重推原编号且不刷新有效期。
		metrics.ObserveChallenge("invite", "push_failed")
		return &pb.ChallengePlayerResponse{ChallengeId: invitation.ChallengeId, ErrorMessage: tipErr(constants.ErrInternal, "邀请通知尚未确认，请重试原请求")}, nil
	}
	metrics.ObserveChallenge("invite", "ok")
	return &pb.ChallengePlayerResponse{ChallengeId: invitation.ChallengeId}, nil
}

func globalChallengeInvitationNotice(invitation *pb.MatchChallengeInvitationRecord, publicName string) *pb.ChallengeInviteS2C {
	return &pb.ChallengeInviteS2C{ChallengeId: invitation.ChallengeId, ChallengerId: invitation.ChallengerPlayerId, ChallengerName: publicName, BattleConfigId: invitation.ConfigId, ExpiresAtMs: invitation.ExpiresMs}
}

// 展示只取既有角色名注册表；在线session.account同样是私有登录账号，不能充当昵称。
func challengePublicName(ctx context.Context, s *svc.ServiceContext, player uint64) string {
	if s.DataServiceClient == nil {
		return ""
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	response, err := s.DataServiceClient.BatchGetPlayerName(lookup, &dspb.BatchGetPlayerNameRequest{PlayerIds: []uint64{player}})
	if err != nil {
		return ""
	}
	return response.GetNames()[player]
}

func (l *ChallengeLogic) globalChallengeFailure(err error) (*pb.RespondChallengeResponse, error) {
	switch {
	case errors.Is(err, authority.ErrChallengeNotTarget):
		return &pb.RespondChallengeResponse{ErrorMessage: tipErr(constants.ErrChallengeNotTarget, "该邀请不是发给你的")}, nil
	case errors.Is(err, authority.ErrChallengeExpired), errors.Is(err, authority.ErrOriginalPlayer), errors.Is(err, authority.ErrDirectConflict), errors.Is(err, authority.ErrDirectCancelled):
		return &pb.RespondChallengeResponse{ErrorMessage: tipErr(constants.ErrChallengeExpired, "原切磋邀请已失效或已经作出决定")}, nil
	default:
		l.Error("全球切磋决定原件提交未确认")
		return &pb.RespondChallengeResponse{ErrorMessage: tipErr(constants.ErrInternal, "切磋结果尚未确认，请重试原邀请")}, nil
	}
}

func (l *ChallengeLogic) respondGlobalChallenge(in *pb.RespondChallengeRequest, responder uint64) (*pb.RespondChallengeResponse, error) {
	invitation, err := l.svcCtx.Authority.ChallengeForActor(l.ctx, in.ChallengeId, responder)
	if err != nil {
		return l.globalChallengeFailure(err)
	}
	var formation *pb.MatchFormationRecord
	var group []*pb.MatchAuthorityTicketRecord
	if invitation.State == 1 && in.Accept {
		for _, player := range []uint64{invitation.ChallengerPlayerId, invitation.TargetPlayerId} {
			locked, e := isPlayerBattleLocked(l.svcCtx, player)
			if e != nil {
				return l.globalChallengeFailure(e)
			}
			if locked {
				return &pb.RespondChallengeResponse{ErrorMessage: tipErr(constants.ErrChallengeSelfBusy, "成员仍在其它战斗中")}, nil
			}
		}
		formation, group, err = buildGlobalDirect(l.ctx, l.svcCtx, pb.MatchMode_MATCH_MODE_PVP_CHALLENGE, invitation.ConfigId, []uint64{invitation.ChallengerPlayerId, invitation.TargetPlayerId}, nil, directOptions{})
		if err != nil {
			return l.globalChallengeFailure(err)
		}
	}
	formation, changed, err := l.svcCtx.Authority.DecideChallenge(l.ctx, invitation, in.Accept, formation, group)
	if err != nil {
		return l.globalChallengeFailure(err)
	}
	if changed {
		l.pushResult(invitation.ChallengerPlayerId, invitation.ChallengeId, in.Accept, responder)
		if in.Accept {
			l.pushResult(responder, invitation.ChallengeId, true, responder)
		}
	}
	if in.Accept && formation == nil {
		original, err := l.svcCtx.Authority.DirectAdmission(l.ctx, &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: responder, RequestLow: invitation.ChallengeId})
		if err != nil {
			return l.globalChallengeFailure(err)
		}
		if original == nil || !original.ExitPrepared {
			return l.globalChallengeFailure(authority.ErrDirectConflict)
		}
		formation, err = resumeGlobalDirectAdmission(l.ctx, l.svcCtx, original)
		if err != nil {
			return l.globalChallengeFailure(err)
		}
	}
	if formation != nil && formation.State < 4 {
		launchGlobalFormation(l.svcCtx, formation)
	}
	return &pb.RespondChallengeResponse{}, nil
}
