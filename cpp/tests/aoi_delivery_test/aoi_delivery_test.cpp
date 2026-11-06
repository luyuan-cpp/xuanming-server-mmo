// 直接编译生产 AOI/Grid/Interest/View，只替换网络发送边界，观察实际接收者和消息体。
// 单向关注的验收场景见 docs/design/aoi-directed-interest.md「验收场景」。
#include <gtest/gtest.h>
#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/registry/actor_log.h"
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "rpc/service_metadata/player_state_attribute_sync_service_metadata.h"
#include <limits>
#include <vector>
#include "hexagons_grid.h"
#include "spatial/system/aoi.h"
#include "spatial/system/interest.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "combat/buff/comp/buff_comp.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
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

struct BroadcastDelivery {
    uint32_t messageId;
    EntityUnorderedSet receivers;
    std::string body;
};
std::vector<BroadcastDelivery> broadcasts;

int CreatesFor(entt::entity receiver, entt::entity target) {
    int count = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorListCreateMessageId) continue;
        ActorListCreateS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        for (const auto& actor : message.actor_list()) if (actor.entity() == actor_net_id::Of(tlsEcs.actorRegistry, target)) ++count;
    }
    return count;
}

int DestroysFor(entt::entity receiver, entt::entity target) {
    int count = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorListDestroyMessageId) continue;
        ActorListDestroyS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        for (auto actor : message.entity()) if (actor == actor_net_id::Of(tlsEcs.actorRegistry, target)) ++count;
    }
    return count;
}

// 离场路径发的是单实体 ActorDestroyS2C。
int ActorDestroysFor(entt::entity receiver, entt::entity target) {
    int count = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorDestroyMessageId) continue;
        ActorDestroyS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        if (message.entity() == actor_net_id::Of(tlsEcs.actorRegistry, target)) ++count;
    }
    return count;
}

// receiver 最近一次收到的 target 创建消息里的 x 坐标;没收到返回 NaN。
double CreatedX(entt::entity receiver, entt::entity target) {
    double x = std::numeric_limits<double>::quiet_NaN();
    for (const auto& sent : deliveries) {
        if (sent.receiver != receiver || sent.messageId != SceneSceneClientPlayerNotifyActorListCreateMessageId) continue;
        ActorListCreateS2C message;
        EXPECT_TRUE(message.ParseFromString(sent.body));
        for (const auto& actor : message.actor_list()) {
            if (actor.entity() == actor_net_id::Of(tlsEcs.actorRegistry, target)) x = actor.transform().location().x();
        }
    }
    return x;
}

// 移动、属性、技能广播的接收者:反向索引里可见的观察者。
EntityUnorderedSet VisibleWatchersOf(entt::entity target) {
    EntityVector watchers;
    InterestSystem::GetVisibleWatchers(target, watchers);
    return {watchers.begin(), watchers.end()};
}

AoiFollowGrant SkillGrant(uint64_t id, bool revealsStealth = false) {
    return AoiFollowGrant{AoiFollowSource{AoiFollowKind::kSkill, id}, AoiPriority::kPinned, revealsStealth};
}

AoiFollowGrant TeamGrant(uint64_t teamId) {
    return AoiFollowGrant{AoiFollowSource{AoiFollowKind::kTeam, teamId}, AoiPriority::kTeammate, false};
}

class AoiDeliveryTest : public ::testing::Test {
protected:
    SceneEntity scene = entt::null;
    void SetUp() override {
        tlsEcs.Clear();
        tlsIdGeneratorManager.SetNodeId(93);
        actor_net_id::Install(tlsEcs.actorRegistry);
        deliveries.clear();
        broadcasts.clear();
        scene = tlsEcs.sceneRegistry.create();
    }
    void TearDown() override { tlsEcs.Clear(); deliveries.clear(); broadcasts.clear(); }
    entt::entity Spawn(double x = 0, double y = 0) {
        auto entity = tlsEcs.actorRegistry.create();
        EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, entity));
        auto& transform = tlsEcs.actorRegistry.emplace<Transform>(entity);
        transform.mutable_location()->set_x(x);
        transform.mutable_location()->set_y(y);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(entity, SceneEntityComp{scene});
        tlsEcs.actorRegistry.emplace<Player>(entity);
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, 10000 + entt::to_integral(entity));
        return entity;
    }
    static void MoveTo(entt::entity entity, double x, double y) {
        auto& location = *tlsEcs.actorRegistry.get<Transform>(entity).mutable_location();
        location.set_x(x);
        location.set_y(y);
    }
};
}

