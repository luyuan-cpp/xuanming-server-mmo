// 直接编译生产 AOI/Grid/Interest/View，只替换网络发送边界，观察实际接收者和消息体。
// 单向关注的验收场景见 docs/design/aoi-directed-interest.md「验收场景」。
#include <gtest/gtest.h>
#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/registry/actor_log.h"
#include "generated/attribute/actorbaseattributess2c_attribute_sync.h"
#include "rpc/service_metadata/player_state_attribute_sync_service_metadata.h"
#include <algorithm>
#include <limits>
#include <vector>
#include "hexagons_grid.h"
#include "spatial/system/aoi.h"
#include "spatial/system/grid.h"
#include "spatial/system/interest.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "spatial/constants/view.h"
#include "combat/buff/comp/buff_comp.h"
#include "combat/engagement/comp/combat_engagement_comp.h"
#include "combat/engagement/system/combat_engagement.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/base_comp.pb.h"
#include "proto/common/component/npc_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "spatial/system/aoi_load.h"
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

// 任务玩法还没接入关注:用一个技能来源带任务 NPC 标签,模拟「玩家正关注着的任务 NPC」。
AoiFollowGrant QuestNpcGrant(uint64_t id) {
    return AoiFollowGrant{AoiFollowSource{AoiFollowKind::kSkill, id}, AoiPriority::kQuestNpc, false};
}

// 实体当前位置所在的六边形格。
Hex HexOf(entt::entity entity) {
    return GridSystem::CalculateHexPosition(tlsEcs.actorRegistry.get<Transform>(entity));
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
    // 生产代码还不刷怪:按 NPC 的身份造一个(带 Npc 标记,不是玩家)。
    entt::entity SpawnNpc(double x, double y) {
        const auto npc = Spawn(x, y);
        tlsEcs.actorRegistry.remove<Player, PlayerGuidComp>(npc);
        tlsEcs.actorRegistry.emplace<Npc>(npc);
        return npc;
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
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(pinnedObserver).IsVisible(mover));
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
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(pinnedObserver).IsVisible(mover));

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
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).IsVisible(target));
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
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).IsVisible(nearby));
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
    ASSERT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(nearby).IsVisible(target));
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
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).IsVisible(other)) << "与离场者无关的关系不受影响";
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
    ASSERT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(watcher).VisibleCount());
    deliveries.clear();

    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    EXPECT_EQ(1, DestroysFor(watcher, neighbor)) << "被淘汰者通知到观察者本人";
    EXPECT_EQ(1, CreatesFor(watcher, target));
    EXPECT_EQ(0, DestroysFor(neighbor, watcher)) << "淘汰按观察者独立判断,不要求互相占位";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(neighbor).IsVisible(watcher));

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

// ---------------------------------------------------------------------------
// 空间关注按距离重判(2026-11-10 冒烟回归):进视野 ≤ 视野半径、出视野 > 离开半径,
// 任一方位置变化后重新评估,不再只在跨格那一刻判距离。口径见 spatial/constants/view.h。
// 下面的坐标都取在 x = 0 的竖线上并避开格边(y = ±17.32、±51.96…),格子关系用前置断言写明。
// ---------------------------------------------------------------------------

// 冒烟复现:两人同在一格、相距 14,之后走到 5 m。修复前同格内移动不再判距离,永远互相看不见。
TEST_F(AoiDeliveryTest, ApproachWithinSameGridEntersViewWithoutCrossing) {
    const auto stationary = Spawn(0, 0);
    const auto walker = Spawn(0, 14);
    AoiSystem::Update(0);
    ASSERT_EQ(0, hex_distance(HexOf(stationary), HexOf(walker))) << "前置条件:同一格";
    ASSERT_TRUE(VisibleWatchersOf(walker).empty()) << "前置条件:相距 14,超出视野半径";
    deliveries.clear();

    MoveTo(walker, 0, 5);
    AoiSystem::Update(0);

    EXPECT_EQ(1, CreatesFor(stationary, walker)) << "静止的一方也收到走近者的创建";
    EXPECT_EQ(1, CreatesFor(walker, stationary));
    EXPECT_EQ((EntityUnorderedSet{stationary}), VisibleWatchersOf(walker));
    EXPECT_EQ((EntityUnorderedSet{walker}), VisibleWatchersOf(stationary));

    deliveries.clear();
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "位置没变不重新评估,不重复发送";
}

