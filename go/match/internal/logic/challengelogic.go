package logic

import (
	"context"
	"errors"
	"strconv"

	"match/generated/pb/game"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"shared/safego"
)

// challenge:{id} hash 字段名。
const (
	challengeFieldChallenger = "challenger"
	challengeFieldTarget     = "target"
	challengeFieldConfig     = "config"
	challengeFieldExpiresAt  = "expires_at_ms"
)

type ChallengeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewChallengeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ChallengeLogic {
	return &ChallengeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// challengeTTL 返回挑战记录 TTL(秒)。
func (l *ChallengeLogic) challengeTTL() int {
	ttl := int(l.svcCtx.Config.ChallengeTTLSeconds)
	if ttl <= 0 {
		ttl = 60
	}
	return ttl
}

// ChallengePlayer 场景发起 PK(切磋,设计文档 §3.1b):
// 发起时刻只做咨询性检查,不冻结任何人;冻结发生在应战后的 gather。
// 同一目标同时只挂一个待应答挑战(challenge:target:{player_id} SETNX),后来者拒绝。
func (l *ChallengeLogic) ChallengePlayer(in *matchpb.ChallengePlayerRequest) (*matchpb.ChallengePlayerResponse, error) {
	challengerId := authoritativePlayerID(l.ctx, in.PlayerId)
	if challengerId == 0 {
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	if in.TargetPlayerId == 0 || in.TargetPlayerId == challengerId {
		metrics.ObserveChallenge("invite", "self")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeSelf, "不能挑战自己"),
		}, nil
	}
	// 入口白名单(PlayMode 表,docs/design/global-match-hierarchical-pools.md §7):切磋只能用表里开放的
	// 模板,客户端任填的 battle_config_id 一律拒绝。
	if !l.svcCtx.Catalog.Open(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, in.BattleConfigId) {
		metrics.ObserveChallenge("invite", "template_not_open")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrModeNotOpen, "该切磋玩法暂未开放"),
		}, nil
	}
	if l.svcCtx.Authority != nil {
		return l.createGlobalChallenge(in, challengerId)
	}

	// 咨询性检查双方 battle:lock(权威性复查在应战时刻做)。
	if locked, err := isPlayerBattleLocked(l.svcCtx, challengerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] Failed to check challenger's battle lock player=%d: %v", challengerId, err)
		}
		metrics.ObserveChallenge("invite", "self_busy")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeSelfBusy, "战斗尚未结束,无法发起切磋"),
		}, nil
	}
	if locked, err := isPlayerBattleLocked(l.svcCtx, in.TargetPlayerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] Failed to check target's battle lock target=%d: %v", in.TargetPlayerId, err)
		}
		metrics.ObserveChallenge("invite", "target_busy")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeTargetBusy, "对方正在战斗中"),
		}, nil
	}

	// 目标在线校验(挑战弹窗要经 gate 推到目标客户端)。
	targetSession, err := loadPlayerSession(l.svcCtx, in.TargetPlayerId)
	if err != nil {
		l.Errorf("[challenge] Failed to read target session target=%d: %v", in.TargetPlayerId, err)
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if !isSessionOnline(targetSession) {
		metrics.ObserveChallenge("invite", "target_offline")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeTargetOffline, "对方不在线"),
		}, nil
	}

	// challenge_id 与 battle_id 同源(biz_tag = battle)。
	challengeId, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		l.Errorf("[challenge] Failed to generate challenge_id challenger=%d: %v", challengerId, err)
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	ttl := l.challengeTTL()
	expiresAtMs := nowMs() + uint64(ttl)*1000

	// 占坑与写记录是发起挑战的提交点,受请求 ctx 约束:先检查 ctx,再用请求 ctx 发出。截止之后才挂上的挑战
	// 会晚于 gate 已放行的同一会话下一条请求生效(docs/design/client-request-ordering.md §6.1)。
	// 失败时的 cleanup 是补偿,仍走独立 ctx。
	if err := l.ctx.Err(); err != nil {
		l.Errorf("[challenge] Request deadline passed before reserving target slot; not writing challenger=%d target=%d: %v",
			challengerId, in.TargetPlayerId, err)
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	// 同一目标同时只挂一个待应答挑战:SETNX 占坑,后来者拒绝。
	acquired, err := l.svcCtx.MatchRedis.SetnxExCtx(l.ctx, challengeTargetKey(in.TargetPlayerId),
		strconv.FormatUint(challengeId, 10), ttl)
	if err != nil {
		l.Errorf("[challenge] Failed to reserve target slot target=%d: %v", in.TargetPlayerId, err)
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if !acquired {
		metrics.ObserveChallenge("invite", "pending")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengePending, "对方已有待处理的切磋邀请"),
		}, nil
	}

	cleanup := func() {
		deleteChallengeRecord(l.svcCtx, challengeId, in.TargetPlayerId)
	}

	recordKey := challengeKey(challengeId)
	if err := l.svcCtx.MatchRedis.HmsetCtx(l.ctx, recordKey, map[string]string{
		challengeFieldChallenger: strconv.FormatUint(challengerId, 10),
		challengeFieldTarget:     strconv.FormatUint(in.TargetPlayerId, 10),
		challengeFieldConfig:     strconv.FormatUint(uint64(in.BattleConfigId), 10),
		challengeFieldExpiresAt:  strconv.FormatUint(expiresAtMs, 10),
	}); err != nil {
		l.Errorf("[challenge] Failed to write challenge record challenge=%d: %v", challengeId, err)
		cleanup()
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	// 记录已写入,TTL 必须补上(否则记录永久残留),所以这一步不受请求 ctx 约束。
	if err := l.svcCtx.MatchRedis.Expire(recordKey, ttl); err != nil {
		l.Errorf("[challenge] Failed to set challenge record TTL challenge=%d: %v", challengeId, err)
	}

	challengerName := challengePublicName(l.ctx, l.svcCtx, challengerId)

	// 推挑战弹窗给目标(经 Kafka gate PushToPlayerEvent,设计决策 D6)。
	invite := &matchpb.ChallengeInviteS2C{
		ChallengeId:    challengeId,
		ChallengerId:   challengerId,
		ChallengerName: challengerName,
		BattleConfigId: in.BattleConfigId,
		ExpiresAtMs:    expiresAtMs,
	}
	if err := pushToPlayer(l.svcCtx, in.TargetPlayerId,
		uint32(game.MatchServiceNotifyChallengeInviteMessageId), invite); err != nil {
		l.Errorf("[challenge] Failed to push challenge prompt challenge=%d target=%d: %v", challengeId, in.TargetPlayerId, err)
		cleanup()
		metrics.ObserveChallenge("invite", "push_failed")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "邀请发送失败,请稍后再试"),
		}, nil
	}

	l.Infof("[challenge] Challenge initiated challenge=%d challenger=%d target=%d config=%d",
		challengeId, challengerId, in.TargetPlayerId, in.BattleConfigId)
	metrics.ObserveChallenge("invite", "ok")
	return &matchpb.ChallengePlayerResponse{ChallengeId: challengeId}, nil
}

