#include "role_admission_system.h"
#include "player/system/handoff_sql_barrier.h"

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <limits>
#include <map>
#include <optional>
#include <string>
#include <utility>
#include <vector>
#include "core/security/service_call_auth.h"
#include "core/system/redis.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo_grpc/channel.h"
#include "network/node_utils.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_scene.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/base/node.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "thread_context/redis_manager.h"
#include "time/system/time.h"

struct RoleAdmissionState {
    struct Role {
    common::base::RoleAdmissionIdentity identity;
    common::base::RoleAdmissionIdentity confirmedIdentity;
    PlayerEnterContext context;
    uint64_t authorized{0}, sealedThrough{0}, closedThrough{0};
    uint64_t requiredEpoch{0}, requiredSeq{0};
    uint32_t registerCalls{0}, callbacks{0}, attempts{0};
    bool waitingFence{false}, fenceInFlight{false};
    uint32_t fenceAttempts{0};
    std::optional<std::string> fencePrior;
    std::string fenceRevocation;
    bool pendingFenceRevoke{false}, fenceRevokeInFlight{false};
    bool waitingRegister{false}, controlInFlight{false}, sqlInFlight{false}, sqlReady{false}, drained{false};
    bool tradeSaveRequired{false}, tradeSaveQueued{false};
    std::optional<common::base::RoleTradeSeal> seal;
    std::optional<common::base::RoleTradeSeal> completedSeal;
    RoleAdmissionSystem::Clock::time_point retryAt{};
    RoleAdmissionSystem::Clock::time_point registerDeadline{};
    // 只读应用确认至多一个physical命令；超时交付false后仍保槽，直到原transport回调解除。
    uint64_t snapshotSqlSequence{0}, snapshotSqlInFlight{0};
    RoleAdmissionSystem::Clock::time_point snapshotSqlDeadline{};
    std::function<void(std::optional<RoleAdmissionSystem::SqlAck>)> snapshotSqlCompletion;
    };
    RoleAdmissionSystem::Dependencies dependencies;
    std::map<Guid, Role> roles;
    uint64_t nextGeneration{0};
    bool timerArmed{false};
};
namespace {
using Clock = RoleAdmissionSystem::Clock;
using Role = RoleAdmissionState::Role;
constexpr std::string_view kMethod = "/data_service.DataService/";
constexpr const char* kSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";
thread_local auto state = std::make_shared<RoleAdmissionState>();
Clock::time_point Now() { return state->dependencies.now ? state->dependencies.now() : Clock::now(); }
RoleAdmissionSystem::LocalNode ReadLocalNode() {
    if (state->dependencies.localNode) return state->dependencies.localNode();
    return {GetNodeInfo().node_uuid(), std::to_string(GetNodeInfo().node_id()), GetZoneId()};
}
void Reject(Guid player, uint32_t session) {
    if (state->dependencies.reject) state->dependencies.reject(player, session);
    else PlayerLifecycleSystem::RejectRoleAdmission(player, session);
}
void Activate(Guid player, PlayerEnterContext context) {
    if (state->dependencies.activate) { state->dependencies.activate(player, std::move(context)); return; }
    if (PlayerLifecycleSystem::HandleEnterGameNode(player, std::move(context)) == PlayerLifecycleSystem::EnterGameNodeOutcome::kLoadRequired) {
        if (const auto& client = tlsRedisSystem.GetPlayerDataRedis()) client->AsyncLoad(player);
    }
}
void Freeze(Guid player) {
    if (state->dependencies.freeze) state->dependencies.freeze(player);
    else PlayerLifecycleSystem::FreezeRoleForTrade(player);
}
bool Owns(const std::weak_ptr<RoleAdmissionState>& original) { return original.lock() == state; }

entt::entity ReadyNode() {
    auto& registry = tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService);
    for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>()) {
        const auto& channel = registry.get<MuduoGrpcChannelPtr>(entity);
        if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady) return entity;
    }
    return entt::null;
}
template<class Request> bool Sign(Request& request, std::string_view method) {
    const char* secret = std::getenv(kSecretEnv);
    return secret != nullptr && service_call_auth::Sign(std::string(kMethod) + std::string(method), request,
        *request.mutable_auth(), "scene", secret, TimeSystem::NowMillisecondsUTC());
}
void ArmTimer() {
    if (state->timerArmed) return;
    if (auto* loop = muduo::net::EventLoop::getEventLoopOfCurrentThread()) {
        state->timerArmed = true;
        // 无对象捕获，所有状态属于本逻辑线程；不靠墙钟/租约结束交易冻结。
        const std::weak_ptr<RoleAdmissionState> original = state;
        loop->runEvery(0.5, [original] { if (Owns(original)) RoleAdmissionSystem::Tick(); });
    }
}
// Redis 适配器不持有连接；HGET 原件随后只作为同一候选的 CAS 输入。
// 上层弱 state/原 generation 在两次异步边界均复核，迟到完成不会激活新实体。
bool RegisterFence(Guid player, std::string candidate, std::optional<std::string> expected,
    std::function<bool(const std::string&)> stillQualified, RoleAdmissionSystem::FenceReply completed) {
    const auto parsed = handoff_sql_barrier::ParseFence(candidate);
    if (!parsed || parsed->state != handoff_sql_barrier::FenceState::kHeld) return false;
    auto submit = [player, candidate, stillQualified, completed](std::string prior) {
        if (!stillQualified(prior)) { completed(false, prior); return; }
        auto& redis = tlsRedis.GetZoneRedis();
        if (!redis || !redis->connected()) { completed(false, prior); return; }
        const auto record = handoff_sql_barrier::ParseFence(candidate);
        const std::string owner = player_ownership::OwnerEpochRedisKey(player);
        const std::string barrier = "player:{" + std::to_string(player) + "}:save_barrier";
        const std::string mark = "player:" + std::to_string(player) + ":handoff";
        const auto epoch = std::to_string(record->ownerEpoch);
        const int sent = redis->command([completed, prior](hiredis::Hiredis*, redisReply* reply) {
            completed(reply && reply->type == REDIS_REPLY_INTEGER && reply->integer == 1, prior);
        }, "EVAL %s 3 %b %b %b %b %b %b", handoff_sql_barrier::kRegisterFenceLua.c_str(),
            owner.data(), owner.size(), barrier.data(), barrier.size(), mark.data(), mark.size(),
            epoch.data(), epoch.size(), candidate.data(), candidate.size(), prior.data(), prior.size());
        if (sent != REDIS_OK) completed(false, prior);
    };
    if (expected) { submit(*expected); return true; }
    auto& redis = tlsRedis.GetZoneRedis();
    if (!redis || !redis->connected()) return false;
    const std::string barrier = "player:{" + std::to_string(player) + "}:save_barrier";
    return redis->command([submit, completed](hiredis::Hiredis*, redisReply* reply) {
        if (reply && reply->type == REDIS_REPLY_NIL) submit("");
        else if (reply && reply->type == REDIS_REPLY_STRING && reply->str)
            submit(std::string(reply->str, reply->len));
        else completed(false, std::nullopt);
    }, "HGET %b handoff_fence", barrier.data(), barrier.size()) == REDIS_OK;
}
void CancelPendingFence(Guid playerId) {
    auto found = state->roles.find(playerId);
    if (found == state->roles.end()) return;
    auto& role = found->second;
    // 没有捕获prior，原HGET→CAS检查就不可能已放行；也不fresh-read重建撤销资格。
    if (!role.fencePrior) { role.pendingFenceRevoke = false; return; }
    role.pendingFenceRevoke = true;
    if (role.fenceRevokeInFlight) return;
    if (role.fenceRevocation.empty()) {
        const auto next = handoff_sql_barrier::RotateFence(role.context.handoffFence, handoff_sql_barrier::FenceState::kRevoked);
        if (!next) return;
        role.fenceRevocation = *next;
    }
    const auto candidate = role.context.handoffFence;
    const auto generation = role.identity.generation();
    const auto prior = *role.fencePrior;
    const auto revoked = role.fenceRevocation;
    const auto epoch = std::to_string(role.context.ownerEpoch);
    const std::weak_ptr<RoleAdmissionState> original = state;
    role.fenceRevokeInFlight = true; ++role.callbacks;
    auto completed = [original, playerId, generation, candidate](bool confirmed) {
        if (!Owns(original)) return;
        auto it = state->roles.find(playerId); if (it == state->roles.end()) return;
        auto& current = it->second;
        if (current.callbacks) --current.callbacks;
        if (current.identity.generation() != generation || current.context.handoffFence != candidate) return;
        current.fenceRevokeInFlight = false;
        if (confirmed) current.pendingFenceRevoke = false;
    };
    bool sent = false;
    if (state->dependencies.revokeHandoffFence) {
        sent = state->dependencies.revokeHandoffFence(playerId, candidate, prior, revoked, completed);
    } else if (auto& redis = tlsRedis.GetZoneRedis(); redis && redis->connected()) {
        sent = redis->command([completed](hiredis::Hiredis*, redisReply* reply) {
            completed(reply && reply->type == REDIS_REPLY_INTEGER && (reply->integer == 0 || reply->integer == 1));
        }, "EVAL %s 3 %s %s %s %s %b %b %b", handoff_sql_barrier::kCancelRegistrationLua.c_str(),
            player_ownership::OwnerEpochRedisKey(playerId).c_str(),
            ("player:{" + std::to_string(playerId) + "}:save_barrier").c_str(),
            ("player:" + std::to_string(playerId) + ":handoff").c_str(), epoch.c_str(),
            candidate.data(), candidate.size(), prior.data(), prior.size(), revoked.data(), revoked.size()) == REDIS_OK;
    }
    if (!sent) { role.fenceRevokeInFlight = false; --role.callbacks; }
}
void SendFence(Guid playerId) {
    auto found = state->roles.find(playerId);
    if (found == state->roles.end()) return;
    auto& role = found->second;
    if (!role.waitingFence || role.fenceInFlight) return;
    if (Now() >= role.registerDeadline || role.fenceAttempts >= 3) {
        role.waitingFence = false;
        CancelPendingFence(playerId);
        Reject(playerId, role.context.enterInfo.session_id()); return;
    }
    const auto generation = role.identity.generation();
    if (generation <= role.closedThrough || generation <= role.sealedThrough) { role.waitingFence = false; return; }
    role.fenceInFlight = true; ++role.fenceAttempts; ++role.callbacks;
    const std::weak_ptr<RoleAdmissionState> original = state;
    auto stillQualified = [original, playerId, generation, candidate = role.context.handoffFence](const std::string& prior) {
        if (!Owns(original)) return false;
        const auto it = state->roles.find(playerId);
        if (it == state->roles.end()) return false;
        auto& current = it->second;
        const bool allowed = current.waitingFence && current.identity.generation() == generation
            && current.context.handoffFence == candidate && generation > current.closedThrough
            && generation > current.sealedThrough && Now() < current.registerDeadline;
        if (!allowed || (current.fencePrior && *current.fencePrior != prior)) return false;
        current.fencePrior = prior; // 发CAS前固定，Seal可撤销尚在途的登记。
        return true;
    };
    auto completed = [original, playerId, generation](bool ok, std::optional<std::string> prior) {
        if (!Owns(original)) return;
        auto it = state->roles.find(playerId);
        if (it == state->roles.end()) return;
        auto& current = it->second;
        if (current.callbacks) --current.callbacks;
        if (current.identity.generation() != generation) return;
        current.fenceInFlight = false;
        if (!current.waitingFence) return;
        if (prior && !current.fencePrior) current.fencePrior = std::move(prior);
        if (Now() >= current.registerDeadline || generation <= current.closedThrough || generation <= current.sealedThrough) {
            current.waitingFence = false; CancelPendingFence(playerId); Reject(playerId, current.context.enterInfo.session_id()); return;
        }
        if (!ok) { current.retryAt = Now() + std::chrono::seconds(1); return; }
        current.waitingFence = false; current.authorized = generation;
        Activate(playerId, current.context);
    };
    const bool sent = state->dependencies.registerHandoffFence
        ? state->dependencies.registerHandoffFence(playerId, role.context.handoffFence, role.fencePrior, stillQualified, completed)
        : RegisterFence(playerId, role.context.handoffFence, role.fencePrior, stillQualified, completed);
    if (!sent) {
        role.fenceInFlight = false; --role.callbacks;
        role.retryAt = Now() + std::chrono::seconds(1);
    }
}
void SendRegister(Guid playerId) {
    auto it = state->roles.find(playerId);
    if (it == state->roles.end()) return;
    auto& role = it->second;
    if (!role.waitingRegister) return;
    if (Now() >= role.registerDeadline) {
        role.waitingRegister = false;
        Reject(playerId, role.context.enterInfo.session_id());
        return;
    }
    if (role.registerCalls != 0 || role.attempts >= 3) return;
    data_service::RegisterRoleAdmissionRequest request;
    *request.mutable_identity() = role.identity;
    const uint64_t generation = role.identity.generation();
    ++role.attempts; ++role.registerCalls;
    const std::weak_ptr<RoleAdmissionState> original = state;
    auto completed = [original, playerId, generation](bool ok, const data_service::RegisterRoleAdmissionResponse* reply) {
        if (!Owns(original)) return;
        auto found = state->roles.find(playerId);
        if (found == state->roles.end()) return;
        auto& current = found->second;
        if (current.registerCalls > 0) --current.registerCalls;
        if (current.identity.generation() != generation || !current.waitingRegister) return;
        current.retryAt = Now() + std::chrono::seconds(1);
        if (Now() >= current.registerDeadline) {
            current.waitingRegister = false;
            Reject(playerId, current.context.enterInfo.session_id());
            return;
        }
        if (!ok || reply == nullptr || reply->identity().SerializeAsString() != current.identity.SerializeAsString()
            || reply->closed_through() >= generation) {
            if (current.attempts >= 3) {
                current.waitingRegister = false;
                Reject(playerId, current.context.enterInfo.session_id());
            }
            return; // 未知结果只重放原generation，绝不先建实体。
        }
        current.waitingRegister = false;
        current.closedThrough = std::max(current.closedThrough, reply->closed_through());
        current.confirmedIdentity = current.identity;
        if (generation <= current.sealedThrough || generation <= current.closedThrough) return;
        if (current.seal && current.drained) {
            current.completedSeal = std::move(current.seal);
            current.seal.reset(); current.drained = false;
            current.tradeSaveRequired = false; current.tradeSaveQueued = false;
        }
        current.waitingFence = true;
        SendFence(playerId);
    };
    bool sent = false;
    if (state->dependencies.registerAdmission) sent = state->dependencies.registerAdmission(request, completed);
    else if (const auto node = ReadyNode(); node != entt::null && Sign(request, "RegisterRoleAdmission")) {
        data_service::SendDataServiceRegisterRoleAdmission(tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService), node, request,
            [completed](const GrpcCallOutcome& outcome, const data_service::RegisterRoleAdmissionResponse* reply) { completed(outcome.ok(), reply); });
        sent = true;
    }
    if (!sent) { --role.registerCalls; --role.attempts; role.retryAt = Now() + std::chrono::seconds(1); }
}
std::string Version(uint64_t epoch, uint64_t seq) {
    const auto first = std::to_string(epoch), second = std::to_string(seq);
    return std::string(20 - first.size(), '0') + first + ":" + std::string(20 - second.size(), '0') + second;
}
// 只读原子快照：不写handoff、没有35秒自动解冻。字符串版本保留uint64精度。
constexpr const char* kReadSqlAck = R"(
    if redis.call('TYPE',KEYS[1]).ok~='string' or redis.call('TYPE',KEYS[2]).ok~='hash' then return {} end
    local a=redis.call('HMGET',KEYS[2],'pending','player_database','player_database_1')
    return {redis.call('GET',KEYS[1]),a[1],a[2],a[3]}
)";
bool SqlReady(const RoleAdmissionSystem::SqlAck& reply, const std::string& floor) {
    const auto& owner = reply.ownerEpoch; const auto& pending = reply.pending;
    if (owner.empty() || owner.size() > 20 || owner.front() == '0'
        || !std::all_of(owner.begin(), owner.end(), [](char c) { return c >= '0' && c <= '9'; })
        || (owner.size() == 20 && owner > "18446744073709551615") || pending.size() != 41 || pending[20] != ':'
        || !std::all_of(pending.begin(), pending.begin() + 20, [](char c) { return c >= '0' && c <= '9'; })
        || !std::all_of(pending.begin() + 21, pending.end(), [](char c) { return c >= '0' && c <= '9'; })
        || pending.substr(0,20) != std::string(20 - owner.size(), '0') + owner
        || pending.substr(21) == std::string(20, '0') || pending.substr(21) > "18446744073709551615") return false;
    return pending == reply.playerDatabase && pending == reply.playerDatabase1 && pending >= floor;
}
bool ReadSqlAck(Guid playerId, std::function<void(std::optional<RoleAdmissionSystem::SqlAck>)> completed) {
    bool sent = false;
    if (state->dependencies.readSqlAck) sent = state->dependencies.readSqlAck(playerId, completed);
    else if (auto& redis = tlsRedis.GetZoneRedis(); redis && redis->connected()) {
        const std::string owner = player_ownership::OwnerEpochRedisKey(playerId);
        const std::string barrier = "player:{" + std::to_string(playerId) + "}:save_barrier";
        sent = redis->command([completed](hiredis::Hiredis*, redisReply* reply) {
            if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 4) { completed(std::nullopt); return; }
            std::vector<std::string> values;
            for (size_t i = 0; i < 4; ++i) {
                const auto* value = reply->element[i];
                if (value == nullptr || value->type != REDIS_REPLY_STRING || value->str == nullptr) { completed(std::nullopt); return; }
                values.emplace_back(value->str, value->len);
            }
            completed(RoleAdmissionSystem::SqlAck{values[0], values[1], values[2], values[3]});
        }, "EVAL %s 2 %b %b", kReadSqlAck, owner.data(), owner.size(), barrier.data(), barrier.size()) == REDIS_OK;
    }
    return sent;
}
void CheckSql(Guid playerId, Role& role) {
    if (role.sqlInFlight || role.sqlReady) return;
    if (role.requiredEpoch == 0 && role.requiredSeq == 0) { role.sqlReady = true; return; }
    const auto expected = std::pair{role.requiredEpoch, role.requiredSeq};
    role.sqlInFlight = true;
    const std::weak_ptr<RoleAdmissionState> original = state;
    auto completed = [original, playerId, expected](std::optional<RoleAdmissionSystem::SqlAck> reply) {
        if (!Owns(original)) return;
        auto found = state->roles.find(playerId);
        if (found == state->roles.end()) return;
        auto& current = found->second; current.sqlInFlight = false;
        if (expected == std::pair{current.requiredEpoch, current.requiredSeq}) {
            current.sqlReady = reply && SqlReady(*reply, Version(expected.first, expected.second));
        }
    };
    const bool sent = ReadSqlAck(playerId, std::move(completed));
    if (!sent) role.sqlInFlight = false;
}
void CloseOrAck(Guid playerId, Role& role) {
    if (role.controlInFlight) return;
    if (role.seal && !role.drained) {
        if (role.tradeSaveRequired && !role.tradeSaveQueued) return;
        CheckSql(playerId, role);
        if (!role.sqlReady) return;
        data_service::AckRoleTradeDrainRequest request;
        *request.mutable_seal() = *role.seal;
        request.set_required_owner_epoch(role.requiredEpoch); request.set_required_save_seq(role.requiredSeq);
        const auto listing = role.seal->listing_id(); const auto through = role.seal->through_generation();
        role.controlInFlight = true;
        const std::weak_ptr<RoleAdmissionState> original = state;
        auto completed = [original, playerId, listing, through](bool ok) {
                if (!Owns(original)) return;
                auto found = state->roles.find(playerId); if (found == state->roles.end()) return;
                auto& current = found->second; current.controlInFlight = false;
                if (ok && current.seal && current.seal->listing_id() == listing && current.seal->through_generation() == through) {
                    current.drained = true; current.closedThrough = std::max(current.closedThrough, through);
                }
            };
        bool sent = false;
        if (state->dependencies.ackDrain) sent = state->dependencies.ackDrain(request, completed);
        else if (const auto node = ReadyNode(); node != entt::null && Sign(request, "AckRoleTradeDrain")) {
            data_service::SendDataServiceAckRoleTradeDrain(tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService), node,
                request, [completed](const GrpcCallOutcome& outcome, const google::protobuf::Empty*) { completed(outcome.ok()); });
            sent = true;
        }
        if (!sent) role.controlInFlight = false;
        return;
    }
    if (role.seal) return; // 本轮 holder 已 ACK；未被 SQL 受理的新候选不再补 Close。
    const uint64_t through = role.identity.generation();
    if (through == 0 || role.closedThrough >= through) return;
    data_service::CloseRoleAdmissionsRequest request;
    request.set_player_id(playerId); request.set_scene_instance_id(role.identity.scene_instance_id());
    request.set_owner_revision(role.identity.owner_revision()); request.set_through_generation(through);
    request.set_required_owner_epoch(role.requiredEpoch); request.set_required_save_seq(role.requiredSeq);
    role.controlInFlight = true;
    const std::weak_ptr<RoleAdmissionState> original = state;
    auto completed = [original, playerId, through](bool ok, const data_service::CloseRoleAdmissionsResponse* reply) {
            if (!Owns(original)) return;
            auto found = state->roles.find(playerId); if (found == state->roles.end()) return;
            auto& current = found->second; current.controlInFlight = false;
            if (ok && reply != nullptr && reply->closed_through() >= through) {
                current.closedThrough = std::max(current.closedThrough, through);
            }
        };
    bool sent = false;
    if (state->dependencies.closeAdmissions) sent = state->dependencies.closeAdmissions(request, completed);
    else if (const auto node = ReadyNode(); node != entt::null && Sign(request, "CloseRoleAdmissions")) {
        data_service::SendDataServiceCloseRoleAdmissions(tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService), node, request,
            [completed](const GrpcCallOutcome& outcome, const data_service::CloseRoleAdmissionsResponse* reply) { completed(outcome.ok(), reply); });
        sent = true;
    }
    if (!sent) role.controlInFlight = false;
}
} // namespace

