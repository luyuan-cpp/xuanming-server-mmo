#include <gtest/gtest.h>

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <functional>
#include <iterator>
#include <limits>
#include <memory>
#include <optional>
#include <random>
#include <regex>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <google/protobuf/descriptor.h>


#include "deps/hiredis/hiredis.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/system/currency_system.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "player/comp/player_frozen_comp.h" // 冻结用例:IsCrossZoneFrozen 就是 any_of<PlayerFrozenComp>
#include "battle/system/battle_lock_rules.h" // 玩家域键形金样:战斗锁与上下文的格式串
#include "player/comp/player_ownership_comp.h" // 玩家域键形金样
#include "player/system/player_landing.h" // 玩家域键形金样:同步摘要的格式串
#include "player/system/handoff_sql_barrier.h"
#include "proto/common/base/currency_type.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetFrozen / kAssetBlocked / kAssetCurrencyInsufficient
#include "table/proto/tip/common_error_tip.pb.h"
#include <thread_context/ecs_context.h>
#include "modules/id_segment/guid_segment_registry.h"
#include "../test_config_helper.h"

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
// 币种取值表:currency.h 由 proto/common/base/currency_type.proto 派生
// ---------------------------------------------------------------------------

// CurrencyComp.values 以币种为下标、kCurrencyMax 当上限,前提是 proto 取值从 0 连续。
// 删值、跳号或复用都会让已入箱的旧条目在所有版本上永远判成版本错位,这里对照描述符守住
// (Go 侧 shared/currencytype 的 TestValuesAreContiguousFromZero 守同一条)。
TEST(CurrencyTest, CurrencyTypeEnumIsDerivedFromTheProtoTable)
{
    const auto *descriptor = common::base::CurrencyType_descriptor();
    ASSERT_NE(nullptr, descriptor);
    EXPECT_EQ(static_cast<uint32_t>(common::base::CurrencyType_ARRAYSIZE), static_cast<uint32_t>(kCurrencyMax));
    EXPECT_EQ(common::base::CurrencyType_ARRAYSIZE, descriptor->value_count()) << "取值必须从 0 连续,不得跳号";
    for (int i = 0; i < descriptor->value_count(); ++i)
    {
        EXPECT_NE(nullptr, descriptor->FindValueByNumber(i)) << "缺少取值 " << i;
        EXPECT_TRUE(IsKnownCurrencyType(static_cast<uint32_t>(i)));
    }
    EXPECT_FALSE(IsKnownCurrencyType(static_cast<uint32_t>(kCurrencyMax)));
    EXPECT_FALSE(IsKnownCurrencyType(std::numeric_limits<uint32_t>::max()));

    EXPECT_EQ(0u, static_cast<uint32_t>(kCurrencyGold));
    EXPECT_EQ(1u, static_cast<uint32_t>(kCurrencyDiamond));
    EXPECT_EQ(2u, static_cast<uint32_t>(kCurrencyBindDiamond));
    EXPECT_EQ("CURRENCY_TYPE_GOLD", common::base::CurrencyType_Name(common::base::CURRENCY_TYPE_GOLD));
    EXPECT_EQ("CURRENCY_TYPE_DIAMOND", common::base::CurrencyType_Name(common::base::CURRENCY_TYPE_DIAMOND));
    EXPECT_EQ("CURRENCY_TYPE_BIND_DIAMOND", common::base::CurrencyType_Name(common::base::CURRENCY_TYPE_BIND_DIAMOND));
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
    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyGold, 60));

    // Add 100 gold — 60 goes to debt, 40 is credited.
    EXPECT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(40u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DebtPartialRepayment)
{
    auto player = CreateTestPlayer();

    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyGold, 100));

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

    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyGold, 100));

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

    EXPECT_EQ(kInvalidParameter, CurrencySystem::AttachDebt(player, kCurrencyGold, 10))
        << "溢出拒绝必须如实回码,收件箱执行器据此记 REJECTED";
    EXPECT_EQ(std::numeric_limits<uint64_t>::max() - 5, debt.owed);

    CurrencySystem::AdjustDebt(player, kCurrencyGold, 10);
    EXPECT_EQ(std::numeric_limits<uint64_t>::max() - 5, debt.owed);

    DestroyTestPlayer(player);
}

// AttachDebt 改为返回 tip 码(docs/design/player-inbox-service.md §7.6):纯参数错误与缺组件都回
// kInvalidParameter,且不留下任何欠款条目。
TEST(CurrencyTest, AttachDebtReportsInvalidParametersWithoutSideEffects)
{
    auto player = CreateTestPlayer();
    const auto &comp = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player);

    EXPECT_EQ(kInvalidParameter, CurrencySystem::AttachDebt(player, kCurrencyGold, 0));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AttachDebt(player, kCurrencyGold, -5));
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AttachDebt(player, kCurrencyMax, 10)) << "越界币种";
    EXPECT_TRUE(comp.debts.empty()) << "拒绝路径不得落下欠款条目";

    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyGold, 7, "audit", "gm"));
    ASSERT_EQ(1u, comp.debts.count(static_cast<uint32_t>(kCurrencyGold)));
    EXPECT_EQ(7u, comp.debts.at(static_cast<uint32_t>(kCurrencyGold)).owed);

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AttachDebtWithoutRuntimeCurrencyCompIsRejected)
{
    auto player = CreateTestPlayer();
    tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player);

    EXPECT_EQ(kInvalidParameter, CurrencySystem::AttachDebt(player, kCurrencyGold, 10));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerCurrencyComp>(player)) << "只取不建";

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
    ASSERT_TRUE(source.SaveToProto(proto));

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

