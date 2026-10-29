package logic

import (
	"context"
	"errors"
	"strconv"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
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
//   - 互斥检查:持有 match ticket / battle:lock → 拒绝(一名玩家同一时刻只保留一条
//     battle 直连 —— 客户端单条链路,battle 房间 directConnByPlayer 按 player_id 单槽,
//     观战与参战不能并存);已在观战则先按标记里的原件清退旧观战再接入(服务端换场语义,
//     客户端无需先 StopWatchBattle;重看同一场也一样,见下方清退分支);
//   - 观战账本(与全局 BeginSpectator 同口径):每选中一场生成一份 AddObserver 原件(规范 UUID op、
//     准入截止、battle 实例,newLocalObserverAdd),随观战标记先落 Redis 再发 Add;补偿与清退的
//     RemoveObserver 一律带同一个 op;
//   - battle_id=0 随机挑一场;AddObserver 报房间不存在时懒剔除索引并换一场重试一次;
//     指定场先查活跃集合、再读记录:读记录前未公开且仍在建房窗口内(gather 先写记录后建房,
//     D82 还会改写记录),只回"不存在"、不剔除(roomMayBeCreating);记录已不在时只摘活跃集合
//     残留成员、不 DEL;
//   - 观众是否为该场参战者由 battle 侧校验(房间成员名单只有 battle 有权威),
//     这里不重复;
//   - AddObserver 的其余拒绝(观众席位满回「观战人数已满」ErrSpectateFull;墓碑表满 / 参战者 /
//     签不出票 kServiceUnavailable(turn-based §22 D70)回「该战斗当前无法观战」,映射见
//     spectateRejectVerdict)一律回滚观战标记、不动索引、不换场;签票失败时 battle 侧
//     不会留下该观众(新观众不登记,幂等重推路径摘出名单并关其直连),match 不必再发
//     RemoveObserver;
//   - 成功即返回:落点分配(NotifyBattleAssigned)由 battle 推给客户端(尚无直连时经
//     Kafka→gate,turn-based §22 D68),
//     客户端凭票直连 battle,观战首帧(NotifySpectateState)随直连握手下发
//     (turn-based §22 D69),收到首帧才算进入观战。
func (l *WatchBattleLogic) WatchBattle(in *matchpb.WatchBattleRequest) (*matchpb.WatchBattleResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if playerId == 0 {
		metrics.ObserveWatchBattle("internal")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	if l.svcCtx.Authority != nil {
		return globalWatchBattle(l, in, playerId)
	}

	internalErr := func(stage string, err error) (*matchpb.WatchBattleResponse, error) {
		l.Errorf("[spectate] WatchBattle %s failed player=%d: %v", stage, playerId, err)
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
		// 换场清退 / 删旧标记是副作用,截止之后不再发出(client-request-ordering.md §6.1):前面几次无 ctx 的读在
		// Redis 慢时可以卡过 router 的 ForwardTimeoutMs,gate lane 已放行下一条 WatchBattle;它刚建立的观战标记
		// 会被这条已超时的请求读到并拆掉。
		if err := l.ctx.Err(); err != nil {
			return internalErr("请求已截止,不再清退旧观战", err)
		}
		// "已在观战"不拒绝,懒清退旧观战后放行:StopWatchBattle 经 battle 直连直达
		// 不经 match,战斗收尾 battle 也不回写 Redis,标记只能靠 TTL 自灭,一律
		// 拒绝会把玩家卡死整个 TTL 窗口。metrics 在此仅计数懒清退;该标签的
		// 拒绝出口只剩后方 SetnxEx 并发抢占失败一处。
		metrics.ObserveWatchBattle("already_watching")
		// 换场、随机模式与重看同一场一律按旧原件 RemoveObserver 后删标记(标记非法 / 战斗已收尾时只删标记)。
		// 重看同一场也要先摘:battle 名单里挂着旧 op 时,新 op 的 Add 会被观战账本拒绝(只认原 op 的逐字节
		// 重放),而旧原件的准入截止多半已过、无法原样重放。代价是旧会话会先收到一次 SpectateEnd(rewatch),
		// 随后凭新分配重连;全局路径(prepareGlobalWatch 先 cancelGlobalSpectator)同口径。
		stopWatching(l.svcCtx, playerId, watchingRaw, "rewatch")
		// 不 return:后续成功路径的 SetnxEx 会为新观战原子抢占标记。
	}

	// 观众必须在线:观众路由(session/gate)从 player:session 组装,观众尚无直连时
	// 落点分配(NotifyBattleAssigned)经 gate 推送(turn-based §22 D68 大厅公告),
	// 不在线没有可路由的会话。
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
		// published / checkedAtMs 是建房窗口判定(roomMayBeCreating)的输入,必须取在读记录**之前**:
		// 记录可能在读之后被 gather 改写(D82 换节点)。随机模式挑的就是活跃集合成员,天然已公开。
		published := true
		var checkedAtMs uint64
		if randomMode {
			battleId, err = pickRandomBattle(l.svcCtx)
			if err != nil {
				return internalErr("随机选场", err)
			}
			if battleId == 0 {
				break // 无可观战战斗
			}
		} else {
			checkedAtMs = nowMs()
			published = l.spectateBattlePublished(battleId)
		}

		record, err := loadSpectateBattle(l.svcCtx, battleId)
		if err != nil {
			return internalErr("读观战记录", err)
		}
		if record == nil {
			// 记录已不在(TTL 到期 / 已被剔除 / gather 还没预写)。只摘活跃集合里的残留成员,不 DEL:
			// GET 已读到空,此刻 DEL 只可能删掉 GET 之后才预写的记录(gather 第 4.1 步),无效或有害。
			// 读记录前不在活跃集合的场什么都不写,免得误摘随后才公开(ZADD)的成员。
			if published {
				l.removeActiveMember(battleId)
			}
			if randomMode {
				continue
			}
			metrics.ObserveWatchBattle("not_found")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗不存在或已结束"),
			}, nil
		}

		// 定位 battle 实例并固定本次观战的原件(op / 准入截止 / 实例)。要在写标记之前做完:标记就是原件,
		// 并发的 gather 清退读到标记时必须能拿到原 op 去 RemoveObserver。节点定位失败(未注册 / 歧义 /
		// 没有实例 uuid)不是 battle 的业务拒绝,按「其它拒绝」回:不动索引、不换场,标记也还没写。
		endpoint, instance, err := spectateTarget(l.ctx, l.svcCtx, record.GetBattleNodeId(), battleId)
		if err != nil {
			l.Errorf("[spectate] Failed to locate battle node player=%d battle=%d node=%d: %v",
				playerId, battleId, record.GetBattleNodeId(), err)
			metrics.ObserveWatchBattle("rejected")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗当前无法观战"),
			}, nil
		}
		original := newLocalObserverAdd(battleId, playerId, session.Account, routing, instance)
		mark, err := encodeWatchingMark(original)
		if err != nil {
			return internalErr("组观战原件", err)
		}

		// 先原子抢占互斥标记(SETNX+EX)再 AddObserver:标记必须先于观众登记生效,
		// 否则并发的 JoinQueue 会漏掉清退;入口已清掉本玩家旧标记,抢占失败只可能
		// 是并发 WatchBattle,由 SetnxEx 一锤定音拒绝。AddObserver 失败回滚 DEL。
		if beforeAcquireWatchingHook != nil {
			beforeAcquireWatchingHook(playerId)
		}
		// 抢占标记与挂观众是 WatchBattle 的提交点,都受请求 ctx 约束:截止之后不再发出,否则会在客户端
		// 收到超时、gate 放行同一会话的下一条请求之后才把观战绑上(docs/design/client-request-ordering.md §6.1)。
		// 失败回滚(deleteSpectateWatching / removeObserver)是补偿,仍走独立 ctx。
		acquired, err := l.svcCtx.MatchRedis.SetnxExCtx(l.ctx, spectateWatchingKey(playerId),
			mark, spectateTTLSeconds(l.svcCtx))
		if err != nil {
			return internalErr("写观战标记", err)
		}
		if !acquired {
			metrics.ObserveWatchBattle("already_watching")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrAlreadyWatching, "已在观战另一场战斗"),
			}, nil
		}
		rejectTip, err := addObserver(l.ctx, endpoint, original)
		if err == nil {
			// double-check 收尾:关『检查→写标记』TOCTOU 的另一半窗口——上方
			// 『标记先于登记』只保证标记落地之后的 JoinQueue 能看到并清退;若并发
			// gather(尤其 PVE_SOLO 即时凑单)在入口 ticket 检查之后、标记落地之前
			// 已完成清退,观众登记仍可能晚于参战开局落地。复查命中即自我清退;
			// RemoveObserver 按 battle_id 定位房间,只摘该场观众名单并关该场直连,
			// 不会误伤玩家在另一场的参战直连。
			recheckTicket, terr := loadTicket(l.svcCtx, playerId)
			if terr != nil {
				// 复查读 Redis 失败不阻断成功返回:双检是尽力收窄,不引入新失败面。
				l.Errorf("[spectate] Double-check failed to read ticket player=%d: %v", playerId, terr)
			}
			recheckLocked, lerr := isPlayerBattleLocked(l.svcCtx, playerId)
			if lerr != nil {
				l.Errorf("[spectate] Double-check failed to check battle lock player=%d: %v", playerId, lerr)
			}
			if recheckTicket != nil || recheckLocked {
				removeObserver(l.svcCtx, record.GetBattleNodeId(), original, "concurrent_queue")
				deleteSpectateWatching(l.svcCtx, playerId)
				l.Infof("[spectate] Spectating conflicts with concurrent queue join; removing self player=%d battle=%d ticket=%t locked=%t",
					playerId, battleId, recheckTicket != nil, recheckLocked)
				metrics.ObserveWatchBattle("queued")
				return &matchpb.WatchBattleResponse{
					ErrorMessage: tipErr(constants.ErrSpectateWhileQueued, "匹配中无法观战"),
				}, nil
			}
			l.Infof("[spectate] Spectator joined successfully player=%d battle=%d node=%d random=%t",
				playerId, battleId, record.GetBattleNodeId(), randomMode)
			metrics.ObserveWatchBattle("ok")
			return &matchpb.WatchBattleResponse{BattleId: battleId, SpectatorOperationId: original.GetSpectatorOperationId()}, nil
		}
		if errors.Is(err, errAddObserverOutcomeUnknown) {
			// 结果未知(多半是请求 ctx 截止):观众可能已在 battle 挂上,要回失败就先摘掉,与回滚标记保持一致。
			// removeObserver 走独立 ctx,是补偿;带同一个 op,截止前还会给它立墓碑,挡住仍在途中的 Add。
			removeObserver(l.svcCtx, record.GetBattleNodeId(), original, "watch_outcome_unknown")
		}
		deleteSpectateWatching(l.svcCtx, playerId)
		// RPC 失败(rejectTip=0)不是 battle 的业务拒绝,按「其它拒绝」处理:不动索引、不换场。
		clientTip, outcome, roomMissing := constants.ErrBattleNotWatchable, "rejected", false
		if rejectTip != 0 {
			clientTip, outcome, roomMissing = spectateRejectVerdict(rejectTip)
		}
		if roomMissing {
			if roomMayBeCreating(record, published, checkedAtMs) {
				// 房间可能还在建:只回未找到,记录与活跃集合都不动。
				l.Infof("[spectate] Specified room reported missing but record remains within room creation window; retaining index player=%d battle=%d created_at_ms=%d: %v",
					playerId, battleId, record.GetSummary().GetCreatedAtMs(), err)
			} else {
				l.Infof("[spectate] Spectator target has ended; removing lazily player=%d battle=%d: %v", playerId, battleId, err)
				removeSpectateBattle(l.svcCtx, battleId)
				if randomMode {
					continue
				}
			}
			metrics.ObserveWatchBattle(outcome)
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(clientTip, "该战斗不存在或已结束"),
			}, nil
		}
		if clientTip == constants.ErrSpectateFull {
			// 满员是规则拒绝,不是故障;随机模式也不换场,同一请求的结论不随选序漂移。
			l.Infof("[spectate] Spectator seats full player=%d battle=%d node=%d random=%t",
				playerId, battleId, record.GetBattleNodeId(), randomMode)
			metrics.ObserveWatchBattle(outcome)
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(clientTip, "观战人数已满"),
			}, nil
		}
		l.Errorf("[spectate] AddObserver failed player=%d battle=%d node=%d: %v",
			playerId, battleId, record.GetBattleNodeId(), err)
		metrics.ObserveWatchBattle(outcome)
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(clientTip, "该战斗当前无法观战"),
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
		l.Errorf("[spectate] Failed to read spectator location player=%d: %v", playerId, err)
		return 0
	}
	if loc == nil {
		return 0
	}
	return loc.ZoneId
}

