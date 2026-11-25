package logic

import (
	"context"
	"errors"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// RankedLogic 实现论剑排位首发 V1 的 4 条客户端 RPC(448-451)。
type RankedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRankedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RankedLogic {
	return &RankedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func rankedTip(id uint32) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id}
}

func rankedErrorTip(err error) uint32 {
	switch {
	case errors.Is(err, authority.ErrRankedSeasonSettling):
		return constants.ErrRankedSeasonSettling
	case errors.Is(err, authority.ErrRankedRuleVersionMismatch):
		return constants.ErrRankedRuleVersionMismatch
	case errors.Is(err, authority.ErrRankedPresetInvalid):
		return constants.ErrRankedPresetInvalid
	default:
		return constants.ErrInternal
	}
}

func (l *RankedLogic) GetRankedProfile(in *matchpb.GetRankedProfileRequest) (*matchpb.GetRankedProfileResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 {
		return &matchpb.GetRankedProfileResponse{ErrorMessage: rankedTip(constants.ErrInternal)}, nil
	}
	resp, err := l.svcCtx.Authority.LoadRankedProfile(l.ctx, playerID, in.GetIncludeHistory())
	if err != nil {
		l.Errorf("[ranked] GetRankedProfile failed player=%d: %v", playerID, err)
		return &matchpb.GetRankedProfileResponse{ErrorMessage: rankedTip(rankedErrorTip(err))}, nil
	}
	resp.ErrorMessage = rankedTip(0)
	return resp, nil
}

func (l *RankedLogic) SaveRankedPreset(in *matchpb.SaveRankedPresetRequest) (*matchpb.SaveRankedPresetResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 {
		return &matchpb.SaveRankedPresetResponse{ErrorMessage: rankedTip(constants.ErrInternal)}, nil
	}
	sel, curVer, err := l.svcCtx.Authority.SaveRankedPreset(l.ctx, playerID, in.GetCharacterPresetId(), in.GetPetPresetId(), in.GetRuleVersion())
	if err != nil {
		code := rankedErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[ranked] SaveRankedPreset failed player=%d: %v", playerID, err)
		}
		return &matchpb.SaveRankedPresetResponse{ErrorMessage: rankedTip(code), RuleVersion: curVer}, nil
	}
	return &matchpb.SaveRankedPresetResponse{
		ErrorMessage:    rankedTip(0),
		SelectedPresets: sel,
		RuleVersion:     curVer,
	}, nil
}

func (l *RankedLogic) UpdateRankedCosmetic(in *matchpb.UpdateRankedCosmeticRequest) (*matchpb.UpdateRankedCosmeticResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 {
		return &matchpb.UpdateRankedCosmeticResponse{ErrorMessage: rankedTip(constants.ErrInternal)}, nil
	}
	loadout, err := l.svcCtx.Authority.UpdateRankedCosmetic(l.ctx, playerID, in.GetLoadout())
	if err != nil {
		code := rankedErrorTip(err)
		if code == constants.ErrInternal {
			l.Errorf("[ranked] UpdateRankedCosmetic failed player=%d: %v", playerID, err)
		}
		return &matchpb.UpdateRankedCosmeticResponse{ErrorMessage: rankedTip(code)}, nil
	}
	return &matchpb.UpdateRankedCosmeticResponse{
		ErrorMessage: rankedTip(0),
		Loadout:      loadout,
	}, nil
}

func (l *RankedLogic) GetRankedBattleResult(in *matchpb.GetRankedBattleResultRequest) (*matchpb.GetRankedBattleResultResponse, error) {
	playerID := authoritativePlayerID(l.ctx, 0)
	if playerID == 0 {
		return &matchpb.GetRankedBattleResultResponse{ErrorMessage: rankedTip(constants.ErrInternal)}, nil
	}
	res, err := l.svcCtx.Authority.GetRankedBattleResult(l.ctx, playerID, in.GetBattleId())
	if err != nil {
		l.Errorf("[ranked] GetRankedBattleResult failed player=%d battle=%d: %v", playerID, in.GetBattleId(), err)
		return &matchpb.GetRankedBattleResultResponse{ErrorMessage: rankedTip(constants.ErrInternal)}, nil
	}
	return &matchpb.GetRankedBattleResultResponse{
		ErrorMessage: rankedTip(0),
		Result:       res,
	}, nil
}
