package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
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
//     RemoveObserver 一律带同一个 op。标记只在拿到取消证明或 Add 确定不会登记时才按值删(规则见 stopWatching),
//     换场时旧观战取消未获证明就回 16004、不登记新操作;
//   - 会话复核:离线或已被重连替换的会话回「会话不在线」,在清退旧观战之前判定(与 JoinQueue、全局路径同口径);
//   - 可见性闸(battle-spectate-tiered-fanout.md §10 第 6 条):指定场在互斥检查之后、任何副作用(清退旧观战、
//     写标记、挂观众)之前选定,先查活跃集合、再读记录。记录已不在与私密局(记录 visibility 非 PUBLIC)回同一个
//     「不存在或已结束」,都不清退旧观战 —— 否则「旧观战有没有被拆」会成为探测私密局是否存在的旁路;两种情形
//     都只摘读记录前已在活跃集合的成员、不 DEL 记录(私密局的记录是丢票补签定位 battle 节点的唯一来源);
//   - battle_id=0 随机挑一场:挑到记录已不在 / 私密的成员摘掉后换一场,AddObserver 报房间不存在时懒剔除索引
//     后换一场,合计最多挑两次;指定场读记录前未公开且仍在建房窗口内(gather 先写记录后建房,D82 还会
//     改写记录)时,房间不存在只回"不存在"、不剔除(roomMayBeCreating);
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
//     (turn-based §22 D69),收到首帧才算进入观战;
//   - 档位(battle-spectate-tiered-fanout.md §15.6,与全局路径共用 spectateAdmissionTier):本场还是 DIRECT 才走上面的 L0;
//     L0 回满员且中继可用时把本场升到 RELAY,本次请求继续做 L1 准入,不回 16021;已升档的场不再撞 L0。L1 / L2 观众没有
//     观战标记,回 delivery=LOBBY 与观战凭证,客户端凭它到 gate 订阅(admitSpectateLobby)。
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
		return l.internalWatchResponse(playerId, stage, err), nil
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

	// 可见性闸:指定场在任何副作用之前选定(见函数注释);放在互斥检查之后,排队中 / 战斗中的玩家不论
	// 指定哪一场都先得到同一个互斥结论,不额外暴露目标场的信息。随机模式在下方循环里按同一判定逐个复核。
	randomMode := in.BattleId == 0
	var specified watchTarget
	if !randomMode {
		var outcome string
		if specified, outcome, err = l.selectSpecifiedBattle(in.BattleId); err != nil {
			return internalErr("读观战记录", err)
		}
		if outcome != "" {
			if outcome == "private" {
				l.Infof("[spectate] Refusing to spectate non-public battle player=%d battle=%d", playerId, in.BattleId)
			}
			metrics.ObserveWatchBattle(outcome)
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrBattleNotWatchable, "该战斗不存在或已结束"),
			}, nil
		}
	}

	// 观众必须在线:观众路由(session/gate)从 player:session 组装,观众尚无直连时
	// 落点分配(NotifyBattleAssigned)经 gate 推送(turn-based §22 D68 大厅公告),
	// 不在线没有可路由的会话。这一段要在清退旧观战之前:离线或已被替换的会话不该有任何副作用。
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
	// 跨会话栅栏(client-request-ordering.md §6.3,与 JoinQueue callerSessionStale、全局路径同口径):gate 只在同一会话内保序,
	// 玩家重连后旧会话上还在途的请求可能晚到,不能拆新会话的观战、再用新会话的路由挂旧请求选的场。
	// 没有会话 metadata 的调用(内部调用、测试)不做这道复核。
	if detail, ok := ctxkeys.GetSessionDetails(l.ctx); ok &&
		(detail.GetSessionId() != session.GetSessionId() || detail.GetGateInstanceId() != session.GetGateInstanceId()) {
		l.Infof("[spectate] WatchBattle from a superseded session; rejecting player=%d request_session=%d current_session=%d",
			playerId, detail.GetSessionId(), session.GetSessionId())
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
		// 换场、随机模式与重看同一场一律沿旧原件取消,拿到证明才删旧标记(标记值非法时只删标记)。
		// 重看同一场也要先摘:battle 名单里挂着旧 op 时,新 op 的 Add 会被观战账本拒绝(只认原 op 的逐字节
		// 重放),而旧原件的准入截止多半已过、无法原样重放。代价是旧会话会先收到一次 SpectateEnd(rewatch),
		// 随后凭新分配重连;全局路径(prepareGlobalWatch 先 cancelGlobalSpectator)同口径。
		// 取消未获证明时不登记新操作:旧观众可能还挂着,再挂新观众就成了同时观战两场。客户端重试时沿同一个 op 再取消。
		if err := stopWatching(l.svcCtx, playerId, watchingRaw, "rewatch"); err != nil {
			return internalErr("清退旧观战", err)
		}
		// 后续成功路径的 SetnxEx 会为新观战原子抢占标记。
	}

	// 选场 + 挂观众。指定场已在副作用之前选定;随机模式(battle_id=0)挑到记录已不在 / 私密的成员、
	// 或 AddObserver 报房间不存在时,摘掉后换一场,最多挑两次(设计文档 §10.2 数据流末行)。
	maxAttempts := 1
	if randomMode {
		maxAttempts = 2
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		target := specified
		if randomMode {
			picked, err := pickRandomBattle(l.svcCtx)
			if err != nil {
				return internalErr("随机选场", err)
			}
			if picked == 0 {
				break // 无可观战战斗
			}
			record, err := loadSpectateBattle(l.svcCtx, picked)
			if err != nil {
				return internalErr("读观战记录", err)
			}
			// 随机模式挑的就是活跃集合成员,天然已公开;建房窗口判定只对指定场有意义。
			target = watchTarget{battleId: picked, record: record, published: true}
			if l.unwatchableOutcome(target) != "" {
				continue // 记录已不在 / 私密(与指定场同一判定):成员已摘,换一场
			}
		}
		battleId := target.battleId

		// 档位判定:已升档的场不再撞 L0,直接做 L1 / L2 准入。
		tier, err := spectateAdmissionTier(l.ctx, l.svcCtx, battleId)
		if err != nil {
			return internalErr("读观战档位", err)
		}
		var rejectTip uint32
		var cause error
		if tier == battlepb.ESpectateTier_SPECTATE_TIER_DIRECT {
			var done *matchpb.WatchBattleResponse
			done, rejectTip, cause = l.admitDirectSeat(target, playerId, routing, randomMode)
			if done != nil {
				return done, nil
			}
			if rejectTip == constants.ErrSpectateFull {
				// 直连席已满:中继可用就把本场升到 RELAY,本次请求接着做 L1 准入;不可用则 tier 仍是 DIRECT,按拒绝回 16021。
				if tier, err = spectateDirectFullToRelay(l.ctx, l.svcCtx, battleId); err != nil {
					return internalErr("升观战档位", err)
				}
			}
		}
		if tier != battlepb.ESpectateTier_SPECTATE_TIER_DIRECT {
			viewer := spectateLobbyViewer{playerId: playerId, gateNodeId: uint32(gateNodeId), gateInstanceId: session.GateInstanceId}
			var admission spectateLobbyAdmission
			admission, cause = admitSpectateLobby(l.ctx, l.svcCtx, target.record, tier, viewer)
			if cause == nil {
				l.Infof("[spectate] Lobby spectator admitted player=%d battle=%d gate=%d random=%t", playerId, battleId, viewer.gateNodeId, randomMode)
				metrics.ObserveWatchBattle("lobby")
				return &matchpb.WatchBattleResponse{
					BattleId:       battleId,
					Delivery:       matchpb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY,
					SpectateTicket: admission.ticket,
				}, nil
			}
			rejectTip = admission.rejectTip
		}
		resp, retry := l.rejectedWatch(target, playerId, rejectTip, cause, randomMode)
		if retry {
			continue
		}
		return resp, nil
	}

	metrics.ObserveWatchBattle("no_battle")
	return &matchpb.WatchBattleResponse{
		ErrorMessage: tipErr(constants.ErrNoWatchableBattle, "当前没有可观战的战斗"),
	}, nil
}

