package main

import (
	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/login"
	"proto/scene"
	"robot/config"
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
