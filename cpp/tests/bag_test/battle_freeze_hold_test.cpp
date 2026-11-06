// FIGHTING 冻结到期之后的有界续冻(2026-11-06,数据覆写审计 R2 r2-v1#1 残余窗口)与
// 资产通道整批预检的号段续段(r2-battle#3)。设计见 docs/design/turn-battle-gap-closure.md §11。
//
// 不连 Redis、不连 match:SQL 终局查询、条件删锁、组队跟随唤醒、锁脚本都经 PlayerBattleSystem 的注入点替换,
// reaper 的时刻由用例显式给(ReapExpiredFreezes(nowMs))。资产通道走真实的 Debit / Credit 入口与签名。
// 生产传输(battle_terminal_query::Send)的回包归一化单独由 BattleTerminalQueryReply.* 盯住。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "muduo/net/EventLoop.h"

#include "core/security/service_call_auth.h"
#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h" // 资产通道续段夹具先清掉别的用例残留的全局封禁
#include "modules/id_segment/guid_segment_registry.h"
#include "player/system/player_inbox_rules.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/common/event/battle_event.pb.h" // BattleConfirmedEvent:开局确认写锁
#include "proto/match/match_internal.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "security/token_security.h"
#include "services/scene/battle/system/battle_freeze_hold_rules.h"
#include "services/scene/battle/system/battle_terminal_query.h"
#include "services/scene/battle/system/player_battle.h"
#include "services/scene/core/system/scene_task_scope.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/asset_op_auth.h"
#include "services/scene/player/system/asset_op_system.h"
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

// 冻结到期续冻的私有入口(reaper 第一遍、作废槽位、查询在途登记)。只转调生产函数,不复制任何判定。
class PlayerBattleFreezeHoldTestAccess
{
public:
    static void ReapExpiredFreezes(uint64_t nowMs) { PlayerBattleSystem::ReapExpiredFreezes(nowMs); }
    static uint64_t ExpiredFightingBattleId(entt::entity player) { return PlayerBattleSystem::ExpiredFightingBattleId(player); }
    static bool IsTerminalQueryInFlight(uint64_t playerId, uint64_t battleId)
    {
        return PlayerBattleSystem::IsTerminalQueryInFlight(playerId, battleId);
    }
};

namespace
{
using battle_freeze_hold::ExpiredFightingStep;
using battle_freeze_hold::FreezeHoldEvent;
using battle_freeze_hold::TerminalAnswer;

// ── 注入点的记录器(都是不带捕获的函数指针,状态只能放在文件级;夹具进出各清一次)──────────────────

// SQL 终局查询:记下每一次查询与它的 done,由用例决定何时、回什么。sendable=false 模拟「没发出去」。
struct RecordedTerminalQuery
{
    uint64_t battleId = 0;
    std::function<void(TerminalAnswer)> done;
};
std::vector<RecordedTerminalQuery> gTerminalQueries;
bool gTerminalQuerySendable = true;

bool RecordTerminalQuery(uint64_t battleId, std::function<void(TerminalAnswer)> done)
{
    if (!gTerminalQuerySendable)
    {
        return false;
    }
    gTerminalQueries.push_back({battleId, std::move(done)});
    return true;
}

// 回答最早的一次查询。没有在途查询时返回 false。
bool AnswerOldestQuery(TerminalAnswer answer)
{
    if (gTerminalQueries.empty())
    {
        return false;
    }
    auto query = std::move(gTerminalQueries.front());
    gTerminalQueries.erase(gTerminalQueries.begin());
    query.done(answer);
    return true;
}

// 条件删锁:只记 (player_id, battle_id),不回包。
std::vector<std::pair<uint64_t, uint64_t>> gLockReleases;
bool RecordLockRelease(uint64_t playerId, uint64_t battleId, std::function<void(redisReply*)>)
{
    gLockReleases.emplace_back(playerId, battleId);
    return true;
}

void IgnoreFollowWake(entt::entity) {}

// 锁脚本观察点:续冻时锁跟着续,盯它发的是哪种脚本、TTL 多长。
struct ObservedLockScript
{
    std::string op;
    uint64_t battleId = 0;
    uint64_t ttlSec = 0;
};
std::vector<ObservedLockScript> gLockScripts;
void ObserveLockScript(const char* op, uint64_t, uint64_t battleId, uint64_t ttlSec, const std::string&)
{
    gLockScripts.push_back({op, battleId, ttlSec});
}
// 不连 Redis:没问到之后不排重试,链当场结束(续期在途登记随之摘除)。
bool RejectLockRetry(uint64_t, std::function<void()>) { return false; }

// 终局查询判丢失用的单调钟(PlayerBattleSystem::SetTerminalQueryClockForTest):用例手动拨,与墙钟(ReapExpiredFreezes 的 nowMs)
// 各走各的,才能分别盯住「判丢失只看单调钟」「判上限只看墙钟」。
uint64_t gSteadyNowMs = 0;
uint64_t FakeSteadyNowMs() { return gSteadyNowMs; }
constexpr uint64_t kReaperTickMs = static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);

// 资产通道注入:时钟、存盘、密钥都替掉(与 asset_op_system_test 同一套路)。
constexpr std::string_view kHoldAssetOpSecret = "battle-freeze-hold-asset-op-secret-0123456789";
int64_t gAssetOpNowMs = 1'790'000'000'000;
int64_t AssetOpNowMs() { return gAssetOpNowMs; }
bool AssetOpPersist(entt::entity) { return true; }
std::string AssetOpSecret(std::string_view) { return std::string(kHoldAssetOpSecret); }

constexpr uint64_t kBattleId = 880011;
constexpr uint64_t kSyntheticDeadlineMs = 1'800'000'000'000ULL;

uint64_t CountOf(FreezeHoldEvent event) { return PlayerBattleSystem::FreezeHoldEventCount(event); }

