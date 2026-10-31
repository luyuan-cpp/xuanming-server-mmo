#include "player/system/player_store_client.h"

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <utility>

#include <node_config_manager.h>
#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "core/network/grpc_client_call.h"
#include "core/security/service_call_auth.h"
#include "engine/config/player_store_path.h"
#include "grpc_client/player_store/player_store_grpc_client.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "proto/common/base/common.pb.h"
#include "proto/common/base/node.pb.h"
#include "thread_context/node_context_manager.h"
#include "time/system/time.h"
#include "utils/random/random.h"

namespace
{
    constexpr auto kProfileIndex = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_PROFILE);
    constexpr char kSavePlayerMethod[] = "/playerstore.PlayerStore/SavePlayer";
    constexpr char kLoadAndClaimMethod[] = "/playerstore.PlayerStore/LoadAndClaim";
    constexpr char kSceneCaller[] = "scene";
    constexpr char kSecretEnv[] = "MMORPG_PLAYER_STORE_SECRET_SCENE";
    // 与 go/db 的 serviceauth 同一条线,短了 go/db 也会拒绝启动。
    constexpr std::size_t kMinSecretBytes = 32;
    // 连续失败时每这么多次打一条,第一次也打。退避封顶 5s 时约等于每分钟一条。
    constexpr uint32_t kFailureLogEveryN = 12;

    bool IsOwnerRejection(playerstore::SavePlayerResult result)
    {
        return result == playerstore::SAVE_PLAYER_STALE_OWNER || result == playerstore::SAVE_PLAYER_NOT_HOLDER ||
               result == playerstore::SAVE_PLAYER_UNCLAIMED;
    }

    // 每次发送都重新签名:签名有时间窗,故障久了不重签就永久失败。失败时已打日志。
    template <class Request>
    std::optional<Request> SignForPlayerStore(const Request &request, const char *method)
    {
        const char *secret = std::getenv(kSecretEnv);
        if (secret == nullptr)
        {
            LOG_ERROR << "[player_store] " << kSecretEnv << " is not set";
            return std::nullopt;
        }
        auto signedRequest = request;
        if (!service_call_auth::Sign(method, signedRequest, *signedRequest.mutable_auth(), kSceneCaller, secret,
                                     TimeSystem::NowMillisecondsUTC()))
        {
            LOG_ERROR << "[player_store] failed to sign " << method << " of player " << request.player_id();
            return std::nullopt;
        }
        return signedRequest;
    }

    entt::entity PickReadyPlayerStoreNode()
    {
        auto &registry = tlsNodeContextManager.GetRegistry(common::base::PlayerStoreNodeService);
        std::vector<entt::entity> ready;
        for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
        {
            const auto &channel = registry.get<MuduoGrpcChannelPtr>(entity);
            if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady)
            {
                ready.push_back(entity);
            }
        }
        if (ready.empty())
        {
            return entt::null;
        }
        return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
    }
} // namespace

bool DetachedReleaseSettled(playerstore::SavePlayerResult result)
{
    switch (result)
    {
    case playerstore::SAVE_PLAYER_COMMITTED:
    case playerstore::SAVE_PLAYER_EQUAL:
    case playerstore::SAVE_PLAYER_NO_OWNER:
    case playerstore::SAVE_PLAYER_UNCLAIMED:
    case playerstore::SAVE_PLAYER_STALE_OWNER:
    case playerstore::SAVE_PLAYER_NOT_HOLDER:
    case playerstore::SAVE_PLAYER_OLDER_SEQ:
        return true;
    default:
        return false;
    }
}


PlayerStoreClient::PlayerStoreClient(Options options) : options_(std::move(options))
{
}

bool PlayerStoreClient::Seed(Guid playerId, uint32_t storageId, uint64_t ownerEpoch, uint64_t committedSaveSeq, player_store_rows::KindBytes confirmed)
{
    if (HasUnsettled(playerId))
    {
        return false;
    }
    State &state = states_[playerId];
    state.storageId = storageId;
    state.ownerEpoch = ownerEpoch;
    state.confirmed = std::move(confirmed);
    state.uncertain.reset();
    // 认领时库里已经把 released 清成 0。
    state.releaseCommitted = false;
    state.committedOwnerEpoch = ownerEpoch;
    state.committedSaveSeq = committedSaveSeq;
    state.backoffSec = options_.retryMinSec;
    state.failedAttempts = 0;
    return true;
}

