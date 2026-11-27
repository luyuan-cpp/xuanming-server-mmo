package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"match/generated/pb/game"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	chatpb "proto/chat"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	dspb "proto/data_service"
	friendpb "proto/friend"
	guildpb "proto/guild"
	matchpb "proto/match"
	scenepb "proto/scene"
	smpb "proto/scene_manager"
	"shared/generated/table"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const (
	jianghuDirectKind uint32 = 7 // 天墉生死状点名成局意图类型
)

type jianghuRuleConfig struct {
	enabled                  bool
	silverWritCost           uint64
	goldWritItemID           uint32
	inviteTTLMs              uint64
	teamConfirmMs            uint64
	sameTargetCooldownMs     uint64
	dailyUnmatchedLimit      uint32
	duelRoundLimit           uint32
	streakHighlightThreshold uint32
}

// loadJianghuRule 读取 JianghuRule 表 id=1；表未加载时返回与一期配置一致的默认值，表显式设 enabled=0 时 fail-closed 关闭。
func loadJianghuRule() jianghuRuleConfig {
	cfg := jianghuRuleConfig{
		enabled:                  true,
		silverWritCost:           500,
		goldWritItemID:           3415,
		inviteTTLMs:              60_000,
		teamConfirmMs:            30_000,
		sameTargetCooldownMs:     300_000,
		dailyUnmatchedLimit:      20,
		duelRoundLimit:           30,
		streakHighlightThreshold: 3,
	}
	row, ok := table.JianghuRuleTableManagerInstance.FindById(1)
	if !ok || row == nil {
		return cfg
	}
	cfg.enabled = row.GetEnabled() != 0
	if row.GetSilverWritCost() > 0 {
		cfg.silverWritCost = row.GetSilverWritCost()
	}
	if row.GetGoldWritItemId() > 0 {
		cfg.goldWritItemID = row.GetGoldWritItemId()
	}
	if row.GetInviteTtlSec() > 0 {
		cfg.inviteTTLMs = uint64(row.GetInviteTtlSec()) * 1000
	}
	if row.GetTeamConfirmSec() > 0 {
		cfg.teamConfirmMs = uint64(row.GetTeamConfirmSec()) * 1000
	}
	if row.GetSameTargetCooldownSec() > 0 {
		cfg.sameTargetCooldownMs = uint64(row.GetSameTargetCooldownSec()) * 1000
	}
	if row.GetDailyUnmatchedInviteLimit() > 0 {
		cfg.dailyUnmatchedLimit = row.GetDailyUnmatchedInviteLimit()
	}
	if row.GetDuelRoundLimit() > 0 {
		cfg.duelRoundLimit = row.GetDuelRoundLimit()
	}
	if row.GetStreakHighlightThreshold() > 0 {
		cfg.streakHighlightThreshold = row.GetStreakHighlightThreshold()
	}
	return cfg
}

// 包级可替换测试桩，生产走真实 gRPC/Redis/成局管线，单测可无外部依赖注入验证。
var (
	jianghuNowMsFn                 = func() uint64 { return uint64(time.Now().UnixMilli()) }
	checkJianghuEligibilityRPCFn   = checkJianghuEligibilityRPC
	checkJianghuPlayersAvailableFn = checkJianghuPlayersAvailableDefault
	checkJianghuPlayerOnlineFn     = checkJianghuPlayerOnlineDefault
	pushJianghuToPlayerFn          = pushToPlayer
	launchJianghuBattleFn          = launchJianghuDuelFormationDefault
)

func checkJianghuEligibilityRPC(ctx context.Context, endpoint string, req *scenepb.CheckJianghuDuelEligibilityRequest) (*scenepb.CheckJianghuDuelEligibilityResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return smpb.NewSceneNodeGrpcClient(conn).CheckJianghuDuelEligibility(callCtx, req)
}

func checkJianghuPlayerOnlineDefault(svcCtx *svc.ServiceContext, playerID uint64) bool {
	if svcCtx == nil || svcCtx.PlayerRedis == nil || playerID == 0 {
		return false
	}
	session, err := loadPlayerSession(svcCtx, playerID)
	return err == nil && isSessionOnline(session)
}

func checkJianghuPlayersAvailableDefault(svcCtx *svc.ServiceContext, sideA, sideB []uint64) (uint32, string) {
	if svcCtx == nil || svcCtx.PlayerRedis == nil {
		return 0, ""
	}
	for _, pid := range append(append([]uint64(nil), sideA...), sideB...) {
		locked, err := isPlayerBattleLocked(svcCtx, pid)
		if err != nil {
			return constants.ErrJianghuServiceBusy, "战斗状态暂不可用"
		}
		if locked {
			return constants.ErrChallengeTargetBusy, "有参战道友正在其它战斗中"
		}
		session, err := loadPlayerSession(svcCtx, pid)
		if err != nil {
			return constants.ErrJianghuServiceBusy, "在线状态暂不可用"
		}
		if !isSessionOnline(session) {
			return constants.ErrChallengeTargetOffline, "有参战道友当前不在线"
		}
	}
	return 0, ""
}

func jianghuStoreOf(svcCtx *svc.ServiceContext) authority.JianghuStore {
	if svcCtx == nil {
		return nil
	}
	if svcCtx.JianghuStore != nil {
		return svcCtx.JianghuStore
	}
	if svcCtx.Authority != nil {
		return svcCtx.Authority
	}
	return nil
}

