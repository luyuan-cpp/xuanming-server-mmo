#include <gtest/gtest.h>
#include <iomanip>
#include <memory>
#include <limits>
#include <sstream>
#include <utility>
#include <vector>
#include "core/security/service_call_auth.h"
#include "core/type_alias/player_session_type_alias.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_scene.h"
#include "player/system/role_admission_system.h"
#include "player/system/handoff_sql_barrier.h"
#include "player/system/release_fence.h"
#include "battle/system/player_battle.h"
#include "player/comp/player_frozen_comp.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "spatial/comp/scene_node_scene_comp.h"
#include "thread_context/ecs_context.h"

namespace {
using Admission = RoleAdmissionSystem;
using namespace std::chrono_literals;
constexpr Guid kPlayer = 970400001;
constexpr uint64_t kScene = 970400002;
constexpr uint64_t kRevision = 17;

std::string Version(uint64_t epoch, uint64_t seq) {
    std::ostringstream text;
    text << std::setfill('0') << std::setw(20) << epoch << ':' << std::setw(20) << seq;
    return text.str();
}

class RoleAdmissionTest : public testing::Test {
protected:
    void SetUp() override {
        scene = tlsEcs.sceneRegistry.create();
        tlsEcs.sceneRegistry.emplace<SceneInfoComp>(scene).set_scene_id(kScene);
        Admission::Dependencies dependencies;
        dependencies.now = [this] { ++clockReads; return now; };
        dependencies.localNode = [this] {
            ++localNodeReads;
            return Admission::LocalNode{"01234567-89ab-cdef-0123-456789abcdef", "91", 3};
        };
        dependencies.registerAdmission = [this](auto request, auto reply) {
            registers.push_back(std::move(request)); registerReplies.push_back(std::move(reply)); return true;
        };
        dependencies.closeAdmissions = [this](auto request, auto reply) {
            closes.push_back(std::move(request)); closeReplies.push_back(std::move(reply)); return true;
        };
        dependencies.ackDrain = [this](auto request, auto reply) {
            acks.push_back(std::move(request)); ackReplies.push_back(std::move(reply)); return true;
        };
        dependencies.readSqlAck = [this](Guid player, auto reply) {
            EXPECT_EQ(kPlayer, player); sqlReplies.push_back(std::move(reply)); return true;
        };
        dependencies.registerHandoffFence = [this](Guid player, std::string candidate, std::optional<std::string> prior,
            std::function<bool(const std::string&)> stillQualified, Admission::FenceReply reply) {
            EXPECT_EQ(kPlayer, player);
            fenceCandidates.push_back(candidate); fencePriors.push_back(prior);
            auto afterRead = [this, stillQualified = std::move(stillQualified), reply = std::move(reply)] {
                const bool allowed = stillQualified("");
                if (allowed) ++fenceWrites;
                auto finish = [reply, allowed] { reply(allowed, std::string("")); };
                if (delayFenceReply) fenceReplies.push_back(std::move(finish)); else finish();
            };
            if (delayFence) fenceReads.push_back(std::move(afterRead)); else afterRead();
            return true;
        };
        dependencies.revokeHandoffFence = [this](Guid player, std::string candidate, std::string prior,
            std::string revoked, std::function<void(bool)> reply) {
            EXPECT_EQ(kPlayer, player); revokedCandidates.push_back(std::move(candidate));
            EXPECT_TRUE(handoff_sql_barrier::ParseFence(revoked));
            reply(true); return true;
        };
        dependencies.activate = [this](Guid player, PlayerEnterContext context) {
            EXPECT_EQ(kPlayer, player); activated.push_back(std::move(context));
        };
        dependencies.freeze = [this](Guid player) { EXPECT_EQ(kPlayer, player); ++freezes; };
        dependencies.reject = [this](Guid player, uint32_t) { EXPECT_TRUE(player == 0 || player == kPlayer); ++rejections; };
        dependencies.hasPendingLoad = [this](Guid) { return pendingLoad; };
        scope = std::make_unique<Admission::ScopedDependencies>(std::move(dependencies));
        ASSERT_EQ(scene, PlayerSceneSystem::FindSceneById(kScene)) << "夹具场景必须能被生产查找入口解析";
        ASSERT_EQ(kSuccess, PlayerSceneSystem::CheckEnterScene(kPlayer, scene));
        ASSERT_TRUE(enter_fence::Admits(PlayerLifecycleSystem::JudgeEnter(kPlayer, 91, 9)));
    }
    void TearDown() override {
        RemoveEntity();
        PlayerLifecycleSystem::GetPendingEnterMap().erase(kPlayer);
        PlayerLifecycleSystem::EraseSessionMappingIfOwned(91, kPlayer);
        if (tlsEcs.sceneRegistry.valid(scene)) tlsEcs.sceneRegistry.destroy(scene);
        scope.reset();
    }
    PlayerEnterContext Context() const {
        PlayerEnterContext context;
        context.enterInfo.set_scene_id(kScene);
        context.enterInfo.set_session_id(91);
        context.ownerRevision = kRevision; context.ownerEpoch = 8; context.loginGeneration = 9;
        return context;
    }
    void Enter() {
        const auto context = Context();
        const auto found = PlayerSceneSystem::FindSceneById(context.enterInfo.scene_id());
        const auto player = tlsEcs.GetPlayer(kPlayer);
        const auto allowed = player == entt::null ? PlayerSceneSystem::CheckEnterScene(kPlayer, found)
            : PlayerSceneSystem::CheckEnterScene(player, found);
        const auto verdict = PlayerLifecycleSystem::JudgeEnter(kPlayer, context.enterInfo.session_id(), context.loginGeneration);
        std::ostringstream details;
        details << "scene_found=" << (found == scene) << " scene_allowed=" << allowed
            << " expected_success=" << kSuccess << " enter_fence=" << enter_fence::VerdictName(verdict)
            << " revision=" << context.ownerRevision << " epoch=" << context.ownerEpoch
            << " login=" << context.loginGeneration << " session=" << context.enterInfo.session_id()
            << " node_reads=" << localNodeReads << " clock_reads=" << clockReads;
        lastEnterPreconditions = details.str();
        ASSERT_EQ(scene, found) << lastEnterPreconditions;
        ASSERT_EQ(kSuccess, allowed) << lastEnterPreconditions;
        ASSERT_TRUE(enter_fence::Admits(verdict)) << lastEnterPreconditions;
        const bool first = registers.empty() && rejections == 0;
        const auto beforeNodeReads = localNodeReads;
        Admission::RequestEnter(kPlayer, context);
        ASSERT_GT(localNodeReads, beforeNodeReads) << "必须调用当前ScopedDependencies的原节点提供者；" << lastEnterPreconditions;
        if (first) {
            ASSERT_EQ(1u, registers.size()) << "完整前置条件后的首次登记不能立即拒绝；" << lastEnterPreconditions
                << " node_reads_after=" << localNodeReads << " clock_reads_after=" << clockReads
                << " rejections=" << rejections;
        }
    }
    void CompleteRegister(size_t index, bool ok = true, uint64_t closed = 0) {
        ASSERT_LT(index, registerReplies.size()) << lastEnterPreconditions;
        ASSERT_LT(index, registers.size()) << lastEnterPreconditions;
        data_service::RegisterRoleAdmissionResponse reply;
        *reply.mutable_identity() = registers[index].identity();
        reply.set_closed_through(closed);
        auto completed = std::move(registerReplies[index]);
        ASSERT_TRUE(static_cast<bool>(completed));
        completed(ok, ok ? &reply : nullptr);
    }
    common::base::RoleTradeSeal SealFor(size_t index = 0, uint64_t listing = 71) const {
        const auto& identity = registers.at(index).identity();
        common::base::RoleTradeSeal seal;
        seal.set_listing_id(listing); seal.set_player_id(identity.player_id());
        seal.set_owner_revision(identity.owner_revision()); seal.set_scene_instance_id(identity.scene_instance_id());
        seal.set_through_generation(identity.generation()); seal.set_scene_node_id(identity.scene_node_id());
        seal.set_zone_id(identity.zone_id());
        return seal;
    }
    void Tick() { now += 2s; Admission::Tick(); }
    void Sql(size_t index, uint64_t epoch, uint64_t seq) {
        ASSERT_LT(index, sqlReplies.size());
        auto completed = std::move(sqlReplies[index]);
        ASSERT_TRUE(static_cast<bool>(completed));
        const auto version = Version(epoch, seq);
        completed(Admission::SqlAck{std::to_string(epoch), version, version, version});
    }
    void AddEntity() {
        entity = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, kPlayer);
        tlsEcs.playerList[kPlayer] = entity;
    }
    void RemoveEntity() {
        if (tlsEcs.actorRegistry.valid(entity)) tlsEcs.actorRegistry.destroy(entity);
        const auto found = tlsEcs.playerList.find(kPlayer);
        if (found != tlsEcs.playerList.end() && found->second == entity) tlsEcs.playerList.erase(found);
        entity = entt::null;
    }
    SceneEntity scene{entt::null};
    entt::entity entity{entt::null};
    Admission::Clock::time_point now{Admission::Clock::time_point{} + 1h};
    std::unique_ptr<Admission::ScopedDependencies> scope;
    std::vector<data_service::RegisterRoleAdmissionRequest> registers;
    std::vector<Admission::RegisterReply> registerReplies;
    std::vector<data_service::CloseRoleAdmissionsRequest> closes;
    std::vector<Admission::CloseReply> closeReplies;
    std::vector<data_service::AckRoleTradeDrainRequest> acks;
    std::vector<std::function<void(bool)>> ackReplies;
    std::vector<std::function<void(std::optional<Admission::SqlAck>)>> sqlReplies;
    std::vector<PlayerEnterContext> activated;
    uint32_t freezes{0}, rejections{0};
    uint32_t localNodeReads{0}, clockReads{0};
    std::string lastEnterPreconditions;
    bool pendingLoad{false};
    bool delayFence{false};
    bool delayFenceReply{false};
    std::vector<std::function<void()>> fenceReplies;
    uint32_t fenceWrites{0};
    std::vector<std::string> fenceCandidates;
    std::vector<std::string> revokedCandidates;
    std::vector<std::optional<std::string>> fencePriors;
    std::vector<std::function<void()>> fenceReads;
};