void PlayerStoreClient::SetOwnerEpoch(Guid playerId, uint64_t ownerEpoch)
{
    const auto it = states_.find(playerId);
    if (it != states_.end())
    {
        it->second.ownerEpoch = ownerEpoch;
    }
}

bool PlayerStoreClient::HasUnsettled(Guid playerId) const
{
    if (detachedInFlight_.count(playerId) != 0)
    {
        return true;
    }
    const auto it = states_.find(playerId);
    return it != states_.end() && (it->second.inflight.has_value() || it->second.pending.has_value());
}

std::vector<Guid> PlayerStoreClient::UnsettledPlayers() const
{
    std::vector<Guid> players(detachedInFlight_.begin(), detachedInFlight_.end());
    for (const auto &[playerId, state] : states_)
    {
        if (state.inflight.has_value() || state.pending.has_value())
        {
            players.push_back(playerId);
        }
    }
    return players;
}

bool PlayerStoreClient::Forget(Guid playerId)
{
    if (HasUnsettled(playerId))
    {
        return false;
    }
    states_.erase(playerId);
    return true;
}

PlayerStoreClient::KindSet PlayerStoreClient::DirtyKinds(const State &state, const player_store_rows::KindBytes &rows) const
{
    KindSet dirty = state.uncertain;
    for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
    {
        if (rows[kind] != state.confirmed[kind])
        {
            dirty.set(kind);
        }
    }
    return dirty;
}

PlayerStoreClient::SubmitResult PlayerStoreClient::Submit(Guid playerId, player_store_rows::KindBytes rows, const SubmitOptions &submitOptions, SaveDone done)
{
    const auto it = states_.find(playerId);
    if (it == states_.end())
    {
        return SubmitResult::kNotSeeded;
    }
    State &state = it->second;
    // pending 非空也算忙:回调里再次 Submit 时,这份快照比 pending 新,不能抢到它前面发出去。
    if (state.inflight.has_value() || state.pending.has_value())
    {
        if (!state.pending.has_value())
        {
            state.pending.emplace();
        }
        Pending &pending = *state.pending;
        pending.rows = std::move(rows);
        // force 粘性:被覆盖的那份要求的是「一定写一笔」,这个要求不能跟着快照一起丢。
        // release 不粘:最后一份说了算,断线重连复用实体时后来的存盘要把释放撤销。
        pending.options.force = pending.options.force || submitOptions.force;
        pending.options.release = submitOptions.release;
        pending.options.schemaGen = submitOptions.schemaGen;
        if (pending.callbacks.latest)
        {
            pending.callbacks.superseded.push_back(std::move(pending.callbacks.latest));
        }
        pending.callbacks.latest = std::move(done);
        return SubmitResult::kAccepted;
    }
    Callbacks callbacks;
    callbacks.latest = std::move(done);
    switch (Start(playerId, state, rows, submitOptions, callbacks))
    {
    case StartResult::kSent:
        return SubmitResult::kAccepted;
    case StartResult::kNothingToWrite:
        return SubmitResult::kAlreadyPersisted;
    case StartResult::kSeqExhausted:
    default:
        // 不走回调:调用方还在自己的栈上,回调里的处置可能销毁它正在用的实体。
        return SubmitResult::kSeqExhausted;
    }
}

