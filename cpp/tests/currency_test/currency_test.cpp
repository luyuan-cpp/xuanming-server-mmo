#include <gtest/gtest.h>

#include <cstdlib>
#include <cstring>
#include <limits>
#include <memory>
#include <string>

#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"

#include "infra/storage/redis_client/redis_client.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/system/currency_system.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "player/comp/player_frozen_comp.h" // 冻结用例:IsCrossZoneFrozen 就是 any_of<PlayerFrozenComp>
#include "proto/common/component/currency_comp.pb.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetFrozen / kAssetBlocked / kAssetCurrencyInsufficient
#include "table/proto/tip/common_error_tip.pb.h"
#include <thread_context/ecs_context.h>
#include "modules/id_segment/guid_segment_registry.h"

template <class MessageKey, class MessageValue>
struct MessageAsyncClientTestPeer
{
    using Client = MessageAsyncClient<MessageKey, MessageValue>;
    using Element = typename Client::Element;
    using ElementPtr = typename Client::ElementPtr;

    static ElementPtr TrackLoad(Client& client, const MessageKey& key)
    {
        auto element = std::make_shared<Element>();
        element->message_key = key;
        element->redis_key = client.full_name() + ":" + std::to_string(key);
        client.loading_queue_[element->redis_key] = element;
        return element;
    }

    static void Deliver(Client& client, redisReply* reply, const ElementPtr& element)
    {
        client.OnLoaded(nullptr, reply, element);
    }

    // 模拟一条带 guard 的存盘已经发出、正在等回包(不连 Redis,直接登记进 saving_queue_)。
    static ElementPtr TrackGuardedSave(Client& client, const MessageKey& key,
                                       const std::string& guardKey, const std::string& guardExpected)
    {
        auto element = std::make_shared<Element>();
        element->message_key = key;
        element->redis_key = client.full_name() + ":" + std::to_string(key);
        element->message_value = std::make_shared<MessageValue>();
        element->guard_key = guardKey;
        element->guard_expected = guardExpected;
        client.saving_queue_[element->redis_key] = element;
        return element;
    }

    static void DeliverSaved(Client& client, redisReply* reply, const ElementPtr& element)
    {
        client.OnSaved(nullptr, reply, element);
    }

    // 模拟一条普通(无 guard)存盘已经发出、正在等回包;载荷由调用方给,用来区分新旧值。
    static ElementPtr TrackSave(Client& client, const MessageKey& key, const std::shared_ptr<MessageValue>& value)
    {
        auto element = std::make_shared<Element>();
        element->message_key = key;
        element->redis_key = client.full_name() + ":" + std::to_string(key);
        element->message_value = value;
        client.saving_queue_[element->redis_key] = element;
        return element;
    }

    // 该 key 当前排队中的那份;没有则返回 nullptr。
    static ElementPtr PendingSave(Client& client, const MessageKey& key)
    {
        const auto it = client.pending_save_queue_.find(client.full_name() + ":" + std::to_string(key));
        return it == client.pending_save_queue_.end() ? nullptr : it->second;
    }

    // 模拟定时器到期把排队值发出:挪进在途,不碰 hiredis(没有连接时 IssueSave 只会把它放回排队)。
    // 调用方要保证此刻没有在途 —— 与 IssueSave 的前提相同。
    static ElementPtr IssuePendingForTest(Client& client, const MessageKey& key)
    {
        const std::string redisKey = client.full_name() + ":" + std::to_string(key);
        const auto it = client.pending_save_queue_.find(redisKey);
        if (it == client.pending_save_queue_.end() || client.saving_queue_.count(redisKey) != 0)
        {
            return nullptr;
        }
        auto element = it->second;
        client.pending_save_queue_.erase(it);
        client.saving_queue_[redisKey] = element;
        return element;
    }
};

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

entt::entity CreateTestPlayer()
{
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);
    tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    return player;
}

void DestroyTestPlayer(entt::entity player)
{
    tlsEcs.actorRegistry.destroy(player);
}

// ---------------------------------------------------------------------------
// AddCurrency
// ---------------------------------------------------------------------------

TEST(CurrencyTest, AddCurrencyBasic)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));

    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 50));
    EXPECT_EQ(150u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AddCurrencyInvalidAmount)
{
    auto player = CreateTestPlayer();

    EXPECT_NE(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 0));
    EXPECT_NE(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, -1));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AddCurrencyMultipleTypes)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, 50));
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyBindDiamond, 30));

    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(50u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(30u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// DeductCurrency
// ---------------------------------------------------------------------------

TEST(CurrencyTest, DeductCurrencyBasic)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 200);
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 80));
    EXPECT_EQ(120u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DeductCurrencyInsufficientFunds)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 50);
    EXPECT_NE(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 100));
    // Balance stays unchanged after failed deduction.
    EXPECT_EQ(50u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DeductCurrencyInvalidAmount)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 100);
    EXPECT_NE(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 0));
    EXPECT_NE(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, -5));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DeductCurrencyExact)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyDiamond, 77);
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyDiamond, 77));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// CanAfford
// ---------------------------------------------------------------------------

TEST(CurrencyTest, CanAfford)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 100);
    EXPECT_TRUE(CurrencySystem::CanAfford(player, kCurrencyGold, 50));
    EXPECT_TRUE(CurrencySystem::CanAfford(player, kCurrencyGold, 100));
    EXPECT_FALSE(CurrencySystem::CanAfford(player, kCurrencyGold, 101));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// AttachDebt (补缴)
// ---------------------------------------------------------------------------