TEST_F(RoleAdmissionTest, LiveBattleFinishesBeforeTradeFreezesOrUnloadsThePlayer) {
    AddEntity();
    tlsEcs.actorRegistry.emplace<InBattleComp>(entity);
    PlayerLifecycleSystem::FreezeRoleForTrade(kPlayer);
    EXPECT_TRUE(tlsEcs.actorRegistry.valid(entity));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(entity));
    EXPECT_TRUE(sqlReplies.empty());
    EXPECT_TRUE(acks.empty());
}

TEST_F(RoleAdmissionTest, AcceptedTradeSealRejectsNewBattleBeforeDrainCompletes) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    AddEntity();
    ASSERT_TRUE(Admission::Seal(SealFor()));
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(entity));
    PrepareBattleRequest request;
    request.set_player_id(kPlayer);
    request.set_battle_id(83);
    request.set_deadline_ms(std::numeric_limits<uint64_t>::max());
    PrepareBattleResponse response;
    PlayerBattleSystem::PrepareBattle(request, response);
    EXPECT_EQ(kFeatureUnavailable, response.error_message().id());
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<InBattleComp>(entity));
    EXPECT_TRUE(acks.empty());
}

TEST_F(RoleAdmissionTest, MissingIdentityAndSceneFailBeforeRegistration) {
    for (unsigned field = 0; field != 6; ++field) {
        auto context = Context();
        switch (field) {
        case 0: context.ownerRevision = 0; break;
        case 1: context.ownerEpoch = 0; break;
        case 2: context.loginGeneration = 0; break;
        case 3: context.enterInfo.set_session_id(0); break;
        case 4: context.enterInfo.set_scene_id(0); break;
        default: break;
        }
        Admission::RequestEnter(field == 5 ? Guid{0} : kPlayer, context);
    }
    EXPECT_EQ(6u, rejections);
    EXPECT_TRUE(registers.empty()); EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, 1, kRevision));
}

