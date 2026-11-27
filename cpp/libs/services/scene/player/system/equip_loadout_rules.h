#pragma once

#include <algorithm>
#include <array>
#include <cctype>
#include <cstdint>
#include <functional>
#include <map>
#include <set>
#include <string>
#include <string_view>
#include <vector>

// 装备流派方案预设纯规则（不依赖 ECS / Protobuf / 时钟 / 网络，便于独立单测）。
// 对应设计文档：docs/planning/equipment-loadout-presets-v1.md（方案 A · 7 套三合一共享流派席位）。
// 有效席位上限（2..7）与战法·加点方案统一由 skill_loadout_rules.h 计算：
// 创角永久免费 2 套 + 85 级满级永久 +1 套 + 轻享卡有效期内 +2 套 + 按套月租 0..5 套，总封顶 7 套，到期只锁不删。
namespace equiploadout
{

// EquipSlot.xlsx 正式 8 槽：3 武器 / 4 帽子 / 5 衣服 / 6 鞋子 / 7 首饰一 / 8 首饰二 / 9 法宝一 / 10 法宝二。
inline constexpr std::array<uint32_t, 8> kFormalSlots = {3u, 4u, 5u, 6u, 7u, 8u, 9u, 10u};

inline constexpr uint32_t kFreeSchemes = 2u;
inline constexpr uint32_t kMaxSchemes = 7u;
inline constexpr uint32_t kMaxRoleTag = 4u; // 0默认 / 1输出 / 2控场 / 3辅助 / 4生存
inline constexpr uint32_t kMaxNameCodePoints = 12u;

// Item.xlsx 历史预留 ID（方案 A 统一经战法·加点按月租用接口共享 7 套席位，不再单独扣符买断）。
inline constexpr uint32_t kUnlockItemConfigId = 3413u;

inline constexpr bool IsFormalSlot(uint32_t slot)
{
	return slot >= 3u && slot <= 10u;
}

inline constexpr bool ValidRoleTag(uint32_t roleTag)
{
	return roleTag <= kMaxRoleTag;
}

inline constexpr uint32_t EffectiveUnlockedCount(uint32_t stored)
{
	if (stored < kFreeSchemes)
	{
		return kFreeSchemes;
	}
	return stored > kMaxSchemes ? kMaxSchemes : stored;
}

inline constexpr uint32_t EffectiveActiveSchemeId(uint32_t stored, uint32_t maxAllowed = kMaxSchemes)
{
	const uint32_t cap = EffectiveUnlockedCount(maxAllowed);
	return (stored >= 1u && stored <= cap) ? stored : 1u;
}

inline constexpr uint64_t EffectiveStateVersion(uint64_t stored)
{
	return stored == 0 ? 1u : stored;
}

inline constexpr bool ValidSchemeId(uint32_t schemeId, uint32_t unlockedCount)
{
	return schemeId >= 1u && schemeId <= EffectiveUnlockedCount(unlockedCount);
}

inline std::string DefaultSchemeName(uint32_t schemeId)
{
	switch (schemeId)
	{
	case 1u: return "方案一";
	case 2u: return "方案二";
	case 3u: return "方案三";
	case 4u: return "方案四";
	case 5u: return "方案五";
	case 6u: return "方案六";
	case 7u: return "方案七";
	default: return "方案" + std::to_string(schemeId);
	}
}

inline bool ValidRequestId(std::string_view id)
{
	if (id.size() < 16 || id.size() > 64)
	{
		return false;
	}
	for (const char c : id)
	{
		const bool ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
						(c >= '0' && c <= '9') || c == '-' || c == '_';
		if (!ok)
		{
			return false;
		}
	}
	return true;
}

struct UnlockSpec
{
	bool valid{false};
	uint32_t targetSchemeId{0};
	uint64_t costDiamond{0};
	bool allowBindDiamond{false};
};

// 方案位付费阶梯（零战力纯便利，上限 5 套）：
// 方案 2: 60 灵石（允许优先绑灵+非绑补足）；方案 3/4/5: 120/180/240 非绑灵石；全档均优先扣 3413 扩容符。
inline constexpr UnlockSpec UnlockSpecForTarget(uint32_t targetSchemeId)
{
	switch (targetSchemeId)
	{
	case 2u: return UnlockSpec{true, 2u, 60u, true};
	case 3u: return UnlockSpec{true, 3u, 120u, false};
	case 4u: return UnlockSpec{true, 4u, 180u, false};
	case 5u: return UnlockSpec{true, 5u, 240u, false};
	default: return UnlockSpec{};
	}
}

struct UnlockPaymentPlan
{
	bool canAfford{false};
	bool useTalisman{false};
	uint64_t spendBindDiamond{0};
	uint64_t spendDiamond{0};
};

inline UnlockPaymentPlan PlanUnlockPayment(const UnlockSpec &spec, uint32_t talismanAvailable,
										   uint64_t bindDiamondBalance, uint64_t diamondBalance,
										   bool useBindDiamondFirst)
{
	UnlockPaymentPlan plan;
	if (!spec.valid)
	{
		return plan;
	}
	if (talismanAvailable >= 1u)
	{
		plan.canAfford = true;
		plan.useTalisman = true;
		return plan;
	}
	if (spec.allowBindDiamond && useBindDiamondFirst)
	{
		const uint64_t fromBind = std::min(bindDiamondBalance, spec.costDiamond);
		const uint64_t needDiamond = spec.costDiamond - fromBind;
		if (diamondBalance < needDiamond)
		{
			return plan;
		}
		plan.canAfford = true;
		plan.spendBindDiamond = fromBind;
		plan.spendDiamond = needDiamond;
		return plan;
	}
	if (diamondBalance < spec.costDiamond)
	{
		return plan;
	}
	plan.canAfford = true;
	plan.spendDiamond = spec.costDiamond;
	return plan;
}

inline bool IsInvisibleCodePoint(uint32_t cp)
{
	return cp == 0x20 || cp == 0xA0 || (cp >= 0x2000 && cp <= 0x200F) || cp == 0x2028 || cp == 0x2029 ||
		   cp == 0x202F || cp == 0x205F || cp == 0x2060 || cp == 0x3000 || cp == 0xFEFF;
}

inline bool DefaultSensitiveWordFilter(std::string_view text)
{
	std::string lower;
	lower.reserve(text.size());
	for (const unsigned char ch : text)
	{
		lower.push_back(static_cast<char>(std::tolower(ch)));
	}
	static constexpr std::array<std::string_view, 9> kBlockedTerms = {
		"官方", "客服", "管理员", "系统消息", "外挂", "代充", "私服", "gm", "admin",
	};
	for (const auto term : kBlockedTerms)
	{
		if (lower.find(term) != std::string::npos)
		{
			return false;
		}
	}
	return true;
}

// 名称合法性校验：UTF-8 1..12 字符、非全空白、无控制字符/非法序列/富文本尖括号、过敏感词检查。
inline bool ValidateSchemeName(std::string_view name,
							   const std::function<bool(std::string_view)> &sensitiveWordFilter = {})
{
	if (name.empty())
	{
		return false;
	}
	uint32_t codePoints = 0;
	bool hasVisible = false;
	for (std::size_t i = 0; i < name.size();)
	{
		const auto c = static_cast<unsigned char>(name[i]);
		if (c < 0x20 || c == 0x7F || c == '<' || c == '>')
		{
			return false;
		}
		uint32_t cp = c;
		std::size_t len = 1;
		uint32_t minCp = 0;
		if (c < 0x80)
		{
			len = 1;
			minCp = 0;
		}
		else if ((c & 0xE0) == 0xC0)
		{
			cp = c & 0x1F;
			len = 2;
			minCp = 0x80;
		}
		else if ((c & 0xF0) == 0xE0)
		{
			cp = c & 0x0F;
			len = 3;
			minCp = 0x800;
		}
		else if ((c & 0xF8) == 0xF0)
		{
			cp = c & 0x07;
			len = 4;
			minCp = 0x10000;
		}
		else
		{
			return false;
		}
		if (i + len > name.size())
		{
			return false;
		}
		for (std::size_t k = 1; k < len; ++k)
		{
			const auto cc = static_cast<unsigned char>(name[i + k]);
			if ((cc & 0xC0) != 0x80)
			{
				return false;
			}
			cp = (cp << 6) | (cc & 0x3F);
		}
		if (cp < minCp || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF) || (cp >= 0x80 && cp <= 0x9F))
		{
			return false;
		}
		i += len;
		++codePoints;
		if (codePoints > kMaxNameCodePoints)
		{
			return false;
		}
		if (!IsInvisibleCodePoint(cp))
		{
			hasVisible = true;
		}
	}
	if (!hasVisible)
	{
		return false;
	}
	if (sensitiveWordFilter)
	{
		return sensitiveWordFilter(name);
	}
	return DefaultSensitiveWordFilter(name);
}