// 欠款上本版本不认识的字段随这条欠款携带、存盘原样写回;欠款删除后重建得到新对象,不带旧欠款的新字段
// (docs/design/mixed-version-persist-field-protection.md §4.1 的复活反例,§10 的回归用例)。
TEST(CurrencyTest, DebtUnknownFieldsTravelWithTheDebtOnly)
{
    constexpr int kFutureField = 9999; // 本版本不认识的字段号
    CurrencyComp persisted;
    auto *entry = persisted.add_debts();
    entry->set_currency_type(static_cast<uint32_t>(kCurrencyGold));
    entry->set_owed(500);
    entry->mutable_unknown_fields()->AddVarint(kFutureField, 42);

    auto player = CreateTestPlayer();
    auto &runtime = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player);
    runtime.LoadFromProto(persisted);

    CurrencyComp saved;
    ASSERT_TRUE(runtime.SaveToProto(saved));
    ASSERT_EQ(1, saved.debts_size());
    ASSERT_EQ(1, saved.debts(0).unknown_fields().field_count()) << "未知字段必须随欠款写回";
    EXPECT_EQ(kFutureField, saved.debts(0).unknown_fields().field(0).number());
    EXPECT_EQ(42u, saved.debts(0).unknown_fields().field(0).varint());

    // 还清删除后再挂同币种:新欠款是新对象,不得带上旧欠款的未知字段。
    runtime.debts.erase(static_cast<uint32_t>(kCurrencyGold));
    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyGold, 80));
    CurrencyComp reattached;
    ASSERT_TRUE(runtime.SaveToProto(reattached));
    ASSERT_EQ(1, reattached.debts_size());
    EXPECT_EQ(80u, reattached.debts(0).owed());
    EXPECT_TRUE(reattached.debts(0).unknown_fields().empty()) << "旧欠款的新字段复活到了新欠款上";

    DestroyTestPlayer(player);
}

// 已有欠款带着本版本不认识的字段时,旧版本不把新的一笔并进去(设计 §4.4)。
TEST(CurrencyTest, AttachDebtRefusesToMergeIntoDebtWithUnknownFields)
{
    CurrencyComp persisted;
    auto *entry = persisted.add_debts();
    entry->set_currency_type(static_cast<uint32_t>(kCurrencyGold));
    entry->set_owed(500);
    entry->mutable_unknown_fields()->AddVarint(9999, 1);

    auto player = CreateTestPlayer();
    auto &runtime = tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player);
    runtime.LoadFromProto(persisted);

    // 只读谓词与原语同口径:调用方(收件箱执行器)靠它先判、回版本错位(kVersionSkew),不拿通用码 kFeatureUnavailable 反推。
    EXPECT_TRUE(CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyGold));
    EXPECT_FALSE(CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyDiamond)) << "没有这种币种的欠款";
    EXPECT_FALSE(CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyMax)) << "越界币种";

    EXPECT_EQ(kFeatureUnavailable, CurrencySystem::AttachDebt(player, kCurrencyGold, 100));
    EXPECT_EQ(500u, runtime.debts.at(static_cast<uint32_t>(kCurrencyGold)).owed) << "拒绝时欠款不得改动";
    // 别的币种没有携带未知字段,照常可挂。
    EXPECT_EQ(kSuccess, CurrencySystem::AttachDebt(player, kCurrencyDiamond, 100));
    EXPECT_FALSE(CurrencySystem::DebtCarriesUnknownFields(player, kCurrencyDiamond)) << "新建的欠款不带未知字段";

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, AnomalyBucketsSeparateCurrencyAndItemAndClearForTestIsolation)
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

    AnomalyDetector::ClearAll();
    EXPECT_EQ(0u, AnomalyDetector::GetCurrencyGainCount(player, kCurrencyGold));

    DestroyTestPlayer(player);
    AnomalyDetector::ClearAll();
}

namespace
{
// currency_test 不链接节点生命周期宿主;沿源码契约守住“缓存丢失不建空号”。
std::string ReadPlayerLifecycleSourceForContract()
{
    const std::filesystem::path relative = "cpp/libs/services/scene/player/system/player_lifecycle.cpp";
    for (auto root : {std::filesystem::path(__FILE__).parent_path(), std::filesystem::current_path()})
    {
        while (!root.empty())
        {
            std::ifstream stream(root / relative, std::ios::binary);
            if (stream)
            {
                std::string source{std::istreambuf_iterator<char>(stream), std::istreambuf_iterator<char>()};
                source.erase(std::remove(source.begin(), source.end(), '\r'), source.end());
                return source;
            }
            const auto parent = root.parent_path();
            if (parent == root) break;
            root = parent;
        }
    }
    return {};
}
} // namespace

TEST(CurrencyTest, PlayerLoadFailureNeverCreatesEmptyPlayerSourceContract)
{
    const auto source = ReadPlayerLifecycleSourceForContract();
    ASSERT_FALSE(source.empty()) << "找不到生命周期源码,不能跳过资产恢复契约";
    const auto start = source.find("void PlayerLifecycleSystem::HandlePlayerAsyncLoadFailed(");
    ASSERT_NE(std::string::npos, start);
    const auto end = source.find("\nvoid PlayerLifecycleSystem::HandlePlayerAsyncLoaded(", start);
    ASSERT_NE(std::string::npos, end);
    const auto body = source.find('{', start);
    ASSERT_LT(body, end);
    const auto failure = source.substr(body, end - body);
    EXPECT_EQ(std::string::npos, failure.find("HandlePlayerAsyncLoaded("));
    // 类型别名可以引用 PlayerAllData,禁止的是构造空对象来伪装一次成功载入。
    EXPECT_FALSE(std::regex_search(failure,
        std::regex(R"(\bPlayerAllData\s+[A-Za-z_][A-Za-z_0-9]*\s*(?:;|\{\s*\}\s*;))")));
    EXPECT_FALSE(std::regex_search(failure, std::regex(R"(\bPlayerAllData\s*(?:\{\s*\}|\(\s*\)))")));
    EXPECT_NE(std::string::npos, failure.find("SendTipToPendingSession("));
    EXPECT_NE(std::string::npos, failure.find("EraseSessionMappingIfOwned("));
    EXPECT_NE(std::string::npos, failure.find("tlsPendingEnterMap.erase("));
    EXPECT_NE(std::string::npos, failure.find("AbandonInheritClear("));
}