TEST_F(RoleAdmissionTest, UnknownRegisterReplaysOriginalAndNeverActivatesBeforeConfirmation) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_EQ(1u, registers.size());
    const auto generation = registers.front().identity().generation();
    EXPECT_NE(0u, generation); EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, generation, kRevision));
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0, false));
    Tick();
    ASSERT_EQ(2u, registers.size());
    EXPECT_EQ(registers[0].SerializeAsString(), registers[1].SerializeAsString());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(1));
    ASSERT_EQ(1u, activated.size());
    EXPECT_TRUE(Admission::Allows(kPlayer, generation, kRevision));
    EXPECT_FALSE(Admission::Allows(kPlayer, 0, kRevision));
    EXPECT_FALSE(Admission::Allows(kPlayer, generation, 0));
    EXPECT_FALSE(Admission::Allows(kPlayer, generation, kRevision + 1));
    EXPECT_EQ(generation, activated.front().roleAdmissionGeneration);
}

TEST_F(RoleAdmissionTest, UnknownRegisterDoesNotChangeGenerationWhenAnotherRouteArrives) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(Enter());
    EXPECT_EQ(1u, registers.size()); EXPECT_EQ(0u, rejections);
    auto changed = Context(); changed.ownerRevision += 1;
    Admission::RequestEnter(kPlayer, changed);
    EXPECT_EQ(1u, registers.size()); EXPECT_EQ(1u, rejections);
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    EXPECT_EQ(1u, activated.size());
}

