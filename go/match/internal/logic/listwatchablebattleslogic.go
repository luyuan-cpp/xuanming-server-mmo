package logic

import (
	"context"
	"strconv"

	"match/internal/svc"

	matchpb "proto/match"
	"shared/releasetrack"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 观战列表条数:0 = 用默认;上限收口防一次拉爆(索引里最多也就几百场)。
const (
	defaultWatchableListLimit = 20
	maxWatchableListLimit     = 50
)

// 列表读回源时各取多少条:一个请求最多看 2×limit 条活跃候选(stable 观众在 canary 镜像非空时多读一倍)与 limit 条赛事局。
// 缓存按上限取一份,各请求截自己要的前缀:全服前 k 一定是全服前 n 的前缀(k ≤ n)。
const (
	spectateLobbyActiveMax   = 2 * maxWatchableListLimit
	spectateLobbyFeaturedMax = maxWatchableListLimit
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
// 逆序取前 limit 条,逐条由 SQL 成局组摘要;过期/缺失/损坏的条目现场懒剔除且不回填
// (下一次请求自然补齐,列表短一点无妨)。私密局(记录 visibility 非 PUBLIC,§10 第 6 条)
// 不出现在列表里:它本不该在活跃集合(addSpectateActive 拒收),读到即残留,只摘成员。
// 只读接口,无互斥检查 —— 排队中的玩家也可以先浏览列表,互斥在 WatchBattle 时强制。
//
// 运营标记的赛事局(featured,§15.6)排在最前。它们从赛事局索引取,不从活跃集合的前 limit 条里挑:活跃集合按开局时间
// 倒序,比赛开打几分钟后就被新局挤出第一页,只在页内排序的话赛事局恰恰看不到。
//
// 两把索引都拆成 16 个桶,读的是本副本 1 秒缓存里的合并结果(readSpectateLobby),列表最多旧 1 秒。
func (l *ListWatchableBattlesLogic) ListWatchableBattles(in *matchpb.ListWatchableBattlesRequest) (*matchpb.ListWatchableBattlesResponse, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultWatchableListLimit
	}
	if limit > maxWatchableListLimit {
		limit = maxWatchableListLimit
	}

	// 发布轨道(设计 §2.6):stable 观众看不到 canary 对局(赛事局也一样),按成局原件记的轨道过滤,不摘索引(canary 观众照样能看)。
	// canary 镜像里有节点时多读一倍候选,被滤掉的场不至于让这一页变短;镜像为空时不多读,残留的 canary 场照样滤掉,
	// 只是这一页可能短一点(与懒剔除不回填同口径)。轨道读不出是内部链路故障。
	viewerTrack, err := sessionReleaseTrack(l.ctx)
	if err != nil {
		l.Errorf("[spectate] Failed to read the viewer release track: %v", err)
		return nil, err
	}
	scan := limit
	if !viewerTrack.IsCanary() && canaryBattlesMayExist(l.svcCtx) {
		scan = 2 * limit
	}

	lobby, err := readSpectateLobby(l.svcCtx)
	if err != nil {
		l.Errorf("[spectate] Failed to read spectator index: %v", err)
		return nil, err
	}
	pairs := lobby.Active[:min(int(scan), len(lobby.Active))]

	staleBefore := spectateStaleBeforeMs()
	battles := make([]*matchpb.BattleWatchSummary, 0, len(pairs))
	listed := make(map[uint64]bool)
	// 每条都要读一次权威库:请求已取消或超时就停,剩下的读只会白占连接池(撮合与观战原件事务共用它)。
	for _, battleId := range l.featuredBattleIds(lobby.Featured[:min(int(limit), len(lobby.Featured))]) {
		if err := l.ctx.Err(); err != nil {
			return nil, err
		}
		if summary := l.watchableSummary(battleId, viewerTrack); summary != nil {
			summary.Featured = true
			battles = append(battles, summary)
			listed[battleId] = true
		}
	}
	for _, pair := range pairs {
		if uint32(len(battles)) >= limit {
			break
		}
		if err := l.ctx.Err(); err != nil {
			return nil, err
		}
		battleId, err := strconv.ParseUint(pair.Key, 10, 64)
		if err != nil {
			l.Errorf("[spectate] Invalid member %q in spectator index; removing", pair.Key)
			removeSpectateIndexGarbage(l.svcCtx, spectateActiveIndex, pair.Key)
			continue
		}
		if listed[battleId] {
			continue
		}
		if uint64(pair.Score) < staleBefore {
			removeSpectateBattle(l.svcCtx, battleId)
			continue
		}
		if summary := l.watchableSummary(battleId, viewerTrack); summary != nil {
			battles = append(battles, summary)
		}
	}
	return &matchpb.ListWatchableBattlesResponse{Battles: battles}, nil
}

// featuredBattleIds 解析赛事局索引里未到期的成员(后标记的在前),坏成员现场摘掉。
func (l *ListWatchableBattlesLogic) featuredBattleIds(pairs []redis.Pair) []uint64 {
	ids := make([]uint64, 0, len(pairs))
	for _, pair := range pairs {
		battleId, err := strconv.ParseUint(pair.Key, 10, 64)
		if err != nil {
			l.Errorf("[spectate] Invalid member %q in featured index; removing", pair.Key)
			removeSpectateIndexGarbage(l.svcCtx, spectateFeaturedIndex, pair.Key)
			continue
		}
		ids = append(ids, battleId)
	}
	return ids
}

// readSpectateLobby 是观战列表的读:经本副本的 1 秒缓存(svc.SpectateLobbyCache),过期时同一时刻只有一个请求回源。
// 没装配缓存(手工构造的单测 ServiceContext)时每次回源。
func readSpectateLobby(s *svc.ServiceContext) (svc.SpectateLobbySnapshot, error) {
	return s.SpectateLobby.Load(func() (svc.SpectateLobbySnapshot, error) { return loadSpectateLobby(s) })
}

// loadSpectateLobby 回源:活跃集合开局最晚的前 spectateLobbyActiveMax 条与赛事局索引未到期的前 spectateLobbyFeaturedMax 条,
// 各是 16 个桶在一个 pipeline 里读完再合并。活跃集合读失败整个列表失败(不能把故障伪装成「没有可观战的战斗」);
// 赛事局读失败只记日志,赛事局排序只是展示。回源由同一时刻的多个请求共用,不挂在其中某一个请求的 ctx 上。
func loadSpectateLobby(s *svc.ServiceContext) (svc.SpectateLobbySnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	active, err := spectateActiveIndex.top(ctx, s.MatchRedis, spectateLobbyActiveMax)
	if err != nil {
		return svc.SpectateLobbySnapshot{}, err
	}
	featured, err := spectateFeaturedIndex.topFrom(ctx, s.MatchRedis, int64(nowMs()), spectateLobbyFeaturedMax)
	if err != nil {
		logx.Errorf("[spectate] Failed to read featured index: %v", err)
	}
	return svc.SpectateLobbySnapshot{Featured: featured, Active: active}, nil
}

// watchableSummary 读一场的摘要;不能列出时返回 nil,并按原因清理索引:成局不存在或终局已落定即残留,这场的观战数据一起删;
// 私密局只摘活跃集合成员;读失败(SQL 抖动 / 原件损坏)跳过本条,不剔除,下次可能恢复。
// 这个观众按轨道看不了的场(viewerMayWatch)也返回 nil,但不动索引:换一个观众它照样能列出。
func (l *ListWatchableBattlesLogic) watchableSummary(battleId uint64, viewer releasetrack.Track) *matchpb.BattleWatchSummary {
	record, err := loadSpectateBattleFn(l.ctx, l.svcCtx, battleId)
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
	if !viewerMayWatch(viewer, record.GetReleaseTrack()) {
		return nil
	}
	return record.GetSummary()
}
