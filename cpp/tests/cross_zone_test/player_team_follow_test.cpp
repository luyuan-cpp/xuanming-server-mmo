#include <algorithm>
#include <chrono>
#include <cstdint>
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
#include "engine/core/metrics/prometheus_text.h" // 跟随事件计数的 /metrics 导出(TeamFollowSystem::WriteMetrics)
#include "engine/core/type_define/player_guid_comp.h"
#include "frame/manager/frame_time.h"
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
#include "proto/common/component/player_comp.pb.h"
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

    static void SetLeadership(entt::entity player, bool isLeader)
    {
        PlayerTeamSystem::SetLeadershipSnapshotForTest(player, isLeader);
    }

    static void ApplyMembership(entt::entity player, uint64_t teamId, uint64_t epoch)
    {
        PlayerTeamSystem::ApplyMembership(player, teamId, epoch);
    }

    static void SetDefaultMainCity(uint32_t sceneConfigId)
    {
        PlayerTeamSystem::SetDefaultMainCityForTest(sceneConfigId);
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
constexpr double kLeaderX = 30.0; // 超过归队阈值 kTeamRegroupDistance(24):旧规则会瞬移,新规则直接放进槽位
constexpr uint64_t kTeamId = 950200100;

SessionId SessionOnGate(uint32_t gateId)
{
    return (gateId << SessionIdGenerator::sequence_bit_count()) | 1;
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
        scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(kCurrentSceneId);
        player = MakePlayer(kPlayerId, 0.0);
        MakePlayer(kLeaderId, kLeaderX);
        BindSession(player, kPlayerId, SessionOnGate(kOriginalGateId));
        scope.emplace(loop, [&redis = this->redis] { return redis.ready; },
            [&redis = this->redis](std::string_view key, RedisReplyHandler callback)
            {
                return redis.Get(key, std::move(callback));
            });
    }

    void TearDown() override
    {
        tasks.reset();
        scope.reset();
        tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService).clear();
        tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService).clear();
        server.reset();
        // 通道与服务端已在 I/O 派发栈外释放。处理它们排队的连接拆除以及任务完成通知。
        this->loop.queueInLoop([&loop = this->loop] { loop.queueInLoop([&loop] { loop.quit(); }); });
        this->loop.wakeup();
        this->loop.loop();
        tlsEcs.Clear();
    }

    entt::entity MakePlayer(uint64_t id, double x)
    {
        auto& registry = tlsEcs.actorRegistry;
        const auto player = registry.create();
        registry.emplace<PlayerGuidComp>(player, id);
        registry.emplace<SceneEntityComp>(player, scene);
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
        PlayerTeamFollowTestAccess::Check(player, kLeaderId);
    }

    void OpenLocationRead()
    {
        Check();
        ASSERT_EQ(redis.pending.size(), 1u);
        redis.Reply(REDIS_REPLY_NIL);
        ASSERT_EQ(redis.pending.size(), 1u);
        ASSERT_EQ(redis.keys.size(), 2u);
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
        server = muduo_grpc::Server::Create(loop, "127.0.0.1", port, std::move(serverOptions));
        ASSERT_NE(server, nullptr);
        ASSERT_TRUE(server->RegisterService(sceneManager));
        server->Start();

        muduo_grpc::ChannelOptions channelOptions;
        channelOptions.maintenance = std::make_shared<muduo_grpc::MaintenanceState>();
        auto channel = muduo_grpc::Channel::Create(loop, "127.0.0.1", port, std::move(channelOptions));
        ASSERT_NE(channel, nullptr);
        auto& registry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);
        const auto entity = registry.create();
        registry.emplace<NodeInfo>(entity);
        registry.emplace<MuduoGrpcChannelPtr>(entity, std::move(channel));
    }

    muduo::net::EventLoop loop;
    FakeRedis redis;
    std::optional<PlayerTeamFollowScope> scope;
    std::optional<SceneTaskScope> tasks{std::in_place, loop};
    RecordingSceneManager sceneManager;
    std::shared_ptr<muduo_grpc::Server> server;
    SceneEntity scene{entt::null};
    entt::entity player{entt::null};
};

TEST_F(PlayerTeamFollowTest, FirstGetRequiresExplicitNil)
{
    for (const auto type : {REDIS_REPLY_STRING, REDIS_REPLY_ERROR, REDIS_REPLY_INTEGER})
    {
        SCOPED_TRACE(type);
        const auto count = redis.keys.size();
        Check();
        redis.Reply(type); // STRING 的长度为零，依然是已有键。
        EXPECT_EQ(redis.keys.size(), count + 1);
        EXPECT_TRUE(redis.pending.empty());
        ExpectNoRegroup(player);
    }

    const auto count = redis.keys.size();
    Check();
    redis.Disconnect();
    EXPECT_EQ(redis.keys.size(), count + 1);
    redis.rejectNext = true;
    Check();
    EXPECT_EQ(redis.keys.size(), count + 2);
    EXPECT_TRUE(redis.pending.empty());
    ExpectNoRegroup(player);
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
    redis.ready = false;
    redis.Reply(REDIS_REPLY_NIL);
    EXPECT_EQ(redis.keys.size(), 1u);
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, HeldRepliesLeaveLoopFreeAndRegroupOnOwningThread)
{
    const auto owner = std::this_thread::get_id();
    const auto finished = std::make_shared<bool>(false);
    this->loop.queueInLoop([&loop = this->loop, &redis = this->redis, player = this->player, owner, finished]
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
    this->loop.wakeup();
    EXPECT_TRUE(muduo_grpc_test::RunLoopUntil(this->loop, [finished] { return *finished; }, 5s));
}

TEST_F(PlayerTeamFollowTest, MissingMalformedOrRemoteLeaderLocationDoesNotMovePlayer)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    // "远端"只剩别的 zone:队长在本 zone 别的节点上时照样跟(2026-11-10 起,team-follow-server-trail.md §3.7),
    // 见下面的 LeaderOnAnotherNodeOfTheZoneIsFollowedBySceneChange。
    const std::vector<std::pair<int, std::string>> replies{
        {REDIS_REPLY_NIL, {}}, {REDIS_REPLY_ERROR, {}}, {REDIS_REPLY_STRING, {}},
        {REDIS_REPLY_STRING, std::string(1, '\x80')}, {REDIS_REPLY_STRING, LeaderLocation(0)},
        {REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId, kZoneId + 1)},
    };
    for (const auto& [type, bytes] : replies)
    {
        Check();
        redis.Reply(REDIS_REPLY_NIL);
        redis.Reply(type, bytes);
        ExpectNoRegroup(player);
    }
    Check();
    redis.Reply(REDIS_REPLY_NIL);
    redis.Disconnect();
    ExpectNoRegroup(player);

    Check();
    redis.rejectNext = true;
    redis.Reply(REDIS_REPLY_NIL);
    EXPECT_TRUE(redis.pending.empty());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, ReusedEntitySlotDoesNotContinueAfterFirstGet)
{
    Check();
    const auto original = player;
    tlsEcs.actorRegistry.destroy(original);
    tlsEcs.playerList.erase(kPlayerId);
    const auto replacement = MakePlayer(kPlayerId + 10, 0.0);
    BindSession(replacement, kPlayerId + 10, SessionOnGate(kOriginalGateId));
    ASSERT_EQ(entt::to_entity(original), entt::to_entity(replacement));
    ASSERT_NE(original, replacement);
    redis.Reply(REDIS_REPLY_NIL);
    EXPECT_EQ(redis.keys.size(), 1u);
    ExpectNoRegroup(replacement);
}

