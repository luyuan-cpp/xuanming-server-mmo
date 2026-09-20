#include "currency_system.h"

#include <algorithm>
#include <limits>

#include <muduo/base/Logging.h>

#include "engine/core/error_handling/error_handling.h"
#include "engine/core/time/system/time.h"
#include "table/proto/tip/asset_error_tip.pb.h" // kAssetFrozen / kAssetBlocked / kAssetCurrencyInsufficient
#include "table/proto/tip/common_error_tip.pb.h"
#include "core/utils/registry/game_registry.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "modules/transaction_log/transaction_log_system.h"
#include "proto/common/component/currency_comp.pb.h"
#include "services/scene/player/system/player_lifecycle.h" // IsCrossZoneFrozen — see cross-zone-readiness-audit.md §11.1
#include <ecs_context.h>

// ---------------------------------------------------------------------------
// Internal helper
// ---------------------------------------------------------------------------

namespace
{

// 币种是否在枚举范围内。**纯判定,零副作用、不碰 ECS**,所以允许排在业务条件
// (冻结 / 封禁)之前 —— 而且必须排在前面:越界币种是调用方写错了,永远不可能
// 成功;若让冻结先命中,资产通道会拿到 RETRY 类的 kAssetFrozen,对一个注定失败的
// 请求无限重投(04-asset-channel.md §4.10 的 RETRY / REJECTED 分类)。
// 这与背包侧"纯预检必须全部跑在 ReserveOrEvict 之前"是同一条纪律。
bool IsKnownCurrencyType(CurrencyType type)
{
    return static_cast<uint32_t>(type) < static_cast<uint32_t>(kCurrencyMax);
}

} // namespace

void CurrencySystem::EnsureCurrencySlots(CurrencyComp &currency)
{
    auto *values = currency.mutable_values();
    while (values->size() < static_cast<int>(kCurrencyMax))
    {
        values->Add(0);
    }
}

uint64_t *CurrencySystem::ResolveCurrencyField(entt::entity player, CurrencyType type)
{
    if (!IsKnownCurrencyType(type))
    {
        LOG_ERROR << "CurrencySystem: unknown CurrencyType=" << static_cast<uint32_t>(type)
                  << entt::to_integral(player);
        return nullptr;
    }

    auto *currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    if (currency == nullptr)
    {
        LOG_ERROR << "CurrencySystem: CurrencyComp missing on entity "
                  << entt::to_integral(player);
        return nullptr;
    }

    EnsureCurrencySlots(*currency);
    auto *values = currency->mutable_values();
    return &(*values)[static_cast<uint32_t>(type)];
}

// ---------------------------------------------------------------------------
// AddCurrency — the single unified entry point for all currency gains.
// ---------------------------------------------------------------------------