// JianghuLogic 实现江湖恩怨与天墉生死状一期客户端 RPC(548..554)。
type JianghuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewJianghuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *JianghuLogic {
	return &JianghuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *JianghuLogic) CreateJianghuDuelInvite(in *matchpb.CreateJianghuDuelInviteRequest) (*matchpb.CreateJianghuDuelInviteResponse, error) {
	rule := loadJianghuRule()
	if !rule.enabled {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuDuelDisabled, "天墉生死状暂未开放")}, nil
	}
	callerID := authoritativePlayerID(l.ctx, 0)
	if callerID == 0 {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	targetID := in.GetTargetPlayerId()
	if targetID == 0 || targetID == callerID {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrChallengeSelf, "不能向自己下达战书")}, nil
	}
	writTier := in.GetWritTier()
	if writTier != matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_SILVER && writTier != matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_GOLD_ITEM {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuInsufficientWritOrSilver, "战书档位无效")}, nil
	}
	store := jianghuStoreOf(l.svcCtx)
	if store == nil {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "生死状服务暂不可用")}, nil
	}

	// 1. 按权威队伍核验双侧成员列表（严禁客户端伪造未组队成员或跨队成员）。
	sideA, errCode, errMsg := l.resolveAuthoritativeSide(store, callerID, in.GetChallengerMemberIds())
	if errCode != 0 {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(errCode, errMsg)}, nil
	}
	sideB, errCode, errMsg := l.resolveAuthoritativeSide(store, targetID, in.GetTargetMemberIds())
	if errCode != 0 {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(errCode, errMsg)}, nil
	}
	if len(sideA) == 0 || len(sideA) > kMaxBattleTeamSize || len(sideA) != len(sideB) {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuTeamSizeMismatch, "双方出战人数须一致且在1至5人内")}, nil
	}
	if hasPlayerOverlap(sideA, sideB) {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuTeamSizeMismatch, "双方队员存在重复，无法立生死状")}, nil
	}

	// 2. 校验全员在线与空闲（未处于战斗中）。
	if code, msg := checkJianghuPlayersAvailableFn(l.svcCtx, sideA, sideB); code != 0 {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(code, msg)}, nil
	}

	// 3. 校验拉黑与好友关系（任意一对单向/双向拉黑拒绝；全体互为好友可绕过闭关免战；缺失 FriendClient 一律 fail-closed）。
	if l.svcCtx == nil || l.svcCtx.FriendClient == nil {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "社交关系校验服务未就绪")}, nil
	}
	fCtx, fCancel := context.WithTimeout(l.ctx, 2*time.Second)
	relResp, relErr := l.svcCtx.FriendClient.BatchCheckSocialRelations(fCtx, &friendpb.BatchCheckSocialRelationsRequest{
		SideAPlayerIds: sideA,
		SideBPlayerIds: sideB,
	})
	fCancel()
	if relErr != nil || relResp.GetErrorMessage().GetId() != 0 {
		l.Errorf("[jianghu] BatchCheckSocialRelations failed caller=%d target=%d err=%v tip=%d", callerID, targetID, relErr, relResp.GetErrorMessage().GetId())
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "社交关系校验暂不可用")}, nil
	}
	if relResp.GetHasAnyBlock() {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuBlockedByPeer, "双方队伍存在拉黑关系，无法下达战书")}, nil
	}
	allAreFriends := relResp.GetAllAreFriends()

	// 4. 校验闭关免战开关与发起方队长资费预检（只读，不扣费）。
	if code, msg := l.checkSceneEligibility(sideA, sideB, callerID, writTier, rule, allAreFriends); code != 0 {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(code, msg)}, nil
	}

	// 5. 生成永久战书 ID、解析权威昵称与纯同帮小队归属。
	inviteID, err := l.nextID()
	if err != nil {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "服务器繁忙，请稍后再试")}, nil
	}
	names := l.resolveDisplayNames(append(append([]uint64(nil), sideA...), sideB...))
	sideAGuildID, sideAGuildName := l.resolvePureGuild(sideA)
	sideBGuildID, sideBGuildName := l.resolvePureGuild(sideB)

	challengerViews := make([]*matchpb.JianghuDuelMemberView, 0, len(sideA))
	for _, pid := range sideA {
		challengerViews = append(challengerViews, &matchpb.JianghuDuelMemberView{
			PlayerId:  pid,
			Name:      names[pid],
			Confirmed: pid == callerID,
		})
	}
	targetViews := make([]*matchpb.JianghuDuelMemberView, 0, len(sideB))
	for _, pid := range sideB {
		targetViews = append(targetViews, &matchpb.JianghuDuelMemberView{
			PlayerId:  pid,
			Name:      names[pid],
			Confirmed: false,
		})
	}
	membersPayload, err := authority.EncodeMembersSnapshot(challengerViews, targetViews)
	if err != nil {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrInternal, "成员快照编码失败")}, nil
	}

	nowMs := jianghuNowMsFn()
	state := uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET)
	expiresMs := nowMs + rule.inviteTTLMs
	confirmDeadlineMs := uint64(0)
	if len(sideA) > 1 {
		state = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_CHALLENGER_TEAM)
		confirmDeadlineMs = nowMs + rule.teamConfirmMs
		expiresMs = confirmDeadlineMs + rule.inviteTTLMs
	}

	challengerAcc, challengerRev := l.ownerIdentity(callerID)
	targetAcc, targetRev := l.ownerIdentity(targetID)
	rec := &matchpb.MatchJianghuDuelInvitationRecord{
		DuelInviteId:        inviteID,
		WritTier:            uint32(writTier),
		TeamSize:            uint32(len(sideA)),
		State:               state,
		ChallengerCaptainId: callerID,
		TargetCaptainId:     targetID,
		ChallengerAccount:   challengerAcc,
		ChallengerRevision:  challengerRev,
		TargetAccount:       targetAcc,
		TargetRevision:      targetRev,
		SideAGuildId:        sideAGuildID,
		SideAGuildName:      sideAGuildName,
		SideBGuildId:        sideBGuildID,
		SideBGuildName:      sideBGuildName,
		MembersPayload:      membersPayload,
		CreatedMs:           nowMs,
		ExpiresMs:           expiresMs,
		ConfirmDeadlineMs:   confirmDeadlineMs,
	}
	saved, err := store.CreateJianghuDuelInvite(l.ctx, rec, rule.sameTargetCooldownMs, rule.dailyUnmatchedLimit, nowMs)
	if err != nil {
		return &matchpb.CreateJianghuDuelInviteResponse{ErrorMessage: mapJianghuStoreError(err)}, nil
	}
	view := buildJianghuInviteView(saved, nowMs)

	// 6. 下行推送：1V1 立即推给目标队长；2-5 人先推给发起方本队队员确认。
	if saved.State == uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET) {
		l.pushInvite(targetID, view)
	} else {
		for _, pid := range sideA {
			if pid != callerID {
				l.pushInvite(pid, view)
			}
		}
	}
	return &matchpb.CreateJianghuDuelInviteResponse{Invite: view}, nil
}

