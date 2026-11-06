#include <algorithm>
#include <chrono>
#include <deque>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include <gtest/gtest.h>
#include <hiredis/hiredis.h>

#include "../muduo_grpc_test/muduo_test_loop.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "modules/scene/comp/scene_comp.h"
#include "muduo/net/EventLoop.h"
#include "muduo_grpc/channel.h"
#include "muduo_grpc/method_table.h"
#include "muduo_grpc/server.h"
#include "muduo_grpc/server_call.h"
#include "network/network_utils.h"
#include "network/node_utils.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "node/system/node/node_util.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/comp/team_follow_comp.h"
#include "player/system/connection_lost_hold.h"
#include "player/system/enter_fence.h"
#include "player/system/player_team.h"
#include "player/system/player_team_follow_scope.h"
#include "player/system/team_follow_system.h"
#include "proto/common/component/team_comp.pb.h"
#include "core/system/scene_task_scope.h"
#include "player/system/role_admission_system.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/scene/scene_info.pb.h"
#include "proto/scene_manager/storage.pb.h"
#include "spatial/comp/teleport_settle_comp.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "time/system/time.h"
#include "type_alias/player_session_type_alias.h"

NodeInfo& TestLocalNodeInfo();

class PlayerTeamFollowTestAccess
{
public:
    static void Check(entt::entity player, uint64_t leaderId)
    {
        PlayerTeamSystem::CheckFollowLeader(player, leaderId);
    }
};

namespace
{
using namespace std::chrono_literals;
using RedisReplyHandler = PlayerTeamFollowScope::RedisReplyHandler;

constexpr uint64_t kPlayerId = 950200001;
constexpr uint64_t kLeaderId = 950200002;
constexpr uint64_t kCurrentSceneId = 700;
constexpr uint32_t kZoneId = 3;
constexpr uint32_t kSceneNodeId = 12;
constexpr uint32_t kOriginalGateId = 5;
constexpr uint32_t kCurrentGateId = 6;
constexpr double kLeaderX = 20.0;
constexpr uint64_t kTeamId = 950200100;

SessionId SessionOnGate(uint32_t gateId)
{
    return (gateId << SessionIdGenerator::node_bit()) | 1;
}

void Deliver(RedisReplyHandler callback, int type, std::string bytes = {})
{
    redisReply reply{};
    reply.type = type;
    reply.str = bytes.data();
    reply.len = bytes.size();
    callback(&reply);
}

std::string LeaderLocation(uint64_t sceneId = kCurrentSceneId, uint32_t zoneId = kZoneId,
                           uint32_t nodeId = kSceneNodeId)
{
    storage::PlayerLocation location;
    location.set_scene_id(sceneId);
    location.set_zone_id(zoneId);
    location.set_node_id(std::to_string(nodeId));
    // 跟随不读回滚回执，但必须能解析带内嵌零字节的完整 Redis 值。
    location.set_rollback_receipt(std::string("test\0receipt", 12));
    return location.SerializeAsString();
}

class FakeRedis
{
public:
    int Get(std::string_view key, RedisReplyHandler callback)
    {
        keys.emplace_back(key);
        threads.push_back(std::this_thread::get_id());
        if (std::exchange(rejectNext, false))
        {
            return REDIS_ERR;
        }
        if (auto inlineReply = std::exchange(onSubmit, {}))
        {
            inlineReply(std::move(callback));
        }
        else
        {
            pending.push_back(std::move(callback));
        }
        return REDIS_OK;
    }

    RedisReplyHandler TakeReply()
    {
        auto callback = std::move(pending.front());
        pending.pop_front();
        return callback;
    }

    void Reply(int type, std::string bytes = {})
    {
        ASSERT_FALSE(pending.empty());
        Deliver(TakeReply(), type, std::move(bytes));
    }

    void Disconnect()
    {
        ASSERT_FALSE(pending.empty());
        TakeReply()(nullptr);
    }

