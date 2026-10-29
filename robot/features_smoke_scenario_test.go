package main

import (
	"errors"
	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/login"
	"proto/match"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
	tiptable "shared/generated/pb/table"
	"testing"
)

func TestFeatureExistingRoleSelectionNeverInventsRole(t *testing.T) {
	if _, err := featureExistingPlayerID(&login.LoginResponse{}, 0); err == nil {
		t.Fatal("missing role should stop")
	}
	response := &login.LoginResponse{Players: []*login.AccountSimplePlayerWrapper{
		{Player: &base.AccountSimplePlayer{PlayerId: 11}}, {Player: &base.AccountSimplePlayer{PlayerId: 22}},
	}}
	if id, err := featureExistingPlayerID(response, 22); err != nil || id != 22 {
		t.Fatal("selected existing role not preserved")
	}
	if _, err := featureExistingPlayerID(response, 33); err == nil {
		t.Fatal("unknown role should stop")
	}
}

func TestFeaturesSmokeRequiresExplicitAccountBeforeAnyNetwork(t *testing.T) {
	if err := RunFeaturesSmoke(&config.Config{}, nil); err == nil {
		t.Fatal("missing account allowed")
	}
	if err := RunFeaturesSmoke(&config.Config{FeaturesSmoke: config.FeaturesSmokeConfig{Account: "selected", BagType: 3, SortBag: true}}, nil); err == nil {
		t.Fatal("unsupported sorting allowed")
	}
}

func featureTestBag() *scene.BagInfo {
	return &scene.BagInfo{
		Items:  []*scene.BagItemInfo{{ItemId: 1, ConfigId: 101, Count: 3, MaxStack: 99}, {ItemId: 2, ConfigId: 101, Count: 4, MaxStack: 99}},
		Layout: &scene.BagLayoutInfo{Capacity: 28, Slots: []*scene.BagSlotInfo{{Slot: 0, ItemId: 1, Width: 1, Height: 1}, {Slot: 9, ItemId: 2, Width: 1, Height: 1}}},
	}
}

func TestFeatureBagConservationAllowsMergedStacksButRejectsLostOrOrphanedItems(t *testing.T) {
	bag := featureTestBag()
	totals, err := featureBagInventory(bag)
	if err != nil || totals[[3]uint32{101, 0, 99}] != 7 {
		t.Fatal("wrong totals")
	}
	merged := proto.Clone(bag).(*scene.BagInfo)
	merged.Items = merged.Items[:1]
	merged.Items[0].Count = 7
	merged.Layout.Slots = merged.Layout.Slots[:1]
	mergedTotals, err := featureBagInventory(merged)
	if err != nil || mergedTotals[[3]uint32{101, 0, 99}] != 7 {
		t.Fatal("valid merge rejected")
	}
	bag.Layout.Slots[1].ItemId = 999
	if _, err := featureBagInventory(bag); err == nil {
		t.Fatal("orphaned layout accepted")
	}
}

func TestFeatureBagEqualityIgnoresWireOrderButDetectsLayoutAndCountChanges(t *testing.T) {
	a := featureTestBag()
	b := proto.Clone(a).(*scene.BagInfo)
	b.Items[0], b.Items[1] = b.Items[1], b.Items[0]
	b.Layout.Slots[0], b.Layout.Slots[1] = b.Layout.Slots[1], b.Layout.Slots[0]
	if !featureBagsEqual(a, b) {
		t.Fatal("wire order should have no semantics")
	}
	b.Layout.Slots[0].Slot = 10
	if featureBagsEqual(a, b) {
		t.Fatal("changed slot should be detected")
	}
}

