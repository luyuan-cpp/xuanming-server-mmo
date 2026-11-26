#pragma once

// 官方灵石百宝阁的纯业务规则(不依赖 ECS / 表单例 / 时钟源,供 C++ scene 与单测共用)。
// 设计文档:docs/design/spirit-shop-and-starter-packs-v1.md、docs/planning/spirit-shop-v2-festival-and-bundles.md。

#include <cstddef>
#include <cstdint>
#include <limits>
#include <string>
#include <vector>

#include "proto/common/component/player_spirit_shop_comp.pb.h"
#include "proto/scene/player_spirit_shop.pb.h"

namespace spirit_shop_rules
{
	inline constexpr uint32_t kResetLifetime = 0;
	inline constexpr uint32_t kResetWeekly = 1;

	inline constexpr uint32_t kGrantModeAll = 0;
	inline constexpr uint32_t kGrantModeChooseOne = 1;
	inline constexpr std::size_t kMaxGoodsItems = 4;

	inline constexpr uint64_t kMaxUnitPrice = 1'000'000'000ULL;
	inline constexpr uint32_t kMaxBuyCountPerRequest = 99;
	inline constexpr uint32_t kMaxLimitGroupWeight = 1'000'000;
	inline constexpr int kMaxGoodsRecords = 512;
	inline constexpr int kMaxGroupRecords = 128;

	// 与 PlayerEquipmentMaterialReward 同一游戏周口径:UTC+8 周一 05:00 重置。
	// 1970-01-05 05:00:00 +08:00 == 1970-01-04 21:00:00 UTC == 334,800,000 ms。
	inline constexpr uint64_t kWeekMs = 7ULL * 24ULL * 3600ULL * 1000ULL;
	inline constexpr uint64_t kFirstMonday05Utc8Ms = 334800000ULL;

	struct ResolvedItem
	{
		uint32_t itemConfigId{0};
		uint32_t itemCount{0};
		bool bound{true};
	};

	struct ResolvedLimitGroup
	{
		uint32_t groupId{0};
		std::string name;
		uint32_t resetKind{kResetLifetime};
		uint32_t maxWeight{0};
	};

	struct ResolvedGoods
	{
		uint32_t goodsId{0};
		uint32_t tabId{0};
		uint32_t sortOrder{0};
		std::string name;
		std::string description;
		std::string iconKey;
		std::string badgeText;
		uint32_t currencyType{1};
		uint64_t unitPrice{0};
		uint32_t maxBuyPerRequest{1};
		std::vector<ResolvedItem> items;
		uint32_t limitResetKind{kResetLifetime};
		uint32_t limitCount{0};
		uint32_t limitGroupId{0};
		uint32_t limitGroupWeight{0};
		uint64_t startTimeMs{0};
		uint64_t endTimeMs{0};
		uint32_t grantMode{kGrantModeAll};
		uint64_t originalPrice{0};
		uint32_t minLevel{0};
		uint32_t maxLevel{0};
	};

	inline bool IsValidResetKind(uint32_t resetKind)
	{
		return resetKind == kResetLifetime || resetKind == kResetWeekly;
	}

	inline bool IsValidGrantMode(uint32_t grantMode)
	{
		return grantMode == kGrantModeAll || grantMode == kGrantModeChooseOne;
	}

	// 定时上下架判定:左闭右开 [startTimeMs, endTimeMs);任一端为 0 表示该侧不限。
	inline bool IsInSaleWindow(uint64_t startTimeMs, uint64_t endTimeMs, uint64_t nowMs)
	{
		if (startTimeMs > 0 && nowMs < startTimeMs)
		{
			return false;
		}
		if (endTimeMs > 0 && nowMs >= endTimeMs)
		{
			return false;
		}
		return true;
	}

	// 角色有效等级阶段解锁判定:任一端为 0 表示该侧不限。
	inline bool IsLevelEligible(uint32_t effectiveLevel, uint32_t minLevel, uint32_t maxLevel)
	{
		if (minLevel > 0 && effectiveLevel < minLevel)
		{
			return false;
		}
		if (maxLevel > 0 && effectiveLevel > maxLevel)
		{
			return false;
		}
		return true;
	}