PlayerStoreClient::StartResult PlayerStoreClient::Start(Guid playerId, State &state, const player_store_rows::KindBytes &rows, const SubmitOptions &submitOptions, Callbacks &callbacks)
{
    KindSet dirty = DirtyKinds(state, rows);
    const bool epochBehind = state.committedOwnerEpoch != state.ownerEpoch;
    if (dirty.none() && !submitOptions.release && !submitOptions.force && !state.releaseCommitted && !epochBehind)
    {
        return StartResult::kNothingToWrite;
    }
    const std::optional<uint64_t> saveSeq = options_.nextSeq(playerId);
    if (!saveSeq)
    {
        return StartResult::kSeqExhausted;
    }
    InFlight inflight;
    auto &request = inflight.request;
    request.set_player_id(playerId);
    request.set_storage_id(state.storageId);
    request.set_owner_epoch(state.ownerEpoch);
    request.set_save_seq(*saveSeq);
    request.set_schema_gen(submitOptions.schemaGen);
    request.set_instance_id(options_.instanceId());
    request.set_release(submitOptions.release);

    std::string stampedProfile;
    const bool stamped = options_.stampProfile && dirty.any();
    if (stamped)
    {
        stampedProfile = rows[kProfileIndex];
        options_.stampProfile(playerId, *saveSeq, stampedProfile);
        dirty.set(kProfileIndex);
    }
    for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
    {
        if (!dirty.test(kind))
        {
            continue;
        }
        auto *row = request.add_rows();
        row->set_kind(static_cast<playerstore::PlayerStoreKind>(kind));
        row->set_payload(stamped && kind == kProfileIndex ? stampedProfile : rows[kind]);
        inflight.confirmOnCommit[kind] = rows[kind];
    }
    inflight.sentKinds = dirty;
    inflight.callbacks = std::move(callbacks);
    state.inflight.emplace(std::move(inflight));
    state.backoffSec = options_.retryMinSec;
    state.failedAttempts = 0;
    if (const std::size_t requestBytes = state.inflight->request.ByteSizeLong(); requestBytes > options_.maxRequestBytes)
    {
        // 和 go/db 回 TOO_LARGE 走同一条路。排到下一轮:调用方可能还在自己的栈上,回调里的处置可能销毁它正在用的实体。
        LOG_ERROR << "[player_store] save of player " << playerId << " is " << requestBytes << " bytes, over the transport budget "
                  << options_.maxRequestBytes << "; refusing it without sending";
        options_.schedule(0.0, [this, playerId, attemptGen = ++state.attemptGen]()
        {
            playerstore::SavePlayerResponse response;
            response.set_result(playerstore::SAVE_PLAYER_TOO_LARGE);
            OnAttemptDone(playerId, attemptGen, true, &response);
        });
        return StartResult::kSent;
    }
    Attempt(playerId, state);
    return StartResult::kSent;
}

void PlayerStoreClient::Attempt(Guid playerId, State &state)
{
    const uint64_t attemptGen = ++state.attemptGen;
    const bool sent = options_.send(state.inflight->request, [this, playerId, attemptGen](bool ok, const playerstore::SavePlayerResponse *response)
    {
        OnAttemptDone(playerId, attemptGen, ok, response);
    });
    if (!sent)
    {
        ScheduleRetry(playerId, state);
    }
}

void PlayerStoreClient::ScheduleRetry(Guid playerId, State &state)
{
    if (state.failedAttempts++ % kFailureLogEveryN == 0)
    {
        LOG_ERROR << "[player_store] save of player " << playerId << " seq " << state.inflight->request.save_seq()
                  << " has not reached a definite result after " << state.failedAttempts
                  << " attempt(s); keeping the snapshot and retrying (metric=player_store_save_retry)";
    }
    const double delay = Jittered(state.backoffSec);
    state.backoffSec = std::min(state.backoffSec * 2.0, options_.retryMaxSec);
    options_.schedule(delay, [this, playerId, attemptGen = state.attemptGen]()
    {
        const auto it = states_.find(playerId);
        if (it == states_.end() || !it->second.inflight.has_value() || it->second.attemptGen != attemptGen)
        {
            return;
        }
        Attempt(playerId, it->second);
    });
}