TEST_F(RoleAdmissionTest, ThreeFailedAttemptsStopWithoutActivation) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0, false));
    Tick();
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(1, false));
    Tick();
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(2, false));
    Tick();
    EXPECT_EQ(3u, registers.size()); EXPECT_EQ(1u, rejections); EXPECT_TRUE(activated.empty());
}

TEST_F(RoleAdmissionTest, LateSuccessAfterDeadlineCannotActivateEvenBeforeTick) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    now += 16s;
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    EXPECT_TRUE(activated.empty()); EXPECT_EQ(1u, rejections);
}

TEST_F(RoleAdmissionTest, ClosedThroughInRegisterReplyNeverGrantsThatGeneration) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0, true, registers[0].identity().generation()));
    EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, registers[0].identity().generation(), kRevision));
}

TEST_F(RoleAdmissionTest, CallbackFromDestroyedScopeCannotMutateRestoredThreadState) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    auto callback = std::move(registerReplies.front());
    data_service::RegisterRoleAdmissionResponse reply;
    *reply.mutable_identity() = registers.front().identity();
    scope.reset();
    callback(true, &reply);
    EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, reply.identity().generation(), kRevision));
}

TEST_F(RoleAdmissionTest, DrainWaitsForEntityCallbacksAndActualForcedSave) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    AddEntity();
    Admission::ObserveSave(kPlayer, 8, 2);
    Admission::BeginCallback(kPlayer);
    ASSERT_TRUE(Admission::Seal(SealFor()));
    EXPECT_TRUE(Admission::IsSealed(kPlayer));
    EXPECT_FALSE(Admission::Allows(kPlayer, registers[0].identity().generation(), kRevision));
    Tick(); EXPECT_TRUE(sqlReplies.empty()); EXPECT_TRUE(acks.empty());
    RemoveEntity(); Admission::OnDestroyed(kPlayer); Tick();
    EXPECT_TRUE(sqlReplies.empty());
    Admission::EndCallback(kPlayer); Tick();
    EXPECT_TRUE(sqlReplies.empty()) << "未成功排入强制存盘，旧SQL满足也不能排空";
    now += 90s; Tick();
    EXPECT_TRUE(Admission::IsSealed(kPlayer)); EXPECT_TRUE(acks.empty());
    Admission::ObserveTradeSaveQueued(kPlayer, 8, 3); Tick();
    ASSERT_EQ(1u, sqlReplies.size()); Sql(0, 8, 3); Tick();
    ASSERT_EQ(1u, acks.size());
    EXPECT_EQ(3u, acks[0].required_save_seq());
}