func (l *JianghuLogic) ConfirmJianghuDuelMember(in *matchpb.ConfirmJianghuDuelMemberRequest) (*matchpb.ConfirmJianghuDuelMemberResponse, error) {
	rule := loadJianghuRule()
	if !rule.enabled {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuDuelDisabled, "天墉生死状暂未开放")}, nil
	}
	callerID := authoritativePlayerID(l.ctx, 0)
	if callerID == 0 {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	store := jianghuStoreOf(l.svcCtx)
	if store == nil {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "生死状服务暂不可用")}, nil
	}
	nowMs := jianghuNowMsFn()
	var (
		tipCode          uint32
		tipMsg           string
		pushReason       string
		pushToAll        bool
		pushToTargetCap  bool
		pushSide         []*matchpb.JianghuDuelMemberView
		readyToFinalize  bool
		finalChallengers []*matchpb.JianghuDuelMemberView
		finalTargets     []*matchpb.JianghuDuelMemberView
		finalExpected    uint32
	)
	updated, err := store.MutateJianghuDuelInvite(l.ctx, in.GetDuelInviteId(), func(rec *matchpb.MatchJianghuDuelInvitationRecord, checkTargetBusy func(uint64, uint64) (bool, error)) (bool, error) {
		state := matchpb.JianghuDuelInviteState(rec.State)
		if state != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_CHALLENGER_TEAM &&
			state != matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM {
			tipCode, tipMsg = constants.ErrJianghuInvitePendingOrExpired, "该战书已不在小队确认阶段"
			return false, nil
		}
		challengers, targets, decErr := authority.DecodeMembersSnapshot(rec.MembersPayload)
		if decErr != nil {
			tipCode, tipMsg = constants.ErrInternal, "成员快照损坏"
			return false, nil
		}
		finalChallengers, finalTargets = challengers, targets
		if rec.ConfirmDeadlineMs > 0 && nowMs > rec.ConfirmDeadlineMs {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			tipCode, tipMsg = constants.ErrJianghuInvitePendingOrExpired, "小队确认已超时作废"
			pushToAll, pushReason = true, "expired"
			return true, nil
		}
		activeSide := challengers
		activeCaptain := rec.ChallengerCaptainId
		if state == matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM {
			activeSide = targets
			activeCaptain = rec.TargetCaptainId
		}
		memberIdx := -1
		for i, m := range activeSide {
			if m.GetPlayerId() == callerID {
				memberIdx = i
				break
			}
		}
		if memberIdx < 0 {
			tipCode, tipMsg = constants.ErrJianghuNotInviteParticipant, "您不是当前待确认小队的成员"
			return false, nil
		}
		if !in.GetAgree() {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			pushToAll, pushReason = true, "member_declined"
			return true, nil
		}
		if !l.verifyRosterUnchanged(store, activeCaptain, memberIDsOf(activeSide)) {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			tipCode, tipMsg = constants.ErrJianghuTeamChangedAborted, "小队成员已变动，战书已自动作废"
			pushToAll, pushReason = true, "team_changed"
			return true, nil
		}

		activeSide[memberIdx].Confirmed = true
		allConfirmed := true
		for _, m := range activeSide {
			if !m.GetConfirmed() {
				allConfirmed = false
				break
			}
		}
		payload, encErr := authority.EncodeMembersSnapshot(challengers, targets)
		if encErr != nil {
			tipCode, tipMsg = constants.ErrInternal, "成员快照编码失败"
			return false, nil
		}
		rec.MembersPayload = payload

		if !allConfirmed {
			pushSide, pushReason = activeSide, "member_confirmed"
			return true, nil
		}

		if state == matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_CHALLENGER_TEAM {
			busy, bErr := checkTargetBusy(rec.TargetCaptainId, nowMs)
			if bErr != nil {
				return false, bErr
			}
			if busy {
				rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
				tipCode, tipMsg = constants.ErrJianghuInvitePendingOrExpired, "对方当前已有待处理战书，请稍后再试"
				pushToAll, pushReason = true, "aborted"
				return true, nil
			}
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET)
			rec.ConfirmDeadlineMs = 0
			rec.ExpiresMs = nowMs + rule.inviteTTLMs
			pushToTargetCap = true
			pushSide, pushReason = challengers, "sent_to_target"
			return true, nil
		}

		// AWAITING_TARGET_TEAM 全员确认完毕：先核发起方名册未变，落盘最终确认态后由 finalizeMatchFromInvite 原子抢占 MATCHED 开局。
		if !l.verifyRosterUnchanged(store, rec.ChallengerCaptainId, memberIDsOf(challengers)) {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			tipCode, tipMsg = constants.ErrJianghuTeamChangedAborted, "发起方小队成员已变动，战书已自动作废"
			pushToAll, pushReason = true, "team_changed"
			return true, nil
		}
		readyToFinalize = true
		finalExpected = uint32(state)
		return true, nil
	})
	if err != nil {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: mapJianghuStoreError(err)}, nil
	}
	view := buildJianghuInviteView(updated, nowMs)
	if pushToAll {
		l.pushStatusToAll(finalChallengers, finalTargets, view, pushReason)
	} else {
		if pushToTargetCap {
			l.pushInvite(updated.TargetCaptainId, view)
		}
		if len(pushSide) > 0 {
			l.pushStatusToMembers(pushSide, view, pushReason)
		}
	}
	if tipCode != 0 {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(tipCode, tipMsg), Invite: view}, nil
	}
	if readyToFinalize {
		return l.finalizeMatchFromInvite(store, updated, finalChallengers, finalTargets, finalExpected, rule, nowMs)
	}
	return &matchpb.ConfirmJianghuDuelMemberResponse{Invite: view}, nil
}