    std::vector<std::string> keys;
    std::vector<std::thread::id> threads;
    std::deque<RedisReplyHandler> pending;
    std::function<void(RedisReplyHandler)> onSubmit;
    bool ready{true};
    bool rejectNext{false};
};

class RecordingSceneManager final : public muduo_grpc::Service
{
public:
    void RegisterMethods(muduo_grpc::MethodTable& methods) override
    {
        methods.AddUnary(std::string(scene_manager::kSceneManagerEnterSceneGrpcPath),
            [weak = std::weak_ptr(requests)](muduo_grpc::ServerCall call, std::string_view bytes)
            {
                const auto observed = weak.lock();
                if (!observed || !call.TryStartBusiness())
                {
                    return;
                }
                scene_manager::EnterSceneRequest request;
                if (!request.ParseFromArray(bytes.data(), static_cast<int>(bytes.size())))
                {
                    call.FinishError({muduo_grpc::StatusCode::kInvalidArgument, "invalid request"});
                    return;
                }
                scene_manager::EnterSceneResponse response;
                response.set_player_id(request.player_id());
                response.set_correlation_id(request.correlation_id());
                response.set_error_code(5); // 普通拒绝，走现有在途记录清理路径。
                observed->push_back(std::move(request));
                call.FinishOk(response.SerializeAsString());
            });
    }

    std::shared_ptr<std::vector<scene_manager::EnterSceneRequest>> requests =
        std::make_shared<std::vector<scene_manager::EnterSceneRequest>>();
};

class PlayerTeamFollowTest : public ::testing::Test
{
protected:
    void SetUp() override
    {
        tlsEcs.Clear();
        TestLocalNodeInfo().set_zone_id(kZoneId);
        TestLocalNodeInfo().set_node_id(kSceneNodeId);
        scene_ = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene_).set_scene_id(kCurrentSceneId);
        player_ = MakePlayer(kPlayerId, 0.0);
        MakePlayer(kLeaderId, kLeaderX);
        BindSession(player_, kPlayerId, SessionOnGate(kOriginalGateId));
        scope_.emplace(loop_, [&redis = redis_] { return redis.ready; },
            [&redis = redis_](std::string_view key, RedisReplyHandler callback)
            {
                return redis.Get(key, std::move(callback));
            });
    }

    void TearDown() override
    {
        tasks_.reset();
        scope_.reset();
        tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService).clear();
        tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService).clear();
        server_.reset();
        // 通道与服务端已在 I/O 派发栈外释放。处理它们排队的连接拆除以及任务完成通知。
        loop_.queueInLoop([&loop = loop_] { loop.queueInLoop([&loop] { loop.quit(); }); });
        loop_.wakeup();
        loop_.loop();
        tlsEcs.Clear();
    }

    entt::entity MakePlayer(uint64_t id, double x)
    {
        auto& registry = tlsEcs.actorRegistry;
        const auto player = registry.create();
        registry.emplace<PlayerGuidComp>(player, id);
        registry.emplace<SceneEntityComp>(player, scene_);
        auto& transform = registry.emplace<Transform>(player);
        transform.mutable_location()->set_x(x);
        transform.mutable_rotation()->set_y(x);
        tlsEcs.playerList.emplace(id, player);
        return player;
    }

    void BindSession(entt::entity player, uint64_t playerId, SessionId sessionId)
    {
        auto& snapshot = tlsEcs.actorRegistry.get_or_emplace<PlayerSessionSnapshotComp>(player);
        SessionMap().erase(snapshot.gate_session_id());
        snapshot.set_gate_session_id(sessionId);
        SessionMap().insert_or_assign(sessionId, playerId);
    }

    void Check()
    {
        PlayerTeamFollowTestAccess::Check(player_, kLeaderId);
    }

    void OpenLocationRead()
    {
        Check();
        ASSERT_EQ(redis_.pending.size(), 1u);
        redis_.Reply(REDIS_REPLY_NIL);
        ASSERT_EQ(redis_.pending.size(), 1u);
        ASSERT_EQ(redis_.keys.size(), 2u);
    }

    static void ExpectNoRegroup(entt::entity player)
    {
        const auto* transform = tlsEcs.actorRegistry.try_get<Transform>(player);
        ASSERT_NE(transform, nullptr);
        EXPECT_DOUBLE_EQ(transform->location().x(), 0.0);
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeleportSettleComp>(player));
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    }

    void StartSceneManager()
    {
        const auto port = muduo_grpc_test::FindFreeLoopbackPort();
        ASSERT_NE(port, 0);
        muduo_grpc::ServerOptions serverOptions;
        serverOptions.statsLogInterval = 0s;
        server_ = muduo_grpc::Server::Create(loop_, "127.0.0.1", port, std::move(serverOptions));
        ASSERT_NE(server_, nullptr);
        ASSERT_TRUE(server_->RegisterService(sceneManager_));
        server_->Start();

        muduo_grpc::ChannelOptions channelOptions;
        channelOptions.maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
        auto channel = muduo_grpc::Channel::Create(loop_, "127.0.0.1", port, std::move(channelOptions));
        ASSERT_NE(channel, nullptr);
        auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        const auto entity = registry.create();
        registry.emplace<NodeInfo>(entity);
        registry.emplace<MuduoGrpcChannelPtr>(entity, std::move(channel));
    }

    muduo::net::EventLoop loop_;
    FakeRedis redis_;
    std::optional<PlayerTeamFollowScope> scope_;
    std::optional<SceneTaskScope> tasks_{std::in_place, loop_};
    RecordingSceneManager sceneManager_;
    std::shared_ptr<muduo_grpc::Server> server_;
    SceneEntity scene_{entt::null};
    entt::entity player_{entt::null};
};

