package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"proto/battle"
	"proto/common/base"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
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

// 直连上的自动战斗应答与拒绝进大厅同一套 waiter(turn-based §22 D73):callVia 经给定的发送函数发出,
// 直连回来的拒绝变成 featureServerError 而不是超时;直连上未认领的信封错误(如补拉应答)只丢弃,
// 不产生 feature 快照、也不交给通用分发(client 为 nil 仍不 panic)。
func TestFeatureBattleDirectRepliesShareTheLobbyWaiter(t *testing.T) {
	p := gameobject.NewPlayer(44)
	s := &featureSmokeSession{player: p, stats: metrics.NewStats(), done: make(chan struct{})}
	unavailable := uint32(tiptable.CommonError_kServiceUnavailable)

	before := p.FeatureSequence()
	s.onFeatureBattleDirectMessage(nil, &base.MessageContent{MessageId: game.BattleClientPlayerGetBattleStateMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: unavailable}})
	if p.FeatureSequence() != before {
		t.Fatal("unclaimed direct envelope error became a feature snapshot")
	}

	sent := 0
	rejectingSend := func(id uint32, _ proto.Message) error {
		sent++
		s.onFeatureBattleDirectMessage(nil, &base.MessageContent{MessageId: id, ErrorMessage: &base.TipInfoMessage{Id: unavailable}})
		return nil
	}
	_, err := s.callVia(rejectingSend, game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{BattleId: 7, Enabled: true})
	var server *featureServerError
	if sent != 1 || !errors.As(err, &server) || server.MessageID != game.BattleClientPlayerSetAutoBattleMessageId || server.TipID != unavailable {
		t.Fatalf("direct auto-battle rejection not surfaced: sent=%d err=%v", sent, err)
	}

	acceptingSend := func(id uint32, _ proto.Message) error {
		body, err := proto.Marshal(&battle.SetAutoBattleResponse{})
		if err != nil {
			return err
		}
		s.onFeatureBattleDirectMessage(nil, &base.MessageContent{MessageId: id, SerializedMessage: body})
		return nil
	}
	resp, err := s.callVia(acceptingSend, game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{BattleId: 7, Enabled: true})
	if err != nil || resp == nil {
		t.Fatalf("direct auto-battle acceptance lost: resp=%v err=%v", resp, err)
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