TEST_F(PlayerTeamFollowTest, ChangedPlayerIdDoesNotContinueAfterSecondGet)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    // 保留原会话索引，确保本例由第二跳的实体身份复核挡住。
    tlsEcs.actorRegistry.get<PlayerGuidComp>(player).value = kPlayerId + 10;
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, BattleStartedWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, DisconnectedWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId));
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, OwnershipHandoffWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player);
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, SceneChangeWhileLocationPendingPreventsFollow)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    auto& pending = tlsEcs.actorRegistry.emplace<PlayerSceneChangeInFlightComp>(player);
    pending.sentAtMs = TimeSystem::NowMillisecondsUTC();
    pending.correlationId = 33;
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeleportSettleComp>(player));
    const auto* unchanged = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(player);
    ASSERT_NE(unchanged, nullptr);
    EXPECT_EQ(unchanged->correlationId, 33u);
}

TEST_F(PlayerTeamFollowTest, DifferentSceneSendsCorrelatedRequestForCurrentGateSession)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());

    const auto session = SessionOnGate(kCurrentGateId);
    BindSession(this->player, kPlayerId, session);
    tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(this->player, session, 29u);
    tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(this->player, 29u, 7u);
    auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
    auto& gate = gateRegistry.emplace<NodeInfo>(gateRegistry.create());
    gate.set_zone_id(kZoneId);
    gate.set_node_id(kCurrentGateId);
    gate.set_node_uuid("gate-after-rebind");

    constexpr uint64_t kDestination = kCurrentSceneId + 1;
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kDestination));
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(this->player);
    ASSERT_NE(pending, nullptr);
    EXPECT_FALSE(pending->playerRequested);
    EXPECT_EQ(pending->sceneId, kDestination);
    const auto correlationId = pending->correlationId;
    EXPECT_NE(correlationId, 0u);

    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [requests = sceneManager.requests, player = this->player]
    {
        return !requests->empty() &&
               !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    ASSERT_EQ(sceneManager.requests->size(), 1u);
    const auto& request = sceneManager.requests->front();
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
    ExpectNoRegroup(this->player);
}

TEST_F(PlayerTeamFollowTest, DifferentSceneRequestNamesTheGateProcessThatHoldsTheSession)
{
    // 同一 node_id 短暂有两个 gate 进程、两者在本节点上都有实体时,跟随发出的 EnterScene 带会话绑定时记下的那个进程
    // (scene-enter-chain-residuals.md §14.6);旧代码按 node_id 取第一个匹配的实体,带的可能是另一个进程。
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());

    const auto session = SessionOnGate(kCurrentGateId);
    BindSession(this->player, kPlayerId, session);
    auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
    for (const char* instance : {"gate-old-process", "gate-new-process"})
    {
        auto& gate = gateRegistry.emplace<NodeInfo>(gateRegistry.create());
        gate.set_zone_id(kZoneId);
        gate.set_node_id(kCurrentGateId);
        gate.set_node_uuid(instance);
    }
    // 绑在按 node_id 先找到的那个以外的进程上:不论实体遍历顺序,旧代码都带错。
    const auto byNodeId = ResolveLocalZoneGateEntity(session);
    ASSERT_TRUE(byNodeId.has_value());
    const std::string boundInstance =
        gateRegistry.get<NodeInfo>(*byNodeId).node_uuid() == "gate-old-process" ? "gate-new-process" : "gate-old-process";
    tlsEcs.actorRegistry.emplace<PlayerLoginIdentityComp>(this->player, session, 29u, boundInstance);
    tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(this->player, 29u, 7u);

    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId + 1));
    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [requests = sceneManager.requests, player = this->player]
    {
        return !requests->empty() &&
               !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    ASSERT_EQ(sceneManager.requests->size(), 1u);
    const auto& request = sceneManager.requests->front();
    EXPECT_EQ(request.session_id(), session);
    EXPECT_EQ(request.gate_id(), std::to_string(kCurrentGateId));
    EXPECT_EQ(request.gate_instance_id(), boundInstance) << "变异红:带的是会话所在的进程,不是按 node_id 先找到的那个";
}

TEST_F(PlayerTeamFollowTest, StoppingRejectsNewChecksAndLateReplyAfterScopeDestruction)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    auto late = redis.TakeReply();
    tasks->Stop();
    Check();
    ASSERT_EQ(redis.keys.size(), 2u);
    tasks.reset();
    Deliver(std::move(late), REDIS_REPLY_STRING, LeaderLocation());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, CompletedRegroupSurvivesLaterStop)
{
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    tasks->Stop();
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player).location().x(), kLeaderX);
    EXPECT_EQ(redis.keys.size(), 2u);
}

TEST_F(PlayerTeamFollowTest, InlineRepliesCompleteBothGetsOnce)
{
    this->redis.onSubmit = [&redis = this->redis](RedisReplyHandler callback)
    {
        redis.onSubmit = [](RedisReplyHandler location)
        {
            Deliver(std::move(location), REDIS_REPLY_STRING, LeaderLocation());
        };
        Deliver(std::move(callback), REDIS_REPLY_NIL);
    };
    Check();
    EXPECT_EQ(this->redis.keys.size(), 2u);
    EXPECT_TRUE(this->redis.pending.empty());
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player).location().x(), kLeaderX);
}

TEST_F(PlayerTeamFollowTest, StopReenteredAfterInlineReplyPreventsSecondGet)
{
    redis.onSubmit = [&scope = *tasks](RedisReplyHandler callback)
    {
        Deliver(std::move(callback), REDIS_REPLY_NIL);
        scope.Stop();
    };
    Check();
    EXPECT_EQ(redis.keys.size(), 1u);
    EXPECT_TRUE(redis.pending.empty());
    ExpectNoRegroup(player);
}

