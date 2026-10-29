package logic

import (
	"context"
	"time"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"shared/safego"
)

// tipErr 组装客户端提示(照 friend/guild 的 per-service tip 模式)。
func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

type JoinQueueLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewJoinQueueLogic(ctx context.Context, svcCtx *svc.ServiceContext) *JoinQueueLogic {
	return &JoinQueueLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// JoinQueue 玩家入队(设计文档 §5.4 / §11;跨 zone 见 cross-zone-matchmaking.md):
//   - 咨询性查 battle:lock:{player_id},存在即拒(权威判定仍在 scene 的 InBattleComp);
//   - 读 player:{id}:location 取 zone 记进票据;位置缺失直接拒(ErrNotInScene,
//     决策 D6:没有位置的玩家 gather 必败,提前拒比进队再失败省);
//   - 匹配池全局不分 zone(决策 D1),zone 只进票据与日志;
//   - MATCH_MODE_PVE_SOLO 即时开战:不入队,直接走 gather 开局管线;
//   - MATCH_MODE_PVE_TEAM 按人数档凑单(pve-team-size-matching.md):请求的 team_size 就是凑满人数,
//     0 = 该副本上限(battle_config_id 查配置,上限 5 收口,D14),超过上限拒绝;
//     1 人档与 PVE_SOLO 走同一条即时开战路径,2 人起进各自人数档的 FIFO 队列;
//   - MATCH_MODE_1V1 两人凑对;MATCH_MODE_5V5 凑 10 人(二期开放,D15);
//   - 3v3 未开放,切磋走 ChallengePlayer,均拒绝直接入队。
func (l *JoinQueueLogic) JoinQueue(in *matchpb.JoinQueueRequest) (*matchpb.JoinQueueResponse, error) {
	playerId := authoritativePlayerID(l.ctx, in.PlayerId)
	modeName := in.Mode.String()
	if playerId == 0 {
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}

	// 一期只做单人入队 FIFO 凑单,预组队(party_member_ids)二期接入。
	if len(in.PartyMemberIds) > 1 {
		l.Infof("[match] JoinQueue includes %d preformed party members; ignored in phase one, only caller queued player=%d",
			len(in.PartyMemberIds), playerId)
	}

	// 模式与凑满人数。
	var required uint32
	switch in.Mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		required = 1
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		// 副本人数上限:配置值按引擎每队上限 5 收口(D14,requiredPlayers 同一口径)。
		limit := requiredPlayers(l.svcCtx, int32(in.Mode), in.BattleConfigId)
		if limit == 0 {
			metrics.ObserveJoinQueue(modeName, "no_team_size")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrTeamSizeNotConfigured,
				ErrorMessage: tipErr(constants.ErrTeamSizeNotConfigured, "该副本未开放组队"),
			}, nil
		}
		// 人数档:请求要几人就凑几人;0 = 上限(老客户端不填 team_size,行为与加字段前一致)。
		required = in.TeamSize
		if required == 0 {
			required = limit
		}
		if required > limit {
			metrics.ObserveJoinQueue(modeName, "team_size_over_limit")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrModeNotOpen,
				ErrorMessage: tipErr(constants.ErrModeNotOpen, "该副本不支持这个队伍人数"),
			}, nil
		}
	case matchpb.MatchMode_MATCH_MODE_1V1:
		required = 2
	case matchpb.MatchMode_MATCH_MODE_5V5:
		required = required5v5Players
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		sideSize := in.TeamSize
		if sideSize == 0 {
			sideSize = 1
		}
		if sideSize > kMaxBattleTeamSize {
			return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrModeNotOpen,
				ErrorMessage: tipErr(constants.ErrModeNotOpen, "PVP每方人数须为1到5人")}, nil
		}
		required = sideSize * 2
	default:
		// 3v3 未开放;切磋(PVP_CHALLENGE)点名成局,不走排队入口。
		metrics.ObserveJoinQueue(modeName, "mode_not_open")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrModeNotOpen,
			ErrorMessage: tipErr(constants.ErrModeNotOpen, "该匹配模式未开放"),
		}, nil
	}

	// 咨询性检查战斗锁。
	locked, err := isPlayerBattleLocked(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to check battle lock player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if locked {
		metrics.ObserveJoinQueue(modeName, "in_battle")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInBattle,
			ErrorMessage: tipErr(constants.ErrInBattle, "战斗尚未结束,无法排队"),
		}, nil
	}

	// 每玩家最多一单在途(设计决策 D4)。
	if l.svcCtx.Authority != nil {
		loc, e := loadPlayerLocation(l.svcCtx, playerId)
		if e != nil {
			return nil, e
		}
		if loc == nil {
			return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrNotInScene, ErrorMessage: tipErr(constants.ErrNotInScene, "请先进入场景")}, nil
		}
		stale, e := l.callerSessionStale(playerId)
		if e != nil {
			return nil, e
		}
		if stale {
			return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrNotInScene, ErrorMessage: tipErr(constants.ErrNotInScene, "会话已经失效")}, nil
		}
		return globalJoin(l, in, required, loc.ZoneId)
	}
	existing, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to read ticket player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if existing != nil {
		healed, err := l.healOrphanQueuedTicket(playerId, existing)
		if err != nil {
			l.Errorf("[match] JoinQueue failed to validate existing ticket player=%d: %v", playerId, err)
			metrics.ObserveJoinQueue(modeName, "internal")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrInternal,
				ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
			}, nil
		}
		if !healed {
			metrics.ObserveJoinQueue(modeName, "already_queued")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrAlreadyQueued,
				QueueTicket:  existing.Ticket,
				ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中"),
			}, nil
		}
	}

	// 位置 → zone。契约 key 只读 SharedRedis;缺失即玩家不在任何场景。
	loc, err := loadPlayerLocation(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to read player location player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if loc == nil {
		metrics.ObserveJoinQueue(modeName, "not_in_scene")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrNotInScene,
			ErrorMessage: tipErr(constants.ErrNotInScene, "请先进入场景"),
		}, nil
	}

	// 跨会话栅栏:发起本次请求的会话必须仍是该玩家的当前会话。
	stale, err := l.callerSessionStale(playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to read player session player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if stale {
		metrics.ObserveJoinQueue(modeName, "stale_session")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrNotInScene,
			ErrorMessage: tipErr(constants.ErrNotInScene, "请先进入场景"),
		}, nil
	}

	ticket := &queueTicket{
		Ticket:       uuid.New().String(),
		Mode:         int32(in.Mode),
		Config:       in.BattleConfigId,
		EnqueuedAtMs: nowMs(),
		ZoneId:       loc.ZoneId,
	}

	// 单人即时开战(伪匹配):PVE_SOLO,以及 PVE_TEAM 的 1 人档 —— 一个人不需要等别人,
	// 进队列只会多绕一轮 matcher。不入队,ticket 直接进 matched 态走 gather。
	// 不入队所以 QueueKey 留空;matched 短 TTL 与队列路径同口径(按 1 人算,D5)。
	// 只有这两种模式的 required 可能是 1(1v1 = 2、5v5 = 10)。
	if required == 1 {
		ticket.State = ticketStateMatched
		if resp := l.createTicket(playerId, ticket, matchedTicketTTLFor(l.svcCtx, required), modeName); resp != nil {
			return resp, nil
		}
		svcCtx := l.svcCtx
		mode := in.Mode
		config := in.BattleConfigId
		tickets := map[uint64]string{playerId: ticket.Ticket}
		safego.Go("match.gather.pve_solo", func() {
			runGatherFn(svcCtx, mode, config, []uint64{playerId}, false, tickets)
		})
		l.Infof("[match] Starting single-player PVE battle immediately player=%d zone=%d mode=%s config=%d ticket=%s",
			playerId, loc.ZoneId, modeName, config, ticket.Ticket)
		metrics.ObserveJoinQueue(modeName, "ok")
		return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
	}

	// 入队:先写 ticket 再入队,保证 matcher 弹出时票据一定可见;入队本身是
	// SADD 注册集 + RPUSH 队列 + ZADD 评分镜像一条 Lua(决策 D3 / §11)。评分在
	// 这里读出(按玩家分布的 key,与队列不同 slot)记进票据并作为 ARGV 传入;
	// 读失败按默认 1500,不拒绝排队。
	ticket.State = ticketStateQueued
	ticket.QueueKey = queueKeyForJoin(in.Mode, in.BattleConfigId, required)
	ticket.Rating = loadRatingOrDefault(l.svcCtx, playerId)
	if resp := l.createTicket(playerId, ticket, ticketTTLSeconds(l.svcCtx), modeName); resp != nil {
		return resp, nil
	}
	// 入队用请求 ctx:截止之后不再发出(docs/design/client-request-ordering.md §6.1)。
	if err := enqueueAtomicCtx(l.ctx, l.svcCtx, ticket.QueueKey, playerId, ticket.Rating); err != nil {
		l.Errorf("[match] JoinQueue failed to enqueue player=%d queue=%s: %v", playerId, ticket.QueueKey, err)
		// 补偿删票按 ticket id CAS(独立 ctx,不继承已取消的请求 ctx)。原先是无条件 DEL:
		// 这条补偿可能晚于 gate 放行的同一会话下一条 JoinQueue 落地,无条件 DEL 会删掉新票,
		// 留下「入队成功却没有票」的残留。
		deleteTicketIfOwned(l.svcCtx, playerId, ticket.Ticket)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	l.Infof("[match] Enqueued successfully player=%d zone=%d mode=%s config=%d required=%d rating=%s ticket=%s",
		playerId, loc.ZoneId, modeName, in.BattleConfigId, required, formatRating(ticket.Rating), ticket.Ticket)
	metrics.ObserveJoinQueue(modeName, "ok")
	return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
}

