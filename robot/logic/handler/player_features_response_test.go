package handler

import (
	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"testing"
)

func TestFeatureEnvelopeFailureCannotBecomeEmptySuccess(t *testing.T) {
	p := gameobject.NewPlayer(1)
	id := uint32(game.SceneMissionClientPlayerGetMissionListMessageId)
	good, _ := proto.Marshal(&scene.GetMissionListResponse{StatePersistent: true})
	HandleFeatureMessage(p, &base.MessageContent{MessageId: id, SerializedMessage: good})
	cursor := p.FeatureSequence()
	if !HandleFeatureMessage(p, &base.MessageContent{MessageId: id, ErrorMessage: &base.TipInfoMessage{Id: 42}, SerializedMessage: good}) {
		t.Fatal("feature not handled")
	}
	got, ok := p.FeatureResponse(id, cursor)
	if !ok || got.TipID != 42 || got.Response != nil || got.Failure == "" {
		t.Fatal("envelope error became empty success")
	}
}

func TestFeatureMalformedBodyAndMissingBagAreFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		id   uint32
		body []byte
	}{
		{"malformed", game.SceneMissionClientPlayerGetMissionListMessageId, []byte{0xff}},
		{"missing bag", game.SceneBagClientPlayerGetBagMessageId, nil},
		{"missing sorted bag", game.SceneBagClientPlayerSortBagMessageId, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := gameobject.NewPlayer(1)
			HandleFeatureMessage(p, &base.MessageContent{MessageId: test.id, SerializedMessage: test.body})
			got, ok := p.FeatureResponse(test.id, 0)
			if !ok || got.Failure == "" || got.Response != nil {
				t.Fatal("invalid payload counted as success")
			}
		})
	}
}

func TestFeatureBodyRejectionIsFreshFailureAndEmptyListsAreValid(t *testing.T) {
	p := gameobject.NewPlayer(1)
	id := uint32(game.SceneMissionClientPlayerAcceptMissionMessageId)
	body, _ := proto.Marshal(&scene.GetMissionListResponse{ErrorMessage: &base.TipInfoMessage{Id: 8}})
	HandleFeatureMessage(p, &base.MessageContent{MessageId: id, SerializedMessage: body})
	got, ok := p.FeatureResponse(id, 0)
	if !ok || got.TipID != 8 || got.Response != nil {
		t.Fatal("body rejection was not surfaced")
	}
	cursor := p.FeatureSequence()
	HandleFeatureMessage(p, &base.MessageContent{MessageId: game.SceneActivityClientPlayerGetActivityListMessageId})
	empty, ok := p.FeatureResponse(game.SceneActivityClientPlayerGetActivityListMessageId, cursor)
	if !ok || empty.Failure != "" || empty.Response == nil {
		t.Fatal("valid empty list was rejected")
	}
}

func TestFeatureAllRPCsRetainTheirResponseSource(t *testing.T) {
	p := gameobject.NewPlayer(1)
	for id, response := range map[uint32]proto.Message{
		game.SceneBagClientPlayerGetBagMessageId:                 &scene.GetBagResponse{Bag: &scene.BagInfo{Layout: &scene.BagLayoutInfo{Capacity: 4}}},
		game.SceneBagClientPlayerSortBagMessageId:                &scene.SortBagResponse{Bag: &scene.BagInfo{Layout: &scene.BagLayoutInfo{Capacity: 4}}},
		game.SceneActivityClientPlayerGetActivityListMessageId:   &scene.GetActivityListResponse{},
		game.SceneMissionClientPlayerGetMissionListMessageId:     &scene.GetMissionListResponse{},
		game.SceneMissionClientPlayerAcceptMissionMessageId:      &scene.GetMissionListResponse{},
		game.SceneMissionClientPlayerClaimMissionRewardMessageId: &scene.GetMissionListResponse{},
	} {
		cursor := p.FeatureSequence()
		body, _ := proto.Marshal(response)
		if !HandleFeatureMessage(p, &base.MessageContent{MessageId: id, SerializedMessage: body}) {
			t.Fatalf("not handled: %d", id)
		}
		got, ok := p.FeatureResponse(id, cursor)
		if !ok || got.MessageID != id || got.Failure != "" || !proto.Equal(got.Response, response) {
			t.Fatalf("wrong response/source: %d", id)
		}
	}
	before := p.FeatureSequence()
	if HandleFeatureMessage(p, &base.MessageContent{MessageId: 1}) || p.FeatureSequence() != before {
		t.Fatal("changed a non-feature mode/message")
	}
}