class BattleFreezeHoldTest : public testing::Test
{
protected:
    void SetUp() override
    {
        static uint64_t nextPlayer = 9877700;
        playerId = ++nextPlayer;
        // 续冻时锁跟着续(ConfirmBattleLockIfMatch → 协程任务):需要当前线程的 loop 与任务作用域。
        if (muduo::net::EventLoop::getEventLoopOfCurrentThread() == nullptr)
        {
            loop_ = std::make_unique<muduo::net::EventLoop>();
        }
        if (!SceneTaskScope::IsAccepting())
        {
            tasks_ = std::make_unique<SceneTaskScope>(*muduo::net::EventLoop::getEventLoopOfCurrentThread());
        }
        gTerminalQueries.clear();
        gTerminalQuerySendable = true;
        gLockReleases.clear();
        gLockScripts.clear();
        gSteadyNowMs = 5'000'000;
        PlayerBattleSystem::SetTerminalQueryClockForTest(&FakeSteadyNowMs);
        PlayerBattleSystem::SetBattleTerminalQueryForTest(&RecordTerminalQuery);
        PlayerBattleSystem::SetBattleLockReleaseSenderForTest(&RecordLockRelease);
        PlayerBattleSystem::SetFollowWakeFnForTest(&IgnoreFollowWake);
        PlayerBattleSystem::SetLockScriptObserverForTest(&ObserveLockScript);
        PlayerBattleSystem::SetLockRetrySchedulerForTest(&RejectLockRetry);

        player = tlsEcs.actorRegistry.create();
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{playerId});
        tlsEcs.playerList.erase(playerId);
        tlsEcs.playerList.emplace(playerId, player);
    }

    void TearDown() override
    {
        PlayerBattleSystem::SetBattleTerminalQueryForTest(nullptr);
        PlayerBattleSystem::SetTerminalQueryClockForTest(nullptr);
        PlayerBattleSystem::SetBattleLockReleaseSenderForTest(nullptr);
        PlayerBattleSystem::SetFollowWakeFnForTest(nullptr);
        PlayerBattleSystem::SetLockScriptObserverForTest(nullptr);
        PlayerBattleSystem::SetLockRetrySchedulerForTest(nullptr);
        PlayerAssetOpSystem::SetNowFnForTest(nullptr);
        PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
        PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
        tlsEcs.playerList.erase(playerId);
        if (tlsEcs.actorRegistry.valid(player))
        {
            tlsEcs.actorRegistry.destroy(player);
        }
        tasks_.reset();
        if (loop_)
        {
            loop_->queueInLoop([loop = loop_.get()] { loop->quit(); });
            loop_->loop();
        }
        gTerminalQueries.clear();
        gLockReleases.clear();
        gLockScripts.clear();
    }

    InBattleComp& Fight(uint64_t deadlineMs)
    {
        auto& inBattle = tlsEcs.actorRegistry.emplace_or_replace<InBattleComp>(player);
        inBattle.set_battle_id(kBattleId);
        inBattle.set_state(IN_BATTLE_STATE_FIGHTING);
        inBattle.set_deadline_ms(deadlineMs);
        return inBattle;
    }

    // 帮会捐献流扣金币(GUILD_DEBIT / TX_GUILD_DONATE,caller = guild),真实入口 + 签名。
    ::AssetOpResponse GuildDebitGold(uint64_t seq, uint64_t amount)
    {
        ::AssetOpRequest request;
        request.set_player_id(playerId);
        request.set_stream(ASSET_OP_STREAM_GUILD_DEBIT);
        request.set_seq(seq);
        request.set_correlation_id(77);
        request.set_tx_type(static_cast<uint32_t>(TX_GUILD_DONATE));
        request.set_stream_epoch(100);
        auto* currency = request.mutable_bundle()->add_currencies();
        currency->set_currency_type(kCurrencyGold);
        currency->set_amount(amount);
        auto* auth = request.mutable_auth();
        auth->set_caller("guild");
        auth->set_timestamp_ms(static_cast<uint64_t>(gAssetOpNowMs));
        auth->set_signature_hex(token_security::HmacSha256Hex(kHoldAssetOpSecret, AssetOpCanonical("debit", request)));
        ::AssetOpResponse response;
        PlayerAssetOpSystem::Debit(request, response);
        return response;
    }

    // 在收件箱战斗结算域的最近结局环里记一条结局,走状态机记账同一个入口(player_inbox_rules::RecordDecision)。
    void RecordInboxDecision(uint64_t seq, player_inbox::PlayerInboxOutcome outcome, uint64_t requestId,
                             player_inbox::PlayerInboxMessageType msgType = player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
    {
        auto& ledger = tlsEcs.actorRegistry.get_or_emplace<PlayerInboxLedgerComp>(player);
        auto& cursor = player_inbox_rules::MutableCursor(
            ledger, static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT));
        PlayerInboxDecision decision;
        decision.set_seq(seq);
        decision.set_outcome(static_cast<uint32_t>(outcome));
        decision.set_request_id(requestId);
        decision.set_msg_type(static_cast<uint32_t>(msgType));
        ASSERT_TRUE(player_inbox_rules::RecordDecision(cursor, decision));
    }

    // 资产通道入口要的加载组件(与 asset_op_system_test 的夹具同一组)。
    void GiveAssetChannel()
    {
        PlayerAssetOpSystem::SetNowFnForTest(&AssetOpNowMs);
        PlayerAssetOpSystem::SetPersistFnForTest(&AssetOpPersist);
        PlayerAssetOpSystem::SetSecretLookupForTest(&AssetOpSecret);
        tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player);
        tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
    }

    uint64_t playerId{};
    entt::entity player{entt::null};

private:
    std::unique_ptr<muduo::net::EventLoop> loop_;
    std::unique_ptr<SceneTaskScope> tasks_;
};

// ── 纯判定 ──────────────────────────────────────────────────────────────────────────────

// ABORT 与「没有可裁决原件」(NO_ORIGINAL)照常解冻;COMPLETE(本地还没应用)、PENDING(有原件的 NONE,2026-11-06 负责人定:
// NONE 不是终局证据)与 UNKNOWN 在上限之内续冻、越过上限才解冻;不认识的取值按 UNKNOWN。
TEST(BattleFreezeHoldRules, DecisionTableAndHoldDeadline)
{
    constexpr uint64_t kCapEndMs = 5'000;
    for (const auto answer : {TerminalAnswer::kAbort, TerminalAnswer::kNoOriginal})
    {
        EXPECT_EQ(ExpiredFightingStep::kRelease, battle_freeze_hold::DecideExpiredFighting(answer, 1, kCapEndMs));
        EXPECT_EQ(ExpiredFightingStep::kRelease, battle_freeze_hold::DecideExpiredFighting(answer, kCapEndMs + 1, kCapEndMs))
            << "不会再有结算的局,过了上限也是照常解冻(不算告警)";
    }
    for (const auto answer : {TerminalAnswer::kComplete, TerminalAnswer::kPending, TerminalAnswer::kUnknown,
                              static_cast<TerminalAnswer>(200)})
    {
        EXPECT_EQ(ExpiredFightingStep::kHold, battle_freeze_hold::DecideExpiredFighting(answer, 1, kCapEndMs));
        EXPECT_EQ(ExpiredFightingStep::kHold, battle_freeze_hold::DecideExpiredFighting(answer, kCapEndMs, kCapEndMs))
            << "恰好到上限还续(严格大于才算越过)";
        EXPECT_EQ(ExpiredFightingStep::kReleaseAtCap, battle_freeze_hold::DecideExpiredFighting(answer, kCapEndMs + 1, kCapEndMs));
    }
    EXPECT_STREQ("COMPLETE", battle_freeze_hold::TerminalAnswerName(TerminalAnswer::kComplete));
    EXPECT_STREQ("PENDING", battle_freeze_hold::TerminalAnswerName(TerminalAnswer::kPending));
    EXPECT_STREQ("NO_ORIGINAL", battle_freeze_hold::TerminalAnswerName(TerminalAnswer::kNoOriginal));
    EXPECT_STREQ("?", battle_freeze_hold::TerminalAnswerName(static_cast<TerminalAnswer>(200)));

    EXPECT_EQ(15u * 60u * 1000u, PlayerBattleSystem::kFreezeHoldCapMs);
    EXPECT_EQ(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + PlayerBattleSystem::kFreezeHoldCapMs,
              PlayerBattleSystem::FreezeHoldDeadlineMs(kSyntheticDeadlineMs));
    EXPECT_EQ(0u, PlayerBattleSystem::FreezeHoldDeadlineMs(0)) << "没有期限:reaper 不判到期,也就没有续冻";
    EXPECT_EQ(UINT64_MAX, PlayerBattleSystem::FreezeHoldDeadlineMs(UINT64_MAX - 1)) << "上溢饱和";
    static_assert(PlayerBattleSystem::FreezeHoldDeadlineMs(1) ==
                      PlayerBattleSystem::FreezeExpiresAtMs(false, 1, 0) + PlayerBattleSystem::kFreezeHoldCapMs,
                  "续冻上限是编译期可求值的纯函数");
}