TEST_F(PlayerTeamFollowTest, StopReenteredBeforeInlineReplyPreventsSecondGet)
{
    redis.onSubmit = [&scope = *tasks](RedisReplyHandler callback)
    {
        scope.Stop();
        Deliver(std::move(callback), REDIS_REPLY_NIL);
    };
    Check();
    EXPECT_EQ(redis.keys.size(), 1u);
    EXPECT_TRUE(redis.pending.empty());
    ExpectNoRegroup(player);
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
    JoinTeam(player);
    JoinTeam(tlsEcs.GetPlayer(kLeaderId));
    TeamFollowSystem::SetFollowIntent(player, true);
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId)); // 断线:同场景里的服务器驱动跟随不看会话
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation());
    EXPECT_TRUE(TeamFollowSystem::IsDriven(player)) << "离线也要接到队长身后";
    // 离队长 30,超过归队阈值,直接放进槽位;轨迹刚建、只有队长一个点,槽位就在队长脚下。
    EXPECT_DOUBLE_EQ(tlsEcs.actorRegistry.get<Transform>(player).location().x(), kLeaderX);
}

TEST_F(PlayerTeamFollowTest, OfflineMemberDoesNotAskSceneManagerToChangeScene)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    JoinTeam(player);
    TeamFollowSystem::SetFollowIntent(player, true);
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    SessionMap().erase(SessionOnGate(kOriginalGateId));
    // 队长在别的场景:换场景要经 SceneManager 改 gate 路由,离线的人没有活会话,不发。
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId + 1));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    EXPECT_FALSE(TeamFollowSystem::IsDriven(player));
    ExpectNoRegroup(player);
}

// ---- 跨节点跟随与离线托管的托管落点(docs/design/team-follow-server-trail.md §3.7) ----

// 进断线保留并确认挂机月卡有效(离线托管中):快照会话置无效、擦掉映射,保留组件记断开的那次登录。
void HoldAsHostedOffline(entt::entity player, SessionId lostSession, uint64_t lostGeneration)
{
    auto& snapshot = tlsEcs.actorRegistry.get_or_emplace<PlayerSessionSnapshotComp>(player);
    SessionMap().erase(snapshot.gate_session_id());
    snapshot.set_gate_session_id(kInvalidSessionId);
    auto& held = tlsEcs.actorRegistry.emplace_or_replace<PlayerConnectionLostComp>(player);
    held.lostAt = connection_lost_hold::Clock::now();
    held.sessionId = lostSession;
    held.loginGeneration = lostGeneration;
    held.afkPass = connection_lost_hold::AfkPass::kActive;
    held.afkPassExpiresAtSec = TimeSystem::NowSecondsUTC() + 3600;
    held.hostingStarted = true;
}

TEST_F(PlayerTeamFollowTest, LeaderOnAnotherNodeOfTheZoneIsFollowedBySceneChange)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    constexpr uint64_t kDestination = kCurrentSceneId + 1;
    // 队长在本 zone 的另一个节点:照样发换场景(在线,带 gate);scene_manager 回 18 之后起服务器发起的交接。
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kDestination, kZoneId, kSceneNodeId + 1));
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(this->player);
    ASSERT_NE(pending, nullptr) << "不再因为队长在别的节点就跳过";
    EXPECT_FALSE(pending->playerRequested);
    EXPECT_EQ(pending->sceneId, kDestination);

    // 夹具的 scene_manager 回普通拒绝(5):跟随的应答只记日志、摘掉在途记录。
    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [requests = sceneManager.requests, player = this->player]
    {
        return !requests->empty() && !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    const auto& request = sceneManager.requests->front();
    EXPECT_EQ(request.scene_id(), kDestination);
    EXPECT_EQ(request.session_id(), SessionOnGate(kOriginalGateId));
    EXPECT_EQ(request.gate_id(), std::to_string(kOriginalGateId));
    EXPECT_FALSE(request.hosted_placement()) << "在线的队员走 gate 路由";
    EXPECT_EQ(request.zone_id(), kZoneId);
    ExpectNoRegroup(this->player);
}

TEST_F(PlayerTeamFollowTest, HostedOfflineMemberFollowsWithAHostedPlacement)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    JoinTeam(this->player);
    tlsEcs.actorRegistry.emplace<PlayerRoleAdmissionComp>(this->player, 29u, 7u);
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    constexpr uint64_t kLostGeneration = 41;
    const auto lostSession = SessionOnGate(kOriginalGateId);
    // 位置读在途时玩家掉线、确认开通了挂机(客户端已关):队长在另一个节点的场景,照样跟过去。
    HoldAsHostedOffline(this->player, lostSession, kLostGeneration);
    constexpr uint64_t kDestination = kCurrentSceneId + 1;
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kDestination, kZoneId, kSceneNodeId + 1));
    const auto* pending = tlsEcs.actorRegistry.try_get<PlayerSceneChangeInFlightComp>(this->player);
    ASSERT_NE(pending, nullptr);
    EXPECT_FALSE(pending->playerRequested);

    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [requests = sceneManager.requests, player = this->player]
    {
        return !requests->empty() && !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    const auto& request = sceneManager.requests->front();
    EXPECT_TRUE(request.hosted_placement()) << "没有客户端:托管落点";
    EXPECT_TRUE(request.gate_id().empty()) << "不带 gate:没有可推路由的会话";
    EXPECT_TRUE(request.gate_instance_id().empty());
    EXPECT_EQ(request.session_id(), lostSession) << "会话号只作登录身份:断开的那次登录";
    EXPECT_EQ(request.login_generation(), kLostGeneration);
    EXPECT_EQ(request.scene_id(), kDestination);
    EXPECT_EQ(request.zone_id(), kZoneId);
    EXPECT_EQ(request.gate_zone_id(), kZoneId);
    EXPECT_EQ(request.owner_revision(), 7u);
    EXPECT_NE(request.correlation_id(), 0u);
    ExpectNoRegroup(this->player);
}

TEST_F(PlayerTeamFollowTest, OfflineMemberWithoutConfirmedHostingDoesNotChangeScene)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    // 普通断线保留(还不知道有没有月卡):25 秒窗口建立在 login 断线租约之上,不搬到别处去。
    HoldAsHostedOffline(player, SessionOnGate(kOriginalGateId), 41);
    auto& held = tlsEcs.actorRegistry.get<PlayerConnectionLostComp>(player);
    held.hostingStarted = false;
    held.afkPass = connection_lost_hold::AfkPass::kUnknown;
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId + 1, kZoneId, kSceneNodeId + 1));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player));
    ExpectNoRegroup(player);
}

