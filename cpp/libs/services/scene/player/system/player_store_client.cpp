#include "player/system/player_store_client.h"
#include "core/async/coroutine_callback.h"
#include "core/system/scene_task_scope.h"

#include <boost/cobalt/task.hpp>

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
#include "player/system/save_timing_stats.h"
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
    // go/db 提交了却没写审计发件箱(go/db 比 scene 旧):全进程第一次和此后每这么多次打一条,发布顺序错了时不刷屏。
    constexpr uint64_t kAuditNotWrittenLogEveryN = 1000;
    // 认领到本二进制不认识的内容(行里的未知顶层字段、更新版本才有的行):全进程第一次和此后每这么多次打一条。
    // 新旧版本混跑的窗口里每次进场都会遇到,逐条打会刷屏。
    constexpr uint64_t kCarriedClaimLogEveryN = 1000;

    template<class Reply>
    struct AttemptResult
    {
        bool accepted{false};
        std::optional<Reply> reply;
    };

    template<class Reply>
    void DeliverAttempt(const std::function<void(AttemptResult<Reply>)>& done, bool ok, const Reply* reply)
    {
        AttemptResult<Reply> result{true, std::nullopt};
        if (ok && reply != nullptr) result.reply.emplace(*reply);
        done(std::move(result));
    }

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


PlayerStoreClient::PlayerStoreClient(Options options)
    : lifetime_(std::make_shared<Lifetime>(Lifetime{std::ref(*this)})), options_(std::move(options))
{
}

PlayerStoreClient::~PlayerStoreClient()
{
    lifetime_->owner.reset();
}

PlayerStoreClient* PlayerStoreClient::ResolveOwner(const std::weak_ptr<Lifetime>& original)
{
    const auto lifetime = original.lock();
    return lifetime && lifetime->owner ? &lifetime->owner->get() : nullptr;
}