// roomMayBeCreating 判定 AddObserver 回"房间不存在"时,是否可能只是房间还没建好 —— 是则不能懒剔除
// 索引。published 与 checkedAtMs 必须在**读记录之前**取得(WatchBattle 选场处),本函数不碰 Redis。
//
// 背景:gather 在 CreateBattle **之前**写观战记录(集群外入口 2b 第 9 条),记录存在 ≠ 房间存在;
// D82 换节点时还会把记录改写到重试节点。按 battle_id 观战(帮会历练的 battle_id 在开局前就已可见)
// 落进建房窗口时 battle 回房间不存在,懒剔除会删掉一场即将开打的战斗的记录,而丢票补签
// (RequestBattleTicket)靠它定位节点。开局成功后 gather 会同值补写(publishSpectateBattle),但卡住的
// WatchBattle 若在补写之后才执行 DEL,记录就真丢了。
//
// 两个条件同时成立才算"可能在建":
//  1. 读记录之前这场还不在活跃集合里。publishSpectateBattle 先写最终记录、后 ZADD,所以读记录前已在
//     集合里 ⇒ 读到的必是实际建房节点的最终记录,而且房间在公开前就已建成,此时的"房间不存在"是
//     真收尾。先读记录、后查集合则不成立:可能读到指向首选节点的旧记录,随后 gather 改写记录、在
//     重试节点建房并公开,查集合命中,AddObserver 却打到首选节点。
//  2. checkedAtMs 距记录的 created_at_ms 不超过 gatherCreateStageWorst(gather 从写记录到最后一次
//     CreateBattle 返回的最坏耗时,含 D82 改写)。出了窗口,读记录时 D82 改写必已完成(读到的是最终
//     记录),房间还没建成则 gather 按预算已放弃(失败路径自己删记录),剔除正确;超出预算的极端
//     Redis 卡顿不在保证之内(见 spectate.go spectateRedisOpWorst)。
//
// 查活跃集合失败时调用方按"未公开"传入,窗口内就不剔除:误留的记录指向不存在的房间,补签 / 观战拿到
// 的仍是"房间不存在",随 TTL 自清;误删则可能让进行中的战斗丢票后回不去。created_at_ms 由另一个 match
// 实例的墙钟写入,实例间时钟偏差只让窗口平移同样的量:偏窄退回旧行为(懒剔除 + 开局补写兜底),
// 偏宽只是晚些剔除。created_at_ms 为 0(旧记录)按窗口外处理。
func roomMayBeCreating(record *matchpb.SpectateBattleRecord, published bool, checkedAtMs uint64) bool {
	if published {
		return false
	}
	createdAtMs := record.GetSummary().GetCreatedAtMs()
	if createdAtMs == 0 {
		return false
	}
	if checkedAtMs > createdAtMs && checkedAtMs-createdAtMs > uint64(gatherCreateStageWorst.Milliseconds()) {
		return false
	}
	return true
}

// spectateBattlePublished 查一场是否已在活跃集合(已公开)。查询失败按未公开处理并记日志
// (后果见 roomMayBeCreating 末段:窗口内不剔除,记录缺失时也不摘集合成员)。
func (l *WatchBattleLogic) spectateBattlePublished(battleId uint64) bool {
	_, err := l.svcCtx.MatchRedis.Zscore(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10))
	if err == nil {
		return true
	}
	if !errors.Is(err, redis.Nil) {
		l.Errorf("[spectate] Failed to check active set; treating battle as private battle=%d: %v", battleId, err)
	}
	return false
}

// removeActiveMember 只从活跃集合摘掉一个残留成员(记录已不在时用),不碰记录键。
func (l *WatchBattleLogic) removeActiveMember(battleId uint64) {
	if _, err := l.svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		l.Errorf("[spectate] Spectator index ZREM failed battle=%d: %v", battleId, err)
	}
}
