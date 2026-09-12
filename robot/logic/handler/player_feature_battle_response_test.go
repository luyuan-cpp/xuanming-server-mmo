package handler

import (
	"google.golang.org/protobuf/proto"
	"proto/battle"
	"proto/common/base"
	"proto/match"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"testing"
)

func featureBattlePacket(id uint32, body proto.Message) *base.MessageContent {
	data, err := proto.Marshal(body)
	if err != nil {
		panic(err)
	}
	return &base.MessageContent{MessageId: id, SerializedMessage: data}
}

func TestFeatureBattleEnvelopeErrorsWakeEachDedicatedSource(t *testing.T) {
	p := gameobject.NewPlayer(44)
	for _, id := range []uint32{game.MatchServiceJoinQueueMessageId, game.BattleClientPlayerSetAutoBattleMessageId,
		game.BattleClientPlayerNotifyBattleStartMessageId, game.BattleClientPlayerNotifyTurnResultMessageId,
		game.BattleClientPlayerNotifyBattleEndMessageId} {
		cursor := p.FeatureSequence()
		if !HandleFeatureBattleMessage(p, &base.MessageContent{MessageId: id, ErrorMessage: &base.TipInfoMessage{Id: 29}}) {
			t.Fatal("not consumed")
		}
		snapshot, ok := p.FeatureResponse(id, cursor)
		if !ok || snapshot.TipID != 29 || snapshot.Response != nil || snapshot.MessageID != id {
			t.Fatalf("error lost: %d", id)
		}
	}
}

func TestFeatureBattleQueueAndAutoBodyRejectionsCannotBecomeSuccess(t *testing.T) {
	p := gameobject.NewPlayer(44)
	for _, packet := range []*base.MessageContent{
		featureBattlePacket(game.MatchServiceJoinQueueMessageId, &match.JoinQueueResponse{ErrorCode: 17}),
		featureBattlePacket(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleResponse{ErrorMessage: &base.TipInfoMessage{Id: 18}}),
	} {
		cursor := p.FeatureSequence()
		HandleFeatureBattleMessage(p, packet)
		got, ok := p.FeatureResponse(packet.MessageId, cursor)
		if !ok || got.TipID == 0 || got.Response != nil {
			t.Fatal("rejection hidden")
		}
	}
	HandleFeatureBattleMessage(p, featureBattlePacket(game.MatchServiceJoinQueueMessageId, &match.JoinQueueResponse{QueueTicket: "ticket"}))
	HandleFeatureBattleMessage(p, featureBattlePacket(game.BattleClientPlayerSetAutoBattleMessageId, &battle.SetAutoBattleResponse{}))
	for _, id := range []uint32{game.MatchServiceJoinQueueMessageId, game.BattleClientPlayerSetAutoBattleMessageId} {
		got, ok := p.FeatureResponse(id, 0)
		if !ok || got.TipID != 0 || got.Failure != "" || got.Response == nil {
			t.Fatal("valid response rejected")
		}
	}
}

func TestFeatureBattleRejectsMalformedNotificationsAndWrongSettlementOwner(t *testing.T) {
	p := gameobject.NewPlayer(44)
	p.SignalBattleStart(55)
	for _, packet := range []*base.MessageContent{
		{MessageId: game.BattleClientPlayerNotifyBattleStartMessageId, SerializedMessage: []byte{0xff}},
		featureBattlePacket(game.BattleClientPlayerNotifyTurnResultMessageId, &battle.TurnResultS2C{BattleId: 55}),
		featureBattlePacket(game.BattleClientPlayerNotifyBattleEndMessageId, &battle.BattleEndS2C{BattleId: 55,
			Settlement: &battle.BattleSettlementData{BattleId: 55, PlayerId: 45}}),
	} {
		cursor := p.FeatureSequence()
		HandleFeatureBattleMessage(p, packet)
		got, ok := p.FeatureResponse(packet.MessageId, cursor)
		if !ok || got.Failure == "" || got.Response != nil {
			t.Fatal("invalid notification succeeded")
		}
	}
}

func TestFeatureBattleIgnoresOldEndAndCountsEachRealRoundOnce(t *testing.T) {
	p := gameobject.NewPlayer(44)
	HandleFeatureBattleMessage(p, featureBattlePacket(game.BattleClientPlayerNotifyBattleStartMessageId,
		&battle.BattleStartS2C{BattleId: 55, State: &battle.BattleStateS2C{BattleId: 55}}))
	cursor := p.FeatureSequence()
	HandleFeatureBattleMessage(p, featureBattlePacket(game.BattleClientPlayerNotifyBattleEndMessageId,
		&battle.BattleEndS2C{BattleId: 54, Settlement: &battle.BattleSettlementData{BattleId: 54, PlayerId: 44}}))
	if p.FeatureSequence() != cursor {
		t.Fatal("old login settlement reached current waiter")
	}
	for _, round := range []uint32{1, 1, 2} {
		HandleFeatureBattleMessage(p, featureBattlePacket(game.BattleClientPlayerNotifyTurnResultMessageId,
			&battle.TurnResultS2C{BattleId: 55, RoundIndex: round}))
	}
	if p.GetTurnCount() != 2 {
		t.Fatal("duplicate round counted as real turn")
	}
	if HandleFeatureBattleMessage(p, &base.MessageContent{MessageId: 1}) {
		t.Fatal("unrelated mode intercepted")
	}
}