	// 根据发放模式解析本次实际发入背包的物品列表:
	// - ALL(0):chosenItemIndex 须为 0,发放 goods.items 全部 1..4 项;
	// - CHOOSE_ONE(1):chosenItemIndex 须在 [0, goods.items.size()),仅发放所选 1 项。
	inline bool ResolveSelectedItems(const ResolvedGoods &goods, uint32_t chosenItemIndex,
									 std::vector<ResolvedItem> &outSelected)
	{
		outSelected.clear();
		if (goods.items.empty())
		{
			return false;
		}
		if (goods.grantMode == kGrantModeAll)
		{
			if (chosenItemIndex != 0)
			{
				return false;
			}
			outSelected = goods.items;
			return true;
		}
		if (goods.grantMode == kGrantModeChooseOne)
		{
			if (goods.items.size() < 2 || chosenItemIndex >= goods.items.size())
			{
				return false;
			}
			outSelected.push_back(goods.items[chosenItemIndex]);
			return true;
		}
		return false;
	}

	inline bool ResolveWeek(uint64_t nowMs, uint64_t &startMs, uint64_t &nextMs)
	{
		if (nowMs < kFirstMonday05Utc8Ms)
		{
			return false;
		}
		const uint64_t offset = (nowMs - kFirstMonday05Utc8Ms) % kWeekMs;
		const uint64_t start = nowMs - offset;
		if (start > std::numeric_limits<uint64_t>::max() - kWeekMs)
		{
			return false;
		}
		startMs = start;
		nextMs = start + kWeekMs;
		return true;
	}