// 冒烟复现:跨进新的一组格子时对方还在视野外,之后在格子里走近。修复前对方所在格
// 早已是邻格、不在「新进入的格子」里,不会再判,走到跟前也看不见。
TEST_F(AoiDeliveryTest, CrossIntoGridsWithDistantNeighbourThenApproachEntersView) {
    const auto stationary = Spawn(0, 0);
    const auto walker = Spawn(0, 120);
    AoiSystem::Update(0);
    ASSERT_GT(hex_distance(HexOf(stationary), HexOf(walker)), 1) << "前置条件:起点不在邻格";

    MoveTo(walker, 0, 30);
    AoiSystem::Update(0);
    ASSERT_EQ(1, hex_distance(HexOf(stationary), HexOf(walker))) << "前置条件:跨进了以对方所在格为邻格的格子";
    ASSERT_TRUE(VisibleWatchersOf(walker).empty()) << "前置条件:相距 30,超出视野半径";
    deliveries.clear();

    MoveTo(walker, 0, 20);
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "相距 20 仍在视野外";

    MoveTo(walker, 0, 8);
    AoiSystem::Update(0);
    EXPECT_EQ(1, CreatesFor(stationary, walker));
    EXPECT_EQ(1, CreatesFor(walker, stationary));
}

// 出视野按距离:视野半径与离开半径之间是滞后区间;同一组格子里走远到离开半径外也会移除,
// 之后回到滞后区间不重新进视野,走到视野半径(含等号)才重新进。
TEST_F(AoiDeliveryTest, DistanceHysteresisKeepsViewUntilLeaveRadiusWithinSameGrids) {
    const double viewRadius = kMaxViewRadius;
    const double leaveRadius = kMaxViewRadius * kAoiLeaveRadiusFactor;
    const double bandMiddle = (viewRadius + leaveRadius) / 2;
    const auto stationary = Spawn(0, 0);
    const auto walker = Spawn(0, 5);
    AoiSystem::Update(0);
    ASSERT_EQ((EntityUnorderedSet{stationary}), VisibleWatchersOf(walker));
    deliveries.clear();

    MoveTo(walker, 0, bandMiddle);
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "滞后区间内保持显示";

    MoveTo(walker, 0, leaveRadius + 1);
    ASSERT_LE(hex_distance(HexOf(stationary), HexOf(walker)), 1) << "前置条件:仍在邻格内,移除来自距离而不是格子";
    AoiSystem::Update(0);
    EXPECT_EQ(1, DestroysFor(stationary, walker));
    EXPECT_EQ(1, DestroysFor(walker, stationary));
    EXPECT_TRUE(VisibleWatchersOf(walker).empty());
    EXPECT_TRUE(VisibleWatchersOf(stationary).empty());

    deliveries.clear();
    MoveTo(walker, 0, bandMiddle);
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "滞后区间内不重新进视野";

    MoveTo(walker, 0, viewRadius);
    AoiSystem::Update(0);
    EXPECT_EQ(1, CreatesFor(stationary, walker)) << "视野半径含等号";
    EXPECT_EQ(1, CreatesFor(walker, stationary));
}

// 移动中(速度非零)累计位移达到 kAoiReevaluateDistance 才重判;停下来(速度清零)时按精确位置立即重判。
TEST_F(AoiDeliveryTest, MovingEntityIsReevaluatedPerDistanceStepAndOnStop) {
    const double start = kMaxViewRadius + 0.5;
    const auto stationary = Spawn(0, 0);
    const auto walker = Spawn(0, start);
    tlsEcs.actorRegistry.emplace<Velocity>(walker).set_y(-1);
    AoiSystem::Update(0);
    ASSERT_TRUE(VisibleWatchersOf(walker).empty()) << "前置条件:起点在视野外";
    deliveries.clear();

    // 走了 0.8 步,已进视野半径:还在移动,先攒着。
    MoveTo(walker, 0, start - 0.8 * kAoiReevaluateDistance);
    ASSERT_LE(start - 0.8 * kAoiReevaluateDistance, kMaxViewRadius);
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "移动中累计位移不足一步,先不评估";

    // 累计到 1.6 步:按当前位置评估。
    MoveTo(walker, 0, start - 1.6 * kAoiReevaluateDistance);
    AoiSystem::Update(0);
    EXPECT_EQ(1, CreatesFor(stationary, walker));
    EXPECT_EQ(1, CreatesFor(walker, stationary));

    // 停步:另一人从另一侧走进视野半径后速度清零,位移不足一步也立即按精确位置评估,不会停在视野内却看不见。
    const auto stopper = Spawn(0, -start);
    auto& stopperVelocity = tlsEcs.actorRegistry.emplace<Velocity>(stopper);
    stopperVelocity.set_y(1);
    AoiSystem::Update(0);
    ASSERT_EQ(0, CreatesFor(stationary, stopper)) << "前置条件:起点在视野外";
    MoveTo(stopper, 0, -(start - 0.8 * kAoiReevaluateDistance));
    AoiSystem::Update(0);
    EXPECT_EQ(0, CreatesFor(stationary, stopper)) << "移动中累计位移不足一步,先不评估";

    stopperVelocity.Clear();
    AoiSystem::Update(0);
    EXPECT_EQ(1, CreatesFor(stationary, stopper)) << "停下来的那一刻按精确位置判一次";
    EXPECT_EQ(1, CreatesFor(stopper, stationary));
}

