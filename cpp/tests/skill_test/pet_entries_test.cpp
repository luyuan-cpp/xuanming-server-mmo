#include "gtest/gtest.h"
#include "core/utils/proto/proto_field_checker.h"
#include "network/network_constants.h"
#include "player/system/player_pet.h"
#include "player/system/pet_entry_rules.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "table/code/petentry_table.h"
#include "table/code/pettier_table.h"
#include "table/code/petrule_table.h"
#include "table/code/pet_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributedimension_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include <google/protobuf/unknown_field_set.h>

namespace {
class PetEntriesTest:public testing::Test {
protected:
 entt::entity player=entt::null;static constexpr uint64_t id=888;
 void SetUp()override {
  PetRuleTableManager::Instance().Load();PetEntryTableManager::Instance().Load();PetTierTableManager::Instance().Load();
  PetTableManager::Instance().Load();AttributePoolTableManager::Instance().Load();AttributeDimensionTableManager::Instance().Load();
  player=tlsEcs.actorRegistry.create();tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch=1;auto& c=tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);auto* p=c.add_pets();
  p->set_pet_id(id);p->set_pet_table_id(1);p->set_level(60);p->set_health(40);p->set_mana(80);
  tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(60);
 }
 void TearDown()override{if(tlsEcs.actorRegistry.valid(player))tlsEcs.actorRegistry.destroy(player);}
 PetInstance& Pet(){return *tlsEcs.actorRegistry.get<PlayerPetComp>(player).mutable_pets(0);}
 SavePetEntriesRequest Request(uint32_t points=2){PetEntriesInfo info;EXPECT_EQ(PetSystem::BuildEntries(player,id,info),kSuccess);SavePetEntriesRequest r;
  r.set_pet_id(id);r.set_state_version(info.state_version());r.set_config_version(info.config_version());r.set_request_id("entry-request-0001");(*r.mutable_target())[503]=points;return r;}
 SavePetEntriesResponse Save(const SavePetEntriesRequest& r){SavePetEntriesResponse out;PetSystem::SaveEntries(player,r,out);return out;}
};
TEST_F(PetEntriesTest, AuthoritativeCatalogBudgetAndZeroPointsDoNotAlterSkills) {
 PetEntriesInfo i;ASSERT_EQ(PetSystem::BuildEntries(player,id,i),kSuccess);EXPECT_EQ(i.total_points(),20u);EXPECT_EQ(i.entries_size(),21);
 EXPECT_EQ(i.remaining_points(),20u);EXPECT_TRUE(i.effective());EXPECT_EQ(i.effects().outgoing_heal_bps(),0u);EXPECT_EQ(i.state_version(),1u);
}
TEST_F(PetEntriesTest, WholeTargetCanDecreaseAndRedistributeWithoutChangingFourStatsOrVitals) {
 (*Pet().mutable_allocated())[401]=4;auto r=Request(10);auto first=Save(r);ASSERT_FALSE(first.has_error_message());
 EXPECT_EQ(first.entries().effects().outgoing_heal_bps(),500u);EXPECT_EQ(first.entries().remaining_points(),10u);
 r.set_request_id("entry-request-0002");r.set_state_version(first.entries().state_version());(*r.mutable_target())[503]=1;(*r.mutable_target())[501]=4;
 const auto second=Save(r);ASSERT_FALSE(second.has_error_message());EXPECT_EQ(second.entries().remaining_points(),15u);
 EXPECT_EQ(Pet().allocated().at(401),4u);EXPECT_EQ(Pet().health(),40u);EXPECT_EQ(Pet().mana(),80u);
}
TEST_F(PetEntriesTest, DurableReceiptSurvivesSerializationAndRetryIsExactlyOnce) {
 auto request=Request();const auto first=Save(request);ASSERT_FALSE(first.has_error_message());
 auto bytes=tlsEcs.actorRegistry.get<PlayerPetComp>(player).SerializeAsString();
 tlsEcs.actorRegistry.get<PlayerPetComp>(player).Clear();ASSERT_TRUE(tlsEcs.actorRegistry.get<PlayerPetComp>(player).ParseFromString(bytes));
 const auto replay=Save(request);EXPECT_FALSE(replay.has_error_message());EXPECT_TRUE(replay.replayed());EXPECT_EQ(replay.entries().state_version(),2u);
 (*request.mutable_target())[503]=3;EXPECT_TRUE(Save(request).has_error_message());EXPECT_EQ(Pet().entry_allocated().at(503),2u);
}
TEST_F(PetEntriesTest, ConcurrentOldVersionAndConfigRejectWithoutOverwriting) {
 auto a=Request();auto b=a;b.set_request_id("entry-request-0002");(*b.mutable_target())[503]=3;
 ASSERT_FALSE(Save(a).has_error_message());EXPECT_TRUE(Save(b).has_error_message());
 b.set_state_version(2);b.set_config_version(9999);EXPECT_TRUE(Save(b).has_error_message());EXPECT_EQ(Pet().entry_allocated().at(503),2u);
}
TEST_F(PetEntriesTest, RangeUnknownBudgetAndUnknownPersistedFieldsAreProtected) {
 auto r=Request(21);EXPECT_EQ(Save(r).error_message().id(),kPetDimensionCapExceeded);
 r=Request(20);(*r.mutable_target())[501]=1;EXPECT_EQ(Save(r).error_message().id(),kPetPointsNotEnough);
 r=Request();(*r.mutable_target())[9999]=1;EXPECT_EQ(Save(r).error_message().id(),kPetDimensionNotFound);
 (*Pet().mutable_entry_allocated())[9999]=3;Pet().GetReflection()->MutableUnknownFields(&Pet())->AddVarint(199,42);
 r=Request(18);EXPECT_EQ(Save(r).error_message().id(),kPetPointsNotEnough);
 r=Request(2);ASSERT_FALSE(Save(r).has_error_message());EXPECT_EQ(Pet().entry_allocated().at(9999),3u);
 EXPECT_EQ(Pet().GetReflection()->GetUnknownFields(Pet()).field_count(),1);
}
TEST_F(PetEntriesTest, OwnershipAndBattleFreezeHandoffLocksHaveNoMutation) {
 auto r=Request();r.set_pet_id(id+1);EXPECT_EQ(Save(r).error_message().id(),kPetNotFound);r.set_pet_id(id);
 tlsEcs.actorRegistry.emplace<InBattleComp>(player);EXPECT_EQ(Save(r).error_message().id(),kPetInBattle);tlsEcs.actorRegistry.remove<InBattleComp>(player);
 tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);EXPECT_TRUE(Save(r).has_error_message());tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
 tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);EXPECT_TRUE(Save(r).has_error_message());EXPECT_TRUE(Pet().entry_allocated().empty());
}
TEST_F(PetEntriesTest, PreviewDoesNotWriteAndRejectsStaleState) {
 auto r=Request(4);PreviewPetEntriesRequest p;p.set_pet_id(id);p.set_state_version(r.state_version());p.set_config_version(r.config_version());*p.mutable_target()=r.target();
 PreviewPetEntriesResponse out;PetSystem::PreviewEntries(player,p,out);ASSERT_FALSE(out.has_error_message());EXPECT_EQ(out.entries().effects().outgoing_heal_bps(),200u);
 EXPECT_TRUE(Pet().entry_allocated().empty());EXPECT_TRUE(Pet().entry_last_request_id().empty());p.set_state_version(987);
 PetSystem::PreviewEntries(player,p,out);EXPECT_TRUE(out.has_error_message());EXPECT_TRUE(Pet().entry_allocated().empty());
}
TEST_F(PetEntriesTest, FiveTierIdentityIsIndependentOfQualityAndRetainedOnLoad) {
 Pet().set_cultivation_tier(4);PetInfo info;PetSystem::FillCultivationTier(Pet(),info);EXPECT_EQ(info.cultivation_tier(),4u);EXPECT_EQ(info.cultivation_tier_name(),"神兽");
 PetSystem::InitializeOnLoad(player);EXPECT_EQ(Pet().cultivation_tier(),4u);
 Pet().set_cultivation_tier(0);PetSystem::InitializeOnLoad(player);EXPECT_EQ(Pet().cultivation_tier(),1u);
}