// internalWatchResponse 是 WatchBattle 的服务端故障出口:打 Error 日志、计 internal、回 16004。
func (l *WatchBattleLogic) internalWatchResponse(playerId uint64, stage string, err error) *matchpb.WatchBattleResponse {
	l.Errorf("[spectate] WatchBattle %s failed player=%d: %v", stage, playerId, err)
	metrics.ObserveWatchBattle("internal")
	return &matchpb.WatchBattleResponse{
		ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
	}
}

// admitDirectSeat 是 L0 直连席准入:固定原件 → 抢观战标记 → AddObserver → 双检。done 非 nil = 本请求已有结论,直接回给客户端;
// 否则 AddObserver 失败、标记已按规则处理完(转 L1 或换一场时不会留下这个 op 的标记),rejectTip / err 交给调用方判定
// (转 L1 或 rejectedWatch)。
func (l *WatchBattleLogic) admitDirectSeat(target watchTarget, playerId uint64, routing *battlepb.BattleRouting,
	randomMode bool,
) (done *matchpb.WatchBattleResponse, rejectTip uint32, err error) {
	battleId, record := target.battleId, target.record

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
		}, 0, nil
	}
	// 观众显示名只来自角色资料(与全局路径同口径):登录账号是私有信息,不进观众广播。
	original := newLocalObserverAdd(battleId, playerId, challengePublicName(l.ctx, l.svcCtx, playerId), routing, instance)
	mark, err := encodeWatchingMark(original)
	if err != nil {
		return l.internalWatchResponse(playerId, "组观战原件", err), 0, nil
	}

	// 先原子抢占互斥标记(SETNX+EX)再 AddObserver:标记必须先于观众登记生效,
	// 否则并发的 JoinQueue 会漏掉清退;入口已清掉本玩家旧标记,抢占失败只可能
	// 是并发 WatchBattle,由 SetnxEx 一锤定音拒绝。
	if beforeAcquireWatchingHook != nil {
		beforeAcquireWatchingHook(playerId)
	}
	// 抢占标记与挂观众是 WatchBattle 的提交点,都受请求 ctx 约束:截止之后不再发出,否则会在客户端
	// 收到超时、gate 放行同一会话的下一条请求之后才把观战绑上(docs/design/client-request-ordering.md §6.1)。
	// 失败回滚(releaseWatchingMark / stopWatching)是补偿,仍走独立 ctx。
	acquired, err := l.svcCtx.MatchRedis.SetnxExCtx(l.ctx, spectateWatchingKey(playerId),
		mark, spectateTTLSeconds(l.svcCtx))
	if err != nil {
		return l.internalWatchResponse(playerId, "写观战标记", err), 0, nil
	}
	if !acquired {
		metrics.ObserveWatchBattle("already_watching")
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(constants.ErrAlreadyWatching, "已在观战另一场战斗"),
		}, 0, nil
	}
	rejectTip, err = addObserver(l.ctx, endpoint, original)
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
			if markErr := stopWatching(l.svcCtx, playerId, mark, "concurrent_queue"); markErr != nil {
				l.Errorf("[spectate] Self-eviction not confirmed; marker kept player=%d battle=%d: %v", playerId, battleId, markErr)
			}
			l.Infof("[spectate] Spectating conflicts with concurrent queue join; removing self player=%d battle=%d ticket=%t locked=%t",
				playerId, battleId, recheckTicket != nil, recheckLocked)
			metrics.ObserveWatchBattle("queued")
			return &matchpb.WatchBattleResponse{
				ErrorMessage: tipErr(constants.ErrSpectateWhileQueued, "匹配中无法观战"),
			}, 0, nil
		}
		l.Infof("[spectate] Spectator joined successfully player=%d battle=%d node=%d random=%t",
			playerId, battleId, record.GetBattleNodeId(), randomMode)
		metrics.ObserveWatchBattle("ok")
		metrics.ObserveSpectateTier(metrics.SpectateTierDirect)
		return &matchpb.WatchBattleResponse{
			BattleId:             battleId,
			SpectatorOperationId: original.GetSpectatorOperationId(),
			Delivery:             matchpb.ESpectateDelivery_SPECTATE_DELIVERY_DIRECT,
		}, 0, nil
	}
	if errors.Is(err, errAddObserverOutcomeUnknown) {
		// 结果未知:观众可能已在 battle 挂上,或 Add 还在途中。沿原件取消(走独立 ctx,是补偿;截止前还会给这个 op 立墓碑,
		// 挡住仍在途中的 Add)。未获证明就保留标记,留给下一次 WatchBattle 重试,给客户端的结论不变。
		if markErr := stopWatching(l.svcCtx, playerId, mark, "watch_outcome_unknown"); markErr != nil {
			l.Errorf("[spectate] AddObserver outcome unknown and cancel not confirmed; marker kept player=%d battle=%d: %v", playerId, battleId, markErr)
		}
		return nil, rejectTip, err
	}
	// battle 明确拒绝,或请求截止前没发出:这个 op 不会登记,按值删标记。删不掉就按依赖故障收场:调用方接下来可能转 L1 或换一场,
	// 换场时残留的标记会让 SETNX 得到误导的「已在观战」。残留的标记由下一次 WatchBattle 取消后清掉。
	if markErr := releaseWatchingMark(l.svcCtx, playerId, mark); markErr != nil {
		return l.internalWatchResponse(playerId, "回滚观战标记", fmt.Errorf("%w (AddObserver: %v)", markErr, err)), 0, nil
	}
	return nil, rejectTip, err
}