// ECS 层:直接驱动 TeamFollowSystem::Update,不经 Redis / RPC。
class TeamFollowDriveTest : public ::testing::Test
{
protected:
    void SetUp() override
    {
        tlsEcs.Clear();
        scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(kCurrentSceneId);
        leader = MakeMember(kLeaderId, 0.0, 1);
        follower = MakeMember(kPlayerId, -1.0, 2);
        TeamFollowSystem::SetFollowIntent(follower, true);
        ASSERT_TRUE(TeamFollowSystem::TryAttach(follower, leader, /*walkAllowed=*/true));
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
        registry.emplace<SceneEntityComp>(player, scene);
        registry.emplace<Transform>(player).mutable_location()->set_x(x);
        JoinTeam(player);
        TeamFollowSystem::RememberJoinSeq(player, joinSeq);
        tlsEcs.playerList.emplace(id, player);
        return player;
    }

    // 队长沿 +x 以每 tick 0.45(9 米/秒)走到 toX,每步跑一次跟随系统。
    void WalkLeader(double toX)
    {
        auto& location = *tlsEcs.actorRegistry.get<Transform>(leader).mutable_location();
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

    SceneEntity scene{entt::null};
    entt::entity leader{entt::null};
    entt::entity follower{entt::null};
};

TEST_F(TeamFollowDriveTest, OfflineFollowerKeepsWalkingBehindTheLeader)
{
    tlsEcs.actorRegistry.emplace<PlayerConnectionLostComp>(follower); // 下线进断线保留:不影响服务器驱动
    WalkLeader(9.0);
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower));
    EXPECT_NEAR(X(follower), 9.0 - kTeamFollowSpacing, 1e-6) << "离线的队员照样跟在队长身后一个间距";
}

// 朝向口径(spatial/system/facing.h):被驱动的队员面朝行进方向,Rotation.z 是偏航角(度),x、y 恒 0。
TEST_F(TeamFollowDriveTest, DrivenFollowerFacesItsWalkingDirectionInDegrees)
{
    WalkLeader(4.5);
    auto& leaderLocation = *tlsEcs.actorRegistry.get<Transform>(leader).mutable_location();
    for (int tick = 0; tick < 20; ++tick) // 在 x = 4.5 拐向 +y,走 9 米
    {
        leaderLocation.set_y(leaderLocation.y() + 0.45);
        TeamFollowSystem::Update(0.05);
    }
    const auto& follower = tlsEcs.actorRegistry.get<Transform>(this->follower);
    EXPECT_NEAR(follower.location().x(), 4.5, 1e-6) << "已经过了拐角,沿 +y 走";
    EXPECT_NEAR(follower.rotation().z(), 90.0, 1e-6) << "偏航 90 度,不是弧度 π/2";
    EXPECT_EQ(follower.rotation().x(), 0.0);
    EXPECT_EQ(follower.rotation().y(), 0.0);
}

TEST_F(TeamFollowDriveTest, FollowerInBattleWaitsAndCatchesUpAfterwards)
{
    // 停在 WalkLeader 的整步上，避免最后一步落在轨迹采样阈值。
    constexpr double kLeaderStopX = 4.5;
    tlsEcs.actorRegistry.emplace<InBattleComp>(follower);
    WalkLeader(kLeaderStopX);
    EXPECT_DOUBLE_EQ(X(follower), -1.0) << "战斗中原地不动";
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower)) << "名次照留,不摘驱动";

    tlsEcs.actorRegistry.remove<InBattleComp>(follower);
    Stand(30);
    EXPECT_NEAR(X(follower), kLeaderStopX - kTeamFollowSpacing, 1e-6) << "战斗结束后沿路赶回自己的槽位";
}

TEST_F(TeamFollowDriveTest, LeavingFollowStopsDrivingAtOnce)
{
    // 多走一个间距:队员在 -1,槽位要越过他才开始被带着走,暂离时他得正在走。
    WalkLeader(kTeamFollowSpacing + 3.0);
    TeamFollowSystem::SetFollowIntent(follower, false);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));
    const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(follower);
    ASSERT_NE(velocity, nullptr);
    EXPECT_DOUBLE_EQ(velocity->x(), 0.0) << "停止驱动时清零速度,MovementSystem 不会按队形最后写的速度继续积分";

    const double stoppedAt = X(follower);
    WalkLeader(kTeamFollowSpacing + 6.0);
    EXPECT_DOUBLE_EQ(X(follower), stoppedAt) << "暂离之后不再被带着走";
}

TEST_F(TeamFollowDriveTest, LeaderLeavingTheTeamReleasesTheFollowerOnTheNextTick)
{
    WalkLeader(2.0);
    tlsEcs.actorRegistry.get<TeamId>(leader).set_team_id(0);
    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowFormationComp>(leader));
}

TEST_F(TeamFollowDriveTest, ResendAfterEntryLeavesTheFormationUntouched)
{
    WalkLeader(3.0);
    const double before = X(follower);
    // 接回会话后补推一次状态(测试里没有可投递的网关会话,不发),不能顺带改动队形或驱动。
    TeamFollowSystem::ResendStateAfterEntry(follower);
    TeamFollowSystem::ResendStateAfterEntry(leader); // 队长不是任何人的随从:什么都不做
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower));
    EXPECT_FALSE(TeamFollowSystem::IsDriven(leader));
    EXPECT_DOUBLE_EQ(X(follower), before);

    WalkLeader(6.0);
    EXPECT_NEAR(X(follower), 6.0 - kTeamFollowSpacing, 1e-6) << "补推之后照常被带着走";
}

// 入场后补推的判据(ResendStateAfterEntry 只在 DrivenSlot 有值时发 driven=true,名次就是它)。
TEST_F(TeamFollowDriveTest, DrivenSlotIsWhatTheResendAfterEntryReports)
{
    EXPECT_EQ(TeamFollowSystem::DrivenSlot(follower).value_or(0u), 1u) << "唯一的队员排第一";
    EXPECT_FALSE(TeamFollowSystem::DrivenSlot(leader).has_value()) << "队长不被驱动";

    // 队员已进了别的场景实例、还没轮到下一 tick 按换场景停止驱动:队形记的是旧场景,补推不能说他还在跟。
    const auto otherScene = tlsEcs.sceneRegistry.create();
    tlsEcs.actorRegistry.get<SceneEntityComp>(follower).sceneEntity = otherScene;
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower)) << "前置条件:驱动还没摘";
    EXPECT_FALSE(TeamFollowSystem::DrivenSlot(follower).has_value());

    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower)) << "下一 tick 按换场景停止驱动";
    EXPECT_FALSE(TeamFollowSystem::DrivenSlot(follower).has_value());
}

