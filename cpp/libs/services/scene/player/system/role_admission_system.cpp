#include "role_admission_system.h"
#include "player/system/handoff_sql_barrier.h"
#include "core/async/coroutine_callback.h"
#include "core/system/scene_task_scope.h"

#include <boost/cobalt/task.hpp>

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <limits>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <utility>
#include <vector>
#include "core/security/service_call_auth.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo_grpc/channel.h"
#include "network/node_utils.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_store_client.h"
#include "player/system/player_scene.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/base/node.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"
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
    bool waitingRegister{false}, controlInFlight{false}, sqlReady{false}, drained{false};
    bool tradeSaveRequired{false}, tradeSaveQueued{false}, tradeFreezeSaving{false};
    std::optional<common::base::RoleTradeSeal> seal;
    std::optional<common::base::RoleTradeSeal> completedSeal;
    RoleAdmissionSystem::Clock::time_point retryAt{};
    RoleAdmissionSystem::Clock::time_point registerDeadline{};
    };
    RoleAdmissionSystem::Dependencies dependencies;
    std::map<Guid, Role> roles;
    uint64_t nextGeneration{0};
    bool timerArmed{false};
    bool hasDurableAdmissions{false};
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
        PlayerLifecycleSystem::RequestPlayerLoad(player);
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
namespace cobalt = boost::cobalt;
using muduo::net::EventLoop;
using loop_async::AwaitCallback;

struct FenceAttemptResult {
    bool accepted{false};
    bool registered{false};
    std::optional<std::string> prior;
};
struct AdmissionRegisterResult {
    bool accepted{false};
    std::optional<data_service::RegisterRoleAdmissionResponse> reply;
};

enum class AdmissionTransportKind : uint8_t { kRegister, kFence };
struct AdmissionTransport {
    std::weak_ptr<RoleAdmissionState> original;
    Guid playerId;
    uint64_t generation;
    AdmissionTransportKind kind;
    bool completed{false};
    bool readCompleted{false};

    // 实际终态或明确未接纳才归还计数。回调可能在本地等待取消之后到达。
    void Complete() {
        if (std::exchange(completed, true) || !Owns(original)) return;
        const auto it = state->roles.find(playerId);
        if (it == state->roles.end() || it->second.identity.generation() != generation) return;
        auto& role = it->second;
        if (kind == AdmissionTransportKind::kRegister) {
            if (role.registerCalls > 0) --role.registerCalls;
        } else {
            if (role.callbacks > 0) --role.callbacks;
            role.fenceInFlight = false;
        }
    }
};

