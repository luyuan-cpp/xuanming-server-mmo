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
#include "modules/currency/system/currency_clawback_stats.h" // 补缴因版本错位被跳过的计数(scene 的 Prometheus 出口导出)
#include "modules/gain_block/gain_block_service.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "modules/transaction_log/transaction_log_system.h"
#include "proto/common/component/currency_comp.pb.h"
#include "services/scene/player/system/player_lifecycle.h" // IsPlayerFrozen — see cross-zone-readiness-audit.md §11.1
#include <ecs_context.h>

// ---------------------------------------------------------------------------
// Internal helper
// ---------------------------------------------------------------------------

// 币种判定统一用 currency.h 的 IsKnownCurrencyType。它**纯判定,零副作用、不碰 ECS**,
// 所以下面各原语把它排在业务条件(冻结 / 封禁)之前 —— 而且必须排在前面:原语的调用方
// 传的是编译期常量或启动期已校验的配置,越界币种是调用方写错了,永远不可能成功;
// 若让冻结先命中,会拿到 RETRY 类的 kAssetFrozen,对一个注定失败的请求无限重投
// (04-asset-channel.md §4.10 的 RETRY / REJECTED 分类)。
// 外部载荷的不认识币种不会走到这里:收件箱执行器与资产通道在原语之前先判,按版本错位处理。
// 这与背包侧"纯预检必须全部跑在 ReserveOrEvict 之前"是同一条纪律。

namespace
{

// PlayerCurrencyComp(补缴欠款 + 脏标记)是玩家的**必需组件**,唯一创建点是加载
// (PlayerDatabaseMessageFieldsUnmarshal 从 CurrencyComp.debts 装载)。增减路径只取不建:
// 缺了说明加载不变量已被破坏 —— 欠款压根没装载进来。此时补建一本空的,补缴会被整笔绕过,
// 下一次存盘 SaveToProto 还会拿空表把盘上的欠款覆盖掉(GM 挂的债就此消失)。
// 所以只记错误、返回 nullptr,由调用方在**任何改动之前**拒绝本次操作(kInvalidParameter)。
PlayerCurrencyComp *FindRequiredPlayerCurrencyComp(entt::entity player, const char *caller)
{
    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        LOG_ERROR << caller << ": PlayerCurrencyComp missing on entity "
                  << entt::to_integral(player)
                  << " (load invariant broken; rejected without creating the component)";
    }
    return comp;
}

// 欠款是否带着更新版本才有的字段(新版本给 CurrencyDebtEntry 加的)。已退役字段的残留字节不算,否则带残留的欠款
// 会被永远当成新版本数据(unknown_field_carry::CarriesNewerVersionFields)。
// DebtCarriesUnknownFields 与 AddCurrency 的补缴扣还守卫共用这一个判据(混版本设计 §4.4)。
bool DebtCarriesNewerVersionFields(const CurrencyDebt &debt)
{
    const auto debtEntryName = CurrencyDebtEntry::descriptor()->full_name();
    return unknown_field_carry::CarriesNewerVersionFields(
        std::string_view(debtEntryName.data(), debtEntryName.size()), debt.unknownFields);
}

// 一笔收入按补缴先扣之后的计划:扣还多少、净入账多少,以及是否因欠款带新版本字段而跳过扣还。只读,不动欠款。
// AddCurrency 的提交与 CurrencySystem::CreditOverflows 的预检共用它,两处对同一笔收入不会给出不同结论。
struct CreditPlan
{
    uint64_t deduct = 0;
    uint64_t credit = 0;
    bool clawbackSkippedForVersionSkew = false;
};

CreditPlan PlanCredit(const CurrencyDebt *debt, uint64_t gain)
{
    CreditPlan plan;
    if (debt != nullptr)
    {
        // 只扣有效欠款(没冻结、没过期)。
        const bool expired = (debt->expiresAt > 0 && TimeSystem::NowSecondsUTC() >= debt->expiresAt);
        if (!debt->frozen && !expired)
        {
            plan.deduct = std::min(gain, debt->Remaining());
        }
        // 混版本保护(docs/design/mixed-version-persist-field-protection.md §4.4):欠款带着更新版本才有的字段时,
        // 本版本不扣还也不删除,按冻结处理。新字段可能改了扣还口径(例如利息、分期),按 owed−paid 扣就扣错了;
        // 扣满还会整条删除,新字段随之消失。这笔收入全额入账,欠款原样留给认识这些字段的版本去收。
        if (plan.deduct > 0 && DebtCarriesNewerVersionFields(*debt))
        {
            plan.clawbackSkippedForVersionSkew = true;
            plan.deduct = 0;
        }
    }
    plan.credit = gain - plan.deduct;
    return plan;
}