TEST_F(PlayerTeamFollowTest, FirstGetRequiresExplicitNil)
{
    for (const auto type : {REDIS_REPLY_STRING, REDIS_REPLY_ERROR, REDIS_REPLY_INTEGER})
    {
        SCOPED_TRACE(type);
        const auto count = redis_.keys.size();
        Check();
        redis_.Reply(type); // STRING 的长度为零，依然是已有键。
        EXPECT_EQ(redis_.keys.size(), count + 1);
        EXPECT_TRUE(redis_.pending.empty());
        ExpectNoRegroup(player_);
    }

    const auto count = redis_.keys.size();
    Check();
    redis_.Disconnect();
    EXPECT_EQ(redis_.keys.size(), count + 1);
    redis_.rejectNext = true;
    Check();
    EXPECT_EQ(redis_.keys.size(), count + 2);
    EXPECT_TRUE(redis_.pending.empty());
    ExpectNoRegroup(player_);
}

TEST(PlayerTeamFollowDiscoveryTest, TeamAuthorityUsesTheGlobalServiceRegistry)
{
    const auto type = NodeUtils::GetServiceTypeFromPrefix("TeamNodeService.rpc/zone/1/node_type/10/node_id/1");
    EXPECT_EQ(type, eNodeType::TeamNodeService);
    EXPECT_FALSE(NodeUtils::IsZoneScopedNodeType(type));
}

TEST_F(PlayerTeamFollowTest, RedisNotReadyBeforeSecondGetDoesNotSubmitIt)
{
    Check();
    redis_.ready = false;
    redis_.Reply(REDIS_REPLY_NIL);
    EXPECT_EQ(redis_.keys.size(), 1u);
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, HeldRepliesLeaveLoopFreeAndRegroupOnOwningThread)
{
    const auto owner = std::this_thread::get_id();
    const auto finished = std::make_shared<bool>(false);
    loop_.queueInLoop([&loop = loop_, &redis = redis_, player = player_, owner, finished]
    {
        PlayerTeamFollowTestAccess::Check(player, kLeaderId);
        ASSERT_EQ(redis.keys, (std::vector<std::string>{"battle:lock:{950200001}"}));
        loop.queueInLoop([&loop, &redis, player = player, owner, finished]
        {
            EXPECT_EQ(std::this_thread::get_id(), owner);
            EXPECT_EQ(redis.pending.size(), 1u);
            EXPECT_EQ(redis.keys.size(), 1u);
            ExpectNoRegroup(player);

            redis.Reply(REDIS_REPLY_NIL);
            ASSERT_EQ(redis.keys, (std::vector<std::string>{"battle:lock:{950200001}", "player:{950200002}:location"}));
            EXPECT_EQ(redis.threads, (std::vector<std::thread::id>{owner, owner}));
            loop.queueInLoop([&redis, player = player, owner, finished]
            {
                EXPECT_EQ(std::this_thread::get_id(), owner);
                EXPECT_EQ(redis.pending.size(), 1u);
                ExpectNoRegroup(player);
                redis.Reply(REDIS_REPLY_STRING, LeaderLocation());

                // 回调返回前已继续业务。瞬移写入当前线程所属的真实 ECS。
                const auto& transform = tlsEcs.actorRegistry.get<Transform>(player);
                EXPECT_DOUBLE_EQ(transform.location().x(), kLeaderX);
                EXPECT_DOUBLE_EQ(transform.rotation().y(), kLeaderX);
                EXPECT_TRUE(tlsEcs.actorRegistry.any_of<TeleportSettleComp>(player));
                *finished = true;
            });
        });
    });
    loop_.wakeup();
    EXPECT_TRUE(muduo_grpc_test::RunLoopUntil(loop_, [finished] { return *finished; }, 5s));
}