TEST(CurrencyTest, DebtDeductsFromGain)
{
    auto player = CreateTestPlayer();

    // Attach a debt of 60 gold.
    CurrencySystem::AttachDebt(player, kCurrencyGold, 60);

    // Add 100 gold — 60 goes to debt, 40 is credited.
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(40u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DebtPartialRepayment)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AttachDebt(player, kCurrencyGold, 100);

    // First gain: 30 gold — all goes to debt.
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 30));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    // Second gain: 50 gold — all goes to debt (remaining debt was 70).
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 50));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    // Third gain: 30 gold — 20 goes to debt, 10 credited.
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 30));
    EXPECT_EQ(10u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DebtDoesNotAffectOtherTypes)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AttachDebt(player, kCurrencyGold, 100);

    // Diamond has no debt — full credit.
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, 50));
    EXPECT_EQ(50u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AdjustDebtClampsOverDecreaseAndInt64Min)
{
    auto player = CreateTestPlayer();
    auto &comp = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player);
    auto &debt = comp.debts[static_cast<uint32_t>(kCurrencyGold)];
    debt.owed = 100;
    debt.paid = 40;

    CurrencySystem::AdjustDebt(player, kCurrencyGold, -200);
    EXPECT_EQ(40u, debt.owed);
    EXPECT_EQ(40u, debt.paid);

    debt.owed = 100;
    CurrencySystem::AdjustDebt(player, kCurrencyGold, std::numeric_limits<int64_t>::min());
    EXPECT_EQ(40u, debt.owed);

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DebtIncreaseRejectsUint64Overflow)
{
    auto player = CreateTestPlayer();
    auto &comp = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player);
    auto &debt = comp.debts[static_cast<uint32_t>(kCurrencyGold)];
    debt.owed = std::numeric_limits<uint64_t>::max() - 5;

    CurrencySystem::AttachDebt(player, kCurrencyGold, 10);
    EXPECT_EQ(std::numeric_limits<uint64_t>::max() - 5, debt.owed);

    CurrencySystem::AdjustDebt(player, kCurrencyGold, 10);
    EXPECT_EQ(std::numeric_limits<uint64_t>::max() - 5, debt.owed);

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DebtPersistenceRoundTrip)
{
    PlayerCurrencyComp source;
    CurrencyDebt debt;
    debt.owed = 500;
    debt.paid = 123;
    debt.frozen = true;
    debt.expiresAt = 9999;
    debt.reason = "audit";
    debt.gmOperator = "gm";
    debt.createdAt = 777;
    source.debts[static_cast<uint32_t>(kCurrencyGold)] = debt;

    CurrencyComp proto;
    source.SaveToProto(proto);

    PlayerCurrencyComp restored;
    restored.LoadFromProto(proto);
    const auto it = restored.debts.find(static_cast<uint32_t>(kCurrencyGold));
    ASSERT_NE(restored.debts.end(), it);
    EXPECT_EQ(debt.owed, it->second.owed);
    EXPECT_EQ(debt.paid, it->second.paid);
    EXPECT_EQ(debt.frozen, it->second.frozen);
    EXPECT_EQ(debt.expiresAt, it->second.expiresAt);
    EXPECT_EQ(debt.reason, it->second.reason);
    EXPECT_EQ(debt.gmOperator, it->second.gmOperator);
    EXPECT_EQ(debt.createdAt, it->second.createdAt);
}

TEST(CurrencyTest, AnomalyBucketsSeparateCurrencyAndItemAndClearInConstantTime)
{
    AnomalyDetector::ClearAll();
    const uint32_t sharedNumericKey = static_cast<uint32_t>(kCurrencyGold);

    AnomalyThreshold currencyThreshold;
    currencyThreshold.maxCountPerWindow = 100;
    currencyThreshold.maxAmountPerWindow = std::numeric_limits<uint32_t>::max();
    currencyThreshold.windowSeconds = 600;
    AnomalyDetector::SetCurrencyThreshold(kCurrencyGold, currencyThreshold);

    AnomalyThreshold itemThreshold = currencyThreshold;
    itemThreshold.maxCountPerWindow = 1;
    AnomalyDetector::SetItemThreshold(sharedNumericKey, itemThreshold);

    auto player = CreateTestPlayer();
    EXPECT_FALSE(AnomalyDetector::RecordCurrencyGain(player, kCurrencyGold, 1));
    // A numeric collision with CurrencyType must not make this first item event
    // look like the second event in the same bucket.
    EXPECT_FALSE(AnomalyDetector::RecordItemGain(player, sharedNumericKey, 1));
    EXPECT_EQ(1u, AnomalyDetector::GetCurrencyGainCount(player, kCurrencyGold));

    AnomalyDetector::ClearPlayer(player);
    EXPECT_EQ(0u, AnomalyDetector::GetCurrencyGainCount(player, kCurrencyGold));

    DestroyTestPlayer(player);
    AnomalyDetector::ClearAll();
}

// ---------------------------------------------------------------------------
// MessageAsyncClient 损坏载荷必须 fail-closed
// ---------------------------------------------------------------------------

