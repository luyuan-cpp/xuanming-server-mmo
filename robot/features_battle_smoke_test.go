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
	"robot/pkg"

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

// 直连上的自动战斗应答与拒绝进大厅同一套 feature 快照(turn-based §22 D73):callDirect 经 callVia 用 CallSyncThen 发出,
// 回包在直连的 RecvLoop 上由 recordFeatureReply 交给 HandleFeatureBattleMessage,返回后 featureCallResult 按发送前的游标
// 读快照。这里不经网络、不依赖墙钟,按 CallSyncThen 的结局形状直接喂回包(CallSyncThen 先调 onReply 再返回,由
// pkg 的 TestCallSyncThen* 覆盖):信封拒绝报包裹 *pkg.ServerTipError 的 error,既不是业务拒绝也不是超时,
// 但仍记进快照供 waiter 看;业务拒绝报 featureServerError;受理返回应答。
func TestFeatureBattleDirectRepliesShareTheLobbyWaiter(t *testing.T) {
	p := gameobject.NewPlayer(44)
	s := &featureSmokeSession{player: p, stats: metrics.NewStats(), done: make(chan struct{})}
	unavailable := uint32(tiptable.CommonError_kServiceUnavailable)
	autoID := uint32(game.BattleClientPlayerSetAutoBattleMessageId)
	var server *featureServerError
	var envelope *pkg.ServerTipError

	cursor := p.FeatureSequence()
	tipErr := &pkg.ServerTipError{Tip: unavailable}
	s.recordFeatureReply(&base.MessageContent{Id: 1, MessageId: autoID, ErrorMessage: &base.TipInfoMessage{Id: unavailable}}, tipErr)
	_, err := s.featureCallResult(autoID, cursor, tipErr)
	if !errors.As(err, &envelope) || envelope.Tip != unavailable || errors.As(err, &server) || errors.Is(err, pkg.ErrRPCTimeout) {
		t.Fatalf("direct auto-battle envelope rejection not surfaced as an envelope error: %v", err)
	}
	if snapshot, ok := p.FeatureResponse(autoID, cursor); !ok || snapshot.TipID != unavailable {
		t.Fatal("direct auto-battle envelope rejection was not recorded for the battle waiter")
	}

	rejected, err := proto.Marshal(&battle.SetAutoBattleResponse{ErrorMessage: &base.TipInfoMessage{Id: 71}})
	if err != nil {
		t.Fatal(err)
	}
	cursor = p.FeatureSequence()
	s.recordFeatureReply(&base.MessageContent{Id: 2, MessageId: autoID, SerializedMessage: rejected}, nil)
	_, err = s.featureCallResult(autoID, cursor, nil)
	if !errors.As(err, &server) || server.MessageID != autoID || server.TipID != 71 || errors.As(err, &envelope) {
		t.Fatalf("direct auto-battle business rejection not surfaced as featureServerError: %v", err)
	}

	accepted, err := proto.Marshal(&battle.SetAutoBattleResponse{})
	if err != nil {
		t.Fatal(err)
	}
	cursor = p.FeatureSequence()
	s.recordFeatureReply(&base.MessageContent{Id: 3, MessageId: autoID, SerializedMessage: accepted}, nil)
	resp, err := s.featureCallResult(autoID, cursor, nil)
	if err != nil || resp == nil {
		t.Fatalf("direct auto-battle acceptance lost: resp=%v err=%v", resp, err)
	}
}

// 直连 RecvLoop 回调(id = 0 的消息):未认领的信封错误(如补拉应答)只丢弃,不产生 feature 快照、也不交给通用分发
// (client 为 nil 仍不 panic);SetAutoBattle 号上的消息只可能是 battle 没回显请求号的回包,丢弃,不写快照 ——
// 否则会盖掉 callDirect 刚认领的那一份。
func TestFeatureBattleDirectCallbackDropsUnclaimedAndUncorrelatedReplies(t *testing.T) {
	p := gameobject.NewPlayer(44)
	s := &featureSmokeSession{player: p, stats: metrics.NewStats(), done: make(chan struct{})}
	unavailable := uint32(tiptable.CommonError_kServiceUnavailable)

	before := p.FeatureSequence()
	s.onFeatureBattleDirectMessage(nil, &base.MessageContent{MessageId: game.BattleClientPlayerGetBattleStateMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: unavailable}})
	if p.FeatureSequence() != before {
		t.Fatal("unclaimed direct envelope error became a feature snapshot")
	}

	accepted, err := proto.Marshal(&battle.SetAutoBattleResponse{})
	if err != nil {
		t.Fatal(err)
	}
	direct := &pkg.GameClient{Account: "robot_t", PlayerId: 44}
	s.onFeatureBattleDirectMessage(direct, &base.MessageContent{MessageId: game.BattleClientPlayerSetAutoBattleMessageId,
		SerializedMessage: accepted})
	s.onFeatureBattleDirectMessage(direct, &base.MessageContent{MessageId: game.BattleClientPlayerSetAutoBattleMessageId,
		ErrorMessage: &base.TipInfoMessage{Id: unavailable}})
	if p.FeatureSequence() != before {
		t.Fatal("auto-battle reply without request id became a feature snapshot")
	}
}

// callDirect 走 Call:直连客户端没有连接时本地不发出,报 ErrRPCNotSent,不等满超时、不写快照。
func TestFeatureBattleCallDirectReportsNotSent(t *testing.T) {
	p := gameobject.NewPlayer(44)
	s := &featureSmokeSession{player: p, stats: metrics.NewStats(), done: make(chan struct{})}
	direct := &battleDirectConn{gc: &pkg.GameClient{Account: "robot_t", PlayerId: 44}, battleId: 7}
	before := p.FeatureSequence()
	_, err := s.callDirect(direct, game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleRequest{BattleId: 7, Enabled: true})
	if !errors.Is(err, pkg.ErrRPCNotSent) {
		t.Fatalf("callDirect on a client without connection returned err=%v, want ErrRPCNotSent", err)
	}
	if p.FeatureSequence() != before {
		t.Fatal("unsent auto-battle request wrote a feature snapshot")
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