TEST_F(PlayerTeamFollowTest, MissingMalformedOrRemoteLeaderLocationDoesNotMovePlayer)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    const std::vector<std::pair<int, std::string>> replies{
        {REDIS_REPLY_NIL, {}}, {REDIS_REPLY_ERROR, {}}, {REDIS_REPLY_STRING, {}},
        {REDIS_REPLY_STRING, std::string(1, '\x80')}, {REDIS_REPLY_STRING, LeaderLocation(0)},
        {REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId, kZoneId + 1)},
        {REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId, kZoneId, kSceneNodeId + 1)},
    };
    for (const auto& [type, bytes] : replies)
    {
        Check();
        redis_.Reply(REDIS_REPLY_NIL);
        redis_.Reply(type, bytes);
        ExpectNoRegroup(player_);
    }
    Check();
    redis_.Reply(REDIS_REPLY_NIL);
    redis_.Disconnect();
    ExpectNoRegroup(player_);

    Check();
    redis_.rejectNext = true;
    redis_.Reply(REDIS_REPLY_NIL);
    EXPECT_TRUE(redis_.pending.empty());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, ReusedEntitySlotDoesNotContinueAfterFirstGet)
{
    Check();
    const auto original = player_;
    tlsEcs.actorRegistry.destroy(original);
    tlsEcs.playerList.erase(kPlayerId);
    const auto replacement = MakePlayer(kPlayerId + 10, 0.0);
    BindSession(replacement, kPlayerId + 10, SessionOnGate(kOriginalGateId));
    ASSERT_EQ(entt::to_entity(original), entt::to_entity(replacement));
    ASSERT_NE(original, replacement);
    redis_.Reply(REDIS_REPLY_NIL);
    EXPECT_EQ(redis_.keys.size(), 1u);
    ExpectNoRegroup(replacement);
}

TEST_F(PlayerTeamFollowTest, ChangedPlayerIdDoesNotContinueAfterSecondGet)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    // 保留原会话索引，确保本例由第二跳的实体身份复核挡住。
    tlsEcs.actorRegistry.get<PlayerGuidComp>(player_).value = kPlayerId + 10;
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, BattleStartedWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    tlsEcs.actorRegistry.emplace<InBattleComp>(player_);
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, DisconnectedWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId));
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, OwnershipHandoffWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
    tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player_);
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, SceneChangeWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    auto& pending = tlsEcs.actorRegistry.emplace<PlayerSceneChangeInFlightComp>(player_);
    pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
    pending.correlationId = 33;
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeleportSettleComp>(player_));
    const auto* unchanged = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player_);
    ASSERT_NE(unchanged, nullptr);
    EXPECT_EQ(unchanged->correlationId, 33u);
}

TEST_F(PlayerTeamFollowTest, DifferentSceneSendsCorrelatedRequestForCurrentGateSession)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());

    const auto session = SessionOnGate(kCurrentGateId);
    BindSession(player_, kPlayerId, session);
    tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(player_, session, 29u);
    tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(player_, 29u, 7u);
    auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
    auto& gate = gateRegistry.emplace<NodeInfo>(gateRegistry.create());
    gate.set_zone_id(kZoneId);
    gate.set_node_id(kCurrentGateId);
    gate.set_node_uuid("gate-after-rebind");

    constexpr uint64_t kDestination = kCurrentSceneId + 1;
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation(kDestination));
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player_);
    ASSERT_NE(pending, nullptr);
    EXPECT_FALSE(pending->playerRequested);
    EXPECT_EQ(pending->sceneId, kDestination);
    const auto correlationId = pending->correlationId;
    EXPECT_NE(correlationId, 0u);

    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop_, [requests = sceneManager_.requests, player = player_]
    {
        return !requests->empty() &&
               !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    ASSERT_EQ(sceneManager_.requests->size(), 1u);
    const auto& request = sceneManager_.requests->front();
    EXPECT_EQ(request.player_id(), kPlayerId);
    EXPECT_EQ(request.scene_id(), kDestination);
    EXPECT_EQ(request.session_id(), session);
    EXPECT_EQ(request.gate_id(), std::to_string(kCurrentGateId));
    EXPECT_EQ(request.gate_instance_id(), "gate-after-rebind");
    EXPECT_EQ(request.zone_id(), kZoneId);
    EXPECT_EQ(request.gate_zone_id(), kZoneId);
    EXPECT_EQ(request.login_generation(), 29u);
    EXPECT_EQ(request.owner_revision(), 7u);
    EXPECT_EQ(request.correlation_id(), correlationId);
    EXPECT_TRUE(request.request_id().empty());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, StoppingRejectsNewChecksAndLateReplyAfterScopeDestruction)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    auto late = redis_.TakeReply();
    tasks_->Stop();
    Check();
    ASSERT_EQ(redis_.keys.size(), 2u);
    tasks_.reset();
    Deliver(std::move(late), REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, CompletedRegroupSurvivesLaterStop)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    tasks_->Stop();
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player_).location().x(), kLeaderX);
    EXPECT_EQ(redis_.keys.size(), 2u);
}

