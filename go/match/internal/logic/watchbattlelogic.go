package logic

import (
	"context"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type WatchBattleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewWatchBattleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WatchBattleLogic {
	return &WatchBattleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// WatchBattle 观战入口(设计文档 §10.2,决策 D9/D11)。观战占用与原操作都在 SQL(match_spectator / match_spectator_intent),
// 流程见 global_spectator.go;档位与 L1 / L2 准入见 spectate_tier.go(battle-spectate-tiered-fanout.md §15.6)。
func (l *WatchBattleLogic) WatchBattle(in *matchpb.WatchBattleRequest) (*matchpb.WatchBattleResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		metrics.ObserveWatchBattle("internal")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	return globalWatchBattle(l, in, playerId)
}

// observerZoneId 从 player:{id}:location 取观众 zone(BattleRouting 的携带字段;
// 当前 battle 出站 topic 只用 gate_node_id,zone 缺失不致命,读不到按 0 记日志)。
func (l *WatchBattleLogic) observerZoneId(playerId uint64) uint32 {
	loc, err := loadPlayerLocation(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[spectate] Failed to read spectator location player=%d: %v", playerId, err)
		return 0
	}
	if loc == nil {
		return 0
	}
	return loc.ZoneId
}