// 满表被拒的附近候选不记关系,观察者打上补回标记;名额空出后下一帧就补上,不用等谁走动。
TEST_F(AoiDeliveryTest, CapacityRejectedNeighbourIsAdmittedWhenSlotFreesWithoutMovement) {
    const auto observer = Spawn(0, 0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(observer, AoiClientCapacityComp{kAoiListCapacityMin});
    std::vector<entt::entity> farFollows;
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        farFollows.push_back(Spawn(500.0 + 50.0 * i, 500));
    }
    AoiSystem::Update(0);
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        ASSERT_TRUE(AoiSystem::Follow(observer, farFollows[i], SkillGrant(i + 1)));
    }
    const auto entrant = Spawn(0, 3);
    AoiSystem::Update(0);
    ASSERT_EQ(0, CreatesFor(observer, entrant)) << "前置条件:满表拒收";
    ASSERT_EQ(1, CreatesFor(entrant, observer));
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(observer)) << "前置条件:被拒的观察者打上补回标记";

    AoiSystem::Unfollow(observer, farFollows.front(), AoiFollowSource{AoiFollowKind::kSkill, 1});
    deliveries.clear();
    AoiSystem::Update(0);

    EXPECT_EQ(1, CreatesFor(observer, entrant)) << "名额空出后下一帧补回附近的人";
    EXPECT_EQ(0, CreatesFor(entrant, observer)) << "反向关系早已建立,不重复创建";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(observer)) << "补回后标记摘掉";

    deliveries.clear();
    AoiSystem::Update(0);
    EXPECT_TRUE(deliveries.empty()) << "没有待补回的候选时不再重扫";
}

// 有补回标记但名额仍满:不重扫(标记只在有空名额时消费,开销有界)。
TEST_F(AoiDeliveryTest, RefillMarkWithoutFreeSlotDoesNotRescan) {
    const auto observer = Spawn(0, 0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(observer, AoiClientCapacityComp{kAoiListCapacityMin});
    std::vector<entt::entity> farFollows;
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        farFollows.push_back(Spawn(500.0 + 50.0 * i, 500));
    }
    AoiSystem::Update(0);
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        ASSERT_TRUE(AoiSystem::Follow(observer, farFollows[i], SkillGrant(i + 1)));
    }
    Spawn(0, 3);
    AoiSystem::Update(0);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(observer));
    deliveries.clear();

    AoiSystem::Update(0);

    EXPECT_TRUE(deliveries.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(observer)) << "名额没空,标记留着等下次";
}

// 按距离出视野只撤空间来源:显式关注照常显示,客户端不收移除。
TEST_F(AoiDeliveryTest, DistanceLeaveKeepsExplicitFollow) {
    const auto watcher = Spawn(0, 0);
    const auto target = Spawn(0, 5);
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(watcher, target, SkillGrant(1)));
    deliveries.clear();

    MoveTo(target, 0, kMaxViewRadius * kAoiLeaveRadiusFactor + 1);
    AoiSystem::Update(0);

    EXPECT_EQ(0, DestroysFor(watcher, target)) << "显式来源仍在,不移除";
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).IsVisible(target));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(watcher).entries.at(target).spatial);
    EXPECT_EQ(1, DestroysFor(target, watcher)) << "反方向只有空间来源,按距离移除";
}

// 重判不绕过隐身:走近的隐身目标不进普通观察者的视野。
TEST_F(AoiDeliveryTest, ReevaluationDoesNotRevealStealthedTarget) {
    const auto observer = Spawn(0, 0);
    const auto hidden = Spawn(0, 15);
    tlsEcs.actorRegistry.emplace<StealthedTagComp>(hidden);
    AoiSystem::Update(0);
    deliveries.clear();

    MoveTo(hidden, 0, 3);
    AoiSystem::Update(0);

    EXPECT_EQ(0, CreatesFor(observer, hidden));
    EXPECT_EQ(1, CreatesFor(hidden, observer)) << "隐身者自己照常看见别人";
}

// 双方都不动:B 隐身时附近的普通观察者收到一次移除,B 照样看得见别人;隐身标签还在(叠了多个隐身效果、
// 只结束了一个)时刷新不显形;最后一个隐身效果结束,不用谁走动就按附近候选重建,发创建与当前位置。
TEST_F(AoiDeliveryTest, StealthThenRevealWithoutAnyMovementRebuildsNearbyObservers) {
    const auto observer = Spawn(0, 0);
    const auto hider = Spawn(0, 5);
    AoiSystem::Update(0);
    deliveries.clear();

    tlsEcs.actorRegistry.emplace<StealthedTagComp>(hider);
    AoiSystem::RefreshTargetVisibility(hider);
    EXPECT_EQ(1, DestroysFor(observer, hider));
    EXPECT_EQ(0, DestroysFor(hider, observer)) << "隐身只影响别人看它";
    MoveTo(hider, 0, 4);
    AoiSystem::Update(0);
    EXPECT_EQ(0, CreatesFor(observer, hider)) << "隐身期间位移重判也不会把它加回来";
    MoveTo(hider, 0, 5);
    AoiSystem::Update(0);

    deliveries.clear();
    AoiSystem::RefreshTargetVisibility(hider);
    EXPECT_TRUE(deliveries.empty()) << "标签还在就不显形";

    tlsEcs.actorRegistry.remove<StealthedTagComp>(hider);
    AoiSystem::RefreshTargetVisibility(hider);
    EXPECT_EQ(1, CreatesFor(observer, hider)) << "最后一个隐身效果结束,立即重建附近的观察者";
    EXPECT_EQ((EntityUnorderedSet{observer}), VisibleWatchersOf(hider));
}

