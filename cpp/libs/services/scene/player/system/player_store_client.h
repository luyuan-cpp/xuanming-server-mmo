#pragma once

#include <bitset>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "engine/core/type_define/type_define.h"
#include "player/system/player_store_projection.h"
#include "proto/player_store/player_store.pb.h"

enum class PlayerStoreSaveStatus : uint8_t
{
    kCommitted,        // 已提交(COMMITTED 或 EQUAL)
    kAlreadyPersisted, // 排队的快照没有变化,不需要写
    kRejected,         // 明确没有提交,原因看 result;序号饱和时 result 是 UNSPECIFIED
};

struct PlayerStoreSaveResult
{
    PlayerStoreSaveStatus status{PlayerStoreSaveStatus::kRejected};
    playerstore::SavePlayerResult result{playerstore::SAVE_PLAYER_RESULT_UNSPECIFIED};
    uint64_t rowOwnerEpoch{0};
    uint64_t rowSaveSeq{0};
    // kCommitted、kRejected 是这一笔的代次和序号;kAlreadyPersisted 是上一次提交的值,没提交过是 0。
    uint64_t ownerEpoch{0};
    uint64_t saveSeq{0};
    // 这份快照发出之前就被更新的快照取代了:它没有被写,这里的结局属于取代它的那份。
    bool superseded{false};
};

// 玩家存盘的状态机(规格见 docs/design/player-save-direct-commit.md §5.1)。
//
// 每个玩家同时只有一个在途存盘。结果未知就用同样的内容、同一个序号、同一个代次重试,直到拿到明确结果
// 才发下一笔;在途期间到来的新快照覆盖 pending,排队的是完整快照,所以覆盖不丢行。
//
// 状态以 Guid 为键放在这里,不挂在实体上:实体销毁时存盘还可能在途,结果要有地方落。
// 存盘未落定时不能重新播种(Seed 返回 false),所以迟到的回包不可能改到重新加载之后的状态。
//
// 传输、定时器、发号都是注入的,单测用假实现。单线程语义:所有入口都在 scene 逻辑线程上。
// 回包与定时器的闭包捕获 this,实例必须活得比事件循环长(生产用线程局部实例,AGENTS §11.7 的同寿命例外)。
class PlayerStoreClient
{
public:
    using SaveStatus = PlayerStoreSaveStatus;
    using SaveResult = PlayerStoreSaveResult;

    // 恰好调用一次,不捕获实体句柄:回调时实体可能已经销毁,用 Guid 重新查。
    using SaveDone = std::function<void(const SaveResult &)>;

    struct SubmitOptions
    {
        // 最终存盘:提交后释放写权。没有脏行也要发,挂机下线是常态,不发的话写权永远不释放。
        bool release{false};
        // 没有脏行也要发一笔:调用方要的就是一次确定的提交。
        bool force{false};
        uint32_t schemaGen{0};
    };

    enum class SubmitResult : uint8_t
    {
        kAccepted,         // 已发送或已排队,结果走 SaveDone
        kAlreadyPersisted, // 没有变化也不需要释放,SaveDone 不会被调用
        kNotSeeded,        // 没有播种过,SaveDone 不会被调用
        kSeqExhausted,     // 序号饱和,这一笔没有发出去,SaveDone 不会被调用
    };

    // ok=false 表示结果未知。response 只在回调期间有效。
    using SendDone = std::function<void(bool ok, const playerstore::SavePlayerResponse *response)>;
    // 返回 false = 此刻发不出去(没有可用节点),按退避重试,done 不会被调用。
    // 每次发送都要重新签名:签名有时间窗,故障久了不重签就永久失败。
    using SendFn = std::function<bool(const playerstore::SavePlayerRequest &request, SendDone done)>;
    using ScheduleFn = std::function<void(double delaySec, std::function<void()> fn)>;

    // 认领的传输。约定同 SendDone、SendFn。
    using ClaimDone = std::function<void(bool ok, const playerstore::LoadAndClaimResponse *response)>;
    using SendClaimFn = std::function<bool(const playerstore::LoadAndClaimRequest &request, ClaimDone done)>;
    using Clock = std::chrono::steady_clock;

    struct Options
    {
        SendFn send;
        ScheduleFn schedule;
        // 存盘序号。只有确定要写才取号。nullopt = 序号饱和,这一笔按拒绝处理,不回绕。
        std::function<std::optional<uint64_t>(Guid playerId)> nextSeq;
        // 本进程的 scene_instance_id。
        std::function<std::string()> instanceId;
        // 可选:有脏行时在 kProfile 的字节上打压测探针。比脏不看探针,否则玩家恒脏。
        std::function<void(Guid playerId, uint64_t saveSeq, std::string &profileBytes)> stampProfile;
        // 可选:这个玩家的在途和排队都清空时调用。
        std::function<void(Guid playerId)> onSettled;
        double retryMinSec{0.5};
        double retryMaxSec{5.0};
        // 可选:退避的抖动,入参是基础延迟,返回实际延迟。go/db 重启时在途的存盘同一拍失败,不抖就一起重试。
        std::function<double(double delaySec)> jitter;
        // 一笔存盘序列化后的上限,比传输上限(4 MiB)留出签名的余量。超过的发不出去:通道在本地就拒了,
        // 那会被当成结果未知永远重试,所以这里按 TOO_LARGE 处理。
        std::size_t maxRequestBytes{(4u * 1024 * 1024) - (64u * 1024)};