// rejectedWatch 把 battle 的拒绝或 RPC 失败变成给客户端的结论(L0 的 AddObserver 与 L1 的 AttachRelayGate 共用)。
// retry=true 表示已懒剔除、随机模式该换一场,此时 resp 为 nil。
// RPC 失败(rejectTip=0)不是 battle 的业务拒绝,按「其它拒绝」处理:不动索引、不换场。
func (l *WatchBattleLogic) rejectedWatch(target watchTarget, playerId uint64, rejectTip uint32, cause error, randomMode bool) (resp *matchpb.WatchBattleResponse, retry bool) {
	battleId, record := target.battleId, target.record
	clientTip, outcome, roomMissing := constants.ErrBattleNotWatchable, "rejected", false
	if rejectTip != 0 {
		clientTip, outcome, roomMissing = spectateRejectVerdict(rejectTip)
	}
	if roomMissing {
		if roomMayBeCreating(record, target.published, target.checkedAtMs) {
			// 房间可能还在建:只回未找到,记录与活跃集合都不动。
			l.Infof("[spectate] Specified room reported missing but record remains within room creation window; retaining index player=%d battle=%d created_at_ms=%d: %v",
				playerId, battleId, record.GetSummary().GetCreatedAtMs(), cause)
		} else {
			l.Infof("[spectate] Spectator target has ended; removing lazily player=%d battle=%d: %v", playerId, battleId, cause)
			removeSpectateBattle(l.svcCtx, battleId)
			if randomMode {
				return nil, true
			}
		}
		metrics.ObserveWatchBattle(outcome)
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(clientTip, "该战斗不存在或已结束"),
		}, false
	}
	if clientTip == constants.ErrSpectateFull {
		// 满员是规则拒绝,不是故障;随机模式也不换场,同一请求的结论不随选序漂移。
		l.Infof("[spectate] Spectator seats full player=%d battle=%d node=%d random=%t",
			playerId, battleId, record.GetBattleNodeId(), randomMode)
		metrics.ObserveWatchBattle(outcome)
		return &matchpb.WatchBattleResponse{
			ErrorMessage: tipErr(clientTip, "观战人数已满"),
		}, false
	}
	l.Errorf("[spectate] Spectator admission failed player=%d battle=%d node=%d: %v",
		playerId, battleId, record.GetBattleNodeId(), cause)
	metrics.ObserveWatchBattle(outcome)
	return &matchpb.WatchBattleResponse{
		ErrorMessage: tipErr(clientTip, "该战斗当前无法观战"),
	}, false
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