// 测试只截获既有发送函数；生产 AOI 和可见性/容量/隐身判定未替换。
void SendToPlayerClient(uint32_t id, const google::protobuf::Message& message, entt::entity receiver) {
    deliveries.push_back({id, receiver, message.SerializeAsString()});
}
void BroadcastToPlayerClients(uint32_t id, const google::protobuf::Message& message, const EntityUnorderedSet& receivers) {
    const auto body = message.SerializeAsString();
    broadcasts.push_back({id, receivers, body});
    for (auto receiver : receivers) deliveries.push_back({id, receiver, body});
}
void BroadcastToPlayerClients(uint32_t id, const google::protobuf::Message& message, const EntityVector& receivers) {
    const auto body = message.SerializeAsString();
    broadcasts.push_back({id, EntityUnorderedSet(receivers.begin(), receivers.end()), body});
    for (auto receiver : receivers) deliveries.push_back({id, receiver, body});
}

TEST_F(AoiDeliveryTest, EnterBroadcastSharesOneActorBodyAcrossAcceptedObservers) {
    const auto firstObserver = Spawn();
    const auto secondObserver = Spawn(1, 0);
    const auto fullObserver = Spawn(2, 0);
    AoiSystem::Update(0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(fullObserver, AoiClientCapacityComp{kAoiListCapacityMin});
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        const auto pinned = tlsEcs.actorRegistry.create();
        ASSERT_TRUE(InterestSystem::AddAoiEntity(fullObserver, pinned, AoiPriority::kPinned));
    }
    const auto entrant = Spawn();
    deliveries.clear();
    broadcasts.clear();

    AoiSystem::Update(0);

    ASSERT_EQ(1u, broadcasts.size());
    const auto& sent = broadcasts.front();
    EXPECT_EQ(SceneSceneClientPlayerNotifyActorListCreateMessageId, sent.messageId);
    EXPECT_EQ((EntityUnorderedSet{firstObserver, secondObserver}), sent.receivers);
    ActorListCreateS2C message;
    ASSERT_TRUE(message.ParseFromString(sent.body));
    ASSERT_EQ(1, message.actor_list_size());
    EXPECT_EQ(actor_net_id::Of(tlsEcs.actorRegistry, entrant), message.actor_list(0).entity());
    EXPECT_EQ(1, CreatesFor(firstObserver, entrant));
    EXPECT_EQ(1, CreatesFor(secondObserver, entrant));
    EXPECT_EQ(0, CreatesFor(fullObserver, entrant));
    EXPECT_EQ(1, CreatesFor(entrant, firstObserver));
    EXPECT_EQ(1, CreatesFor(entrant, secondObserver));
    EXPECT_EQ(1, CreatesFor(entrant, fullObserver));

    deliveries.clear();
    broadcasts.clear();
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty());
    EXPECT_TRUE(broadcasts.empty());
}