TEST(CurrencyTest, RedisLoadCorruptPayloadFailsClosed)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int successCount = 0;
    int failureCount = 0;
    Guid failedKey = 0;
    auto failureReason = Client::LoadFailureReason::DataNotFound;
    client.SetLoadCallback([&](Guid, CurrencyComp&) { ++successCount; });
    client.SetLoadFailedCallback([&](Guid key, Client::LoadFailureReason reason) {
        ++failureCount;
        failedKey = key;
        failureReason = reason;
        EXPECT_EQ(0u, client.in_flight_load_count());
        EXPECT_EQ(0u, client.pending_load_count());
    });

    constexpr Guid kPlayerId = 10001;
    const auto element = Peer::TrackLoad(client, kPlayerId);
    char corruptPayload[] = {static_cast<char>(0x80)}; // 未结束的 protobuf varint。
    redisReply reply{};
    reply.type = REDIS_REPLY_STRING;
    reply.str = corruptPayload;
    reply.len = sizeof(corruptPayload);

    ASSERT_EQ(1u, client.in_flight_load_count());
    Peer::Deliver(client, &reply, element);

    EXPECT_EQ(0, successCount);
    EXPECT_EQ(1, failureCount);
    EXPECT_EQ(kPlayerId, failedKey);
    EXPECT_EQ(Client::LoadFailureReason::RedisError, failureReason);
    EXPECT_EQ(0u, client.in_flight_load_count());
    EXPECT_EQ(0u, client.pending_load_count());
    EXPECT_FALSE(element->message_value);
}

TEST(CurrencyTest, RedisLoadOversizePayloadFailsClosed)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int successCount = 0;
    int failureCount = 0;
    Guid failedKey = 0;
    auto failureReason = Client::LoadFailureReason::DataNotFound;
    client.SetLoadCallback([&](Guid, CurrencyComp&) { ++successCount; });
    client.SetLoadFailedCallback([&](Guid key, Client::LoadFailureReason reason) {
        ++failureCount;
        failedKey = key;
        failureReason = reason;
        EXPECT_EQ(0u, client.in_flight_load_count());
        EXPECT_EQ(0u, client.pending_load_count());
    });

    constexpr Guid kPlayerId = 10002;
    const auto element = Peer::TrackLoad(client, kPlayerId);
    char sentinel = 0;
    redisReply reply{};
    reply.type = REDIS_REPLY_STRING;
    reply.str = &sentinel;
    reply.len = static_cast<size_t>(std::numeric_limits<int>::max()) + 1u;

    ASSERT_EQ(1u, client.in_flight_load_count());
    Peer::Deliver(client, &reply, element);

    EXPECT_EQ(0, successCount);
    EXPECT_EQ(1, failureCount);
    EXPECT_EQ(kPlayerId, failedKey);
    EXPECT_EQ(Client::LoadFailureReason::RedisError, failureReason);
    EXPECT_EQ(0u, client.in_flight_load_count());
    EXPECT_EQ(0u, client.pending_load_count());
    EXPECT_FALSE(element->message_value);
}

// ---------------------------------------------------------------------------
// MessageAsyncClient 带归属校验的存盘(reentry-barrier §6.2 方案 a)
// ---------------------------------------------------------------------------

// guard 不等 → Lua 返回 0:是终态"这份期望值已作废",不是故障。必须只回 rejected 回调,
// 不回成功、不回失败、不重试,且同 key 排队中**期望值相同**的更新值也一并丢弃。
TEST(CurrencyTest, RedisGuardedSaveRejectedIsTerminalAndDropsPending)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    int failedCount = 0;
    int rejectedCount = 0;
    Guid rejectedKey = 0;
    std::string rejectedRedisKey;
    std::string rejectedGuard;
    client.SetSaveCallback([&](Guid, CurrencyComp&) { ++savedCount; });
    client.SetSaveFailedCallback([&](Guid, const std::string&, int) { ++failedCount; });
    client.SetSaveRejectedCallback([&](Guid key, const std::string& redisKey, const std::string& guardExpected) {
        ++rejectedCount;
        rejectedKey = key;
        rejectedRedisKey = redisKey;
        rejectedGuard = guardExpected;
        // 回调时队列必须已经清干净:调用方会在回调里销毁实体,之后不能再有任何
        // 针对该 key 的写入冒出来。
        EXPECT_EQ(0u, client.in_flight_save_count());
        EXPECT_EQ(0u, client.pending_save_count());
    });

    constexpr Guid kPlayerId = 10003;
    const std::string guardKey = "player:10003:owner_epoch";
    const auto inFlight = Peer::TrackGuardedSave(client, kPlayerId, guardKey, "7");
    // 同 key 的后续存盘在前一条回包前到达,按既有规则排进 pending(不碰 hiredis)。
    client.Save(std::make_shared<CurrencyComp>(), kPlayerId, guardKey, "7");
    ASSERT_EQ(1u, client.in_flight_save_count());
    ASSERT_EQ(1u, client.pending_save_count());

    redisReply reply{};
    reply.type = REDIS_REPLY_INTEGER;
    reply.integer = 0;
    Peer::DeliverSaved(client, &reply, inFlight);

    EXPECT_EQ(0, savedCount);
    EXPECT_EQ(0, failedCount);
    EXPECT_EQ(1, rejectedCount);
    EXPECT_EQ(kPlayerId, rejectedKey);
    EXPECT_EQ(inFlight->redis_key, rejectedRedisKey);
    // 调用方要拿"被拒的那一份期望值"与实体当前缓存的 epoch 比,才能区分
    // "自己被废黜"与"只是一笔旧代际的在途写"。
    EXPECT_EQ("7", rejectedGuard);
    EXPECT_EQ(0u, client.in_flight_save_count());
    EXPECT_EQ(0u, client.pending_save_count());
}