RoleAdmissionSystem::ScopedDependencies::ScopedDependencies(Dependencies dependencies) : previous_(state) {
    state = std::make_shared<RoleAdmissionState>();
    state->dependencies = std::move(dependencies);
    for (const auto& identity : state->dependencies.confirmedAdmissions) {
        // 夹具原件必须明确角色、世代与修订；无原件与零字段仍统一拒绝。
        if (identity.player_id() == 0 || identity.generation() == 0 || identity.owner_revision() == 0) continue;
        auto& role = state->roles[identity.player_id()];
        role.identity = identity;
        role.confirmedIdentity = identity;
        role.authorized = identity.generation();
        state->nextGeneration = std::max(state->nextGeneration, identity.generation());
    }
}
RoleAdmissionSystem::ScopedDependencies::~ScopedDependencies() { state = std::move(previous_); }

void RoleAdmissionSystem::RequestEnter(Guid playerId, PlayerEnterContext context) {
    ArmTimer();
    const auto session = context.enterInfo.session_id();
    const auto scene = PlayerSceneSystem::FindSceneById(context.enterInfo.scene_id());
    const auto entity = tlsEcs.GetPlayer(playerId);
    const auto allowedScene = entity == entt::null ? PlayerSceneSystem::CheckEnterScene(playerId, scene)
        : PlayerSceneSystem::CheckEnterScene(entity, scene);
    const auto local = ReadLocalNode();
    if (playerId == 0 || context.ownerRevision == 0 || context.ownerEpoch == 0 || context.loginGeneration == 0 || session == 0
        || allowedScene != kSuccess || local.instanceId.empty() || state->nextGeneration == UINT64_MAX
        || !enter_fence::Admits(PlayerLifecycleSystem::JudgeEnter(playerId, session, context.loginGeneration))) {
        Reject(playerId, session); return;
    }
    auto& role = state->roles[playerId];
    if (role.controlInFlight || (role.seal && !role.drained)) { Reject(playerId, session); return; }
    if (role.waitingRegister || role.waitingFence) {
        // 原未知登记不得换世代重建；相同原路由由计时器重投。
        if (role.context.ownerRevision != context.ownerRevision || role.context.ownerEpoch != context.ownerEpoch
            || role.context.loginGeneration != context.loginGeneration || role.context.enterInfo.SerializeAsString() != context.enterInfo.SerializeAsString()) {
            Reject(playerId, session);
        }
        return;
    }
    if (role.registerCalls != 0 || role.fenceInFlight || role.pendingFenceRevoke) { Reject(playerId, session); return; }
    context.roleAdmissionGeneration = ++state->nextGeneration;
    const auto admissionToken = handoff_sql_barrier::NewFenceToken();
    const auto candidate = handoff_sql_barrier::EncodeFence({context.ownerEpoch, local.instanceId,
        context.roleAdmissionGeneration, admissionToken, handoff_sql_barrier::FenceState::kHeld,
        handoff_sql_barrier::NewFenceToken()});
    if (!candidate) { Reject(playerId, session); return; }
    context.handoffFence = *candidate;
    role.context = context; role.waitingRegister = true; role.attempts = 0;
    role.waitingFence = false; role.fenceInFlight = false; role.fenceAttempts = 0; role.fencePrior.reset();
    role.fenceRevocation.clear(); role.pendingFenceRevoke = false; role.fenceRevokeInFlight = false;
    role.registerDeadline = Now() + std::chrono::seconds(15);
    auto& identity = role.identity;
    identity.set_player_id(playerId); identity.set_scene_instance_id(local.instanceId);
    identity.set_generation(context.roleAdmissionGeneration); identity.set_owner_revision(context.ownerRevision);
    identity.set_owner_epoch(context.ownerEpoch); identity.set_scene_node_id(local.nodeId);
    identity.set_scene_id(context.enterInfo.scene_id()); identity.set_zone_id(local.zoneId);
    identity.set_session_id(session); identity.set_login_generation(context.loginGeneration);
    SendRegister(playerId);
}
bool RoleAdmissionSystem::Allows(Guid playerId, uint64_t generation, uint64_t revision) {
    if (generation == 0 || revision == 0) return false;
    auto it = state->roles.find(playerId);
    return it != state->roles.end() && it->second.authorized == generation
        && generation > it->second.sealedThrough && generation > it->second.closedThrough
        && it->second.confirmedIdentity.owner_revision() == revision;
}
bool RoleAdmissionSystem::IsSealed(Guid playerId) {
    auto it = state->roles.find(playerId);
    return it != state->roles.end() && it->second.sealedThrough != 0
        && it->second.authorized <= it->second.sealedThrough;
}
std::optional<common::base::RoleAdmissionIdentity> RoleAdmissionSystem::ConfirmedIdentity(Guid playerId) {
    const auto found = state->roles.find(playerId);
    if (found == state->roles.end()) return std::nullopt;
    const auto& identity = found->second.confirmedIdentity;
    if (!Allows(playerId, identity.generation(), identity.owner_revision())) return std::nullopt;
    return identity;
}
std::optional<common::base::RoleAdmissionIdentity> RoleAdmissionSystem::ConfirmedIdentityForContext(
    Guid playerId, const PlayerEnterContext& context) {
    if (!Allows(playerId, context.roleAdmissionGeneration, context.ownerRevision)) return std::nullopt;
    const auto found = state->roles.find(playerId);
    if (found == state->roles.end()) return std::nullopt;
    const auto& identity = found->second.confirmedIdentity;
    if (identity.player_id() != playerId || identity.generation() != context.roleAdmissionGeneration
        || identity.owner_revision() != context.ownerRevision || identity.owner_epoch() != context.ownerEpoch
        || identity.session_id() != context.enterInfo.session_id()
        || identity.login_generation() != context.loginGeneration
        || identity.scene_id() != context.enterInfo.scene_id() || identity.scene_instance_id().empty()) {
        return std::nullopt;
    }
    return identity;
}
std::function<void()> RoleAdmissionSystem::CaptureSnapshotCommit(entt::entity player,
    std::shared_ptr<const PlayerAllData> source, uint64_t epoch, uint64_t seq) {
    if (!tlsEcs.actorRegistry.valid(player) || !source || epoch == 0 || seq == 0 || source->last_save_seq() != seq) return {};
    const auto playerId = tlsEcs.GetPlayerId(player);
    const auto identity = ConfirmedIdentity(playerId);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    const auto* admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
    if (playerId == 0 || tlsEcs.GetPlayer(playerId) != player || !identity || identity->scene_instance_id().empty()
        || owner == nullptr || owner->epoch != epoch || admission == nullptr
        || admission->generation != identity->generation() || admission->ownerRevision != identity->owner_revision()
        || source->player_database_data().player_id() != playerId || source->player_database_1_data().player_id() != playerId
        || source->player_database_data().fence_owner_epoch() != 0 || source->player_database_data().fence_save_seq() != 0
        || source->player_database_1_data().fence_owner_epoch() != 0 || source->player_database_1_data().fence_save_seq() != 0) return {};
    const auto expectedIdentity = identity->SerializeAsString();
    const std::weak_ptr<RoleAdmissionState> original = state;
    const std::weak_ptr<const PlayerAllData> originalSource = source;
    return [original, originalSource, player = player, playerId, expectedIdentity, epoch, seq] {
        if (!Owns(original) || !tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayer(playerId) != player
            || tlsEcs.GetPlayerId(player) != playerId) return;
        const auto currentIdentity = ConfirmedIdentity(playerId);
        const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
        const auto* admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
        const auto payload = originalSource.lock();
        if (!payload || !currentIdentity || currentIdentity->SerializeAsString() != expectedIdentity
            || owner == nullptr || owner->epoch != epoch || admission == nullptr
            || admission->generation != currentIdentity->generation()
            || admission->ownerRevision != currentIdentity->owner_revision()) return;
        tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).RememberRedisCommit(payload, epoch, seq);
    };
}
bool RoleAdmissionSystem::ConfirmSnapshotSqlCommit(entt::entity player, const PlayerAllData& snapshot,
                                                  std::function<void(bool)> completed) {
    if (!completed || !tlsEcs.actorRegistry.valid(player)) return false;
    const auto playerId = tlsEcs.GetPlayerId(player);
    if (playerId == 0 || tlsEcs.GetPlayer(playerId) != player) return false;
    const auto* stored = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    const auto* admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
    const auto identity = ConfirmedIdentity(playerId);
    const auto& first = snapshot.player_database_data();
    const auto& second = snapshot.player_database_1_data();
    const uint64_t epoch = stored ? stored->committed_owner_epoch : 0, seq = stored ? stored->committed_save_seq : 0;
    if (stored == nullptr || stored->snapshot.get() != &snapshot || owner == nullptr || admission == nullptr
        || !identity || identity->player_id() != playerId || identity->scene_instance_id().empty()
        || admission->generation != identity->generation() || admission->ownerRevision != identity->owner_revision()
        || first.player_id() != playerId || second.player_id() != playerId || epoch == 0 || seq == 0
        || owner->epoch != epoch || first.fence_owner_epoch() != 0 || first.fence_save_seq() != 0
        || second.fence_owner_epoch() != 0 || second.fence_save_seq() != 0) return false;
    std::string bytes;
    if (!snapshot.SerializeToString(&bytes)) return false;
    const auto expectedIdentity = identity->SerializeAsString();
    const auto version = Version(epoch, seq);
    const std::weak_ptr<RoleAdmissionState> original = state;
    auto& role = state->roles.at(playerId);
    if (role.snapshotSqlInFlight != 0 || role.snapshotSqlSequence == UINT64_MAX) return false;
    const auto requestId = ++role.snapshotSqlSequence;
    role.snapshotSqlInFlight = requestId;
    role.snapshotSqlDeadline = Now() + std::chrono::seconds(10);
    ArmTimer();
    const auto finished = std::make_shared<bool>(false);
    auto reply = [original, player = player, playerId, epoch, seq, version, bytes = std::move(bytes), expectedIdentity,
                  finished, completed = std::move(completed)](std::optional<SqlAck> ack) {
        if (*finished) return;
        *finished = true;
        if (!Owns(original)) return; // 原RoleState已消失，不向新宿主交回旧资格。
        auto found = state->roles.find(playerId);
        if (found == state->roles.end()) return;
        const auto currentIdentity = ConfirmedIdentity(playerId);
        if (!tlsEcs.actorRegistry.valid(player) || tlsEcs.GetPlayer(playerId) != player
            || tlsEcs.GetPlayerId(player) != playerId || !currentIdentity
            || currentIdentity->SerializeAsString() != expectedIdentity) { completed(false); return; }
        const auto* currentOwner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
        const auto* currentAdmission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
        const auto* current = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
        std::string currentBytes;
        if (currentOwner == nullptr || currentOwner->epoch != epoch || currentAdmission == nullptr
            || currentAdmission->generation != currentIdentity->generation()
            || currentAdmission->ownerRevision != currentIdentity->owner_revision()
            || current == nullptr || !current->HasSnapshot() || current->committed_owner_epoch != epoch
            || current->committed_save_seq != seq || !current->snapshot->SerializeToString(&currentBytes)
            || currentBytes != bytes) { completed(false); return; }
        completed(ack && ack->ownerEpoch == std::to_string(epoch) && ack->pending == version && SqlReady(*ack, version));
    };
    role.snapshotSqlCompletion = std::move(reply);
    const bool sent = ReadSqlAck(playerId, [original, playerId, requestId](std::optional<SqlAck> ack) {
        if (!Owns(original)) return;
        auto found = state->roles.find(playerId);
        if (found == state->roles.end() || found->second.snapshotSqlInFlight != requestId) return;
        found->second.snapshotSqlInFlight = 0;
        auto completed = std::move(found->second.snapshotSqlCompletion);
        // 超时已释放应用闭包；迟到transport只解除原槽，不能重新交付旧资格。
        if (completed) completed(std::move(ack));
    });
    const bool completedSynchronously = *finished;
    if (!sent && !completedSynchronously) {
        *finished = true;
        if (Owns(original)) {
            auto found = state->roles.find(playerId);
            if (found != state->roles.end() && found->second.snapshotSqlInFlight == requestId) {
                found->second.snapshotSqlInFlight = 0;
                found->second.snapshotSqlCompletion = {};
            }
        }
    }
    // 依赖若违约在同步回调后又返回false，也不能向调用方伪报“尚未接管”并二次收尾。
    return sent || completedSynchronously;
}
bool RoleAdmissionSystem::Seal(const common::base::RoleTradeSeal& seal) {
    const auto local = ReadLocalNode();
    if (seal.player_id() == 0 || seal.listing_id() == 0 || seal.owner_revision() == 0 || seal.through_generation() == 0
        || seal.scene_instance_id() != local.instanceId || seal.scene_node_id() != local.nodeId
        || seal.zone_id() != local.zoneId) return false;
    ArmTimer();
    auto found = state->roles.find(seal.player_id());
    // 没有本进程登记原件不能声称排空，保留SQL侧FREEZING等待核实。
    if (found == state->roles.end()) return false;
    auto& role = found->second;
    if (role.completedSeal && role.completedSeal->listing_id() == seal.listing_id()) {
        return role.completedSeal->SerializeAsString() == seal.SerializeAsString();
    }
    if (role.seal && role.seal->listing_id() == seal.listing_id()) {
        if (role.seal->SerializeAsString() != seal.SerializeAsString()) return false;
        if (role.tradeSaveRequired && !role.tradeSaveQueued) Freeze(seal.player_id());
        Tick();
        return true;
    }
    // 已完成的原件只由 completedSeal 精确判重；未知旧请求不借关闭水位伪装成已确认原件。
    const auto matches = [&seal](const common::base::RoleAdmissionIdentity& identity) {
        return identity.generation() == seal.through_generation() && identity.owner_revision() == seal.owner_revision();
    };
    // Begin 可在下一次 Register 到达 SQL 前固定上一已确认世代；不能因本地已发新登记而漏封旧实体。
    if (!matches(role.identity) && !matches(role.confirmedIdentity)) return false;
    if (role.seal && !role.drained) return false;
    role.seal = seal; role.drained = false; role.sqlReady = false;
    role.tradeSaveRequired = tlsEcs.GetPlayer(seal.player_id()) != entt::null;
    role.tradeSaveQueued = false;
    role.sealedThrough = std::max(role.sealedThrough, seal.through_generation()); role.waitingRegister = false;
    if (role.identity.generation() <= role.sealedThrough) {
        role.waitingFence = false;
        CancelPendingFence(seal.player_id());
    }
    role.retryAt = Now();
    Freeze(seal.player_id());
    Tick(); return true;
}
bool RoleAdmissionSystem::AcceptSeal(const SealAccountRoleTradeRequest& request) {
    const char* secret = std::getenv("MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE");
    if (secret == nullptr || !service_call_auth::Verify("/scene_node.SceneNodeGrpc/SealAccountRoleTrade", request,
            request.auth(), "trade", secret, TimeSystem::NowMillisecondsUTC())) return false;
    return Seal(request.seal());
}
void RoleAdmissionSystem::ObserveSave(Guid playerId, uint64_t epoch, uint64_t seq) {
    auto found = state->roles.find(playerId); if (found == state->roles.end() || epoch == 0 || seq == 0) return;
    auto& current = found->second;
    if (std::pair{epoch, seq} > std::pair{current.requiredEpoch, current.requiredSeq}) {
        current.requiredEpoch = epoch; current.requiredSeq = seq; current.sqlReady = false;
    }
}
void RoleAdmissionSystem::ObserveTradeSaveQueued(Guid playerId, uint64_t epoch, uint64_t seq) {
    auto found = state->roles.find(playerId);
    if (found == state->roles.end() || epoch == 0 || seq == 0) return;
    auto& role = found->second;
    if (!role.seal || role.drained || !role.tradeSaveRequired) return;
    ObserveSave(playerId, epoch, seq);
    role.tradeSaveQueued = true;
}
void RoleAdmissionSystem::BeginCallback(Guid playerId) { auto it = state->roles.find(playerId); if (it != state->roles.end()) ++it->second.callbacks; }
void RoleAdmissionSystem::EndCallback(Guid playerId) { auto it = state->roles.find(playerId); if (it != state->roles.end() && it->second.callbacks > 0) --it->second.callbacks; }
bool RoleAdmissionSystem::HasAdmissionInFlight(Guid playerId) {
    const auto found = state->roles.find(playerId);
    if (found == state->roles.end()) return false;
    const auto& role = found->second;
    // 偏保守:任何一项登记相关的异步步骤没结清都算在途(误判只让这一次凭证不写,不会写错)。
    return role.waitingRegister || role.registerCalls != 0 || role.waitingFence || role.fenceInFlight
        || role.pendingFenceRevoke || role.fenceRevokeInFlight || role.callbacks != 0;
}
std::string RoleAdmissionSystem::LocalInstanceId() { return ReadLocalNode().instanceId; }
void RoleAdmissionSystem::OnDestroyed(Guid) { ArmTimer(); }
void RoleAdmissionSystem::Tick() {
    const auto originalState = state;
    for (auto& [playerId, role] : state->roles) {
        if (role.snapshotSqlInFlight != 0 && role.snapshotSqlCompletion && Now() >= role.snapshotSqlDeadline) {
            auto expired = std::move(role.snapshotSqlCompletion);
            expired(std::nullopt); // 不释放physical槽，不再排新命令；迟到回包只清槽。
            if (state != originalState) return;
        }
        if (role.retryAt > Now()) continue;
        role.retryAt = Now() + std::chrono::seconds(1);
        if (role.waitingRegister) { SendRegister(playerId); continue; }
        if (role.waitingFence) { SendFence(playerId); continue; }
        if (role.pendingFenceRevoke) { CancelPendingFence(playerId); continue; }
        if (role.registerCalls != 0 || role.callbacks != 0 || tlsEcs.GetPlayer(playerId) != entt::null) continue;
        const auto& client = tlsRedisSystem.GetPlayerDataRedis();
        if (state->dependencies.hasPendingLoad ? state->dependencies.hasPendingLoad(playerId)
            : client && client->HasPendingLoad(playerId)) continue;
        if (!PlayerLifecycleSystem::DrainSealedPendingEntry(playerId)) continue;
        CloseOrAck(playerId, role);
    }
}