enum class SwapPlanError : uint8_t
{
	kNone = 0,
	kInvalidSlot = 1,
	kDuplicateItem = 2,
};

struct EquipFromBagMove
{
	uint32_t targetSlot{0};
	uint64_t itemGuid{0};
};

struct MoveWithinEquip
{
	uint32_t fromSlot{0};
	uint32_t targetSlot{0};
	uint64_t itemGuid{0};
};

struct UnequipMove
{
	uint32_t fromSlot{0};
	uint64_t itemGuid{0};
	uint64_t pairedIncomingBagGuid{0}; // 同槽被哪一件背包新件替换（用于原位互换复原原背包格）
};

struct AtomicSwapPlan
{
	SwapPlanError error{SwapPlanError::kNone};
	std::vector<EquipFromBagMove> toEquipFromBag;
	std::vector<MoveWithinEquip> toMoveWithinEquip;
	std::vector<UnequipMove> toUnequip;
	std::vector<uint32_t> missingSlots;
	std::size_t netExtraBagSlotsNeeded{0};
};

// 两阶段原子换装纯规划：
// - 严禁同套方案内重复引用同一个 item_guid；
// - 找不到的 guid 记入 missingSlots 并保持该槽原状不动（除非该槽原穿戴件被方案内其它槽位点名移动）；
// - 同槽旧件换新件按原位互换记账，净背包格需求 = max(0, 纯卸下数 - 背包穿上数)，0 空格亦可 8v8 整套互换。
template <typename InInventoryFn>
inline AtomicSwapPlan PlanAtomicSwap(const std::map<uint32_t, uint64_t> &currentEquippedBySlot,
									 const std::map<uint32_t, uint64_t> &desiredBySlot,
									 InInventoryFn &&inInventory)
{
	AtomicSwapPlan plan;

	std::map<uint64_t, uint32_t> currentSlotOfGuid;
	for (const auto &[slot, guid] : currentEquippedBySlot)
	{
		if (IsFormalSlot(slot) && guid != 0)
		{
			currentSlotOfGuid[guid] = slot;
		}
	}

	std::set<uint64_t> seenDesiredGuids;
	for (const auto &[slot, guid] : desiredBySlot)
	{
		if (!IsFormalSlot(slot))
		{
			plan.error = SwapPlanError::kInvalidSlot;
			return plan;
		}
		if (guid != 0 && !seenDesiredGuids.insert(guid).second)
		{
			plan.error = SwapPlanError::kDuplicateItem;
			return plan;
		}
	}

	// 第一遍：确定哪些目标件真实在场（装备栏或人物背包），哪些缺失。
	std::map<uint32_t, uint64_t> presentTargetBySlot;
	std::set<uint64_t> claimedByTarget;
	std::set<uint32_t> missingSet;

	for (const uint32_t slot : kFormalSlots)
	{
		const auto it = desiredBySlot.find(slot);
		if (it == desiredBySlot.end() || it->second == 0)
		{
			continue;
		}
		const uint64_t guid = it->second;
		if (currentSlotOfGuid.contains(guid) || inInventory(guid))
		{
			presentTargetBySlot[slot] = guid;
			claimedByTarget.insert(guid);
		}
		else
		{
			plan.missingSlots.push_back(slot);
			missingSet.insert(slot);
		}
	}

	// 第二遍：缺失槽尽量保持原件不动（前提是该原件未被方案中其它槽位点名穿走）。
	std::map<uint32_t, uint64_t> finalBySlot = presentTargetBySlot;
	std::set<uint64_t> finalGuids = claimedByTarget;
	for (const uint32_t slot : missingSet)
	{
		const auto curIt = currentEquippedBySlot.find(slot);
		if (curIt != currentEquippedBySlot.end() && curIt->second != 0 && !claimedByTarget.contains(curIt->second))
		{
			finalBySlot[slot] = curIt->second;
			finalGuids.insert(curIt->second);
		}
	}

	// 第三遍：拆分出背包穿上、装备栏内挪槽、以及需卸回背包的旧件。
	std::map<uint32_t, uint64_t> incomingBagGuidBySlot;
	for (const uint32_t slot : kFormalSlots)
	{
		const auto finIt = finalBySlot.find(slot);
		if (finIt == finalBySlot.end() || finIt->second == 0)
		{
			continue;
		}
		const uint64_t targetGuid = finIt->second;
		const auto curSlotIt = currentSlotOfGuid.find(targetGuid);
		if (curSlotIt == currentSlotOfGuid.end())
		{
			plan.toEquipFromBag.push_back(EquipFromBagMove{slot, targetGuid});
			incomingBagGuidBySlot[slot] = targetGuid;
		}
		else if (curSlotIt->second != slot)
		{
			plan.toMoveWithinEquip.push_back(MoveWithinEquip{curSlotIt->second, slot, targetGuid});
		}
	}

	for (const uint32_t slot : kFormalSlots)
	{
		const auto curIt = currentEquippedBySlot.find(slot);
		if (curIt == currentEquippedBySlot.end() || curIt->second == 0)
		{
			continue;
		}
		const uint64_t oldGuid = curIt->second;
		if (finalGuids.contains(oldGuid))
		{
			continue;
		}
		const auto incIt = incomingBagGuidBySlot.find(slot);
		const uint64_t pairedIncoming = (incIt != incomingBagGuidBySlot.end()) ? incIt->second : 0u;
		plan.toUnequip.push_back(UnequipMove{slot, oldGuid, pairedIncoming});
	}

	if (plan.toUnequip.size() > plan.toEquipFromBag.size())
	{
		plan.netExtraBagSlotsNeeded = plan.toUnequip.size() - plan.toEquipFromBag.size();
	}
	else
	{
		plan.netExtraBagSlotsNeeded = 0;
	}
	return plan;
}

} // namespace equiploadout
