package logic

import (
	"context"
	"math"
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
// (下一次请求自然补齐,列表短一点无妨)。私密局(记录 visibility 非 PUBLIC,§10 第 6 条)
// 不出现在列表里:它本不该在活跃集合(addSpectateActive 拒收),读到即残留,只摘成员、
// 不删记录(丢票补签靠记录定位 battle 节点)。只读接口,无互斥检查 —— 排队中的
// 玩家也可以先浏览列表,互斥在 WatchBattle 时强制。
//
// 运营标记的赛事局(featured,§15.6)排在最前。它们从赛事局索引取,不从活跃集合的前 limit 条里挑:活跃集合按开局时间
// 倒序,比赛开打几分钟后就被新局挤出第一页,只在页内排序的话赛事局恰恰看不到。
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
		l.Errorf("[spectate] Failed to read spectator index: %v", err)
		return nil, err
	}

	staleBefore := spectateStaleBeforeMs(l.svcCtx)
	battles := make([]*matchpb.BattleWatchSummary, 0, len(pairs))
	listed := make(map[uint64]bool)
	for _, battleId := range l.featuredBattleIds(limit) {
		if summary := l.watchableSummary(battleId); summary != nil {
			summary.Featured = true
			battles = append(battles, summary)
			listed[battleId] = true
		}
	}
	for _, pair := range pairs {
		if uint32(len(battles)) >= limit {
			break
		}
		battleId, err := strconv.ParseUint(pair.Key, 10, 64)
		if err != nil {
			l.Errorf("[spectate] Invalid member %q in spectator index; removing", pair.Key)
			if _, err := l.svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, pair.Key); err != nil {
				l.Errorf("[spectate] Failed to remove invalid member: %v", err)
			}
			continue
		}
		if listed[battleId] {
			continue
		}
		if uint64(pair.Score) < staleBefore {
			removeSpectateBattle(l.svcCtx, battleId)
			continue
		}
		if summary := l.watchableSummary(battleId); summary != nil {
			battles = append(battles, summary)
		}
	}
	return &matchpb.ListWatchableBattlesResponse{Battles: battles}, nil
}

// featuredBattleIds 读赛事局索引里未到期的成员,后标记的在前。读失败只记日志:赛事局排序只是展示,不能因此让整个列表失败。
func (l *ListWatchableBattlesLogic) featuredBattleIds(limit uint32) []uint64 {
	pairs, err := l.svcCtx.MatchRedis.ZrevrangebyscoreWithScoresAndLimitCtx(l.ctx, spectateFeaturedActiveKey,
		int64(nowMs()), math.MaxInt64, 0, int(limit))
	if err != nil {
		l.Errorf("[spectate] Failed to read featured index: %v", err)
		return nil
	}
	ids := make([]uint64, 0, len(pairs))
	for _, pair := range pairs {
		battleId, err := strconv.ParseUint(pair.Key, 10, 64)
		if err != nil {
			l.Errorf("[spectate] Invalid member %q in featured index; removing", pair.Key)
			if _, err := l.svcCtx.MatchRedis.Zrem(spectateFeaturedActiveKey, pair.Key); err != nil {
				l.Errorf("[spectate] Failed to remove invalid featured member: %v", err)
			}
			continue
		}
		ids = append(ids, battleId)
	}
	return ids
}

// watchableSummary 读一场的摘要;不能列出时返回 nil,并按原因清理索引:记录缺失即残留,记录与成员一起删;
// 私密局只摘活跃集合成员、留记录;读失败(Redis 抖动 / 记录损坏)跳过本条,不剔除,下次可能恢复。
func (l *ListWatchableBattlesLogic) watchableSummary(battleId uint64) *matchpb.BattleWatchSummary {
	record, err := loadSpectateBattle(l.svcCtx, battleId)
	if err != nil {
		l.Errorf("[spectate] Failed to read spectator record battle=%d: %v", battleId, err)
		return nil
	}
	if record == nil || record.GetSummary() == nil {
		removeSpectateBattle(l.svcCtx, battleId)
		return nil
	}
	if !isSpectatePublic(record.GetVisibility()) {
		l.Infof("[spectate] Non-public battle found in spectator index; removing member only battle=%d visibility=%s",
			battleId, record.GetVisibility().String())
		removeSpectateActiveMember(l.svcCtx, battleId)
		return nil
	}
	return record.GetSummary()
}