// callerSessionStale 复核发起本次请求的会话仍是该玩家的当前会话
// (docs/design/client-request-ordering.md §6.3 / O13)。返回 true 表示请求来自旧会话或玩家已不在线,应拒绝。
//
// 为什么要这一道:gate 只在同一个会话内保序。玩家断线重连(可能换了 gate)之后,旧会话上还在途的 JoinQueue
// 与新会话上的 CancelQueue 互不排序,旧的 Join 晚落地会让玩家在新会话里「取消成功、实际仍在排队」。
// player_locator 的会话记录随重连换成新的 session_id / gate_instance_id,旧会话的请求据此拒绝。
//
// 不原子:会话在 SharedRedis,票据在 MatchRedis,读完之后会话仍可能被替换。这一道只是缩小跨会话窗口,
// 剩余的由票据 TTL、GetQueueStatus 权威查询与客户端重连后回源兜底。
// 没有会话 metadata 的调用(内部调用、测试)不做这道复核。
func (l *JoinQueueLogic) callerSessionStale(playerId uint64) (bool, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok {
		return false, nil
	}
	session, err := playercontract.LoadSession(l.ctx, l.svcCtx, playerId)
	if err != nil {
		return false, err
	}
	if session == nil || !playercontract.IsSessionOnline(session) {
		return true, nil
	}
	if session.GetSessionId() != detail.GetSessionId() || session.GetGateInstanceId() != detail.GetGateInstanceId() {
		l.Infof("[match] JoinQueue from a superseded session; rejecting player=%d request_session=%d current_session=%d",
			playerId, detail.GetSessionId(), session.GetSessionId())
		return true, nil
	}
	return false, nil
}