// 视野半径变了也要重判,不用等谁走动(以后的鹰眼、致盲之类会改 ViewRadius);只影响它自己看别人。
TEST_F(AoiDeliveryTest, ViewRadiusChangeTriggersReevaluationWithoutMovement) {
    const auto observer = Spawn(0, 0);
    const auto target = Spawn(0, 14);
    AoiSystem::Update(0);
    ASSERT_EQ(0, CreatesFor(observer, target)) << "前置条件:14 m 超出默认视野";

    tlsEcs.actorRegistry.emplace<ViewRadius>(observer).set_radius(15);
    AoiSystem::Update(0);
    EXPECT_EQ(1, CreatesFor(observer, target)) << "视野放大到 15 m,下一帧就看见";
    EXPECT_EQ(0, CreatesFor(target, observer)) << "对方的视野没变,关注是单向的";

    tlsEcs.actorRegistry.get<ViewRadius>(observer).set_radius(5);
    AoiSystem::Update(0);
    EXPECT_EQ(1, DestroysFor(observer, target)) << "视野缩到 5 m,14 m 超出离开半径 10 m";
}

// 同一帧里两个人先后走近同一个观察者:变化帧末合成一批,观察者只收一条列表消息,里面两个人都在。
TEST_F(AoiDeliveryTest, SameFrameChangesForOneWatcherArriveAsOneListMessage) {
    const auto observer = Spawn(0, 0);
    const auto north = Spawn(0, 14);
    const auto south = Spawn(0, -14);
    AoiSystem::Update(0);
    deliveries.clear();

    MoveTo(north, 0, 9);
    MoveTo(south, 0, -9);
    AoiSystem::Update(0);

    int observerCreateMessages = 0;
    for (const auto& sent : deliveries) {
        if (sent.receiver == observer && sent.messageId == SceneSceneClientPlayerNotifyActorListCreateMessageId) {
            ++observerCreateMessages;
        }
    }
    EXPECT_EQ(1, observerCreateMessages) << "同一帧的变化合成一条";
    EXPECT_EQ(1, CreatesFor(observer, north));
    EXPECT_EQ(1, CreatesFor(observer, south));
    EXPECT_EQ(0, CreatesFor(north, south)) << "两人相距 18 m,彼此不在视野里";
}

// ---------------------------------------------------------------------------
// 战斗关注(交手关系,CombatEngagementSystem):一次伤害或一跳治疗让双方互相显式关注,
// 离得再远也看得见;kCombatDisengageSeconds 秒不再交手就脱战撤销,死亡立即撤销。
// 权重表是静态的,兴趣随时变靠交手来源有寿命。设计见 docs/design/aoi-directed-interest.md「战斗关注」。
// ---------------------------------------------------------------------------

// 远处交手:双方各收一次创建,互相收得到移动;同一对再交手只续期,不重复创建。
TEST_F(AoiDeliveryTest, CombatEngagementMakesFarCombatantsMutuallyVisible) {
    const auto archer = Spawn(0, 0);
    const auto target = Spawn(0, 120);
    AoiSystem::Update(0);
    ASSERT_TRUE(VisibleWatchersOf(target).empty()) << "前置条件:相距 120,没有空间关注";
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(archer, target);

    EXPECT_EQ(1, CreatesFor(archer, target));
    EXPECT_EQ(1, CreatesFor(target, archer)) << "被打的一方也看见远处的攻击者";
    EXPECT_EQ((EntityUnorderedSet{archer}), VisibleWatchersOf(target));
    EXPECT_EQ((EntityUnorderedSet{target}), VisibleWatchersOf(archer));

    deliveries.clear();
    CombatEngagementSystem::RecordDamage(target, archer);
    EXPECT_TRUE(deliveries.empty()) << "再交手只续期";
}