// 在途存盘(guard=7)被拒时,排队中的值已经带着更新的归属(guard=8):调用方在这笔写
// 在途期间拿到了新 epoch,仍是合法持有者。那份更新的快照必须照常发出,不能被当成
// "同一份已作废的归属"丢掉;此时也不回 rejected(那笔旧写已被它取代)。
TEST(CurrencyTest, RedisGuardedSaveRejectedKeepsPendingValueWithNewerGuard)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    int rejectedCount = 0;
    client.SetSaveCallback([&](Guid, CurrencyComp&) { ++savedCount; });
    client.SetSaveRejectedCallback([&](Guid, const std::string&, const std::string&) { ++rejectedCount; });

    constexpr Guid kPlayerId = 10005;
    const std::string guardKey = "player:10005:owner_epoch";
    const auto inFlight = Peer::TrackGuardedSave(client, kPlayerId, guardKey, "7");
    client.Save(std::make_shared<CurrencyComp>(), kPlayerId, guardKey, "8");
    ASSERT_EQ(1u, client.in_flight_save_count());
    ASSERT_EQ(1u, client.pending_save_count());

    redisReply reply{};
    reply.type = REDIS_REPLY_INTEGER;
    reply.integer = 0;
    Peer::DeliverSaved(client, &reply, inFlight);

    EXPECT_EQ(0, savedCount);
    EXPECT_EQ(0, rejectedCount) << "a stale in-flight write superseded by a newer-guard value is not a deposal";
    EXPECT_EQ(0u, client.in_flight_save_count());
    // 本用例没有真实 hiredis 连接,IssueSave 会把它放回 pending 等重连;关键是它**还在**。
    EXPECT_EQ(1u, client.pending_save_count());
}

// guard 相等 → Lua 返回 1:与无 guard 的存盘完全一样走成功回调。
TEST(CurrencyTest, RedisGuardedSaveAcceptedCompletesNormally)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    int rejectedCount = 0;
    Guid savedKey = 0;
    client.SetSaveCallback([&](Guid key, CurrencyComp&) {
        ++savedCount;
        savedKey = key;
    });
    client.SetSaveRejectedCallback([&](Guid, const std::string&, const std::string&) { ++rejectedCount; });

    constexpr Guid kPlayerId = 10004;
    const auto inFlight = Peer::TrackGuardedSave(client, kPlayerId, "player:10004:owner_epoch", "7");
    ASSERT_EQ(1u, client.in_flight_save_count());

    redisReply reply{};
    reply.type = REDIS_REPLY_INTEGER;
    reply.integer = 1;
    Peer::DeliverSaved(client, &reply, inFlight);

    EXPECT_EQ(1, savedCount);
    EXPECT_EQ(kPlayerId, savedKey);
    EXPECT_EQ(0, rejectedCount);
    EXPECT_EQ(0u, client.in_flight_save_count());
    EXPECT_EQ(0u, client.pending_save_count());
}

// ---------------------------------------------------------------------------
// 存盘队列的新旧顺序与归属围栏(帮会二期 B4c,docs/design/guild-phase2/08-save-owner-fence.md §8.2.2 / §8.2.3)
//
// 同 key 写入的不变量:在途至多一份、排队至多一份,且排队的永远比在途新。修复前唯一破坏它的入口是
// EnqueueSave"无在途、排队里有一份失败后等退避的旧值"时直接发新值 —— 之后成功 / 失败 / 重连三条
// 路径都把那份旧值当"更新的值"发出去,盖掉新值。
// ---------------------------------------------------------------------------
namespace
{
std::shared_ptr<CurrencyComp> MarkedCurrency(uint64_t marker)
{
    auto value = std::make_shared<CurrencyComp>();
    value->add_values(marker);
    return value;
}

uint64_t MarkerOf(const CurrencyComp& value)
{
    return value.values_size() > 0 ? value.values(0) : 0;
}

redisReply IntegerReply(long long value)
{
    redisReply reply{};
    reply.type = REDIS_REPLY_INTEGER;
    reply.integer = value;
    return reply;
}

// 一个普通的写失败(不是 NOSCRIPT,NOSCRIPT 走的是原地重发)。
redisReply ErrorReply()
{
    static char kErr[] = "ERR injected by test";
    redisReply reply{};
    reply.type = REDIS_REPLY_ERROR;
    reply.str = kErr;
    reply.len = std::strlen(kErr);
    return reply;
}
} // namespace

// Q1a:在途 S1 写失败进排队等退避;此时来了新值 M(无在途)。M 顶替 S1,并继承它的退避进度
// (retry_count / next_retry_at / save_failure_notified)。
// 本用例没有连接,测不到"已连接时绕过排队直接发出"那条颠倒本身(未连接时修复前 M 也只会进排队);
// 修复前它红在三个继承字段上。颠倒本身由 Q3(真 Redis,RedisConnectedSaveDoesNotBypassFailedPending)覆盖。
TEST(CurrencyTest, RedisSaveSupersedesFailedPendingAndInheritsBackoff)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    client.SetSaveCallback([&](Guid, CurrencyComp&) { ++savedCount; });

    constexpr Guid kPlayerId = 10011;
    const auto s1 = Peer::TrackSave(client, kPlayerId, MarkedCurrency(1));
    auto error = ErrorReply();
    Peer::DeliverSaved(client, &error, s1);

    const auto failed = Peer::PendingSave(client, kPlayerId);
    ASSERT_EQ(s1, failed) << "写失败的那份进排队等退避";
    ASSERT_EQ(0u, client.in_flight_save_count());
    ASSERT_GT(failed->retry_count, 0);
    failed->save_failure_notified = true; // 模拟"这段故障已经告过警"
    const int inheritedRetryCount = failed->retry_count;
    const auto inheritedDueAt = failed->next_retry_at;

    client.Save(MarkedCurrency(2), kPlayerId);

    const auto pending = Peer::PendingSave(client, kPlayerId);
    ASSERT_NE(nullptr, pending);
    EXPECT_EQ(2u, MarkerOf(*pending->message_value)) << "新值顶替了排队的旧值";
    EXPECT_EQ(inheritedRetryCount, pending->retry_count) << "继承退避进度,不重置";
    EXPECT_EQ(inheritedDueAt, pending->next_retry_at) << "沿用旧值的到期时刻,交给定时器发";
    EXPECT_TRUE(pending->save_failure_notified) << "同一段故障不重复告警";
    EXPECT_EQ(1u, client.pending_save_count());
    EXPECT_EQ(0u, client.in_flight_save_count());
    EXPECT_EQ(0, savedCount);
}