uint32_t CurrencySystem::AddCurrency(entt::entity player, CurrencyType type, int64_t amount,
                                     TransactionType txType, uint64_t correlationId)
{
    // ── Strict parameter validation ──────────────────────────────────────
    if (amount <= 0)
    {
        LOG_ERROR << "CurrencySystem::AddCurrency: amount must be > 0, got "
                  << amount << " for CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    // 币种越界与 amount<=0 同类,都是编程错误,必须在冻结 / 封禁这些**会自己消失或
    // 需要记账**的业务条件之前判掉(理由见 IsKnownCurrencyType 注释)。
    if (!IsKnownCurrencyType(type))
    {
        LOG_ERROR << "CurrencySystem::AddCurrency: unknown CurrencyType="
                  << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    // ── Cross-zone Frozen check (Single Writer guarantee) ───────────────
    // 冻结 = 归属交接在途(PlayerLifecycleSystem::StartTravelHandoff:跨 zone 传送,或同 zone
    // 跨节点换图)。PlayerFrozenComp 只与 PlayerTravelHandoffComp 成对出现;已没有 Kafka
    // 迁移包、目的端 ACK 与 reaper —— 目标节点从盘上加载,依据就是冻结那一次存盘。
    // 此时 AddCurrency 只会写进源端 CurrencyComp:handoff 标记写出后 SavePlayerToRedis 直接
    // 跳过,scene_manager 放行后实体随 DestroyDeposedPlayer 不存盘销毁,目标节点永远读不到。
    // 交接未成则由 AbortTravelHandoff 解冻,之后可正常写。
    // 写入类拦截目录见 docs/design/cross-zone-readiness-audit.md §11.1(该文只有 §11 仍有效)。
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
    {
        LOG_WARN << "CurrencySystem::AddCurrency rejected: player frozen for cross-zone migration. "
                 << "CurrencyType=" << static_cast<uint32_t>(type)
                 << " amount=" << amount
                 << " entity=" << entt::to_integral(player);
        // 冻结是**会自己消失**的条件,与"参数写错了"根本不是一类。回 kInvalidParameter
        // 时调用方无法区分二者,资产通道只能一律当终局拒绝,于是玩家过图的那几百毫秒里
        // 到达的帮会发奖会被永久丢掉(guild-phase2.md §S4 4.10)。
        return PrintStackAndReturnError(kAssetFrozen);
    }

    // ── Global (server-wide) block check ────────────────────────────────
    if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kCurrency,
                                        static_cast<uint32_t>(type)))
    {
        LOG_WARN << "CurrencySystem::AddCurrency: currency GLOBALLY blocked. CurrencyType="
                 << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        // 封禁是终局拒绝(REJECTED 类):资产通道据此记账并让业务侧退款,不重投。
        return PrintStackAndReturnError(kAssetBlocked);
    }

    // ── Per-player GM block check ────────────────────────────────────────
    if (IsCurrencyBlocked(player, type))
    {
        LOG_WARN << "CurrencySystem::AddCurrency: currency blocked by GM. CurrencyType="
                 << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kAssetBlocked);
    }

    uint64_t gain = static_cast<uint64_t>(amount);

    uint64_t *balance = ResolveCurrencyField(player, type);
    if (balance == nullptr)
    {
        return PrintStackAndReturnError(kInvalidParameter);
    }

    const uint64_t balanceBefore = *balance;

    // ── Deferred-clawback / 补缴 deduction ──────────────────────────────
    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        comp = &tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player);
    }
    auto it = comp->debts.find(static_cast<uint32_t>(type));
    if (it != comp->debts.end())
    {
        auto &debt = it->second;
        uint64_t remaining = debt.Remaining();
        // Only deduct if the debt is active (not frozen and not expired).
        const bool frozen = debt.frozen;
        const bool expired = (debt.expiresAt > 0 && TimeSystem::NowSecondsUTC() >= debt.expiresAt);
        if (remaining > 0 && !frozen && !expired)
        {
            uint64_t deduct = std::min(gain, remaining);
            debt.paid += deduct;
            gain -= deduct;

            LOG_INFO << "CurrencySystem: deferred clawback deducted " << deduct
                     << " from gain, debt remaining=" << debt.Remaining()
                     << " CurrencyType=" << static_cast<uint32_t>(type)
                     << " entity=" << entt::to_integral(player);

            // Emit clawback tx log entry.
            TransactionLogSystem::LogClawbackDeduction(player, type, deduct, debt.Remaining());

            if (debt.Remaining() <= 0)
            {
                comp->debts.erase(it);
            }
        }
    }

    // ── Credit remaining amount after debt ───────────────────────────────
    if (gain > 0)
    {
        *balance += gain;
    }

    comp->dirty = true;

    // ── Transaction log ──────────────────────────────────────────────────
    TransactionLogSystem::LogCurrencyAdd(player, type,
                                         static_cast<uint64_t>(amount), balanceBefore, *balance,
                                         txType, correlationId);

    // ── Anomaly detection ────────────────────────────────────────────────
    AnomalyDetector::RecordCurrencyGain(player, type, static_cast<uint64_t>(amount));

    return kSuccess;
}

// ---------------------------------------------------------------------------
// DeductCurrency
// ---------------------------------------------------------------------------

