package logic

import (
	"context"

	"match/internal/constants"
	"match/internal/metrics"
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
//   - MATCH_MODE_PVE_TEAM 按 battle_config_id 查凑满人数,FIFO 凑单(上限 5 收口,D14);
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
		l.Infof("[match] JoinQueue 携带预组队成员 %d 人,一期忽略,仅本人入队 player=%d",
			len(in.PartyMemberIds), playerId)
	}

	// 模式与凑满人数。
	var required uint32
	switch in.Mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
		required = 1
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		required = l.svcCtx.Config.PveTeamSizeFor(in.BattleConfigId)
		if required == 0 {
			metrics.ObserveJoinQueue(modeName, "no_team_size")
			return &matchpb.JoinQueueResponse{
				ErrorCode:    constants.ErrTeamSizeNotConfigured,
				ErrorMessage: tipErr(constants.ErrTeamSizeNotConfigured, "该副本未开放组队"),
			}, nil
		}
		// 队伍上限 5 收口(D14):DungeonTable 历史行可能配 10,与引擎
		// Initialize 校验同口径压到 kMaxBattleTeamSize。
		if required > kMaxBattleTeamSize {
			required = kMaxBattleTeamSize
		}
	case matchpb.MatchMode_MATCH_MODE_1V1:
		required = 2
	case matchpb.MatchMode_MATCH_MODE_5V5:
		required = required5v5Players
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
		l.Errorf("[match] JoinQueue 查战斗锁失败 player=%d: %v", playerId, err)
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
	existing, err := loadTicket(l.svcCtx, playerId)
	if err != nil {
		l.Errorf("[match] JoinQueue 读 ticket 失败 player=%d: %v", playerId, err)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if existing != nil {
		healed, err := l.healOrphanQueuedTicket(playerId, existing)
		if err != nil {
			l.Errorf("[match] JoinQueue 校验既有票据失败 player=%d: %v", playerId, err)
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
		l.Errorf("[match] JoinQueue 读玩家位置失败 player=%d: %v", playerId, err)
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

	ticket := &queueTicket{
		Ticket:       uuid.New().String(),
		Mode:         int32(in.Mode),
		Config:       in.BattleConfigId,
		EnqueuedAtMs: nowMs(),
		ZoneId:       loc.ZoneId,
	}

	// PVE solo 即时开战(伪匹配):不入队,ticket 直接进 matched 态走 gather。
	// 不入队所以 QueueKey 留空;matched 短 TTL 与队列路径同口径(按 1 人算,D5)。
	if in.Mode == matchpb.MatchMode_MATCH_MODE_PVE_SOLO {
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
		l.Infof("[match] PVE solo 即时开战 player=%d zone=%d config=%d ticket=%s",
			playerId, loc.ZoneId, config, ticket.Ticket)
		metrics.ObserveJoinQueue(modeName, "ok")
		return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
	}

	// 入队:先写 ticket 再入队,保证 matcher 弹出时票据一定可见;入队本身是
	// SADD 注册集 + RPUSH 队列 + ZADD 评分镜像一条 Lua(决策 D3 / §11)。评分在
	// 这里读出(按玩家分布的 key,与队列不同 slot)记进票据并作为 ARGV 传入;
	// 读失败按默认 1500,不拒绝排队。
	ticket.State = ticketStateQueued
	ticket.QueueKey = matchQueueKey(int32(in.Mode), in.BattleConfigId)
	ticket.Rating = loadRatingOrDefault(l.svcCtx, playerId)
	if resp := l.createTicket(playerId, ticket, ticketTTLSeconds(l.svcCtx), modeName); resp != nil {
		return resp, nil
	}
	if err := enqueueAtomic(l.svcCtx, ticket.QueueKey, playerId, ticket.Rating); err != nil {
		l.Errorf("[match] JoinQueue 入队失败 player=%d queue=%s: %v", playerId, ticket.QueueKey, err)
		deleteTicket(l.svcCtx, playerId)
		metrics.ObserveJoinQueue(modeName, "internal")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrInternal,
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	l.Infof("[match] 入队成功 player=%d zone=%d mode=%s config=%d required=%d rating=%s ticket=%s",
		playerId, loc.ZoneId, modeName, in.BattleConfigId, required, formatRating(ticket.Rating), ticket.Ticket)
	metrics.ObserveJoinQueue(modeName, "ok")
	return &matchpb.JoinQueueResponse{QueueTicket: ticket.Ticket}, nil
}

// healOrphanQueuedTicket 识别并清掉"票据 queued 但队列里没有人"的残留(见
// isQueuedTicketInQueue 的成因列表):票据与队列分属不同 slot,任何一处的
// 崩溃/故障切换都能让两者脱节,而 matcher 只遍历队列、CancelQueue 是唯一能删票
// 的路径 —— 玩家会被 ErrAlreadyQueued 卡满 6h。带 ticket id 且要求仍是 queued 的
// CAS 删票:与此同时被 matcher 弹出推进 matched 的票据不会被误删。返回 true 表示
// 旧票据已清、调用方可以继续按本次请求入队;false 表示确实在途(或已 matched)。
//
// 与并发窗口的叠加(popGroup 弹出后尚未 setTicketMatched / requeueFront CAS 后
// 尚未 LPUSH)都是良性:前者 matcher 的 CAS 失败把他剔出组,后者队列里多一份
// 由 popGroup 去重,两条路都不会留下孤儿。
func (l *JoinQueueLogic) healOrphanQueuedTicket(playerId uint64, existing *queueTicket) (bool, error) {
	if existing.State == ticketStateReady {
		// ready = 开局成功后的短暂停留态(ReadyTicketTTLSeconds 自清)。走到这里说明
		// 上面的 battle:lock 咨询性检查已经放行 —— 锁在结算应用/作废时被 scene 删除,
		// 即那场战斗已经结束(或从未把锁落下)。此时 ready 票据只是尚未过期的残留,
		// 不能再拦"打完立刻再排"(2026-09-02 跨 zone 冒烟复跑:上一局 16s 前结束,
		// ready 票据 TTL 60s 未到,JoinQueue 一直 ErrAlreadyQueued)。CAS 按 ticket id
		// 删,避免误删并发新建的票据。matched 态不在此列:gather 在途,靠 matched TTL 自愈。
		deleteTicketIfOwned(l.svcCtx, playerId, existing.Ticket)
		l.Infof("[match] 清掉已结束战斗残留的 ready 票据 player=%d ticket=%s queue=%q",
			playerId, existing.Ticket, existing.QueueKey)
		return true, nil
	}
	if existing.State != ticketStateQueued {
		return false, nil
	}
	inQueue, err := isQueuedTicketInQueue(l.svcCtx, playerId, existing)
	if err != nil {
		return false, err
	}
	if inQueue {
		return false, nil
	}
	deleted, err := cancelTicketIfQueued(l.svcCtx, playerId, existing.Ticket)
	if err != nil {
		return false, err
	}
	if !deleted {
		// 探测与删票之间被 matcher 推进了 matched(队列里没人是因为刚被弹出)。
		l.Infof("[match] 票据在自愈前已被弹出 player=%d ticket=%s", playerId, existing.Ticket)
		return false, nil
	}
	l.Errorf("[match] 清掉崩溃残留的孤儿票据(queued 但不在队列)player=%d ticket=%s queue=%q enqueued_at_ms=%d",
		playerId, existing.Ticket, existing.QueueKey, existing.EnqueuedAtMs)
	return true, nil
}

// createTicket 不存在才创建票据(createTicketIfAbsent);已存在按 ErrAlreadyQueued
// 返回(同一玩家并发 JoinQueue 的后来者,回现存票据 id)。返回非 nil 即错误应答。
func (l *JoinQueueLogic) createTicket(playerId uint64, ticket *queueTicket, ttl int, modeName string) *matchpb.JoinQueueResponse {
	created, err := createTicketIfAbsent(l.svcCtx, playerId, ticket, ttl)
	if err != nil {
		l.Errorf("[match] JoinQueue 写 ticket 失败 player=%d: %v", playerId, err)
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
		l.Errorf("[match] JoinQueue 读并发票据失败 player=%d: %v", playerId, err)
	}
	existingId := ""
	if winner != nil {
		existingId = winner.Ticket
	}
	l.Infof("[match] JoinQueue 并发重复入队,后来者拒绝 player=%d existing=%s", playerId, existingId)
	metrics.ObserveJoinQueue(modeName, "already_queued")
	return &matchpb.JoinQueueResponse{
		ErrorCode:    constants.ErrAlreadyQueued,
		QueueTicket:  existingId,
		ErrorMessage: tipErr(constants.ErrAlreadyQueued, "已在匹配队列中"),
	}
}