TEST(CurrencyTest, PlayerLoadRejectsMissingOrForeignSubTableIdentitySourceContract)
{
    const auto source = ReadPlayerLifecycleSourceForContract();
    ASSERT_FALSE(source.empty()) << "找不到生命周期源码,不能跳过载入身份契约";
    const auto start = source.find("void PlayerLifecycleSystem::HandlePlayerAsyncLoaded(");
    ASSERT_NE(std::string::npos, start);
    const auto end = source.find("\nvoid PlayerLifecycleSystem::HandlePlayerAsyncSaved(", start);
    ASSERT_NE(std::string::npos, end);
    const auto load = source.substr(start, end - start);
    const auto reject = load.find("if (playerId == kInvalidGuid || message.player_database_data().player_id() != playerId ||");
    ASSERT_NE(std::string::npos, reject);
    const auto secondIdentity = load.find("message.player_database_1_data().player_id() != playerId", reject);
    ASSERT_NE(std::string::npos, secondIdentity);
    const auto fail = load.find("HandlePlayerAsyncLoadFailed(playerId, PlayerLoadFailureReason::kStoreError);", secondIdentity);
    ASSERT_NE(std::string::npos, fail);
    const auto gate = load.find("exit_release_mark::DecideInheritGate(");
    const auto consume = load.find("PlayerEnterContext ctx = std::move(pendingIt->second);");
    const auto initialize = load.find("InitPlayerFromAllData(message, ctx);");
    ASSERT_NE(std::string::npos, gate);
    ASSERT_NE(std::string::npos, consume);
    ASSERT_NE(std::string::npos, initialize);
    EXPECT_LT(fail, gate) << "损坏存档不能暂存为可恢复的 A2′ 载入结果";
    EXPECT_LT(fail, consume) << "失败处理必须还能取得待入场上下文并补写继承围栏";
    EXPECT_LT(consume, initialize);
    EXPECT_EQ(std::string::npos, load.find("set_player_id("));
    EXPECT_EQ(std::string::npos, load.find("patchedMessage"));
    EXPECT_EQ(std::string::npos, load.find("registration_timestamp()"))
        << "合法新号双 ID 已由种子提供,首次注册仍由 InitPlayerFromAllData 统一处理";
}

TEST(CurrencyTest, TravelHandoffMarkRequiresAtomicSqlBarrierSourceContract)
{
    const auto source = ReadPlayerLifecycleSourceForContract();
    ASSERT_FALSE(source.empty());
    const auto start = source.find("void PlayerLifecycleSystem::BeginTravelHandoff(");
    ASSERT_NE(std::string::npos, start);
    const auto end = source.find("\nvoid PlayerLifecycleSystem::HandleTravelMarkWriteRejected(", start);
    ASSERT_NE(std::string::npos, end);
    const auto begin = source.substr(start, end - start);
    EXPECT_NE(std::string::npos, begin.find("handoff_sql_barrier::kPublishMarkDirectLua"));
    EXPECT_EQ(std::string::npos, begin.find("\"SET %s %s EX %d\"")) << "不能绕过资格的原子复核";
    EXPECT_NE(std::string::npos, begin.find("current->frozenAtSteady != frozenAt"));
    EXPECT_NE(std::string::npos, begin.find("current->requestedAtMs = 0;"));
    EXPECT_NE(std::string::npos, begin.find("current->markEpoch = 0;"));
    EXPECT_NE(std::string::npos, begin.find("DestroyDeposedPlayer(playerId, \"handoff_sql_owner_rejected\")"));
    const auto send = begin.find("RequestTravelEnterScene(playerId);");
    ASSERT_NE(std::string::npos, send);
    EXPECT_EQ(std::string::npos, begin.find("RequestTravelEnterScene(playerId);", send + 1));
    EXPECT_GT(send, begin.find("reply->integer != 1")) << "只有明确 1 能发 SM";
}

// 存盘接入点(docs/design/player-save-direct-commit.md §6)。这几处的顺序决定了会不会回档,而宿主跑不到 EnterScene 和
// 真 Redis 的凭证链,这里守住顺序。
TEST(CurrencyTest, PlayerStoreDirectPathSourceContract)
{
    const auto source = ReadPlayerLifecycleSourceForContract();
    ASSERT_FALSE(source.empty());

    // 交给状态机在 marshal、交接跳过之后。在发号之前:号只由状态机在确定要写时取。
    const auto save = source.find("PlayerLifecycleSystem::SaveResult PlayerLifecycleSystem::SavePlayerImpl(");
    ASSERT_NE(std::string::npos, save);
    const auto marshal = source.find("MarshalPlayerForSave(player, playerId, *message)", save);
    const auto handoffSkip = source.find("return SaveResult::kHandoffAlreadyRequested;", save);
    const auto divert = source.find("return SubmitPlayerSave(player, playerId, message, allowSkipWhenPersisted);", save);
    ASSERT_NE(std::string::npos, divert);
    EXPECT_LT(marshal, handoffSkip);
    EXPECT_LT(handoffSkip, divert);

    // 重连取消退出之后写一笔清掉 released,这一笔要带本次路由的代次。
    const auto cancelled = source.find("const bool exitCancelled = tlsEcs.actorRegistry.any_of<UnregisterPlayer>(player);");
    const auto adopted = source.find("ownerEpoch.handoffFence = ctx.handoffFence;");
    const auto cleared = source.find("if (exitCancelled)");
    ASSERT_NE(std::string::npos, cleared);
    EXPECT_LT(cancelled, adopted);
    EXPECT_LT(adopted, cleared);

    // 会话结束凭证:先释放,有结论才写。
    const auto read = source.find("void OnSessionEndedQualificationRead(");
    ASSERT_NE(std::string::npos, read);
    const auto qualified = source.find("case CredentialQualification::kQualified:", read);
    EXPECT_NE(std::string::npos, source.find("ReleaseThenWriteSessionEndedCredential(playerId, expectedOwnerEpoch, observed);", qualified));

    // 交接标记:用不查 ACK 的脚本,Redis 里没有 ACK 镜像,查了就永远发不出去。
    const auto begin = source.find("void PlayerLifecycleSystem::BeginTravelHandoff(");
    ASSERT_NE(std::string::npos, begin);
    EXPECT_NE(std::string::npos, source.find("handoff_sql_barrier::kPublishMarkDirectLua", begin));
    EXPECT_EQ(std::string::npos, handoff_sql_barrier::kPublishMarkDirectLua.find("HMGET"));
}