uint32_t CurrencySystem::DeductCurrency(entt::entity player, CurrencyType type, int64_t amount,
                                        TransactionType txType, uint64_t correlationId)
{
    // ── Strict parameter validation ──────────────────────────────────────
    if (amount <= 0)
    {
        LOG_ERROR << "CurrencySystem::DeductCurrency: amount must be > 0, got "
                  << amount << " for CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    // 同 AddCurrency:纯参数校验先跑,免得冻结中的玩家传一个越界币种时拿到 RETRY 类的
    // kAssetFrozen —— 那是一个解冻之后照样失败的请求,却会被资产通道一直重投到超时。
    if (!IsKnownCurrencyType(type))
    {
        LOG_ERROR << "CurrencySystem::DeductCurrency: unknown CurrencyType="
                  << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    // ── Cross-zone Frozen check (Single Writer guarantee) ───────────────
    // 理由同上面的 AddCurrency:归属交接在途,这笔扣款到不了目标节点 —— 放行后源端实体
    // 由 DestroyDeposedPlayer 不存盘销毁(已没有 ACK)。
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
    {
        LOG_WARN << "CurrencySystem::DeductCurrency rejected: player frozen for cross-zone migration. "
                 << "CurrencyType=" << static_cast<uint32_t>(type)
                 << " amount=" << amount
                 << " entity=" << entt::to_integral(player);
        // 同 AddCurrency:RETRY 类,调用方重投即可,不是终局拒绝。
        return PrintStackAndReturnError(kAssetFrozen);
    }

    uint64_t cost = static_cast<uint64_t>(amount);

    uint64_t *balance = ResolveCurrencyField(player, type);
    if (balance == nullptr)
    {
        return PrintStackAndReturnError(kInvalidParameter);
    }

    if (*balance < cost)
    {
        LOG_WARN << "CurrencySystem::DeductCurrency: insufficient funds. balance="
                 << *balance << " requested=" << cost
                 << " CurrencyType=" << static_cast<uint32_t>(type)
                 << " entity=" << entt::to_integral(player);
        // 余额不足是终局拒绝,而且是**玩家看得懂的那一个**:回 kInvalidParameter 时
        // 客户端只能弹"参数错误",帮会捐献失败的真实原因就此丢失。
        return PrintStackAndReturnError(kAssetCurrencyInsufficient);
    }

    const uint64_t balanceBefore = *balance;
    *balance -= cost;

    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        comp = &tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player);
    }
    comp->dirty = true;

    // ── Transaction log ──────────────────────────────────────────────────
    TransactionLogSystem::LogCurrencyDeduct(player, type, cost, balanceBefore, *balance,
                                            txType, correlationId);

    return kSuccess;
}

// ---------------------------------------------------------------------------
// GetBalance
// ---------------------------------------------------------------------------

uint64_t CurrencySystem::GetBalance(entt::entity player, CurrencyType type)
{
    const uint64_t *balance = ResolveCurrencyField(player, type);
    return balance ? *balance : 0;
}

// ---------------------------------------------------------------------------
// CanAfford
// ---------------------------------------------------------------------------

bool CurrencySystem::CanAfford(entt::entity player, CurrencyType type, int64_t amount)
{
    if (amount <= 0)
    {
        LOG_ERROR << "CurrencySystem::CanAfford: amount must be > 0, got "
                  << amount << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return false;
    }
    return GetBalance(player, type) >= static_cast<uint64_t>(amount);
}

// ---------------------------------------------------------------------------
// AttachDebt (补缴)
// ---------------------------------------------------------------------------

void CurrencySystem::AttachDebt(entt::entity player, CurrencyType type, int64_t oweAmount,
                                const std::string &reason,
                                const std::string &gmOperator,
                                uint64_t expiresAt)
{
    if (oweAmount <= 0)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: oweAmount must be > 0, got "
                  << oweAmount << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return;
    }

    uint64_t debtToAdd = static_cast<uint64_t>(oweAmount);

    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: PlayerCurrencyComp missing on entity "
                  << entt::to_integral(player);
        return;
    }

    auto &debt = comp->debts[static_cast<uint32_t>(type)];
    if (debt.owed > std::numeric_limits<uint64_t>::max() - debtToAdd)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: owed overflow rejected, current=" << debt.owed
                  << " add=" << debtToAdd
                  << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return;
    }
    debt.owed += debtToAdd;
    debt.reason = reason;
    debt.gmOperator = gmOperator;
    debt.expiresAt = expiresAt;
    if (debt.createdAt == 0)
    {
        debt.createdAt = TimeSystem::NowSecondsUTC();
    }

    comp->dirty = true;

    LOG_INFO << "CurrencySystem: debt attached, total owed=" << debt.owed
             << " paid=" << debt.paid
             << " CurrencyType=" << static_cast<uint32_t>(type)
             << " entity=" << entt::to_integral(player);
}