void PlayerStoreClient::OnAttemptDone(Guid playerId, uint64_t attemptGen, bool ok, const playerstore::SavePlayerResponse *response)
{
    const auto it = states_.find(playerId);
    if (it == states_.end() || !it->second.inflight.has_value() || it->second.attemptGen != attemptGen)
    {
        return;
    }
    State &state = it->second;
    if (!ok || response == nullptr)
    {
        // 结果未知:可能已经提交。同内容同序号重试,go/db 会回 EQUAL。
        state.uncertain |= state.inflight->sentKinds;
        ScheduleRetry(playerId, state);
        return;
    }
    const auto &request = state.inflight->request;
    SaveResult result;
    result.result = response->result();
    result.rowOwnerEpoch = response->row_owner_epoch();
    result.rowSaveSeq = response->row_save_seq();
    result.ownerEpoch = request.owner_epoch();
    result.saveSeq = request.save_seq();
    switch (response->result())
    {
    case playerstore::SAVE_PLAYER_COMMITTED:
    case playerstore::SAVE_PLAYER_EQUAL:
        result.status = SaveStatus::kCommitted;
        Finish(playerId, state, result, /*committed=*/true);
        return;
    case playerstore::SAVE_PLAYER_NO_OWNER:
    case playerstore::SAVE_PLAYER_SCHEMA_TOO_NEW:
    case playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED:
        // 写权行还没有,或者 go/db 比 scene 旧(发布顺序问题)。这笔没有提交,等它好了再试。日志和结果未知同一个节流。
        if (state.failedAttempts % kFailureLogEveryN == 0)
        {
            LOG_ERROR << "[player_store] save of player " << playerId << " seq " << request.save_seq()
                      << " will retry: " << playerstore::SavePlayerResult_Name(response->result());
        }
        ScheduleRetry(playerId, state);
        return;
    default:
        // 这一笔明确没有提交,但之前结果未知的那次发送可能已经提交了,库里是哪一份不知道,
        // 所以这些行必须并入脏集,否则改回原值的行不会被写。
        LOG_ERROR << "[player_store] save of player " << playerId << " seq " << request.save_seq()
                  << " rejected: " << playerstore::SavePlayerResult_Name(response->result())
                  << " owner_epoch=" << request.owner_epoch() << " row_owner_epoch=" << response->row_owner_epoch()
                  << " row_save_seq=" << response->row_save_seq();
        state.uncertain |= state.inflight->sentKinds;
        // 排队的快照带着同一个代次,发出去也只会再被拒,一并丢弃;代次变了的照常发。
        if (IsOwnerRejection(response->result()) && state.pending.has_value() && state.ownerEpoch == request.owner_epoch())
        {
            Pending dropped = std::move(*state.pending);
            state.pending.reset();
            if (dropped.callbacks.latest)
            {
                dropped.callbacks.superseded.push_back(std::move(dropped.callbacks.latest));
            }
            result.status = SaveStatus::kRejected;
            Fire(dropped.callbacks, result);
        }
        result.status = SaveStatus::kRejected;
        Finish(playerId, state, result, /*committed=*/false);
        return;
    }
}

void PlayerStoreClient::Fire(Callbacks &callbacks, const SaveResult &result)
{
    SaveResult superseded = result;
    superseded.superseded = true;
    for (const auto &done : callbacks.superseded)
    {
        if (done)
        {
            done(superseded);
        }
    }
    if (callbacks.latest)
    {
        callbacks.latest(result);
    }
}

void PlayerStoreClient::Finish(Guid playerId, State &state, const SaveResult &result, bool committed)
{
    InFlight finished = std::move(*state.inflight);
    state.inflight.reset();
    if (committed)
    {
        for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
        {
            if (finished.sentKinds.test(kind))
            {
                state.confirmed[kind] = std::move(finished.confirmOnCommit[kind]);
                state.uncertain.reset(kind);
            }
        }
        state.releaseCommitted = finished.request.release();
        state.committedOwnerEpoch = finished.request.owner_epoch();
        state.committedSaveSeq = finished.request.save_seq();
    }
    // 回调里可能再次 Submit(进 pending),也可能 Forget,之后不再使用 state。
    Fire(finished.callbacks, result);
    Advance(playerId);
}

void PlayerStoreClient::Advance(Guid playerId)
{
    // 循环:pending 可能没有变化,直接结束,而它的回调里又可能 Submit。
    while (true)
    {
        const auto it = states_.find(playerId);
        if (it == states_.end())
        {
            // 回调里销毁了实体、回收了状态(被废黜):等着落定的调用方照样要通知,否则它们永远留在表里。
            NotifySettled(playerId);
            return;
        }
        if (it->second.inflight.has_value())
        {
            return;
        }
        State &state = it->second;
        if (!state.pending.has_value())
        {
            NotifySettled(playerId);
            return;
        }
        Pending next = std::move(*state.pending);
        state.pending.reset();
        const StartResult started = Start(playerId, state, next.rows, next.options, next.callbacks);
        if (started == StartResult::kSent)
        {
            return;
        }
        SaveResult outcome;
        if (started == StartResult::kNothingToWrite)
        {
            outcome.status = SaveStatus::kAlreadyPersisted;
            outcome.ownerEpoch = state.committedOwnerEpoch;
            outcome.saveSeq = state.committedSaveSeq;
        }
        Fire(next.callbacks, outcome);
    }
}