bool PlayerStoreClient::Seed(Guid playerId, uint32_t storageId, uint64_t ownerEpoch, uint64_t committedSaveSeq, player_store_rows::KindBytes confirmed,
                             player_store_rows::KindBytes carried)
{
    if (HasUnsettled(playerId))
    {
        return false;
    }
    State &state = states_[playerId];
    state.storageId = storageId;
    state.ownerEpoch = ownerEpoch;
    state.confirmed = std::move(confirmed);
    // 每次播种都整份换掉:上一次认领带回的内容不留到这一次。
    state.carried = std::move(carried);
    state.hasCarried = std::any_of(state.carried.begin(), state.carried.end(), [](const std::string &bytes) { return !bytes.empty(); });
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

void PlayerStoreClient::AppendCarried(const State &state, player_store_rows::KindBytes &rows)
{
    if (!state.hasCarried)
    {
        return;
    }
    for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
    {
        rows[kind] += state.carried[kind];
    }
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

const PlayerStoreClient::KindSet &PlayerStoreClient::PiggybackKinds()
{
    static const KindSet kinds = []
    {
        KindSet set;
        for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
        {
            if (player_store_rows::IsDataKind(kind) &&
                player_store_rows::RowSavePolicy(kind) == PERSIST_ROW_SAVE_PIGGYBACK)
            {
                set.set(kind);
            }
        }
        return set;
    }();
    return kinds;
}

bool PlayerStoreClient::DeferPeriodicSave(Guid playerId, const player_store_rows::KindBytes &rows)
{
    if (options_.piggybackFallbackSec <= 0)
    {
        return false;
    }
    const auto it = states_.find(playerId);
    // 有在途或排队时不推迟:让这份快照照常并进排队,不改排队与回调的语义。
    if (it == states_.end() || it->second.inflight.has_value() || it->second.pending.has_value())
    {
        return false;
    }
    State &state = it->second;
    // 有到期的审计记录就不推迟:它们要随这一笔交出去(docs/design/player-audit-outbox.md §5.2)。
    // 这份快照此刻才取,水位就是现在的水位。
    if (options_.auditDue && options_.auditDue(playerId, options_.commitMark ? options_.commitMark() : 0))
    {
        return false;
    }
    const KindSet &piggyback = PiggybackKinds();
    // 和 Submit 同一把尺子:接上认领时带回的未知字段再比。没有带回内容时(全网同一版本的常态)不拷贝。
    KindSet dirty;
    if (state.hasCarried)
    {
        player_store_rows::KindBytes composed = rows;
        AppendCarried(state, composed);
        dirty = DirtyKinds(state, composed);
    }
    else
    {
        dirty = DirtyKinds(state, rows);
    }
    // 一定要写的情况:有别的行脏(位置照样顺带写出去);只顺带的行结果未决(库里是哪一份不知道,不能拖);
    // 上一笔带着 release 或代次落后(Start 会强制写一笔)。没有任何变化时交给 Submit 判「已落盘」,计数照旧。
    if (dirty.none() || (dirty & ~piggyback).any() || (state.uncertain & piggyback).any() ||
        state.releaseCommitted || state.committedOwnerEpoch != state.ownerEpoch)
    {
        return false;
    }
    const auto now = Now();
    if (!state.piggybackDeferredSince)
    {
        state.piggybackDeferredSince = now;
        return true;
    }
    const std::chrono::duration<double> deferred = now - *state.piggybackDeferredSince;
    return deferred.count() < options_.piggybackFallbackSec;
}

PlayerStoreClient::SubmitResult PlayerStoreClient::Submit(Guid playerId, player_store_rows::KindBytes rows, const SubmitOptions &submitOptions, SaveDone done)
{
    const auto it = states_.find(playerId);
    if (it == states_.end())
    {
        return SubmitResult::kNotSeeded;
    }
    if (!SceneTaskScope::IsDurableAccepting()) return SubmitResult::kStopped;
    State &state = it->second;
    // 投影结果只有本二进制认识的字段。先接上认领时带回的未知字段:下面比脏、排队、发送、确认用的都是要写进库的整行。
    // 只在这里接一次;状态机内部再用的行(排队的快照、续笔用的 confirmed)都已经是整行。
    AppendCarried(state, rows);
    // 快照就是调用方此刻的内存,水位要在这时取:之后再发生的改动不在这份快照里。
    const uint64_t commitMark = options_.commitMark ? options_.commitMark() : 0;
    // pending 非空也算忙:回调里再次 Submit 时,这份快照比 pending 新,不能抢到它前面发出去。
    if (state.inflight.has_value() || state.pending.has_value())
    {
        if (!state.pending.has_value())
        {
            state.pending.emplace();
        }
        Pending &pending = *state.pending;
        pending.rows = std::move(rows);
        pending.commitMark = std::max(pending.commitMark, commitMark);
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
    switch (Start(playerId, state, rows, submitOptions, callbacks, commitMark))
    {
    case StartResult::kSent:
        return SubmitResult::kAccepted;
    case StartResult::kNothingToWrite:
        // 和已确认的内容一样:这份快照里的改动早已在库里。
        NotifyCommittedThrough(playerId, commitMark);
        return SubmitResult::kAlreadyPersisted;
    case StartResult::kSeqExhausted:
    default:
        // 不走回调:调用方还在自己的栈上,回调里的处置可能销毁它正在用的实体。
        return SubmitResult::kSeqExhausted;
    }
}

PlayerStoreClient::StartResult PlayerStoreClient::Start(Guid playerId, State &state, const player_store_rows::KindBytes &rows, const SubmitOptions &submitOptions, Callbacks &callbacks,
                                                        uint64_t commitMark)
{
    const auto compareStart = save_timing_stats::Clock::now();
    KindSet dirty = DirtyKinds(state, rows);
    save_timing_stats::ObserveSince(save_timing_stats::Stage::kCompare, compareStart);
    const bool epochBehind = state.committedOwnerEpoch != state.ownerEpoch;
    // 水位不大于这份快照的审计记录还没交出去:没有脏行也要写一笔。净变化为零的改动(先 +100 后 −100)同样有流水要交,
    // 它们只能随存盘进发件箱(docs/design/player-audit-outbox.md §5.2)。
    const bool auditDue = options_.auditDue && options_.auditDue(playerId, commitMark);
    if (dirty.none() && !submitOptions.release && !submitOptions.force && !state.releaseCommitted && !epochBehind && !auditDue)
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
    // 审计记录随这一笔交出去:go/db 在提交存档的同一个事务里写进发件箱。只在这里拷一次,重试发的是同一份请求。
    AuditAttachment attached;
    if (options_.attachAudit)
    {
        attached = options_.attachAudit(playerId, commitMark, request);
    }
    inflight.carriesAudit = request.transaction_logs_size() > 0 || request.audit_snapshots_size() > 0;
    // 都带上了:这份快照里的审计记录提交后全部放行。没带完:只放行到带上的最后一条,剩下的还没进库。
    inflight.auditAckMark = attached.more ? attached.through : commitMark;
    if (attached.more && !state.auditStalled)
    {
        // 到期的审计记录超过一笔的预算(存盘中断很久之后才会出现):分笔。这一笔先把行和前一段记录提交,不带 release:
        // release 之后别的节点可以认领,剩下的记录就没有机会由本节点提交了。调用方要的 release 与回调留给最后一笔(Finish)。
        // 行已经随这一笔写了,续笔不必再被强制写一次。
        inflight.continuation = submitOptions;
        inflight.continuation->force = false;
        request.set_release(false);
    }

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
    // 只顺带的行跟着这一笔写出去了:推迟的计时从下一次推迟重新开始(规格 §1.1)。
    if ((dirty & PiggybackKinds()).any())
    {
        state.piggybackDeferredSince.reset();
    }
    inflight.callbacks = std::move(callbacks);
    inflight.commitMark = commitMark;
    state.inflight.emplace(std::move(inflight));
    state.backoffSec = options_.retryMinSec;
    state.failedAttempts = 0;
    if (const std::size_t requestBytes = state.inflight->request.ByteSizeLong(); requestBytes > options_.maxRequestBytes)
    {
        // 和 go/db 回 TOO_LARGE 走同一条路。排到下一轮:调用方可能还在自己的栈上,回调里的处置可能销毁它正在用的实体。
        LOG_ERROR << "[player_store] save of player " << playerId << " is " << requestBytes << " bytes, over the transport budget "
                  << options_.maxRequestBytes << "; refusing it without sending";
        const std::weak_ptr<Lifetime> original = lifetime_;
        options_.schedule(0.0, [original, playerId, attemptGen = ++state.attemptGen]()
        {
            if (!SceneTaskScope::IsDurableAccepting()) return;
            auto* owner = ResolveOwner(original);
            if (owner == nullptr) return;
            const auto it = owner->states_.find(playerId);
            if (it == owner->states_.end() || !it->second.inflight || it->second.attemptGen != attemptGen) return;
            playerstore::SavePlayerResponse response;
            response.set_result(playerstore::SAVE_PLAYER_TOO_LARGE);
            owner->HandleSaveResponse(playerId, it->second, response);
        });
        return StartResult::kSent;
    }
    Attempt(playerId, state);
    return StartResult::kSent;
}

void PlayerStoreClient::Attempt(Guid playerId, State &state)
{
    if (!SceneTaskScope::IsDurableAccepting()) return;
    const uint64_t attemptGen = ++state.attemptGen;
    (void)SceneTaskScope::TryStartDurable(RunSaveAttempt(lifetime_, playerId, attemptGen), "PlayerStoreSave");
}

boost::cobalt::task<void> PlayerStoreClient::RunSaveAttempt(std::weak_ptr<Lifetime> original, Guid playerId, uint64_t attemptGen)
{
    using Result = AttemptResult<playerstore::SavePlayerResponse>;
    auto submit = [&](auto done) {
        auto* owner = ResolveOwner(original);
        if (owner == nullptr) return false;
        const auto it = owner->states_.find(playerId);
        if (it == owner->states_.end() || !it->second.inflight || it->second.attemptGen != attemptGen) return false;
        return owner->options_.send(it->second.inflight->request,
            [done = std::move(done)](bool ok, const playerstore::SavePlayerResponse* reply) {
                DeliverAttempt(done, ok, reply);
            });
    };
    auto& loop = *muduo::net::EventLoop::getEventLoopOfCurrentThread();
    auto outcome = co_await loop_async::AwaitCallback<Result>(loop, std::move(submit), Result{});
    // 最终拆除时只结束本地等待，未确认的快照仍未落定。
    if (!outcome || !SceneTaskScope::IsDurableAccepting()) co_return;
    auto* owner = ResolveOwner(original);
    if (owner == nullptr) co_return;
    const auto it = owner->states_.find(playerId);
    if (it == owner->states_.end() || !it->second.inflight || it->second.attemptGen != attemptGen) co_return;
    auto& state = it->second;
    if (!outcome->reply)
    {
        // 已接纳但结局未知，可能已经提交。未接纳没有这次写入。
        if (outcome->accepted) state.uncertain |= state.inflight->sentKinds;
        owner->ScheduleRetry(playerId, state);
        co_return;
    }
    owner->HandleSaveResponse(playerId, state, *outcome->reply);
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
    const std::weak_ptr<Lifetime> original = lifetime_;
    options_.schedule(delay, [original, playerId, attemptGen = state.attemptGen]()
    {
        auto* owner = ResolveOwner(original);
        if (owner == nullptr) return;
        const auto it = owner->states_.find(playerId);
        if (it == owner->states_.end() || !it->second.inflight || it->second.attemptGen != attemptGen) return;
        owner->Attempt(playerId, it->second);
    });
}

void PlayerStoreClient::HandleSaveResponse(Guid playerId, State &state, const playerstore::SavePlayerResponse &response)
{
    const auto &request = state.inflight->request;
    SaveResult result;
    result.result = response.result();
    result.rowOwnerEpoch = response.row_owner_epoch();
    result.rowSaveSeq = response.row_save_seq();
    result.ownerEpoch = request.owner_epoch();
    result.saveSeq = request.save_seq();
    switch (response.result())
    {
    case playerstore::SAVE_PLAYER_COMMITTED:
    case playerstore::SAVE_PLAYER_EQUAL:
    {
        result.status = SaveStatus::kCommitted;
        // 带了审计记录却没有 audit_outbox_written:go/db 不认识审计字段(比 scene 旧),存档提交了、审计被它丢了。
        // 行照常确认;审计记录不放行,留在队列里随下一笔再交(docs/design/player-audit-outbox.md §5.2、§10)。
        const bool auditWritten = !state.inflight->carriesAudit || response.audit_outbox_written();
        if (!auditWritten && auditNotWrittenCount_++ % kAuditNotWrittenLogEveryN == 0)
        {
            LOG_ERROR << "[player_store] save of player " << playerId << " seq " << request.save_seq()
                      << " committed, but the store did not write its " << request.transaction_logs_size() << " transaction log(s) and "
                      << request.audit_snapshots_size() << " snapshot request(s) to the audit outbox (store older than scene?);"
                      << " keeping them for the next save (total=" << auditNotWrittenCount_ << ", metric=audit_outbox_store_too_old)";
        }
        Finish(playerId, state, result, /*committed=*/true, auditWritten);
        return;
    }
    case playerstore::SAVE_PLAYER_NO_OWNER:
    case playerstore::SAVE_PLAYER_SCHEMA_TOO_NEW:
    case playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED:
        // 写权行还没有,或者 go/db 比 scene 旧(发布顺序问题)。这笔没有提交,等它好了再试。日志和结果未知同一个节流。
        if (state.failedAttempts % kFailureLogEveryN == 0)
        {
            LOG_ERROR << "[player_store] save of player " << playerId << " seq " << request.save_seq()
                      << " will retry: " << playerstore::SavePlayerResult_Name(response.result());
        }
        ScheduleRetry(playerId, state);
        return;
    default:
        // 这一笔明确没有提交,但之前结果未知的那次发送可能已经提交了,库里是哪一份不知道,
        // 所以这些行必须并入脏集,否则改回原值的行不会被写。
        LOG_ERROR << "[player_store] save of player " << playerId << " seq " << request.save_seq()
                  << " rejected: " << playerstore::SavePlayerResult_Name(response.result())
                  << " owner_epoch=" << request.owner_epoch() << " row_owner_epoch=" << response.row_owner_epoch()
                  << " row_save_seq=" << response.row_save_seq();
        state.uncertain |= state.inflight->sentKinds;
        // 排队的快照带着同一个代次,发出去也只会再被拒,一并丢弃;代次变了的照常发。
        if (IsOwnerRejection(response.result()) && state.pending.has_value() && state.ownerEpoch == request.owner_epoch())
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

void PlayerStoreClient::Finish(Guid playerId, State &state, const SaveResult &result, bool committed, bool auditWritten)
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
        if (finished.carriesAudit)
        {
            state.auditStalled = !auditWritten;
        }
        // 先放行再回调:回调里可能销毁实体,那之后还没放行的会被当成没落库丢掉。
        if (auditWritten)
        {
            NotifyCommittedThrough(playerId, finished.auditAckMark);
        }
        if (finished.continuation.has_value() && !state.pending.has_value())
        {
            // 这一笔的审计记录没带完(Start 里分笔):行已经提交,紧接着再发一笔带剩下的记录,内容就是刚确认的行(没有脏行)。
            // 调用方的 release 与回调都交给它 —— 对调用方来说,这次存盘到最后一笔提交才算完成;它被拒,调用方看到的就是被拒。
            // 排队位置上已经有更新的快照时不用补:那份的水位更大,会带走剩下的记录,release 也以它为准,这一笔照常回调。
            Pending next;
            next.rows = state.confirmed;
            next.options = *finished.continuation;
            next.callbacks = std::move(finished.callbacks);
            next.commitMark = finished.commitMark;
            state.pending.emplace(std::move(next));
            Advance(playerId);
            return;
        }
    }
    // 回调里可能再次 Submit(进 pending),也可能 Forget,之后不再使用 state。
    Fire(finished.callbacks, result);
    Advance(playerId);
}

void PlayerStoreClient::NotifyCommittedThrough(Guid playerId, uint64_t commitMark) const
{
    if (options_.onCommittedThrough)
    {
        options_.onCommittedThrough(playerId, commitMark);
    }
}

void PlayerStoreClient::Advance(Guid playerId)
{
    // pending 可能没有变化，回调也可能再次 Submit 或关闭任务准入。
    while (SceneTaskScope::IsDurableAccepting())
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
        const StartResult started = Start(playerId, state, next.rows, next.options, next.callbacks, next.commitMark);
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
            // 排队的那份和已确认的内容一样:它里面的改动早已在库里。先放行再回调,理由同 Finish。
            NotifyCommittedThrough(playerId, next.commitMark);
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
    const std::weak_ptr<Lifetime> original = lifetime_;
    auto complete = [original, playerId, done = std::move(done)](bool ok, const playerstore::SavePlayerResponse* response)
    {
        auto* owner = ResolveOwner(original);
        if (owner == nullptr) return;
        owner->detachedInFlight_.erase(playerId);
        done(ok && response != nullptr, response != nullptr ? response->result() : playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED);
        owner = ResolveOwner(original);
        if (owner != nullptr && !owner->HasUnsettled(playerId))
        {
            owner->NotifySettled(playerId);
        }
    };
    if (!options_.send(request, complete)) complete(false, nullptr);
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
    if (!SceneTaskScope::IsDurableAccepting()) return;
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
    if (!SceneTaskScope::IsDurableAccepting()) return;
    auto request = options_.claimRequest(playerId);
    if (!request)
    {
        FinishClaim(playerId, nullptr);
        return;
    }
    const uint64_t attemptGen = ++claim.attemptGen;
    (void)SceneTaskScope::TryStartDurable(
        RunClaimAttempt(lifetime_, playerId, attemptGen, std::move(*request)), "PlayerStoreClaim");
}

boost::cobalt::task<void> PlayerStoreClient::RunClaimAttempt(std::weak_ptr<Lifetime> original, Guid playerId,
    uint64_t attemptGen, playerstore::LoadAndClaimRequest request)
{
    using Result = AttemptResult<playerstore::LoadAndClaimResponse>;
    auto submit = [&](auto done) {
        auto* owner = ResolveOwner(original);
        if (owner == nullptr) return false;
        return owner->options_.sendClaim(request,
            [done = std::move(done)](bool ok, const playerstore::LoadAndClaimResponse* reply) {
                DeliverAttempt(done, ok, reply);
            });
    };
    auto& loop = *muduo::net::EventLoop::getEventLoopOfCurrentThread();
    auto outcome = co_await loop_async::AwaitCallback<Result>(loop, std::move(submit), Result{});
    if (!outcome || !SceneTaskScope::IsDurableAccepting()) co_return;
    auto* owner = ResolveOwner(original);
    if (owner == nullptr) co_return;
    const auto it = owner->claims_.find(playerId);
    if (it == owner->claims_.end() || it->second.attemptGen != attemptGen) co_return;
    const auto& response = outcome->reply;
    if (!response)
    {
        owner->ScheduleClaimRetry(playerId, it->second);
        co_return;
    }
    if (response->result() != playerstore::LOAD_AND_CLAIM_CLAIMED)
    {
        owner->FinishClaim(playerId, &*response);
        co_return;
    }

    player_store_rows::KindBytes confirmed;
    player_store_rows::KindBytes carried;
    std::size_t carriedRows = 0;
    std::size_t newerRows = 0;
    for (const auto &row : response->rows())
    {
        const auto kind = static_cast<std::size_t>(row.kind());
        // 更新版本才有的行:不读、不播种。本二进制不会写它,库里那一行原样留着(player_store_projection.h IsNewerKind)。
        if (player_store_rows::IsNewerKind(kind))
        {
            ++newerRows;
            continue;
        }
        // 剩下不是数据行的只有退役号(旧分行布局的 1、2、5):按现在的分行解读别的布局写的行会丢字段(规格 §1.1)。
        if (!player_store_rows::IsDataKind(kind))
        {
            LOG_ERROR << "[player_store] claim of player " << playerId << " returned a retired kind " << kind
                      << " (a row from a retired row layout; the project has not launched, rebuild the database)";
            owner->FinishClaim(playerId, nullptr);
            co_return;
        }
        // 这一行里本二进制不认识的顶层字段:取出来随播种存下,之后每次写这一行都接回去(头文件类注释)。
        if (owner->options_.carriedBytes)
        {
            if (!owner->options_.carriedBytes(row.payload(), carried[kind]))
            {
                LOG_ERROR << "[player_store] claim of player " << playerId << " returned a row of kind " << kind
                          << " that cannot be parsed; refusing the entry";
                owner->FinishClaim(playerId, nullptr);
                co_return;
            }
            carriedRows += carried[kind].empty() ? 0 : 1;
        }
        confirmed[kind] = row.payload();
    }
    // 全网同一版本、也没有退役字段残留时两个数都是 0,不打。全进程第一次和此后每 kCarriedClaimLogEveryN 次打一条。
    if ((carriedRows != 0 || newerRows != 0) && owner->carriedClaimCount_++ % kCarriedClaimLogEveryN == 0)
    {
        LOG_INFO << "[player_store] claim of player " << playerId << " holds data this build does not know: unknown top-level fields in "
                 << carriedRows << " row(s) are carried back on every write, " << newerRows
                 << " row(s) of newer kinds are left untouched (total=" << owner->carriedClaimCount_
                 << ", metric=player_store_claim_carried)";
    }
    if (!owner->Seed(playerId, response->storage_id(), response->owner_epoch(), response->save_seq(), std::move(confirmed), std::move(carried)))
    {
        LOG_ERROR << "[player_store] claim of player " << playerId << " landed while a save is unsettled; refusing the entry";
        owner->FinishClaim(playerId, nullptr);
        co_return;
    }
    owner->FinishClaim(playerId, &*response);
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
    const std::weak_ptr<Lifetime> original = lifetime_;
    options_.schedule(delay, [original, playerId, attemptGen = claim.attemptGen]()
    {
        auto* owner = ResolveOwner(original);
        if (owner == nullptr) return;
        const auto it = owner->claims_.find(playerId);
        if (it == owner->claims_.end() || it->second.attemptGen != attemptGen) return;
        owner->AttemptClaim(playerId, it->second);
    });
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
