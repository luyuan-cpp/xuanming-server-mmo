package logic

import (
	"context"
	"errors"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// TrialChallengeCardLogic 负责五行试炼志 V3 好友约战帖与受控 PVE 录像播放的三条客户端协议。
// player_id 仅认 gate 注入的会话身份(ctxkeys)，缺失即 fail-closed。
type TrialChallengeCardLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTrialChallengeCardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TrialChallengeCardLogic {
	return &TrialChallengeCardLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *TrialChallengeCardLogic) CreateTrialChallengeCard(in *matchpb.CreateTrialChallengeCardRequest) (*matchpb.CreateTrialChallengeCardResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[trial_card] CreateTrialChallengeCard missing session identity battle=%d", in.GetBattleId())
		return &matchpb.CreateTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetBattleId() == 0 {
		return &matchpb.CreateTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "该试炼战报不可用于签发约战帖")}, nil
	}
	if l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.CreateTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "约战帖服务暂不可用")}, nil
	}
	card, err := l.svcCtx.Authority.CreateTrialChallengeCard(l.ctx, detail.GetPlayerId(), in.GetBattleId(), l.svcCtx.BattleIDGen.Generate)
	switch {
	case errors.Is(err, authority.ErrTrialChallengeBattleNotEligible):
		return &matchpb.CreateTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "仅本人近期有效通关的试炼志战报可签发约战帖")}, nil
	case err != nil:
		l.Errorf("[trial_card] CreateTrialChallengeCard failed player=%d battle=%d: %v", detail.GetPlayerId(), in.GetBattleId(), err)
		return &matchpb.CreateTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "签发约战帖失败，请稍后重试")}, nil
	}
	return &matchpb.CreateTrialChallengeCardResponse{Card: card}, nil
}

func (l *TrialChallengeCardLogic) GetTrialChallengeCard(in *matchpb.GetTrialChallengeCardRequest) (*matchpb.GetTrialChallengeCardResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[trial_card] GetTrialChallengeCard missing session identity card=%d", in.GetCardId())
		return &matchpb.GetTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetCardId() == 0 {
		return &matchpb.GetTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "约战帖不存在或已过期")}, nil
	}
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "约战帖暂时读不到")}, nil
	}
	card, err := l.svcCtx.Authority.GetTrialChallengeCard(l.ctx, in.GetCardId())
	switch {
	case errors.Is(err, authority.ErrTrialChallengeCardNotFound):
		return &matchpb.GetTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "约战帖不存在或已过期")}, nil
	case err != nil:
		l.Errorf("[trial_card] GetTrialChallengeCard failed player=%d card=%d: %v", detail.GetPlayerId(), in.GetCardId(), err)
		return &matchpb.GetTrialChallengeCardResponse{ErrorMessage: tipErr(constants.ErrInternal, "约战帖暂时读不到")}, nil
	}
	return &matchpb.GetTrialChallengeCardResponse{Card: card}, nil
}

func (l *TrialChallengeCardLogic) GetSharedTrialReplay(in *matchpb.GetSharedTrialReplayRequest) (*matchpb.GetSharedTrialReplayResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[trial_card] GetSharedTrialReplay missing session identity card=%d", in.GetCardId())
		return &matchpb.GetSharedTrialReplayResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetCardId() == 0 {
		return &matchpb.GetSharedTrialReplayResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "约战帖回放不存在或已过期")}, nil
	}
	if l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetSharedTrialReplayResponse{ErrorMessage: tipErr(constants.ErrInternal, "回放暂时读不到")}, nil
	}
	env, err := l.svcCtx.Authority.GetSharedTrialReplay(l.ctx, in.GetCardId())
	switch {
	case errors.Is(err, authority.ErrTrialChallengeReplayNotFound):
		return &matchpb.GetSharedTrialReplayResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "约战帖回放不存在或已过期")}, nil
	case err != nil:
		l.Errorf("[trial_card] GetSharedTrialReplay failed player=%d card=%d: %v", detail.GetPlayerId(), in.GetCardId(), err)
		return &matchpb.GetSharedTrialReplayResponse{ErrorMessage: tipErr(constants.ErrInternal, "回放暂时读不到")}, nil
	}
	return &matchpb.GetSharedTrialReplayResponse{Replay: env}, nil
}