TEST_F(AoiDeliveryTest, SpatialLeaveBroadcastExcludesFollowersAndUninterestedObservers) {
    const auto firstObserver = Spawn();
    const auto secondObserver = Spawn(1, 0);
    const auto pinnedObserver = Spawn(2, 0);
    const auto uninterestedObserver = Spawn(3, 0);
    const auto mover = Spawn();
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(pinnedObserver, mover, SkillGrant(1)));
    InterestSystem::RemoveAoiEntity(uninterestedObserver, mover);
    deliveries.clear();
    broadcasts.clear();
    auto& transform = tlsEcs.actorRegistry.get<Transform>(mover);
    transform.mutable_location()->set_x(1000);
    transform.mutable_location()->set_y(1000);

    AoiSystem::Update(0);

    ASSERT_EQ(1u, broadcasts.size());
    const auto& sent = broadcasts.front();
    EXPECT_EQ(SceneSceneClientPlayerNotifyActorListDestroyMessageId, sent.messageId);
    EXPECT_EQ((EntityUnorderedSet{firstObserver, secondObserver}), sent.receivers);
    ActorListDestroyS2C message;
    ASSERT_TRUE(message.ParseFromString(sent.body));
    ASSERT_EQ(1, message.entity_size());
    EXPECT_EQ(actor_net_id::Of(tlsEcs.actorRegistry, mover), message.entity(0));
    EXPECT_EQ(1, DestroysFor(firstObserver, mover));
    EXPECT_EQ(1, DestroysFor(secondObserver, mover));
    EXPECT_EQ(0, DestroysFor(pinnedObserver, mover));
    EXPECT_EQ(0, DestroysFor(uninterestedObserver, mover));
    EXPECT_EQ(1, DestroysFor(mover, firstObserver));
    EXPECT_EQ(1, DestroysFor(mover, secondObserver));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(pinnedObserver).Contains(mover));
}

TEST_F(AoiDeliveryTest, VisibilityResetBroadcastsRemovalAndNextUpdateRebuildsObservers) {
    const auto firstObserver = Spawn();
    const auto secondObserver = Spawn(1, 0);
    const auto pinnedObserver = Spawn(2, 0);
    const auto mover = Spawn();
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(pinnedObserver, mover, SkillGrant(1)));
    deliveries.clear();
    broadcasts.clear();

    AoiSystem::ResetEntityVisibility(mover);

    ASSERT_EQ(1u, broadcasts.size());
    const auto& sent = broadcasts.front();
    EXPECT_EQ(SceneSceneClientPlayerNotifyActorListDestroyMessageId, sent.messageId);
    EXPECT_EQ((EntityUnorderedSet{firstObserver, secondObserver}), sent.receivers);
    EXPECT_EQ(1, DestroysFor(firstObserver, mover));
    EXPECT_EQ(1, DestroysFor(secondObserver, mover));
    EXPECT_EQ(0, DestroysFor(pinnedObserver, mover));
    EXPECT_EQ(1, DestroysFor(mover, firstObserver));
    EXPECT_EQ(1, DestroysFor(mover, secondObserver));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<Hex>(mover));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(pinnedObserver).Contains(mover));

    deliveries.clear();
    broadcasts.clear();
    AoiSystem::Update(0);

    ASSERT_EQ(1u, broadcasts.size());
    EXPECT_EQ(SceneSceneClientPlayerNotifyActorListCreateMessageId, broadcasts.front().messageId);
    EXPECT_EQ((EntityUnorderedSet{firstObserver, secondObserver}), broadcasts.front().receivers);
    EXPECT_EQ(1, CreatesFor(firstObserver, mover));
    EXPECT_EQ(1, CreatesFor(secondObserver, mover));
    EXPECT_EQ(0, CreatesFor(pinnedObserver, mover));
    EXPECT_EQ(1, CreatesFor(mover, firstObserver));
    EXPECT_EQ(1, CreatesFor(mover, secondObserver));
}

TEST_F(AoiDeliveryTest, EmptySceneAndLoneActorCreateOnlyOccupiedGridState) {
    AoiSystem::Update(0);
    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(scene));
    const auto loneActor = Spawn();
    AoiSystem::Update(0);
    EXPECT_TRUE(tlsEcs.sceneRegistry.any_of<SceneGridListComp>(scene));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(loneActor));
    EXPECT_TRUE(deliveries.empty());
}

TEST_F(AoiDeliveryTest, DestroyedTargetDoesNotCreateWatchersInterestList) {
    const auto watcher = Spawn();
    const auto target = Spawn();
    tlsEcs.actorRegistry.destroy(target);
    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, target));
    EXPECT_FALSE(InterestSystem::AddAoiEntity(watcher, target, AoiPriority::kPinned));
    EXPECT_FALSE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(watcher));
    EXPECT_TRUE(deliveries.empty());
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
        ASSERT_TRUE(InterestSystem::AddAoiEntity(stationary, pinned, AoiPriority::kPinned));
    }
    auto entrant = Spawn();
    deliveries.clear();
    AoiSystem::Update(0);
    EXPECT_EQ(CreatesFor(stationary, entrant), 0);
    EXPECT_EQ(CreatesFor(entrant, stationary), 1);
}

