package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"proto/battle"
	"proto/common/base"
	"proto/match"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/pkg"
)

// 只开放当前明确验收的 PVE1、任务12链；所有变更选项在连接账号之前校验。
func validateFeatureBattleOptions(options config.FeaturesSmokeConfig) error {
	if options.BattleConfigID == 0 {
		return nil
	}
	if options.BattleConfigID != 1 || options.Scope != 0 || options.AcceptMissionID != 12 ||
		options.ClaimMissionID != 12 || !options.VerifyRelogin {
		return fmt.Errorf("battle_config_id requires PVE1, scope=0, accept_mission_id=12, claim_mission_id=12 and verify_relogin=true")
	}
	return nil
}

func featureMissionAcceptDecision(mission *scene.PlayerMissionInfo, allowActive bool) (bool, error) {
	if mission == nil {
		return false, fmt.Errorf("selected mission is absent")
	}
	if mission.CanAccept {
		return true, nil
	}
	if allowActive && mission.Status == scene.PlayerMissionStatus_PLAYER_MISSION_ACTIVE {
		return false, nil
	}
	return false, fmt.Errorf("selected mission does not advertise CanAccept or allowed ACTIVE continuation")
}

var featureBattleMessageIDs = []uint32{
	game.MatchServiceJoinQueueMessageId,
	game.BattleClientPlayerSetAutoBattleMessageId,
	game.BattleClientPlayerNotifyBattleStartMessageId,
	game.BattleClientPlayerNotifyTurnResultMessageId,
	game.BattleClientPlayerNotifyBattleEndMessageId,
}

// 同时检查本局各消息的错误，不能只等待终局而把排队/auto拒绝隐藏成超时。
func waitFeatureBattleResponse(ctx context.Context, player *gameobject.Player, done <-chan struct{},
	after uint64, wanted uint32) (proto.Message, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, id := range featureBattleMessageIDs {
			if snapshot, ok := player.FeatureResponse(id, after); ok {
				if snapshot.TipID != 0 {
					return nil, &featureServerError{id, snapshot.TipID}
				}
				if snapshot.Failure != "" {
					return nil, fmt.Errorf("feature battle rejected message_id=%d: %s", id, snapshot.Failure)
				}
			}
		}
		if snapshot, ok := player.FeatureResponse(wanted, after); ok && snapshot.Response != nil {
			return snapshot.Response, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, fmt.Errorf("feature battle response timed out/cancelled message_id=%d: %w", wanted, ctx.Err())
		case <-done:
			return nil, fmt.Errorf("feature battle connection closed message_id=%d", wanted)
		}
	}
}

func validateFeatureBattleVictory(playerID, battleID uint64, turns int, end *battle.BattleEndS2C) error {
	if battleID == 0 || end.GetBattleId() != battleID || end.GetSettlement().GetBattleId() != battleID ||
		end.GetSettlement().GetPlayerId() != playerID {
		return fmt.Errorf("feature battle end does not match the started battle and player")
	}
	if end.GetOutcome() != battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN ||
		end.GetSettlement().GetOutcome() != battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN {
		return fmt.Errorf("feature PVE did not produce SIDE_A_WIN")
	}
	if turns < 1 || end.GetSettlement().GetTotalRounds() < 1 {
		return fmt.Errorf("feature battle has no real turn result")
	}
	return nil
}

// 使用当前已有角色的 gate 连接，不复用任何会建角的登录、观战或 GM helper。
func (s *featureSmokeSession) runFeatureBattle(cfg *config.Config) error {
	if s.player.GetBattleId() != 0 {
		return fmt.Errorf("feature role already has a battle; refusing to join another")
	}
	// cursor 取在 JoinQueue 之前:开战 / 终局 waiter 只认它之后的快照。JoinQueue 走大厅的 callVia,
	// 回包经 HandleFeatureBattleMessage 记进快照(waiter 也看它),被拒时在这里就失败。
	cursor := s.player.FeatureSequence()
	if _, err := s.call(game.MatchServiceJoinQueueMessageId, &match.JoinQueueRequest{
		PlayerId: s.gc.PlayerId, Mode: match.MatchMode_MATCH_MODE_PVE_SOLO,
		BattleConfigId: cfg.FeaturesSmoke.BattleConfigID, ZoneId: cfg.ZoneID,
	}); err != nil {
		return err
	}
	startContext, cancelStart := context.WithTimeout(context.Background(), battleSmokeStartTimeout)
	body, err := waitFeatureBattleResponse(startContext, s.player, s.done, cursor, game.BattleClientPlayerNotifyBattleStartMessageId)
	cancelStart()
	if err != nil {
		return err
	}
	battleID := body.(*battle.BattleStartS2C).GetBattleId()
	if battleID == 0 {
		return fmt.Errorf("feature battle started without identity")
	}
	// 直连是唯一战斗通路(turn-based §22 D73):自动战斗只能经直连开,回合结算只从直连来;
	// 终局包可能来自直连,也可能来自 scene 结算后的大厅推送,两边都进同一个 waiter。
	direct, err := s.openFeatureBattleDirect(battleID)
	if err != nil {
		return err
	}
	defer direct.Close()
	if _, err := s.callDirect(direct, game.BattleClientPlayerSetAutoBattleMessageId,
		&battle.SetAutoBattleRequest{BattleId: battleID, Enabled: true}); err != nil {
		return err
	}
	endContext, cancelEnd := context.WithTimeout(context.Background(), battleSmokeEndTimeout)
	body, err = waitFeatureBattleResponse(endContext, s.player, s.done, cursor, game.BattleClientPlayerNotifyBattleEndMessageId)
	cancelEnd()
	if err != nil {
		return err
	}
	if err := validateFeatureBattleVictory(s.gc.PlayerId, battleID, s.player.GetTurnCount(), body.(*battle.BattleEndS2C)); err != nil {
		return err
	}
	fmt.Printf("FEATURES_BATTLE_OK battle=%d config=%d outcome=SIDE_A_WIN turns=%d\n", battleID, cfg.FeaturesSmoke.BattleConfigID, s.player.GetTurnCount())
	return nil
}