func (l *JianghuLogic) RespondJianghuDuelInvite(in *matchpb.RespondJianghuDuelInviteRequest) (*matchpb.RespondJianghuDuelInviteResponse, error) {
	rule := loadJianghuRule()
	if !rule.enabled {
		return &matchpb.RespondJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuDuelDisabled, "天墉生死状暂未开放")}, nil
	}
	callerID := authoritativePlayerID(l.ctx, 0)
	if callerID == 0 {
		return &matchpb.RespondJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	store := jianghuStoreOf(l.svcCtx)
	if store == nil {
		return &matchpb.RespondJianghuDuelInviteResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "生死状服务暂不可用")}, nil
	}
	nowMs := jianghuNowMsFn()
	var (
		tipCode          uint32
		tipMsg           string
		pushReason       string
		pushToAll        bool
		pushTargetTeam   bool
		readyToFinalize  bool
		finalChallengers []*matchpb.JianghuDuelMemberView
		finalTargets     []*matchpb.JianghuDuelMemberView
	)
	updated, err := store.MutateJianghuDuelInvite(l.ctx, in.GetDuelInviteId(), func(rec *matchpb.MatchJianghuDuelInvitationRecord, _ func(uint64, uint64) (bool, error)) (bool, error) {
		if rec.TargetCaptainId != callerID {
			tipCode, tipMsg = constants.ErrJianghuNotInviteParticipant, "该战书不是发给您的"
			return false, nil
		}
		if rec.State != uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET) {
			tipCode, tipMsg = constants.ErrJianghuInvitePendingOrExpired, "该战书已失效或已处理"
			return false, nil
		}
		challengers, targets, decErr := authority.DecodeMembersSnapshot(rec.MembersPayload)
		if decErr != nil {
			tipCode, tipMsg = constants.ErrInternal, "成员快照损坏"
			return false, nil
		}
		finalChallengers, finalTargets = challengers, targets
		if rec.ExpiresMs > 0 && nowMs > rec.ExpiresMs {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			tipCode, tipMsg = constants.ErrJianghuInvitePendingOrExpired, "该战书已过期"
			pushToAll, pushReason = true, "expired"
			return true, nil
		}
		if !in.GetAccept() {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_DECLINED)
			pushToAll, pushReason = true, "declined"
			return true, nil
		}
		if !l.verifyRosterUnchanged(store, rec.ChallengerCaptainId, memberIDsOf(challengers)) ||
			!l.verifyRosterUnchanged(store, rec.TargetCaptainId, memberIDsOf(targets)) {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
			tipCode, tipMsg = constants.ErrJianghuTeamChangedAborted, "小队成员已变动，战书已自动作废"
			pushToAll, pushReason = true, "team_changed"
			return true, nil
		}
		for _, m := range targets {
			if m.GetPlayerId() == callerID {
				m.Confirmed = true
			}
		}
		payload, encErr := authority.EncodeMembersSnapshot(challengers, targets)
		if encErr != nil {
			tipCode, tipMsg = constants.ErrInternal, "成员快照编码失败"
			return false, nil
		}
		rec.MembersPayload = payload
		if rec.TeamSize > 1 {
			rec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_AWAITING_TARGET_TEAM)
			rec.ConfirmDeadlineMs = nowMs + rule.teamConfirmMs
			rec.ExpiresMs = rec.ConfirmDeadlineMs
			pushTargetTeam = true
			return true, nil
		}
		readyToFinalize = true
		return true, nil
	})
	if err != nil {
		return &matchpb.RespondJianghuDuelInviteResponse{ErrorMessage: mapJianghuStoreError(err)}, nil
	}
	view := buildJianghuInviteView(updated, nowMs)
	if pushToAll {
		l.pushStatusToAll(finalChallengers, finalTargets, view, pushReason)
	} else if pushTargetTeam {
		for _, m := range finalTargets {
			if m.GetPlayerId() != callerID {
				l.pushInvite(m.GetPlayerId(), view)
			}
		}
		l.pushStatusToMembers(finalChallengers, view, "awaiting_target_team")
	}
	if tipCode != 0 {
		return &matchpb.RespondJianghuDuelInviteResponse{ErrorMessage: tipErr(tipCode, tipMsg), Invite: view}, nil
	}
	if readyToFinalize {
		confirmResp, fErr := l.finalizeMatchFromInvite(store, updated, finalChallengers, finalTargets,
			uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_PENDING_TARGET), rule, nowMs)
		if fErr != nil {
			return nil, fErr
		}
		return &matchpb.RespondJianghuDuelInviteResponse{
			ErrorMessage: confirmResp.ErrorMessage,
			Invite:       confirmResp.Invite,
		}, nil
	}
	return &matchpb.RespondJianghuDuelInviteResponse{Invite: view}, nil
}

func (l *JianghuLogic) ListJianghuGrudges(in *matchpb.ListJianghuGrudgesRequest) (*matchpb.ListJianghuGrudgesResponse, error) {
	callerID := authoritativePlayerID(l.ctx, 0)
	if callerID == 0 {
		return &matchpb.ListJianghuGrudgesResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	store := jianghuStoreOf(l.svcCtx)
	if store == nil {
		return &matchpb.ListJianghuGrudgesResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "恩怨录暂不可用")}, nil
	}
	_, ownerRev := l.ownerIdentity(callerID)
	entries, err := store.ListJianghuGrudges(l.ctx, callerID, ownerRev)
	if err != nil {
		return &matchpb.ListJianghuGrudgesResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "恩怨录查询失败")}, nil
	}
	for _, entry := range entries {
		// 仅暴露布尔在线状态，绝不暴露和平玩家所在地图或坐标。
		entry.PeerOnline = checkJianghuPlayerOnlineFn(l.svcCtx, entry.GetPeerPlayerId())
	}
	nowMs := jianghuNowMsFn()
	queryIDs := []uint64{callerID}
	if teamID, _, teamMembers, tErr := store.LoadTeamMemberIDs(l.ctx, callerID); tErr == nil && teamID != 0 {
		for _, pid := range teamMembers {
			if pid != 0 && pid != callerID {
				queryIDs = append(queryIDs, pid)
			}
		}
	}
	seenInvites := make(map[uint64]struct{})
	activeViews := make([]*matchpb.JianghuDuelInviteView, 0, 4)
	for _, qid := range queryIDs {
		activeRecs, aErr := store.ListActiveJianghuDuelInvitesForPlayer(l.ctx, qid, nowMs)
		if aErr != nil {
			return &matchpb.ListJianghuGrudgesResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "在途战书查询失败")}, nil
		}
		for _, r := range activeRecs {
			if _, dup := seenInvites[r.DuelInviteId]; dup {
				continue
			}
			view := buildJianghuInviteView(r, nowMs)
			if !inviteContainsPlayer(view, callerID) {
				continue
			}
			seenInvites[r.DuelInviteId] = struct{}{}
			activeViews = append(activeViews, view)
		}
	}
	return &matchpb.ListJianghuGrudgesResponse{
		Grudges:       entries,
		ActiveInvites: activeViews,
		ServerTimeMs:  nowMs,
	}, nil
}

func (l *JianghuLogic) DismissJianghuGrudge(in *matchpb.DismissJianghuGrudgeRequest) (*matchpb.DismissJianghuGrudgeResponse, error) {
	callerID := authoritativePlayerID(l.ctx, 0)
	if callerID == 0 {
		return &matchpb.DismissJianghuGrudgeResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetPeerPlayerId() == 0 {
		return &matchpb.DismissJianghuGrudgeResponse{ErrorMessage: tipErr(constants.ErrJianghuGrudgeNotFound, "未找到该宿敌记录")}, nil
	}
	store := jianghuStoreOf(l.svcCtx)
	if store == nil {
		return &matchpb.DismissJianghuGrudgeResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "恩怨录暂不可用")}, nil
	}
	_, ownerRev := l.ownerIdentity(callerID)
	found, err := store.DismissJianghuGrudge(l.ctx, callerID, ownerRev, in.GetPeerPlayerId())
	if err != nil {
		return &matchpb.DismissJianghuGrudgeResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "一笔勾销失败")}, nil
	}
	if !found {
		return &matchpb.DismissJianghuGrudgeResponse{ErrorMessage: tipErr(constants.ErrJianghuGrudgeNotFound, "未找到该宿敌记录")}, nil
	}
	return &matchpb.DismissJianghuGrudgeResponse{}, nil
}