	inline bool TotalCost(uint64_t unitPrice, uint32_t count, int64_t &outCost)
	{
		if (unitPrice == 0 || unitPrice > kMaxUnitPrice || count == 0 || count > kMaxBuyCountPerRequest)
		{
			return false;
		}
		const uint64_t total = unitPrice * static_cast<uint64_t>(count);
		if (total > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()))
		{
			return false;
		}
		outCost = static_cast<int64_t>(total);
		return true;
	}

	inline bool TotalGroupWeight(uint32_t weightPerUnit, uint32_t count, uint32_t &outWeight)
	{
		if (weightPerUnit == 0 || weightPerUnit > kMaxLimitGroupWeight || count == 0 || count > kMaxBuyCountPerRequest)
		{
			return false;
		}
		const uint64_t total = static_cast<uint64_t>(weightPerUnit) * static_cast<uint64_t>(count);
		if (total > std::numeric_limits<uint32_t>::max())
		{
			return false;
		}
		outWeight = static_cast<uint32_t>(total);
		return true;
	}

	// 读取某商品当前生效的已购份数。周限购遇时钟回拨(weekStart < record.week_starts_at_ms)保守沿用已购数,绝不趁机清零。
	inline uint32_t BoughtCount(const PlayerSpiritShopComp &comp, uint32_t goodsId, uint32_t resetKind, uint64_t weekStartMs)
	{
		for (const auto &rec : comp.goods_records())
		{
			if (rec.goods_id() != goodsId)
			{
				continue;
			}
			if (resetKind == kResetWeekly)
			{
				if (weekStartMs > rec.week_starts_at_ms())
				{
					return 0;
				}
				return rec.weekly_bought();
			}
			return rec.lifetime_bought();
		}
		return 0;
	}

	// 读取某共享限购组当前生效的已用权重(抗时钟回拨同上)。
	inline uint32_t UsedGroupWeight(const PlayerSpiritShopComp &comp, uint32_t groupId, uint32_t resetKind, uint64_t weekStartMs)
	{
		for (const auto &rec : comp.group_records())
		{
			if (rec.group_id() != groupId)
			{
				continue;
			}
			if (resetKind == kResetWeekly)
			{
				if (weekStartMs > rec.week_starts_at_ms())
				{
					return 0;
				}
				return rec.weekly_used();
			}
			return rec.lifetime_used();
		}
		return 0;
	}

	inline bool WouldExceedLimit(uint32_t used, uint32_t delta, uint32_t limit)
	{
		if (limit == 0)
		{
			return false;
		}
		return static_cast<uint64_t>(used) + static_cast<uint64_t>(delta) > static_cast<uint64_t>(limit);
	}

	// 核对客户端报价防呆中的物品列表:条数、顺序、模板 ID、数量、绑定态必须逐项完全一致。
	inline bool ExpectedItemsMatch(const BuySpiritShopGoodsRequest &request, const std::vector<ResolvedItem> &actual)
	{
		if (static_cast<std::size_t>(request.expected_items_size()) != actual.size())
		{
			return false;
		}
		for (std::size_t i = 0; i < actual.size(); ++i)
		{
			const auto &exp = request.expected_items(static_cast<int>(i));
			const auto &act = actual[i];
			if (exp.item_config_id() != act.itemConfigId ||
				exp.item_count() != act.itemCount ||
				exp.bound() != act.bound)
			{
				return false;
			}
		}
		return true;
	}

	// 校验持久化限购组件结构合法性(防畸形存档或超容)。返回空串表示合法。
	inline std::string ValidateComp(const PlayerSpiritShopComp &comp)
	{
		if (comp.goods_records_size() > kMaxGoodsRecords)
		{
			return "goods_records exceeds capacity";
		}
		if (comp.group_records_size() > kMaxGroupRecords)
		{
			return "group_records exceeds capacity";
		}
		for (int i = 0; i < comp.goods_records_size(); ++i)
		{
			const uint32_t id = comp.goods_records(i).goods_id();
			if (id == 0)
			{
				return "goods_records contains zero goods_id";
			}
			for (int j = i + 1; j < comp.goods_records_size(); ++j)
			{
				if (comp.goods_records(j).goods_id() == id)
				{
					return "goods_records contains duplicate goods_id";
				}
			}
		}
		for (int i = 0; i < comp.group_records_size(); ++i)
		{
			const uint32_t id = comp.group_records(i).group_id();
			if (id == 0)
			{
				return "group_records contains zero group_id";
			}
			for (int j = i + 1; j < comp.group_records_size(); ++j)
			{
				if (comp.group_records(j).group_id() == id)
				{
					return "group_records contains duplicate group_id";
				}
			}
		}
		return {};
	}

	// 在候选中记录一次成交(单品计数、共享组权重、revision + 1)。
	inline bool RecordPurchase(PlayerSpiritShopComp &comp, uint32_t goodsId, uint32_t count,
							   uint32_t goodsLimitCount, uint32_t groupId, uint32_t addGroupWeight,
							   uint64_t weekStartMs)
	{
		if (comp.shop_revision() == std::numeric_limits<uint64_t>::max())
		{
			return false;
		}
		if (goodsLimitCount > 0)
		{
			SpiritShopGoodsPurchaseRecord *target = nullptr;
			for (int i = 0; i < comp.goods_records_size(); ++i)
			{
				if (comp.goods_records(i).goods_id() == goodsId)
				{
					target = comp.mutable_goods_records(i);
					break;
				}
			}
			if (target == nullptr)
			{
				if (comp.goods_records_size() >= kMaxGoodsRecords)
				{
					return false;
				}
				target = comp.add_goods_records();
				target->set_goods_id(goodsId);
			}
			const uint64_t nextLifetime = static_cast<uint64_t>(target->lifetime_bought()) + count;
			if (nextLifetime > std::numeric_limits<uint32_t>::max())
			{
				return false;
			}
			target->set_lifetime_bought(static_cast<uint32_t>(nextLifetime));
			if (weekStartMs > target->week_starts_at_ms())
			{
				target->set_week_starts_at_ms(weekStartMs);
				target->set_weekly_bought(count);
			}
			else
			{
				const uint64_t nextWeekly = static_cast<uint64_t>(target->weekly_bought()) + count;
				if (nextWeekly > std::numeric_limits<uint32_t>::max())
				{
					return false;
				}
				target->set_weekly_bought(static_cast<uint32_t>(nextWeekly));
			}
		}
		if (groupId > 0 && addGroupWeight > 0)
		{
			SpiritShopGroupQuotaRecord *target = nullptr;
			for (int i = 0; i < comp.group_records_size(); ++i)
			{
				if (comp.group_records(i).group_id() == groupId)
				{
					target = comp.mutable_group_records(i);
					break;
				}
			}
			if (target == nullptr)
			{
				if (comp.group_records_size() >= kMaxGroupRecords)
				{
					return false;
				}
				target = comp.add_group_records();
				target->set_group_id(groupId);
			}
			const uint64_t nextLifetime = static_cast<uint64_t>(target->lifetime_used()) + addGroupWeight;
			if (nextLifetime > std::numeric_limits<uint32_t>::max())
			{
				return false;
			}
			target->set_lifetime_used(static_cast<uint32_t>(nextLifetime));
			if (weekStartMs > target->week_starts_at_ms())
			{
				target->set_week_starts_at_ms(weekStartMs);
				target->set_weekly_used(addGroupWeight);
			}
			else
			{
				const uint64_t nextWeekly = static_cast<uint64_t>(target->weekly_used()) + addGroupWeight;
				if (nextWeekly > std::numeric_limits<uint32_t>::max())
				{
					return false;
				}
				target->set_weekly_used(static_cast<uint32_t>(nextWeekly));
			}
		}
		comp.set_shop_revision(comp.shop_revision() + 1);
		return true;
	}
} // namespace spirit_shop_rules
