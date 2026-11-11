#pragma once

// 物品数量预留的纯规则(docs/design/battle-item-reservation.md)。零 ECS 依赖,只读写 PlayerItemHoldComp,单测直接盯。
//
// 数据在 PlayerItemHoldComp(proto/common/component/player_item_hold_comp.proto),规则的执行点只有 BagService:
// 出包入口按「可计数总量 - 别的预留」放行,结算按自己那份预留扣(bag_service.h「物品数量预留」一段)。
// 这里只回答「这份清单记了什么、某个配置号被占了多少」,不知道背包、实体、时钟,也不发日志。
//
// 规则:
//   - 一份预留按 hold_id 认,在本玩家名下唯一;lines 按 config_id 升序、互不重复、count > 0。
//   - 某个配置号被占用的数量 = 全部预留里这一配置号的数量之和。本版本不认识的种类、加载校验没过的记录也照样算:
//     多占只会让玩家暂时拿不走东西,少占会让晚到的结算扣不出来(数据层的强一致优先)。
//   - 同时至多 kMaxHolds 份。正常情况下至多 1 份(玩家同一时刻只在一局里);留余量给「冻结已按期限解开、
//     终局还没落定」的旧局。

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <map>
#include <set>
#include <string>

#include "proto/common/component/player_item_hold_comp.pb.h"

namespace item_hold
{
	// 预留的种类(落盘存 uint32)。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class ItemHoldKind : uint8_t
	{
		kUnspecified, // 0:只会出现在坏数据里,Add 拒绝
		kBattle,      // 1:回合制战斗,hold_id = battle_id,由 PlayerBattleSystem 建立与释放

		kCount
	};

	inline constexpr const char* kItemHoldKindNames[] = {"unspecified", "battle"};
	static_assert(std::size(kItemHoldKindNames) == static_cast<std::size_t>(ItemHoldKind::kCount),
				  "kItemHoldKindNames 与 ItemHoldKind 逐项对应");

	// 只进日志。本版本不认识的种类回 "unknown"。
	constexpr const char* ItemHoldKindName(const uint32_t storedKind)
	{
		return storedKind < std::size(kItemHoldKindNames) ? kItemHoldKindNames[storedKind] : "unknown";
	}

	// 一名玩家同时至多几份预留。正常 0 或 1 份,上界只防坏数据把每次出包的遍历拖慢。
	inline constexpr std::size_t kMaxHolds = 8;
	// 一份预留至多几种物品。战斗快照只带 battle_usable 的道具,远小于这个数。
	inline constexpr std::size_t kMaxLinesPerHold = 256;

	// 一份预留的物品清单:配置号 -> 数量。std::map 有序,写进 lines 时天然按 config_id 升序。
	using HoldLines = std::map<uint32_t, uint64_t>;

	// 某个配置号被占用的数量:全部预留(除 exceptHoldId 那一份;0 = 不排除)里这一配置号的数量之和,上溢饱和。纯函数。
	inline uint64_t HeldCount(const PlayerItemHoldComp& holds, const uint32_t configId, const uint64_t exceptHoldId = 0)
	{
		uint64_t total = 0;
		for (const auto& hold : holds.holds())
		{
			if (exceptHoldId != 0 && hold.hold_id() == exceptHoldId)
			{
				continue;
			}
			for (const auto& line : hold.lines())
			{
				if (line.config_id() != configId)
				{
					continue;
				}
				total = line.count() > UINT64_MAX - total ? UINT64_MAX : total + line.count();
			}
		}
		return total;
	}

	// 按号找一份预留;没有返回 nullptr。只读。
	inline const ItemHold* Find(const PlayerItemHoldComp& holds, const uint64_t holdId)
	{
		for (const auto& hold : holds.holds())
		{
			if (hold.hold_id() == holdId)
			{
				return &hold;
			}
		}
		return nullptr;
	}

	// Add 的结论。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class AddVerdict : uint8_t
	{
		kAdded,     // 已追加
		kInvalid,   // 号为 0、种类不合法、清单为空 / 过长 / 含 0:调用方缺陷
		kDuplicate, // 这个号已经有一份预留
		kFull,      // 已有 kMaxHolds 份

		kCount
	};

	// 追加一份预留;不是 kAdded 时一字不改。不判数量够不够(那要看背包,BagService::PlaceHold 判)。
	inline AddVerdict Add(PlayerItemHoldComp& holds, const uint64_t holdId, const ItemHoldKind kind, const HoldLines& lines,
						  const uint64_t createdAtMs, const uint64_t reviewAfterMs)
	{
		if (holdId == 0 || kind == ItemHoldKind::kUnspecified || kind >= ItemHoldKind::kCount || lines.empty() ||
			lines.size() > kMaxLinesPerHold)
		{
			return AddVerdict::kInvalid;
		}
		for (const auto& [configId, count] : lines)
		{
			if (configId == 0 || count == 0)
			{
				return AddVerdict::kInvalid;
			}
		}
		if (Find(holds, holdId) != nullptr)
		{
			return AddVerdict::kDuplicate;
		}
		if (static_cast<std::size_t>(holds.holds_size()) >= kMaxHolds)
		{
			return AddVerdict::kFull;
		}
		auto* hold = holds.add_holds();
		hold->set_hold_id(holdId);
		hold->set_kind(static_cast<uint32_t>(kind));
		for (const auto& [configId, count] : lines)
		{
			auto* line = hold->add_lines();
			line->set_config_id(configId);
			line->set_count(count);
		}
		hold->set_created_at_ms(createdAtMs);
		hold->set_review_after_ms(reviewAfterMs);
		return AddVerdict::kAdded;
	}

	// 摘掉这个号的全部预留(坏数据里可能重复),返回摘掉了几份;其余预留的相对顺序不变。
	inline std::size_t Remove(PlayerItemHoldComp& holds, const uint64_t holdId)
	{
		std::size_t removed = 0;
		auto* list = holds.mutable_holds();
		for (int index = list->size() - 1; index >= 0; --index)
		{
			if (list->Get(index).hold_id() == holdId)
			{
				list->DeleteSubrange(index, 1);
				++removed;
			}
		}
		return removed;
	}

	// 加载时的卫生检查:空串 = 合法;否则是第一处问题的原因(只进日志)。
	// 不合法的记录照样原样携带、照样计入占用(HeldCount 不看合法性),不在加载时改写:
	// 清掉它等于把可能还在等结算的道具放出去,而它是不是坏数据要人看。本版本不认识的种类不算问题。
	inline std::string Validate(const PlayerItemHoldComp& holds)
	{
		if (static_cast<std::size_t>(holds.holds_size()) > kMaxHolds)
		{
			return "too_many_holds";
		}
		std::set<uint64_t> seen;
		for (const auto& hold : holds.holds())
		{
			if (hold.hold_id() == 0)
			{
				return "zero_hold_id";
			}
			if (!seen.insert(hold.hold_id()).second)
			{
				return "duplicate_hold_id";
			}
			if (hold.kind() == static_cast<uint32_t>(ItemHoldKind::kUnspecified))
			{
				return "unspecified_kind";
			}
			if (hold.lines_size() == 0 || static_cast<std::size_t>(hold.lines_size()) > kMaxLinesPerHold)
			{
				return "bad_line_count";
			}
			uint32_t previous = 0;
			for (const auto& line : hold.lines())
			{
				if (line.config_id() == 0 || line.count() == 0)
				{
					return "zero_config_or_count";
				}
				if (line.config_id() <= previous)
				{
					return "lines_not_ascending";
				}
				previous = line.config_id();
			}
		}
		return {};
	}
} // namespace item_hold
