#pragma once

#include <cstdint>

#include "proto/common/component/battle_settlement_ledger_comp.pb.h"

// 结算幂等账本的纯规则(无 ECS、无 Redis、无时钟)。
// 语义与推导见 battle_settlement_ledger_comp.proto 与
// docs/design/turn-battle-gap-closure.md §9.3;单测 battle_settlement_ledger_test.cpp。
//
// 与 battle_settlement_application_cache.h 的分工(两者都要,不可互相替代):
//  - 应用缓存:thread_local、不落库。只负责**同一次应用在途**的重入保护
//    (应用中途触发的领域事件回头再调进来),以及同一进程内的快路径。
//  - 本账本:挂在玩家实体上、跟着 player_database 落库。是**权威**的"这一局是否已应用",
//    唯一能跨进程重启、跨实体重建生效的那一层。
//
// 两条判据必须分开,不能合成一条(这是第一版设计被评审判 broken 的主因):
//  - `HasApplied(实体上的活账本)`  = "别再发一次奖" —— 内存里已经应用过;
//  - "出现在 PlayerLastPersistedSnapshotComp 的账本里" = "可以销账" —— 字节确实在盘上。
// 活账本里有、落盘快照里没有,恰恰是最危险的那一刻:此时销账 = 崩溃即永久丢失。
namespace battle_settlement {

// 账本容量上限。
//
// **这不是"一个玩家能打多少场"的容量**,那样推是错的(评审驳回过一版按"离线不打战斗"
// 估的容量)。真正的界来自生命周期:条目只在"已应用但销账尚未确认"期间存在,销账 EVAL
// 一成功就被摘掉,所以稳态长度是 0~1。要同时堆到 64 条,意味着连续 64 局的条件删全部失败
// —— 那是 Redis 已经不可用,而不是玩家打得多。
//
// 因此 64 是**异常兜底**:留足排查余量,又不让这段跟着玩家存盘无限长大(64 × 16B ≈ 1KB)。
// 真的溢出说明系统已经病了,淘汰最旧项并让调用方打 ERROR,不要静默增长。
inline constexpr int kMaxAppliedRecords = 64;

inline bool HasApplied(const BattleSettlementLedgerComp& ledger, uint64_t battleId)
{
	if (battleId == 0) return false;
	for (const auto& entry : ledger.applied())
	{
		if (entry.battle_id() == battleId) return true;
	}
	return false;
}

// 登记一局。已在账本里则只刷新时间戳(重投重复登记不应造成第二条)。
// 返回 true = 为了腾地方淘汰了最旧的一条(调用方应打 ERROR:见 kMaxAppliedRecords)。
inline bool RecordApplied(BattleSettlementLedgerComp& ledger, uint64_t battleId, uint64_t nowMs)
{
	if (battleId == 0) return false;
	for (auto& entry : *ledger.mutable_applied())
	{
		if (entry.battle_id() == battleId)
		{
			entry.set_applied_at_ms(nowMs);
			return false;
		}
	}
	bool evicted = false;
	while (ledger.applied_size() >= kMaxAppliedRecords)
	{
		// 淘汰 applied_at_ms 最小的一条:顺序由字段决定,不由下标决定(§11.6)。
		int oldest = 0;
		for (int i = 1; i < ledger.applied_size(); ++i)
		{
			if (ledger.applied(i).applied_at_ms() < ledger.applied(oldest).applied_at_ms()) oldest = i;
		}
		ledger.mutable_applied()->SwapElements(oldest, ledger.applied_size() - 1);
		ledger.mutable_applied()->RemoveLast();
		evicted = true;
	}
	auto* added = ledger.add_applied();
	added->set_battle_id(battleId);
	added->set_applied_at_ms(nowMs);
	return evicted;
}

// 摘掉一局。**只允许在销账(条件删 pending)确认成功之后调用** ——
// 提前摘会让"销账其实没落地 + 重启"退回重复发奖。返回 true = 确实摘掉了一条。
inline bool ForgetApplied(BattleSettlementLedgerComp& ledger, uint64_t battleId)
{
	for (int i = 0; i < ledger.applied_size(); ++i)
	{
		if (ledger.applied(i).battle_id() != battleId) continue;
		ledger.mutable_applied()->SwapElements(i, ledger.applied_size() - 1);
		ledger.mutable_applied()->RemoveLast();
		return true;
	}
	return false;
}

} // namespace battle_settlement