// RespondChallenge 应战 / 拒战(设计文档 §3.1b):
// challenge 未过期校验 + 双方 battle:lock 权威性复查 → 推双方
// NotifyChallengeResult → 进入 §3.1 标准 gather(mode=PVP_CHALLENGE)。
func (l *ChallengeLogic) RespondChallenge(in *matchpb.RespondChallengeRequest) (*matchpb.RespondChallengeResponse, error) {
	responderId := authoritativePlayerID(l.ctx, in.PlayerId)
	if responderId == 0 {
		metrics.ObserveChallenge("respond", "internal")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	if l.svcCtx.Authority != nil {
		return l.respondGlobalChallenge(in, responderId)
	}

	fields, err := l.svcCtx.MatchRedis.Hgetall(challengeKey(in.ChallengeId))
	if err != nil {
		l.Errorf("[challenge] Failed to read challenge record challenge=%d: %v", in.ChallengeId, err)
		metrics.ObserveChallenge("respond", "internal")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if len(fields) == 0 {
		metrics.ObserveChallenge("respond", "expired")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeExpired, "切磋邀请已过期"),
		}, nil
	}

	challengerId, _ := strconv.ParseUint(fields[challengeFieldChallenger], 10, 64)
	targetId, _ := strconv.ParseUint(fields[challengeFieldTarget], 10, 64)
	configId, _ := strconv.ParseUint(fields[challengeFieldConfig], 10, 32)
	expiresAtMs, _ := strconv.ParseUint(fields[challengeFieldExpiresAt], 10, 64)

	if targetId != responderId {
		metrics.ObserveChallenge("respond", "not_target")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeNotTarget, "该邀请不是发给你的"),
		}, nil
	}

	// 下面是应答的决定点:消费记录、推结果、启动 gather。请求 ctx 已取消就不做决定:客户端此时已收到
	// 超时,gate 已放行同一会话的下一条请求,截止之后才开局会让顺序乱掉(docs/design/client-request-ordering.md §6.1)。
	// 决定一旦做出,消费与开局都走独立 ctx 执行到底,不半途而废。
	if err := l.ctx.Err(); err != nil {
		l.Errorf("[challenge] Request deadline passed before deciding; leaving challenge record unconsumed challenge=%d responder=%d: %v",
			in.ChallengeId, responderId, err)
		metrics.ObserveChallenge("respond", "internal")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	// 挑战记录一次性消费:应战/拒战/过期都作废(TTL 是兜底,这里显式删)。
	deleteChallengeRecord(l.svcCtx, in.ChallengeId, targetId)

	if expiresAtMs > 0 && nowMs() >= expiresAtMs {
		metrics.ObserveChallenge("respond", "expired")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeExpired, "切磋邀请已过期"),
		}, nil
	}

	// 拒战:通知发起者,收场。
	if !in.Accept {
		l.pushResult(challengerId, in.ChallengeId, false, responderId)
		l.Infof("[challenge] Challenge rejected challenge=%d challenger=%d responder=%d",
			in.ChallengeId, challengerId, responderId)
		metrics.ObserveChallenge("respond", "declined")
		return &matchpb.RespondChallengeResponse{}, nil
	}

	// 应战:双方 battle:lock 权威性复查 —— 发起后任何一方可能已经排队开战
	// (先到先得,后到的 gather 拒绝;这里提前挡住明确的冲突)。
	if locked, err := isPlayerBattleLocked(l.svcCtx, challengerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] Failed to recheck challenger's battle lock challenger=%d: %v", challengerId, err)
		}
		l.pushResult(challengerId, in.ChallengeId, false, responderId)
		metrics.ObserveChallenge("respond", "challenger_busy")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeExpired, "发起者已进入其它战斗"),
		}, nil
	}
	if locked, err := isPlayerBattleLocked(l.svcCtx, responderId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] Failed to recheck responder's battle lock responder=%d: %v", responderId, err)
		}
		l.pushResult(challengerId, in.ChallengeId, false, responderId)
		metrics.ObserveChallenge("respond", "responder_busy")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeSelfBusy, "战斗尚未结束,无法应战"),
		}, nil
	}

	// 推双方成局通知,再进标准 gather(异步:gather 是多跳 RPC,不占应答 RPC)。
	l.pushResult(challengerId, in.ChallengeId, true, responderId)
	l.pushResult(responderId, in.ChallengeId, true, responderId)

	svcCtx := l.svcCtx
	challengeId := in.ChallengeId
	safego.Go("match.gather.challenge", func() {
		if !RunChallengeGather(svcCtx, uint32(configId), challengerId, responderId) {
			// gather 失败:已冻结者已被解冻(补偿矩阵),这里只能事后周知。
			// 一期用 accepted=false 的结果消息兜底提示双方,产品化提示二期。
			logx.Errorf("[challenge] Failed to start battle after challenge acceptance challenge=%d challenger=%d responder=%d",
				challengeId, challengerId, responderId)
			pushChallengeResult(svcCtx, challengerId, challengeId, false, responderId)
			pushChallengeResult(svcCtx, responderId, challengeId, false, responderId)
		}
	})

	l.Infof("[challenge] Challenge accepted; entering battle start pipeline challenge=%d challenger=%d responder=%d config=%d",
		in.ChallengeId, challengerId, responderId, configId)
	metrics.ObserveChallenge("respond", "accepted")
	return &matchpb.RespondChallengeResponse{}, nil
}