// ---- 队长离开本节点:原随从等队长的去留,有了结论再跟一次(team-follow-server-trail.md §3.7) ----

// 队长开始一次跨节点交接(冻结 + 交接意图):下一 tick 队形以 LEADER_GONE 解散。
void StartLeaderHandoff(entt::entity leader)
{
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(leader);
    tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(leader);
}

TEST_F(TeamFollowDriveTest, LeaderLeavingTheNodeIsAwaitedAndRecheckedOnceItIsGone)
{
    WalkLeader(2.0);
    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower)) << "记下在等队长的去留";
    EXPECT_TRUE(TeamFollowSystem::TakeFollowersWithSettledLeader().empty())
        << "队长还在交接中:location 还指着本节点的旧场景,现在检查只会跟回原地";
    EXPECT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));

    // 交接放行:队长实体在本节点销毁,location 已指向新落点。
    tlsEcs.playerList.erase(kLeaderId);
    tlsEcs.actorRegistry.destroy(leader);
    const auto& settled = TeamFollowSystem::TakeFollowersWithSettledLeader();
    ASSERT_EQ(settled.size(), 1u);
    EXPECT_EQ(settled.front(), follower) << "交给跟随检查,队长去了别的节点就跟过去";
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));
    EXPECT_TRUE(TeamFollowSystem::TakeFollowersWithSettledLeader().empty()) << "只交出一次";
}

TEST_F(TeamFollowDriveTest, AbortedLeaderHandoffSettlesTheWaitingFollower)
{
    WalkLeader(2.0);
    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));

    // 交接作废、原地解冻:队长还在本节点,不再处于离开中 —— 重新检查,接回原来的队长身后。
    tlsEcs.actorRegistry.remove<PlayerFrozenComp, PlayerTravelHandoffComp>(leader);
    const auto& settled = TeamFollowSystem::TakeFollowersWithSettledLeader();
    ASSERT_EQ(settled.size(), 1u);
    EXPECT_EQ(settled.front(), follower);
}

TEST_F(TeamFollowDriveTest, WaitingFollowerWhoLeftFollowIsNotRechecked)
{
    WalkLeader(2.0);
    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));

    TeamFollowSystem::SetFollowIntent(follower, false); // 等的时候点了暂离
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));
    tlsEcs.playerList.erase(kLeaderId);
    tlsEcs.actorRegistry.destroy(leader);
    EXPECT_TRUE(TeamFollowSystem::TakeFollowersWithSettledLeader().empty()) << "暂离中的不再替他跟";
}

TEST_F(TeamFollowDriveTest, WaitingForTheLeaderGivesUpAfterTheTimeout)
{
    WalkLeader(2.0);
    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));

    auto& frameTime = tlsFrameTimeManager.frameTime;
    const auto savedFrame = frameTime.current_frame();
    frameTime.set_current_frame(savedFrame + kTeamFollowAwaitLeaderFrames);
    EXPECT_TRUE(TeamFollowSystem::TakeFollowersWithSettledLeader().empty());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower)) << "等满放弃,交给别的信号";
    tlsEcs.playerList.erase(kLeaderId);
    tlsEcs.actorRegistry.destroy(leader);
    EXPECT_TRUE(TeamFollowSystem::TakeFollowersWithSettledLeader().empty());
    frameTime.set_current_frame(savedFrame);
}

// 托管中的兜底重试(§3.7,2026-11-11):只替"在队伍里、想跟却没接在队长身后"的人再走一遍跟随检查。
TEST_F(TeamFollowDriveTest, OnlyADetachedMemberWhoWantsToFollowNeedsARecheck)
{
    EXPECT_FALSE(PlayerTeamSystem::NeedsFollowRecheck(follower)) << "已接在队长身后:照常被带着走,不发读";
    EXPECT_FALSE(PlayerTeamSystem::NeedsFollowRecheck(leader)) << "队长没有跟随意图";

    TeamFollowSystem::Detach(follower, TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW); // 例如严格导航拒了一步:意图还在
    EXPECT_TRUE(PlayerTeamSystem::NeedsFollowRecheck(follower)) << "想跟却没接上:兜底重试替他再走一遍跟随检查";

    tlsEcs.actorRegistry.get<TeamId>(follower).set_team_id(0);
    EXPECT_FALSE(PlayerTeamSystem::NeedsFollowRecheck(follower)) << "不在队伍里:没有可跟的人";
    tlsEcs.actorRegistry.get<TeamId>(follower).set_team_id(kTeamId);

    TeamFollowSystem::SetFollowIntent(follower, false);
    EXPECT_FALSE(PlayerTeamSystem::NeedsFollowRecheck(follower)) << "暂离:不替他跟";
}

TEST_F(TeamFollowDriveTest, HostedLeaderAndUnknownTeamRecheckRules)
{
    // 已确认是队长:即使托管开始挂了跟随意图(防惰性转让后漏跟),周期心跳也不应对队长发冗余读。
    TeamFollowSystem::SetFollowIntent(leader, true);
    PlayerTeamFollowTestAccess::SetLeadership(leader, true);
    EXPECT_FALSE(PlayerTeamSystem::NeedsFollowRecheck(leader)) << "已确认队长不触发 15s 兜底重查";

    // 队长职务转走后(isLeader=false):立刻允许补查跟随新队长。
    PlayerTeamFollowTestAccess::SetLeadership(leader, false);
    EXPECT_TRUE(PlayerTeamSystem::NeedsFollowRecheck(leader));

    // 跨节点新实体尚未同步 TeamId:挂了意图但缺 TeamId 时仍应补查。
    TeamFollowSystem::Detach(follower, TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW);
    tlsEcs.actorRegistry.remove<TeamId>(follower);
    EXPECT_TRUE(PlayerTeamSystem::NeedsFollowRecheck(follower)) << "缺 TeamId = 尚未同步到组队权威,仍需补查";
}

TEST_F(TeamFollowDriveTest, EmptyFormationIsReclaimedOnNextUpdate)
{
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowFormationComp>(leader));
    TeamFollowSystem::SetFollowIntent(follower, false);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));

    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowFormationComp>(leader))
        << "最后一名随从离开后,空队形组件在下一拍回收,不再以 20Hz 空转轨迹与导航射线";
}

