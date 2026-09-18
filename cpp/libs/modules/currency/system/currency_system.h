#pragma once

#include <string>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/currency/constants/currency.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/rollback/transaction_log.pb.h" // TransactionType — 流水分类随调用点传入

// Stateless system that handles all currency mutations.
// Every add/deduct goes through this system — no direct field writes.
//
// ⚠️ DEFERRED-CLAWBACK INVARIANT (do not break):
//   `AddCurrency` is the SINGLE unified currency-credit entry point. The
//   补缴 (deferred clawback) hook in currency_system.cpp lines 95-128
//   intercepts here. If you grant currency by:
//     • calling `currency.mutable_values()->Set(idx, x)` directly
//     • assigning to a `*balance = ...` pointer obtained outside this system
//     • bypassing this class via Kafka writes that overwrite CurrencyComp
//   the clawback hook is silently bypassed and any active debts won't deduct.
//
//   If you genuinely need a non-clawback path (e.g. snapshot restore), use
//   the explicit TX_ROLLBACK_RESTORE / TX_SYSTEM_GRANT pathways and document
//   why clawback should not apply to that grant.
//
//   Audit history (see docs/ops/deferred-clawback-bypass-audit-2026-05.md):
//     • 2026-05-15: grep -r "mutable_values\(\)" cpp/ → 2 hits, both in
//       currency_system.cpp (EnsureCurrencySlots init, ResolveCurrencyField
//       private helper). 0 external callers. Invariant holds.
class CurrencySystem
{
public:
    // Add currency to a player. amount must be > 0; returns error code otherwise.
    // Automatically deducts from any outstanding debt (补缴) before crediting.
    // Writes a TransactionLogEntry to Kafka.
    //
    // txType / correlationId 落进流水(guild-phase2.md §S4 4.11)。**两个都有默认值**,
    // 现有调用点一行不用改;通用资产通道按流的 tx 白名单传入具体类型,correlationId
    // 传帮会 op_id / 聚宝斋 listing_id,事后能把"这笔钱"和"哪次业务操作"对上。
    //
    // 返回值口径(§4.10,与 kInvalidParameter 分家):
    //   冻结      -> kAssetFrozen              (RETRY 类:条件会消失,调用方可重投)
    //   封禁      -> kAssetBlocked             (REJECTED 类:终局拒绝)
    //   amount<=0 / 类型越界 / 缺组件 -> kInvalidParameter(编程错误,不变)
    static uint32_t AddCurrency(entt::entity player, CurrencyType type, int64_t amount,
                                TransactionType txType = TX_CURRENCY_ADD,
                                uint64_t correlationId = 0);

    // Deduct currency from a player. amount must be > 0; returns error code if
    // the parameter is invalid or the player has insufficient funds.
    // Writes a TransactionLogEntry to Kafka.
    //
    // 返回值口径(§4.10):冻结 -> kAssetFrozen;余额不足 -> kAssetCurrencyInsufficient;
    // amount<=0 / 类型越界 / 缺组件 -> kInvalidParameter(不变)。
    static uint32_t DeductCurrency(entt::entity player, CurrencyType type, int64_t amount,
                                   TransactionType txType = TX_CURRENCY_DEDUCT,
                                   uint64_t correlationId = 0);

    // Query current balance (returns 0 if component missing).
    static uint64_t GetBalance(entt::entity player, CurrencyType type);

    // Check whether the player can afford `amount` of `type`.
    static bool CanAfford(entt::entity player, CurrencyType type, int64_t amount);

    // ── Deferred Clawback (补缴) — GM debt management ───────────────────

    // Attach a deferred-clawback debt.  Future AddCurrency calls will
    // automatically deduct from gains until the debt is repaid.
    static void AttachDebt(entt::entity player, CurrencyType type, int64_t oweAmount,
                           const std::string &reason = {},
                           const std::string &gmOperator = {},
                           uint64_t expiresAt = 0);

    // Write off all remaining debt for the given currency type.
    static uint64_t WaiveDebt(entt::entity player, CurrencyType type,
                              const std::string &gmOperator = {},
                              const std::string &reason = {});

    // Adjust the owed amount by `delta` (positive = increase, negative = decrease).
    // Clamps so that owed never goes below paid.
    static void AdjustDebt(entt::entity player, CurrencyType type, int64_t delta,
                           const std::string &gmOperator = {},
                           const std::string &reason = {});

    // Freeze or unfreeze auto-deduction for a currency type's debt.
    static void FreezeDebt(entt::entity player, CurrencyType type, bool freeze,
                           const std::string &gmOperator = {});

    // Return all active debts for the player (for GM query).
    static std::vector<CurrencyDebtEntry> QueryDebts(entt::entity player);

    // ── GM block/unblock ─────────────────────────────────────────────────

    // GM: block a currency type — all future AddCurrency calls for this type will be rejected.
    static uint32_t BlockCurrency(entt::entity player, CurrencyType type);

    // GM: unblock a previously blocked currency type.
    static uint32_t UnblockCurrency(entt::entity player, CurrencyType type);

    // Query whether a currency type is currently blocked.
    static bool IsCurrencyBlocked(entt::entity player, CurrencyType type);

private:
    // Ensure proto slot array has at least kCurrencyMax elements.
    static void EnsureCurrencySlots(CurrencyComp &currency);

    // Resolve the uint64 slot pointer from proto CurrencyComp.
    // Returns nullptr + logs error when type is out of range.
    static uint64_t *ResolveCurrencyField(entt::entity player, CurrencyType type);
};