TEST_F(PlayerTeamFollowTest, InlineRepliesCompleteBothGetsOnce)
{
    redis_.onSubmit = [&redis = redis_](RedisReplyHandler callback)
    {
        redis.onSubmit = [](RedisReplyHandler location)
        {
            Deliver(std::move(location), REDIS_REPLY_STRING, LeaderLocation());
        };
        Deliver(std::move(callback), REDIS_REPLY_NIL);
    };
    Check();
    EXPECT_EQ(redis_.keys.size(), 2u);
    EXPECT_TRUE(redis_.pending.empty());
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player_).location().x(), kLeaderX);
}

TEST_F(PlayerTeamFollowTest, StopReenteredAfterInlineReplyPreventsSecondGet)
{
    redis_.onSubmit = [&scope = *tasks_](RedisReplyHandler callback)
    {
        Deliver(std::move(callback), REDIS_REPLY_NIL);
        scope.Stop();
    };
    Check();
    EXPECT_EQ(redis_.keys.size(), 1u);
    EXPECT_TRUE(redis_.pending.empty());
    ExpectNoRegroup(player_);
}

TEST_F(PlayerTeamFollowTest, StopReenteredBeforeInlineReplyPreventsSecondGet)
{
    redis_.onSubmit = [&scope = *tasks_](RedisReplyHandler callback)
    {
        scope.Stop();
        Deliver(std::move(callback), REDIS_REPLY_NIL);
    };
    Check();
    EXPECT_EQ(redis_.keys.size(), 1u);
    EXPECT_TRUE(redis_.pending.empty());
    ExpectNoRegroup(player_);
}

// ---- 服务器驱动跟随:离线的队员也跟(docs/design/team-follow-server-trail.md §3.6) ----

void JoinTeam(entt::entity player)
{
    auto& team = tlsEcs.actorRegistry.emplace_or_replace<TeamId>(player);
    team.set_team_id(kTeamId);
    team.set_membership_epoch(1);
}

TEST_F(PlayerTeamFollowTest, OfflineMemberWhoDeclaredFollowIsAttachedInTheSameScene)
{
    JoinTeam(player_);
    JoinTeam(tlsEcs.GetPlayer(kLeaderId));
    TeamFollowSystem::SetFollowIntent(player_, true);
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId)); // 断线:同场景里的服务器驱动跟随不看会话
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation());
    EXPECT_TRUE(TeamFollowSystem::IsDriven(player_)) << "离线也要接到队长身后";
    // 离队长 20,超过归队阈值,直接放进槽位;轨迹刚建、只有队长一个点,槽位就在队长脚下。
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player_).location().x(), kLeaderX);
}

TEST_F(PlayerTeamFollowTest, OfflineMemberDoesNotAskSceneManagerToChangeScene)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    JoinTeam(player_);
    TeamFollowSystem::SetFollowIntent(player_, true);
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId));
    // 队长在别的场景:换场景要经 SceneManager 改 gate 路由,离线的人没有活会话,不发。
    redis_.Reply(REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId + 1));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player_));
    EXPECT_FALSE(TeamFollowSystem::IsDriven(player_));
    ExpectNoRegroup(player_);
}