func (l *JianghuLogic) NotifyJianghuDuelInvite(in *matchpb.JianghuDuelInviteS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

func (l *JianghuLogic) NotifyJianghuDuelStatus(in *matchpb.JianghuDuelStatusS2C) (*base.Empty, error) {
	return &base.Empty{}, nil
}

func (l *JianghuLogic) finalizeMatchFromInvite(
	store authority.JianghuStore,
	rec *matchpb.MatchJianghuDuelInvitationRecord,
	challengers []*matchpb.JianghuDuelMemberView,
	targets []*matchpb.JianghuDuelMemberView,
	expectedState uint32,
	rule jianghuRuleConfig,
	nowMs uint64,
) (*matchpb.ConfirmJianghuDuelMemberResponse, error) {
	sideA := memberIDsOf(challengers)
	sideB := memberIDsOf(targets)
	if code, msg := checkJianghuPlayersAvailableFn(l.svcCtx, sideA, sideB); code != 0 {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(code, msg)}, nil
	}
	if l.svcCtx == nil || l.svcCtx.FriendClient == nil {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "社交关系校验服务未就绪")}, nil
	}
	fCtx, fCancel := context.WithTimeout(l.ctx, 2*time.Second)
	relResp, relErr := l.svcCtx.FriendClient.BatchCheckSocialRelations(fCtx, &friendpb.BatchCheckSocialRelationsRequest{
		SideAPlayerIds: sideA,
		SideBPlayerIds: sideB,
	})
	fCancel()
	if relErr != nil || relResp.GetErrorMessage().GetId() != 0 {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "社交关系校验暂不可用")}, nil
	}
	if relResp.GetHasAnyBlock() {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuBlockedByPeer, "双方存在拉黑关系，无法成局")}, nil
	}

	// 先在 SQL FOR UPDATE 锁内原子抢占 MATCHED 终态，确认独占成功后再调用 launchJianghuBattleFn，彻底杜绝并发拒战/超时与已发房竞态。
	claimed := false
	claimedRec, mErr := store.MutateJianghuDuelInvite(l.ctx, rec.DuelInviteId, func(cur *matchpb.MatchJianghuDuelInvitationRecord, _ func(uint64, uint64) (bool, error)) (bool, error) {
		if cur.State != expectedState {
			return false, nil
		}
		if cur.ConfirmDeadlineMs > 0 && nowMs > cur.ConfirmDeadlineMs {
			return false, nil
		}
		if cur.ExpiresMs > 0 && nowMs > cur.ExpiresMs {
			return false, nil
		}
		cur.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_MATCHED)
		cur.ConfirmDeadlineMs = 0
		claimed = true
		return true, nil
	})
	if mErr != nil || !claimed {
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuInvitePendingOrExpired, "战书状态已变更")}, nil
	}

	duelCtx := &battlepb.JianghuDuelBattleContext{
		DuelInviteId:          claimedRec.DuelInviteId,
		WritTier:              claimedRec.WritTier,
		ChallengerCaptainId:   claimedRec.ChallengerCaptainId,
		ChallengerCaptainName: memberNameOf(challengers, claimedRec.ChallengerCaptainId),
		TargetCaptainId:       claimedRec.TargetCaptainId,
		TargetCaptainName:     memberNameOf(targets, claimedRec.TargetCaptainId),
		SideAGuildId:          claimedRec.SideAGuildId,
		SideAGuildName:        claimedRec.SideAGuildName,
		SideBGuildId:          claimedRec.SideBGuildId,
		SideBGuildName:        claimedRec.SideBGuildName,
		RoundLimitOverride:    rule.duelRoundLimit,
		TeamSize:              claimedRec.TeamSize,
	}
	battleID, err := launchJianghuBattleFn(l.ctx, l.svcCtx, claimedRec, sideA, sideB, duelCtx)
	if err != nil {
		l.Errorf("[jianghu] Failed to launch jianghu duel formation invite=%d: %v", claimedRec.DuelInviteId, err)
		_ = store.AbortMatchedJianghuDuelInvite(l.ctx, claimedRec.DuelInviteId)
		claimedRec.State = uint32(matchpb.JianghuDuelInviteState_JIANGHU_DUEL_INVITE_STATE_ABORTED)
		view := buildJianghuInviteView(claimedRec, nowMs)
		l.pushStatusToAll(challengers, targets, view, "aborted")
		return &matchpb.ConfirmJianghuDuelMemberResponse{ErrorMessage: tipErr(constants.ErrJianghuServiceBusy, "成局启动失败，请稍后重试"), Invite: view}, nil
	}
	claimedRec.BattleId = battleID
	_ = store.SetJianghuDuelInviteBattleID(l.ctx, claimedRec.DuelInviteId, battleID)
	// 成局不计入“每日未成局 20 次”防骚扰上限。
	_ = store.DecrementDailyUnmatchedOnMatch(l.ctx, claimedRec.ChallengerCaptainId, nowMs)
	view := buildJianghuInviteView(claimedRec, nowMs)
	l.pushStatusToAll(challengers, targets, view, "matched")
	return &matchpb.ConfirmJianghuDuelMemberResponse{Invite: view}, nil
}