// 脱战:最后一次交手后满 kCombatDisengageSeconds 秒撤销;期间再交手重新计时。
TEST_F(AoiDeliveryTest, CombatEngagementExpiresAfterDisengageTimeAndRenewsOnInteraction) {
    const auto lhs = Spawn(0, 0);
    const auto rhs = Spawn(0, 120);
    AoiSystem::Update(0);
    CombatEngagementSystem::RecordDamage(lhs, rhs);
    deliveries.clear();

    CombatEngagementSystem::Update(kCombatDisengageSeconds - 1);
    EXPECT_TRUE(deliveries.empty()) << "还没到脱战时间";

    CombatEngagementSystem::RecordDamage(lhs, rhs);
    CombatEngagementSystem::Update(kCombatDisengageSeconds - 1);
    EXPECT_TRUE(deliveries.empty()) << "再交手重新计时";

    CombatEngagementSystem::Update(1);
    EXPECT_EQ(1, DestroysFor(lhs, rhs));
    EXPECT_EQ(1, DestroysFor(rhs, lhs));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(lhs)) << "最后一份交手关系结束时摘掉组件";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(rhs));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<AoiListComp>(lhs).HasRelation(rhs));
}

// 附近交手:脱战只撤交手来源,对方仍在视野里就照常显示,不发移除。
TEST_F(AoiDeliveryTest, DisengageKeepsNearbyCombatantVisible) {
    const auto lhs = Spawn(0, 0);
    const auto rhs = Spawn(0, 5);
    AoiSystem::Update(0);
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(lhs, rhs);
    EXPECT_TRUE(deliveries.empty()) << "已在显示,交手不重复创建";
    CombatEngagementSystem::Update(kCombatDisengageSeconds);

    EXPECT_TRUE(deliveries.empty());
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(lhs).IsVisible(rhs));
    EXPECT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(lhs).entries.at(rhs).follows.empty()) << "交手来源已撤";
}

// 死亡:死者的交手关系立即结束,远处的交手对象收到一次移除。
TEST_F(AoiDeliveryTest, DeathEndsCombatEngagementImmediately) {
    const auto killer = Spawn(0, 0);
    const auto victim = Spawn(0, 120);
    const auto healer = Spawn(0, -120);
    AoiSystem::Update(0);
    CombatEngagementSystem::RecordDamage(killer, victim);
    CombatEngagementSystem::RecordHeal(healer, victim);
    deliveries.clear();

    CombatEngagementSystem::OnDeath(victim);

    EXPECT_EQ(1, DestroysFor(killer, victim));
    EXPECT_EQ(1, DestroysFor(healer, victim));
    EXPECT_EQ(1, DestroysFor(victim, killer));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(victim));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(killer));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(healer));
}

// 战斗时先看交手的人:满表全是任务 NPC 时,交手对象挤掉一个;脱战后被挤掉的任务 NPC 自然回来。
// 权重表全程没变,变的只是交手来源的有无。
TEST_F(AoiDeliveryTest, CombatOutranksQuestNpcAndQuestNpcReturnsAfterDisengage) {
    const auto player = Spawn(0, 0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(player, AoiClientCapacityComp{kAoiListCapacityMin});
    std::vector<entt::entity> questNpcs;
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        questNpcs.push_back(Spawn(500.0 + 50.0 * i, 500));
    }
    const auto enemy = Spawn(0, -120);
    AoiSystem::Update(0);
    for (uint32_t i = 0; i < kAoiListCapacityMin; ++i) {
        ASSERT_TRUE(AoiSystem::Follow(player, questNpcs[i], QuestNpcGrant(i + 1)));
    }
    ASSERT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(player).VisibleCount());
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(enemy, player);

    EXPECT_EQ(1, CreatesFor(player, enemy)) << "交手对象挤进满表";
    int questNpcRemovals = 0;
    for (const auto npc : questNpcs) questNpcRemovals += DestroysFor(player, npc);
    EXPECT_EQ(1, questNpcRemovals) << "只挤掉一个任务 NPC,它转为暂隐、关系保留";
    EXPECT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(player).VisibleCount());
    deliveries.clear();

    CombatEngagementSystem::Update(kCombatDisengageSeconds);

    EXPECT_EQ(1, DestroysFor(player, enemy)) << "脱战撤销交手关注";
    int questNpcReturns = 0;
    for (const auto npc : questNpcs) questNpcReturns += CreatesFor(player, npc);
    EXPECT_EQ(1, questNpcReturns) << "空出的名额补回被挤掉的任务 NPC";
    EXPECT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(player).VisibleCount());
}

// 冒烟场景:打架时附近的人被交手对象挤掉;脱战后站着不动,下一帧也把附近的人补回来。
TEST_F(AoiDeliveryTest, NearbyActorEvictedDuringCombatReturnsAfterDisengageWithoutMovement) {
    const auto player = Spawn(0, 0);
    tlsEcs.actorRegistry.emplace<AoiClientCapacityComp>(player, AoiClientCapacityComp{kAoiListCapacityMin});
    std::vector<entt::entity> farFollows;
    for (uint32_t i = 1; i < kAoiListCapacityMin; ++i) {
        farFollows.push_back(Spawn(500.0 + 50.0 * i, 500));
    }
    const auto nearbyNpc = Spawn(0, 3);
    const auto enemy = Spawn(0, -120);
    AoiSystem::Update(0);
    for (uint32_t i = 0; i < farFollows.size(); ++i) {
        ASSERT_TRUE(AoiSystem::Follow(player, farFollows[i], SkillGrant(i + 1)));
    }
    ASSERT_TRUE(tlsEcs.actorRegistry.get<AoiListComp>(player).IsVisible(nearbyNpc));
    ASSERT_EQ(kAoiListCapacityMin, tlsEcs.actorRegistry.get<AoiListComp>(player).VisibleCount());
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(player, enemy);
    ASSERT_EQ(1, DestroysFor(player, nearbyNpc)) << "前置条件:附近的普通候选被交手对象挤掉";
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(player));

    CombatEngagementSystem::Update(kCombatDisengageSeconds);
    deliveries.clear();
    AoiSystem::Update(0);

    EXPECT_EQ(1, CreatesFor(player, nearbyNpc)) << "名额空出后,不用走动也补回附近的人";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(player));
}