TEST_F(AoiDeliveryTest, ExplicitFollowIsNotDestroyedOnSpatialLeave) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    auto mover = Spawn();
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(stationary, mover, SkillGrant(1)));
    deliveries.clear();
    auto* transform = tlsEcs.actorRegistry.try_get<Transform>(mover);
    ASSERT_NE(transform, nullptr);
    transform->mutable_location()->set_x(1000);
    transform->mutable_location()->set_y(1000);
    AoiSystem::Update(0);
    EXPECT_EQ(DestroysFor(stationary, mover), 0);
    EXPECT_EQ(DestroysFor(mover, stationary), 1);
}

TEST_F(AoiDeliveryTest, VisibilityAndLeaveNotificationsCarryCorrectBodies) {
    auto stationary = Spawn();
    AoiSystem::Update(0);
    auto mover = Spawn();
    AoiSystem::Update(0); // ActorListCreate
    auto* transform = tlsEcs.actorRegistry.try_get<Transform>(mover);
    ASSERT_NE(transform, nullptr);
    transform->mutable_location()->set_x(1000);
    transform->mutable_location()->set_y(1000);
    AoiSystem::Update(0); // ActorListDestroy
    auto returner = Spawn();
    AoiSystem::Update(0);
    AoiSystem::OnLeaveScene(returner); // ActorDestroy(离场者的移除按单实体消息广播)

    // 前置条件:三条路径确实都发过消息,且消息体正确。
    ASSERT_EQ(CreatesFor(stationary, mover), 1);
    ASSERT_EQ(DestroysFor(stationary, mover), 1);
    int actorDestroyToStationary = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver != stationary || sent.messageId != SceneSceneClientPlayerNotifyActorDestroyMessageId) continue;
        ActorDestroyS2C message;
        ASSERT_TRUE(message.ParseFromString(sent.body));
        EXPECT_EQ(message.entity(), actor_net_id::Of(tlsEcs.actorRegistry, returner));
        ++actorDestroyToStationary;
    }
    ASSERT_EQ(actorDestroyToStationary, 1);
}

TEST_F(AoiDeliveryTest, LeaveCleansRelationsThroughIndexesOnly) {
    const auto target = Spawn(0, 0);
    const auto nearby = Spawn(1, 0);
    const auto bystander = Spawn(500, 500);
    AoiSystem::Update(0);
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(bystander));
    // 直接写正向表、绕过反向索引,代表一条与 target 没有索引关联的条目:
    // 离场若退化成扫描全节点的 AoiListComp,这条会被误删并发出移除。
    tlsEcs.actorRegistry.emplace<AoiListComp>(bystander).entries.emplace(target, AoiEntry{});
    deliveries.clear();

    AoiSystem::OnLeaveScene(target);

    EXPECT_EQ(1, ActorDestroysFor(nearby, target));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(nearby).HasRelation(target));
    EXPECT_EQ(0, ActorDestroysFor(bystander, target));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(bystander).HasRelation(target))
        << "离场只访问正反索引里与它直接关联的关系,不扫全节点";
}

// 验收:A 在邻格外关注 B,先收创建和当前位置,再收 B 的移动;B 不因此收到 A 的移动。
TEST_F(AoiDeliveryTest, ExplicitFollowBeyondGridSendsCreateAndDeliversOneWay) {
    const auto watcher = Spawn(0, 0);
    const auto target = Spawn(500, 500);
    AoiSystem::Update(0);
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(watcher)) << "前置条件:相距很远,没有空间关注";
    deliveries.clear();

    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));

    EXPECT_EQ(1, CreatesFor(watcher, target));
    EXPECT_DOUBLE_EQ(500.0, CreatedX(watcher, target)) << "创建消息带当前位置";
    EXPECT_EQ(0, CreatesFor(target, watcher)) << "单向关注:目标不因此看到观察者";
    EXPECT_EQ((EntityUnorderedSet{watcher}), VisibleWatchersOf(target)) << "目标的移动与属性发给关注者";
    EXPECT_TRUE(VisibleWatchersOf(watcher).empty()) << "观察者的移动不发给目标";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(target));

    deliveries.clear();
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    EXPECT_TRUE(deliveries.empty()) << "同一来源重复登记不重复创建";
    EXPECT_EQ(1u, tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.at(target).follows.size());
}