// openFeatureBattleDirect 凭本局的 NotifyBattleAssigned 直连 battle 节点(分配先于 BattleStart 经大厅
// 下发,D70,通常此刻早已到达)。本会话的 Player 只打这一场(runFeatureBattle 入口已拒绝第二场),
// 所以可以用一次性的 WaitBattleAssigned,但仍核对 battle_id 与参战角色。
func (s *featureSmokeSession) openFeatureBattleDirect(battleID uint64) (*battleDirectConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), battleDirectConnTimeout)
	assigned, err := s.player.WaitBattleAssigned(ctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("feature battle has no direct-connect assignment: %w", err)
	}
	if assigned.GetBattleId() != battleID || assigned.GetRole() != battle.EBattleTicketRole_BATTLE_TICKET_ROLE_PARTICIPANT {
		return nil, fmt.Errorf("feature battle assignment does not match the started battle as participant")
	}
	direct, err := dialBattleDirect(s.gc.Account, s.gc.PlayerId, assigned, s.stats, s.onFeatureBattleDirectMessage)
	if err != nil {
		return nil, fmt.Errorf("feature battle direct connection failed: %w", err)
	}
	return direct, nil
}

// onFeatureBattleDirectMessage 是 features-smoke 交给 dialBattleDirect 的直连回调,跑在直连的 RecvLoop 上。
// 它收两类消息:直连 RecvLoop 上的推送(id = 0),先交 HandleFeatureBattleMessage(与大厅连接进同一组 waiter),
// 没认领的走直连的通用分发;以及 pullBattleState 转交的就绪补拉 GetBattleState 应答,HandleFeatureBattleMessage
// 不认领,同样走通用分发。
//
// SetAutoBattle 只经 callDirect 发出,回包带请求号、由 GameClient 交给那次调用,不会到这里;同号的消息只可能是
// id = 0,说明 battle 没回显请求号,那次调用将以超时收场。丢弃而不交 HandleFeatureBattleMessage:写进快照会盖掉
// 调用刚认领、还没来得及读的那一份。
func (s *featureSmokeSession) onFeatureBattleDirectMessage(client *pkg.GameClient, message *base.MessageContent) {
	if message.GetMessageId() == game.BattleClientPlayerSetAutoBattleMessageId {
		smokeDropUncorrelatedReply(client.Account, message)
		return
	}
	if handler.HandleFeatureBattleMessage(s.player, message) {
		return
	}
	dispatchBattleDirectDefault(client, message)
}

// callDirect 经战斗直连发一条战斗客户端 RPC,与大厅连接同一套 callVia:回包按请求号认领,写进同一个 Player 的
// feature 快照后再返回。直连面对验票之后的每条请求恰好回一个包(应答,或回显请求号的信封错误,
// battle_client_edge.cpp DispatchVerifiedRequest),所以可以同步等;唯一例外是已被新直连顶替的旧连接,
// battle 不回包、直接关连接,调用以超时收场。
func (s *featureSmokeSession) callDirect(direct *battleDirectConn, id uint32, request proto.Message) (proto.Message, error) {
	return s.callVia(direct.gc, id, request)
}

// BattleEnd 可先于 scene 应用结算；只轮询服务器任务快照，不在机器人本地累加击杀。
func waitFeatureMissionClaimable(ctx context.Context, scope, missionID uint32,
	read func() (*scene.GetMissionListResponse, error)) (*scene.GetMissionListResponse, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mission did not become claimable after battle: %w", err)
		}
		list, err := read()
		if err != nil {
			return nil, err
		}
		mission := featureMission(list, scope, missionID)
		if mission == nil {
			return nil, fmt.Errorf("battle mission missing from authoritative response")
		}
		if mission.CanClaim {
			return list, nil
		}
		if mission.Status != scene.PlayerMissionStatus_PLAYER_MISSION_ACTIVE {
			return nil, fmt.Errorf("battle mission is not ACTIVE or claimable: status=%d", mission.Status)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return nil, fmt.Errorf("mission did not become claimable after battle: %w", ctx.Err())
		}
	}
}