// Q1b:接 Q1a,到期发出的是 M,落地回调带的是 M,之后 S1 再也不会被发出。
TEST(CurrencyTest, RedisSaveSupersededPendingPersistsNewestOnly)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    uint64_t savedMarker = 0;
    client.SetSaveCallback([&](Guid, CurrencyComp& value) {
        ++savedCount;
        savedMarker = MarkerOf(value);
    });

    constexpr Guid kPlayerId = 10012;
    const auto s1 = Peer::TrackSave(client, kPlayerId, MarkedCurrency(1));
    auto error = ErrorReply();
    Peer::DeliverSaved(client, &error, s1);
    client.Save(MarkedCurrency(2), kPlayerId);

    const auto issued = Peer::IssuePendingForTest(client, kPlayerId);
    ASSERT_NE(nullptr, issued);
    auto ok = IntegerReply(1);
    Peer::DeliverSaved(client, &ok, issued);

    EXPECT_EQ(1, savedCount);
    EXPECT_EQ(2u, savedMarker) << "落地并回调的是新值";
    EXPECT_EQ(0u, client.pending_save_count()) << "旧值 S1 再也不会被发出";
    EXPECT_EQ(0u, client.in_flight_save_count());
}

// Q1c:顶替后的 M 发出又失败,重新排队的仍是 M,最终落地的也是 M(不是 S1)。
TEST(CurrencyTest, RedisSaveSupersededPendingRetriesNewestAfterError)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    int savedCount = 0;
    uint64_t savedMarker = 0;
    client.SetSaveCallback([&](Guid, CurrencyComp& value) {
        ++savedCount;
        savedMarker = MarkerOf(value);
    });

    constexpr Guid kPlayerId = 10013;
    const auto s1 = Peer::TrackSave(client, kPlayerId, MarkedCurrency(1));
    auto error = ErrorReply();
    Peer::DeliverSaved(client, &error, s1);
    client.Save(MarkedCurrency(2), kPlayerId);

    const auto firstTry = Peer::IssuePendingForTest(client, kPlayerId);
    ASSERT_NE(nullptr, firstTry);
    Peer::DeliverSaved(client, &error, firstTry);

    const auto requeued = Peer::PendingSave(client, kPlayerId);
    ASSERT_NE(nullptr, requeued);
    EXPECT_EQ(2u, MarkerOf(*requeued->message_value)) << "失败后重新排队的是新值,不是 S1";

    const auto secondTry = Peer::IssuePendingForTest(client, kPlayerId);
    ASSERT_NE(nullptr, secondTry);
    auto ok = IntegerReply(1);
    Peer::DeliverSaved(client, &ok, secondTry);

    EXPECT_EQ(1, savedCount);
    EXPECT_EQ(2u, savedMarker);
    EXPECT_EQ(0u, client.pending_save_count());
    EXPECT_EQ(0u, client.in_flight_save_count());
}

// Q2:HasUnsettledSave 看"在途或排队",按 key 区分,只读;在成功回调里恒为 false。
TEST(CurrencyTest, RedisHasUnsettledSaveTracksInFlightAndPending)
{
    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    Client::HiredisPtr hiredis;
    Client client(hiredis);
    constexpr Guid kPlayerId = 10014;
    constexpr Guid kOtherPlayerId = 10015;
    bool unsettledInsideCallback = true;
    client.SetSaveCallback([&](Guid key, CurrencyComp&) { unsettledInsideCallback = client.HasUnsettledSave(key); });

    EXPECT_FALSE(client.HasUnsettledSave(kPlayerId));

    const auto inFlight = Peer::TrackSave(client, kPlayerId, MarkedCurrency(1));
    EXPECT_TRUE(client.HasUnsettledSave(kPlayerId)) << "仅在途";
    EXPECT_FALSE(client.HasUnsettledSave(kOtherPlayerId)) << "按 key 区分";

    client.Save(MarkedCurrency(2), kPlayerId);
    EXPECT_TRUE(client.HasUnsettledSave(kPlayerId)) << "在途 + 排队";
    EXPECT_EQ(1u, client.in_flight_save_count()) << "查询不改队列";
    EXPECT_EQ(1u, client.pending_save_count());

    // 在途那份写失败:排队里是更新的值 → 发它(未连接,回到排队)、丢掉失败的那份。
    auto error = ErrorReply();
    Peer::DeliverSaved(client, &error, inFlight);
    EXPECT_EQ(0u, client.in_flight_save_count());
    EXPECT_EQ(1u, client.pending_save_count());
    EXPECT_TRUE(client.HasUnsettledSave(kPlayerId)) << "仅排队也算未落地";

    const auto issued = Peer::IssuePendingForTest(client, kPlayerId);
    ASSERT_NE(nullptr, issued);
    auto ok = IntegerReply(1);
    Peer::DeliverSaved(client, &ok, issued);
    EXPECT_FALSE(unsettledInsideCallback) << "成功回调只在排队为空时发布,回调里查询恒为 false";
    EXPECT_FALSE(client.HasUnsettledSave(kPlayerId)) << "全部落地";
}

