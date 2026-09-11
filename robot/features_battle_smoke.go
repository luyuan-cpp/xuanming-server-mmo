package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"proto/battle"
	"proto/match"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
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
	if _, err := s.call(game.BattleClientPlayerSetAutoBattleMessageId,
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
