package logic

import (
	"context"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	base "proto/common/base"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
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
//   - 玩家不在任何场景直接拒(ErrNotInScene,决策 D6:没有位置的玩家备战必败,提前拒比进队再失败省);
//   - 入口白名单与人数上限来自 PlayMode 表(模板目录 svcCtx.Catalog);
//   - 票据记 home zone,匹配范围按 zone → 大区 → 全服逐级放宽(global-match-hierarchical-pools.md);
//   - MATCH_MODE_PVE_SOLO 与 PVE_TEAM 的 1 人档入队提交后立即尝试成局;
//   - MATCH_MODE_PVE_TEAM 按人数档凑单(pve-team-size-matching.md):请求的 team_size 就是凑满人数,
//     0 = 该副本上限(battle_config_id 查配置,上限 5 收口,D14),超过上限拒绝;
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

	// 一期只做单人入队,预组队(party_member_ids)走组队的整队开战。
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
		// 人数上限来自 PlayMode 表(模板目录编译时已按 Dungeon.max_team_size 与引擎上限 5 收口);
		// 没有开放的 PVE_TEAM 行即 0(未开放组队)。
		limit := min(l.svcCtx.Catalog.MaxTeamSize(in.Mode, in.BattleConfigId), kMaxBattleTeamSize)
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
		// 每侧人数上限来自 PlayMode 表(不超过引擎每队上限 5);模板没有开放 PVP_TEAM 即 0,一律拒绝。
		if limit := min(l.svcCtx.Catalog.MaxTeamSize(in.Mode, in.BattleConfigId), kMaxBattleTeamSize); sideSize > limit {
			metrics.ObserveJoinQueue(modeName, "team_size_over_limit")
			return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrModeNotOpen,
				ErrorMessage: tipErr(constants.ErrModeNotOpen, "该模板不支持这个每方人数")}, nil
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
	// 入口白名单(PlayMode 表,docs/design/global-match-hierarchical-pools.md §7):(模式, 怪物战斗模板)
	// 不在表里或已关闭一律拒绝,堵住客户端任填 battle_config_id 自建私有池。
	if !l.svcCtx.Catalog.Open(in.Mode, in.BattleConfigId) {
		metrics.ObserveJoinQueue(modeName, "template_not_open")
		return &matchpb.JoinQueueResponse{
			ErrorCode:    constants.ErrModeNotOpen,
			ErrorMessage: tipErr(constants.ErrModeNotOpen, "该副本暂未开放"),
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

	loc, err := loadPlayerLocation(l.svcCtx, playerId)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrNotInScene, ErrorMessage: tipErr(constants.ErrNotInScene, "请先进入场景")}, nil
	}
	stale, err := l.callerSessionStale(playerId)
	if err != nil {
		return nil, err
	}
	if stale {
		return &matchpb.JoinQueueResponse{ErrorCode: constants.ErrNotInScene, ErrorMessage: tipErr(constants.ErrNotInScene, "会话已经失效")}, nil
	}
	// 票据记 home zone(角色归属)而不是当前所在场景的 zone:范围树按 home 逐级放宽,
	// 整队提交按 home 复核,访客在别的 zone 排队也不会因此提交必败。
	return globalJoin(l, in, required)
}

// callerSessionStale 复核发起本次请求的会话仍是该玩家的当前会话
// (docs/design/client-request-ordering.md §6.3 / O13)。返回 true 表示请求来自旧会话或玩家已不在线,应拒绝。
//
// 为什么要这一道:gate 只在同一个会话内保序。玩家断线重连(可能换了 gate)之后,旧会话上还在途的 JoinQueue
// 与新会话上的 CancelQueue 互不排序,旧的 Join 晚落地会让玩家在新会话里「取消成功、实际仍在排队」。
// player_locator 的会话记录随重连换成新的 session_id / gate_instance_id,旧会话的请求据此拒绝。
//
// 不原子:会话在 PlayerRedis,票据在 SQL,读完之后会话仍可能被替换。这一道只是缩小跨会话窗口,
// 剩余的由 SQL 提交点的原身份复核与客户端重连后回源兜底。
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