TEST_F(TeamFollowDriveTest, LeaderJumpDoesNotTeleportFollowerInBattle)
{
    WalkLeader(kTeamFollowSpacing + 3.0); // 队员正在走,速度非零且已广播行走
    const double battleX = X(follower);
    tlsEcs.actorRegistry.emplace<InBattleComp>(follower);

    // 队长瞬移到远方(> kTeamFollowMaxLeaderJump 且超过步行距离):触发 RestartFormation。
    tlsEcs.actorRegistry.get<Transform>(leader).mutable_location()->set_x(100.0);
    TeamFollowSystem::Update(0.05);

    EXPECT_DOUBLE_EQ(X(follower), battleX) << "战斗中绝不瞬移";
    EXPECT_TRUE(TeamFollowSystem::IsDriven(follower)) << "名次照留,不摘驱动";
    EXPECT_EQ(TeamFollowSystem::DrivenSlot(follower).value_or(0u), 1u);
    const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(follower);
    ASSERT_NE(velocity, nullptr);
    EXPECT_DOUBLE_EQ(velocity->x(), 0.0) << "重建队形时已替进战队员停步";
    EXPECT_FALSE(tlsEcs.actorRegistry.get<TeamFollowerComp>(follower).broadcastMoving);

    tlsEcs.actorRegistry.remove<InBattleComp>(follower);
    TeamFollowSystem::Update(0.05);
    EXPECT_NEAR(X(follower), 100.0, 1e-6) << "战斗结束后按 needsPlacement 放进队长的新槽位";
}

TEST_F(TeamFollowDriveTest, LeaderJumpWithoutTeleportKeepsBroadcastStateUntilFollowerStops)
{
    auto& frameTime = tlsFrameTimeManager.frameTime;
    const auto savedFrame = frameTime.current_frame();

    // 先拉出 > kTeamFollowMaxLeaderJump(4m) 的差距但未超追赶上限:战斗中停在 -1,队长走到 9。
    tlsEcs.actorRegistry.emplace<InBattleComp>(follower);
    WalkLeader(9.0);
    tlsEcs.actorRegistry.remove<InBattleComp>(follower);

    // 追赶一 tick:队员正在走(速度非零、broadcastMoving=true),此时仍离队长 > 4m。
    frameTime.set_current_frame(savedFrame + 1);
    TeamFollowSystem::Update(0.05);
    ASSERT_TRUE(tlsEcs.actorRegistry.get<TeamFollowerComp>(follower).broadcastMoving);

    // 队长跳回队员脚下(> 4m):触发 RestartFormation,PlaceMember 走 snap(不瞬移),必须保留 broadcastMoving。
    const double followerX = X(follower);
    tlsEcs.actorRegistry.get<Transform>(leader).mutable_location()->set_x(followerX);
    frameTime.set_current_frame(savedFrame + 2);
    TeamFollowSystem::Update(0.05);
    EXPECT_TRUE(tlsEcs.actorRegistry.get<TeamFollowerComp>(follower).broadcastMoving)
        << "不瞬移的重建不能把已广播的行走状态抹掉,否则下一 tick 停步不会发停步广播";

    // 下一 tick 已在槽位上(moved==0):StopMotion 清速度并补发停步广播(更新 broadcastFrame、清 broadcastMoving)。
    frameTime.set_current_frame(savedFrame + 3);
    TeamFollowSystem::Update(0.05);
    const auto& driven = tlsEcs.actorRegistry.get<TeamFollowerComp>(follower);
    EXPECT_FALSE(driven.broadcastMoving);
    EXPECT_EQ(driven.broadcastFrame, savedFrame + 3);
    const auto* velocity = tlsEcs.actorRegistry.try_get<Velocity>(follower);
    ASSERT_NE(velocity, nullptr);
    EXPECT_DOUBLE_EQ(velocity->x(), 0.0);

    frameTime.set_current_frame(savedFrame);
}

TEST_F(PlayerTeamFollowTest, HostedPlayerAutoReturnsToMainCityWhenRemovedFromTeam)
{
    ASSERT_NO_FATAL_FAILURE(StartSceneManager());
    constexpr uint32_t kMainCityConfId = 1001;
    constexpr uint32_t kFieldMapConfId = 2001;
    PlayerTeamFollowTestAccess::SetDefaultMainCity(kMainCityConfId);

    // 离线托管状态 + 当前在野外地图 + 曾有队伍
    const auto lostSession = SessionOnGate(kCurrentGateId);
    SessionMap().erase(SessionOnGate(kOriginalGateId));
    tlsEcs.actorRegistry.remove<PlayerSessionSnapshotComp>(this->player);
    auto& held = tlsEcs.actorRegistry.emplace<PlayerConnectionLostComp>(
        this->player, PlayerConnectionLostComp{connection_lost_hold::Clock::now(), lostSession, 41u});
    held.afkPass = connection_lost_hold::AfkPass::kActive;
    held.afkPassExpiresAtSec = TimeSystem::NowSecondsUTC() + 3600;
    held.hostingStarted = true;
    tlsEcs.actorRegistry.emplace_or_replace<PlayerRoleAdmissionComp>(this->player, 41u, 9u);

    auto& sceneInfo = tlsEcs.sceneRegistry.get<SceneInfoComp>(this->scene);
    sceneInfo.set_scene_config_id(kFieldMapConfId);

    auto& profile = tlsEcs.actorRegistry.emplace_or_replace<PlayerUint64Comp>(this->player);
    profile.set_afk_pass_expires_at(held.afkPassExpiresAtSec);
    profile.set_afk_auto_return_city_on_team_end(true);

    auto& team = tlsEcs.actorRegistry.emplace_or_replace<TeamId>(this->player);
    team.set_team_id(kTeamId);
    team.set_membership_epoch(10);

    // 离队 / 解散 (teamId 0, epoch 11): 自动发 hosted_placement 回默认主城安全区
    PlayerTeamFollowTestAccess::ApplyMembership(this->player, 0, 11);
    ASSERT_TRUE(muduo_grpc_test::RunLoopUntil(loop, [requests = sceneManager.requests, player = this->player]
    {
        return !requests->empty() &&
               !tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(player);
    }, 5s));
    ASSERT_EQ(sceneManager.requests->size(), 1u);
    const auto& req = sceneManager.requests->front();
    EXPECT_EQ(req.player_id(), kPlayerId);
    EXPECT_TRUE(req.hosted_placement());
    EXPECT_EQ(req.scene_id(), 0u);
    EXPECT_EQ(req.scene_conf_id(), kMainCityConfId);
    EXPECT_EQ(req.session_id(), lostSession);
    EXPECT_EQ(req.login_generation(), 41u);
    EXPECT_TRUE(req.gate_id().empty());

    // 若已在默认主城:再次离队不重复发回城请求
    sceneInfo.set_scene_config_id(kMainCityConfId);
    PlayerTeamFollowTestAccess::ApplyMembership(this->player, kTeamId, 12);
    PlayerTeamFollowTestAccess::ApplyMembership(this->player, 0, 13);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSceneChangeInFlightComp>(this->player));
    EXPECT_EQ(sceneManager.requests->size(), 1u);

    PlayerTeamFollowTestAccess::SetDefaultMainCity(0);
}