// watchTarget 是 WatchBattle 选中的一场:观战记录,加上建房窗口判定(roomMayBeCreating)的两个输入。
// published / checkedAtMs 必须取在读记录**之前**:记录可能在读之后被 gather 改写(D82 换节点)。
type watchTarget struct {
	battleId    uint64
	record      *matchpb.SpectateBattleRecord // nil = 记录已不在
	published   bool                          // 读记录前已在活跃集合
	checkedAtMs uint64                        // 读记录前的本机墙钟;随机模式不用
}

// selectSpecifiedBattle 读指定场并过可见性闸。只读活跃集合与记录(不可观战时至多摘一个活跃集合成员),
// 不碰观战标记、不打 battle,所以能放在 WatchBattle 的一切副作用之前。outcome 非空 = 不可观战
// (not_found / private,见 unwatchableOutcome),调用方回「该战斗不存在或已结束」;读记录失败返回 err。
func (l *WatchBattleLogic) selectSpecifiedBattle(battleId uint64) (target watchTarget, outcome string, err error) {
	target = watchTarget{battleId: battleId, checkedAtMs: nowMs()}
	target.published = l.spectateBattlePublished(battleId)
	if target.record, err = loadSpectateBattle(l.svcCtx, battleId); err != nil {
		return target, "", err
	}
	return target, l.unwatchableOutcome(target), nil
}