TEST_F(RoleAdmissionTest, StrictAckNeedsExactBothRowsAndCompleteUint64Version) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    constexpr uint64_t seq = UINT64_MAX;
    Admission::ObserveSave(kPlayer, 8, seq);
    ASSERT_TRUE(Admission::Seal(SealFor()));
    ASSERT_EQ(1u, sqlReplies.size());
    const auto version = Version(8, seq);
    auto callback = std::move(sqlReplies[0]);
    callback(Admission::SqlAck{"8", version, version, Version(8, seq - 1)});
    Tick(); EXPECT_TRUE(acks.empty());
    ASSERT_EQ(2u, sqlReplies.size());
    auto malformed = std::move(sqlReplies[1]);
    malformed(Admission::SqlAck{"08", version, version, version});
    Tick(); EXPECT_TRUE(acks.empty());
    ASSERT_EQ(3u, sqlReplies.size()); Sql(2, 8, seq); Tick();
    ASSERT_EQ(1u, acks.size()); EXPECT_EQ(seq, acks.front().required_save_seq());
}

TEST_F(RoleAdmissionTest, LateLoadCannotActivateAndItsOriginalFenceRaisesDrainFloor) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    ASSERT_EQ(1u, activated.size());
    auto context = activated.back();
    context.inheritClear.outstandingReplies = 1;
    PlayerLifecycleSystem::GetPendingEnterMap()[kPlayer] = context;
    SessionMap()[91] = kPlayer;
    pendingLoad = true;
    Admission::BeginCallback(kPlayer);
    ASSERT_TRUE(Admission::Seal(SealFor()));
    PlayerAllData loaded;
    loaded.mutable_player_database_data()->set_player_id(kPlayer);
    loaded.mutable_player_database_data()->set_fence_owner_epoch(9);
    loaded.mutable_player_database_data()->set_fence_save_seq(2);
    loaded.mutable_player_database_1_data()->set_player_id(kPlayer);
    loaded.mutable_player_database_1_data()->set_fence_owner_epoch(9);
    loaded.mutable_player_database_1_data()->set_fence_save_seq(2);
    PlayerLifecycleSystem::HandlePlayerAsyncLoaded(kPlayer, loaded);
    EXPECT_EQ(1u, activated.size()); EXPECT_TRUE(sqlReplies.empty());
    Tick(); EXPECT_TRUE(acks.empty());
    pendingLoad = false; Admission::EndCallback(kPlayer);
    auto& pending = PlayerLifecycleSystem::GetPendingEnterMap();
    pending.at(kPlayer).inheritClear.outstandingReplies = 0;
    Tick();
    EXPECT_EQ(0u, pending.count(kPlayer)); EXPECT_FALSE(SessionMap().contains(91));
    ASSERT_EQ(1u, sqlReplies.size()); Sql(0, 9, 1); Tick();
    EXPECT_TRUE(acks.empty());
    ASSERT_EQ(2u, sqlReplies.size()); Sql(1, 9, 2); Tick();
    ASSERT_EQ(1u, acks.size()); EXPECT_EQ(9u, acks[0].required_owner_epoch()); EXPECT_EQ(2u, acks[0].required_save_seq());
}