// 玩家域键形的跨语言金样(离线):与 go/shared/ownerepoch 的 TestKeyFormats、go/shared/handofffence 的键测试、
// go/shared/redisslot 的 TestPlayerKeyGoldenVectors、go/shared/playersync 的 TestKeyFormatMatchesCpp 钉同一组字面量,
// 改任何一侧都要同批改另一侧(docs/design/redis-player-store-cluster.md §5.2)。
TEST(CurrencyTest, PlayerDomainRedisKeysMatchGoContract)
{
    EXPECT_EQ("player:{12345}:owner_epoch", player_ownership::OwnerEpochRedisKey(12345));
    EXPECT_EQ("player:{18446744073709551615}:owner_epoch",
              player_ownership::OwnerEpochRedisKey(std::numeric_limits<Guid>::max()));
    EXPECT_EQ("player:{12345}:handoff", player_ownership::HandoffRedisKey(12345));
    EXPECT_EQ("player:{42}:handoff_fence", player_ownership::HandoffFenceRedisKey(42));
    EXPECT_EQ("player:{42}:location", player_ownership::LocationRedisKey(42));
    EXPECT_EQ("player:{18446744073709551615}:location",
              player_ownership::LocationRedisKey(std::numeric_limits<Guid>::max()));
    EXPECT_STREQ("battle:lock:{%llu}", battle_lock::kBattleLockKeyFmt);
    EXPECT_STREQ("battle:ctx:{%llu}", battle_lock::kBattleCtxKeyFmt);
    EXPECT_STREQ("player:{%llu}:sync_digest", player_landing::kSyncDigestKeyFmt);

    // 同一段 Lua 里出现的键必须同 slot:交接与归属脚本(owner_epoch、handoff_fence、handoff、location)、战斗锁脚本(lock、ctx)。
    for (const Guid playerId : {Guid{1}, Guid{42}, Guid{12345}, std::numeric_limits<Guid>::max()})
    {
        const auto slot = redis_slot::PlayerSlot(playerId);
        EXPECT_EQ(slot, redis_slot::HashSlot(player_ownership::OwnerEpochRedisKey(playerId)));
        EXPECT_EQ(slot, redis_slot::HashSlot(player_ownership::HandoffRedisKey(playerId)));
        EXPECT_EQ(slot, redis_slot::HashSlot(player_ownership::HandoffFenceRedisKey(playerId)));
        EXPECT_EQ(slot, redis_slot::HashSlot(player_ownership::LocationRedisKey(playerId)));
        const std::string tag = redis_slot::PlayerTag(playerId);
        EXPECT_EQ(slot, redis_slot::HashSlot("battle:lock:" + tag));
        EXPECT_EQ(slot, redis_slot::HashSlot("battle:ctx:" + tag));
        EXPECT_EQ(slot, redis_slot::HashSlot("player:" + tag + ":sync_digest"));
    }
}

// ── 真 Redis 用例:读 MMORPG_TEST_REDIS_ADDR(host:port,host 必须是 IP),未设就跳过 ──────────
// 只指向本机 / 专用的测试 Redis,不要指向共享或预发实例:用例会写并删除自己的键。
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

// 随机性只隔离集成夹具身份,不参与结局或时间断言;写前还会断言三键均未占用。
Guid IsolatedRedisPlayerIdForTest()
{
    std::random_device entropy;
    const auto value = (static_cast<uint64_t>(entropy()) << 32) | static_cast<uint64_t>(entropy());
    return value == 0 ? 1 : value;
}

// 同步连接,只用来布置与核对键。
struct SyncRedisForTest
{
    std::unique_ptr<redisContext, decltype(&redisFree)> context{nullptr, &redisFree};

    bool Connect(const std::string& host, uint16_t port)
    {
        context.reset(redisConnect(host.c_str(), port));
        return context != nullptr && context->err == 0;
    }

    // 键不存在时返回 "(nil)"。
    std::string Get(const std::string& key)
    {
        auto* reply = static_cast<redisReply*>(redisCommand(context.get(), "GET %b", key.data(), key.size()));
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
        Run(redisCommand(context.get(), "SET %b %b", key.data(), key.size(), value.data(), value.size()));
    }

    void Del(const std::string& key) { Run(redisCommand(context.get(), "DEL %b", key.data(), key.size())); }

    // 回整数;连接或命令出错返回 -1,让"应为 0"的断言出错时也红,而不是误判通过。
    long long Exists(const std::string& key)
    {
        return Integer(redisCommand(context.get(), "EXISTS %b", key.data(), key.size()));
    }

private:
    static void Run(void* reply)
    {
        if (reply != nullptr)
        {
            freeReplyObject(reply);
        }
    }

    static long long Integer(void* raw)
    {
        const auto* reply = static_cast<redisReply*>(raw);
        const long long value = (reply != nullptr && reply->type == REDIS_REPLY_INTEGER) ? reply->integer : -1;
        Run(raw);
        return value;
    }
};
} // namespace