// 评审修正(2026-11-06):ctx 缺失时按锁剩余 TTL 反推 deadline。锁 TTL 含续冻上限,看得出是 FIGHTING 锁时扣掉锁余量、续冻上限、
// 终局宽限与投递余量,首次查询落在原来的到期时刻:写锁时 EX 的秒级下取整、TTL 读数的四舍五入都只让它偏晚,不超过两秒。
// 修复前直接取「现在 + TTL」,首次查询晚约 18 分钟。分不清是不是备战期的锁时照旧取「现在 + TTL」。
TEST(BattleFreezeHoldRules, LockTtlDeadlineInferenceLandsOnTheOriginalExpiry)
{
    constexpr uint64_t kDeadlineMs = kSyntheticDeadlineMs;
    const uint64_t originalExpiryMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kDeadlineMs, 0);
    const uint64_t holdDeadlineMs = PlayerBattleSystem::FreezeHoldDeadlineMs(kDeadlineMs);
    // 锁按 FightingLockTtlSecFor 写:EX = floor((续冻上限 - 写锁时刻) / 1000) + kLockExtraTtlSec。Redis 的 TTL 读数四舍五入到秒。
    for (const uint64_t writtenBeforeDeadlineMs : {200'000ULL, 200'400ULL, 200'999ULL})
    {
        const uint64_t writtenAtMs = kDeadlineMs - writtenBeforeDeadlineMs;
        const uint64_t exSec = (holdDeadlineMs - writtenAtMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec;
        const uint64_t lockExpiresAtMs = writtenAtMs + exSec * 1000;
        for (const uint64_t rebuiltAtMs :
             {kDeadlineMs - 100'000, kDeadlineMs + 10'000, kDeadlineMs + 10'499, originalExpiryMs - 5'000})
        {
            SCOPED_TRACE(testing::Message() << "written_before_deadline_ms=" << writtenBeforeDeadlineMs
                                            << " rebuilt_at_ms=" << rebuiltAtMs);
            const auto ttlSec = static_cast<int64_t>((lockExpiresAtMs - rebuiltAtMs + 500) / 1000);
            const uint64_t inferredMs = PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(rebuiltAtMs, ttlSec);
            const uint64_t inferredExpiryMs = PlayerBattleSystem::FreezeExpiresAtMs(false, inferredMs, 0);
            EXPECT_GE(inferredExpiryMs, originalExpiryMs) << "只会偏晚";
            EXPECT_LE(inferredExpiryMs, originalExpiryMs + 2'000) << "偏晚不超过两秒";
        }
    }

    // 剩余 TTL 不超过「续冻上限 + 锁余量」:备战期的锁(只有备战期限 + 60 秒)与续冻后段的 FIGHTING 锁分不清,照旧取「现在 + TTL」。
    constexpr uint64_t kNowMs = 1'800'000'000'000ULL;
    constexpr auto kAmbiguousTtlSec =
        static_cast<int64_t>(PlayerBattleSystem::kFreezeHoldCapMs / 1000 + PlayerBattleSystem::kLockExtraTtlSec);
    EXPECT_EQ(kNowMs + 90'000, PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, 90)) << "备战期的锁";
    EXPECT_EQ(kNowMs + static_cast<uint64_t>(kAmbiguousTtlSec) * 1000,
              PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, kAmbiguousTtlSec))
        << "恰好等于上限 + 余量:仍按保守口径";
    EXPECT_LT(PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, kAmbiguousTtlSec + 1), kNowMs)
        << "多一秒就看得出是 FIGHTING 锁,按精确口径:deadline 已在现在之前";
    EXPECT_EQ(kNowMs, PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, -1)) << "锁没有过期时刻";
    EXPECT_EQ(kNowMs, PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, -2)) << "锁已不在";
    EXPECT_EQ(kNowMs, PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, 0));
    EXPECT_NE(0u, PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(0, kAmbiguousTtlSec + 1))
        << "下溢时也给非 0 的期限,reaper 一定判得到期";
    static_assert(PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs(kNowMs, 90) == kNowMs + 90'000,
                  "反推是编译期可求值的纯函数");
}

// ── reaper 续冻 ────────────────────────────────────────────────────────────────────────

// 到期时 SQL 终局已 COMPLETE、本地还没应用:冻结保留、锁不放、不记作废局;锁跟着续到续冻上限;同一拍不重复查,下一拍再查。
TEST_F(BattleFreezeHoldTest, CompleteNotAppliedHereKeepsTheFreezeAndExtendsTheLock)
{
    // 期限放在真实时钟之前:续期脚本按真实时钟算 TTL,续冻上限离真实时钟只剩十几分钟,TTL 断言才收得紧。
    const uint64_t realNowMs = TimeSystem::NowMillisecondsUTC();
    const uint64_t deadlineMs = realNowMs - 200'000;
    Fight(deadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, deadlineMs, 0);
    const uint64_t heldBefore = CountOf(FreezeHoldEvent::kHeldComplete);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    ASSERT_EQ(1u, gTerminalQueries.size()) << "到期先查 SQL 终局";
    EXPECT_EQ(kBattleId, gTerminalQueries.front().battleId);
    EXPECT_TRUE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "回答到达之前冻结不动";
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 2);
    EXPECT_EQ(1u, gTerminalQueries.size()) << "同一局至多一条查询在途";

    const uint64_t sentNotBeforeMs = TimeSystem::NowMillisecondsUTC();
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kComplete));
    const uint64_t sentNotAfterMs = TimeSystem::NowMillisecondsUTC();
    EXPECT_FALSE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "COMPLETE 而本地还没应用:续冻";
    EXPECT_TRUE(gLockReleases.empty()) << "续冻期间锁不能放";
    EXPECT_EQ(0u, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player)) << "续冻不是作废";
    EXPECT_EQ(heldBefore + 1, CountOf(FreezeHoldEvent::kHeldComplete));
    ASSERT_EQ(1u, gLockScripts.size()) << "锁跟着续";
    EXPECT_EQ("renew_lock", gLockScripts.front().op);
    EXPECT_EQ(kBattleId, gLockScripts.front().battleId);
    const uint64_t holdDeadlineMs = PlayerBattleSystem::FreezeHoldDeadlineMs(deadlineMs);
    EXPECT_GE(gLockScripts.front().ttlSec, (holdDeadlineMs - sentNotAfterMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec)
        << "续冻的锁要活过续冻上限";
    EXPECT_LE(gLockScripts.front().ttlSec, (holdDeadlineMs - sentNotBeforeMs) / 1000 + PlayerBattleSystem::kLockExtraTtlSec);

    const uint64_t nextTickMs = expiresAtMs + 1 + static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(nextTickMs);
    EXPECT_EQ(1u, gTerminalQueries.size()) << "续冻一拍:下一拍再查一次";
}