// unwatchableOutcome 判定选中的场能否观战(指定场与随机模式共用这一判定):能则返回空串;记录已不在
// (TTL 到期 / 已被剔除 / gather 还没预写)返回 not_found;记录可见性非 PUBLIC(私密局,随建房原件冻结)
// 返回 private。
//
// 不可观战时只摘活跃集合里的成员、不 DEL 记录:记录缺失时 GET 已读到空,此刻 DEL 只可能删掉 GET 之后才
// 预写的记录(gather 第 4.1 步),无效或有害;私密局的记录要留给丢票补签定位 battle 节点。只摘读记录前已在
// 集合里的成员,免得误摘随后才公开(ZADD)的场;私密成员本不该在集合里(addSpectateActive 拒收),在就是残留。
func (l *WatchBattleLogic) unwatchableOutcome(target watchTarget) string {
	outcome := "not_found"
	if target.record != nil {
		if isSpectatePublic(target.record.GetVisibility()) {
			return ""
		}
		outcome = "private"
		if target.published {
			l.Infof("[spectate] Non-public battle found in spectator index; removing member only battle=%d visibility=%s",
				target.battleId, target.record.GetVisibility().String())
		}
	}
	if target.published {
		l.removeActiveMember(target.battleId)
	}
	return outcome
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
// (后果见 roomMayBeCreating 末段:窗口内不剔除;记录缺失或私密时也不摘集合成员,留给列表 / 随机选场再摘)。
func (l *WatchBattleLogic) spectateBattlePublished(battleId uint64) bool {
	_, err := l.svcCtx.MatchRedis.Zscore(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10))
	if err == nil {
		return true
	}
	if !errors.Is(err, redis.Nil) {
		l.Errorf("[spectate] Failed to check active set; treating battle as unpublished battle=%d: %v", battleId, err)
	}
	return false
}

// removeActiveMember 只从活跃集合摘掉一个成员(记录已不在 / 私密局时用),不碰记录键。
func (l *WatchBattleLogic) removeActiveMember(battleId uint64) {
	removeSpectateActiveMember(l.svcCtx, battleId)
}