// healOrphanQueuedTicket JoinQueue 的票据自愈,实现收口在包级函数 healOrphanTicket
// (整队开战预检共用,team-system.md §E.1 第 5 步),行为与抽出前逐字一致。
func (l *JoinQueueLogic) healOrphanQueuedTicket(playerId uint64, existing *queueTicket) (bool, error) {
	return healOrphanTicket(l.svcCtx, l.Logger, playerId, existing)
}

// healOrphanTicket 识别并清掉"票据 queued 但队列里没有人"的残留(见
// isQueuedTicketInQueue 的成因列表):票据与队列分属不同 slot,任何一处的
// 崩溃/故障切换都能让两者脱节,而 matcher 只遍历队列、CancelQueue 是唯一能删票
// 的路径 —— 玩家会被 ErrAlreadyQueued 卡满 6h。带 ticket id 且要求仍是 queued 的
// CAS 删票:与此同时被 matcher 弹出推进 matched 的票据不会被误删。返回 true 表示
// 旧票据已清、调用方可以继续按本次请求入队 / 开战;false 表示确实在途(或已 matched)。
//
// 前置条件:调用方已确认 battle:lock 不存在(ready 分支依赖它,见下)。
// log 由调用方给(JoinQueue 带请求上下文的 Logger,整队开战给 logx.WithContext)。
//
// 与并发窗口的叠加(popGroup 弹出后尚未 setTicketMatched / requeueFront CAS 后
// 尚未 LPUSH)都是良性:前者 matcher 的 CAS 失败把他剔出组,后者队列里多一份
// 由 popGroup 去重,两条路都不会留下孤儿。
func healOrphanTicket(svcCtx *svc.ServiceContext, log logx.Logger, playerId uint64, existing *queueTicket) (bool, error) {
	if svcCtx.Authority != nil {
		if existing.State == ticketStateReady {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			t, e := svcCtx.Authority.Ticket(ctx, playerId)
			if e != nil {
				return false, e
			}
			if t != nil && t.State == 3 {
				return releaseGlobalFinished(ctx, svcCtx, t)
			}
		}
		return false, nil
	}
	if existing.State == ticketStateReady {
		// ready = 开局成功后的短暂停留态(ReadyTicketTTLSeconds 自清)。走到这里说明
		// 上面的 battle:lock 咨询性检查已经放行 —— 锁在结算应用/作废时被 scene 删除,
		// 即那场战斗已经结束(或从未把锁落下)。此时 ready 票据只是尚未过期的残留,
		// 不能再拦"打完立刻再排"(2026-09-02 跨 zone 冒烟复跑:上一局 16s 前结束,
		// ready 票据 TTL 60s 未到,JoinQueue 一直 ErrAlreadyQueued)。CAS 按 ticket id
		// 删,避免误删并发新建的票据。matched 态不在此列:gather 在途,靠 matched TTL 自愈。
		deleteTicketIfOwned(svcCtx, playerId, existing.Ticket)
		log.Infof("[match] Removed ready ticket left by an ended battle player=%d ticket=%s queue=%q",
			playerId, existing.Ticket, existing.QueueKey)
		return true, nil
	}
	if existing.State != ticketStateQueued {
		return false, nil
	}
	inQueue, err := isQueuedTicketInQueue(svcCtx, playerId, existing)
	if err != nil {
		return false, err
	}
	if existing.PartyId != "" {
		// 队伍补位票:不在队列时撤的是整单(不只删自己这一张),在队列时还要问 team 这一单是否仍然有效。
		return healPartyTicket(svcCtx, log, playerId, existing, inQueue)
	}
	if inQueue {
		return false, nil
	}
	deleted, err := cancelTicketIfQueued(svcCtx, playerId, existing.Ticket)
	if err != nil {
		return false, err
	}
	if !deleted {
		// 探测与删票之间被 matcher 推进了 matched(队列里没人是因为刚被弹出)。
		log.Infof("[match] Ticket popped before recovery player=%d ticket=%s", playerId, existing.Ticket)
		return false, nil
	}
	log.Errorf("[match] Removed orphan ticket left by a crash (queued but absent from queue) player=%d ticket=%s queue=%q enqueued_at_ms=%d",
		playerId, existing.Ticket, existing.QueueKey, existing.EnqueuedAtMs)
	return true, nil
}