func launchJianghuDuelFormationDefault(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	rec *matchpb.MatchJianghuDuelInvitationRecord,
	sideA, sideB []uint64,
	duelCtx *battlepb.JianghuDuelBattleContext,
) (uint64, error) {
	if svcCtx == nil || svcCtx.Authority == nil {
		return 0, errors.New("match authority unavailable")
	}
	members := make([]uint64, 0, len(sideA)+len(sideB))
	memberTeams := make(map[uint64]uint32, len(sideA)+len(sideB))
	for _, pid := range sideA {
		members = append(members, pid)
		memberTeams[pid] = 0
	}
	for _, pid := range sideB {
		members = append(members, pid)
		memberTeams[pid] = 1
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(duelCtx)
	if err != nil {
		return 0, err
	}
	key := &matchpb.MatchDirectIntentRecord{
		Kind:        jianghuDirectKind,
		OwnerId:     rec.ChallengerCaptainId,
		RequestHigh: rec.DuelInviteId,
		RequestLow:  1,
	}
	f, err := reserveGlobalDirect(ctx, svcCtx, key, payload, matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL, 0, members, directOptions{
		memberTeams:        memberTeams,
		jianghuDuelContext: duelCtx,
	})
	if err != nil {
		return 0, err
	}
	if f != nil && f.State < 4 {
		launchGlobalFormation(svcCtx, f)
	}
	return f.GetBattleId(), nil
}

func (l *JianghuLogic) resolveAuthoritativeSide(store authority.JianghuStore, captainID uint64, requested []uint64) ([]uint64, uint32, string) {
	teamID, leaderID, authMembers, err := store.LoadTeamMemberIDs(l.ctx, captainID)
	if err != nil {
		l.Errorf("[jianghu] LoadTeamMemberIDs failed captain=%d: %v", captainID, err)
		return nil, constants.ErrJianghuServiceBusy, "小队信息暂不可用"
	}
	if teamID != 0 && leaderID != 0 && leaderID != captainID {
		return nil, constants.ErrJianghuNotInviteParticipant, "小队生死状仅限队长发起或应战"
	}
	normAuth, ok := authority.NormalizeJianghuTeamIDs(captainID, authMembers)
	if !ok {
		return nil, constants.ErrJianghuTeamSizeMismatch, "小队人数不合法"
	}
	if teamID == 0 {
		// 单人无队状态：拒绝客户端夹带额外队员。
		if len(requested) > 1 || (len(requested) == 1 && requested[0] != captainID) {
			return nil, constants.ErrJianghuTeamSizeMismatch, "仅组队状态下可发起多人战书"
		}
		return []uint64{captainID}, 0, ""
	}
	// 组队状态：若客户端显式传入成员清单，须与权威队伍一致（防过时视图）。
	if len(requested) > 0 {
		normReq, reqOK := authority.NormalizeJianghuTeamIDs(captainID, requested)
		if !reqOK || !samePlayerSet(normAuth, normReq) {
			return nil, constants.ErrJianghuTeamChangedAborted, "小队成员已变动，请刷新后重下战书"
		}
	}
	return normAuth, 0, ""
}

func (l *JianghuLogic) verifyRosterUnchanged(store authority.JianghuStore, captainID uint64, expected []uint64) bool {
	teamID, leaderID, authMembers, err := store.LoadTeamMemberIDs(l.ctx, captainID)
	if err != nil {
		return false
	}
	if len(expected) == 1 {
		return teamID == 0 || (samePlayerSet(expected, authMembers) && (leaderID == 0 || leaderID == captainID))
	}
	return teamID != 0 && (leaderID == 0 || leaderID == captainID) && samePlayerSet(expected, authMembers)
}

func (l *JianghuLogic) checkSceneEligibility(
	sideA, sideB []uint64,
	payerID uint64,
	writTier matchpb.JianghuWritTier,
	rule jianghuRuleConfig,
	allAreFriends bool,
) (uint32, string) {
	if l.svcCtx == nil || l.svcCtx.SceneNodes == nil {
		return 0, ""
	}
	byEndpoint := make(map[string][]uint64)
	sideASet := make(map[uint64]bool, len(sideA))
	for _, pid := range sideA {
		sideASet[pid] = true
	}
	allIDs := append(append([]uint64(nil), sideA...), sideB...)
	for _, pid := range allIDs {
		loc, err := loadPlayerLocation(l.svcCtx, pid)
		if err != nil {
			return constants.ErrJianghuServiceBusy, "场景节点位置暂不可用"
		}
		if loc == nil {
			return constants.ErrChallengeTargetOffline, "有参战道友当前未在场景中"
		}
		ep, err := l.svcCtx.SceneNodes.EndpointOf(loc.ZoneId, loc.NodeId)
		if err != nil || ep == "" {
			return constants.ErrJianghuServiceBusy, "场景节点暂不可达"
		}
		byEndpoint[ep] = append(byEndpoint[ep], pid)
	}

	payerChecked := false
	for ep, pids := range byEndpoint {
		req := &scenepb.CheckJianghuDuelEligibilityRequest{
			PlayerIds: pids,
		}
		for _, pid := range pids {
			if pid == payerID {
				req.PayerPlayerId = payerID
				req.WritTier = uint32(writTier)
				if writTier == matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_GOLD_ITEM {
					req.ItemConfigId = rule.goldWritItemID
				} else {
					req.SilverCost = rule.silverWritCost
				}
				payerChecked = true
				break
			}
		}
		resp, err := checkJianghuEligibilityRPCFn(l.ctx, ep, req)
		if err != nil || resp.GetErrorMessage().GetId() != 0 {
			l.Errorf("[jianghu] CheckJianghuDuelEligibility failed ep=%s err=%v tip=%d", ep, err, resp.GetErrorMessage().GetId())
			return constants.ErrJianghuServiceBusy, "参战资格校验暂不可用"
		}
		if req.PayerPlayerId != 0 && !resp.GetPayerHasSufficientFee() {
			return constants.ErrJianghuInsufficientWritOrSilver, "银两或金帖生死状不足"
		}
		for _, entry := range resp.GetPlayers() {
			if !entry.GetOnlineOnNode() {
				return constants.ErrChallengeTargetOffline, "有参战道友当前不在线"
			}
			if entry.GetDuelRefuseStrangers() && !allAreFriends {
				if sideASet[entry.GetPlayerId()] {
					return constants.ErrJianghuSelfRefusesStrangers, "本方有成员开启了闭关免战，请先关闭或互加好友"
				}
				return constants.ErrJianghuTargetRefusesStrangers, "对方开启了闭关免战，暂不接受陌生人战书"
			}
		}
	}
	if !payerChecked {
		return constants.ErrJianghuServiceBusy, "未能校验发起方资费状态"
	}
	return 0, ""
}

func (l *JianghuLogic) resolveDisplayNames(ids []uint64) map[uint64]string {
	out := make(map[uint64]string, len(ids))
	for _, pid := range ids {
		out[pid] = fmt.Sprintf("道友%d", pid)
	}
	if l.svcCtx == nil || l.svcCtx.DataServiceClient == nil || len(ids) == 0 {
		return out
	}
	qCtx, cancel := context.WithTimeout(l.ctx, time.Second)
	defer cancel()
	resp, err := l.svcCtx.DataServiceClient.BatchGetPlayerName(qCtx, &dspb.BatchGetPlayerNameRequest{PlayerIds: ids})
	if err != nil {
		return out
	}
	for pid, name := range resp.GetNames() {
		if name != "" {
			out[pid] = name
		}
	}
	return out
}

// resolvePureGuild 校验一侧全员是否同属同一个帮会；仅当全员同帮时才返回非 0 guild_id。
func (l *JianghuLogic) resolvePureGuild(members []uint64) (uint64, string) {
	if l.svcCtx == nil || l.svcCtx.GuildClient == nil || len(members) == 0 {
		return 0, ""
	}
	var commonID uint64
	var commonName string
	for _, pid := range members {
		qCtx, cancel := context.WithTimeout(l.ctx, time.Second)
		resp, err := l.svcCtx.GuildClient.GetPlayerGuild(qCtx, &guildpb.GetPlayerGuildRequest{PlayerId: pid})
		cancel()
		if err != nil || resp.GetErrorMessage().GetId() != 0 || resp.GetGuild() == nil || resp.GetGuild().GetGuildId() == 0 {
			return 0, ""
		}
		gid := resp.GetGuild().GetGuildId()
		if commonID == 0 {
			commonID = gid
			commonName = resp.GetGuild().GetName()
		} else if commonID != gid {
			return 0, ""
		}
	}
	return commonID, commonName
}

func (l *JianghuLogic) ownerIdentity(playerID uint64) (string, uint64) {
	if l.svcCtx != nil && l.svcCtx.Authority != nil {
		qCtx, cancel := context.WithTimeout(l.ctx, time.Second)
		p, _, err := l.svcCtx.Authority.Profile(qCtx, playerID)
		cancel()
		if err == nil && p != nil {
			return p.GetOwnerAccount(), p.GetOwnerRevision()
		}
	}
	return "", 0
}

func (l *JianghuLogic) nextID() (uint64, error) {
	if l.svcCtx != nil && l.svcCtx.BattleIDGen != nil {
		return l.svcCtx.BattleIDGen.Generate()
	}
	return jianghuNowMsFn(), nil
}

func (l *JianghuLogic) pushInvite(toPlayerID uint64, view *matchpb.JianghuDuelInviteView) {
	if err := pushJianghuToPlayerFn(l.ctx, l.svcCtx, toPlayerID,
		uint32(game.MatchServiceNotifyJianghuDuelInviteMessageId),
		&matchpb.JianghuDuelInviteS2C{Invite: view}); err != nil && !errors.Is(err, errPlayerOffline) {
		l.Errorf("[jianghu] Push invite failed player=%d invite=%d: %v", toPlayerID, view.GetDuelInviteId(), err)
	}
}

func (l *JianghuLogic) pushStatusToMembers(members []*matchpb.JianghuDuelMemberView, view *matchpb.JianghuDuelInviteView, reason string) {
	msg := &matchpb.JianghuDuelStatusS2C{Invite: view, ReasonCode: reason}
	for _, m := range members {
		if m.GetPlayerId() == 0 {
			continue
		}
		if err := pushJianghuToPlayerFn(l.ctx, l.svcCtx, m.GetPlayerId(),
			uint32(game.MatchServiceNotifyJianghuDuelStatusMessageId), msg); err != nil && !errors.Is(err, errPlayerOffline) {
			l.Errorf("[jianghu] Push status failed player=%d invite=%d reason=%s: %v", m.GetPlayerId(), view.GetDuelInviteId(), reason, err)
		}
	}
}

func (l *JianghuLogic) pushStatusToAll(challengers, targets []*matchpb.JianghuDuelMemberView, view *matchpb.JianghuDuelInviteView, reason string) {
	l.pushStatusToMembers(challengers, view, reason)
	l.pushStatusToMembers(targets, view, reason)
}

func buildJianghuInviteView(rec *matchpb.MatchJianghuDuelInvitationRecord, nowMs uint64) *matchpb.JianghuDuelInviteView {
	if rec == nil {
		return nil
	}
	challengers, targets, _ := authority.DecodeMembersSnapshot(rec.MembersPayload)
	return &matchpb.JianghuDuelInviteView{
		DuelInviteId:          rec.DuelInviteId,
		WritTier:              matchpb.JianghuWritTier(rec.WritTier),
		TeamSize:              rec.TeamSize,
		State:                 matchpb.JianghuDuelInviteState(rec.State),
		ChallengerCaptainId:   rec.ChallengerCaptainId,
		ChallengerCaptainName: memberNameOf(challengers, rec.ChallengerCaptainId),
		TargetCaptainId:       rec.TargetCaptainId,
		TargetCaptainName:     memberNameOf(targets, rec.TargetCaptainId),
		SideAGuildId:          rec.SideAGuildId,
		SideAGuildName:        rec.SideAGuildName,
		SideBGuildId:          rec.SideBGuildId,
		SideBGuildName:        rec.SideBGuildName,
		ChallengerMembers:     challengers,
		TargetMembers:         targets,
		CreatedAtMs:           rec.CreatedMs,
		ExpiresAtMs:           rec.ExpiresMs,
		ConfirmDeadlineMs:     rec.ConfirmDeadlineMs,
		BattleId:              rec.BattleId,
		ServerTimeMs:          nowMs,
	}
}

func mapJianghuStoreError(err error) *base.TipInfoMessage {
	switch {
	case errors.Is(err, authority.ErrJianghuSameTargetCooldown):
		return tipErr(constants.ErrJianghuSameTargetCooldown, "对同一道友下达战书间隔不足5分钟，请稍后再试")
	case errors.Is(err, authority.ErrJianghuDailyInviteLimitReached):
		return tipErr(constants.ErrJianghuDailyInviteLimitReached, "今日未成局战书邀请已达上限")
	case errors.Is(err, authority.ErrJianghuInvitePending), errors.Is(err, authority.ErrJianghuInviteNotFound):
		return tipErr(constants.ErrJianghuInvitePendingOrExpired, "对方已有待处理战书或原战书已失效")
	default:
		return tipErr(constants.ErrJianghuServiceBusy, "生死状服务繁忙，请稍后再试")
	}
}

func memberIDsOf(views []*matchpb.JianghuDuelMemberView) []uint64 {
	out := make([]uint64, 0, len(views))
	for _, m := range views {
		if m.GetPlayerId() != 0 {
			out = append(out, m.GetPlayerId())
		}
	}
	return out
}

func memberNameOf(views []*matchpb.JianghuDuelMemberView, pid uint64) string {
	for _, m := range views {
		if m.GetPlayerId() == pid && m.GetName() != "" {
			return m.GetName()
		}
	}
	return fmt.Sprintf("道友%d", pid)
}

func samePlayerSet(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]uint64(nil), a...)
	sb := append([]uint64(nil), b...)
	sort.Slice(sa, func(i, j int) bool { return sa[i] < sa[j] })
	sort.Slice(sb, func(i, j int) bool { return sb[i] < sb[j] })
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func hasPlayerOverlap(a, b []uint64) bool {
	set := make(map[uint64]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; ok {
			return true
		}
	}
	return false
}