TEST_F(PetEntriesTest, FullCatalogTargetPassesSceneFieldThresholdAndEmptyTargetClearsKnown) {
 PetEntriesInfo catalog;
 ASSERT_EQ(PetSystem::BuildEntries(player, id, catalog), kSuccess);
 ASSERT_EQ(catalog.entries_size(), 21);

 PreviewPetEntriesRequest previewReq;
 previewReq.set_pet_id(id);
 previewReq.set_state_version(catalog.state_version());
 previewReq.set_config_version(catalog.config_version());
 for (const auto& row : catalog.entries()) {
  (*previewReq.mutable_target())[row.id()] = row.id() == 501 ? 2u : 0u;
 }
 std::string details;
 EXPECT_FALSE(ProtoFieldChecker::CheckFieldSizes(previewReq, kProtoFieldCheckerThreshold, details)) << details;

 PreviewPetEntriesResponse previewResp;
 PetSystem::PreviewEntries(player, previewReq, previewResp);
 ASSERT_FALSE(previewResp.has_error_message());
 EXPECT_EQ(previewResp.entries().remaining_points(), 18u);

 auto saveReq = Request(3);
 auto firstSave = Save(saveReq);
 ASSERT_FALSE(firstSave.has_error_message());
 EXPECT_EQ(firstSave.entries().remaining_points(), 17u);

 SavePetEntriesRequest clearReq;
 clearReq.set_pet_id(id);
 clearReq.set_state_version(firstSave.entries().state_version());
 clearReq.set_config_version(firstSave.entries().config_version());
 clearReq.set_request_id("entry-request-clear-0001");
 const auto cleared = Save(clearReq);
 ASSERT_FALSE(cleared.has_error_message());
 EXPECT_EQ(cleared.entries().remaining_points(), 20u);
 EXPECT_TRUE(Pet().entry_allocated().empty());
}
}
