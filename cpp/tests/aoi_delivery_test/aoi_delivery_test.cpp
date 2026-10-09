// 直接编译生产 AOI/Grid/Interest/View，只替换网络发送边界，观察实际接收者和消息体。
#include <gtest/gtest.h>
#include <vector>
#include "spatial/system/aoi.h"
#include "spatial/system/interest.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "combat/buff/comp/buff_comp.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/event/scene_event.pb.h"
#include "proto/scene/player_scene.pb.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "thread_context/ecs_context.h"

namespace {
struct Delivery {
    uint32_t messageId;
    entt::entity receiver;
    std::string body;
};
std::vector<Delivery> deliveries;

int CreatesFor(entt::entity receiver, entt::entity target) {
    int count = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorListCreateMessageId) continue;
        ActorListCreateS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        for (const auto& actor : message.actor_list()) if (actor.entity() == entt::to_integral(target)) ++count;
    }
    return count;
}

int DestroysFor(entt::entity receiver, entt::entity target) {
    int count = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorListDestroyMessageId) continue;
        ActorListDestroyS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        for (auto actor : message.entity()) if (actor == entt::to_integral(target)) ++count;
    }
    return count;
}

class AoiDeliveryTest : public ::testing::Test {
protected:
    entt::entity scene = entt::null;
    void SetUp() override {
        tlsEcs.Clear();
        deliveries.clear();
        tlsEcs.globalRegistry.emplace<ActorListCreateS2C>(tlsEcs.GlobalEntity());
        tlsEcs.globalRegistry.emplace<ActorListDestroyS2C>(tlsEcs.GlobalEntity());
        tlsEcs.globalRegistry.emplace<ActorDestroyS2C>(tlsEcs.GlobalEntity());
        scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneGridListComp>(scene);
    }
    void TearDown() override { tlsEcs.Clear(); deliveries.clear(); }
    entt::entity Spawn(double x = 0, double y = 0) {
        auto entity = tlsEcs.actorRegistry.create();
        auto& transform = tlsEcs.actorRegistry.emplace<Transform>(entity);
        transform.mutable_location()->set_x(x);
        transform.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity, SceneEntityComp{scene});
        tlsEcs.actorRegistry.emplace<Player>(entity);
        tlsEcs.actorRegistry.emplace<Guid>(entity, 10000 + entt::to_integral(entity));
        return entity;
    }
};
}

// 测试只截获既有发送函数；生产 AOI 和可见性/容量/隐身判定未替换。
void SendMessageToClientViaGate(uint32_t id, const google::protobuf::Message& message, entt::entity receiver) {
    deliveries.push_back({id, receiver, message.SerializeAsString()});
}
void BroadcastMessageToPlayers(uint32_t id, const google::protobuf::Message& message, const EntityUnorderedSet& receivers) {
    for (auto receiver : receivers) SendMessageToClientViaGate(id, message, receiver);
}

TEST_F(AoiDeliveryTest, StationaryObserverReceivesNewEntrantWithoutMoving) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    deliveries.clear();
    auto entrant = Spawn();
    AoiSystem::Update(0);
    EXPECT_EQ(CreatesFor(stationary, entrant), 1) << "既有静止玩家必须收到后来者的 ActorCreate";
    EXPECT_EQ(CreatesFor(entrant, stationary), 1);
    EXPECT_EQ(CreatesFor(stationary, stationary), 0);
    deliveries.clear();
    for (int frame = 0; frame < 10; ++frame) AoiSystem::Update(0.1);
    EXPECT_TRUE(deliveries.empty()) << "稳定视野不得每帧重复发送出生消息";
}

TEST_F(AoiDeliveryTest, MovingOutNotifiesBothObservers) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    auto mover = Spawn();
    AoiSystem::Update(0);
    deliveries.clear();
    auto* transform = tlsEcs.actorRegistry.try_get<Transform>(mover);
    ASSERT_NE(transform, nullptr);
    transform->mutable_location()->set_x(1000);
    transform->mutable_location()->set_y(1000);
    AoiSystem::Update(0);
    EXPECT_EQ(DestroysFor(stationary, mover), 1) << "静止观察者也必须清除移出视野的角色";
    EXPECT_EQ(DestroysFor(mover, stationary), 1);
}

TEST_F(AoiDeliveryTest, HiddenEntrantIsNotBroadcastToObserver) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    auto entrant = Spawn();
    tlsEcs.actorRegistry.emplace<StealthedTagComp>(entrant);
    deliveries.clear();
    AoiSystem::Update(0);
    EXPECT_EQ(CreatesFor(stationary, entrant), 0);
    EXPECT_EQ(CreatesFor(entrant, stationary), 1);
}

TEST_F(AoiDeliveryTest, FullObserverDoesNotReceiveRejectedNormalEntrant) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(stationary, AoiClientCapacityComp{kAoiListCapacityMin});
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        auto pinned = tlsEcs.actorRegistry.create();
        ASSERT_TRUE(InterestSystem::PinAoiEntity(stationary, pinned));
    }
    auto entrant = Spawn();
    deliveries.clear();
    AoiSystem::Update(0);
    EXPECT_EQ(CreatesFor(stationary, entrant), 0);
    EXPECT_EQ(CreatesFor(entrant, stationary), 1);
}

TEST_F(AoiDeliveryTest, ReversePinnedEntryIsNotDestroyedOnSpatialLeave) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    auto mover = Spawn();
    AoiSystem::Update(0);
    ASSERT_TRUE(InterestSystem::PinAoiEntity(stationary, mover));
    deliveries.clear();
    auto* transform = tlsEcs.actorRegistry.try_get<Transform>(mover);
    ASSERT_NE(transform, nullptr);
    transform->mutable_location()->set_x(1000);
    transform->mutable_location()->set_y(1000);
    AoiSystem::Update(0);
    EXPECT_EQ(DestroysFor(stationary, mover), 0);
    EXPECT_EQ(DestroysFor(mover, stationary), 1);
}