TEST_F(RoleAdmissionTest, OldHolderSealDuringUnconfirmedNewRegisterStillFreezesOriginal) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    AddEntity();
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_EQ(2u, registers.size());
    const auto oldSeal = SealFor();
    ASSERT_TRUE(Admission::Seal(oldSeal));
    EXPECT_TRUE(Admission::IsSealed(kPlayer));
    EXPECT_EQ(1u, freezes);
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(1, false));
    EXPECT_EQ(1u, activated.size());
    Admission::ObserveTradeSaveQueued(kPlayer, 8, 2); RemoveEntity(); Tick();
    ASSERT_EQ(1u, sqlReplies.size()); Sql(0, 8, 2); Tick();
    ASSERT_EQ(1u, acks.size());
    EXPECT_EQ(oldSeal.SerializeAsString(), acks[0].seal().SerializeAsString());
    auto ack = std::move(ackReplies[0]); ack(true);

    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_EQ(3u, registers.size());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(2, true, oldSeal.through_generation()));
    const auto current = registers[2].identity().generation();
    ASSERT_TRUE(Admission::Allows(kPlayer, current, kRevision));
    EXPECT_FALSE(Admission::IsSealed(kPlayer));
    EXPECT_TRUE(Admission::Seal(oldSeal)); EXPECT_EQ(1u, freezes);
    auto changed = oldSeal; changed.set_owner_revision(kRevision + 1);
    EXPECT_FALSE(Admission::Seal(changed));
    EXPECT_TRUE(Admission::Allows(kPlayer, current, kRevision));
}

TEST_F(RoleAdmissionTest, CloseKeepsHistoricalFloorAndClosedAuthorizationNeverReactivates) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    Admission::ObserveSave(kPlayer, 8, 12); Tick();
    ASSERT_EQ(1u, closes.size());
    data_service::CloseRoleAdmissionsResponse reply;
    reply.set_closed_through(closes[0].through_generation());
    auto close = std::move(closeReplies[0]); close(true, &reply);
    EXPECT_FALSE(Admission::Allows(kPlayer, closes[0].through_generation(), kRevision));
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(1, true, reply.closed_through()));
    Admission::ObserveSave(kPlayer, 0, 0); Tick();
    ASSERT_EQ(2u, closes.size());
    EXPECT_EQ(8u, closes[1].required_owner_epoch()); EXPECT_EQ(12u, closes[1].required_save_seq());
}

TEST_F(RoleAdmissionTest, HolderCapturedBeforeCloseStillReceivesListingAckAfterClose) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    Tick();
    ASSERT_EQ(1u, closes.size());
    data_service::CloseRoleAdmissionsResponse reply;
    reply.set_closed_through(closes[0].through_generation());
    auto close = std::move(closeReplies[0]); close(true, &reply);
    ASSERT_TRUE(Admission::Seal(SealFor())); Tick();
    ASSERT_EQ(1u, acks.size()) << "Close不能替代已固定listing holder的ACK";
}

TEST_F(RoleAdmissionTest, FailedAckRetriesExactOriginalAndCannotOpenAdmission) {
    ASSERT_NO_FATAL_FAILURE(Enter());
    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    ASSERT_TRUE(Admission::Seal(SealFor()));
    ASSERT_EQ(1u, acks.size());
    auto failed = std::move(ackReplies[0]); failed(false);
    ASSERT_NO_FATAL_FAILURE(Enter());
    EXPECT_EQ(1u, registers.size()); EXPECT_EQ(1u, rejections);
    Tick(); ASSERT_EQ(2u, acks.size());
    EXPECT_EQ(acks[0].SerializeAsString(), acks[1].SerializeAsString());
    auto completed = std::move(ackReplies[1]); completed(true);
    Tick(); EXPECT_EQ(2u, acks.size());
    EXPECT_FALSE(Admission::Allows(kPlayer, registers[0].identity().generation(), kRevision));
}

TEST(RoleAdmissionScope, ExplicitNonzeroOriginalIsRequiredAndNestedScopeRestoresState) {
    Admission::Dependencies dependencies;
    auto& confirmed = dependencies.confirmedAdmissions.emplace_back();
    confirmed.set_player_id(kPlayer); confirmed.set_generation(31); confirmed.set_owner_revision(kRevision);
    auto& invalid = dependencies.confirmedAdmissions.emplace_back();
    invalid.set_player_id(kPlayer + 1); invalid.set_owner_revision(kRevision);
    {
        Admission::ScopedDependencies outer(std::move(dependencies));
        EXPECT_TRUE(Admission::Allows(kPlayer, 31, kRevision));
        EXPECT_FALSE(Admission::Allows(kPlayer + 1, 0, kRevision));
        {
            Admission::ScopedDependencies inner(Admission::Dependencies{});
            EXPECT_FALSE(Admission::Allows(kPlayer, 31, kRevision));
        }
        EXPECT_TRUE(Admission::Allows(kPlayer, 31, kRevision));
    }
    EXPECT_FALSE(Admission::Allows(kPlayer, 31, kRevision));
}