func TestFeatureMissionPersistenceComparesScopeAndProgress(t *testing.T) {
	a := &scene.GetMissionListResponse{Missions: []*scene.PlayerMissionInfo{{Scope: 2, MissionId: 15, Status: scene.PlayerMissionStatus_PLAYER_MISSION_ACTIVE,
		Objectives: []*scene.MissionObjectiveInfo{{ObjectiveIndex: 0, Progress: 2, Target: 3}}}}}
	b := proto.Clone(a).(*scene.GetMissionListResponse)
	if !featureMissionProgressEqual(a, b) {
		t.Fatal("matching progress rejected")
	}
	b.Missions[0].Scope = 3
	if featureMissionProgressEqual(a, b) {
		t.Fatal("different scope accepted")
	}
	b.Missions[0].Scope = 2
	b.Missions[0].Objectives[0].Progress = 0
	if featureMissionProgressEqual(a, b) {
		t.Fatal("lost progress accepted")
	}
}

func featureTestSession() (*featureSmokeSession, *gameobject.Player) {
	p := gameobject.NewPlayer(44)
	return &featureSmokeSession{gc: &pkg.GameClient{Account: "robot_t", PlayerId: 44}, player: p,
		stats: metrics.NewStats(), done: make(chan struct{})}, p
}

func featureTestReply(t *testing.T, requestID uint64, messageID uint32, response proto.Message) *base.MessageContent {
	t.Helper()
	body, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return &base.MessageContent{Id: requestID, MessageId: messageID, SerializedMessage: body}
}

// 大厅 callVia 的结局:回包经 recordFeatureReply 交给 HandleFeature* 写快照,featureCallResult 按发送前的游标读。
// 不经网络、不依赖墙钟,按 CallSyncThen 的结局形状直接喂回包(CallSyncThen 先调 onReply 再返回,由 pkg 的
// TestCallSyncThen* 覆盖)。三种拒绝要分得开:重复领奖只认业务拒绝(featureServerError),信封拒绝说明请求没到
// 任务逻辑;响应残缺仍由 HandleFeatureMessage 判失败,场景不复制判据;JoinQueue 归 HandleFeatureBattleMessage。
func TestFeatureLobbyCallSeparatesEnvelopeBusinessAndIncompleteReplies(t *testing.T) {
	s, p := featureTestSession()
	claimID := uint32(game.SceneMissionClientPlayerClaimMissionRewardMessageId)
	var server *featureServerError
	var envelope *pkg.ServerTipError

	unavailable := uint32(tiptable.CommonError_kServiceUnavailable)
	cursor := p.FeatureSequence()
	tipErr := &pkg.ServerTipError{Tip: unavailable}
	s.recordFeatureReply(&base.MessageContent{Id: 1, MessageId: claimID, ErrorMessage: &base.TipInfoMessage{Id: unavailable}}, tipErr)
	_, err := s.featureCallResult(claimID, cursor, tipErr)
	if !errors.As(err, &envelope) || envelope.Tip != unavailable || errors.As(err, &server) {
		t.Fatalf("envelope rejection must not pass as a business rejection: %v", err)
	}

	cursor = p.FeatureSequence()
	s.recordFeatureReply(featureTestReply(t, 2, claimID, &scene.GetMissionListResponse{ErrorMessage: &base.TipInfoMessage{Id: 71}}), nil)
	_, err = s.featureCallResult(claimID, cursor, nil)
	if !errors.As(err, &server) || server.MessageID != claimID || server.TipID != 71 {
		t.Fatalf("business rejection not surfaced as featureServerError: %v", err)
	}

	cursor = p.FeatureSequence()
	s.recordFeatureReply(featureTestReply(t, 3, game.SceneMissionClientPlayerGetMissionListMessageId,
		&scene.GetMissionListResponse{StatePersistent: true}), nil)
	body, err := s.featureCallResult(game.SceneMissionClientPlayerGetMissionListMessageId, cursor, nil)
	if err != nil || !body.(*scene.GetMissionListResponse).StatePersistent {
		t.Fatalf("accepted mission list lost: body=%v err=%v", body, err)
	}

	cursor = p.FeatureSequence()
	s.recordFeatureReply(featureTestReply(t, 4, game.SceneBagClientPlayerGetBagMessageId, &scene.GetBagResponse{}), nil)
	_, err = s.featureCallResult(game.SceneBagClientPlayerGetBagMessageId, cursor, nil)
	if err == nil || errors.As(err, &server) || errors.As(err, &envelope) {
		t.Fatalf("bag reply without layout must fail as an incomplete response: %v", err)
	}

	cursor = p.FeatureSequence()
	s.recordFeatureReply(featureTestReply(t, 5, game.MatchServiceJoinQueueMessageId, &match.JoinQueueResponse{QueueTicket: "ticket"}), nil)
	if _, err := s.featureCallResult(game.MatchServiceJoinQueueMessageId, cursor, nil); err != nil {
		t.Fatalf("accepted JoinQueue on the lobby lost: %v", err)
	}
	cursor = p.FeatureSequence()
	s.recordFeatureReply(featureTestReply(t, 6, game.MatchServiceJoinQueueMessageId, &match.JoinQueueResponse{}), nil)
	if _, err := s.featureCallResult(game.MatchServiceJoinQueueMessageId, cursor, nil); err == nil {
		t.Fatal("JoinQueue without ticket accepted")
	}
}