void PlayerStoreClient::RunWhenSettled(Guid playerId, std::function<void()> fn)
{
    if (!HasUnsettled(playerId))
    {
        fn();
        return;
    }
    settledWaiters_[playerId].push_back(std::move(fn));
}

void PlayerStoreClient::NotifySettled(Guid playerId)
{
    if (options_.onSettled)
    {
        options_.onSettled(playerId);
    }
    if (const auto waiters = settledWaiters_.find(playerId); waiters != settledWaiters_.end())
    {
        auto fns = std::move(waiters->second);
        settledWaiters_.erase(waiters);
        for (const auto &fn : fns)
        {
            fn();
        }
    }
    // 回调里可能又发了一笔存盘:等它的认领接着等。
    if (HasUnsettled(playerId))
    {
        return;
    }
    const auto claim = claims_.find(playerId);
    if (claim != claims_.end() && claim->second.waitingForSettle)
    {
        claim->second.waitingForSettle = false;
        StartClaim(playerId, claim->second);
    }
}

void PlayerStoreClient::ReleaseDetached(Guid playerId, uint64_t ownerEpoch, uint32_t schemaGen, ReleaseDone done)
{
    const std::optional<uint64_t> saveSeq = options_.nextSeq(playerId);
    if (!saveSeq)
    {
        done(false, playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED);
        return;
    }
    playerstore::SavePlayerRequest request;
    request.set_player_id(playerId);
    request.set_owner_epoch(ownerEpoch);
    request.set_save_seq(*saveSeq);
    request.set_schema_gen(schemaGen);
    request.set_instance_id(options_.instanceId());
    request.set_release(true);
    detachedInFlight_.insert(playerId);
    // shared_ptr:send 返回 false 时 done 还要在这里调用,而闭包已经移交给了 send。
    auto shared = std::make_shared<ReleaseDone>(std::move(done));
    const bool sent = options_.send(request, [this, playerId, shared](bool ok, const playerstore::SavePlayerResponse *response)
    {
        detachedInFlight_.erase(playerId);
        (*shared)(ok && response != nullptr, response != nullptr ? response->result() : playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED);
        if (!HasUnsettled(playerId))
        {
            NotifySettled(playerId);
        }
    });
    if (!sent)
    {
        detachedInFlight_.erase(playerId);
        (*shared)(false, playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED);
        if (!HasUnsettled(playerId))
        {
            NotifySettled(playerId);
        }
    }
}

double PlayerStoreClient::Jittered(double delaySec) const
{
    return options_.jitter ? options_.jitter(delaySec) : delaySec;
}

PlayerStoreClient::Clock::time_point PlayerStoreClient::Now() const
{
    return options_.now ? options_.now() : Clock::now();
}

void PlayerStoreClient::RequestClaim(Guid playerId)
{
    const auto [it, inserted] = claims_.try_emplace(playerId);
    if (!inserted)
    {
        return;
    }
    if (HasUnsettled(playerId))
    {
        it->second.waitingForSettle = true;
        return;
    }
    StartClaim(playerId, it->second);
}

bool PlayerStoreClient::HasPendingClaim(Guid playerId) const
{
    return claims_.count(playerId) != 0;
}

void PlayerStoreClient::StartClaim(Guid playerId, Claim &claim)
{
    claim.deadline = Now() + std::chrono::duration_cast<Clock::duration>(std::chrono::duration<double>(options_.claimBudgetSec));
    claim.backoffSec = options_.retryMinSec;
    AttemptClaim(playerId, claim);
}

void PlayerStoreClient::AttemptClaim(Guid playerId, Claim &claim)
{
    const auto request = options_.claimRequest(playerId);
    if (!request)
    {
        FinishClaim(playerId, nullptr);
        return;
    }
    const uint64_t attemptGen = ++claim.attemptGen;
    const bool sent = options_.sendClaim(*request, [this, playerId, attemptGen](bool ok, const playerstore::LoadAndClaimResponse *response)
    {
        OnClaimAttemptDone(playerId, attemptGen, ok, response);
    });
    if (!sent)
    {
        ScheduleClaimRetry(playerId, claim);
    }
}