// ---- 跟随事件计数经 /metrics 导出(2026-11-27 起;TeamFollowSystem::WriteMetrics,team-follow-server-trail.md §10) ----
// 计数是本线程的、跨用例累加,下面的用例都按「之前 / 之后」的差值断言。

std::string TeamFollowEvent(std::string_view event, std::string_view reason = {})
{
    std::string series = "scene_team_follow_events_total{event=\"" + std::string(event) + "\"";
    if (!reason.empty())
    {
        series += ",reason=\"" + std::string(reason) + "\"";
    }
    return series + "}";
}

std::string TeamFollowAttached(std::string_view placement)
{
    return "scene_team_follow_attached_total{placement=\"" + std::string(placement) + "\"}";
}

// 本线程计数此刻导出的值;没有这条样本返回 UINT64_MAX。样本行在行首,按 "\n<序列> " 找才不会落到 HELP 行上。
uint64_t TeamFollowMetric(const std::string& series)
{
    prometheus_text::Writer writer;
    TeamFollowSystem::WriteMetrics(writer);
    EXPECT_EQ(writer.DroppedSamples(), 0u);
    const std::string& text = writer.Text();
    const auto at = text.find("\n" + series + " ");
    return at == std::string::npos ? UINT64_MAX : std::stoull(text.substr(at + series.size() + 2));
}

uint64_t TeamFollowAttachedTotal()
{
    uint64_t total = 0;
    for (const std::string_view placement : {"snap", "walk", "teleport"})
    {
        total += TeamFollowMetric(TeamFollowAttached(placement));
    }
    return total;
}

// 每个取值进程一启动就以 0 导出,increase() 才抓得到第一次。标签取值在这里钉死:skipped / recheck 与各处日志里
// reason= 的字面量逐字相同,detached / dissolved 是 TeamFollowStopReason 的枚举名(日志同一个字符串),改名会让
// 看板与日志查询对不上。计数是本线程的,换一条没跑过跟随的新线程渲染,每一项都应恰好是 0。
TEST(TeamFollowMetrics, EveryEventReasonAndPlacementIsExportedFromZero)
{
    std::string text;
    uint32_t dropped = UINT32_MAX;
    std::thread([&text, &dropped]
    {
        prometheus_text::Writer writer;
        TeamFollowSystem::WriteMetrics(writer);
        text = writer.Text();
        dropped = writer.DroppedSamples();
    }).join();
    EXPECT_EQ(dropped, 0u);
    EXPECT_NE(text.find("# TYPE scene_team_follow_events_total counter\n"), std::string::npos);
    EXPECT_NE(text.find("# TYPE scene_team_follow_attached_total counter\n"), std::string::npos);

    std::vector<std::string> series;
    for (const std::string_view reason : {"in_battle", "ownership_in_flight", "scene_change_in_flight", "redis_error",
                                          "battle_lock", "cross_zone", "session_not_live", "formation_full"})
    {
        series.push_back(TeamFollowEvent("skipped", reason));
    }
    for (const std::string_view reason : {"hosting_heartbeat", "leader_settled"})
    {
        series.push_back(TeamFollowEvent("recheck", reason));
    }
    for (const std::string_view reason : {"client_follow", "client_leave", "offline_hosting"})
    {
        series.push_back(TeamFollowEvent("intent", reason));
    }
    for (const std::string_view reason :
         {"TEAM_FOLLOW_STOP_REASON_UNSPECIFIED", "TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW", "TEAM_FOLLOW_STOP_REASON_TEAM_CHANGED",
          "TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED", "TEAM_FOLLOW_STOP_REASON_BECAME_LEADER",
          "TEAM_FOLLOW_STOP_REASON_SCENE_CHANGED", "TEAM_FOLLOW_STOP_REASON_LEADER_GONE", "TEAM_FOLLOW_STOP_REASON_LEAVING"})
    {
        series.push_back(TeamFollowEvent("detached", reason));
        series.push_back(TeamFollowEvent("dissolved", reason));
    }
    series.push_back(TeamFollowEvent("placed", "fell_behind"));
    series.push_back(TeamFollowEvent("restarted"));
    series.push_back(TeamFollowEvent("await_leader_expired"));
    for (const std::string_view placement : {"snap", "walk", "teleport"})
    {
        series.push_back(TeamFollowAttached(placement));
    }

    for (const auto& one : series)
    {
        EXPECT_NE(text.find("\n" + one + " 0\n"), std::string::npos) << one;
    }
    // 没有名单之外的样本:两族各一行 HELP、一行 TYPE,其余每行一条样本。
    EXPECT_EQ(static_cast<std::size_t>(std::count(text.begin(), text.end(), '\n')), series.size() + 4) << text;
    EXPECT_EQ(text.find("player_id"), std::string::npos) << "玩家号不做标签(AGENTS.md §9)";
}

// 暂离计一次 intent(client_leave)与一次 detached(LEFT_FOLLOW);再声明、再接上计一次 attached;
// 队长离队后下一 tick 解散队形,队长那边计 dissolved、队员这边计 detached,原因都是 LEADER_CHANGED。
TEST_F(TeamFollowDriveTest, LeavingRejoiningAndDissolvingAreCountedLikeTheLogLines)
{
    const auto clientLeave = TeamFollowEvent("intent", "client_leave");
    const auto leftFollow = TeamFollowEvent("detached", "TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW");
    const auto memberLeaderChanged = TeamFollowEvent("detached", "TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED");
    const auto formationLeaderChanged = TeamFollowEvent("dissolved", "TEAM_FOLLOW_STOP_REASON_LEADER_CHANGED");
    const uint64_t clientLeaveBefore = TeamFollowMetric(clientLeave);
    const uint64_t leftFollowBefore = TeamFollowMetric(leftFollow);
    const uint64_t attachedBefore = TeamFollowAttachedTotal();
    ASSERT_NE(clientLeaveBefore, UINT64_MAX);
    ASSERT_NE(leftFollowBefore, UINT64_MAX);

    PlayerTeamSystem::OnClientFollowRequest(follower, false);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));
    EXPECT_EQ(TeamFollowMetric(clientLeave), clientLeaveBefore + 1);
    EXPECT_EQ(TeamFollowMetric(leftFollow), leftFollowBefore + 1);

    TeamFollowSystem::SetFollowIntent(follower, true);
    ASSERT_TRUE(TeamFollowSystem::TryAttach(follower, leader, /*walkAllowed=*/true));
    EXPECT_EQ(TeamFollowAttachedTotal(), attachedBefore + 1);

    const uint64_t memberBefore = TeamFollowMetric(memberLeaderChanged);
    const uint64_t formationBefore = TeamFollowMetric(formationLeaderChanged);
    tlsEcs.actorRegistry.get<TeamId>(leader).set_team_id(0);
    TeamFollowSystem::Update(0.05);
    EXPECT_FALSE(TeamFollowSystem::IsDriven(follower));
    EXPECT_EQ(TeamFollowMetric(memberLeaderChanged), memberBefore + 1);
    EXPECT_EQ(TeamFollowMetric(formationLeaderChanged), formationBefore + 1);
}