        SendClaimFn sendClaim;
        // 发送时才组请求:载入期间路由可能已经覆盖了待入场上下文。nullopt = 这次载入已经放弃。
        std::function<std::optional<playerstore::LoadAndClaimRequest>(Guid playerId)> claimRequest;
        // 认领结束,恰好一次。CLAIMED 时已经播种。response 为空 = 没有结论(预算用完、载入已放弃、响应不合法)。
        std::function<void(Guid playerId, const playerstore::LoadAndClaimResponse *response)> onClaimed;
        // 结果未知时的重试预算,从第一次发送算起。
        double claimBudgetSec{30.0};
        // 可选:缺省是单调时钟。
        std::function<Clock::time_point()> now;
    };

    explicit PlayerStoreClient(Options options);
    PlayerStoreClient(const PlayerStoreClient &) = delete;
    PlayerStoreClient &operator=(const PlayerStoreClient &) = delete;

    // 认领成功后调用。confirmed 必须是库里的原字节,不能是加载后重新序列化的结果:否则加载时改过的字段
    // 不会被写回。存盘未落定时返回 false,调用方要等落定再加载。
    // committedSaveSeq 是响应里的 save_seq,新号是 0。
    bool Seed(Guid playerId, uint32_t storageId, uint64_t ownerEpoch, uint64_t committedSaveSeq, player_store_rows::KindBytes confirmed);

    // 本地代次变了(路由刷新、复用实体)。在途那笔不变,下一笔用新值。比库里新就算没有脏行也会写一笔,把代次抬上去。
    void SetOwnerEpoch(Guid playerId, uint64_t ownerEpoch);

    // 提交一份完整快照。rows 是未打探针的投影结果。
    SubmitResult Submit(Guid playerId, player_store_rows::KindBytes rows, const SubmitOptions &options, SaveDone done);

    // 发一笔不带状态的空行释放:实体已经没了(也没有播种状态)时用,比如会话结束凭证。storage_id 发 0,
    // 由 go/db 查落点。不重试:结果未知就算没释放,调用方的下一次请求会再来。done 恰好调用一次,ok=false 是结果未知。
    // 在途期间 HasUnsettled 为真:它提交时会把 released 置 1,不能和本节点的下一次认领交错。
    using ReleaseDone = std::function<void(bool ok, playerstore::SavePlayerResult result)>;
    void ReleaseDetached(Guid playerId, uint64_t ownerEpoch, uint32_t schemaGen, ReleaseDone done);

    // 认领写权并读出存档(规格 §3)。已有在途的不重发,完成时调用方用最新的待入场上下文。
    // 存盘未落定时先等它落定:同代次销毁重载时,晚到的那一笔会让库和内存分叉。结果未知就重试,认领是幂等的。
    void RequestClaim(Guid playerId);

    // 这次认领还没有结果(含等待存盘落定)。
    [[nodiscard]] bool HasPendingClaim(Guid playerId) const;

    [[nodiscard]] bool HasUnsettled(Guid playerId) const;

    // 这个玩家的存盘都落定之后调用一次(提交或被拒都算);此刻没有未落定的存盘就立刻调用。
    // 取代旧路径同一条连接上的 PING 快路径。闭包不捕获实体句柄,用 Guid 重新查。
    void RunWhenSettled(Guid playerId, std::function<void()> fn);

    // 还有未落定存盘的玩家。只用于停机时留证据。
    [[nodiscard]] std::vector<Guid> UnsettledPlayers() const;

    // 玩家离开本节点且存盘已落定时调用。未落定返回 false,状态保留,等落定时由调用方再次 Forget。
    bool Forget(Guid playerId);

private:
    using KindSet = std::bitset<player_store_rows::kKindCount>;

    struct Callbacks
    {
        std::vector<SaveDone> superseded;
        SaveDone latest;
    };

    struct InFlight
    {
        // 重试发的就是它:同内容、同序号、同代次。
        playerstore::SavePlayerRequest request;
        KindSet sentKinds;
        // 提交成功后写进 confirmed 的字节,不含探针。
        player_store_rows::KindBytes confirmOnCommit;
        Callbacks callbacks;
    };

    struct Pending
    {
        player_store_rows::KindBytes rows;
        SubmitOptions options;
        Callbacks callbacks;
    };