// 验收:B 进出 A 的邻格、显式来源不变时,A 的客户端持续显示 B,不重复创建或移除。
TEST_F(AoiDeliveryTest, ExplicitFollowKeepsTargetAcrossGridEntryAndExit) {
    const auto watcher = Spawn(0, 0);
    const auto target = Spawn(500, 500);
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    deliveries.clear();

    MoveTo(target, 1, 0);
    AoiSystem::Update(0);
    EXPECT_EQ(0, CreatesFor(watcher, target)) << "已在显示,进入邻格不重复创建";
    EXPECT_EQ(1, CreatesFor(target, watcher)) << "目标一侧的空间关注照常建立";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.at(target).spatial);

    MoveTo(target, 500, 500);
    AoiSystem::Update(0);
    EXPECT_EQ(0, DestroysFor(watcher, target)) << "显式来源仍在,走出邻格不移除";
    EXPECT_EQ(1, DestroysFor(target, watcher));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).Contains(target));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.at(target).spatial);
    EXPECT_EQ((EntityUnorderedSet{watcher}), VisibleWatchersOf(target)) << "范围外继续收移动";
}

// 验收:队伍与技能两个来源同时关注;结束技能不影响队伍来源,最后一个来源撤销时只发一次移除。
TEST_F(AoiDeliveryTest, MultipleSourcesShareOneVisibleStateAndRemoveOnce) {
    const auto watcher = Spawn(0, 0);
    const auto target = Spawn(500, 500);
    AoiSystem::Update(0);
    deliveries.clear();
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, TeamGrant(42)));
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(7)));
    EXPECT_EQ(1, CreatesFor(watcher, target)) << "多个来源只有一份客户端可见状态";
    deliveries.clear();

    AoiSystem::Unfollow(watcher, target, AoiFollowSource{AoiFollowKind::kSkill, 7});
    EXPECT_TRUE(deliveries.empty()) << "结束技能不影响队伍来源";
    EXPECT_EQ((EntityUnorderedSet{watcher}), VisibleWatchersOf(target));

    AoiSystem::Unfollow(watcher, target, AoiFollowSource{AoiFollowKind::kSkill, 7});
    AoiSystem::Unfollow(watcher, target, AoiFollowSource{AoiFollowKind::kTeam, 99});
    EXPECT_TRUE(deliveries.empty()) << "撤销已撤或不存在的来源是无操作";

    AoiSystem::Unfollow(watcher, target, AoiFollowSource{AoiFollowKind::kTeam, 42});
    EXPECT_EQ(1, DestroysFor(watcher, target));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).HasRelation(target));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiWatchersComp>(target).watchers.empty()) << "两张索引同步清掉";
}

// 验收:解除最后一个显式关注时,目标仍在附近且可见就继续显示,否则立即移除。
TEST_F(AoiDeliveryTest, UnfollowKeepsNearbyTargetAndRemovesDistantOne) {
    const auto watcher = Spawn(0, 0);
    const auto nearby = Spawn(1, 0);
    const auto distant = Spawn(500, 500);
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(watcher, nearby, SkillGrant(1)));
    ASSERT_TRUE(AoiSystem::Follow(watcher, distant, SkillGrant(2)));
    deliveries.clear();

    AoiSystem::Unfollow(watcher, nearby, AoiFollowSource{AoiFollowKind::kSkill, 1});
    AoiSystem::Unfollow(watcher, distant, AoiFollowSource{AoiFollowKind::kSkill, 2});

    EXPECT_EQ(0, DestroysFor(watcher, nearby));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).Contains(nearby));
    EXPECT_EQ(1, DestroysFor(watcher, distant));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).HasRelation(distant));
}