// Q5:守卫脚本的结构 —— 先比对归属、后写数据;缺键补种;不等在写之前返回 0。
// 不代替下面的真 Redis 用例,只保证没有 Redis 的环境也能抓住脚本被改坏。
TEST(CurrencyTest, RedisGuardScriptComparesBeforeWrite)
{
    const std::string lua = kSaveIfGuardLuaScript;
    const auto readGuard = lua.find("redis.call('GET', KEYS[2])");
    const auto writeData = lua.find("redis.call('SET', KEYS[1]");
    ASSERT_NE(std::string::npos, readGuard);
    ASSERT_NE(std::string::npos, writeData);
    EXPECT_LT(readGuard, writeData) << "先比对归属、后写数据";
    EXPECT_NE(std::string::npos, lua.find("cur == false")) << "guard 键缺失时补种(跨 zone 会话论证过的取舍)";
    const auto mismatch = lua.find("cur ~= ARGV[2]");
    ASSERT_NE(std::string::npos, mismatch);
    const auto reject = lua.find("return 0", mismatch);
    ASSERT_NE(std::string::npos, reject);
    EXPECT_LT(reject, writeData) << "归属不等必须在写数据之前返回";
}

// ── 真 Redis 用例:读 MMORPG_TEST_REDIS_ADDR(host:port,host 必须是 IP),未设就跳过 ──────────
// 只指向本机 / 专用的测试 Redis,不要指向共享或预发实例:用例会写并删除自己的键(含 dirty_keys_set 的成员)。
namespace
{
bool TestRedisAddr(std::string& host, uint16_t& port)
{
    const char* raw = std::getenv("MMORPG_TEST_REDIS_ADDR");
    if (raw == nullptr || *raw == '\0')
    {
        return false;
    }
    const std::string addr(raw);
    const auto colon = addr.rfind(':');
    if (colon == std::string::npos || colon == 0 || colon + 1 >= addr.size())
    {
        return false;
    }
    host = addr.substr(0, colon);
    const long parsed = std::strtol(addr.c_str() + colon + 1, nullptr, 10);
    if (parsed <= 0 || parsed > 65535)
    {
        return false;
    }
    port = static_cast<uint16_t>(parsed);
    return true;
}

// 在 loop 上跑到连接回调或 3 秒超时。
std::unique_ptr<hiredis::Hiredis> ConnectForTest(muduo::net::EventLoop& loop, const std::string& host, uint16_t port)
{
    auto connection = std::make_unique<hiredis::Hiredis>(&loop, muduo::net::InetAddress(host, port));
    connection->setConnectCallback([&loop](hiredis::Hiredis*, int) { loop.quit(); });
    connection->connect();
    const auto timeout = loop.runAfter(3.0, [&loop] { loop.quit(); });
    loop.loop();
    loop.cancel(timeout);
    connection->setConnectCallback({});
    return connection;
}

// 有意泄漏:析构 Hiredis 要走 redisAsyncFree 与 Channel 注销,测试里 loop 与连接的收尾顺序不受控,没必要冒这个险。
// (third_party/muduo 原版 Hiredis 有"连接态析构断言 / 断开后二次 redisAsyncFree"的问题;本工程实际链接的
// cpp/libs/engine/muduo_windows 版已修,这里泄漏只是为了不依赖收尾顺序。)
// 做成 RAII:用例中途 ASSERT 失败提前返回时也照样泄漏,而不是在析构里断言、带崩整个测试进程。
// 声明在 connection 之后、客户端之前:客户端先析构,再放手,最后 connection 以空指针析构。
struct LeakConnectionOnExit
{
    std::unique_ptr<hiredis::Hiredis>& connection;
    ~LeakConnectionOnExit() { (void)connection.release(); }
};

// 同步连接,只用来布置与核对键。
struct SyncRedisForTest
{
    redisContext* context{nullptr};

    ~SyncRedisForTest()
    {
        if (context != nullptr)
        {
            redisFree(context);
        }
    }

    bool Connect(const std::string& host, uint16_t port)
    {
        context = redisConnect(host.c_str(), port);
        return context != nullptr && context->err == 0;
    }

    // 键不存在时返回 "(nil)"。
    std::string Get(const std::string& key)
    {
        auto* reply = static_cast<redisReply*>(redisCommand(context, "GET %b", key.data(), key.size()));
        std::string value = "(nil)";
        if (reply != nullptr && reply->type == REDIS_REPLY_STRING)
        {
            value.assign(reply->str, reply->len);
        }
        if (reply != nullptr)
        {
            freeReplyObject(reply);
        }
        return value;
    }

    void Set(const std::string& key, const std::string& value)
    {
        Run(redisCommand(context, "SET %b %b", key.data(), key.size(), value.data(), value.size()));
    }

    void Del(const std::string& key) { Run(redisCommand(context, "DEL %b", key.data(), key.size())); }

    void SRem(const std::string& set, const std::string& member)
    {
        Run(redisCommand(context, "SREM %b %b", set.data(), set.size(), member.data(), member.size()));
    }

private:
    static void Run(void* reply)
    {
        if (reply != nullptr)
        {
            freeReplyObject(reply);
        }
    }
};
} // namespace