// 没有回包的结局按原因分开报;回包到了却没有新快照(消息号不归 HandleFeature* 认领)时失败,不拿游标之前的旧快照
// 充当本次结果;没有连接时本地不发出,报 ErrRPCNotSent、不写快照。
func TestFeatureLobbyCallNeverReturnsStaleSnapshotOrHidesNoReply(t *testing.T) {
	s, p := featureTestSession()
	listID := uint32(game.SceneMissionClientPlayerGetMissionListMessageId)
	p.SetFeatureSnapshot(listID, &scene.GetMissionListResponse{}, 0, "")
	cursor := p.FeatureSequence()

	if _, err := s.featureCallResult(listID, cursor, pkg.ErrRPCTimeout); !errors.Is(err, pkg.ErrRPCTimeout) {
		t.Fatalf("timeout hidden: %v", err)
	}
	if _, err := s.featureCallResult(listID, cursor, pkg.ErrRPCDisconnected); !errors.Is(err, pkg.ErrRPCDisconnected) {
		t.Fatalf("disconnect hidden: %v", err)
	}
	if body, err := s.featureCallResult(listID, cursor, nil); err == nil || body != nil {
		t.Fatalf("stale snapshot before the cursor became this call's result: body=%v err=%v", body, err)
	}

	if _, err := s.call(listID, &scene.GetMissionListRequest{}); !errors.Is(err, pkg.ErrRPCNotSent) {
		t.Fatalf("call without connection returned err=%v, want ErrRPCNotSent", err)
	}
	if p.FeatureSequence() != cursor {
		t.Fatal("unsent request wrote a feature snapshot")
	}
}

// 大厅 RecvLoop 回调丢弃的只是本场景经 callVia 发出的请求号(id = 0 说明服务端没回显请求号);
// 战斗公告与重定向是推送,不在其中。
func TestFeatureLobbyCallMessagesAreRequestsNotPushes(t *testing.T) {
	for _, id := range []uint32{
		game.SceneBagClientPlayerGetBagMessageId,
		game.SceneBagClientPlayerSortBagMessageId,
		game.SceneMissionClientPlayerGetMissionListMessageId,
		game.SceneMissionClientPlayerAcceptMissionMessageId,
		game.SceneMissionClientPlayerClaimMissionRewardMessageId,
		game.SceneActivityClientPlayerGetActivityListMessageId,
		game.MatchServiceJoinQueueMessageId,
	} {
		if !featureLobbyCallMessage(id) {
			t.Fatalf("request message_id=%d not treated as a call", id)
		}
	}
	for _, id := range []uint32{
		game.BattleClientPlayerNotifyBattleStartMessageId,
		game.BattleClientPlayerNotifyBattleEndMessageId,
		game.SceneClientPlayerCommonRedirectToGateMessageId,
	} {
		if featureLobbyCallMessage(id) {
			t.Fatalf("push message_id=%d treated as a call", id)
		}
	}
}