// 续冻到上限仍没在本地收尾:打告警计数,再按现行规则解冻(记作废局、条件删锁、摘冻结)。
// 越过上限之后的处置只有解冻一种,这一拍不再查询(评审修正 2026-11-06:解冻不晚于越过上限后的第一拍,锁一定还在)。
TEST_F(BattleFreezeHoldTest, HoldCapReleasesTheFreezeAndCountsTheAlarm)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t pastCapMs = PlayerBattleSystem::FreezeHoldDeadlineMs(kSyntheticDeadlineMs) + 1;
    const uint64_t capBefore = CountOf(FreezeHoldEvent::kCapReached);
    const uint64_t heldBefore = CountOf(FreezeHoldEvent::kHeldComplete);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(pastCapMs);
    EXPECT_TRUE(gTerminalQueries.empty()) << "越过上限不再查询";

    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "到上限:按现行规则解冻";
    EXPECT_EQ(kBattleId, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(std::make_pair(playerId, kBattleId), gLockReleases.front());
    EXPECT_EQ(capBefore + 1, CountOf(FreezeHoldEvent::kCapReached)) << "到上限计一次告警";
    EXPECT_EQ(heldBefore, CountOf(FreezeHoldEvent::kHeldComplete));
}

// 查询失败(没问到)在上限内续冻一拍,同样受上限约束;越过上限的「未知」照样计告警并解冻。
TEST_F(BattleFreezeHoldTest, QueryFailureHoldsOneTickWithinTheCap)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0);
    const uint64_t unknownBefore = CountOf(FreezeHoldEvent::kHeldUnknown);
    const uint64_t capBefore = CountOf(FreezeHoldEvent::kCapReached);

    gTerminalQuerySendable = false; // 没有就绪的 match 节点 / 没有场景密钥
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    EXPECT_TRUE(gTerminalQueries.empty());
    EXPECT_FALSE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId)) << "没发出去就不登记在途";
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "没问到不是结论:续冻一拍";
    EXPECT_TRUE(gLockReleases.empty());
    EXPECT_EQ(unknownBefore + 1, CountOf(FreezeHoldEvent::kHeldUnknown));

    gTerminalQuerySendable = true;
    const uint64_t nextTickMs = expiresAtMs + 1 + static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(nextTickMs);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kUnknown)) << "下一拍再问;传输失败 / 验签不过的回答同样是未知";
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_EQ(unknownBefore + 2, CountOf(FreezeHoldEvent::kHeldUnknown));

    gTerminalQuerySendable = false;
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeHoldDeadlineMs(kSyntheticDeadlineMs) + 1);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "未知也受同一个上限约束";
    EXPECT_EQ(capBefore + 1, CountOf(FreezeHoldEvent::kCapReached));
    ASSERT_EQ(1u, gLockReleases.size());
}

uint64_t HeldTotal()
{
    return CountOf(FreezeHoldEvent::kHeldComplete) + CountOf(FreezeHoldEvent::kHeldUnknown) +
           CountOf(FreezeHoldEvent::kHeldPending);
}

// 到期时 SQL 终局已 ABORT:照常解冻(记作废局、条件删锁、摘冻结),与改动前的到期收尾相同;不计续冻,也不计告警。
TEST_F(BattleFreezeHoldTest, AbortReleasesTheFreezeAsBefore)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t heldBefore = HeldTotal();
    const uint64_t capBefore = CountOf(FreezeHoldEvent::kCapReached);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort));

    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_EQ(kBattleId, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(kBattleId, gLockReleases.front().second);
    EXPECT_EQ(heldBefore, HeldTotal());
    EXPECT_EQ(capBefore, CountOf(FreezeHoldEvent::kCapReached));
}

// 有可裁决的 v1 原件而终局还没落定(NONE,match 回 terminal_expected = true):按「结果未知」续冻,不当场解冻。
// 冻结到期晚于 match 补写 ABORT 的时刻,到期还是 NONE 只说明 ABORT 还没写成,COMPLETE 仍可能先落库(2026-11-06 负责人定)。
// 下一拍终局落定为 ABORT 才解冻。
TEST_F(BattleFreezeHoldTest, PendingTerminalWithSqlOriginalHoldsLikeUnknown)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0);
    const uint64_t pendingBefore = CountOf(FreezeHoldEvent::kHeldPending);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kPending));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "NONE 不是终局证据:续冻";
    EXPECT_TRUE(gLockReleases.empty());
    EXPECT_EQ(0u, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player));
    EXPECT_EQ(pendingBefore + 1, CountOf(FreezeHoldEvent::kHeldPending));
    ASSERT_FALSE(gLockScripts.empty()) << "锁跟着续";
    EXPECT_EQ("renew_lock", gLockScripts.back().op);

    const uint64_t nextTickMs = expiresAtMs + 1 + static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(nextTickMs);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort)) << "match 补写的 ABORT 落定";
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    ASSERT_EQ(1u, gLockReleases.size());
}

// 没有可裁决 SQL 原件的局(没有成局行、终局协议 0 的旧局、battle 明确采用旧协议;match 回 NONE 且 terminal_expected = false):
// 这种局永远不会有 COMPLETE,到期照常解冻,不能被 NONE 拖满续冻上限。
TEST_F(BattleFreezeHoldTest, BattleWithoutSqlOriginalReleasesAtExpiry)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t heldBefore = HeldTotal();
    const uint64_t capBefore = CountOf(FreezeHoldEvent::kCapReached);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kNoOriginal));

    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "到期那一拍就放,不等上限";
    EXPECT_EQ(kBattleId, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(heldBefore, HeldTotal());
    EXPECT_EQ(capBefore, CountOf(FreezeHoldEvent::kCapReached));
}

