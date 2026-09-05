package logic

import (
	"context"
	"strconv"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
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

// WatchBattle 观战入口(设计文档 §10.2,决策 D9/D11):
//   - 互斥检查:持有 match ticket / battle:lock → 拒绝(观众绑定与参战绑定共用
//     SessionInfo 的 BattleNodeService 槽位,互斥杜绝绑定覆盖竞态);已在观战
//     则先清退旧场再接入新场(服务端换场语义,客户端无需先 StopWatchBattle);
//   - battle_id=0 随机挑一场;AddObserver 报房间不存在时懒剔除索引并换一场重试一次;
//   - 观众是否为该场参战者由 battle 侧校验(房间成员名单只有 battle 有权威),
//     这里不重复;
//   - 成功即返回,观战首帧(NotifySpectateState)由 battle 节点经 Kafka gate 推送,
//     客户端收到首帧才算进入观战。
func (l *WatchBattleLogic) WatchBattle(in *matchpb.WatchBattleRequest) (*matchpb.WatchBattleResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		metrics.ObserveWatchBattle("internal")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}

	internalErr := func(stage string, err error) (*matchpb.WatchBattleResponse, error) {
		l.Errorf("[spectate] WatchBattle %s失败 player=%d: %v", stage, playerId, err)
		metrics.ObserveWatchBattle("internal")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	// 互斥检查(D11):ticket / battle:lock 拒绝;已在观战走换场清退。
	ticket, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		return internalErr("读 ticket ", err)
	}
	if ticket != nil {
		metrics.ObserveWatchBattle("queued")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrSpectateWhileQueued, "匹配中无法观战"),
		}, nil
	}
	locked, err := isPlayerBattleLocked(l.svcCtx, playerId)
	if err != nil {
		return internalErr("查战斗锁", err)
	}
	if locked {
		metrics.ObserveWatchBattle("in_battle")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrSpectateWhileInBattle, "战斗尚未结束,无法观战"),
		}, nil
	}
	watchingRaw, err := l.svcCtx.MatchRedis.Get(spectateWatchingKey(playerId))
	if err != nil {
		return internalErr("读观战标记", err)
	}
	if watchingRaw != "" {
		// "已在观战"不拒绝,懒清退旧场后放行:StopWatchBattle 走 gate→battle 直达
		// 不经 match,战斗收尾 battle 也不回写 Redis,标记只能靠 TTL 自灭,一律
		// 拒绝会把玩家卡死整个 TTL 窗口。metrics 在此仅计数懒清退;该标签的
		// 拒绝出口只剩后方 SetnxEx 并发抢占失败一处。
		metrics.ObserveWatchBattle("already_watching")
		if prevBattleId, parseErr := strconv.ParseUint(watchingRaw, 10, 64); parseErr != nil {
			// 标记值非法:删掉残留后按新请求继续。
			deleteSpectateWatching(l.svcCtx, playerId)
		} else if in.BattleId != 0 && in.BattleId == prevBattleId {
			// 重看同一场:仅删标记,重推首帧/换会话重绑由 battle 侧 AddObserver
			// 幂等分支负责;勿发 removeObserver,免得给仍存活的旧会话推假 SpectateEnd。
			deleteSpectateWatching(l.svcCtx, playerId)
		} else {
			// 换场/随机模式:尽力对仍存活的旧场 removeObserver 并删标记
			// (战斗已收尾则 stopWatchingIfAny 内部只删标记)。
			stopWatchingIfAny(l.svcCtx, playerId, "rewatch")
		}
		// 三种情况都不 return:后续成功路径的 SetnxEx 会为新场原子抢占标记。
	}

	// 观众必须在线:观众路由(session/gate)从 player:session 组装,首帧经
	// gate 推送,不在线没有可路由的会话。
	session, err := loadPlayerSession(l.svcCtx, playerId)
	if err != nil {
		return internalErr("读会话", err)
	}
	if !isSessionOnline(session) {
		metrics.ObserveWatchBattle("offline")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrSpectateOffline, "会话不在线,无法观战"),
		}, nil
	}
	// gate_id 是数字节点 id 的字符串形态(battle 出站 topic = gate-{gate_node_id})。
	gateNodeId, err := strconv.ParseUint(session.GateId, 10, 32)
	if err != nil {
		return internalErr("解析 gate 节点 id ", err)
	}
	// scene 字段留 0 即不变量 6 的实现:观众永远收不到结算事件。
	routing := &battlepb.BattleRouting{
		SessionId:      session.SessionId,
		GateNodeId:     uint32(gateNodeId),
		GateInstanceId: session.GateInstanceId,
		ZoneId:         l.observerZoneId(playerId),
	}

	// 选场 + 挂观众。随机模式(battle_id=0)在房间不存在时懒剔除后换一场,
	// 最多重试一次(设计文档 §10.2 数据流末行)。
	randomMode := in.BattleId == 0
	maxAttempts := 1
	if randomMode {
		maxAttempts = 2
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		battleId := in.BattleId
		if randomMode {
			battleId, err = pickRandomBattle(l.svcCtx)
			if err != nil {
				return internalErr("随机选场", err)
			}
			if battleId == 0 {
				break // 无可观战战斗
			}
		}

		record, err := loadSpectateBattle(l.svcCtx, battleId)
		if err != nil {
			return internalErr("读观战记录", err)
		}
		if record == nil {
			// 记录 TTL 已清但 ZSET 可能残留:懒剔除。
			removeSpectateBattle(l.svcCtx, battleId)
			if randomMode {
				continue
			}
			metrics.ObserveWatchBattle("not_found")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗不存在或已结束"),
			}, nil
		}

		// 先原子抢占互斥标记(SETNX+EX)再 AddObserver:标记必须先于绑定生效,
		// 否则并发的 JoinQueue 会漏掉清退;入口已清掉本玩家旧标记,抢占失败只可能
		// 是并发 WatchBattle,由 SetnxEx 一锤定音拒绝。AddObserver 失败回滚 DEL。
		if beforeAcquireWatchingHook != nil {
			beforeAcquireWatchingHook(playerId)
		}
		acquired, err := l.svcCtx.MatchRedis.SetnxEx(spectateWatchingKey(playerId),
			strconv.FormatUint(battleId, 10), spectateTTLSeconds(l.svcCtx))
		if err != nil {
			return internalErr("写观战标记", err)
		}
		if !acquired {
			metrics.ObserveWatchBattle("already_watching")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrAlreadyWatching, "已在观战另一场战斗"),
			}, nil
		}
		roomMissing, err := addObserver(l.svcCtx, record.GetBattleNodeId(), battleId,
			playerId, session.Account, routing)
		if err == nil {
			// double-check 收尾:关『检查→写标记』TOCTOU 的另一半窗口——上方
			// 『标记先于绑定』只保证标记落地之后的 JoinQueue 能看到并清退;若并发
			// gather(尤其 PVE_SOLO 即时凑单)在入口 ticket 检查之后、标记落地之前
			// 已完成清退,观众绑定仍可能晚于参战绑定落地。复查命中即自我清退;
			// gate 侧 Unbind 按 battle_id 匹配,补偿解绑不会误伤参战绑定。
			recheckTicket, terr := loadTicket(l.svcCtx, playerId)
			if terr != nil {
				// 复查读 Redis 失败不阻断成功返回:双检是尽力收窄,不引入新失败面。
				l.Errorf("[spectate] double-check 读 ticket 失败 player=%d: %v", playerId, terr)
			}
			recheckLocked, lerr := isPlayerBattleLocked(l.svcCtx, playerId)
			if lerr != nil {
				l.Errorf("[spectate] double-check 查战斗锁失败 player=%d: %v", playerId, lerr)
			}
			if recheckTicket != nil || recheckLocked {
				removeObserver(l.svcCtx, record.GetBattleNodeId(), battleId, playerId, "concurrent_queue")
				deleteSpectateWatching(l.svcCtx, playerId)
				l.Infof("[spectate] 观战与并发排队冲突,自我清退 player=%d battle=%d ticket=%t locked=%t",
					playerId, battleId, recheckTicket != nil, recheckLocked)
				metrics.ObserveWatchBattle("queued")
				return &matchpb.WatchBattleResponse{
					ErrorMessage: tipErr(constants.ErrSpectateWhileQueued, "匹配中无法观战"),
				}, nil
			}
			l.Infof("[spectate] 观战接入成功 player=%d battle=%d node=%d random=%t",
				playerId, battleId, record.GetBattleNodeId(), randomMode)
			metrics.ObserveWatchBattle("ok")
			return &matchpb.WatchBattleResponse{BattleId: battleId}, nil
		}
		deleteSpectateWatching(l.svcCtx, playerId)
		if roomMissing {
			l.Infof("[spectate] 观战目标已收尾,懒剔除 player=%d battle=%d: %v", playerId, battleId, err)
			removeSpectateBattle(l.svcCtx, battleId)
			if randomMode {
				continue
			}
			metrics.ObserveWatchBattle("not_found")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗不存在或已结束"),
			}, nil
		}
		l.Errorf("[spectate] AddObserver 失败 player=%d battle=%d node=%d: %v",
			playerId, battleId, record.GetBattleNodeId(), err)
		metrics.ObserveWatchBattle("rejected")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗当前无法观战"),
		}, nil
	}

	metrics.ObserveWatchBattle("no_battle")
	return &matchpb.WatchBattleResponse{
		ErrorMessage: tipErr(constants.ErrNoWatchableBattle, "当前没有可观战的战斗"),
	}, nil
}

// observerZoneId 从 player:{id}:location 取观众 zone(BattleRouting 的携带字段;
// 当前 battle 出站 topic 只用 gate_node_id,zone 缺失不致命,读不到按 0 记日志)。
func (l *WatchBattleLogic) observerZoneId(playerId uint64) uint32 {
	loc, err := loadPlayerLocation(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[spectate] 读观众位置失败 player=%d: %v", playerId, err)
		return 0
	}
	if loc == nil {
		return 0
	}
	return loc.ZoneId
}