// Q3(颠倒的回归用例):已连接、无在途、排队里有一份失败后等退避的旧值时,新值不得被直接发出。
// 修复前这条红:新值被 IssueSave 发出(在途 1),排队里仍是更旧的 S1。只需要"已连接"这一状态,不等回包。
TEST(CurrencyTest, RedisConnectedSaveDoesNotBypassFailedPending)
{
    std::string host;
    uint16_t port = 0;
    if (!TestRedisAddr(host, port))
    {
        GTEST_SKIP() << "MMORPG_TEST_REDIS_ADDR 未设置(host:port),跳过真 Redis 用例";
    }

    using Client = MessageAsyncClient<Guid, CurrencyComp>;
    using Peer = MessageAsyncClientTestPeer<Guid, CurrencyComp>;

    muduo::net::EventLoop loop;
    Client::HiredisPtr connection = ConnectForTest(loop, host, port);
    const LeakConnectionOnExit leak{connection};
    ASSERT_TRUE(connection->connected()) << "连不上 " << host << ":" << port;
    {
        Client client(connection);
        constexpr Guid kPlayerId = 950299001;
        const auto s1 = Peer::TrackSave(client, kPlayerId, MarkedCurrency(1));
        auto error = ErrorReply();
        Peer::DeliverSaved(client, &error, s1);
        ASSERT_EQ(s1, Peer::PendingSave(client, kPlayerId));

        client.Save(MarkedCurrency(2), kPlayerId);

        EXPECT_EQ(0u, client.in_flight_save_count()) << "新值不得绕过排队的旧值直接发出";
        const auto pending = Peer::PendingSave(client, kPlayerId);
        ASSERT_NE(nullptr, pending);
        EXPECT_EQ(2u, MarkerOf(*pending->message_value)) << "排队的是新值,旧值 S1 已被顶替";
    }
}

// Q4(守卫脚本语义,端到端):guard 相等 → 写入;不等 → 拒绝且数据不变;缺键 → 补种并写入。
TEST(CurrencyTest, RedisGuardedSaveSemanticsAgainstRealRedis)
{
    std::string host;
    uint16_t port = 0;
    if (!TestRedisAddr(host, port))
    {
        GTEST_SKIP() << "MMORPG_TEST_REDIS_ADDR 未设置(host:port),跳过真 Redis 用例";
    }

    using Client = MessageAsyncClient<Guid, CurrencyComp>;

    SyncRedisForTest sync;
    ASSERT_TRUE(sync.Connect(host, port)) << "同步连接失败 " << host << ":" << port;

    muduo::net::EventLoop loop;
    Client::HiredisPtr connection = ConnectForTest(loop, host, port);
    const LeakConnectionOnExit leak{connection};
    ASSERT_TRUE(connection->connected()) << "连不上 " << host << ":" << port;
    {
        Client client(connection);
        std::string outcome;
        client.SetSaveCallback([&](Guid, CurrencyComp&) {
            outcome = "saved";
            loop.quit();
        });
        client.SetSaveRejectedCallback([&](Guid, const std::string&, const std::string&) {
            outcome = "rejected";
            loop.quit();
        });

        constexpr Guid kPlayerId = 950299002;
        const std::string guardKey = "b4c_test:950299002:owner_epoch";
        const std::string blobKey = client.full_name() + ":" + std::to_string(kPlayerId);
        const auto cleanup = [&] {
            sync.Del(blobKey);
            sync.Del(guardKey);
            sync.SRem("dirty_keys_set", blobKey);
        };
        // 一次完整的带 guard 存盘:跑事件循环直到落地回调或 3 秒超时,返回结局("" = 超时)。
        const auto saveOnce = [&](const std::shared_ptr<CurrencyComp>& value, const std::string& expected) {
            outcome.clear();
            client.Save(value, kPlayerId, guardKey, expected);
            const auto timeout = loop.runAfter(3.0, [&loop] { loop.quit(); });
            loop.loop();
            loop.cancel(timeout);
            return outcome;
        };
        cleanup();

        // 1. guard 相等 → 写入
        sync.Set(guardKey, "5");
        const auto first = MarkedCurrency(1);
        EXPECT_EQ("saved", saveOnce(first, "5"));
        EXPECT_EQ(first->SerializeAsString(), sync.Get(blobKey));

        // 2. guard 不等 → 拒绝,数据不变
        sync.Set(guardKey, "6");
        EXPECT_EQ("rejected", saveOnce(MarkedCurrency(2), "5"));
        EXPECT_EQ(first->SerializeAsString(), sync.Get(blobKey)) << "被拒的写不能落地";

        // 3. guard 缺键 → 补种成调用方的期望值,并写入
        sync.Del(guardKey);
        const auto third = MarkedCurrency(3);
        EXPECT_EQ("saved", saveOnce(third, "9"));
        EXPECT_EQ(third->SerializeAsString(), sync.Get(blobKey));
        EXPECT_EQ("9", sync.Get(guardKey));

        cleanup();
    }
}

// ---------------------------------------------------------------------------
// BlockCurrency (GM禁止获取)
// ---------------------------------------------------------------------------

