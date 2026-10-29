#pragma once

// 收件箱 SCENE_GM_SANCTION 域的执行器(docs/design/player-inbox-service.md §2.2、§7.6)。
//
// 三种类型 → 三个现成原语:
//   GM_BLOCK_CURRENCY_GAIN    → CurrencySystem::BlockCurrency    (天然幂等)
//   GM_UNBLOCK_CURRENCY_GAIN  → CurrencySystem::UnblockCurrency  (天然幂等)
//   GM_ATTACH_CURRENCY_DEBT   → CurrencySystem::AttachDebt       (**不幂等**,只靠收件箱账本去重)
//
// 结局口径:
//   * 载荷解码失败、欠款额不在 (0, INT64_MAX] → kRejected(kPlayerInboxPayloadInvalid);
//   * 载荷币种不在本二进制取值表内(IsKnownCurrencyType)→ kVersionSkew:Go 入箱已按同源取值表
//     (proto/common/base/currency_type.proto)拒绝写错的值,这里只可能是更新版本加的币种;本二进制在会话内
//     永远不会认识它,所以不用 kDefer。与版本无关的确定性校验(解码、欠款额)排在它之前。
//   * 原语返回 kSuccess → kApplied;原语返回其他 tip 码 → kRejected(该码),例如 AttachDebt 的 owed 溢出;
//   * CurrencyComp / PlayerCurrencyComp 缺失(加载不变量被破坏)→ kDefer:不改任何状态,不把一条 GM 处置
//     因本地缺陷钉成终态 REJECTED;此时存盘本身也会被 Marshal 整份拒绝,结局落不了盘。
//   * ATTACH_DEBT 时同币种的既有欠款带本版本不认识的字段(CurrencySystem::DebtCarriesUnknownFields)→ kVersionSkew:
//     版本错位,AttachDebt 不会合并;不记终态,收件箱停用本域,实体重建后重拉(已作废记 SKIPPED,落到新版本节点再挂;
//     退出保留期内重登复用实体,不重拉)。
//     不用 kDefer:这种错位会在会话内消失(欠款还清被删),DEFER 的本地续做看不到之后挂的 VOID
//     (docs/design/mixed-version-persist-field-protection.md §4.4,player-inbox-service.md §7.5 评审 L1)。
//
// 不判冻结 / 战斗 / 可改:那是 PlayerInboxSystem 前置闸的事(闸**不下沉**到 CurrencySystem,D48 红线)。
// 契约见 player_inbox_registry.h 的 PlayerInboxExecutorFn。单测:cpp/tests/currency_test/gm_sanction_inbox_executor_test.cpp。

#include <string>

#include "entt/src/entt/entity/entity.hpp"
#include "player/system/player_inbox_registry.h"

class GmSanctionInboxExecutor
{
public:
	static PlayerInboxExecResult BlockCurrencyGain(entt::entity player, const std::string &payload);
	static PlayerInboxExecResult UnblockCurrencyGain(entt::entity player, const std::string &payload);
	static PlayerInboxExecResult AttachCurrencyDebt(entt::entity player, const std::string &payload);
};