// FIGHTING 锁从确认开局那一刻起就活到续冻上限之后(FreezeHoldDeadlineMs + 余量,2026-11-06 负责人定):
// 玩家在冻结到期之前下线、超过到期点才重登时锁还在,登录按锁重建冻结,到期处理照常查终局。
// 夹具没有 Redis,重登读锁这一步走不到;这里盯的是开局确认写下的锁 TTL 本身(重登读到的就是它)。
TEST_F(BattleFreezeHoldTest, FightingLockWrittenAtConfirmOutlivesTheHoldCapSoAReloginAfterExpiryCanRebuild)
{
    constexpr uint64_t kFarFutureDeadlineMs = 4'102'444'800'000ULL; // 2100 年:发送时刻离到期还很远
    auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player);
    inBattle.set_battle_id(kBattleId);
    inBattle.set_state(IN_BATTLE_STATE_PREPARING);
    inBattle.set_deadline_ms(kFarFutureDeadlineMs);
    inBattle.set_prepare_deadline_ms(kFarFutureDeadlineMs - 200'000);
    BattleConfirmedEvent event;
    event.set_player_id(playerId);
    event.set_battle_id(kBattleId);
    event.set_deadline_ms(kFarFutureDeadlineMs);

    const uint64_t sentNotBeforeMs = TimeSystem::NowMillisecondsUTC();
    PlayerBattleSystem::ConfirmBattle(event);
    const uint64_t sentNotAfterMs = TimeSystem::NowMillisecondsUTC();

    ASSERT_EQ(1u, gLockScripts.size());
    EXPECT_EQ("renew_lock", gLockScripts.front().op);
    const uint64_t ttlSec = gLockScripts.front().ttlSec;
    const uint64_t lockExpiresNotBeforeMs = sentNotBeforeMs + ttlSec * 1000;
    EXPECT_GT(lockExpiresNotBeforeMs, PlayerBattleSystem::FreezeHoldDeadlineMs(kFarFutureDeadlineMs))
        << "锁要活过续冻上限:到期之后才重登也读得到锁";
    EXPECT_GT(lockExpiresNotBeforeMs, PlayerBattleSystem::FreezeExpiresAtMs(false, kFarFutureDeadlineMs, 0) +
                                          PlayerBattleSystem::kLockExtraTtlSec * 1000ULL)
        << "比改动前(冻结到期 + 60s)更久";
    EXPECT_LE(ttlSec, (PlayerBattleSystem::FreezeHoldDeadlineMs(kFarFutureDeadlineMs) - sentNotBeforeMs) / 1000 +
                          PlayerBattleSystem::kLockExtraTtlSec)
        << "也只多到续冻上限 + 余量为止";
    EXPECT_GE(ttlSec, (PlayerBattleSystem::FreezeHoldDeadlineMs(kFarFutureDeadlineMs) - sentNotAfterMs) / 1000 +
                          PlayerBattleSystem::kLockExtraTtlSec);
}

// 续冻期间资产通道得到 RETRY(战斗闸只看 InBattleComp,不扣钱、不记账);终局判 ABORT 解冻之后同一 seq 重投照常扣款。
TEST_F(BattleFreezeHoldTest, AssetChannelGetsRetryWhileTheFreezeIsHeld)
{
    GiveAssetChannel();
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    Fight(kSyntheticDeadlineMs);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kComplete));
    ASSERT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "前置:续冻中";
    const auto blocked = GuildDebitGold(1, 30);
    EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, blocked.outcome());
    EXPECT_EQ(static_cast<uint32_t>(kAssetInBattle), blocked.reason().id());
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold)) << "续冻期间药 / 钱都挪不走";

    const uint64_t nextTickMs = expiresAtMs + 1 + static_cast<uint64_t>(PlayerBattleSystem::kReaperIntervalSec * 1000.0);
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(nextTickMs);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort));
    ASSERT_FALSE(PlayerBattleSystem::IsInBattle(player));
    const auto applied = GuildDebitGold(1, 30);
    EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, applied.outcome());
    EXPECT_EQ(70u, CurrencySystem::GetBalance(player, kCurrencyGold));
}

// 这一局的结算已在本实体应用过(收件箱战斗域最近结局环里有它):不必查,照常解冻。
// 典型场景:结算提交之后删锁没落地,玩家重登按锁重建了冻结 —— 不能再续冻到上限。
TEST_F(BattleFreezeHoldTest, SettlementAlreadyAppliedHereReleasesWithoutAQuery)
{
    auto& ledger = tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player);
    auto& cursor = player_inbox_rules::MutableCursor(
        ledger, static_cast<uint32_t>(player_inbox::PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT));
    PlayerInboxDecision decision;
    decision.set_seq(1);
    decision.set_outcome(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_OUTCOME_APPLIED));
    decision.set_request_id(kBattleId);
    decision.set_msg_type(static_cast<uint32_t>(player_inbox::PLAYER_INBOX_MSG_BATTLE_SETTLEMENT));
    ASSERT_TRUE(player_inbox_rules::RecordDecision(cursor, decision));
    Fight(kSyntheticDeadlineMs);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1);
    EXPECT_TRUE(gTerminalQueries.empty()) << "本地已应用:不查";
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player));
    ASSERT_EQ(1u, gLockReleases.size());
}

// 评审核实(2026-11-06):战斗结算域里环中的 REJECTED / SKIPPED 都不是终局,不能当「不会再有结算」提前解冻。
// 收件箱 Ack 对这个域只收 APPLIED(其余回 CONFLICT、行不变),VoidItem 对这个域一律拒绝,scene 的执行器从不回 kRejected,
// 回放到非 APPLIED 的原结局时状态机直接停用该域;唯一出口是修好之后让 scene 把原件执行成功,结算之后仍可能到达。
// 所以与没有结局一样:照旧查终局、续冻,到上限告警交人工。
TEST_F(BattleFreezeHoldTest, RejectedOrSkippedInTheBattleRingIsNotTerminalAndStillQueries)
{
    for (const auto outcome : {player_inbox::PLAYER_INBOX_OUTCOME_REJECTED, player_inbox::PLAYER_INBOX_OUTCOME_SKIPPED})
    {
        SCOPED_TRACE(static_cast<int>(outcome));
        tlsEcs.actorRegistry.remove<PlayerInboxLedgerComp>(player);
        gLockReleases.clear();
        gTerminalQueries.clear();
        ASSERT_NO_FATAL_FAILURE(RecordInboxDecision(1, outcome, kBattleId));
        Fight(kSyntheticDeadlineMs);
        const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0);
        const uint64_t heldBefore = CountOf(FreezeHoldEvent::kHeldComplete);

        PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
        ASSERT_EQ(1u, gTerminalQueries.size()) << "不是终局:照旧查";
        EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "回答到达之前冻结不动";
        ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kComplete));
        EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "COMPLETE 而本地没有 APPLIED:续冻";
        EXPECT_EQ(heldBefore + 1, CountOf(FreezeHoldEvent::kHeldComplete));
        EXPECT_TRUE(gLockReleases.empty());
        tlsEcs.actorRegistry.remove<InBattleComp>(player); // 下一轮从干净的冻结开始
    }
}

// 只认这一局的战斗结算 APPLIED:别的局的 APPLIED、同一 request_id 的别的收件箱类型都不算这一局已收尾,照旧查终局续冻。
TEST_F(BattleFreezeHoldTest, InboxDecisionsOfOtherBattlesOrTypesStillQuery)
{
    ASSERT_NO_FATAL_FAILURE(RecordInboxDecision(1, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, kBattleId + 1));
    ASSERT_NO_FATAL_FAILURE(RecordInboxDecision(2, player_inbox::PLAYER_INBOX_OUTCOME_APPLIED, kBattleId,
                                                player_inbox::PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN));
    Fight(kSyntheticDeadlineMs);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1);
    ASSERT_EQ(1u, gTerminalQueries.size()) << "不是这一局的结算终局:照旧查";
    EXPECT_EQ(kBattleId, gTerminalQueries.front().battleId);
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player));
    EXPECT_TRUE(gLockReleases.empty());
}

