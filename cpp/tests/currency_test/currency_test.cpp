#include <gtest/gtest.h>

#include <limits>

#include "infra/storage/redis_client/redis_client.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/system/currency_system.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "proto/common/component/currency_comp.pb.h"
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