// 交接标记脚本:调用方在最终存盘明确成功之后才发,脚本不看 ACK。
TEST(CurrencyTest, TravelPublishMarkDirectAgainstRealRedis)
{
    std::string host;
    uint16_t port = 0;
    if (!TestRedisAddr(host, port)) GTEST_SKIP() << "需要专用 MMORPG_TEST_REDIS_ADDR";
    SyncRedisForTest sync;
    ASSERT_TRUE(sync.Connect(host, port));
    const auto member = std::to_string(IsolatedRedisPlayerIdForTest());
    const std::string ownerKey = "player:{" + member + "}:owner_epoch";
    const std::string handoffKey = "player:{" + member + "}:handoff_fence";
    const std::string markKey = "player:{" + member + "}:handoff";
    ASSERT_EQ(0LL, sync.Exists(ownerKey));
    ASSERT_EQ(0LL, sync.Exists(handoffKey));
    ASSERT_EQ(0LL, sync.Exists(markKey));
    struct Cleanup
    {
        SyncRedisForTest& sync;
        const std::string& owner;
        const std::string& handoff;
        const std::string& mark;
        ~Cleanup() { sync.Del(owner); sync.Del(handoff); sync.Del(mark); }
    } cleanup{sync, ownerKey, handoffKey, markKey};
    using Reply = std::unique_ptr<redisReply, decltype(&freeReplyObject)>;
    const auto setFence = [&](const std::string& value) {
        Reply reply(static_cast<redisReply*>(redisCommand(sync.context.get(), "HSET %b handoff_fence %b",
            handoffKey.data(), handoffKey.size(), value.data(), value.size())), &freeReplyObject);
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type);
    };
    const std::string held = "5|sql-barrier-test|1|" + std::string(32, 'a') + "|H|" + std::string(32, 'b');
    const std::string published = "5|sql-barrier-test|1|" + std::string(32, 'a') + "|F|" + std::string(32, 'c');
    const auto publish = [&](const std::string& marker) {
        Reply reply(static_cast<redisReply*>(redisCommand(sync.context.get(), "EVAL %s 3 %b %b %b 5 %b 300 %b %b",
            handoff_sql_barrier::kPublishMarkDirectLua.c_str(), ownerKey.data(), ownerKey.size(), handoffKey.data(), handoffKey.size(),
            markKey.data(), markKey.size(), marker.data(), marker.size(), held.data(), held.size(), published.data(), published.size())), &freeReplyObject);
        if (reply == nullptr || reply->type != REDIS_REPLY_INTEGER) return -99LL;
        return static_cast<long long>(reply->integer);
    };
    sync.Set(ownerKey, "5");
    setFence(held);

    EXPECT_EQ(1LL, publish("5:1000")) << "Redis 里没有任何 ACK 字段也能发布";
    EXPECT_EQ("5:1000", sync.Get(markKey));
    EXPECT_EQ(1LL, publish("5:1000")) << "重放幂等";

    sync.Del(markKey);
    setFence(held);
    sync.Set(ownerKey, "6");
    EXPECT_EQ(-1LL, publish("5:1001")) << "归属已前进";
    EXPECT_EQ(0LL, sync.Exists(markKey));

    sync.Set(ownerKey, "5");
    setFence("5|sql-barrier-test|1|" + std::string(32, 'a') + "|H|" + std::string(32, 'd'));
    EXPECT_EQ(-2LL, publish("5:1002")) << "资格已被换掉";
    EXPECT_EQ(0LL, sync.Exists(markKey));
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
// 必需组件缺失必须 fail-closed(docs/design/ecs-entt-misuse-audit-20261015.md F025)
//
// PlayerCurrencyComp 只在玩家加载时挂上(欠款从 CurrencyComp.debts 装载)。修复前增减路径在
// 缺它时当场 get_or_emplace 一本空的继续记账:欠款被绕过、组件集合被悄悄改掉,
// 下一次存盘还会用空表覆盖盘上的欠款。修复后:拒绝(kInvalidParameter)、余额不动、不补建组件。
// ---------------------------------------------------------------------------

TEST(CurrencyTest, AddCurrencyMissingPlayerCurrencyCompFailsClosed)
{
    // 只有 CurrencyComp、没有 PlayerCurrencyComp:模拟加载不完整的实体。
    const auto player = tlsEcs.actorRegistry.create();
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player);

    // 修复前这三条都红:返回 kSuccess、余额变成 100、组件被补建出来。
    EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    EXPECT_EQ(0u, CurrencySystem::GetBalance(player, kCurrencyGold));
    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerCurrencyComp>(player));

    DestroyTestPlayer(player);
}

TEST(CurrencyTest, DeductCurrencyMissingPlayerCurrencyCompFailsClosed)
{
    auto player = CreateTestPlayer();
    ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player, kCurrencyGold, 100));
    tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player);

    // 修复前先扣钱、再补建组件:返回 kSuccess、余额 60、组件重新出现 —— 三条都红。
    EXPECT_EQ(kInvalidParameter, CurrencySystem::DeductCurrency(player, kCurrencyGold, 40));
    EXPECT_EQ(100u, CurrencySystem::GetBalance(player, kCurrencyGold));
    // ASSERT 而非 EXPECT:修复前组件已被补建,下面的 emplace 会触发 entt 断言把整个测试进程带崩。
    ASSERT_FALSE(tlsEcs.actorRegistry.any_of<PlayerCurrencyComp>(player));

    // 组件按加载路径补回来之后同一笔扣款照常成立:拒绝的原因确实只是缺组件。
    tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player);
    EXPECT_EQ(kSuccess, CurrencySystem::DeductCurrency(player, kCurrencyGold, 40));
    EXPECT_EQ(60u, CurrencySystem::GetBalance(player, kCurrencyGold));

    DestroyTestPlayer(player);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