// 离场:对方离开场景后,交手记录在下一次脱战计时里清掉,只留离场那一次移除,不重复发。
TEST_F(AoiDeliveryTest, CounterpartLeavingSceneEndsEngagementWithoutExtraMessages) {
    const auto stayer = Spawn(0, 0);
    const auto leaver = Spawn(0, 120);
    AoiSystem::Update(0);
    CombatEngagementSystem::RecordDamage(stayer, leaver);
    deliveries.clear();

    AoiSystem::OnLeaveScene(leaver);
    tlsEcs.actorRegistry.remove<SceneEntityComp, Hex>(leaver);  // 与 PlayerSceneSystem::LeaveCurrentScene 一致
    ASSERT_EQ(1, ActorDestroysFor(stayer, leaver));
    deliveries.clear();

    CombatEngagementSystem::Update(0);

    EXPECT_TRUE(deliveries.empty()) << "离场已发过移除";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(stayer));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(leaver));
}

// 自己打自己、跨场景、已销毁的实体都不算交手,也不留组件。
TEST_F(AoiDeliveryTest, InvalidCombatInteractionsAreIgnored) {
    const auto actor = Spawn(0, 0);
    const auto elsewhere = Spawn(0, 120);
    tlsEcs.actorRegistry.get<SceneEntityComp>(elsewhere).sceneEntity = tlsEcs.sceneRegistry.create();
    const auto destroyed = Spawn(0, 5);
    tlsEcs.actorRegistry.destroy(destroyed);

    CombatEngagementSystem::RecordDamage(actor, actor);
    CombatEngagementSystem::RecordDamage(actor, elsewhere);
    CombatEngagementSystem::RecordDamage(destroyed, actor);
    CombatEngagementSystem::RecordHeal(actor, actor);
    CombatEngagementSystem::RecordHeal(elsewhere, actor);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(actor));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatEngagementComp>(elsewhere));
    EXPECT_TRUE(deliveries.empty());
}

// ---------------------------------------------------------------------------
// 仇恨名单(CombatAllyRosterComp):一起打同一个 NPC 的人互相算友方交手,离得再远也互相看得见。
// ---------------------------------------------------------------------------

// 两个人一前一后打同一个怪:第二个人出手时,两人互相关注;怪的仇恨名单按加入先后记着他们。
TEST_F(AoiDeliveryTest, FightersOfTheSameNpcBecomeCombatAllies) {
    const auto boss = SpawnNpc(0, 0);
    const auto archer = Spawn(0, 120);
    const auto warrior = Spawn(0, -120);
    AoiSystem::Update(0);
    ASSERT_TRUE(VisibleWatchersOf(archer).empty()) << "前置条件:彼此相距 240,没有空间关注";
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(archer, boss);
    EXPECT_EQ(0, CreatesFor(archer, warrior)) << "还没有共同的敌人";
    CombatEngagementSystem::RecordDamage(warrior, boss);

    EXPECT_EQ(1, CreatesFor(archer, warrior)) << "一起打同一个怪,远处的友方也看得见";
    EXPECT_EQ(1, CreatesFor(warrior, archer));
    EXPECT_EQ(1, CreatesFor(warrior, boss));
    EXPECT_EQ((std::vector<entt::entity>{archer, warrior}),
              tlsEcs.actorRegistry.get<CombatAllyRosterComp>(boss).members);
}

// 治疗仇恨:给正在打怪的人加血,治疗者也进怪的仇恨名单,和打这个怪的其他人互相算友方。
TEST_F(AoiDeliveryTest, HealAggroPutsHealerOnTheNpcRosterAndAmongItsFighters) {
    const auto boss = SpawnNpc(0, 0);
    const auto tank = Spawn(0, 120);
    const auto dps = Spawn(120, 0);
    const auto healer = Spawn(0, -120);
    AoiSystem::Update(0);
    CombatEngagementSystem::RecordDamage(tank, boss);
    CombatEngagementSystem::RecordDamage(dps, boss);
    deliveries.clear();

    CombatEngagementSystem::RecordHeal(healer, tank);

    EXPECT_EQ(1, CreatesFor(healer, tank)) << "治疗关系";
    EXPECT_EQ(1, CreatesFor(healer, boss)) << "治疗者和坦克正在打的怪也算交手";
    EXPECT_EQ(1, CreatesFor(healer, dps)) << "进了怪的仇恨名单,和打这个怪的其他人互相算友方";
    EXPECT_EQ(1, CreatesFor(dps, healer));
    const auto& roster = tlsEcs.actorRegistry.get<CombatAllyRosterComp>(boss).members;
    EXPECT_NE(roster.end(), std::ranges::find(roster, healer));
}