// deleteChallengeRecord 删挑战记录与目标占坑。两条单 key DEL 而不是一条双 key
// DEL:challenge:{id} 与 challenge:target:{pid} 身份不同不能共 hash tag,集群下
// 双 key DEL 会 CROSSSLOT(设计决策 D7)。非原子可接受 —— 目标占坑有 TTL,
// 残留只影响 60s 内对同一目标重复发起。
func deleteChallengeRecord(svcCtx *svc.ServiceContext, challengeId uint64, targetPlayerId uint64) {
	if _, err := svcCtx.MatchRedis.Del(challengeKey(challengeId)); err != nil {
		logx.Errorf("[challenge] Failed to delete challenge record challenge=%d: %v", challengeId, err)
	}
	if _, err := svcCtx.MatchRedis.Del(challengeTargetKey(targetPlayerId)); err != nil {
		logx.Errorf("[challenge] Failed to release target slot challenge=%d target=%d: %v", challengeId, targetPlayerId, err)
	}
}

// pushResult 推挑战结果(尽力而为:推送失败不回滚业务状态,只记日志)。
func (l *ChallengeLogic) pushResult(toPlayerId, challengeId uint64, accepted bool, responderId uint64) {
	pushChallengeResult(l.svcCtx, toPlayerId, challengeId, accepted, responderId)
}

func pushChallengeResult(svcCtx *svc.ServiceContext, toPlayerId, challengeId uint64, accepted bool, responderId uint64) {
	result := &matchpb.ChallengeResultS2C{
		ChallengeId: challengeId,
		Accepted:    accepted,
		ResponderId: responderId,
	}
	if err := pushToPlayer(svcCtx, toPlayerId,
		uint32(game.MatchServiceNotifyChallengeResultMessageId), result); err != nil {
		if errors.Is(err, errPlayerOffline) {
			logx.Infof("[challenge] Result notification target offline; abandoning player=%d challenge=%d", toPlayerId, challengeId)
			return
		}
		logx.Errorf("[challenge] Failed to push challenge result player=%d challenge=%d: %v", toPlayerId, challengeId, err)
	}
}

// NotifyChallengeInvite / NotifyChallengeResult 是 S2C 推送消息借 service 声明
// 拿 message id 的占位 RPC(经 Kafka gate 推送,不会被当作 C2S 调用),
// 服务端实现为空操作。
func (l *ChallengeLogic) NotifyChallengeInvite(in *matchpb.ChallengeInviteS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

func (l *ChallengeLogic) NotifyChallengeResult(in *matchpb.ChallengeResultS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}
