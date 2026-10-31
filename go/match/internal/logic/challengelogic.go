package logic

import (
	"context"
	"errors"

	"match/generated/pb/game"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
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
// 发起时刻只做咨询性检查,不冻结任何人;邀请原件在 SQL 提交(global_challenge.go),
// 同一目标同时只挂一个待应答挑战,后来者拒绝;冻结发生在应战之后的备战。
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
	return l.createGlobalChallenge(in, challengerId)
}

// RespondChallenge 应战 / 拒战(设计文档 §3.1b):接受与拒绝在同一 SQL 决策锁中争抢,
// 接受包含全员占用和成局,拒绝写永久决策墓碑(global_challenge.go)。
func (l *ChallengeLogic) RespondChallenge(in *matchpb.RespondChallengeRequest) (*matchpb.RespondChallengeResponse, error) {
	responderId := authoritativePlayerID(l.ctx, in.PlayerId)
	if responderId == 0 {
		metrics.ObserveChallenge("respond", "internal")
		return &matchpb.RespondChallengeResponse{
			ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份"),
		}, nil
	}
	return l.respondGlobalChallenge(in, responderId)
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
