#pragma once

// 本人近期战斗结果列表的纯规则(docs/design/battle-result-recovery.md §4.2)。
//
// 列表是持久数据 PlayerBattleResultComp(player_database.battle_results):结算应用的同一帧写入,随同一次存档提交。
// 这里只放与 ECS、时钟、日志无关的列表操作;调用方(player_battle.cpp)给入当前列表与输入,结果完全确定,单测只测这里。
//
// 每个函数都维持的不变量:
//   - battle_id 非 0,同一 battle_id 至多一条;
//   - result_seq 在列表内唯一、只增:新条目取「last_result_seq 与现存最大 result_seq 的较大者 + 1」,
//     加载来的列表即使 last_result_seq 落后也不会撞号;
//   - 条数不超过 kMaxEntries,超出按 result_seq 从小到大淘汰;
//   - 新旧只看 result_seq,不看下标(AGENTS §11.6):存放顺序只为确定性序列化。
//
// 「hidden」是已写进列表、但这份存档还没确认提交的局(player_battle.cpp 的 BattleResultAwaitingCommitComp):
// 它们不对客户端公开,也不接受确认。
//
// 线程:纯函数,没有共享状态。

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <utility>
#include <vector>

#include "network/network_constants.h" // kProtoFieldCheckerThreshold
#include "proto/common/component/player_battle_result_comp.pb.h"

namespace battle_result
{
	// 每名玩家保留的结果条数,同时是查询应答与确认请求的条数上限。
	// 不能超过场景入口的 repeated 字段上限:超过的确认请求会在派发之前被静默丢弃(scene_handler.cpp 的
	// ProtoFieldChecker),客户端拿不到任何回包。
	inline constexpr std::size_t kMaxEntries = 10;
	static_assert(kMaxEntries <= kProtoFieldCheckerThreshold,
				  "近期战斗结果的条数上限超过了场景入口的 repeated 字段上限,满额的确认请求会被静默丢弃");

	// 一条记录里每张物品清单最多几行。超出时只留配置号最小的那些,并标 lines_truncated。
	// 实际战斗只有几行;上限只为钉住单条记录(约 0.8 KB 封顶)、进而整个列表(约 8 KB 封顶)的大小。
	inline constexpr std::size_t kMaxItemLinesPerList = 32;

	// Record 的结论。最后一项固定为 kCount(AGENTS §11.2)。
	enum class RecordOutcome : uint8_t
	{
		kRecorded,        // 已追加
		kDuplicateBattle, // 已有同一 battle_id 的记录:什么都不改,保留第一条
		kInvalidBattle,   // battle_id 为 0:什么都不改
		kCount
	};

	inline bool ContainsBattle(const std::vector<uint64_t>& battleIds, const uint64_t battleId)
	{
		return std::find(battleIds.begin(), battleIds.end(), battleId) != battleIds.end();
	}

	// 列表里最大的 result_seq;空列表为 0。
	inline uint64_t MaxResultSeq(const PlayerBattleResultComp& history)
	{
		uint64_t maxSeq = 0;
		for (const auto& entry : history.entries())
		{
			maxSeq = std::max(maxSeq, entry.result_seq());
		}
		return maxSeq;
	}

	inline const PlayerBattleResult* FindByBattle(const PlayerBattleResultComp& history, const uint64_t battleId)
	{
		for (const auto& entry : history.entries())
		{
			if (entry.battle_id() == battleId)
			{
				return &entry;
			}
		}
		return nullptr;
	}

	// 追加一局结果。entry 的 result_seq 与 seen 由这里决定,调用方填的值被覆盖。
	// 超过 kMaxEntries 时淘汰 result_seq 最小的条目,被淘汰的 battle_id 追加进 evicted(调用方据此摘掉它们的待公开标记)。
	// result_seq 一局加一,64 位用不完,不做回绕处理。
	inline RecordOutcome Record(PlayerBattleResultComp& history, PlayerBattleResult entry, std::vector<uint64_t>& evicted)
	{
		if (entry.battle_id() == 0)
		{
			return RecordOutcome::kInvalidBattle;
		}
		if (FindByBattle(history, entry.battle_id()) != nullptr)
		{
			return RecordOutcome::kDuplicateBattle;
		}
		const uint64_t nextSeq = std::max(history.last_result_seq(), MaxResultSeq(history)) + 1;
		entry.set_result_seq(nextSeq);
		entry.set_seen(false);
		history.set_last_result_seq(nextSeq);
		*history.add_entries() = std::move(entry);
		while (static_cast<std::size_t>(history.entries_size()) > kMaxEntries)
		{
			int oldest = 0;
			for (int i = 1; i < history.entries_size(); ++i)
			{
				if (history.entries(i).result_seq() < history.entries(oldest).result_seq())
				{
					oldest = i;
				}
			}
			evicted.push_back(history.entries(oldest).battle_id());
			history.mutable_entries()->DeleteSubrange(oldest, 1);
		}
		return RecordOutcome::kRecorded;
	}

	// 公开的结果:按 result_seq 降序(新的在前),跳过 hidden 里的局,至多 kMaxEntries 条。
	// 返回的指针指向 history 里的条目,history 被修改之前有效。
	inline std::vector<const PlayerBattleResult*> VisibleNewestFirst(const PlayerBattleResultComp& history,
																	  const std::vector<uint64_t>& hidden)
	{
		std::vector<const PlayerBattleResult*> visible;
		for (const auto& entry : history.entries())
		{
			if (!ContainsBattle(hidden, entry.battle_id()))
			{
				visible.push_back(&entry);
			}
		}
		std::sort(visible.begin(), visible.end(),
				  [](const PlayerBattleResult* a, const PlayerBattleResult* b) { return a->result_seq() > b->result_seq(); });
		if (visible.size() > kMaxEntries)
		{
			visible.resize(kMaxEntries);
		}
		return visible;
	}

	// 把 battleIds 里的局标成已展示,返回这次由未读变成已读的条数。
	// 不认识的局(已淘汰、从未有过)与 hidden 里的局跳过;同一 battle_id 重复出现只算一次。幂等。
	template <typename BattleIds>
	uint32_t MarkSeen(PlayerBattleResultComp& history, const BattleIds& battleIds, const std::vector<uint64_t>& hidden)
	{
		uint32_t changed = 0;
		for (auto& entry : *history.mutable_entries())
		{
			if (entry.seen() || ContainsBattle(hidden, entry.battle_id()))
			{
				continue;
			}
			if (std::find(battleIds.begin(), battleIds.end(), entry.battle_id()) == battleIds.end())
			{
				continue;
			}
			entry.set_seen(true);
			++changed;
		}
		return changed;
	}

	// 物品清单:按配置号升序写进 lines,跳过配置号或数量为 0 的项,至多 kMaxItemLinesPerList 行。
	// 返回 true = 有行被截掉。输入不要求有序(调用方传的是 unordered_map 的拷贝)。
	inline bool FillItemLines(std::vector<std::pair<uint32_t, uint32_t>> items,
							  google::protobuf::RepeatedPtrField<PlayerBattleResultItem>& lines)
	{
		std::sort(items.begin(), items.end());
		bool truncated = false;
		for (const auto& [itemTableId, count] : items)
		{
			if (itemTableId == 0 || count == 0)
			{
				continue;
			}
			if (static_cast<std::size_t>(lines.size()) >= kMaxItemLinesPerList)
			{
				truncated = true;
				break;
			}
			auto* line = lines.Add();
			line->set_item_table_id(itemTableId);
			line->set_count(count);
		}
		return truncated;
	}
} // namespace battle_result