// createTicket 不存在才创建票据(createTicketIfAbsent);已存在按 ErrAlreadyQueued
// 返回(同一玩家并发 JoinQueue 的后来者,回现存票据 id)。返回非 nil 即错误应答。
//
// 建票是 JoinQueue 的提交点,受请求 ctx 约束:先检查 ctx,再用请求 ctx 发出建票脚本。
// gate 对同一会话单飞,路由服回超时就放行下一条(例如 CancelQueue);截止之后才建出的票会晚于
// 那条取消落地,玩家看到「取消成功」却仍在排队(docs/design/client-request-ordering.md §1.3、§6.1)。
func (l *JoinQueueLogic) createTicket(playerId uint64, ticket *queueTicket, ttl int, modeName string) *matchpb.JoinQueueResponse {
	if err := l.ctx.Err(); err != nil {
		l.Errorf("[match] JoinQueue request deadline passed before writing ticket; not writing player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}
	}
	created, err := createTicketIfAbsentCtx(l.ctx, l.svcCtx, playerId, ticket, ttl)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to write ticket player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}
	}
	if created {
		return nil
	}
	winner, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue failed to read concurrent ticket player=%d: %v", playerId, err)
	}
	existingId := ""
	if winner != nil {
		existingId = winner.Ticket
	}
	l.Infof("[match] JoinQueue concurrent duplicate enqueue; rejecting later request player=%d existing=%s", playerId, existingId)
	metrics.ObserveJoinQueue(modeName, "already_queued")
	return &matchpb.JoinQueueResponse{
		ErrorCode:    constants.ErrAlreadyQueued,
		QueueTicket:  existingId,
		ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中"),
	}
}