func inviteContainsPlayer(view *matchpb.JianghuDuelInviteView, playerID uint64) bool {
	if view == nil || playerID == 0 {
		return false
	}
	if view.GetChallengerCaptainId() == playerID || view.GetTargetCaptainId() == playerID {
		return true
	}
	for _, m := range view.GetChallengerMembers() {
		if m.GetPlayerId() == playerID {
			return true
		}
	}
	for _, m := range view.GetTargetMembers() {
		if m.GetPlayerId() == playerID {
			return true
		}
	}
	return false
}

// ---- 开战播报与战果结算（供 gather.go / rating.go 复用）----
// 普通战书银两的退还只有 scene 一条通道(2026-11-26,数据覆写审计 r2 第 4 轮,群仙问道赛设计 V1 第七节「生死状退费」):
// scene 预扣时把待退标记随存档原子落盘,取消备战一律按标记在线退、作废由 scene 按 SQL 终局复核退;match 不发退费邮件,
// 取消也从不要求 scene 跳过退还,两条通道叠加成双退的路径因此不存在。

func publishJianghuWorldNotice(svcCtx *svc.ServiceContext, req *chatpb.PublishSystemWorldNoticeRequest) {
	if svcCtx == nil || svcCtx.ChatClient == nil || req == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := svcCtx.ChatClient.PublishSystemWorldNotice(ctx, req)
	if err != nil || resp.GetErrorMessage().GetId() != 0 {
		logx.Errorf("[jianghu] PublishSystemWorldNotice failed notice_id=%s template=%d tip=%d err=%v",
			req.GetNoticeId(), req.GetTemplateId(), resp.GetErrorMessage().GetId(), err)
	}
}

