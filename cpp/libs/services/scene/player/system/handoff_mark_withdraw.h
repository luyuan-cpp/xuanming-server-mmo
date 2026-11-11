#pragma once
#include "services/scene/player/system/handoff_sql_barrier.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <string>
#include <utility>
#include <vector>

// scene 逻辑线程独占的撤资格重试表,只收「退出优先」作废交接的撤回(PlayerLifecycleSystem::WithdrawHandoffMark 的两个
// 调用点)。在线 Abort 不进这张表:交接组件保持冻结、自己重发撤销,确认之后才解冻(AbortTravelHandoff)。
// Entry 保存原 H/F 与发出前固定的 R,不保存连接或对象指针。只有拿到 Redis 的整数应答(撤销已执行,或旧资格已被更新世代
// 取代)才销账;标记 TTL 到期不能证明迟到的 publisher 已无资格,所以条目没有截止时刻,一直重试到确认为止。
// 不设容量上限、不淘汰、不拒收(2026-11-11 起;原先满 4096 条就拒收,新撤回既不登记也不发送,没有日志也没有计数):
// 条目在,IsSceneChangeBusy 才拦得住这名玩家。丢掉一条未确认的撤回 = 旧标记照样有效、闸口却放开了 —— 玩家在退出收尾前
// 重连回来、实体留下继续玩时,本节点随后替他发的跨节点 EnterScene 会凭旧标记免存盘过换手门(回档,
// docs/ops/cross-zone-failure-test-runbook.md F3「D-1 为什么是缺陷」)。
// 别的机制替不了这张表:资格写成 R 与删标记是同一段 Lua(kLuaDelIfEqual),撤回不发,资格就停在原来的 H / F、标记也还在;
// scene_manager 的换手门只比标记与 owner_epoch,不看资格;go/db 只按归属代次拒旧写,而凭旧标记放行之前 owner_epoch 一直没变。
// 内存有自然上界:条目只在「交接在途时玩家退出」时产生;新的载入要先在 Redis 上登记资格,Redis 不通时进不来;有条目的玩家
// 在本节点又发不出新的换图与传送。所以条目数与 Redis 不可用那段时间里本节点上的玩家数同一量级,每条几百字节。
// 任何更新实体的回调另核原 entity/holder/冻结代次,旧 Confirm 不能消掉新资格的同毫秒条目。
namespace handoff_mark_withdraw
{
	using Clock = std::chrono::steady_clock;

	// 同一条目两次重试的最小间隔。Redis 重连成功时也不提前重发:负责人 2026-11-05 定「重连成功不做业务逻辑」
	// (docs/design/infra-reconnect-overview.md R14),原来重连时忽略本间隔的强制重试已删。
	inline constexpr std::chrono::seconds kRetryInterval{5};

	// 撤回脚本就是资格撤销脚本 handoff_sql_barrier::kRevokeFenceLua(契约见那里)。KEYS = owner_epoch / handoff_fence /
	// handoff 标记;ARGV = 原 H、发布时的 F(没发布过就传原 H)、本条固定的 R、标记的毫秒戳。资格仍是本条的 H/F/R 之一时
	// 写成 R,标记的毫秒戳等于本条的才一并删掉,回 1;旧资格已被更新世代取代时什么都不改,回 0(旧资格已不可写)。
	// 1 / 0 都说明脚本执行过,可以销账;ERROR 应答 = 没执行,留表重试。
	// 不能退回无条件 DEL:撤回会被延迟重发,这期间同一玩家可能已经有新的持有者写了新标记。
	inline const std::string& kLuaDelIfEqual = handoff_sql_barrier::kRevokeFenceLua;

	struct Entry
	{
		uint64_t playerId{0};
		std::string holderFence;
        std::string publishedFence;
        std::string revokedFence;
        std::string markValue;		   // 标记原文 "{epoch}:{ms}",唯一标识"我写的那一份"
		Clock::time_point nextAttemptAt; // 周期重试的最早时刻
		uint32_t attempts{0};			 // 已被 CollectDue 取走几次;调用方据此只在首发失败时打 ERROR
	};

	class Queue
	{
	public:
		// 登记一条撤回。同 (player, mark, 原 holder) 已在表里时幂等返回 false,原条目照旧拦人、照旧重试;否则追加并返回 true。
		// 从不因容量拒收,也从不淘汰别的条目(理由见文件头)。
		bool Add(uint64_t playerId, std::string markValue, Clock::time_point now, std::string holderFence = {},
                 std::string publishedFence = {}, std::string revokedFence = {})
		{
			for (const auto &entry : entries_)
			{
				if (entry.playerId == playerId && entry.markValue == markValue && entry.holderFence == holderFence)
				{
					return false;
				}
			}
			Entry entry;
			entry.playerId = playerId;
			entry.markValue = std::move(markValue);
            entry.holderFence = std::move(holderFence);
            entry.publishedFence = std::move(publishedFence);
            entry.revokedFence = std::move(revokedFence);
			entry.nextAttemptAt = now; // 立刻到期:调用方登记后紧接着 CollectDue 取走并发出第一次撤回
			entries_.push_back(std::move(entry));
			return true;
		}

		bool HasPlayer(uint64_t playerId) const
		{
			return std::any_of(entries_.begin(), entries_.end(),
							   [playerId](const Entry &entry) { return entry.playerId == playerId; });
		}

        // 只取到期重试副本，不因标记TTL删除条目；销账必须确认同一原holder。到期只看条目自己的 nextAttemptAt。
		std::vector<Entry> CollectDue(Clock::time_point now)
		{
			std::vector<Entry> due;
			for (auto &entry : entries_)
			{
				if (entry.nextAttemptAt > now)
				{
					continue;
				}
				entry.nextAttemptAt = now + kRetryInterval;
				++entry.attempts;
				due.push_back(entry);
			}
			return due;
		}

		// 销账:只删精确匹配的那一条。返回 false = 表里没有这一条(已被另一次应答销过,或从未登记)。
		bool Confirm(uint64_t playerId, const std::string &markValue, const std::string& holderFence = {})
		{
			const auto it = std::find_if(entries_.begin(), entries_.end(),
										 [&](const Entry &entry)
										 { return entry.playerId == playerId && entry.markValue == markValue && entry.holderFence == holderFence; });
			if (it == entries_.end())
			{
				return false;
			}
			entries_.erase(it);
			return true;
		}

		std::size_t size() const { return entries_.size(); }

	private:
		// 线性表:平时为空,只有 Redis 不通时才有条目;查找点(IsSceneChangeBusy)不是 per-tick 路径。
		std::vector<Entry> entries_;
	};
} // namespace handoff_mark_withdraw