// 净入账 credit 加到 balance 上会不会回绕。
bool BalanceOverflows(uint64_t balance, uint64_t credit)
{
    return credit > std::numeric_limits<uint64_t>::max() - balance;
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
    // 需要记账**的业务条件之前判掉(理由见文件开头的币种判定注释)。
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
    // 此时 AddCurrency 只会写进源端 CurrencyComp:handoff 标记写出后 SavePlayer 直接
    // 跳过,scene_manager 放行后实体随 DestroyDeposedPlayer 不存盘销毁,目标节点永远读不到。
    // 交接未成则由 AbortTravelHandoff 解冻,之后可正常写。
    // 写入类拦截目录见 docs/design/cross-zone-readiness-audit.md §11.1(该文只有 §11 仍有效)。
    if (PlayerLifecycleSystem::IsPlayerFrozen(player))
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

    // 交易退回本人托管的货币不是新增获得,两道封禁都不判(GainBlockService::CountsAsGain,审计 r2-trade#1)。
    const bool countsAsGain = GainBlockService::CountsAsGain(txType);

    // ── Global (server-wide) block check ────────────────────────────────
    if (countsAsGain && GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kCurrency,
                                                            static_cast<uint32_t>(type)))
    {
        LOG_WARN << "CurrencySystem::AddCurrency: currency GLOBALLY blocked. CurrencyType="
                 << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        // 封禁是终局拒绝(REJECTED 类):资产通道据此记账并让业务侧退款,不重投。
        return PrintStackAndReturnError(kAssetBlocked);
    }

    // ── Per-player GM block check ────────────────────────────────────────
    if (countsAsGain && IsCurrencyBlocked(player, type))
    {
        LOG_WARN << "CurrencySystem::AddCurrency: currency blocked by GM. CurrencyType="
                 << static_cast<uint32_t>(type) << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kAssetBlocked);
    }

    // 必需组件先齐再动账(校验前置):补缴记录缺失时一分钱都不能入账 —— 入了账,
    // 欠款就被绕过,而这笔收入的流水一经写出便收不回来。
    auto *comp = FindRequiredPlayerCurrencyComp(player, "CurrencySystem::AddCurrency");
    if (comp == nullptr)
    {
        return PrintStackAndReturnError(kInvalidParameter);
    }

    const uint64_t gain = static_cast<uint64_t>(amount);

    uint64_t *balance = ResolveCurrencyField(player, type);
    if (balance == nullptr)
    {
        return PrintStackAndReturnError(kInvalidParameter);
    }

    const uint64_t balanceBefore = *balance;

    // ── Deferred-clawback / 补缴:先只算扣还额,不动欠款 ─────────────────
    // 判定与提交分两段:下面的溢出判定要用扣还后的净入账额,而拒绝必须发生在扣还之前 ——
    // 先扣还、再发现溢出而拒绝,欠款已经少了一截,这笔收入却没入账,扣掉的那部分就凭空没了。
    // 扣还计划(PlanCredit)与资产通道入账之前的溢出预检(CreditOverflows)是同一份。
    auto debtIt = comp->debts.find(static_cast<uint32_t>(type));
    CurrencyDebt *debt = (debtIt != comp->debts.end()) ? &debtIt->second : nullptr;
    const CreditPlan plan = PlanCredit(debt, gain);
    const uint64_t deduct = plan.deduct;
    const uint64_t credit = plan.credit;
    const bool clawbackSkippedForVersionSkew = plan.clawbackSkippedForVersionSkew;

    // ── uint64 溢出:零副作用拒绝 ─────────────────────────────────────────
    // 回绕后的余额是个很小的数:玩家的钱凭空没了,流水却记成一笔正常入账。余额逼近 2^64 只可能是坏数据或刷钱,
    // 与 amount<=0 同类按编程错误回 kInvalidParameter(不新增 tip)。按净入账额判,与战斗结算的整批预检
    // CanCreditWholeBattleCurrency 同口径 —— 预检放行的那一笔这里不能再拒,否则多币种结算会只入一半。
    // 判定排在扣还、流水、异常检测之前:拒绝时欠款、余额、脏标记、流水、异常窗口都不动。
    if (BalanceOverflows(balanceBefore, credit))
    {
        LOG_ERROR << "CurrencySystem::AddCurrency: balance overflow rejected, balance=" << balanceBefore
                  << " credit=" << credit << " amount=" << amount
                  << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return PrintStackAndReturnError(kInvalidParameter);
    }

    // ── 提交:从这里起不再有失败分支 ───────────────────────────────────
    if (clawbackSkippedForVersionSkew)
    {
        // 版本错位的统一计数口径:值班查询按 metric= / where= 数行(asset-channel-alerts.yaml 文件头);同一时刻进程计数加一,
        // 由 scene 的 Prometheus 出口导出为 scene_currency_clawback_version_skew_total,告警读它(persist-version-skew-alerts.yaml)。
        currency_clawback_stats::IncVersionSkewSkipped();
        LOG_WARN << "[CurrencySystem] metric=persist_version_skew where=currency_clawback player_id="
                 << tlsEcs.GetPlayerId(player) << " currency_type=" << static_cast<uint32_t>(type)
                 << " owed=" << debt->owed << " paid=" << debt->paid << " gain=" << gain
                 << "; debt carries fields unknown to this version, clawback skipped as if frozen";
    }
    if (deduct > 0)
    {
        debt->paid += deduct;

        LOG_INFO << "CurrencySystem: deferred clawback deducted " << deduct
                 << " from gain, debt remaining=" << debt->Remaining()
                 << " CurrencyType=" << static_cast<uint32_t>(type)
                 << " entity=" << entt::to_integral(player);

        // Emit clawback tx log entry.
        TransactionLogSystem::LogClawbackDeduction(player, type, deduct, debt->Remaining());

        if (debt->Remaining() == 0)
        {
            comp->debts.erase(debtIt);
        }
    }

    // ── Credit remaining amount after debt ───────────────────────────────
    *balance += credit;

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
    if (PlayerLifecycleSystem::IsPlayerFrozen(player))
    {
        LOG_WARN << "CurrencySystem::DeductCurrency rejected: player frozen for cross-zone migration. "
                 << "CurrencyType=" << static_cast<uint32_t>(type)
                 << " amount=" << amount
                 << " entity=" << entt::to_integral(player);
        // 同 AddCurrency:RETRY 类,调用方重投即可,不是终局拒绝。
        return PrintStackAndReturnError(kAssetFrozen);
    }

    // 同 AddCurrency:必需组件在扣余额之前确认。原先是先扣钱、再发现缺组件时当场补建一本空的,
    // 扣款照样成立,而缺组件本身(欠款没装载)被悄悄掩盖。
    auto *comp = FindRequiredPlayerCurrencyComp(player, "CurrencySystem::DeductCurrency");
    if (comp == nullptr)
    {
        return PrintStackAndReturnError(kInvalidParameter);
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

uint32_t CurrencySystem::AttachDebt(entt::entity player, CurrencyType type, int64_t oweAmount,
                                    const std::string &reason,
                                    const std::string &gmOperator,
                                    uint64_t expiresAt)
{
    if (oweAmount <= 0)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: oweAmount must be > 0, got "
                  << oweAmount << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return kInvalidParameter;
    }
    // 越界币种是调用方写错了:不能让它在 debts 里落下一条任何货币都对不上的欠款(以前会)。
    if (!IsKnownCurrencyType(type))
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: invalid CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return kInvalidParameter;
    }

    uint64_t debtToAdd = static_cast<uint64_t>(oweAmount);

    auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: PlayerCurrencyComp missing on entity "
                  << entt::to_integral(player);
        return kInvalidParameter;
    }

    // 混版本保护(docs/design/mixed-version-persist-field-protection.md §4.4):已有欠款带着本版本不认识的字段时不合并。
    // 新字段的含义可能依赖这笔欠款的金额或来源(例如利息、分期),旧代码把新的一笔并进去,两者就对不上了。
    // 本原语只负责 fail-closed 拒绝、不合并。这是版本错位,不是确定性拒绝:调用方必须先用
    // DebtCarriesUnknownFields 判定,按版本错位处理(不执行、不记终态拒绝,之后重新取一次处置再判),
    // 不能按本地副本原地重试 —— 本地副本看不到服务端之后挂的作废;这笔欠款若在会话内被删除,错位随之消失,
    // 续做就会挂上一条可能已被作废的处置(AddCurrency 不扣还带新字段的欠款,它不会被还清删除;错位不消失时续做只会空转)
    // (收件箱执行器 GmSanctionInboxExecutor::AttachCurrencyDebt 回 kVersionSkew,收件箱停用本域,实体重建后重拉(退出完成后再登录,或换节点);退出保留期内重登会复用实体,不重拉)。
    if (DebtCarriesUnknownFields(player, type))
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: existing debt carries fields unknown to this version, refusing to merge"
                  << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return kFeatureUnavailable;
    }

    auto &debt = comp->debts[static_cast<uint32_t>(type)];
    // 已过期的旧欠款不再收(AddCurrency 跳过它),新的一笔不能把它复活:先作废,再按新欠款算
    // (docs/design/data-overwrite-rollback-audit-r2-20261019.md 第 6 条)。
    if (debt.expiresAt > 0 && TimeSystem::NowSecondsUTC() >= debt.expiresAt)
    {
        LOG_INFO << "CurrencySystem::AttachDebt: expired debt written off before merge, owed=" << debt.owed
                 << " paid=" << debt.paid
                 << " CurrencyType=" << static_cast<uint32_t>(type)
                 << " entity=" << entt::to_integral(player);
        debt = CurrencyDebt{};
    }
    const bool merging = debt.Remaining() > 0;
    if (debt.owed > std::numeric_limits<uint64_t>::max() - debtToAdd)
    {
        LOG_ERROR << "CurrencySystem::AttachDebt: owed overflow rejected, current=" << debt.owed
                  << " add=" << debtToAdd
                  << " CurrencyType=" << static_cast<uint32_t>(type)
                  << " entity=" << entt::to_integral(player);
        return kInvalidParameter;
    }
    debt.owed += debtToAdd;
    debt.reason = reason;
    debt.gmOperator = gmOperator;
    // 到期时间取两单较晚的,0 是不过期:小单不能让大单提前失效。旧欠款已还清时按新单算。
    if (merging)
    {
        debt.expiresAt = (debt.expiresAt == 0 || expiresAt == 0) ? 0 : std::max(debt.expiresAt, expiresAt);
    }
    else
    {
        debt.expiresAt = expiresAt;
    }
    if (debt.createdAt == 0)
    {
        debt.createdAt = TimeSystem::NowSecondsUTC();
    }

    comp->dirty = true;

    LOG_INFO << "CurrencySystem: debt attached, total owed=" << debt.owed
             << " paid=" << debt.paid
             << " CurrencyType=" << static_cast<uint32_t>(type)
             << " entity=" << entt::to_integral(player);
    return kSuccess;
}