// 查询在途期间结算确认提交、冻结已摘(或玩家已在下一局):迟到的回答什么都不做。
TEST_F(BattleFreezeHoldTest, AnswerArrivingAfterTheFreezeWasClearedDoesNothing)
{
    Fight(kSyntheticDeadlineMs);
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1);
    ASSERT_EQ(1u, gTerminalQueries.size());
    tlsEcs.actorRegistry.remove<InBattleComp>(player); // CompleteInboxSettlement 摘掉了同一局的冻结

    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort));
    EXPECT_TRUE(gLockReleases.empty()) << "没有要处置的冻结,也不替结算路径删锁";
    EXPECT_EQ(0u, PlayerBattleFreezeHoldTestAccess::ExpiredFightingBattleId(player));
}

// 一次丢失的回调(按单调钟超过一拍加一次调用期限仍没回)不能把玩家永远冻住:按「未知」处置并计数,迟到的旧回答照样能放人。
TEST_F(BattleFreezeHoldTest, LostQueryIsTreatedAsUnknownAndALateAnswerStillCounts)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t sentAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1;
    const uint64_t lostAfterMs = kReaperTickMs + PlayerBattleSystem::kTerminalQueryDeadlineMs;
    const uint64_t lostBefore = CountOf(FreezeHoldEvent::kQueryLost);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs);
    ASSERT_EQ(1u, gTerminalQueries.size());
    gSteadyNowMs += lostAfterMs;
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs + kReaperTickMs);
    EXPECT_EQ(lostBefore, CountOf(FreezeHoldEvent::kQueryLost)) << "还在一拍加一次期限之内:继续等";
    EXPECT_TRUE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));

    gSteadyNowMs += 1;
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs + 2 * kReaperTickMs);
    EXPECT_EQ(lostBefore + 1, CountOf(FreezeHoldEvent::kQueryLost));
    EXPECT_FALSE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "丢失按未知:续冻一拍";
    EXPECT_EQ(1u, gTerminalQueries.size()) << "这一拍不另发";

    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort)) << "旧查询的回答迟到";
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "迟到的 ABORT 照样放人";
}

// 评审修正(2026-11-06):在途多久按单调钟量,墙钟只给跨进程的截止时刻用。墙钟前跳不会把正常在途的查询误判丢失,
// 墙钟回拨也不会让丢失的查询永远不判。修复前两样都按墙钟差判:前跳当场判丢失,回拨一直等。
TEST_F(BattleFreezeHoldTest, LostQueryIsMeasuredOnTheMonotonicClockNotTheWallClock)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t sentAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0) + 1;
    const uint64_t lostAfterMs = kReaperTickMs + PlayerBattleSystem::kTerminalQueryDeadlineMs;
    const uint64_t lostBefore = CountOf(FreezeHoldEvent::kQueryLost);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs);
    ASSERT_EQ(1u, gTerminalQueries.size());

    gSteadyNowMs += 1'000;                                                          // 单调钟只过了 1 秒
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs + 10 * 60 * 1000); // 墙钟前跳 10 分钟(仍在上限之内)
    EXPECT_EQ(lostBefore, CountOf(FreezeHoldEvent::kQueryLost)) << "墙钟前跳不算在途时长";
    EXPECT_TRUE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));
    EXPECT_EQ(1u, gTerminalQueries.size()) << "在途时不另发";

    gSteadyNowMs += lostAfterMs;                              // 单调钟已过一拍加一次期限
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(sentAtMs); // 墙钟回拨到发出那一刻
    EXPECT_EQ(lostBefore + 1, CountOf(FreezeHoldEvent::kQueryLost)) << "墙钟回拨也照样判丢失";
    EXPECT_FALSE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "丢失按未知:上限之内续冻";
}

// 评审修正(2026-11-06):越过续冻上限的那一拍直接解冻,不等在途的查询。任何路径(含查询丢失)的解冻都不晚于越过上限后的
// 第一拍,而锁按 FreezeHoldDeadlineMs + kLockExtraTtlSec 写:解冻时锁一定还在,match 不会先放下一场。
TEST_F(BattleFreezeHoldTest, PastTheCapReleasesOnTheFirstTickWithoutWaitingForAQueryInFlight)
{
    Fight(kSyntheticDeadlineMs);
    const uint64_t holdDeadlineMs = PlayerBattleSystem::FreezeHoldDeadlineMs(kSyntheticDeadlineMs);
    const uint64_t capBefore = CountOf(FreezeHoldEvent::kCapReached);

    const uint64_t lastTickBeforeCapMs = holdDeadlineMs - 1'000;
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(lastTickBeforeCapMs); // 上限前最后一拍发出查询,一直没回
    ASSERT_EQ(1u, gTerminalQueries.size());

    gSteadyNowMs += kReaperTickMs; // 只过了一拍:还没到判丢失
    const uint64_t firstTickPastCapMs = lastTickBeforeCapMs + kReaperTickMs;
    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(firstTickPastCapMs);
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "越过上限的第一拍就解冻";
    EXPECT_FALSE(PlayerBattleFreezeHoldTestAccess::IsTerminalQueryInFlight(playerId, kBattleId)) << "在途登记一并摘掉";
    EXPECT_EQ(capBefore + 1, CountOf(FreezeHoldEvent::kCapReached));
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(1u, gTerminalQueries.size()) << "越过上限不再发查询";
    EXPECT_LT(firstTickPastCapMs, holdDeadlineMs + PlayerBattleSystem::kLockExtraTtlSec * 1000ULL - 1'000)
        << "解冻早于锁过期(EX 秒级下取整最多早到 1 秒)";

    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kComplete)) << "迟到的回答";
    EXPECT_EQ(1u, gLockReleases.size()) << "冻结已不在:什么都不做";
    EXPECT_EQ(capBefore + 1, CountOf(FreezeHoldEvent::kCapReached));
}

// 评审修正(2026-11-06):状态缺失(NONE)的冻结,reaper 与锁 TTL(LockTtlSecOf)都按 FIGHTING 处理,回答也必须照 FIGHTING 处置。
// 修复前回答只认 FIGHTING:每次回答都被丢掉,不计续冻,ABORT 也放不了人,只能拖到续冻上限。
TEST_F(BattleFreezeHoldTest, StatelessFreezeIsResolvedLikeFightingAndStaysBounded)
{
    Fight(kSyntheticDeadlineMs).set_state(IN_BATTLE_STATE_NONE);
    const uint64_t expiresAtMs = PlayerBattleSystem::FreezeExpiresAtMs(false, kSyntheticDeadlineMs, 0);
    const uint64_t unknownBefore = CountOf(FreezeHoldEvent::kHeldUnknown);

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kUnknown));
    EXPECT_TRUE(PlayerBattleSystem::IsInBattle(player)) << "上限之内与 FIGHTING 一样续冻";
    EXPECT_EQ(unknownBefore + 1, CountOf(FreezeHoldEvent::kHeldUnknown)) << "回答照 FIGHTING 处置,不是被丢掉";
    EXPECT_TRUE(gLockReleases.empty());

    PlayerBattleFreezeHoldTestAccess::ReapExpiredFreezes(expiresAtMs + 1 + kReaperTickMs);
    ASSERT_TRUE(AnswerOldestQuery(TerminalAnswer::kAbort));
    EXPECT_FALSE(PlayerBattleSystem::IsInBattle(player)) << "ABORT 当场解冻,不被拖到续冻上限";
    ASSERT_EQ(1u, gLockReleases.size());
    EXPECT_EQ(kBattleId, gLockReleases.front().second);
}