// 恢复时只认原 Scene 的入场状态和原世代。先固定 prior，Seal 才能撤销尚在途的 CAS。
bool QualifyFence(const std::weak_ptr<RoleAdmissionState>& original, Guid playerId, uint64_t generation,
    const std::string& candidate, const std::string& prior) {
    if (!Owns(original)) return false;
    const auto it = state->roles.find(playerId);
    if (it == state->roles.end()) return false;
    auto& role = it->second;
    if (!role.waitingFence || role.identity.generation() != generation || role.context.handoffFence != candidate
        || generation <= role.closedThrough || generation <= role.sealedThrough || Now() >= role.registerDeadline
        || (role.fencePrior && *role.fencePrior != prior)) return false;
    role.fencePrior = prior;
    return true;
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
    } else if (tlsPlayerRedis.connected()) {
        sent = tlsPlayerRedis.command([completed](hiredis::Hiredis*, redisReply* reply) {
            completed(reply && reply->type == REDIS_REPLY_INTEGER && (reply->integer == 0 || reply->integer == 1));
        }, "EVAL %s 3 %s %s %s %s %b %b %b", handoff_sql_barrier::kCancelRegistrationLua.c_str(),
            player_ownership::OwnerEpochRedisKey(playerId).c_str(),
            player_ownership::HandoffFenceRedisKey(playerId).c_str(),
            player_ownership::HandoffRedisKey(playerId).c_str(), epoch.c_str(),
            candidate.data(), candidate.size(), prior.data(), prior.size(), revoked.data(), revoked.size()) == REDIS_OK;
    }
    if (!sent) { role.fenceRevokeInFlight = false; --role.callbacks; }
}
cobalt::task<void> RegisterFenceAttempt(Guid playerId) {
    const std::weak_ptr<RoleAdmissionState> original = state;
    uint64_t generation;
    std::string candidate;
    std::optional<std::string> prior;
    {
        const auto it = state->roles.find(playerId);
        if (it == state->roles.end()) co_return;
        auto& role = it->second;
        if (!role.waitingFence || role.fenceInFlight) co_return;
        if (Now() >= role.registerDeadline || role.fenceAttempts >= 3) {
            const auto session = role.context.NotifiableSessionId();
            role.waitingFence = false;
            CancelPendingFence(playerId);
            Reject(playerId, session);
            co_return;
        }
        generation = role.identity.generation();
        if (generation <= role.closedThrough || generation <= role.sealedThrough) {
            role.waitingFence = false;
            co_return;
        }
        candidate = role.context.handoffFence;
        prior = role.fencePrior;
        role.fenceInFlight = true;
        ++role.fenceAttempts;
        ++role.callbacks;
    }

    auto& loop = *EventLoop::getEventLoopOfCurrentThread();
    const auto transport = std::make_shared<AdmissionTransport>(
        AdmissionTransport{original, playerId, generation, AdmissionTransportKind::kFence});
    FenceAttemptResult result;
    if (const auto parsed = handoff_sql_barrier::ParseFence(candidate);
        parsed && parsed->state == handoff_sql_barrier::FenceState::kHeld) {
        const auto fenceKey = player_ownership::HandoffFenceRedisKey(playerId);
        if (!prior) {
            auto read = [&](auto done) {
                auto completed = [transport, done = std::move(done)](std::optional<std::string> old) {
                    if (std::exchange(transport->readCompleted, true)) return;
                    if (!SceneTaskScope::IsDurableAccepting()) transport->Complete();
                    done(FenceAttemptResult{true, false, std::move(old)});
                };
                bool sent = false;
                if (state->dependencies.readHandoffFence) {
                    sent = state->dependencies.readHandoffFence(playerId, std::move(completed));
                } else if (tlsPlayerRedis.connected()) {
                    sent = tlsPlayerRedis.command([completed = std::move(completed)](hiredis::Hiredis*, redisReply* reply) mutable {
                        std::optional<std::string> old;
                        if (reply && reply->type == REDIS_REPLY_NIL) old.emplace();
                        else if (reply && reply->type == REDIS_REPLY_STRING && reply->str) old.emplace(reply->str, reply->len);
                        completed(std::move(old));
                    }, "HGET %b handoff_fence", fenceKey.data(), fenceKey.size()) == REDIS_OK;
                }
                if (!sent) transport->Complete();
                return sent;
            };
            auto readResult = co_await AwaitCallback<FenceAttemptResult>(loop, std::move(read), FenceAttemptResult{});
            if (!readResult || !Owns(original)) co_return;
            if (!SceneTaskScope::IsDurableAccepting()) {
                transport->Complete(); // 读已完成，停止之前未提交 CAS。
                co_return;
            }
            result.accepted = readResult->accepted;
            prior = std::move(readResult->prior);
        }
        if (prior) {
            result.accepted = true;
            if (QualifyFence(original, playerId, generation, candidate, *prior)) {
                const auto ownerKey = player_ownership::OwnerEpochRedisKey(playerId);
                const auto markKey = player_ownership::HandoffRedisKey(playerId);
                const auto epoch = std::to_string(parsed->ownerEpoch);
                auto write = [&](auto done) {
                    auto completed = [transport, done = std::move(done)](bool registered) {
                        transport->Complete();
                        done(registered);
                    };
                    bool sent = false;
                    if (state->dependencies.writeHandoffFence) {
                        sent = state->dependencies.writeHandoffFence(playerId, candidate, *prior, std::move(completed));
                    } else if (tlsPlayerRedis.connected()) {
                        sent = tlsPlayerRedis.command([completed = std::move(completed)](hiredis::Hiredis*, redisReply* reply) {
                            completed(reply && reply->type == REDIS_REPLY_INTEGER && reply->integer == 1);
                        }, "EVAL %s 3 %b %b %b %b %b %b", handoff_sql_barrier::kRegisterFenceLua.c_str(),
                            ownerKey.data(), ownerKey.size(), fenceKey.data(), fenceKey.size(), markKey.data(), markKey.size(),
                            epoch.data(), epoch.size(), candidate.data(), candidate.size(), prior->data(), prior->size()) == REDIS_OK;
                    }
                    if (!sent) transport->Complete();
                    return sent;
                };
                const auto registered = co_await AwaitCallback<bool>(loop, std::move(write), false);
                if (!registered || !SceneTaskScope::IsDurableAccepting()) co_return;
                result.registered = *registered;
            }
            result.prior = std::move(prior);
        }
    }

    transport->Complete(); // HGET 失败或资格失效时不会提交 CAS。
    if (!Owns(original)) co_return;
    const auto it = state->roles.find(playerId);
    if (it == state->roles.end() || it->second.identity.generation() != generation) co_return;
    auto& role = it->second;
    if (!result.accepted) {
        role.retryAt = Now() + std::chrono::seconds(1);
        co_return;
    }
    if (!role.waitingFence) co_return;
    if (result.prior && !role.fencePrior) role.fencePrior = std::move(result.prior);
    if (Now() >= role.registerDeadline || generation <= role.closedThrough || generation <= role.sealedThrough) {
        const auto session = role.context.NotifiableSessionId();
        role.waitingFence = false;
        CancelPendingFence(playerId);
        Reject(playerId, session);
        co_return;
    }
    if (!result.registered) {
        role.retryAt = Now() + std::chrono::seconds(1);
        co_return;
    }
    role.waitingFence = false;
    role.authorized = generation;
    Activate(playerId, role.context);
}