int main(int argc, char** argv)
{
    // 资产通道物品用例必须使用真实配表，不因默认配置目录为空而全部跳过。
    if (!test_config::FindAndLoadTestConfig(argc, argv)) return 1;
    // 生产线程由 scene 的 ConfigureGuidSegmentClients 按配置启用 txlog 号段并经 DataService 领首段;
    // 单测没有那条启动链。给 txlog 种类装一个不回话的假传输并直接投一段大范围,
    // 避免交易流水因 tx_id 号段未就绪被 fail-closed 丢弃(那条路径由 bag_test 专门覆盖)。
    {
        auto &txlog = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog);
        GuidSegmentClient::Options options;
        options.mixIds = false;
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

TEST(CurrencyTest, HandoffQualificationCASRevokesLateWritersAndReplaysLostRepliesAgainstRealRedis)
{
    std::string host; uint16_t port=0;
    if (!TestRedisAddr(host,port)) GTEST_SKIP() << "需要专用MMORPG_TEST_REDIS_ADDR";
    SyncRedisForTest sync; ASSERT_TRUE(sync.Connect(host,port));
    const auto id=std::to_string(IsolatedRedisPlayerIdForTest());
    const std::string owner="player:{"+id+"}:owner_epoch", handoff="player:{"+id+"}:handoff_fence", mark="player:{"+id+"}:handoff";
    ASSERT_EQ(0LL,sync.Exists(owner));ASSERT_EQ(0LL,sync.Exists(handoff));ASSERT_EQ(0LL,sync.Exists(mark));
    struct Cleanup { SyncRedisForTest& sync;std::vector<std::string> keys;~Cleanup(){ for(const auto& key:keys) sync.Del(key); } } cleanup{sync,{owner,handoff,mark}};
    using Reply=std::unique_ptr<redisReply,decltype(&freeReplyObject)>;
    const auto command=[&](std::vector<std::string> args) {
        std::vector<const char*> values;std::vector<size_t> sizes;
        for(const auto& arg:args){values.push_back(arg.data());sizes.push_back(arg.size());}
        return Reply(static_cast<redisReply*>(redisCommandArgv(sync.context.get(),static_cast<int>(args.size()),values.data(),sizes.data())),&freeReplyObject);
    };
    const auto eval=[&](const std::string& lua,std::vector<std::string> args) {
        std::vector<std::string> all{"EVAL",lua,"3",owner,handoff,mark};all.insert(all.end(),args.begin(),args.end());
        return command(std::move(all));
    };
    const auto expectInteger=[&](const Reply& reply,long long value){ ASSERT_NE(nullptr,reply);ASSERT_EQ(REDIS_REPLY_INTEGER,reply->type);EXPECT_EQ(value,reply->integer); };
    const auto makeFence=[](std::string generation,char admission,char state,char token) {
        return "7|scene|"+generation+"|"+std::string(32,admission)+"|"+state+"|"+std::string(32,token);
    };
    const auto held=makeFence("1",'a','H','b'), frozen=makeFence("1",'a','F','c'), revoked=makeFence("1",'a','R','d');
    const auto resumed=makeFence("1",'a','H','e');
    sync.Set(owner,"7");
    expectInteger(eval(handoff_sql_barrier::kRegisterFenceLua,{"7",held,""}),1);
    // 原写帧在另一连接/队列中尚未抵达：mark缺席仍必须撤销，迟到SET不得复活。
    expectInteger(eval(handoff_sql_barrier::kRevokeFenceLua,{held,frozen,revoked,"1000"}),1);
    // -2 = 原 H 已被轮换(2026-10-23 起与 owner 已推进的 0 分开)。
    expectInteger(eval(handoff_sql_barrier::kReleaseMarkLua,{"7","7:1000","300",held,frozen}),-2);
    EXPECT_EQ(0LL,sync.Exists(mark));
    auto location=owner+":location-fixture";ASSERT_EQ(0LL,sync.Exists(location));cleanup.keys.push_back(location);
    const auto resume=[&]{return command({"EVAL",handoff_sql_barrier::kResumeFenceLua,"4",owner,handoff,mark,location,revoked,resumed,"7",""});};
    // 忽略第一次已提交应答，第二次只能重放完全相同H，不能临时重造资格。
    auto lost=resume();ASSERT_NE(nullptr,lost);ASSERT_EQ(REDIS_REPLY_INTEGER,lost->type);ASSERT_EQ(1,lost->integer);lost.reset();
    expectInteger(resume(),1);
    expectInteger(eval(handoff_sql_barrier::kReleaseMarkLua,{"7","7:1000","300",held,frozen}),-2);
    const auto newer=makeFence("2",'f','H','1');
    expectInteger(eval(handoff_sql_barrier::kRegisterFenceLua,{"7",newer,resumed}),1);
    expectInteger(eval(handoff_sql_barrier::kRegisterFenceLua,{"7",held,newer}),-2); // 旧N读到新prior仍不可接任
    const auto before=command({"HGET",handoff,"handoff_fence"});ASSERT_NE(nullptr,before);ASSERT_EQ(REDIS_REPLY_STRING,before->type);
    sync.Set(mark,"malformed");
    auto bad=eval(handoff_sql_barrier::kRevokeFenceLua,{newer,newer,makeFence("2",'f','R','2'),"1000"});
    ASSERT_NE(nullptr,bad);EXPECT_EQ(REDIS_REPLY_ERROR,bad->type);
    auto after=command({"HGET",handoff,"handoff_fence"});ASSERT_NE(nullptr,after);ASSERT_EQ(REDIS_REPLY_STRING,after->type);
    EXPECT_EQ(std::string(before->str,before->len),std::string(after->str,after->len));
    EXPECT_EQ("malformed",sync.Get(mark)) << "损坏mark不准先写R再报错";
}

// r2-lc#1 / r2-lc#2(data-overwrite-rollback-audit-r2-20261019.md §3.6;cross-zone-scene-travel.md 文末「迟到标记写」专节):
// zone Redis 存活探测(RedisManager::DropUnresponsive)摘掉无应答的旧连接后,内核仍可能把旧连接上未确认的 EVAL 重传到 Redis,
// 晚于新连接上的取证 / 同节点重载登记才执行。这里用两条真实连接复现这个到达顺序:旧连接只 redisAppendCommandArgv(帧留在
// 本地输出缓冲、尚未发出),新连接先跑完取证 / 重载,再 redisGetReply 把旧帧真正发出去。断言迟到的标记写被
// handoff_fence 的原件 CAS 拒绝:新持有期间 handoff 键没有当前代际的标记,资格仍是新持有者的。
// 不依赖墙钟、时钟同步或连接顺序;Lua 层的同序重放与 scene_manager 消费方断言见
// Go TestLateMarkWritesFromReplacedConnectionNeverOpenHandoffGate。
TEST(CurrencyTest, HandoffMarkFramesDelayedOnReplacedConnectionCannotReviveCredentialAgainstRealRedis)
{
    std::string host;
    uint16_t port = 0;
    if (!TestRedisAddr(host, port)) GTEST_SKIP() << "需要专用 MMORPG_TEST_REDIS_ADDR";
    SyncRedisForTest live;  // 存活探测重连之后的新连接
    SyncRedisForTest stale; // 被摘下的旧连接:帧还在内核里重传,稍后才到 Redis
    ASSERT_TRUE(live.Connect(host, port));
    ASSERT_TRUE(stale.Connect(host, port));
    using Reply = std::unique_ptr<redisReply, decltype(&freeReplyObject)>;
    struct Argv
    {
        std::vector<const char*> values;
        std::vector<size_t> sizes;
        explicit Argv(const std::vector<std::string>& args)
        {
            for (const auto& arg : args) { values.push_back(arg.data()); sizes.push_back(arg.size()); }
        }
    };
    const auto send = [](SyncRedisForTest& connection, const std::vector<std::string>& args) {
        Argv argv(args);
        return Reply(static_cast<redisReply*>(redisCommandArgv(connection.context.get(), static_cast<int>(args.size()),
            argv.values.data(), argv.sizes.data())), &freeReplyObject);
    };
    // 只进本地输出缓冲、不发出:模拟"已交给内核、尚未到达 Redis"的旧帧。
    const auto hold = [](SyncRedisForTest& connection, const std::vector<std::string>& args) {
        Argv argv(args);
        return redisAppendCommandArgv(connection.context.get(), static_cast<int>(args.size()),
            argv.values.data(), argv.sizes.data());
    };
    // 把旧帧真正发出去并读它的应答。
    const auto deliver = [](SyncRedisForTest& connection) {
        void* raw = nullptr;
        if (redisGetReply(connection.context.get(), &raw) != REDIS_OK) raw = nullptr;
        return Reply(static_cast<redisReply*>(raw), &freeReplyObject);
    };
    const auto expectInteger = [](const Reply& reply, long long value) {
        ASSERT_NE(nullptr, reply);
        ASSERT_EQ(REDIS_REPLY_INTEGER, reply->type) << (reply->type == REDIS_REPLY_ERROR ? reply->str : "");
        EXPECT_EQ(value, reply->integer);
    };
    const auto fenceOf = [&](const std::string& handoff) {
        auto reply = send(live, {"HGET", handoff, "handoff_fence"});
        return reply != nullptr && reply->type == REDIS_REPLY_STRING ? std::string(reply->str, reply->len) : std::string("(nil)");
    };
    // E=5、同一场景实例 S;generation / admission / state / token 由各步给出。
    const auto makeFence = [](const std::string& generation, char admission, char state, char token) {
        return "5|scene-x|" + generation + "|" + std::string(32, admission) + "|" + state + "|" + std::string(32, token);
    };
    struct Keys
    {
        std::string owner, handoff, mark, location;
    };
    const auto isolatedKeys = [] {
        const auto id = std::to_string(IsolatedRedisPlayerIdForTest());
        return Keys{"player:{" + id + "}:owner_epoch", "player:{" + id + "}:handoff_fence", "player:{" + id + "}:handoff",
                    "player:{" + id + "}:location-fixture"};
    };
    struct Cleanup
    {
        SyncRedisForTest& sync;
        Keys keys;
        ~Cleanup() { sync.Del(keys.owner); sync.Del(keys.handoff); sync.Del(keys.mark); sync.Del(keys.location); }
    };

    // r2-lc#1:BeginTravelHandoff 的发布帧晚于新连接上判 kUnchanged 的取证与解冻。
    {
        const Keys k = isolatedKeys();
        ASSERT_EQ(0LL, live.Exists(k.owner)); ASSERT_EQ(0LL, live.Exists(k.handoff));
        ASSERT_EQ(0LL, live.Exists(k.mark)); ASSERT_EQ(0LL, live.Exists(k.location));
        Cleanup cleanup{live, k};
        const std::string locationBytes = "location-bytes-on-this-node";
        const auto held = makeFence("1", 'a', 'H', 'b');
        const auto published = makeFence("1", 'a', 'F', 'c');
        const auto revoked = makeFence("1", 'a', 'R', 'd');
        const auto resumed = makeFence("1", 'a', 'H', 'e');
        live.Set(k.owner, "5");
        live.Set(k.location, locationBytes);
        expectInteger(send(live, {"HSET", k.handoff, "handoff_fence", held}), 1);
        // ① 冻结快照已提交,在旧连接上发 kPublishMarkDirectLua("5:1000"),帧还没到 Redis。
        ASSERT_EQ(REDIS_OK, hold(stale, {"EVAL", handoff_sql_barrier::kPublishMarkDirectLua, "3", k.owner, k.handoff, k.mark,
                                         "5", "5:1000", "300", held, published}));
        // ② 存活探测摘连接,回调收到空应答 → kMarkWriteUnknown;重连后看门狗在新连接上取证:标记尚未到达、owner 未变。
        auto judged = send(live, {"EVAL", handoff_sql_barrier::kJudgeOutcomeLua, "4", k.owner, k.handoff, k.mark, k.location,
                                  held, published, revoked, "1000"});
        ASSERT_NE(nullptr, judged);
        ASSERT_EQ(REDIS_REPLY_ARRAY, judged->type) << (judged->type == REDIS_REPLY_ERROR ? judged->str : "");
        ASSERT_EQ(2u, judged->elements);
        ASSERT_EQ(REDIS_REPLY_STRING, judged->element[0]->type);
        EXPECT_EQ("5", std::string(judged->element[0]->str, judged->element[0]->len)) << "kUnchanged:owner 仍是 E";
        // ③ ResumeTravelAfterRevocation:R → 新 H,确认后才解冻。
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kResumeFenceLua, "4", k.owner, k.handoff, k.mark, k.location,
                                  revoked, resumed, "5", locationBytes}), 1);
        // ④ 旧帧此刻才到:原 H 已不是现任资格,必须被拒。
        expectInteger(deliver(stale), -2);
        EXPECT_EQ(0LL, live.Exists(k.mark)) << "解冻后的新持有期间不得出现当前代际的有效标记";
        EXPECT_EQ(resumed, fenceOf(k.handoff));
        // 新持有者自己的下一次交接照常:围栏只拒旧资格,不伤活性。
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kPublishMarkDirectLua, "3", k.owner, k.handoff, k.mark,
                                  "5", "5:2000", "300", resumed, makeFence("1", 'a', 'F', 'f')}), 1);
        EXPECT_EQ("5:2000", live.Get(k.mark));
    }

    // r2-lc#2:A1′ 的条件写帧晚于同节点短重连的登记与确认(同节点不铸新 epoch)。A2′ 放弃补写、改派与改派踢线前补写
    // 走同一段 kReleaseMarkLua、只消费各自缓存的原 H,迟到时同样被拒。
    {
        const Keys k = isolatedKeys();
        ASSERT_EQ(0LL, live.Exists(k.owner)); ASSERT_EQ(0LL, live.Exists(k.handoff)); ASSERT_EQ(0LL, live.Exists(k.mark));
        Cleanup cleanup{live, k};
        const auto held = makeFence("1", 'a', 'H', 'b');
        const auto released = makeFence("1", 'a', 'F', 'c');
        const auto reloaded = makeFence("2", '7', 'H', '8'); // SQL 确认的新 admission generation,同 E、同 S
        live.Set(k.owner, "5");
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kRegisterFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", held, ""}), 1);
        // ① 退出存盘落地,在旧连接上发 A1′("5:1000"),帧还没到 Redis;实体随即销毁。
        ASSERT_EQ(REDIS_OK, hold(stale, {"EVAL", handoff_sql_barrier::kReleaseMarkLua, "3", k.owner, k.handoff, k.mark,
                                         "5", "5:1000", "300", held, released}));
        // ② 玩家短重连回同一节点:新连接上登记 N=2(原子清继承标记)并确认。
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kRegisterFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", reloaded, held}), 1);
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kConfirmFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", reloaded}), 0);
        // ③ 旧 A1′ 此刻才到:-2 = 原 H 已被新登记轮换,未写。
        expectInteger(deliver(stale), -2);
        EXPECT_EQ(0LL, live.Exists(k.mark)) << "新持有期间不得出现当前代际的有效标记";
        EXPECT_EQ(reloaded, fenceOf(k.handoff));
    }

    // 会话结束凭证(location-release-binding.md §3.3,2026-10-23):本节点完全不持有时没有缓存的原 H,先 HGET 读现任原件、
    // 在 scene 线程核对(本进程实例、期望 E、仍完全不持有)之后以它 CAS(kCredentialMarkLua)。这里按 Redis 实际执行顺序验:
    // (a) 原件是 F(A1′ 写过、标记已过期)也能写出凭证 —— §15 起等待超时不再授权删 location,不写就永远删不掉;
    // (b) 读到原件之后 CAS 帧滞留在旧连接,期间同节点短重连登记新 N:迟到的凭证帧回 -2、不写;
    // (c) 原件是 R(登记已落地又被撤销,继承标记已被登记删掉)也能写出凭证;(d) owner 已推进时回 0。
    {
        const Keys k = isolatedKeys();
        ASSERT_EQ(0LL, live.Exists(k.owner)); ASSERT_EQ(0LL, live.Exists(k.handoff)); ASSERT_EQ(0LL, live.Exists(k.mark));
        Cleanup cleanup{live, k};
        const auto held = makeFence("1", 'a', 'H', 'b');
        const auto released = makeFence("1", 'a', 'F', 'c');
        live.Set(k.owner, "5");
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kRegisterFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", held, ""}), 1);
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kReleaseMarkLua, "3", k.owner, k.handoff, k.mark,
                                  "5", "5:1000", "300", held, released}), 1);
        live.Del(k.mark); // A1′ 的标记 TTL 到期

        // (a)
        const std::string observedFrozen = fenceOf(k.handoff);
        ASSERT_EQ(released, observedFrozen);
        const auto credential = makeFence("1", 'a', 'F', 'd');
        const std::vector<std::string> credentialWrite{"EVAL", handoff_sql_barrier::kCredentialMarkLua, "3", k.owner,
                                                       k.handoff, k.mark, "5", "5:2000", "300", observedFrozen, credential};
        expectInteger(send(live, credentialWrite), 1);
        EXPECT_EQ("5:2000", live.Get(k.mark)) << "持有节点活着而不持有:凭证让 LeaveScene / 换手门拿到 E 代释放证据";
        EXPECT_EQ(credential, fenceOf(k.handoff));
        expectInteger(send(live, credentialWrite), 1); // 同一 F 已在:幂等
        EXPECT_EQ("5:2000", live.Get(k.mark));

        // (b)
        live.Del(k.mark);
        const std::string observed = fenceOf(k.handoff);
        ASSERT_EQ(credential, observed);
        ASSERT_EQ(REDIS_OK, hold(stale, {"EVAL", handoff_sql_barrier::kCredentialMarkLua, "3", k.owner, k.handoff, k.mark,
                                         "5", "5:3000", "300", observed, makeFence("1", 'a', 'F', 'e')}));
        const auto reloaded = makeFence("2", '7', 'H', '8');
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kRegisterFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", reloaded, observed}), 1);
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kConfirmFenceLua, "3", k.owner, k.handoff, k.mark,
                                  "5", reloaded}), 0);
        expectInteger(deliver(stale), -2);
        EXPECT_EQ(0LL, live.Exists(k.mark)) << "新持有期间迟到的凭证帧不得留下当前代际的有效标记";
        EXPECT_EQ(reloaded, fenceOf(k.handoff));

        // (c)
        const auto revokedReload = makeFence("2", '7', 'R', 'a');
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kCancelRegistrationLua, "3", k.owner, k.handoff, k.mark,
                                  "5", reloaded, observed, revokedReload}), 1);
        const std::string observedRevoked = fenceOf(k.handoff);
        ASSERT_EQ(revokedReload, observedRevoked);
        const auto credentialAfterRevoke = makeFence("2", '7', 'F', 'b');
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kCredentialMarkLua, "3", k.owner, k.handoff, k.mark,
                                  "5", "5:3500", "300", observedRevoked, credentialAfterRevoke}), 1);
        EXPECT_EQ("5:3500", live.Get(k.mark));

        // (d)
        live.Del(k.mark);
        live.Set(k.owner, "6"); // 跨节点接手铸了 E+1
        expectInteger(send(live, {"EVAL", handoff_sql_barrier::kCredentialMarkLua, "3", k.owner, k.handoff, k.mark,
                                  "5", "5:4000", "300", credentialAfterRevoke, makeFence("2", '7', 'F', 'c')}), 0);
        EXPECT_EQ(0LL, live.Exists(k.mark));
        EXPECT_EQ(credentialAfterRevoke, fenceOf(k.handoff));
    }
}