TEST(RoleAdmissionAuthentication, SignatureBindsMethodCallerBodyAndDeadline) {
    constexpr std::string_view method = "/scene_node.SceneNodeGrpc/SealAccountRoleTrade";
    constexpr std::string_view secret = "role-admission-test-secret-at-least-32-bytes";
    constexpr uint64_t now = 1900000000123;
    SealAccountRoleTradeRequest request;
    request.mutable_seal()->set_player_id(kPlayer);
    request.mutable_seal()->set_listing_id(71);
    ASSERT_TRUE(service_call_auth::Sign(method, request, *request.mutable_auth(), "trade", secret, now));
    EXPECT_TRUE(service_call_auth::Verify(method, request, request.auth(), "trade", secret, now));
    EXPECT_TRUE(service_call_auth::Verify(method, request, request.auth(), "trade", std::string(" \t") + std::string(secret) + "\r\n", now));
    EXPECT_FALSE(service_call_auth::Verify("/scene_node.SceneNodeGrpc/ReleasePlayer", request, request.auth(), "trade", secret, now));
    EXPECT_FALSE(service_call_auth::Verify(method, request, request.auth(), "scene", secret, now));
    EXPECT_FALSE(service_call_auth::Verify(method, request, request.auth(), "trade", secret, now + 300001));
    EXPECT_FALSE(service_call_auth::Verify(method, request, request.auth(), "trade", "short", now));
    request.mutable_seal()->set_owner_revision(kRevision);
    EXPECT_FALSE(service_call_auth::Verify(method, request, request.auth(), "trade", secret, now));
}
} // namespace

TEST_F(RoleAdmissionTest, FenceReadAfterSealDoesNotSubmitOrActivate) {
    delayFence = true;
    Enter(); CompleteRegister(0);
    ASSERT_EQ(1u, fenceReads.size()); EXPECT_TRUE(activated.empty());
    ASSERT_TRUE(Admission::Seal(SealFor()));
    fenceReads[0]();
    EXPECT_EQ(0u, fenceWrites) << "Seal先于HGET完成，不能再发CAS登记/DEL";
    EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, registers[0].identity().generation(), kRevision));
}

TEST_F(RoleAdmissionTest, FenceReadAfterOriginalDeadlineDoesNotSubmitOrActivate) {
    delayFence = true;
    Enter(); CompleteRegister(0);
    ASSERT_EQ(1u, fenceReads.size());
    now += 15s;
    fenceReads[0]();
    EXPECT_EQ(0u, fenceWrites); EXPECT_TRUE(activated.empty());
    EXPECT_EQ(1u, rejections);
}

TEST_F(RoleAdmissionTest, FenceReadAfterScopeEndsCannotBorrowReplacementState) {
    delayFence = true;
    Enter(); CompleteRegister(0);
    ASSERT_EQ(1u, fenceReads.size());
    scope.reset();
    fenceReads[0]();
    EXPECT_EQ(0u, fenceWrites); EXPECT_TRUE(activated.empty());
}

TEST_F(RoleAdmissionTest, ContextIdentityMustMatchOriginalAdmissionAndSession) {
    Enter(); CompleteRegister(0);
    ASSERT_EQ(1u, activated.size());
    auto original = activated[0];
    ASSERT_TRUE(Admission::ConfirmedIdentityForContext(kPlayer, original));
    const auto parsed = handoff_sql_barrier::ParseFence(original.handoffFence);
    ASSERT_TRUE(parsed);
    EXPECT_EQ(original.roleAdmissionGeneration, parsed->generation);
    EXPECT_EQ(registers[0].identity().scene_instance_id(), parsed->sceneInstance);
    auto wrong = original;
    wrong.enterInfo.set_session_id(92);
    EXPECT_FALSE(Admission::ConfirmedIdentityForContext(kPlayer, wrong));
    Enter(); CompleteRegister(1);
    ASSERT_EQ(2u, activated.size());
    EXPECT_FALSE(Admission::ConfirmedIdentityForContext(kPlayer, original));
    EXPECT_TRUE(Admission::ConfirmedIdentityForContext(kPlayer, activated[1]));
    EXPECT_GT(activated[1].roleAdmissionGeneration, original.roleAdmissionGeneration);
    EXPECT_NE(activated[1].handoffFence, original.handoffFence);
}