cobalt::task<void> RegisterAdmissionAttempt(Guid playerId) {
    const std::weak_ptr<RoleAdmissionState> original = state;
    data_service::RegisterRoleAdmissionRequest request;
    uint64_t generation;
    {
        const auto it = state->roles.find(playerId);
        if (it == state->roles.end()) co_return;
        auto& role = it->second;
        if (!role.waitingRegister) co_return;
        if (Now() >= role.registerDeadline) {
            role.waitingRegister = false;
            Reject(playerId, role.context.NotifiableSessionId());
            co_return;
        }
        if (role.registerCalls != 0 || role.attempts >= 3) co_return;
        *request.mutable_identity() = role.identity;
        generation = role.identity.generation();
        ++role.attempts;
        ++role.registerCalls;
    }

    const auto transport = std::make_shared<AdmissionTransport>(
        AdmissionTransport{original, playerId, generation, AdmissionTransportKind::kRegister});
    auto submit = [&](auto done) {
        auto completed = [transport, done = std::move(done)](bool ok, const data_service::RegisterRoleAdmissionResponse* reply) {
            transport->Complete();
            AdmissionRegisterResult result{true, std::nullopt};
            if (ok && reply != nullptr) result.reply.emplace(*reply);
            done(std::move(result));
        };
        if (state->dependencies.registerAdmission) {
            const bool sent = state->dependencies.registerAdmission(request, std::move(completed));
            if (!sent) transport->Complete();
            return sent;
        }
        const auto node = ReadyNode();
        if (node == entt::null || !Sign(request, "RegisterRoleAdmission")) {
            transport->Complete();
            return false;
        }
        data_service::SendDataServiceRegisterRoleAdmission(tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService),
            node, request, [completed = std::move(completed)](const GrpcCallOutcome& outcome,
                const data_service::RegisterRoleAdmissionResponse* reply) mutable { completed(outcome.ok(), reply); });
        return true;
    };
    auto& loop = *EventLoop::getEventLoopOfCurrentThread();
    const auto result = co_await AwaitCallback<AdmissionRegisterResult>(loop, std::move(submit), AdmissionRegisterResult{});
    // 停机取消的只是本地等待，未到终态的登记不从账本退役。
    if (!result || !Owns(original) || !SceneTaskScope::IsDurableAccepting()) co_return;
    {
        const auto it = state->roles.find(playerId);
        if (it == state->roles.end()) co_return;
        auto& role = it->second;
        if (role.identity.generation() != generation) co_return;
        if (!result->accepted) {
            --role.attempts;
            role.retryAt = Now() + std::chrono::seconds(1);
            co_return;
        }
        if (!role.waitingRegister) co_return;
        role.retryAt = Now() + std::chrono::seconds(1);
        if (Now() >= role.registerDeadline) {
            role.waitingRegister = false;
            Reject(playerId, role.context.NotifiableSessionId());
            co_return;
        }
        const auto& reply = result->reply;
        if (!reply || reply->identity().SerializeAsString() != role.identity.SerializeAsString()
            || reply->closed_through() >= generation) {
            if (role.attempts >= 3) {
                role.waitingRegister = false;
                Reject(playerId, role.context.NotifiableSessionId());
            }
            co_return; // 未知结果只重放原 generation。
        }
        role.waitingRegister = false;
        role.closedThrough = std::max(role.closedThrough, reply->closed_through());
        role.confirmedIdentity = role.identity;
        if (generation <= role.sealedThrough || generation <= role.closedThrough) co_return;
        if (role.seal && role.drained) {
            role.completedSeal = std::move(role.seal);
            role.seal.reset();
            role.drained = false;
            role.tradeSaveRequired = false;
            role.tradeSaveQueued = false;
            role.tradeFreezeSaving = false;
        }
        role.waitingFence = true;
    }
    co_await RegisterFenceAttempt(playerId);
}