void PlayerStoreClient::ScheduleClaimRetry(Guid playerId, Claim &claim)
{
    if (Now() >= claim.deadline)
    {
        LOG_ERROR << "[player_store] claim of player " << playerId << " has no definite result after " << claim.failedAttempts + 1
                  << " attempt(s); giving up this entry (metric=player_store_claim_budget_exhausted)";
        FinishClaim(playerId, nullptr);
        return;
    }
    ++claim.failedAttempts;
    const double delay = Jittered(claim.backoffSec);
    claim.backoffSec = std::min(claim.backoffSec * 2.0, options_.retryMaxSec);
    options_.schedule(delay, [this, playerId, attemptGen = claim.attemptGen]()
    {
        const auto it = claims_.find(playerId);
        if (it == claims_.end() || it->second.attemptGen != attemptGen)
        {
            return;
        }
        AttemptClaim(playerId, it->second);
    });
}

void PlayerStoreClient::OnClaimAttemptDone(Guid playerId, uint64_t attemptGen, bool ok, const playerstore::LoadAndClaimResponse *response)
{
    const auto it = claims_.find(playerId);
    if (it == claims_.end() || it->second.attemptGen != attemptGen)
    {
        return;
    }
    if (!ok || response == nullptr)
    {
        ScheduleClaimRetry(playerId, it->second);
        return;
    }
    if (response->result() != playerstore::LOAD_AND_CLAIM_CLAIMED)
    {
        FinishClaim(playerId, response);
        return;
    }
    player_store_rows::KindBytes confirmed;
    for (const auto &row : response->rows())
    {
        const auto kind = static_cast<std::size_t>(row.kind());
        if (kind == 0 || kind >= player_store_rows::kKindCount)
        {
            LOG_ERROR << "[player_store] claim of player " << playerId << " returned an unknown kind " << kind;
            FinishClaim(playerId, nullptr);
            return;
        }
        confirmed[kind] = row.payload();
    }
    if (!Seed(playerId, response->storage_id(), response->owner_epoch(), response->save_seq(), std::move(confirmed)))
    {
        LOG_ERROR << "[player_store] claim of player " << playerId << " landed while a save is unsettled; refusing the entry";
        FinishClaim(playerId, nullptr);
        return;
    }
    FinishClaim(playerId, response);
}

void PlayerStoreClient::FinishClaim(Guid playerId, const playerstore::LoadAndClaimResponse *response)
{
    claims_.erase(playerId);
    options_.onClaimed(playerId, response);
}

bool ValidatePlayerStoreStartup(std::string &error)
{
    if (!player_store_rows::ValidatePersistGroups(error))
    {
        return false;
    }
    const char *secret = std::getenv(kSecretEnv);
    if (secret == nullptr || std::strlen(secret) < kMinSecretBytes)
    {
        error = std::string(kSecretEnv) + " must be set to at least " + std::to_string(kMinSecretBytes) + " bytes";
        return false;
    }
    return true;
}

bool SendPlayerStoreSaveOverGrpc(const playerstore::SavePlayerRequest &request, PlayerStoreClient::SendDone done)
{
    const auto signedRequest = SignForPlayerStore(request, kSavePlayerMethod);
    if (!signedRequest)
    {
        return false;
    }
    const entt::entity node = PickReadyPlayerStoreNode();
    if (node == entt::null)
    {
        return false;
    }
    playerstore::SendPlayerStoreSavePlayer(
        tlsNodeContextManager.GetRegistry(common::base::PlayerStoreNodeService), node, *signedRequest,
        [done = std::move(done)](const GrpcCallOutcome &outcome, const playerstore::SavePlayerResponse *reply)
        {
            done(outcome.ok() && reply != nullptr, reply);
        });
    return true;
}

bool SendPlayerStoreClaimOverGrpc(const playerstore::LoadAndClaimRequest &request, PlayerStoreClient::ClaimDone done)
{
    const auto signedRequest = SignForPlayerStore(request, kLoadAndClaimMethod);
    if (!signedRequest)
    {
        return false;
    }
    const entt::entity node = PickReadyPlayerStoreNode();
    if (node == entt::null)
    {
        return false;
    }
    playerstore::SendPlayerStoreLoadAndClaim(
        tlsNodeContextManager.GetRegistry(common::base::PlayerStoreNodeService), node, *signedRequest,
        [done = std::move(done)](const GrpcCallOutcome &outcome, const playerstore::LoadAndClaimResponse *reply)
        {
            done(outcome.ok() && reply != nullptr, reply);
        });
    return true;
}
