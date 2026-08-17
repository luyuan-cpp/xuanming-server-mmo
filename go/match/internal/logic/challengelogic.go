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

	// 咨询性检查双方 battle:lock(权威性复查在应战时刻做)。
	if locked, err := isPlayerBattleLocked(l.svcCtx, challengerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] 查发起者战斗锁失败 player=%d: %v", challengerId, err)
		}
		metrics.ObserveChallenge("invite", "self_busy")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeSelfBusy, "战斗尚未结束,无法发起切磋"),
		}, nil
	}
	if locked, err := isPlayerBattleLocked(l.svcCtx, in.TargetPlayerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] 查目标战斗锁失败 target=%d: %v", in.TargetPlayerId, err)
		}
		metrics.ObserveChallenge("invite", "target_busy")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrChallengeTargetBusy, "对方正在战斗中"),
		}, nil
	}

	// 目标在线校验(挑战弹窗要经 gate 推到目标客户端)。
	targetSession, err := loadPlayerSession(l.svcCtx, in.TargetPlayerId)
	if err != nil {
		l.Errorf("[challenge] 读目标会话失败 target=%d: %v", in.TargetPlayerId, err)
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

	// challenge_id 与 battle_id 同源(match 节点 snowflake)。
	challengeId, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		l.Errorf("[challenge] challenge_id 生成失败 challenger=%d: %v", challengerId, err)
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}

	ttl := l.challengeTTL()
	expiresAtMs := nowMs() + uint64(ttl)*1000

	// 同一目标同时只挂一个待应答挑战:SETNX 占坑,后来者拒绝。
	acquired, err := l.svcCtx.Redis.SetnxEx(challengeTargetKey(in.TargetPlayerId),
		strconv.FormatUint(challengeId, 10), ttl)
	if err != nil {
		l.Errorf("[challenge] 目标占坑失败 target=%d: %v", in.TargetPlayerId, err)
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
		if _, err := l.svcCtx.Redis.Del(challengeKey(challengeId), challengeTargetKey(in.TargetPlayerId)); err != nil {
			l.Errorf("[challenge] 清理挑战记录失败 challenge=%d: %v", challengeId, err)
		}
	}

	recordKey := challengeKey(challengeId)
	if err := l.svcCtx.Redis.Hmset(recordKey, map[string]string{
		challengeFieldChallenger: strconv.FormatUint(challengerId, 10),
		challengeFieldTarget:     strconv.FormatUint(in.TargetPlayerId, 10),
		challengeFieldConfig:     strconv.FormatUint(uint64(in.BattleConfigId), 10),
		challengeFieldExpiresAt:  strconv.FormatUint(expiresAtMs, 10),
	}); err != nil {
		l.Errorf("[challenge] 写挑战记录失败 challenge=%d: %v", challengeId, err)
		cleanup()
		metrics.ObserveChallenge("invite", "internal")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "服务器繁忙,请稍后再试"),
		}, nil
	}
	if err := l.svcCtx.Redis.Expire(recordKey, ttl); err != nil {
		l.Errorf("[challenge] 挑战记录挂 TTL 失败 challenge=%d: %v", challengeId, err)
	}

	// 发起者名字:一期用账号名占位(PlayerSession 无角色昵称,待接玩家昵称数据源)。
	challengerName := ""
	if challengerSession, err := loadPlayerSession(l.svcCtx, challengerId); err == nil && challengerSession != nil {
		challengerName = challengerSession.Account
	}

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
		l.Errorf("[challenge] 推挑战弹窗失败 challenge=%d target=%d: %v", challengeId, in.TargetPlayerId, err)
		cleanup()
		metrics.ObserveChallenge("invite", "push_failed")
		return &matchpb.ChallengePlayerResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "邀请发送失败,请稍后再试"),
		}, nil
	}

	l.Infof("[challenge] 挑战已发起 challenge=%d challenger=%d target=%d config=%d",
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

	fields, err := l.svcCtx.Redis.Hgetall(challengeKey(in.ChallengeId))
	if err != nil {
		l.Errorf("[challenge] 读挑战记录失败 challenge=%d: %v", in.ChallengeId, err)
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

	// 挑战记录一次性消费:应战/拒战/过期都作废(TTL 是兜底,这里显式删)。
	if _, err := l.svcCtx.Redis.Del(challengeKey(in.ChallengeId), challengeTargetKey(targetId)); err != nil {
		l.Errorf("[challenge] 删除挑战记录失败 challenge=%d: %v", in.ChallengeId, err)
	}

	if expiresAtMs > 0 && nowMs() >= expiresAtMs {
		metrics.ObserveChallenge("respond", "expired")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeExpired, "切磋邀请已过期"),
		}, nil
	}

	// 拒战:通知发起者,收场。
	if !in.Accept {
		l.pushResult(challengerId, in.ChallengeId, false, responderId)
		l.Infof("[challenge] 已拒绝 challenge=%d challenger=%d responder=%d",
			in.ChallengeId, challengerId, responderId)
		metrics.ObserveChallenge("respond", "declined")
		return &matchpb.RespondChallengeResponse{}, nil
	}

	// 应战:双方 battle:lock 权威性复查 —— 发起后任何一方可能已经排队开战
	// (先到先得,后到的 gather 拒绝;这里提前挡住明确的冲突)。
	if locked, err := isPlayerBattleLocked(l.svcCtx, challengerId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] 复查发起者战斗锁失败 challenger=%d: %v", challengerId, err)
		}
		l.pushResult(challengerId, in.ChallengeId, false, responderId)
		metrics.ObserveChallenge("respond", "challenger_busy")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrChallengeExpired, "发起者已进入其它战斗"),
		}, nil
	}
	if locked, err := isPlayerBattleLocked(l.svcCtx, responderId); err != nil || locked {
		if err != nil {
			l.Errorf("[challenge] 复查应战者战斗锁失败 responder=%d: %v", responderId, err)
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
			logx.Errorf("[challenge] 应战后开局失败 challenge=%d challenger=%d responder=%d",
				challengeId, challengerId, responderId)
			pushChallengeResult(svcCtx, challengerId, challengeId, false, responderId)
			pushChallengeResult(svcCtx, responderId, challengeId, false, responderId)
		}
	})

	l.Infof("[challenge] 已应战,进入开局管线 challenge=%d challenger=%d responder=%d config=%d",
		in.ChallengeId, challengerId, responderId, configId)
	metrics.ObserveChallenge("respond", "accepted")
	return &matchpb.RespondChallengeResponse{}, nil
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
			logx.Infof("[challenge] 结果通知目标不在线,放弃 player=%d challenge=%d", toPlayerId, challengeId)
			return
		}
		logx.Errorf("[challenge] 推挑战结果失败 player=%d challenge=%d: %v", toPlayerId, challengeId, err)
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