bool CurrencySystem::DebtCarriesUnknownFields(entt::entity player, CurrencyType type)
{
    if (!IsKnownCurrencyType(type))
    {
        return false;
    }
    const auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (comp == nullptr)
    {
        return false;
    }
    const auto existing = comp->debts.find(static_cast<uint32_t>(type));
    if (existing == comp->debts.end())
    {
        return false;
    }
    // 只认更新版本加的字段;已退役字段的残留字节不算(否则带残留的欠款会被永远当成新版本数据,再也挂不上新欠款)。
    // 与 AddCurrency 的补缴扣还守卫是同一个判据。
    return DebtCarriesNewerVersionFields(existing->second);
}

bool CurrencySystem::CreditOverflows(entt::entity player, CurrencyType type, uint64_t amount)
{
    if (amount == 0 || !IsKnownCurrencyType(type))
    {
        return false;
    }
    const auto *balances = tlsEcs.actorRegistry.try_get<CurrencyComp>(player);
    const auto *comp = tlsEcs.actorRegistry.try_get<PlayerCurrencyComp>(player);
    if (balances == nullptr || comp == nullptr)
    {
        return false;
    }
    // 余额槽位缺的按 0 读,与 EnsureCurrencySlots 补的零值一致;这里只读,不补槽位。
    const auto index = static_cast<int>(type);
    const uint64_t balance = index < balances->values_size() ? balances->values(index) : 0;
    const auto debtIt = comp->debts.find(static_cast<uint32_t>(type));
    const CurrencyDebt *debt = (debtIt != comp->debts.end()) ? &debtIt->second : nullptr;
    return BalanceOverflows(balance, PlanCredit(debt, amount).credit);
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
    if (!IsKnownCurrencyType(type))
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
    if (!IsKnownCurrencyType(type))
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