// 玩家之间的混战不推友方:打同一个人的两个人可能彼此也是敌人。
TEST_F(AoiDeliveryTest, PlayersHittingTheSamePlayerDoNotBecomeAllies) {
    const auto victim = Spawn(0, 0);
    const auto lhs = Spawn(0, 120);
    const auto rhs = Spawn(0, -120);
    AoiSystem::Update(0);
    deliveries.clear();

    CombatEngagementSystem::RecordDamage(lhs, victim);
    CombatEngagementSystem::RecordDamage(rhs, victim);

    EXPECT_EQ(0, CreatesFor(lhs, rhs));
    EXPECT_EQ(0, CreatesFor(rhs, lhs));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatAllyRosterComp>(victim)) << "仇恨名单只挂在 NPC 上";
}

// 怪死了:它的仇恨名单清空、交手双方各收一次移除;刚一起打完的两个人按脱战时间才互相移除。
TEST_F(AoiDeliveryTest, NpcDeathClearsRosterAndAlliesExpireByDisengageTime) {
    const auto boss = SpawnNpc(0, 0);
    const auto archer = Spawn(0, 120);
    const auto warrior = Spawn(0, -120);
    AoiSystem::Update(0);
    CombatEngagementSystem::RecordDamage(archer, boss);
    CombatEngagementSystem::RecordDamage(warrior, boss);
    deliveries.clear();

    CombatEngagementSystem::OnDeath(boss);

    EXPECT_EQ(1, DestroysFor(archer, boss));
    EXPECT_EQ(1, DestroysFor(warrior, boss));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CombatAllyRosterComp>(boss));
    EXPECT_EQ(0, DestroysFor(archer, warrior)) << "友方交手不随怪的死亡立即撤销";

    CombatEngagementSystem::Update(kCombatDisengageSeconds);
    EXPECT_EQ(1, DestroysFor(archer, warrior));
    EXPECT_EQ(1, DestroysFor(warrior, archer));
}

// 仇恨名单有上限:满了以后来的人照样看得见怪,但不和名单里的人互相关注;有人离开后,它下一次出手时补进来。
TEST_F(AoiDeliveryTest, RosterCapStopsAllyFanOutUntilASlotFrees) {
    const auto boss = SpawnNpc(0, 0);
    std::vector<entt::entity> fighters;
    for (std::size_t i = 0; i <= kMaxCombatAllyRoster; ++i) {
        fighters.push_back(Spawn(300.0 + 50.0 * static_cast<double>(i), 300));
    }
    AoiSystem::Update(0);
    for (const auto fighter : fighters) {
        CombatEngagementSystem::RecordDamage(fighter, boss);
    }
    const auto late = fighters.back();
    const auto& roster = tlsEcs.actorRegistry.get<CombatAllyRosterComp>(boss).members;
    ASSERT_EQ(kMaxCombatAllyRoster, roster.size());
    ASSERT_EQ(roster.end(), std::ranges::find(roster, late)) << "前置条件:名单满了,最后一个没进去";
    EXPECT_EQ(0, CreatesFor(late, fighters.front())) << "名单外的人不和名单里的人互相关注";
    EXPECT_EQ(1, CreatesFor(late, boss)) << "但照样看得见自己打的怪";

    CombatEngagementSystem::OnDeath(fighters.front());
    deliveries.clear();
    CombatEngagementSystem::RecordDamage(late, boss);

    const auto& refilled = tlsEcs.actorRegistry.get<CombatAllyRosterComp>(boss).members;
    EXPECT_NE(refilled.end(), std::ranges::find(refilled, late)) << "空出位置后,下一次出手补进名单";
    EXPECT_EQ(1, CreatesFor(late, fighters[1]));
    EXPECT_EQ(1, CreatesFor(fighters[1], late));
}

// ---------------------------------------------------------------------------
// 服务器压力(AoiLoadMonitor + AoiSystem::ApplyServerPressure):模拟步耗时高了就调小关注容量,
// 超出的按权重从低到高、同权重先远后近裁掉,压力回落后补回。
// ---------------------------------------------------------------------------