// ---------------------------------------------------------------------------
// WaiveDebt — write off remaining debt
// ---------------------------------------------------------------------------

uint64_t CurrencySystem::WaiveDebt(entt::entity player, CurrencyType type,
                                   const std::string &gmOperator,
                                   const std::string &reason)
{
    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        return 0;
    }

    auto it = comp->debts.find(static_cast<uint32_t>(type));
    if (it == comp->debts.end())
    {
        return 0;
    }

    const uint64_t waived = it->second.Remaining();
    comp->debts.erase(it);
    comp->dirty = true;

    LOG_INFO << "CurrencySystem: debt waived, amount=" << waived
             << " CurrencyType=" << static_cast<uint32_t>(type)
             << " operator=" << gmOperator
             << " reason=" << reason
             << " entity=" << entt::to_integral(player);
    return waived;
}

// ---------------------------------------------------------------------------
// AdjustDebt — increase or decrease the owed amount
// ---------------------------------------------------------------------------

void CurrencySystem::AdjustDebt(entt::entity player, CurrencyType type, int64_t delta,
                                const std::string &gmOperator,
                                const std::string &reason)
{
    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        LOG_ERROR << "CurrencySystem::AdjustDebt: PlayerCurrencyComp missing on entity "
                  << entt::to_integral(player);
        return;
    }

    auto it = comp->debts.find(static_cast<uint32_t>(type));
    if (it == comp->debts.end())
    {
        if (delta <= 0)
        {
            return; // Nothing to decrease
        }
        // Create new debt
        CurrencyDebt newDebt;
        newDebt.owed = static_cast<uint64_t>(delta);
        newDebt.createdAt = TimeSystem::NowSecondsUTC();
        newDebt.reason = reason;
        newDebt.gmOperator = gmOperator;
        comp->debts[static_cast<uint32_t>(type)] = std::move(newDebt);
        comp->dirty = true;
        return;
    }

    auto &debt = it->second;
    if (delta > 0)
    {
        const uint64_t increase = static_cast<uint64_t>(delta);
        if (debt.owed > std::numeric_limits<uint64_t>::max() - increase)
        {
            LOG_ERROR << "CurrencySystem::AdjustDebt: owed overflow rejected, current=" << debt.owed
                      << " add=" << increase
                      << " CurrencyType=" << static_cast<uint32_t>(type)
                      << " entity=" << entt::to_integral(player);
            return;
        }
        debt.owed += increase;
    }
    else
    {
        // -delta 对 INT64_MIN 是有符号溢出(UB),先在 uint64 域里取绝对值。
        const uint64_t decrease = (delta == std::numeric_limits<int64_t>::min())
                                      ? (static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) + 1u)
                                      : static_cast<uint64_t>(-delta);

        // 钳制:owed 不得低于 paid。
        //
        // 判定里**不能出现 `debt.owed - decrease`** —— decrease > owed 时该减法
        // 在 uint64 域环绕成约 2^64 的巨值,钳制条件恒为假,于是落进 else 再减
        // 一次、owed 变成天文数字。后果不是数字难看:Remaining() 随之巨大,
        // AddCurrency 的补缴 hook 会把该玩家该币种的**每一笔后续收入**全额扣走,
        // 每笔还都记一条看似正常的 TX_DEFERRED_CLAWBACK 流水。
        // GM 想清债时随手多减一点(owed=100 却发 delta=-200)就会触发。
        if (decrease >= debt.owed || debt.owed - decrease < debt.paid)
        {
            debt.owed = debt.paid; // effectively zeroes remaining
        }
        else
        {
            debt.owed -= decrease;
        }
    }

    if (!gmOperator.empty())
    {
        debt.gmOperator = gmOperator;
    }
    if (!reason.empty())
    {
        debt.reason = reason;
    }

    comp->dirty = true;

    LOG_INFO << "CurrencySystem: debt adjusted, owed=" << debt.owed
             << " paid=" << debt.paid
             << " delta=" << delta
             << " CurrencyType=" << static_cast<uint32_t>(type)
             << " entity=" << entt::to_integral(player);
}

// ---------------------------------------------------------------------------
// FreezeDebt — pause/resume auto-deduction
// ---------------------------------------------------------------------------