TEST_F(RoleAdmissionTest, SealRevokesSubmittedRegistrationBeforeItsReplyCanActivate) {
    delayFenceReply = true;
    Enter(); CompleteRegister(0);
    ASSERT_EQ(1u, fenceWrites); ASSERT_EQ(1u, fenceReplies.size());
    EXPECT_TRUE(activated.empty());
    ASSERT_TRUE(Admission::Seal(SealFor()));
    ASSERT_EQ(1u, revokedCandidates.size()); EXPECT_EQ(fenceCandidates[0], revokedCandidates[0]);
    fenceReplies[0]();
    EXPECT_TRUE(activated.empty());
    EXPECT_FALSE(Admission::Allows(kPlayer, registers[0].identity().generation(), kRevision));
}

// 会话结束凭证(location-release-binding.md §3.3,2026-10-23):本节点完全不持有时凭证要先读 handoff_fence 原件再 CAS。
// 登记在途时原件可能正是这条登记刚写的 H —— 一条正在建立的持有 —— 必须算作"本节点可能持有",既不读也不写;
// 登记结清(Activate 之后由待入场条目 / 实体表示持有)才不再算。核对原件 S 用的本进程实例号与登记写入 S 同一来源。
TEST_F(RoleAdmissionTest, RegistrationInFlightBlocksSessionEndedCredential) {
    EXPECT_FALSE(Admission::HasAdmissionInFlight(kPlayer));
    EXPECT_EQ("01234567-89ab-cdef-0123-456789abcdef", Admission::LocalInstanceId());
    const auto skipLocalHolder = static_cast<std::size_t>(release_fence::SessionEndedCredential::kSkipLocalHolder);
    const auto releaseWhileInFlight = [&](const char* stage) {
        const auto before = release_fence_stats::Read();
        PlayerLifecycleSystem::HandleSessionEndedRelease(kPlayer, Context().ownerEpoch, 0, 0);
        const auto after = release_fence_stats::Read();
        EXPECT_EQ(after.credentialDecisions[skipLocalHolder], before.credentialDecisions[skipLocalHolder] + 1) << stage;
        EXPECT_EQ(after.credentialQualifications, before.credentialQualifications) << stage << ":不读资格原件";
        EXPECT_EQ(after.credentialWritten + after.credentialEpochMoved + after.credentialSuperseded + after.credentialFailed,
                  before.credentialWritten + before.credentialEpochMoved + before.credentialSuperseded + before.credentialFailed)
            << stage << ":不写凭证";
    };

    delayFence = true;
    ASSERT_NO_FATAL_FAILURE(Enter());
    EXPECT_TRUE(Admission::HasAdmissionInFlight(kPlayer)) << "SQL 登记在途";
    releaseWhileInFlight("sql registration");

    ASSERT_NO_FATAL_FAILURE(CompleteRegister(0));
    ASSERT_EQ(1u, fenceReads.size());
    EXPECT_TRUE(Admission::HasAdmissionInFlight(kPlayer)) << "handoff_fence 登记在途(prior 读 / CAS 未回)";
    releaseWhileInFlight("fence registration");

    fenceReads[0]();
    ASSERT_EQ(1u, activated.size());
    EXPECT_FALSE(Admission::HasAdmissionInFlight(kPlayer)) << "登记已结清,持有改由待入场条目 / 实体表示";
    const auto parsed = handoff_sql_barrier::ParseFence(activated[0].handoffFence);
    ASSERT_TRUE(parsed);
    EXPECT_EQ(Admission::LocalInstanceId(), parsed->sceneInstance) << "凭证核对 S 与登记写入 S 必须同一来源";
}