// 持续满负荷时压力升到顶,负载回落后降回 0。
TEST(AoiLoadMonitorTest, PressureRisesUnderSustainedOverloadAndRecovers) {
    AoiLoadMonitor monitor;
    constexpr double kStep = 0.05;
    for (int i = 0; i < 200; ++i) monitor.Observe(0.2 * kStep, kStep);
    EXPECT_DOUBLE_EQ(0.0, monitor.PressureFactor()) << "利用率 20% 不受压";

    for (int i = 0; i < 1000; ++i) monitor.Observe(1.0 * kStep, kStep);
    EXPECT_DOUBLE_EQ(1.0, monitor.PressureFactor()) << "满负荷持续一段时间,压力到顶";

    for (int i = 0; i < 1000; ++i) monitor.Observe(0.2 * kStep, kStep);
    EXPECT_DOUBLE_EQ(0.0, monitor.PressureFactor()) << "负载回落,压力回到 0";
}

// 每秒只决定一次;档位边界附近的小幅波动不换档,偏离够一档才换。
TEST(AoiLoadMonitorTest, DecidesOncePerIntervalAndHoldsLevelWithinHysteresis) {
    AoiLoadMonitor monitor;
    constexpr double kStep = 0.05;
    int decisions = 0;
    for (int i = 0; i < 100; ++i) decisions += monitor.Observe(0.1 * kStep, kStep) ? 1 : 0;
    EXPECT_EQ(5, decisions) << "20 FPS 下 100 步 = 5 秒,每秒决定一次";

    const auto settle = [&monitor](double utilization) {
        for (int i = 0; i < 2000; ++i) monitor.Observe(utilization * kStep, kStep);
    };
    settle(0.7);
    EXPECT_DOUBLE_EQ(0.5, monitor.PressureFactor()) << "利用率 0.7 对应第 5 档";
    settle(0.72);
    EXPECT_DOUBLE_EQ(0.5, monitor.PressureFactor()) << "只偏离半档,不换";
    settle(0.74);
    EXPECT_DOUBLE_EQ(0.6, monitor.PressureFactor()) << "偏离一档,换到第 6 档";
}

// 压力把容量降到下限:权重高的留下,同权重离得近的留下,其余裁掉并通知观察者;压力回落后下一帧补回。
TEST_F(AoiDeliveryTest, ServerPressureTrimsLowestWeightFarthestFirstAndRestoresWhenItDrops) {
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene);
    const auto observer = Spawn(0, 0);
    std::vector<entt::entity> nearby;
    for (int i = 0; i < 25; ++i) {
        nearby.push_back(Spawn(0, 1.0 + 0.3 * i));
    }
    const auto questNpc = Spawn(0, -120);
    AoiSystem::Update(0);
    ASSERT_TRUE(AoiSystem::Follow(observer, questNpc, QuestNpcGrant(1)));
    const auto& list = tlsEcs.actorRegistry.get<AoiListComp>(observer);
    ASSERT_EQ(26u, list.VisibleCount()) << "前置条件:25 个附近的人加一个任务 NPC";
    deliveries.clear();

    AoiSystem::ApplyServerPressure(1.0);

    EXPECT_EQ(kAoiListCapacityMin, list.VisibleCount()) << "满压力时容量降到下限";
    EXPECT_TRUE(list.IsVisible(questNpc)) << "权重高的留下";
    for (int i = 0; i < 19; ++i) {
        EXPECT_TRUE(list.IsVisible(nearby[i])) << "同权重离得近的留下,i=" << i;
    }
    for (int i = 19; i < 25; ++i) {
        EXPECT_FALSE(list.HasRelation(nearby[i])) << "离得最远的先裁,i=" << i;
        EXPECT_EQ(1, DestroysFor(observer, nearby[i]));
    }
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<AoiNearbyRefillComp>(observer)) << "被裁的附近的人等容量回升时补回";
    EXPECT_FLOAT_EQ(1.0f, tlsEcs.sceneRegistry.get<ScenePressureComp>(scene).pressureFactor);
    deliveries.clear();

    AoiSystem::ApplyServerPressure(0.0);
    AoiSystem::Update(0);

    for (int i = 19; i < 25; ++i) {
        EXPECT_EQ(1, CreatesFor(observer, nearby[i])) << "压力回落后补回,i=" << i;
    }
    EXPECT_EQ(26u, list.VisibleCount());
}

// 压力每次都写进本线程的全部场景:之后新建的场景下一次就拿到当前压力;值没变的场景不再校正。
TEST_F(AoiDeliveryTest, ServerPressureReachesScenesCreatedLater) {
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene);
    AoiSystem::ApplyServerPressure(0.5);
    const auto later = tlsEcs.sceneRegistry.create();
    tlsEcs.sceneRegistry.emplace<SceneInfoComp>(later);
    EXPECT_FALSE(tlsEcs.sceneRegistry.any_of<ScenePressureComp>(later));

    AoiSystem::ApplyServerPressure(0.5);

    EXPECT_FLOAT_EQ(0.5f, tlsEcs.sceneRegistry.get<ScenePressureComp>(later).pressureFactor);
    EXPECT_FLOAT_EQ(0.5f, tlsEcs.sceneRegistry.get<ScenePressureComp>(scene).pressureFactor);
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