// 验收:B 隐身时普通关注暂停投递并移除一次,侦测授权按玩法给出;恢复时用最新状态重新创建。
TEST_F(AoiDeliveryTest, StealthPausesFollowersAndRecreatesWithLatestStateOnReveal) {
    const auto follower = Spawn(0, 0);
    const auto detector = Spawn(-300, 0);
    const auto nearby = Spawn(501, 500);
    const auto target = Spawn(500, 500);
    AoiSystem::Update(0);
    ASSERT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(nearby).Contains(target));
    ASSERT_TRUE(AoiSystem::Follow(follower, target, SkillGrant(1)));
    ASSERT_TRUE(AoiSystem::Follow(detector, target, SkillGrant(2, /*revealsStealth=*/true)));
    deliveries.clear();

    tlsEcs.actorRegistry.emplace<StealthedTagComp>(target);
    AoiSystem::RefreshTargetVisibility(target);

    EXPECT_EQ(1, DestroysFor(follower, target));
    EXPECT_EQ(1, DestroysFor(nearby, target));
    EXPECT_EQ(0, DestroysFor(detector, target)) << "侦测授权由玩法明确给出";
    EXPECT_EQ((EntityUnorderedSet{detector}), VisibleWatchersOf(target)) << "隐身期间只投递给有侦测授权的关注者";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(follower).HasRelation(target)) << "业务来源保留";
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(nearby).HasRelation(target)) << "空间关注随可见性撤掉";

    deliveries.clear();
    MoveTo(target, 505, 500);
    tlsEcs.actorRegistry.remove<StealthedTagComp>(target);
    AoiSystem::RefreshTargetVisibility(target);

    EXPECT_EQ(1, CreatesFor(follower, target));
    EXPECT_DOUBLE_EQ(505.0, CreatedX(follower, target)) << "恢复时用最新状态重新创建";
    EXPECT_EQ(1, CreatesFor(nearby, target)) << "附近实体立即重新建立空间关注,不等跨格";
    EXPECT_EQ(0, CreatesFor(detector, target));
    EXPECT_EQ((EntityUnorderedSet{follower, detector, nearby}), VisibleWatchersOf(target));
}

// 验收:B 在范围外离场且网格组件已清,A 只收到一次移除;双方索引清干净,其他关系不受影响。
TEST_F(AoiDeliveryTest, OutOfRangeLeaveWithoutGridStateNotifiesFollowerOnce) {
    const auto watcher = Spawn(0, 0);
    const auto target = Spawn(500, 500);
    const auto other = Spawn(-500, -500);
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    ASSERT_TRUE(AoiSystem::Follow(watcher, other, SkillGrant(2)));
    ASSERT_TRUE(AoiSystem::Follow(target, other, SkillGrant(3)));
    // 模拟先被摘出格子的销毁路径:离场清理不能依赖 Hex 或格子成员。
    for (auto& [gridId, grid] : tlsEcs.sceneRegistry.get<SceneGridListComp>(scene)) grid.entities.erase(target);
    tlsEcs.actorRegistry.remove<Hex>(target);
    deliveries.clear();

    AoiSystem::OnLeaveScene(target);

    EXPECT_EQ(1, ActorDestroysFor(watcher, target));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).HasRelation(target));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).Contains(other)) << "与离场者无关的关系不受影响";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(target).entries.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiWatchersComp>(target).watchers.empty());
    const auto& otherWatchers = tlsEcs.actorRegistry.get<AoiWatchersComp>(other).watchers;
    EXPECT_FALSE(otherWatchers.contains(target)) << "离场者的出边也从目标的反向索引里摘掉";
    EXPECT_TRUE(otherWatchers.contains(watcher));

    deliveries.clear();
    AoiSystem::OnLeaveScene(target);
    EXPECT_TRUE(deliveries.empty()) << "重复调用安全";
}

