package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"proto/battle"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
)

func TestFeatureBattleIsExplicitAndInvalidOptionsStopBeforeConnecting(t *testing.T) {
	if err := validateFeatureBattleOptions(config.FeaturesSmokeConfig{}); err != nil {
		t.Fatal(err)
	}
	valid := config.FeaturesSmokeConfig{Account: "selected", BattleConfigID: 1, AcceptMissionID: 12, ClaimMissionID: 12, VerifyRelogin: true}
	if err := validateFeatureBattleOptions(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*config.FeaturesSmokeConfig){
		func(o *config.FeaturesSmokeConfig) { o.BattleConfigID = 2 },
		func(o *config.FeaturesSmokeConfig) { o.AcceptMissionID = 0 },
		func(o *config.FeaturesSmokeConfig) { o.ClaimMissionID = 13 },
		func(o *config.FeaturesSmokeConfig) { o.Scope = 1 },
		func(o *config.FeaturesSmokeConfig) { o.VerifyRelogin = false },
	} {
		invalid := valid
		mutate(&invalid)
		if err := RunFeaturesSmoke(&config.Config{FeaturesSmoke: invalid}, nil); err == nil {
			t.Fatal("invalid battle options reached connection")
		}
	}
}

func TestFeatureBattleContinuesOnlyActualActiveMission(t *testing.T) {
	active := &scene.PlayerMissionInfo{Status: scene.PlayerMissionStatus_PLAYER_MISSION_ACTIVE}
	if accept, err := featureMissionAcceptDecision(active, true); err != nil || accept {
		t.Fatal("ACTIVE must continue without duplicate accept")
	}
	if _, err := featureMissionAcceptDecision(active, false); err == nil {
		t.Fatal("non-battle mode changed behavior")
	}
	if accept, err := featureMissionAcceptDecision(&scene.PlayerMissionInfo{CanAccept: true}, true); err != nil || !accept {
		t.Fatal("CanAccept rejected")
	}
	if _, err := featureMissionAcceptDecision(&scene.PlayerMissionInfo{Status: scene.PlayerMissionStatus_PLAYER_MISSION_COMPLETED}, true); err == nil {
		t.Fatal("already claimed accepted")
	}
}

func TestFeatureBattleWaiterHonorsFreshErrorSourceAndIgnoresStaleResponse(t *testing.T) {
	p := gameobject.NewPlayer(44)
	endID := uint32(game.BattleClientPlayerNotifyBattleEndMessageId)
	p.SetFeatureSnapshot(endID, &battle.BattleEndS2C{BattleId: 8}, 0, "")
	cursor := p.FeatureSequence()
	p.SetFeatureSnapshot(game.MatchServiceJoinQueueMessageId, nil, 71, "rejected")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := waitFeatureBattleResponse(ctx, p, nil, cursor, endID)
	var server *featureServerError
	if !errors.As(err, &server) || server.MessageID != game.MatchServiceJoinQueueMessageId || server.TipID != 71 {
		t.Fatal("fresh queue error did not wake end waiter")
	}
	cursor = p.FeatureSequence()
	closed := make(chan struct{})
	close(closed)
	if _, err := waitFeatureBattleResponse(ctx, p, closed, cursor, endID); err == nil {
		t.Fatal("stale battle end became success")
	}
}

func TestFeatureBattleVictoryRequiresMatchingAuthoritativeWinAndRealTurns(t *testing.T) {
	end := &battle.BattleEndS2C{BattleId: 55, Outcome: battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		Settlement: &battle.BattleSettlementData{BattleId: 55, PlayerId: 44, Outcome: battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN, TotalRounds: 2}}
	if err := validateFeatureBattleVictory(44, 55, 2, end); err != nil {
		t.Fatal(err)
	}
	if err := validateFeatureBattleVictory(44, 54, 2, end); err == nil {
		t.Fatal("wrong battle accepted")
	}
	if err := validateFeatureBattleVictory(45, 55, 2, end); err == nil {
		t.Fatal("wrong player accepted")
	}
	if err := validateFeatureBattleVictory(44, 55, 0, end); err == nil {
		t.Fatal("zero real turns accepted")
	}
	end.Settlement.Outcome = battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN
	if err := validateFeatureBattleVictory(44, 55, 2, end); err == nil {
		t.Fatal("losing settlement accepted")
	}
}

func TestFeatureBattleWaitsForServerClaimableStateWithoutLocalProgress(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	list, err := waitFeatureMissionClaimable(ctx, 0, 12, func() (*scene.GetMissionListResponse, error) {
		calls++
		return &scene.GetMissionListResponse{Missions: []*scene.PlayerMissionInfo{{MissionId: 12,
			Status: scene.PlayerMissionStatus_PLAYER_MISSION_ACTIVE, CanClaim: calls == 2}}}, nil
	})
	if err != nil || calls != 2 || !list.Missions[0].CanClaim {
		t.Fatal("did not wait for authoritative claimable state")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := waitFeatureMissionClaimable(cancelled, 0, 12, func() (*scene.GetMissionListResponse, error) {
		t.Fatal("cancelled poll invoked read")
		return nil, nil
	}); err == nil {
		t.Fatal("timeout hidden")
	}
}
