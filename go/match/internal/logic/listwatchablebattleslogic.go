package logic

import (
	"context"
	"strconv"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// 观战列表条数:0 = 用默认;上限收口防一次拉爆(索引里最多也就几百场)。
const (
	defaultWatchableListLimit = 20
	maxWatchableListLimit     = 50
)

type ListWatchableBattlesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListWatchableBattlesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWatchableBattlesLogic {
	return &ListWatchableBattlesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListWatchableBattles 可观战列表(设计文档 §10,决策 D9):ZSET 按 created_at_ms
// 逆序取前 limit 条,逐条读记录组摘要;过期/缺失/损坏的条目现场懒剔除且不回填
// (下一次请求自然补齐,列表短一点无妨)。只读接口,无互斥检查 —— 排队中的
// 玩家也可以先浏览列表,互斥在 WatchBattle 时强制。
func (l *ListWatchableBattlesLogic) ListWatchableBattles(in *matchpb.ListWatchableBattlesRequest) (*matchpb.ListWatchableBattlesResponse, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultWatchableListLimit
	}
	if limit > maxWatchableListLimit {
		limit = maxWatchableListLimit
	}

	pairs, err := l.svcCtx.MatchRedis.ZrevrangeWithScores(spectateBattlesActiveKey, 0, int64(limit)-1)
	if err != nil {
		l.Errorf("[spectate] 读观战索引失败: %v", err)
		return nil, err
	}

	staleBefore := spectateStaleBeforeMs(l.svcCtx)
	battles := make([]*matchpb.BattleWatchSummary, 0, len(pairs))
	for _, pair := range pairs {
		battleId, err := strconv.ParseUint(pair.Key, 10, 64)
		if err != nil {
			l.Errorf("[spectate] 观战索引出现非法成员 %q,剔除", pair.Key)
			if _, err := l.svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, pair.Key); err != nil {
				l.Errorf("[spectate] 剔除非法成员失败: %v", err)
			}
			continue
		}
		if uint64(pair.Score) < staleBefore {
			removeSpectateBattle(l.svcCtx, battleId)
			continue
		}
		record, err := loadSpectateBattle(l.svcCtx, battleId)
		if err != nil {
			// Redis 抖动/记录损坏:跳过本条,不剔除(下次可能恢复)。
			l.Errorf("[spectate] 读观战记录失败 battle=%d: %v", battleId, err)
			continue
		}
		if record == nil || record.GetSummary() == nil {
			removeSpectateBattle(l.svcCtx, battleId)
			continue
		}
		battles = append(battles, record.GetSummary())
	}
	return &matchpb.ListWatchableBattlesResponse{Battles: battles}, nil
}