// 容量:高权重的显式关注只淘汰本观察者表里的低权重条目并通知本人;低权重的显式关注登记后暂不显示,
// 名额空出后按权重补回。
TEST_F(AoiDeliveryTest, CapacityEvictionIsPerWatcherAndHiddenFollowIsPromotedLater) {
    const auto watcher = Spawn(0, 0);
    const auto neighbor = Spawn(1, 0);
    const auto target = Spawn(500, 500);
    const auto teammate = Spawn(-500, -500);
    AoiSystem::Update(0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(watcher, AoiClientCapacityComp{kAoiListCapacityMin});
    for (uint32_t i = 1; i < kAoiListCapacityMin; ++i) {
        ASSERT_TRUE(InterestSystem::AddAoiEntity(watcher, tlsEcs.actorRegistry.create(), AoiPriority::kPinned));
    }
    ASSERT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(watcher).Size());
    deliveries.clear();

    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    EXPECT_EQ(1, DestroysFor(watcher, neighbor)) << "被淘汰者通知到观察者本人";
    EXPECT_EQ(1, CreatesFor(watcher, target));
    EXPECT_EQ(0, DestroysFor(neighbor, watcher)) << "淘汰按观察者独立判断,不要求互相占位";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(neighbor).Contains(watcher));

    deliveries.clear();
    ASSERT_TRUE(AoiSystem::Follow(watcher, teammate, TeamGrant(42)));
    EXPECT_EQ(0, CreatesFor(watcher, teammate)) << "权重不够淘汰任何条目:登记但暂不显示";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).HasRelation(teammate));
    EXPECT_TRUE(VisibleWatchersOf(teammate).empty());

    AoiSystem::Unfollow(watcher, target, AoiFollowSource{AoiFollowKind::kSkill, 1});
    EXPECT_EQ(1, DestroysFor(watcher, target));
    EXPECT_EQ(1, CreatesFor(watcher, teammate)) << "名额空出后补回暂隐的显式关注";
    EXPECT_EQ((EntityUnorderedSet{watcher}), VisibleWatchersOf(teammate));
}

// 跨场景或无效目标不能建立显式关注,也不得留下组件。
TEST_F(AoiDeliveryTest, FollowRejectsSelfAndOtherSceneTargets) {
    const auto watcher = Spawn(0, 0);
    const auto elsewhere = Spawn(500, 500);
    tlsEcs.actorRegistry.get<SceneEntityComp>(elsewhere).sceneEntity = tlsEcs.sceneRegistry.create();

    EXPECT_FALSE(AoiSystem::Follow(watcher, watcher, SkillGrant(1)));
    EXPECT_FALSE(AoiSystem::Follow(watcher, elsewhere, SkillGrant(1)));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiListComp>(watcher));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiWatchersComp>(elsewhere));
    EXPECT_TRUE(deliveries.empty());
}





TEST_F(AoiDeliveryTest, BaseAttributeSyncCarriesPlayerGuidAndClearsMask) {
    const auto actor = Spawn(17, 23);
    const auto receiver = Spawn(1000, 1000);
    const auto* playerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(actor);
    ASSERT_NE(nullptr, playerGuid);
    const Guid expectedPlayerId = playerGuid->value;
    SetActorBaseAttributesS2CAttrDirtyBit(actor, ActorBaseAttributesS2C::kEntityIdFieldNumber);
    SetActorBaseAttributesS2CAttrDirtyBit(actor, ActorBaseAttributesS2C::kTransformFieldNumber);
    ActorBaseAttributesS2CSyncAttributes(actor, ScenePlayerSyncSyncBaseAttributeMessageId, EntityUnorderedSet{receiver});
    ASSERT_EQ(1u, deliveries.size());
    EXPECT_EQ(receiver, deliveries[0].receiver);
    EXPECT_EQ(ScenePlayerSyncSyncBaseAttributeMessageId, deliveries[0].messageId);
    ActorBaseAttributesS2C message;
    ASSERT_TRUE(message.ParseFromString(deliveries[0].body));
    EXPECT_EQ(expectedPlayerId, message.entity_id());
    EXPECT_EQ(17, message.transform().location().x());
    EXPECT_EQ(23, message.transform().location().y());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
}