// ── 生产传输的回包归一化(battle_terminal_query::ClassifyReply)────────────────────────────────

constexpr std::string_view kSceneSecret = "fixture-only-scene-terminal-secret-0000";

struct SignedTerminalExchange
{
    std::string digest;
    match::GetBattleTerminalDecisionResponse reply;
};

SignedTerminalExchange MakeSignedReply(match::BattleOutcomeDecision decision, bool retry, uint64_t stampMs,
                                       std::string_view signer = battle_terminal_query::kMatchCaller,
                                       std::string_view secret = kSceneSecret, bool terminalExpected = true)
{
    match::GetBattleTerminalDecisionRequest request;
    request.set_battle_id(kBattleId);
    EXPECT_TRUE(service_call_auth::Sign(battle_terminal_query::kMethod, request, *request.mutable_auth(),
                                        battle_terminal_query::kSceneCaller, kSceneSecret, stampMs));
    SignedTerminalExchange exchange;
    exchange.digest = service_call_auth::Digest(request);
    exchange.reply.set_decision(decision);
    exchange.reply.set_retry(retry);
    exchange.reply.set_terminal_expected(terminalExpected);
    exchange.reply.set_request_digest(exchange.digest);
    EXPECT_TRUE(service_call_auth::Sign(std::string(battle_terminal_query::kMethod) + "/reply", exchange.reply,
                                        *exchange.reply.mutable_auth(), signer, secret, stampMs));
    return exchange;
}

// 只有 match 以方法 + "/reply" 签、request_digest 绑定本次请求、且不是 retry 的回包才算结论;其余一律 UNKNOWN。
TEST(BattleTerminalQueryReply, ClassifiesOnlySignedRepliesBoundToTheRequest)
{
    constexpr uint64_t kNowMs = 1'790'000'000'000ULL;
    const auto complete = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_COMPLETE, false, kNowMs);
    EXPECT_EQ(TerminalAnswer::kComplete,
              battle_terminal_query::ClassifyReply(true, &complete.reply, complete.digest, kSceneSecret, kNowMs));
    const auto abort = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_ABORT, false, kNowMs);
    EXPECT_EQ(TerminalAnswer::kAbort, battle_terminal_query::ClassifyReply(true, &abort.reply, abort.digest, kSceneSecret, kNowMs));
    const auto pending = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_UNKNOWN, false, kNowMs);
    EXPECT_EQ(TerminalAnswer::kPending, battle_terminal_query::ClassifyReply(true, &pending.reply, pending.digest, kSceneSecret, kNowMs))
        << "NONE 而有可裁决原件:终局还没落定,不是终局证据";
    const auto noOriginal = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_UNKNOWN, false, kNowMs,
                                            battle_terminal_query::kMatchCaller, kSceneSecret, /*terminalExpected=*/false);
    EXPECT_EQ(TerminalAnswer::kNoOriginal,
              battle_terminal_query::ClassifyReply(true, &noOriginal.reply, noOriginal.digest, kSceneSecret, kNowMs))
        << "NONE 且没有可裁决原件:永远不会有结算,照常解冻";
    auto forgedPending = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_UNKNOWN, false, kNowMs,
                                         battle_terminal_query::kMatchCaller, kSceneSecret, /*terminalExpected=*/false);
    forgedPending.reply.set_terminal_expected(true); // 签名之后改「终局会不会落定」
    EXPECT_EQ(TerminalAnswer::kUnknown,
              battle_terminal_query::ClassifyReply(true, &forgedPending.reply, forgedPending.digest, kSceneSecret, kNowMs))
        << "terminal_expected 同样在签名覆盖之内";

    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(false, &complete.reply, complete.digest, kSceneSecret, kNowMs))
        << "gRPC 非 OK";
    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(true, nullptr, complete.digest, kSceneSecret, kNowMs));
    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(true, &complete.reply, abort.digest + "x", kSceneSecret, kNowMs))
        << "回包绑定的不是这一次请求";
    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(true, &complete.reply, complete.digest, "", kNowMs))
        << "没配场景密钥";
    EXPECT_EQ(TerminalAnswer::kUnknown,
              battle_terminal_query::ClassifyReply(true, &complete.reply, complete.digest, kSceneSecret, kNowMs + 300'001))
        << "签名时间窗之外";

    const auto wrongSigner = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_COMPLETE, false, kNowMs, battle_terminal_query::kSceneCaller);
    EXPECT_EQ(TerminalAnswer::kUnknown,
              battle_terminal_query::ClassifyReply(true, &wrongSigner.reply, wrongSigner.digest, kSceneSecret, kNowMs))
        << "回包必须是 match 签的,scene 自己签的不算";
    const auto wrongSecret = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_COMPLETE, false, kNowMs,
                                             battle_terminal_query::kMatchCaller, "another-secret-of-at-least-32-bytes!!");
    EXPECT_EQ(TerminalAnswer::kUnknown,
              battle_terminal_query::ClassifyReply(true, &wrongSecret.reply, wrongSecret.digest, kSceneSecret, kNowMs));

    auto tampered = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_ABORT, false, kNowMs);
    tampered.reply.set_decision(match::BATTLE_OUTCOME_DECISION_COMPLETE); // 签名之后改决定
    EXPECT_EQ(TerminalAnswer::kUnknown,
              battle_terminal_query::ClassifyReply(true, &tampered.reply, tampered.digest, kSceneSecret, kNowMs));

    const auto retry = MakeSignedReply(match::BATTLE_OUTCOME_DECISION_UNKNOWN, true, kNowMs);
    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(true, &retry.reply, retry.digest, kSceneSecret, kNowMs))
        << "match 带内 retry:这一次没读成 SQL,不是「没有终局」";
    const auto future = MakeSignedReply(static_cast<match::BattleOutcomeDecision>(9), false, kNowMs);
    EXPECT_EQ(TerminalAnswer::kUnknown, battle_terminal_query::ClassifyReply(true, &future.reply, future.digest, kSceneSecret, kNowMs))
        << "不认识的决定不替它猜";
}

// ── 资产通道整批预检的号段续段(r2-battle#3)────────────────────────────────────────────────

constexpr uint32_t kTwoPerStackItem = 9;   // max_stack_size = 2,非装备
constexpr uint32_t kStackableItem = 10;    // max_stack_size = 999