// ECS 层:直接驱动 TeamFollowSystem::Update,不经 Redis / RPC。
class TeamFollowDriveTest : public ::testing::Test
{
protected:
    void SetUp() override
    {
        tlsEcs.Clear();
        scene_ = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene_).set_scene_id(kCurrentSceneId);
        leader_ = MakeMember(kLeaderId, 0.0, 1);
        follower_ = MakeMember(kPlayerId, -1.0, 2);
        TeamFollowSystem::SetFollowIntent(follower_, true);
        ASSERT_TRUE(TeamFollowSystem::TryAttach(follower_, leader_, /*walkAllowed=*/true));
    }

    void TearDown() override
    {
        tlsEcs.Clear();
    }

    entt::entity MakeMember(uint64_t id, double x, uint32_t joinSeq)
    {
        auto& registry = tlsEcs.actorRegistry;
        const auto player = registry.create();
        registry.emplace<PlayerGuidComp>(player, id);
        registry.emplace<SceneEntityComp>(player, scene_);
        registry.emplace<Transform>(player).mutable_location()->set_x(x);
        JoinTeam(player);
        TeamFollowSystem::RememberJoinSeq(player, joinSeq);
        tlsEcs.playerList.emplace(id, player);
        return player;
    }

    // 队长沿 +x 以每 tick 0.45(9 米/秒)走到 toX,每步跑一次跟随系统。
    void WalkLeader(double toX)
    {
        auto& location = *tlsEcs.actorRegistry.get<Transform>(leader_).mutable_location();
        while (location.x() < toX - 1e-9)
        {
            location.set_x(std::min(toX, location.x() + 0.45));
            TeamFollowSystem::Update(0.05);
        }
    }

    void Stand(int ticks)
    {
        for (int i = 0; i < ticks; ++i)
        {
            TeamFollowSystem::Update(0.05);
        }
    }

    static double X(entt::entity player)
    {
        return tlsEcs.actorRegistry.get<Transform>(player).location().x();
    }

    SceneEntity scene_{entt::null};
    entt::entity leader_{entt::null};
    entt::entity follower_{entt::null};
};

TEST_F(TeamFollowDriveTest, OfflineFollowerKeepsWalkingBehindTheLeader)
{
    tlsEcs.actorRegistry.emplace<PlayerConnectionLostComp>(follower_); // 下线进断线保留:不影响服务器驱动
    WalkLeader(9.0);
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower_));
    EXPECT_NEAR(X(follower_), 9.0 - kTeamFollowSpacing, 1e-6) << "离线的队员照样跟在队长身后一个间距";
}

TEST_F(TeamFollowDriveTest, FollowerInBattleWaitsAndCatchesUpAfterwards)
{
    // 停在 WalkLeader 的整步上，避免最后一步落在轨迹采样阈值。
    constexpr double kLeaderStopX = 4.5;
    tlsEcs.actorRegistry.emplace<InBattleComp>(follower_);
    WalkLeader(kLeaderStopX);
    EXPECT_DOUBLE_EQ(X(follower_), -1.0) << "战斗中原地不动";
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower_)) << "名次照留,不摘驱动";

    tlsEcs.actorRegistry.remove<InBattleComp>(follower_);
    Stand(30);
    EXPECT_NEAR(X(follower_), kLeaderStopX - kTeamFollowSpacing, 1e-6) << "战斗结束后沿路赶回自己的槽位";
}

TEST_F(TeamFollowDriveTest, LeavingFollowStopsDrivingAtOnce)
{
    WalkLeader(3.0);
    TeamFollowSystem::SetFollowIntent(follower_, false);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower_));
    const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(follower_);
    ASSERT_NE(velocity, nullptr);
    EXPECT_DOUBLE_EQ(velocity->x(), 0.0) << "停止驱动时清零速度,MovementSystem 不会按队形最后写的速度继续积分";

    const double stoppedAt = X(follower_);
    WalkLeader(6.0);
    EXPECT_DOUBLE_EQ(X(follower_), stoppedAt) << "暂离之后不再被带着走";
}

TEST_F(TeamFollowDriveTest, LeaderLeavingTheTeamReleasesTheFollowerOnTheNextTick)
{
    WalkLeader(2.0);
    tlsEcs.actorRegistry.get<TeamId>(leader_).set_team_id(0);
    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower_));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowFormationComp>(leader_));
}
} // namespace