TEST(CurrencyTest, BlockCurrencyPreventsAdd)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_TRUE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    // AddCurrency should be rejected for blocked type.
    EXPECT_NE(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, BlockCurrencyDoesNotAffectDeduct)
{
    auto player = CreateTestPlayer();

    // Pre-load some gold first.
    CurrencySystem::AddCurrency(player, kCurrencyGold, 200);
    CurrencySystem::BlockCurrency(player, kCurrencyGold);

    // Deduct should still work even when blocked.
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 50));
    EXPECT_EQ(150u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, BlockDoesNotAffectOtherTypes)
{
    auto player = CreateTestPlayer();

    CurrencySystem::BlockCurrency(player, kCurrencyGold);

    // Diamond is not blocked.
    EXPECT_FALSE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyDiamond));
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyDiamond, 100));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyDiamond));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, UnblockCurrencyAllowsAdd)
{
    auto player = CreateTestPlayer();

    CurrencySystem::BlockCurrency(player, kCurrencyGold);
    EXPECT_TRUE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    CurrencySystem::UnblockCurrency(player, kCurrencyGold);
    EXPECT_FALSE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, BlockCurrencyIdempotent)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_TRUE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    // Unblock once should be enough.
    EXPECT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player, kCurrencyGold));
    EXPECT_FALSE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, UnblockNonBlockedIsNoOp)
{
    auto player = CreateTestPlayer();

    EXPECT_FALSE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));
    EXPECT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player, kCurrencyGold));
    EXPECT_FALSE(CurrencySystem::IsCurrencyBlocked(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// GetBalance edge cases
// ---------------------------------------------------------------------------

TEST(CurrencyTest, GetBalanceDefaultZero)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyDiamond));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyBindDiamond));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// 资产通道错误码细分(docs/design/guild-phase2/04-asset-channel.md §4.11、§4.14.3)
//
// 这几条断言的是**具体取值**,不是 `EXPECT_NE(kSuccess, …)`。上面那些老用例只验
// "失败了",于是 2026-09 之前"冻结"和"余额不足"和"参数写错了"回同一个
// kInvalidParameter 也一路绿着 —— 而通用资产通道要靠这个返回值区分
// RETRY(条件会消失,可重投)与 REJECTED(终局拒绝,记账并退款),混在一起
// 就是要么重复发放、要么把玩家过图那几百毫秒里的奖励永久丢掉。
// ---------------------------------------------------------------------------

TEST(CurrencyTest, DeductInsufficientReturnsAssetCode)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 10);
    EXPECT_EQ(kAssetCurrencyInsufficient, CurrencySystem::DeductCurrency(player, kCurrencyGold, 20));
    EXPECT_EQ(10u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DeductFrozenReturnsAssetFrozen)
{
    auto player = CreateTestPlayer();

    CurrencySystem::AddCurrency(player, kCurrencyGold, 100);
    // PlayerLifecycleSystem::IsCrossZoneFrozen 就是 valid() + any_of<PlayerFrozenComp>
    // (player_lifecycle.cpp,函数名定位;那个文件正被跨 zone 会话改着,行号写下来就过期)。
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

    EXPECT_EQ(kAssetFrozen, CurrencySystem::DeductCurrency(player, kCurrencyGold, 30));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));

    // 解冻之后同一笔扣款必须成立 —— 这条才说明 kAssetFrozen 真的是"暂时"。
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 30));
    EXPECT_EQ(70u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AddFrozenReturnsAssetFrozen)
{
    auto player = CreateTestPlayer();

    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(kAssetFrozen, CurrencySystem::AddCurrency(player, kCurrencyGold, 50));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 50));
    EXPECT_EQ(50u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AddBlockedReturnsAssetBlocked)
{
    auto player = CreateTestPlayer();

    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_EQ(kAssetBlocked, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));

    // 封禁只挡获取,不挡扣除(既有语义,顺带钉住:它不该回 kAssetBlocked)。
    CurrencySystem::UnblockCurrency(player, kCurrencyGold);
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player, kCurrencyGold));
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 40));
    EXPECT_EQ(60u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, ProgrammingErrorsStillReturnInvalidParameter)
{
    auto player = CreateTestPlayer();

    // 这条是反向护栏:错误码细分**只**覆盖冻结 / 余额不足 / 封禁三种业务条件。
    // "数量 <= 0""币种越界"是调用方写错了,仍旧回 kInvalidParameter ——
    // 把它们也搬进 27000 段,资产通道就会把编程错误当成可重投的业务条件。
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyGold, 0));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyGold, -1));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::DeductCurrency(player, kCurrencyGold, 0));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::DeductCurrency(player, kCurrencyGold, -5));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyMax, 10));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::DeductCurrency(player, kCurrencyMax, 10));

    // **判定顺序**:冻结 × 越界币种这个组合必须仍旧回 kInvalidParameter。
    // 纯参数校验排在冻结之后的话,这里会拿到 RETRY 类的 kAssetFrozen,资产通道就会
    // 对一个"解冻之后照样失败"的请求一直重投到超时 —— 那是一条只在两个条件同时
    // 成立时才现形的缝,所以单独钉住。
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyMax, 10));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::DeductCurrency(player, kCurrencyMax, 10));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyGold, 0));
    // 同一个冻结玩家、合法币种合法数量 —— 这时才该是 kAssetFrozen,证明上面三条
    // 不是"冻结判定被整个删掉了"。
    EXPECT_EQ(kAssetFrozen, CurrencySystem::AddCurrency(player, kCurrencyGold, 10));
    tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

int main(int argc, char** argv)
{
    // 生产线程由 scene 的 ConfigureGuidSegmentClients 按配置启用 txlog 号段并经 DataService 领首段;
    // 单测没有那条启动链。给 txlog 种类装一个不回话的假传输并直接投一段大范围,
    // 避免交易流水因 tx_id 号段未就绪被 fail-closed 丢弃(那条路径由 bag_test 专门覆盖)。
    {
        auto &txlog = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog);
        GuidSegmentClient::Options options;
        options.kindName = "txlog";
        options.initialStep = 1000000;
        const bool enabled = txlog.Enable(
            options, [](const std::string &, uint32_t) { return true; },
            [](GuidSegmentClient::TimerKind, double, std::function<void()>) {});
        if (!enabled)
        {
            return 1;
        }
        txlog.Warm();
        txlog.OnResponse(0, 1, uint64_t{1} << 50);
    }
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}