void CurrencySystem::FreezeDebt(entt::entity player, CurrencyType type, bool freeze,
                                const std::string &gmOperator)
{
    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        return;
    }

    auto it = comp->debts.find(static_cast<uint32_t>(type));
    if (it == comp->debts.end())
    {
        return;
    }

    it->second.frozen = freeze;
    if (!gmOperator.empty())
    {
        it->second.gmOperator = gmOperator;
    }
    comp->dirty = true;

    LOG_INFO << "CurrencySystem: debt " << (freeze ? "frozen" : "unfrozen")
             << " CurrencyType=" << static_cast<uint32_t>(type)
             << " operator=" << gmOperator
             << " entity=" << entt::to_integral(player);
}

// ---------------------------------------------------------------------------
// QueryDebts — return all active debts for GM inspection
// ---------------------------------------------------------------------------

std::vector<CurrencyDebtEntry> CurrencySystem::QueryDebts(entt::entity player)
{
    std::vector<CurrencyDebtEntry> result;
    const auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        return result;
    }

    result.reserve(comp->debts.size());
    for (const auto &[typeId, debt] : comp->debts)
    {
        CurrencyDebtEntry entry;
        entry.set_currency_type(typeId);
        entry.set_owed(debt.owed);
        entry.set_paid(debt.paid);
        entry.set_frozen(debt.frozen);
        entry.set_expires_at(debt.expiresAt);
        entry.set_reason(debt.reason);
        entry.set_gm_operator(debt.gmOperator);
        entry.set_created_at(debt.createdAt);
        result.push_back(std::move(entry));
    }
    return result;
}

// ---------------------------------------------------------------------------
// BlockCurrency — GM禁止获取某种货币
// ---------------------------------------------------------------------------

uint32_t CurrencySystem::BlockCurrency(entt::entity player, CurrencyType type)
{
    if (static_cast<uint32_t>(type) >= static_cast<uint32_t>(kCurrencyMax))
    {
        LOG_ERROR << "CurrencySystem::BlockCurrency: invalid CurrencyType="
                  << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    auto *currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    if (currency == nullptr)
    {
        LOG_ERROR << "CurrencySystem::BlockCurrency: CurrencyComp missing on entity "
                  << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    const uint32_t typeId = static_cast<uint32_t>(type);
    // Avoid duplicate entries.
    for (int i = 0; i < currency->blocked_types_size(); ++i)
    {
        if (currency->blocked_types(i) == typeId)
        {
            return kSuccess; // already blocked
        }
    }
    currency->add_blocked_types(typeId);

    LOG_INFO << "CurrencySystem: GM blocked CurrencyType=" << typeId
             << " entity=" << entt::to_integral(player);
    return kSuccess;
}

// ---------------------------------------------------------------------------
// UnblockCurrency — GM解除禁止获取
// ---------------------------------------------------------------------------

uint32_t CurrencySystem::UnblockCurrency(entt::entity player, CurrencyType type)
{
    if (static_cast<uint32_t>(type) >= static_cast<uint32_t>(kCurrencyMax))
    {
        LOG_ERROR << "CurrencySystem::UnblockCurrency: invalid CurrencyType="
                  << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    auto *currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    if (currency == nullptr)
    {
        LOG_ERROR << "CurrencySystem::UnblockCurrency: CurrencyComp missing on entity "
                  << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    const uint32_t typeId = static_cast<uint32_t>(type);
    auto *blocked = currency->mutable_blocked_types();
    for (int i = 0; i < blocked->size(); ++i)
    {
        if ((*blocked)[i] == typeId)
        {
            blocked->SwapElements(i, blocked->size() - 1);
            blocked->RemoveLast();
            LOG_INFO << "CurrencySystem: GM unblocked CurrencyType=" << typeId
                     << " entity=" << entt::to_integral(player);
            return kSuccess;
        }
    }
    return kSuccess; // was not blocked
}

// ---------------------------------------------------------------------------
// IsCurrencyBlocked
// ---------------------------------------------------------------------------

bool CurrencySystem::IsCurrencyBlocked(entt::entity player, CurrencyType type)
{
    const auto *currency = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    if (currency == nullptr)
    {
        return false;
    }
    const uint32_t typeId = static_cast<uint32_t>(type);
    for (int i = 0; i < currency->blocked_types_size(); ++i)
    {
        if (currency->blocked_types(i) == typeId)
        {
            return true;
        }
    }
    return false;
}