// 队长瞬移重建队形、托管开始替他开启跟随、托管中的兜底重试,各对应一行日志、各计一次。
TEST_F(TeamFollowDriveTest, LeaderJumpAndOfflineHostingAreCounted)
{
    const auto restarted = TeamFollowEvent("restarted");
    const auto offlineHosting = TeamFollowEvent("intent", "offline_hosting");
    const auto hostingHeartbeat = TeamFollowEvent("recheck", "hosting_heartbeat");
    const uint64_t restartedBefore = TeamFollowMetric(restarted);
    const uint64_t hostingBefore = TeamFollowMetric(offlineHosting);
    const uint64_t heartbeatBefore = TeamFollowMetric(hostingHeartbeat);

    // 队长一 tick 挪出 kTeamFollowMaxLeaderJump(4 米)以上:轨迹从新位置重来,队员按加入规则重新落位。
    tlsEcs.actorRegistry.get<Transform>(leader).mutable_location()->set_x(10.0);
    TeamFollowSystem::Update(0.05);
    EXPECT_EQ(TeamFollowMetric(restarted), restartedBefore + 1);
    ASSERT_TRUE(TeamFollowSystem::IsDriven(follower));

    // 托管开始:服务器替他开启跟随;他已接在队长身后,不再发读。
    PlayerTeamSystem::OnOfflineHostingStarted(follower);
    EXPECT_EQ(TeamFollowMetric(offlineHosting), hostingBefore + 1);

    // 兜底重试只替想跟却没接上的人计:接着的时候不计;摘下来(意图还在)之后计一次。
    PlayerTeamSystem::OnOfflineHostingHeartbeat(follower);
    EXPECT_EQ(TeamFollowMetric(hostingHeartbeat), heartbeatBefore);
    TeamFollowSystem::Detach(follower, TEAM_FOLLOW_STOP_REASON_LEFT_FOLLOW);
    PlayerTeamSystem::OnOfflineHostingHeartbeat(follower);
    EXPECT_EQ(TeamFollowMetric(hostingHeartbeat), heartbeatBefore + 1);
}

// 队长离开本节点:队员以 LEADER_GONE 摘下、队形以 LEADER_GONE 解散;去留有了结论再跟一次,计 recheck(leader_settled)。
TEST_F(TeamFollowDriveTest, WaitingForADepartedLeaderIsCountedWhenItSettles)
{
    const auto leaderGone = TeamFollowEvent("detached", "TEAM_FOLLOW_STOP_REASON_LEADER_GONE");
    const auto dissolvedLeaderGone = TeamFollowEvent("dissolved", "TEAM_FOLLOW_STOP_REASON_LEADER_GONE");
    const auto settled = TeamFollowEvent("recheck", "leader_settled");
    const uint64_t leaderGoneBefore = TeamFollowMetric(leaderGone);
    const uint64_t dissolvedBefore = TeamFollowMetric(dissolvedLeaderGone);
    const uint64_t settledBefore = TeamFollowMetric(settled);

    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    EXPECT_EQ(TeamFollowMetric(leaderGone), leaderGoneBefore + 1);
    EXPECT_EQ(TeamFollowMetric(dissolvedLeaderGone), dissolvedBefore + 1);

    PlayerTeamSystem::FollowAfterLeaderSettled();
    EXPECT_EQ(TeamFollowMetric(settled), settledBefore) << "队长还在交接中:没有结论,不重新检查";

    tlsEcs.playerList.erase(kLeaderId);
    tlsEcs.actorRegistry.destroy(leader);
    PlayerTeamSystem::FollowAfterLeaderSettled();
    EXPECT_EQ(TeamFollowMetric(settled), settledBefore + 1);
}

// 等满 kTeamFollowAwaitLeaderFrames 还没结论就放弃,计 await_leader_expired(这一项没有 reason)。
TEST_F(TeamFollowDriveTest, GivingUpOnADepartedLeaderIsCounted)
{
    const auto expired = TeamFollowEvent("await_leader_expired");
    const uint64_t expiredBefore = TeamFollowMetric(expired);
    StartLeaderHandoff(leader);
    TeamFollowSystem::Update(0.05);
    ASSERT_TRUE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));

    auto& frameTime = tlsFrameTimeManager.frameTime;
    const auto savedFrame = frameTime.current_frame();
    frameTime.set_current_frame(savedFrame + kTeamFollowAwaitLeaderFrames);
    PlayerTeamSystem::FollowAfterLeaderSettled();
    frameTime.set_current_frame(savedFrame);
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<TeamFollowAwaitLeaderComp>(follower));
    EXPECT_EQ(TeamFollowMetric(expired), expiredBefore + 1);
}

// 跟随检查在哪一步停下,就给那个原因计一次(与 metric=team_follow_skipped 日志一一对应)。
TEST_F(PlayerTeamFollowTest, SkippedChecksAreCountedByReason)
{
    const auto skipped = [](std::string_view reason) { return TeamFollowMetric(TeamFollowEvent("skipped", reason)); };
    const uint64_t crossZone = skipped("cross_zone");
    const uint64_t battleLock = skipped("battle_lock");
    const uint64_t redisError = skipped("redis_error");
    const uint64_t inBattle = skipped("in_battle");

    ASSERT_NO_FATAL_FAILURE(OpenLocationRead());
    redis.Reply(REDIS_REPLY_STRING, LeaderLocation(kCurrentSceneId, kZoneId + 1)); // 队长在别的 zone
    EXPECT_EQ(skipped("cross_zone"), crossZone + 1);

    Check();
    redis.Reply(REDIS_REPLY_STRING, "battle-1"); // 本人的战斗锁还在
    EXPECT_EQ(skipped("battle_lock"), battleLock + 1);

    Check();
    redis.Reply(REDIS_REPLY_ERROR); // 战斗锁没读成
    EXPECT_EQ(skipped("redis_error"), redisError + 1);

    tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    Check(); // 入口的守卫就挡下,不发读
    EXPECT_EQ(skipped("in_battle"), inBattle + 1);
    EXPECT_TRUE(redis.pending.empty());
    ExpectNoRegroup(player);
}
} // namespace