int gItemSegmentFetches = 0;

// item 号段换成用例自己的小区间:领段请求只计数,不自动回包;Warm 发出的首段请求随即落下 [lo, hi)。
bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
    auto& client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
    client.Reset();
    GuidSegmentClient::Options options;
    options.mixIds = false;
    options.kindName = "item";
    options.initialStep = 100;
    gItemSegmentFetches = 0;
    if (!client.Enable(options, [](const std::string&, uint32_t) { ++gItemSegmentFetches; return true; },
                       [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; }))
    {
        return false;
    }
    client.Warm();
    client.OnResponse(0, lo, hi);
    return client.IsReady();
}

class AssetOpCreditGuidTopUpTest : public BattleFreezeHoldTest
{
protected:
    void SetUp() override
    {
        BattleFreezeHoldTest::SetUp();
        // 进程内前序用例(GainBlockServiceTest 等)可能留有全局物品封禁:入账会先判封禁、回终局 REJECTED,
        // 本组用例要的 RETRY / APPLIED 就成了执行顺序的函数。先清掉,再把前提钉成断言(评审修正 2026-11-06)。
        GainBlockService::ClearAllGlobalBlocks();
        ItemTableManager::Instance().Load();
        GiveAssetChannel();
        auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
        for (auto& bag : bags.bags) bag.SetPlayerGuid(Guid{playerId});
        bags.bags[kInventory].SetCapacityForRestore(20);
        ASSERT_FALSE(GainBlockService::IsItemGainBlocked(player, kTwoPerStackItem)) << "前提:没有残留的物品封禁";
        ASSERT_FALSE(GainBlockService::IsItemGainBlocked(player, kStackableItem)) << "前提:没有残留的物品封禁";
    }
    void TearDown() override
    {
        // 还原 bag_test 进程里其它用例依赖的号段基线(与 player_battle_settlement_test 同一基线)。
        EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
        BattleFreezeHoldTest::TearDown();
    }
    ::AssetOpResponse CreditItems(uint64_t seq, uint32_t configId, uint32_t count)
    {
        ::AssetOpRequest request;
        request.set_player_id(playerId);
        request.set_stream(ASSET_OP_STREAM_GUILD_CREDIT);
        request.set_seq(seq);
        request.set_correlation_id(78);
        request.set_tx_type(static_cast<uint32_t>(TX_GUILD_SHOP));
        request.set_stream_epoch(100);
        auto* item = request.mutable_bundle()->add_items();
        item->set_config_id(configId);
        item->set_count(count);
        auto* auth = request.mutable_auth();
        auth->set_caller("guild");
        auth->set_timestamp_ms(static_cast<uint64_t>(gAssetOpNowMs));
        auth->set_signature_hex(token_security::HmacSha256Hex(kHoldAssetOpSecret, AssetOpCanonical("credit", request)));
        ::AssetOpResponse response;
        PlayerAssetOpSystem::Credit(request, response);
        return response;
    }
    Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
};

// 需要量按新实例数算(BagService::MaxNewInstancesFor),不按件数:50 件可叠加物只铸 1 个号,号段只剩 5 个也照常入账。
// 改动前按 Σcount = 50 判,永远 RETRY。
TEST_F(AssetOpCreditGuidTopUpTest, GuidPrecheckCountsNewInstancesNotUnits)
{
    ASSERT_TRUE(ArmItemSegment(1, 6)); // 只剩 5 个号
    ASSERT_EQ(1u, BagService::MaxNewInstancesFor(ItemCountMap{{kStackableItem, 50}}));

    const auto response = CreditItems(1, kStackableItem, 50);
    EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
    EXPECT_EQ(50u, Inventory().GetTotalItemCount(kStackableItem));
}

// 「需要量 > 剩余 > 预取阈值、又没有下一段」:回 RETRY、不记账,同时请号段客户端续一段;新段落下之后同一 seq 重投照常入账。
// 改动前预检只读余量,预取只在发号之后才判阈值,这一笔永远等不来新段。
TEST_F(AssetOpCreditGuidTopUpTest, ShortageAboveThePrefetchThresholdRequestsATopUpThenApplies)
{
    ASSERT_TRUE(ArmItemSegment(1, 4)); // 剩 3 个号,预取阈值 ceil(0.1 × 3) = 1:剩余在阈值之上,不会自己预取
    ASSERT_EQ(1, gItemSegmentFetches) << "前置:只有 Warm 的那一次领段";
    ASSERT_EQ(4u, BagService::MaxNewInstancesFor(ItemCountMap{{kTwoPerStackItem, 8}}));

    const auto deferred = CreditItems(1, kTwoPerStackItem, 8);
    EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, deferred.outcome());
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kTwoPerStackItem));
    EXPECT_EQ(2, gItemSegmentFetches) << "预检判出不足就续段";
    EXPECT_EQ(1u, tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().topUpFetches);

    const auto stillDeferred = CreditItems(1, kTwoPerStackItem, 8);
    EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, stillDeferred.outcome());
    EXPECT_EQ(2, gItemSegmentFetches) << "单飞:在途时重投不再多发";

    tlsGuidSegmentRegistry.Get(GuidKind::kItem).OnResponse(0, 100, 200); // 续的那一段落下
    const auto applied = CreditItems(1, kTwoPerStackItem, 8);
    EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, applied.outcome());
    EXPECT_EQ(8u, Inventory().GetTotalItemCount(kTwoPerStackItem));
}

// 评审修正(2026-11-06):要的新实例比号段一段最多能给的(MaxStep)还多:续段永远凑不齐,回 RETRY 就是让 Go 永远重投。
// 按资产通道「确定性失败记终局拒绝」的口径记 REJECTED + kAssetInvalidBundle:一件不发、一个号都不领;
// 结局随账本固定,同一 seq 重投只读回放同一结局,Go 按 REJECTED 终结并做对侧账。
TEST_F(AssetOpCreditGuidTopUpTest, BatchNeedingMoreThanTheMaxStepIsRejectedAsAnInvalidBundle)
{
    ASSERT_TRUE(ArmItemSegment(1, 4)); // 剩 3 个号;step = MaxStep = 100
    ASSERT_EQ(1, gItemSegmentFetches);
    ASSERT_EQ(101u, BagService::MaxNewInstancesFor(ItemCountMap{{kTwoPerStackItem, 202}}));

    const auto rejected = CreditItems(1, kTwoPerStackItem, 202);
    EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, rejected.outcome());
    EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), rejected.reason().id());
    EXPECT_EQ(0u, Inventory().GetTotalItemCount(kTwoPerStackItem));
    EXPECT_EQ(1, gItemSegmentFetches) << "终局结论不领段";
    EXPECT_FALSE(tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().topUpPending) << "也不记续段需求";

    const auto replayed = CreditItems(1, kTwoPerStackItem, 202);
    EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, replayed.outcome()) << "结局已记账:同一 seq 只读回放";
    EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), replayed.reason().id());
    EXPECT_EQ(1, gItemSegmentFetches);
}

} // namespace