func publishJianghuDuelStartNotice(svcCtx *svc.ServiceContext, battleID uint64, duelCtx *battlepb.JianghuDuelBattleContext) {
	if duelCtx == nil || battleID == 0 {
		return
	}
	tmpl := chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_SILVER
	if duelCtx.GetWritTier() == uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_GOLD_ITEM) {
		tmpl = chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_GOLD
	}
	publishJianghuWorldNotice(svcCtx, &chatpb.PublishSystemWorldNoticeRequest{
		NoticeId:   fmt.Sprintf("jh_duel:%d:start", battleID),
		TemplateId: tmpl,
		BattleId:   battleID,
		ActorAName: duelCtx.GetChallengerCaptainName(),
		ActorBName: duelCtx.GetTargetCaptainName(),
		TeamSize:   duelCtx.GetTeamSize(),
	})
}

func applyJianghuDuelResult(svcCtx *svc.ServiceContext, event *kafkapb.BattleResultEvent) (string, error) {
	duelCtx := event.GetJianghuDuelContext()
	if duelCtx == nil || event.GetBattleId() == 0 {
		return ratingOutcomeIgnored, nil
	}
	rule := loadJianghuRule()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// 作废局或 0 回合局：不写入恩怨录，普通战书银两由 scene 随收件箱原子退还（不在此处发重复邮件）。
	if event.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_VOID || event.GetTotalRounds() == 0 {
		return ratingOutcomeIgnored, nil
	}

	store := jianghuStoreOf(svcCtx)
	if store == nil {
		return ratingOutcomeError, errors.New("jianghu store unavailable")
	}
	ownerRevs := map[uint64]uint64{
		duelCtx.GetChallengerCaptainId(): 0,
		duelCtx.GetTargetCaptainId():     0,
	}
	if svcCtx != nil && svcCtx.Authority != nil {
		for pid := range ownerRevs {
			if p, _, err := svcCtx.Authority.Profile(ctx, pid); err == nil && p != nil {
				ownerRevs[pid] = p.GetOwnerRevision()
			}
		}
	}

	summary, applied, err := store.ApplyJianghuDuelBattleResult(ctx, event, ownerRevs)
	if err != nil {
		return ratingOutcomeError, err
	}
	if !applied {
		return ratingOutcomeDuplicate, nil
	}

	// 战果系统模板播报 + 宿敌连胜/终结高光播报。
	winnerName, loserName := duelCtx.GetChallengerCaptainName(), duelCtx.GetTargetCaptainName()
	if event.GetOutcome() == battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN {
		winnerName, loserName = duelCtx.GetTargetCaptainName(), duelCtx.GetChallengerCaptainName()
	}
	if summary.IsDraw {
		publishJianghuWorldNotice(svcCtx, &chatpb.PublishSystemWorldNoticeRequest{
			NoticeId:    fmt.Sprintf("jh_duel:%d:end", event.GetBattleId()),
			TemplateId:  chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_DRAW,
			BattleId:    event.GetBattleId(),
			ActorAName:  duelCtx.GetChallengerCaptainName(),
			ActorBName:  duelCtx.GetTargetCaptainName(),
			TotalRounds: event.GetTotalRounds(),
			TeamSize:    duelCtx.GetTeamSize(),
		})
		return ratingOutcomeApplied, nil
	}

	endTmpl := chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_SILVER
	if duelCtx.GetWritTier() == uint32(matchpb.JianghuWritTier_JIANGHU_WRIT_TIER_GOLD_ITEM) {
		endTmpl = chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_GOLD
	}
	publishJianghuWorldNotice(svcCtx, &chatpb.PublishSystemWorldNoticeRequest{
		NoticeId:    fmt.Sprintf("jh_duel:%d:end", event.GetBattleId()),
		TemplateId:  endTmpl,
		BattleId:    event.GetBattleId(),
		ActorAName:  winnerName,
		ActorBName:  loserName,
		TotalRounds: event.GetTotalRounds(),
		TeamSize:    duelCtx.GetTeamSize(),
	})

	if summary.WinnerNewWinStreak >= rule.streakHighlightThreshold {
		publishJianghuWorldNotice(svcCtx, &chatpb.PublishSystemWorldNoticeRequest{
			NoticeId:    fmt.Sprintf("jh_duel:%d:streak", event.GetBattleId()),
			TemplateId:  chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_MILESTONE,
			BattleId:    event.GetBattleId(),
			ActorAName:  winnerName,
			ActorBName:  loserName,
			StreakCount: summary.WinnerNewWinStreak,
			TeamSize:    duelCtx.GetTeamSize(),
		})
	}
	if summary.LoserPrevWinStreak >= rule.streakHighlightThreshold {
		publishJianghuWorldNotice(svcCtx, &chatpb.PublishSystemWorldNoticeRequest{
			NoticeId:    fmt.Sprintf("jh_duel:%d:streak_break", event.GetBattleId()),
			TemplateId:  chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_BROKEN,
			BattleId:    event.GetBattleId(),
			ActorAName:  winnerName,
			ActorBName:  loserName,
			StreakCount: summary.LoserPrevWinStreak,
			TeamSize:    duelCtx.GetTeamSize(),
		})
	}
	return ratingOutcomeApplied, nil
}
