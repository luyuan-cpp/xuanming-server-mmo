#pragma once

// 游历手记（不肝不逼氪双轨活动）纯业务规则（不依赖 ECS / 表单例 / 时钟源，供 C++ scene 与单测共用）。
// 设计文档：docs/design/activity-travel-pass-and-showcase-v1.md。

#include <cstdint>
#include <limits>
#include <string>
#include <string_view>

#include "proto/common/component/player_travel_pass_comp.pb.h"

namespace travel_pass_rules
{
	inline constexpr uint32_t kMaxSeasons = 32;
	inline constexpr uint32_t kMaxTiersPerSeason = 64;
	inline constexpr uint64_t kMaxUnitPrice = 1'000'000'000ULL;
	inline constexpr std::size_t kMaxCosmeticKeyLength = 64;
	inline constexpr int kMaxCosmeticsPerSeason = 64;

	inline constexpr uint32_t kCosmeticSlotFrame = 1;
	inline constexpr uint32_t kCosmeticSlotTitle = 2;

	// 活动中心固定目录入口 ID（远离普通任务与瑞兽迎春号段）。
	inline constexpr uint32_t kActivityIdTravelPass = 900001u;
	inline constexpr uint32_t kActivityIdAppearanceShowcase = 900002u;
	inline constexpr uint32_t kActivityIdSpiritShopShowcase = 900003u;

	inline bool IsFrameKey(std::string_view key)
	{
		return key.size() > 6 && key.size() <= kMaxCosmeticKeyLength && key.substr(0, 6) == "frame_";
	}

	inline bool IsTitleKey(std::string_view key)
	{
		return key.size() > 6 && key.size() <= kMaxCosmeticKeyLength && key.substr(0, 6) == "title_";
	}

	inline bool IsValidCosmeticKey(std::string_view key)
	{
		return IsFrameKey(key) || IsTitleKey(key);
	}

	inline uint64_t TierBit(uint32_t tierIndex)
	{
		if (tierIndex == 0 || tierIndex > kMaxTiersPerSeason)
		{
			return 0ULL;
		}
		return uint64_t{1} << (tierIndex - 1u);
	}

	inline bool IsTierClaimed(uint64_t mask, uint32_t tierIndex)
	{
		const uint64_t bit = TierBit(tierIndex);
		return bit != 0ULL && (mask & bit) != 0ULL;
	}

	inline bool IsWindowOpen(uint64_t startsAtMs, uint64_t endsAtMs, uint64_t nowMs)
	{
		if (startsAtMs != 0 && nowMs < startsAtMs)
		{
			return false;
		}
		if (endsAtMs != 0 && nowMs >= endsAtMs)
		{
			return false;
		}
		return true;
	}

	inline bool WindowsOverlap(uint64_t startA, uint64_t endA, uint64_t startB, uint64_t endB)
	{
		const uint64_t effectiveEndA = endA == 0 ? std::numeric_limits<uint64_t>::max() : endA;
		const uint64_t effectiveEndB = endB == 0 ? std::numeric_limits<uint64_t>::max() : endB;
		return startA < effectiveEndB && startB < effectiveEndA;
	}

	inline const TravelPassSeasonProgress *FindSeason(const PlayerTravelPassComp &comp, uint32_t passId)
	{
		for (const auto &season : comp.seasons())
		{
			if (season.pass_id() == passId)
			{
				return &season;
			}
		}
		return nullptr;
	}

	inline TravelPassSeasonProgress *FindMutableSeason(PlayerTravelPassComp &comp, uint32_t passId)
	{
		for (int i = 0; i < comp.seasons_size(); ++i)
		{
			if (comp.seasons(i).pass_id() == passId)
			{
				return comp.mutable_seasons(i);
			}
		}
		return nullptr;
	}

	inline TravelPassSeasonProgress *FindOrAddSeason(PlayerTravelPassComp &comp, uint32_t passId)
	{
		if (auto *existing = FindMutableSeason(comp, passId); existing != nullptr)
		{
			return existing;
		}
		if (static_cast<uint32_t>(comp.seasons_size()) >= kMaxSeasons)
		{
			return nullptr;
		}
		auto *created = comp.add_seasons();
		created->set_pass_id(passId);
		return created;
	}

	inline bool HasCosmeticKey(const PlayerTravelPassComp &comp, std::string_view key)
	{
		if (key.empty())
		{
			return false;
		}
		for (const auto &season : comp.seasons())
		{
			for (const auto &owned : season.unlocked_cosmetic_keys())
			{
				if (owned == key)
				{
					return true;
				}
			}
		}
		return false;
	}

	inline bool AddCosmeticKey(TravelPassSeasonProgress &season, std::string_view key)
	{
		if (key.empty())
		{
			return true;
		}
		for (const auto &existing : season.unlocked_cosmetic_keys())
		{
			if (existing == key)
			{
				return true;
			}
		}
		if (season.unlocked_cosmetic_keys_size() >= kMaxCosmeticsPerSeason)
		{
			return false;
		}
		season.add_unlocked_cosmetic_keys(std::string(key));
		return true;
	}

	// 校验持久游历手记组件结构合法性（防畸形存档或超容）。返回空串表示合法。
	inline std::string ValidateComp(const PlayerTravelPassComp &comp)
	{
		if (static_cast<uint32_t>(comp.seasons_size()) > kMaxSeasons)
		{
			return "seasons exceeds capacity";
		}
		for (int i = 0; i < comp.seasons_size(); ++i)
		{
			const auto &s = comp.seasons(i);
			if (s.pass_id() == 0)
			{
				return "seasons contains zero pass_id";
			}
			for (int j = i + 1; j < comp.seasons_size(); ++j)
			{
				if (comp.seasons(j).pass_id() == s.pass_id())
				{
					return "seasons contains duplicate pass_id";
				}
			}
			if (s.unlocked_cosmetic_keys_size() > kMaxCosmeticsPerSeason)
			{
				return "unlocked_cosmetic_keys exceeds capacity";
			}
			for (const auto &key : s.unlocked_cosmetic_keys())
			{
				if (!IsValidCosmeticKey(key))
				{
					return "unlocked_cosmetic_keys contains invalid key";
				}
			}
		}
		if (!comp.equipped_frame_key().empty())
		{
			if (!IsFrameKey(comp.equipped_frame_key()) || !HasCosmeticKey(comp, comp.equipped_frame_key()))
			{
				return "equipped_frame_key is invalid or unowned";
			}
		}
		if (!comp.equipped_title_key().empty())
		{
			if (!IsTitleKey(comp.equipped_title_key()) || !HasCosmeticKey(comp, comp.equipped_title_key()))
			{
				return "equipped_title_key is invalid or unowned";
			}
		}
		return {};
	}
} // namespace travel_pass_rules
