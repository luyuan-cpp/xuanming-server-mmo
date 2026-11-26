#pragma once

// 游历手记（不肝不逼氪双轨活动）与活动体验种草目录聚合（docs/design/activity-travel-pass-and-showcase-v1.md）。
// 线程：只在 scene 主 EventLoop 线程调用，无全局可变状态，不新增线程。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

class BattleSettlementData;
class ClaimTravelPassRewardRequest;
class ClaimTravelPassRewardResponse;
class EquipTravelPassCosmeticRequest;
class EquipTravelPassCosmeticResponse;
class GetActivityListResponse;
class PlayerTravelPassComp;
class TravelPassSnapshot;
class UnlockTravelPassPremiumRequest;
class UnlockTravelPassPremiumResponse;

class PlayerTravelPassSystem
{
public:
	// 启动与表热更后校验 TravelPassSeason / TravelPassTier 配置行（fail-closed，不崩服）。
	static bool ValidateTables();

	// 判定战斗结算是否具备游历印积点资格（复用正式战斗与 PVP 防挂机门槛，逃跑/作废/切磋=0）。
	static bool IsEligibleBattle(const BattleSettlementData &settlement);

	// 在战斗收件箱结算原子帧内准备游历手记候选进度（支持开放期次及已解锁典藏未满点的历史期次）。
	static uint32_t PrepareBattleCredit(const PlayerTravelPassComp &current,
										const BattleSettlementData &settlement,
										uint64_t nowMs,
										PlayerTravelPassComp &candidate,
										bool &pointsIncreased);

	// 纯只读查询游历手记快照（绝不创建组件）。requestedPassId == 0 时自动选当前期或未领完历史期。
	static uint32_t GetState(entt::entity player, uint64_t nowMs, uint32_t requestedPassId,
							 ::TravelPassSnapshot &out);

	// 真实充值灵石解锁典藏进阶轨（强制非 0 幂等键 + 报价/版本防呆 + 解锁后永不过期）。
	static uint32_t UnlockPremium(entt::entity player,
								  const ::UnlockTravelPassPremiumRequest &request,
								  uint64_t nowMs,
								  ::UnlockTravelPassPremiumResponse &response);

	// 领取单档或一键全领当前所有可领免费/已解锁典藏档位（全部物品强制 bound=true，整批原子校验与入包）。
	static uint32_t ClaimReward(entt::entity player,
								const ::ClaimTravelPassRewardRequest &request,
								uint64_t nowMs,
								::ClaimTravelPassRewardResponse &response);

	// 零消耗佩戴或卸下已解锁的纪念头像框/称谓（零战力纯外显）。
	static uint32_t EquipCosmetic(entt::entity player,
								  uint64_t nowMs,
								  const ::EquipTravelPassCosmeticRequest &request,
								  ::EquipTravelPassCosmeticResponse &response);

	// 向 GetActivityList 目录追加「青云游历手记」「留灯风物 · 免费试穿」「灵石百宝阁 · 轻便利」卡片。
	// 纯只读，仅在对应组件与有效表均就绪时追加，不破坏只测任务的旧单测夹具。
	static void AppendActivityCatalogEntries(entt::entity player, uint64_t nowMs, ::GetActivityListResponse &out);
};