    struct State
    {
        uint32_t storageId{0};
        uint64_t ownerEpoch{0};
        player_store_rows::KindBytes confirmed;
        // 结果未决的 kind:上一笔可能已经提交,不知道库里是哪一份,所以必须再写一次。
        KindSet uncertain;
        // 上一次提交带着 release:库里 released=1,后面的存盘即使没有脏行也要写一笔把它清掉。
        bool releaseCommitted{false};
        // 库里已确认的代次与序号:认领或上一次提交的值。代次落后于 ownerEpoch 时必须写一笔。
        uint64_t committedOwnerEpoch{0};
        uint64_t committedSaveSeq{0};
        std::optional<InFlight> inflight;
        std::optional<Pending> pending;
        double backoffSec{0};
        // 每次发送 +1;回包和定时器带着当时的值,只对当次生效。
        uint64_t attemptGen{0};
        // 这一笔连续失败的次数,只用来打日志。
        uint32_t failedAttempts{0};
    };

    enum class StartResult : uint8_t
    {
        kSent,
        kNothingToWrite,
        kSeqExhausted,
    };

    struct Claim
    {
        bool waitingForSettle{false};
        // 第一次发送时定下。
        Clock::time_point deadline{};
        double backoffSec{0};
        // 每次发送 +1;回包和定时器带着当时的值,只对当次生效。
        uint64_t attemptGen{0};
        uint32_t failedAttempts{0};
    };

    [[nodiscard]] KindSet DirtyKinds(const State &state, const player_store_rows::KindBytes &rows) const;
    // 开始一笔。没有发出去时 callbacks 不动,由调用方处置。
    StartResult Start(Guid playerId, State &state, const player_store_rows::KindBytes &rows, const SubmitOptions &submitOptions, Callbacks &callbacks);
    void Attempt(Guid playerId, State &state);
    void ScheduleRetry(Guid playerId, State &state);
    void OnAttemptDone(Guid playerId, uint64_t attemptGen, bool ok, const playerstore::SavePlayerResponse *response);
    void Finish(Guid playerId, State &state, const SaveResult &result, bool committed);
    void Advance(Guid playerId);
    // 这个玩家的存盘都落定了:通知调用方,再放行等着它的认领。
    void NotifySettled(Guid playerId);
    static void Fire(Callbacks &callbacks, const SaveResult &result);

    [[nodiscard]] double Jittered(double delaySec) const;
    [[nodiscard]] Clock::time_point Now() const;
    void StartClaim(Guid playerId, Claim &claim);
    void AttemptClaim(Guid playerId, Claim &claim);
    void ScheduleClaimRetry(Guid playerId, Claim &claim);
    void OnClaimAttemptDone(Guid playerId, uint64_t attemptGen, bool ok, const playerstore::LoadAndClaimResponse *response);
    void FinishClaim(Guid playerId, const playerstore::LoadAndClaimResponse *response);

    Options options_;
    std::unordered_map<Guid, State> states_;
    std::unordered_set<Guid> detachedInFlight_;
    std::unordered_map<Guid, Claim> claims_;
    std::unordered_map<Guid, std::vector<std::function<void()>>> settledWaiters_;
};

// 传输与定时器:生产用 gRPC 和事件循环,单测换成假的,其余接线不变。
struct PlayerStoreTransport
{
    PlayerStoreClient::SendFn send;
    PlayerStoreClient::ScheduleFn schedule;
    PlayerStoreClient::SendClaimFn sendClaim;
};

// 无状态释放的这个结局能不能让调用方写释放标记。调用方这时已经没有实体,也没有未落定的存盘,这个实例不会再写了,
// 所以只要结局说明这笔释放不用再试,标记的语义就成立:
//   COMMITTED、EQUAL         释放提交了;
//   NO_OWNER、UNCLAIMED      这个实例在这个代次上没有认领过,没有什么要释放;
//   STALE_OWNER、NOT_HOLDER  写权行已经不归这个实例(判死释放过,或者新铸的代次恰好等于库里的值);
//   OLDER_SEQ                这个实例还是持有者,但发号器落后于库里:released 留在 0,下次登录解。
// 这些不写标记的话,同节点重登不铸造、认领一直被拒,玩家要锁死到节点重启。
// 其余(INVALID、世代不符)是部署或代码问题,不写,修好之后下一次 LeaveScene 再来。
bool DetachedReleaseSettled(playerstore::SavePlayerResult result);


// 新路径的启动校验:字段分组完整、密钥合格。false 时 error 说明原因,调用方拒绝启动。
bool ValidatePlayerStoreStartup(std::string &error);

// 生产用的发送函数:选本 zone 内已连接的 go/db,用 MMORPG_PLAYER_STORE_SECRET_SCENE 重新签名后发送。
bool SendPlayerStoreSaveOverGrpc(const playerstore::SavePlayerRequest &request, PlayerStoreClient::SendDone done);
// 同上,用于认领。
bool SendPlayerStoreClaimOverGrpc(const playerstore::LoadAndClaimRequest &request, PlayerStoreClient::ClaimDone done);