TEST_F(AoiDeliveryTest, BaseAttributeSyncCarriesPlayerGuidWhenOnlyTransformIsDirty) {
    const auto actor = Spawn(17, 23);
    const auto receiver = Spawn(1000, 1000);
    const auto* playerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(actor);
    ASSERT_NE(nullptr, playerGuid);
    const Guid expectedPlayerId = playerGuid->value;
    SetActorBaseAttributesS2CAttrDirtyBit(actor, ActorBaseAttributesS2C::kTransformFieldNumber);

    ActorBaseAttributesS2CSyncAttributes(actor, ScenePlayerSyncSyncBaseAttributeMessageId, EntityUnorderedSet{receiver});

    ASSERT_EQ(1u, deliveries.size());
    EXPECT_EQ(receiver, deliveries[0].receiver);
    EXPECT_EQ(ScenePlayerSyncSyncBaseAttributeMessageId, deliveries[0].messageId);
    ActorBaseAttributesS2C message;
    ASSERT_TRUE(message.ParseFromString(deliveries[0].body));
    EXPECT_EQ(expectedPlayerId, message.entity_id());
    EXPECT_TRUE(message.has_transform());
    EXPECT_EQ(17, message.transform().location().x());
    EXPECT_EQ(23, message.transform().location().y());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
}

TEST_F(AoiDeliveryTest, AttributeSyncKeepsDirtyStateWithoutRecipientsThenConsumesMissingFields) {
    const auto actor = tlsEcs.actorRegistry.create();
    const auto receiver = Spawn();
    SetActorBaseAttributesS2CAttrDirtyBit(actor, ActorBaseAttributesS2C::kEntityIdFieldNumber);
    ActorBaseAttributesS2CSyncAttributes(actor, ScenePlayerSyncSyncBaseAttributeMessageId, EntityVector{});
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
    ActorBaseAttributesS2CSyncAttributes(actor, ScenePlayerSyncSyncBaseAttributeMessageId, EntityVector{receiver});
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
    EXPECT_TRUE(deliveries.empty()) << "缺少本轮标脏字段时不发空包,仍须消费脏标记";
}

TEST_F(AoiDeliveryTest, AttributeSyncConsumesAnEmptyDirtyMarker) {
    const auto actor = Spawn();
    const auto receiver = Spawn();
    tlsEcs.actorRegistry.emplace<ActorBaseAttributesS2CDirtyMaskComp>(actor);
    ActorBaseAttributesS2CSyncAttributes(actor, ScenePlayerSyncSyncBaseAttributeMessageId, EntityVector{receiver});
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
    EXPECT_TRUE(deliveries.empty());
}

TEST_F(AoiDeliveryTest, DirtyBitOnDestroyedEntityCreatesNothing) {
    const auto actor = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.destroy(actor);
    SetActorBaseAttributesS2CAttrDirtyBit(actor, ActorBaseAttributesS2C::kEntityIdFieldNumber);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(actor));
    const auto liveActor = tlsEcs.actorRegistry.create();
    SetActorBaseAttributesS2CAttrDirtyBit(liveActor, std::numeric_limits<std::size_t>::max());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<ActorBaseAttributesS2CDirtyMaskComp>(liveActor));
}

TEST_F(AoiDeliveryTest, ActorLogKeepsPlayerIdentityAndFullEntitySeparate) {
    const auto actor = Spawn();
    auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(actor);
    ASSERT_NE(nullptr, guid);
    guid->value = (uint64_t{1} << 63) + 123;
    const Guid playerId = guid->value;
    muduo::LogStream live;
    live << ActorLog(actor);
    EXPECT_EQ("player_id=" + std::to_string(playerId) + " entity=" + std::to_string(entt::to_integral(actor)),
              live.buffer().toString());
    tlsEcs.actorRegistry.destroy(actor);
    muduo::LogStream destroyed;
    destroyed << ActorLog(actor);
    EXPECT_EQ("player_id=0 entity=" + std::to_string(entt::to_integral(actor)), destroyed.buffer().toString());
}