void SendRegister(Guid playerId) {
    (void)SceneTaskScope::TryStartDurable(RegisterAdmissionAttempt(playerId), "RegisterRoleAdmission");
}

void SendFence(Guid playerId) {
    (void)SceneTaskScope::TryStartDurable(RegisterFenceAttempt(playerId), "RegisterRoleHandoffFence");
}


bool HasUnsettledSave(Guid playerId) {
    return state->dependencies.hasUnsettledSave ? state->dependencies.hasUnsettledSave(playerId)
        : PlayerLifecycleSystem::IsPlayerSaveUnsettled(playerId);
}
// required 只在提交回调和认领时抬高,所以没有未落定的存盘就说明它已经提交了(规格 §6)。
// 走到这里实体已经销毁,不会再有新的存盘。
void CheckSql(Guid playerId, Role& role) {
    if (role.sqlReady) return;
    if (role.requiredEpoch == 0 && role.requiredSeq == 0) { role.sqlReady = true; return; }
    role.sqlReady = !HasUnsettledSave(playerId);
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
    // session 是本次入场的登录身份(托管落点时是断开的那次登录,照样登记进 SQL 原件);notify 是被拒时能推提示的会话,
    // 托管落点没有在场的客户端,为 0(player-connection-lost-hold.md §4.2)。
    const auto session = context.enterInfo.session_id();
    const auto notify = context.NotifiableSessionId();
    const auto scene = PlayerSceneSystem::FindSceneById(context.enterInfo.scene_id());
    const auto entity = tlsEcs.GetPlayer(playerId);
    const auto allowedScene = entity == entt::null ? PlayerSceneSystem::CheckEnterScene(playerId, scene)
        : PlayerSceneSystem::CheckEnterScene(entity, scene);
    const auto local = ReadLocalNode();
    if (playerId == 0 || context.ownerRevision == 0 || context.ownerEpoch == 0 || context.loginGeneration == 0 || session == 0
        || allowedScene != kSuccess || local.instanceId.empty() || state->nextGeneration == UINT64_MAX
        || !enter_fence::Admits(PlayerLifecycleSystem::JudgeEnter(playerId, session, context.loginGeneration))) {
        Reject(playerId, notify); return;
    }
    if (!SceneTaskScope::IsDurableAccepting()) { Reject(playerId, notify); return; }
    state->hasDurableAdmissions = true;
    auto& role = state->roles[playerId];
    if (role.controlInFlight || (role.seal && !role.drained)) { Reject(playerId, notify); return; }
    if (role.waitingRegister || role.waitingFence) {
        // 原未知登记不得换世代重建；相同原路由由计时器重投。托管落点与 gate 路由不是同一条原路由(一个绑会话、一个不绑)。
        if (role.context.ownerRevision != context.ownerRevision || role.context.ownerEpoch != context.ownerEpoch
            || role.context.loginGeneration != context.loginGeneration || role.context.hostedPlacement != context.hostedPlacement
            || role.context.enterInfo.SerializeAsString() != context.enterInfo.SerializeAsString()) {
            Reject(playerId, notify);
        }
        return;
    }
    if (role.registerCalls != 0 || role.fenceInFlight || role.pendingFenceRevoke) { Reject(playerId, notify); return; }
    context.roleAdmissionGeneration = ++state->nextGeneration;
    const auto admissionToken = handoff_sql_barrier::NewFenceToken();
    const auto candidate = handoff_sql_barrier::EncodeFence({context.ownerEpoch, local.instanceId,
        context.roleAdmissionGeneration, admissionToken, handoff_sql_barrier::FenceState::kHeld,
        handoff_sql_barrier::NewFenceToken()});
    if (!candidate) { Reject(playerId, notify); return; }
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
        || source->player_database_data().player_id() != playerId) return {};
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
        tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).RememberCommit(payload, epoch, seq);
    };
}
bool RoleAdmissionSystem::IsSnapshotCommitted(entt::entity player, const PlayerAllData& snapshot) {
    if (!tlsEcs.actorRegistry.valid(player)) return false;
    const auto playerId = tlsEcs.GetPlayerId(player);
    if (playerId == 0 || tlsEcs.GetPlayer(playerId) != player) return false;
    const auto* stored = tlsEcs.actorRegistry.try_get<PlayerLastPersistedSnapshotComp>(player);
    const auto* owner = tlsEcs.actorRegistry.try_get<PlayerOwnerEpochComp>(player);
    const auto* admission = tlsEcs.actorRegistry.try_get<PlayerRoleAdmissionComp>(player);
    const auto identity = ConfirmedIdentity(playerId);
    const auto& first = snapshot.player_database_data();
    const uint64_t epoch = stored ? stored->committed_owner_epoch : 0, seq = stored ? stored->committed_save_seq : 0;
    return stored != nullptr && stored->snapshot.get() == &snapshot && owner != nullptr && admission != nullptr
        && identity && identity->player_id() == playerId && !identity->scene_instance_id().empty()
        && admission->generation == identity->generation() && admission->ownerRevision == identity->owner_revision()
        && first.player_id() == playerId && epoch != 0 && seq != 0
        && owner->epoch == epoch;
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
    role.tradeSaveQueued = false; role.tradeFreezeSaving = false;
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
void RoleAdmissionSystem::SetTradeFreezeSaving(Guid playerId, bool saving) {
    if (const auto found = state->roles.find(playerId); found != state->roles.end()) found->second.tradeFreezeSaving = saving;
}
bool RoleAdmissionSystem::IsTradeFreezeSaving(Guid playerId) {
    const auto found = state->roles.find(playerId);
    return found != state->roles.end() && found->second.tradeFreezeSaving;
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
    if (state->hasDurableAdmissions && !SceneTaskScope::IsDurableAccepting()) return;
    for (auto& [playerId, role] : state->roles) {
        if (role.retryAt > Now()) continue;
        role.retryAt = Now() + std::chrono::seconds(1);
        if (role.waitingRegister) { SendRegister(playerId); continue; }
        if (role.waitingFence) { SendFence(playerId); continue; }
        if (role.pendingFenceRevoke) { CancelPendingFence(playerId); continue; }
        if (role.registerCalls != 0 || role.callbacks != 0 || tlsEcs.GetPlayer(playerId) != entt::null) continue;
        if (state->dependencies.hasPendingLoad ? state->dependencies.hasPendingLoad(playerId)
            : PlayerLifecycleSystem::HasPendingPlayerLoad(playerId)) continue;
        if (!PlayerLifecycleSystem::DrainSealedPendingEntry(playerId)) continue;
        // 存盘下限在提交后才抬高:实体没了、存盘还未落定时就关闭,下限不含这一笔,它会在冻结之后落地改行(规格 §6)。
        if (HasUnsettledSave(playerId)) continue;
        CloseOrAck(playerId, role);
    }
}
