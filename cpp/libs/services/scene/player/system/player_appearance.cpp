#include "player_appearance.h"

#include <algorithm>
#include <cstdlib>
#include <limits>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "actor/actor_net_id.h"
#include "core/security/service_call_auth.h"
#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "network/client_message_sender.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/system/appearance_rules.h"
#include "player/system/client_request_ledger.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "rpc/service_metadata/player_appearance_service_metadata.h"
#include "spatial/system/view.h"
#include "table/code/appearancerule_table.h"
#include "table/code/appearanceseries_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/code/pet_table.h"
#include "table/code/petcosmetic_table.h"
#include "table/code/skill_table.h"
#include "table/code/skillvfxcosmetic_table.h"
#include "table/proto/tip/appearance_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"

namespace
{
	constexpr const char *kSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";

	struct DirectorySyncState
	{
		std::string targetAppearanceId;
		bool rpcInFlight{false};
		uint32_t retryAttempts{0};
		uint64_t nextRetryAtMs{0};
	};

	// 运行期在途试用表(playerId -> trialExpiresAtSec):表空时 Tick O(1) 返回。
	thread_local std::unordered_map<Guid, uint64_t> tlsActiveTrials;
	// 运行期目录永久外观同步表(仅在永久外观变动且待/正在同步时有条目)。
	thread_local std::unordered_map<Guid, DirectorySyncState> tlsDirectorySyncs;
	thread_local PlayerAppearanceSystem::TestHooks tlsTestHooks;

	bool LoadCurrentRule(player_appearance::Rule &rule, const char *&problem)
	{
		const auto &rows = AppearanceRuleTableManager::Instance().FindAll().data();
		if (rows.empty())
		{
			problem = "AppearanceRule table is empty";
			return false;
		}
		const auto &row = *rows.begin();
		rule = player_appearance::Rule{
			row.card_item_id(),
			row.card_item_count(),
			row.trial_duration_seconds(),
			row.mall_sku_id(),
			row.mall_price_fen(),
		};
		problem = player_appearance::RuleProblem(rule);
		if (problem != nullptr)
		{
			return false;
		}
		if (!ItemTableManager::Instance().Exists(rule.cardItemId))
		{
			problem = "card_item_id does not exist in Item table";
			return false;
		}
		return true;
	}

	struct EnabledAppearanceMeta
	{
		uint32_t configId{0};
		bool cardUnlockable{false};
	};

	// 按 appearance_id 查找已启用的配置行,返回其永久主键与是否允许形象卡兑换(找不到或未启用 configId=0)。
	// 不返回行指针,遵循热更快照边界约定。
	EnabledAppearanceMeta FindEnabledAppearance(std::string_view appearanceId)
	{
		if (appearanceId.empty())
		{
			return {};
		}
		const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
		for (const auto &row : rows)
		{
			if (row.enabled() != 0 && row.appearance_id() == appearanceId)
			{
				return EnabledAppearanceMeta{row.id(), row.card_unlockable() != 0};
			}
		}
		return {};
	}

	// 按 appearance_id 查找任意配置行(含 enabled=0),用于首次切换/读面板时保留旧永久外观的拥有位。
	uint32_t FindAppearanceConfigIdAny(std::string_view appearanceId)
	{
		if (appearanceId.empty())
		{
			return 0;
		}
		const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
		for (const auto &row : rows)
		{
			if (row.appearance_id() == appearanceId)
			{
				return row.id();
			}
		}
		return 0;
	}

	uint32_t CheckMutabilityTip(entt::entity player)
	{
		switch (player_mutability::ClassifyPlayerMutability(player))
		{
		case player_mutability::PlayerMutability::kMutable:
			return kSuccess;
		case player_mutability::PlayerMutability::kFrozen:
		case player_mutability::PlayerMutability::kTravelHandoff:
		case player_mutability::PlayerMutability::kExiting:
			return static_cast<uint32_t>(kAppearanceFrozen);
		case player_mutability::PlayerMutability::kOwnerEpochUnknown:
		default:
			return static_cast<uint32_t>(kServiceUnavailable);
		}
	}

	void RequestSave(entt::entity player, const char *reason)
	{
		if (tlsTestHooks.save)
		{
			tlsTestHooks.save(player, reason);
			return;
		}
		PlayerLifecycleSystem::RequestKeyProgressSave(player, reason);
	}

	void BroadcastAppearanceChanged(entt::entity player,
									Guid playerId,
									const std::string &effectiveAppearanceId,
									bool isTrial,
									uint64_t trialExpiresAt)
	{
		ActorAppearanceChangedS2C msg;
		msg.set_entity(actor_net_id::Of(tlsEcs.actorRegistry, player));
		msg.set_player_id(playerId);
		msg.set_appearance_id(effectiveAppearanceId);
		msg.set_is_trial(isTrial);
		msg.set_trial_expires_at(isTrial ? trialExpiresAt : 0);

		if (tlsTestHooks.broadcast)
		{
			tlsTestHooks.broadcast(player, msg);
			return;
		}
		SendToPlayerClient(SceneAppearanceClientPlayerNotifyActorAppearanceChangedMessageId, msg, player);
		ViewSystem::BroadcastMessageToVisiblePlayers(
			player, SceneAppearanceClientPlayerNotifyActorAppearanceChangedMessageId, msg);
	}

	void PushStateNotify(entt::entity player, const AppearanceStateNotify &notify)
	{
		if (tlsTestHooks.notify)
		{
			tlsTestHooks.notify(player, notify);
			return;
		}
		SendToPlayerClient(SceneAppearanceClientPlayerNotifyAppearanceStateMessageId, notify, player);
	}

	entt::entity PickReadyDataServiceNode()
	{
		auto &registry = tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService);
		for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
		{
			const auto &channel = registry.get<MuduoGrpcChannelPtr>(entity);
			if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady)
			{
				return entity;
			}
		}
		return entt::null;
	}

	bool SendUpdateRoleAppearanceRpc(data_service::UpdateRoleAppearanceRequest &request,
									 uint64_t nowMs,
									 PlayerAppearanceSystem::DirectorySyncDone done)
	{
		if (tlsTestHooks.directorySync)
		{
			return tlsTestHooks.directorySync(request, std::move(done));
		}
		const char *secret = std::getenv(kSecretEnv);
		if (secret == nullptr || !service_call_auth::Sign(
									 data_service::kDataServiceUpdateRoleAppearanceGrpcPath,
									 request,
									 *request.mutable_auth(),
									 "scene",
									 secret,
									 nowMs))
		{
			LOG_ERROR << "[PlayerAppearance] failed to sign UpdateRoleAppearance: missing or invalid " << kSecretEnv;
			return false;
		}
		const entt::entity node = PickReadyDataServiceNode();
		if (node == entt::null)
		{
			LOG_WARN << "[PlayerAppearance] no ready data_service node for UpdateRoleAppearance: player_id="
					 << request.player_id();
			return false;
		}
		data_service::SendDataServiceUpdateRoleAppearance(
			tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService),
			node,
			request,
			[done = std::move(done)](const GrpcCallOutcome &outcome,
									 const data_service::UpdateRoleAppearanceResponse *reply)
			{
				done(outcome.ok(), reply);
			});
		return true;
	}

	void DispatchDirectorySync(Guid playerId, uint64_t nowMs)
	{
		auto it = tlsDirectorySyncs.find(playerId);
		if (it == tlsDirectorySyncs.end() || it->second.rpcInFlight || it->second.targetAppearanceId.empty())
		{
			return;
		}
		const std::string sentAppearance = it->second.targetAppearanceId;
		data_service::UpdateRoleAppearanceRequest req;
		req.set_player_id(playerId);
		req.set_appearance_id(sentAppearance);

		it->second.rpcInFlight = true;
		const bool sent = SendUpdateRoleAppearanceRpc(
			req,
			nowMs,
			[playerId, sentAppearance](bool callOk, const data_service::UpdateRoleAppearanceResponse *reply)
			{
				auto syncIt = tlsDirectorySyncs.find(playerId);
				if (syncIt == tlsDirectorySyncs.end())
				{
					return;
				}
				syncIt->second.rpcInFlight = false;
				const uint64_t callbackNowMs = TimeSystem::NowMillisecondsUTC();
				if (callOk && reply != nullptr)
				{
					// 如果在途期间又切了一次永久外观,立即补发最新目标;否则同步完成。
					if (syncIt->second.targetAppearanceId == sentAppearance)
					{
						tlsDirectorySyncs.erase(syncIt);
						LOG_INFO << "[PlayerAppearance] metric=directory_appearance_synced player_id=" << playerId
								 << " appearance_id=" << sentAppearance << " updated=" << reply->updated();
					}
					else
					{
						syncIt->second.retryAttempts = 0;
						syncIt->second.nextRetryAtMs = 0;
						DispatchDirectorySync(playerId, callbackNowMs);
					}
					return;
				}
				++syncIt->second.retryAttempts;
				syncIt->second.nextRetryAtMs =
					callbackNowMs + player_appearance::DirectorySyncRetryDelayMs(syncIt->second.retryAttempts);
				LOG_WARN << "[PlayerAppearance] metric=directory_appearance_sync_retry player_id=" << playerId
						 << " appearance_id=" << sentAppearance << " attempts=" << syncIt->second.retryAttempts;
			});
		if (!sent)
		{
			// 再次查找以防单测同步回调修改了 map
			auto failIt = tlsDirectorySyncs.find(playerId);
			if (failIt != tlsDirectorySyncs.end())
			{
				failIt->second.rpcInFlight = false;
				++failIt->second.retryAttempts;
				failIt->second.nextRetryAtMs =
					nowMs + player_appearance::DirectorySyncRetryDelayMs(failIt->second.retryAttempts);
			}
		}
	}

	uint32_t OwnedCardCount(entt::entity player, uint32_t cardItemId)
	{
		if (cardItemId == 0)
		{
			return 0;
		}
		auto &registry = tlsEcs.actorRegistry;
		if (!registry.valid(player))
		{
			return 0;
		}
		const auto *bags = registry.try_get<PlayerBagsComp>(player);
		if (bags == nullptr)
		{
			return 0;
		}
		const uint64_t count = BagService::RemovableCount(player, bags->bags[kInventory], cardItemId);
		return count > std::numeric_limits<uint32_t>::max()
				   ? std::numeric_limits<uint32_t>::max()
				   : static_cast<uint32_t>(count);
	}

	// 检查目标技能是否对当前玩家可用(需存在于 Skill 表、职业匹配且已习得或为赛季免学技能)。
	bool PlayerHasUsableSkill(entt::entity player, uint32_t skillTableId)
	{
		if (skillTableId == 0)
		{
			return false;
		}
		const auto *skillRow = SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
		if (skillRow == nullptr)
		{
			return false;
		}
		const auto &registry = tlsEcs.actorRegistry;
		if (!registry.valid(player))
		{
			return false;
		}
		const uint32_t skillClassId = skillRow->class_id();
		if (skillClassId != 0)
		{
			if (const auto *cls = registry.try_get<PlayerUint32Comp>(player); cls != nullptr && cls->class_() != 0 && cls->class_() != skillClassId)
			{
				return false;
			}
		}
		if (skillRow->season_id() != 0)
		{
			return true;
		}
		const auto *skills = registry.try_get<PlayerSkillListComp>(player);
		if (skills == nullptr)
		{
			return false;
		}
		for (const auto &entry : skills->skill_list())
		{
			if (entry.skill_table_id() != skillTableId)
			{
				continue;
			}
			if (skillClassId == 0 || entry.rank() > 0)
			{
				return true;
			}
		}
		return false;
	}
} // namespace

bool PlayerAppearanceSystem::ValidateTables()
{
	player_appearance::Rule rule;
	const char *problem = nullptr;
	if (!LoadCurrentRule(rule, problem))
	{
		LOG_ERROR << "[PlayerAppearance] table validation failed: " << problem;
		return false;
	}
	std::unordered_set<uint32_t> enabledSeriesIds;
	std::unordered_set<std::string> enabledSeriesKeys;
	for (const auto &series : AppearanceSeriesTableManager::Instance().FindAll().data())
	{
		if (series.id() == 0 || series.series_key().empty() || series.name().empty())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: invalid AppearanceSeries row id=" << series.id();
			return false;
		}
		if (series.enabled() != 0)
		{
			if (!enabledSeriesKeys.insert(series.series_key()).second)
			{
				LOG_ERROR << "[PlayerAppearance] table validation failed: duplicate enabled series_key="
						  << series.series_key();
				return false;
			}
			enabledSeriesIds.insert(series.id());
		}
	}

	const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
	if (rows.empty())
	{
		LOG_ERROR << "[PlayerAppearance] table validation failed: CharacterAppearance table is empty";
		return false;
	}
	uint32_t enabledCount = 0;
	uint32_t starterCount = 0;
	for (const auto &row : rows)
	{
		if (row.appearance_id().empty())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: empty appearance_id for id=" << row.id();
			return false;
		}
		if (!player_appearance::AppearanceBitIndex(row.id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: missing bit_index mapping for id=" << row.id();
			return false;
		}
		if (!player_appearance::BiographyFieldsValid(
				row.archive_name(),
				row.nickname(),
				row.biography_summary(),
				row.story_title(),
				row.story_body()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: incomplete biography fields for id=" << row.id();
			return false;
		}
		if (row.enabled() != 0)
		{
			if (row.series_id() != 0 && enabledSeriesIds.find(row.series_id()) == enabledSeriesIds.end())
			{
				LOG_ERROR << "[PlayerAppearance] table validation failed: enabled appearance id=" << row.id()
						  << " references missing or disabled series_id=" << row.series_id();
				return false;
			}
			++enabledCount;
			if (row.starter_creatable() != 0)
			{
				++starterCount;
			}
		}
	}
	if (enabledCount == 0 || starterCount == 0)
	{
		LOG_ERROR << "[PlayerAppearance] table validation failed: enabled=" << enabledCount
				  << " starter_creatable=" << starterCount;
		return false;
	}

	// 校验灵宠幻化目录表(空表允许存在;有行时所有字段、外键与 bit_index 必须合法)。
	std::unordered_set<std::string> enabledPetKeys;
	for (const auto &row : PetCosmeticTableManager::Instance().FindAll().data())
	{
		if (row.id() == 0 || player_appearance::IsBlankText(row.cosmetic_key()) || player_appearance::IsBlankText(row.name()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: invalid PetCosmetic row id=" << row.id();
			return false;
		}
		if (!player_appearance::PetCosmeticBitIndex(row.id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: missing PetCosmetic bit_index id=" << row.id();
			return false;
		}
		if (row.target_pet_table_id() != 0 && !PetTableManager::Instance().Exists(row.target_pet_table_id()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: PetCosmetic id=" << row.id()
					  << " references unknown target_pet_table_id=" << row.target_pet_table_id();
			return false;
		}
		if (row.visual_pet_table_id() == 0 && player_appearance::IsBlankText(row.visual_art_key()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: PetCosmetic id=" << row.id()
					  << " missing both visual_pet_table_id and visual_art_key";
			return false;
		}
		if (row.visual_pet_table_id() != 0 && !PetTableManager::Instance().Exists(row.visual_pet_table_id()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: PetCosmetic id=" << row.id()
					  << " references unknown visual_pet_table_id=" << row.visual_pet_table_id();
			return false;
		}
		if (row.enabled() != 0 && !enabledPetKeys.insert(row.cosmetic_key()).second)
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: duplicate enabled PetCosmetic key="
					  << row.cosmetic_key();
			return false;
		}
	}

	// 校验技能特效目录表(空表允许存在;有行时技能外键、帧条键、色调与 bit_index 必须合法)。
	std::unordered_set<std::string> enabledVfxKeys;
	for (const auto &row : SkillVfxCosmeticTableManager::Instance().FindAll().data())
	{
		if (row.id() == 0 || player_appearance::IsBlankText(row.vfx_key()) ||
			player_appearance::IsBlankText(row.name()) || player_appearance::IsBlankText(row.fx_id()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: invalid SkillVfxCosmetic row id=" << row.id();
			return false;
		}
		if (!player_appearance::SkillVfxBitIndex(row.id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: missing SkillVfxCosmetic bit_index id=" << row.id();
			return false;
		}
		if (!SkillTableManager::Instance().Exists(row.skill_table_id()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: SkillVfxCosmetic id=" << row.id()
					  << " references unknown skill_table_id=" << row.skill_table_id();
			return false;
		}
		if (!player_appearance::IsValidTintHex(row.tint_hex()))
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: SkillVfxCosmetic id=" << row.id()
					  << " invalid tint_hex=" << row.tint_hex();
			return false;
		}
		if (row.enabled() != 0 && !enabledVfxKeys.insert(row.vfx_key()).second)
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: duplicate enabled SkillVfxCosmetic key="
					  << row.vfx_key();
			return false;
		}
	}
	return true;
}

std::string PlayerAppearanceSystem::EffectiveAppearanceId(entt::entity player, uint64_t nowUnixSec)
{
	const auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return {};
	}
	const auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		return {};
	}
	return std::string(player_appearance::EffectiveAppearance(
		profile->appearance_id(), profile->trial_appearance_id(), profile->trial_expires_at(), nowUnixSec));
}

void PlayerAppearanceSystem::FillInfo(entt::entity player, uint64_t nowUnixSec, PlayerAppearanceInfo &info)
{
	info.Clear();
	info.set_server_now(nowUnixSec);

	const auto &registry = tlsEcs.actorRegistry;
	const PlayerProfileComp *profile = registry.valid(player) ? registry.try_get<PlayerProfileComp>(player) : nullptr;
	std::string_view trialBits;
	std::string_view permanentView;
	std::string effectiveOwnedBits;
	if (profile != nullptr)
	{
		const std::string &permanent = profile->appearance_id();
		permanentView = permanent;
		const bool trialActive =
			player_appearance::IsTrialActive(profile->trial_appearance_id(), profile->trial_expires_at(), nowUnixSec);
		info.set_permanent_appearance_id(permanent);
		info.set_active_appearance_id(trialActive ? profile->trial_appearance_id() : permanent);
		if (trialActive)
		{
			info.set_trial_appearance_id(profile->trial_appearance_id());
			info.set_trial_expires_at(profile->trial_expires_at());
		}
		trialBits = profile->appearance_trial_used_bits();
		info.set_trial_used_bits(profile->appearance_trial_used_bits());

		// 对外位图把当前永久形象隐式并入副本(不改写持久化组件,避免只读拉取触发脏存盘),同时保留所有未知高位。
		effectiveOwnedBits = profile->appearance_owned_bits();
		if (const uint32_t permanentCfgId = FindAppearanceConfigIdAny(permanent); permanentCfgId != 0)
		{
			player_appearance::MarkOwned(effectiveOwnedBits, permanentCfgId);
		}
		info.set_owned_bits(effectiveOwnedBits);
	}

	player_appearance::Rule rule;
	const char *problem = nullptr;
	if (LoadCurrentRule(rule, problem))
	{
		info.set_card_item_id(rule.cardItemId);
		info.set_card_item_count(rule.cardItemCount);
		info.set_owned_card_count(OwnedCardCount(player, rule.cardItemId));
		info.set_trial_duration_seconds(rule.trialDurationSeconds);
		info.set_mall_sku_id(rule.mallSkuId);
		info.set_mall_price_fen(rule.mallPriceFen);
	}

	struct EntryRow
	{
		uint32_t configId;
		std::string appearanceId;
		std::string name;
		uint32_t sortOrder;
		bool trialUsed;
		bool owned;
		bool cardUnlockable;
		uint32_t seriesId;
	};
	std::vector<EntryRow> entries;
	const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
	entries.reserve(static_cast<std::size_t>(rows.size()));
	for (const auto &row : rows)
	{
		if (row.enabled() == 0)
		{
			continue;
		}
		entries.push_back(EntryRow{
			row.id(),
			row.appearance_id(),
			row.name(),
			row.sort_order(),
			player_appearance::IsTrialUsed(trialBits, row.id()),
			player_appearance::IsOwned(effectiveOwnedBits, row.id(), permanentView, row.appearance_id()),
			row.card_unlockable() != 0,
			row.series_id(),
		});
	}
	std::sort(entries.begin(), entries.end(), [](const EntryRow &a, const EntryRow &b)
			  {
				  if (a.sortOrder != b.sortOrder)
				  {
					  return a.sortOrder < b.sortOrder;
				  }
				  return a.configId < b.configId;
			  });
	for (const auto &e : entries)
	{
		auto *out = info.add_appearances();
		out->set_config_id(e.configId);
		out->set_appearance_id(e.appearanceId);
		out->set_name(e.name);
		out->set_sort_order(e.sortOrder);
		out->set_trial_used(e.trialUsed);
		out->set_owned(e.owned);
		out->set_card_unlockable(e.cardUnlockable);
		out->set_series_id(e.seriesId);
	}

	// 实时派生各收藏系列图鉴进度(零新存档字段、试用不计入拥有、空系列不输出、纯外观零属性)。
	struct SeriesAggregate
	{
		uint32_t seriesId{0};
		std::string seriesKey;
		std::string name;
		uint32_t sortOrder{0};
		std::string completionBadgeName;
		std::string epilogueTitle;
		uint32_t ownedCount{0};
		uint32_t totalCount{0};
		std::vector<std::string> memberAppearanceIds;
	};
	std::unordered_map<uint32_t, std::size_t> seriesIndexById;
	std::vector<SeriesAggregate> seriesList;
	const auto &seriesRows = AppearanceSeriesTableManager::Instance().FindAll().data();
	seriesList.reserve(static_cast<std::size_t>(seriesRows.size()));
	for (const auto &s : seriesRows)
	{
		if (s.enabled() == 0)
		{
			continue;
		}
		seriesIndexById.emplace(s.id(), seriesList.size());
		seriesList.push_back(SeriesAggregate{
			s.id(),
			s.series_key(),
			s.name(),
			s.sort_order(),
			s.completion_badge_name(),
			s.epilogue_title(),
			0,
			0,
			{},
		});
	}
	for (const auto &e : entries)
	{
		if (e.seriesId == 0)
		{
			continue;
		}
		const auto it = seriesIndexById.find(e.seriesId);
		if (it == seriesIndexById.end())
		{
			continue;
		}
		auto &agg = seriesList[it->second];
		++agg.totalCount;
		if (e.owned)
		{
			++agg.ownedCount;
		}
		agg.memberAppearanceIds.push_back(e.appearanceId);
	}
	std::sort(seriesList.begin(), seriesList.end(), [](const SeriesAggregate &a, const SeriesAggregate &b)
			  {
				  if (a.sortOrder != b.sortOrder)
				  {
					  return a.sortOrder < b.sortOrder;
				  }
				  return a.seriesId < b.seriesId;
			  });
	for (const auto &s : seriesList)
	{
		if (s.totalCount == 0)
		{
			continue;
		}
		auto *out = info.add_series_progress();
		out->set_series_id(s.seriesId);
		out->set_series_key(s.seriesKey);
		out->set_name(s.name);
		out->set_sort_order(s.sortOrder);
		out->set_owned_count(s.ownedCount);
		out->set_total_count(s.totalCount);
		out->set_completed(player_appearance::IsSeriesCompleted(s.ownedCount, s.totalCount));
		out->set_completion_badge_name(s.completionBadgeName);
		out->set_epilogue_title(s.epilogueTitle);
		for (const auto &memberId : s.memberAppearanceIds)
		{
			out->add_member_appearance_ids(memberId);
		}
	}

	// ── 灵宠幻化目录与当前实例穿戴快照（纯外观零数值） ──
	const std::string_view petOwnedBits =
		profile != nullptr ? std::string_view(profile->pet_cosmetic_owned_bits()) : std::string_view();
	if (profile != nullptr)
	{
		info.set_pet_cosmetic_owned_bits(profile->pet_cosmetic_owned_bits());
	}
	struct PetCosmeticRow
	{
		uint32_t configId;
		std::string cosmeticKey;
		std::string name;
		uint32_t targetPetTableId;
		uint32_t visualPetTableId;
		std::string visualArtKey;
		uint32_t sortOrder;
		bool owned;
		uint64_t mallSkuId;
		std::string acquireHint;
		std::string desc;
	};
	std::vector<PetCosmeticRow> petCosmetics;
	const auto &petCosmeticRows = PetCosmeticTableManager::Instance().FindAll().data();
	petCosmetics.reserve(static_cast<std::size_t>(petCosmeticRows.size()));
	for (const auto &row : petCosmeticRows)
	{
		if (row.enabled() == 0)
		{
			continue;
		}
		petCosmetics.push_back(PetCosmeticRow{
			row.id(),
			row.cosmetic_key(),
			row.name(),
			row.target_pet_table_id(),
			row.visual_pet_table_id(),
			row.visual_art_key(),
			row.sort_order(),
			player_appearance::IsPetCosmeticOwned(petOwnedBits, row.id()),
			row.mall_sku_id(),
			row.acquire_hint(),
			row.desc(),
		});
	}
	std::sort(petCosmetics.begin(), petCosmetics.end(), [](const PetCosmeticRow &a, const PetCosmeticRow &b)
			  {
				  if (a.sortOrder != b.sortOrder)
				  {
					  return a.sortOrder < b.sortOrder;
				  }
				  return a.configId < b.configId;
			  });
	for (const auto &item : petCosmetics)
	{
		auto *out = info.add_pet_cosmetics();
		out->set_config_id(item.configId);
		out->set_cosmetic_key(item.cosmeticKey);
		out->set_name(item.name);
		out->set_target_pet_table_id(item.targetPetTableId);
		out->set_visual_pet_table_id(item.visualPetTableId);
		out->set_visual_art_key(item.visualArtKey);
		out->set_sort_order(item.sortOrder);
		out->set_owned(item.owned);
		out->set_mall_sku_id(item.mallSkuId);
		out->set_acquire_hint(item.acquireHint);
		out->set_desc(item.desc);
	}
	if (const auto *pets = registry.valid(player) ? registry.try_get<PlayerPetComp>(player) : nullptr; pets != nullptr)
	{
		for (const auto &pet : pets->pets())
		{
			uint32_t resolvedCosmeticId = 0;
			std::string resolvedArtKey;
			ResolvePetCosmeticVisual(player, pet, resolvedCosmeticId, resolvedArtKey);
			auto *equipped = info.add_equipped_pet_cosmetics();
			equipped->set_pet_id(pet.pet_id());
			equipped->set_cosmetic_id(resolvedCosmeticId);
		}
	}

	// ── 技能特效目录与当前启用映射（纯战斗画面零数值） ──
	const std::string_view vfxOwnedBits =
		profile != nullptr ? std::string_view(profile->skill_vfx_owned_bits()) : std::string_view();
	if (profile != nullptr)
	{
		info.set_skill_vfx_owned_bits(profile->skill_vfx_owned_bits());
	}
	struct SkillVfxRow
	{
		uint32_t configId;
		std::string vfxKey;
		std::string name;
		uint32_t skillTableId;
		std::string fxId;
		std::string projectileFxId;
		std::string telegraphFxId;
		std::string tintHex;
		uint32_t sortOrder;
		bool owned;
		uint64_t mallSkuId;
		std::string acquireHint;
		std::string desc;
	};
	std::vector<SkillVfxRow> vfxList;
	const auto &vfxRows = SkillVfxCosmeticTableManager::Instance().FindAll().data();
	vfxList.reserve(static_cast<std::size_t>(vfxRows.size()));
	for (const auto &row : vfxRows)
	{
		if (row.enabled() == 0)
		{
			continue;
		}
		vfxList.push_back(SkillVfxRow{
			row.id(),
			row.vfx_key(),
			row.name(),
			row.skill_table_id(),
			row.fx_id(),
			row.projectile_fx_id(),
			row.telegraph_fx_id(),
			row.tint_hex(),
			row.sort_order(),
			player_appearance::IsSkillVfxOwned(vfxOwnedBits, row.id()),
			row.mall_sku_id(),
			row.acquire_hint(),
			row.desc(),
		});
	}
	std::sort(vfxList.begin(), vfxList.end(), [](const SkillVfxRow &a, const SkillVfxRow &b)
			  {
				  if (a.sortOrder != b.sortOrder)
				  {
					  return a.sortOrder < b.sortOrder;
				  }
				  return a.configId < b.configId;
			  });
	for (const auto &item : vfxList)
	{
		auto *out = info.add_skill_vfx_cosmetics();
		out->set_config_id(item.configId);
		out->set_vfx_key(item.vfxKey);
		out->set_name(item.name);
		out->set_skill_table_id(item.skillTableId);
		out->set_fx_id(item.fxId);
		out->set_projectile_fx_id(item.projectileFxId);
		out->set_telegraph_fx_id(item.telegraphFxId);
		out->set_tint_hex(item.tintHex);
		out->set_sort_order(item.sortOrder);
		out->set_owned(item.owned);
		out->set_mall_sku_id(item.mallSkuId);
		out->set_acquire_hint(item.acquireHint);
		out->set_desc(item.desc);
	}
	if (profile != nullptr)
	{
		for (const auto &[skillId, cosmeticId] : profile->equipped_skill_vfx())
		{
			if (skillId == 0 || cosmeticId == 0)
			{
				continue;
			}
			const auto *row = SkillVfxCosmeticTableManager::Instance().FindByIdSilent(cosmeticId).first;
			if (row == nullptr || row->enabled() == 0 || row->skill_table_id() != skillId)
			{
				continue;
			}
			if (!player_appearance::IsSkillVfxOwned(vfxOwnedBits, cosmeticId))
			{
				continue;
			}
			(*info.mutable_equipped_skill_vfx())[skillId] = cosmeticId;
		}
	}
}

uint32_t PlayerAppearanceSystem::SwitchAppearance(entt::entity player,
												  const SwitchAppearanceRequest &request,
												  uint64_t nowUnixSec,
												  SwitchAppearanceResponse &response)
{
	response.clear_info();
	response.clear_replayed();

	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;

	// 1. 幂等查账排在一切依赖状态的预检之前(与 AfkPassSystem::Buy 同契约)。
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0)
	{
		return kInvalidParameter;
	}
	std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult first;
	switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kSwitchAppearance, fingerprint, first))
	{
	case ClientRequestVerdict::kReplay:
		response.set_replayed(true);
		FillInfo(player, nowUnixSec, *response.mutable_info());
		return kSuccess;
	case ClientRequestVerdict::kMismatch:
		return kInvalidParameter;
	case ClientRequestVerdict::kUnavailable:
		return kServiceUnavailable;
	default:
		break;
	}

	// 2. 规则表与可写闸。
	player_appearance::Rule rule;
	const char *problem = nullptr;
	if (!LoadCurrentRule(rule, problem))
	{
		LOG_ERROR << "[PlayerAppearance] switch refused: AppearanceRule unavailable (" << problem
				  << ") player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}

	// 3. 目标形象存在、启用且存在稳定位索引(在扣卡前验证,保证扣卡后绝不因位映射缺失失败)。
	const EnabledAppearanceMeta target = FindEnabledAppearance(request.appearance_id());
	if (target.configId == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::AppearanceBitIndex(target.configId).has_value())
	{
		LOG_ERROR << "[PlayerAppearance] switch refused: missing bit_index for config_id=" << target.configId
				  << " player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}

	// 4. 事实源持久组件。
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] switch refused: missing PlayerProfileComp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (profile->appearance_id() == request.appearance_id())
	{
		return static_cast<uint32_t>(kAppearanceAlreadyCurrent);
	}

	// 5. 判定免费切换 vs 扣形象卡买断解锁(fail-closed)。
	const bool alreadyOwned = player_appearance::IsOwned(
		profile->appearance_owned_bits(), target.configId, profile->appearance_id(), request.appearance_id());
	const player_appearance::SwitchPlan plan =
		player_appearance::PlanSwitch(alreadyOwned, request.expect_free_switch(), target.cardUnlockable);
	if (plan == player_appearance::SwitchPlan::kRefuseNotOwned)
	{
		return static_cast<uint32_t>(kAppearanceNotOwned);
	}
	if (plan == player_appearance::SwitchPlan::kRefuseNotCardUnlockable)
	{
		return static_cast<uint32_t>(kAppearanceNotCardUnlockable);
	}

	const bool consumeCard = (plan == player_appearance::SwitchPlan::kCardUnlock);
	if (consumeCard)
	{
		auto *bags = registry.try_get<PlayerBagsComp>(player);
		if (bags == nullptr)
		{
			LOG_ERROR << "[PlayerAppearance] switch refused: missing PlayerBagsComp player_id=" << playerId;
			return static_cast<uint32_t>(kAppearanceUnavailable);
		}
		const ItemCountMap consumed{{rule.cardItemId, rule.cardItemCount}};
		if (BagService::CheckRemovable(player, bags->bags[kInventory], consumed) != BagService::RemovalVerdict::kEnough)
		{
			return static_cast<uint32_t>(kAppearanceNoCard);
		}
		if (const uint32_t removeErr = BagService::RemoveItemsExact(
				player, bags->bags[kInventory], consumed, TX_APPEARANCE_SWITCH, clientRequestId, request.appearance_id());
			removeErr != kSuccess)
		{
			LOG_ERROR << "[PlayerAppearance] switch refused: RemoveItemsExact failed after precheck player_id="
					  << playerId << " item_id=" << rule.cardItemId << " err=" << removeErr;
			return static_cast<uint32_t>(kAppearanceUnavailable);
		}
	}

	// 6. 原子修改:保留原永久形象与新形象的拥有位 -> 更新永久形象并结束试用 -> 记幂等键 -> 请求关键进度存盘 -> 广播。
	const std::string beforePermanent = profile->appearance_id();
	std::string ownedBits = profile->appearance_owned_bits();
	if (const uint32_t beforeConfigId = FindAppearanceConfigIdAny(beforePermanent); beforeConfigId != 0)
	{
		player_appearance::MarkOwned(ownedBits, beforeConfigId);
	}
	player_appearance::MarkOwned(ownedBits, target.configId);
	profile->set_appearance_owned_bits(std::move(ownedBits));

	profile->set_appearance_id(request.appearance_id());
	profile->clear_trial_appearance_id();
	profile->set_trial_expires_at(0);
	profile->set_appearance_switch_seq(profile->appearance_switch_seq() + 1);
	if (playerId != kInvalidGuid)
	{
		tlsActiveTrials.erase(playerId);
	}

	client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kSwitchAppearance,
								   std::move(fingerprint), ClientRequestResult{});
	RequestSave(player, consumeCard ? "appearance_switch" : "appearance_free_switch");

	LOG_INFO << "[PlayerAppearance] metric=appearance_switched player_id=" << playerId
			 << " before=" << beforePermanent << " after=" << request.appearance_id()
			 << " config_id=" << target.configId << " free_switch=" << (!consumeCard ? 1 : 0)
			 << " card_item_id=" << (consumeCard ? rule.cardItemId : 0u)
			 << " card_count=" << (consumeCard ? rule.cardItemCount : 0u)
			 << " client_request_id=" << clientRequestId;

	BroadcastAppearanceChanged(player, playerId, request.appearance_id(), /*isTrial=*/false, /*trialExpiresAt=*/0);
	FillInfo(player, nowUnixSec, *response.mutable_info());
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::GrantAppearance(entt::entity player,
												 std::string_view appearanceId,
												 uint64_t nowUnixSec,
												 bool *alreadyOwned)
{
	if (alreadyOwned != nullptr)
	{
		*alreadyOwned = false;
	}
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;

	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	const EnabledAppearanceMeta target = FindEnabledAppearance(appearanceId);
	if (target.configId == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::AppearanceBitIndex(target.configId).has_value())
	{
		LOG_ERROR << "[PlayerAppearance] grant refused: missing bit_index for config_id=" << target.configId
				  << " player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] grant refused: missing PlayerProfileComp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (player_appearance::IsOwned(
			profile->appearance_owned_bits(), target.configId, profile->appearance_id(), appearanceId))
	{
		if (alreadyOwned != nullptr)
		{
			*alreadyOwned = true;
		}
		return kSuccess;
	}

	std::string ownedBits = profile->appearance_owned_bits();
	player_appearance::MarkOwned(ownedBits, target.configId);
	profile->set_appearance_owned_bits(std::move(ownedBits));
	RequestSave(player, "appearance_grant");

	LOG_INFO << "[PlayerAppearance] metric=appearance_granted player_id=" << playerId
			 << " appearance_id=" << std::string(appearanceId) << " config_id=" << target.configId;

	AppearanceStateNotify notify;
	notify.set_trial_expired(false);
	FillInfo(player, nowUnixSec, *notify.mutable_info());
	PushStateNotify(player, notify);
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::StartTrial(entt::entity player,
											const StartAppearanceTrialRequest &request,
											uint64_t nowUnixSec,
											StartAppearanceTrialResponse &response)
{
	response.clear_info();
	response.clear_replayed();

	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;

	// 1. 幂等查账排在一切状态预检之前:首次成功后该形象的试用 bit 已置 1,重发若不先查账会被误拒。
	const uint64_t clientRequestId = request.client_request_id();
	if (clientRequestId == 0)
	{
		return kInvalidParameter;
	}
	std::string fingerprint = ClientRequestFingerprint(request);
	ClientRequestResult first;
	switch (client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kStartAppearanceTrial, fingerprint, first))
	{
	case ClientRequestVerdict::kReplay:
		response.set_replayed(true);
		FillInfo(player, nowUnixSec, *response.mutable_info());
		return kSuccess;
	case ClientRequestVerdict::kMismatch:
		return kInvalidParameter;
	case ClientRequestVerdict::kUnavailable:
		return kServiceUnavailable;
	default:
		break;
	}

	// 2. 规则表与可写闸。
	player_appearance::Rule rule;
	const char *problem = nullptr;
	if (!LoadCurrentRule(rule, problem))
	{
		LOG_ERROR << "[PlayerAppearance] start trial refused: AppearanceRule unavailable (" << problem
				  << ") player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}

	// 3. 目标形象存在且启用,且存在稳定位索引。
	const EnabledAppearanceMeta target = FindEnabledAppearance(request.appearance_id());
	const uint32_t targetConfigId = target.configId;
	if (targetConfigId == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::AppearanceBitIndex(targetConfigId).has_value())
	{
		LOG_ERROR << "[PlayerAppearance] start trial refused: missing bit_index for config_id=" << targetConfigId
				  << " player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}

	// 4. 事实源组件检查。
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] start trial refused: missing PlayerProfileComp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (profile->appearance_id() == request.appearance_id())
	{
		return static_cast<uint32_t>(kAppearanceAlreadyCurrent);
	}
	if (player_appearance::IsOwned(
			profile->appearance_owned_bits(), targetConfigId, profile->appearance_id(), request.appearance_id()))
	{
		return static_cast<uint32_t>(kAppearanceAlreadyOwned);
	}
	if (player_appearance::IsTrialUsed(profile->appearance_trial_used_bits(), targetConfigId))
	{
		return static_cast<uint32_t>(kAppearanceTrialAlreadyUsed);
	}

	// 5. 写入 bit 位图(保留未知高位) + 覆盖当前试用形象与到期时刻 + 记幂等键 + 存盘 + 广播。
	std::string bits = profile->appearance_trial_used_bits();
	if (!player_appearance::MarkTrialUsed(bits, targetConfigId))
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	const uint64_t expiresAt = nowUnixSec + rule.trialDurationSeconds;
	profile->set_appearance_trial_used_bits(std::move(bits));
	profile->set_trial_appearance_id(request.appearance_id());
	profile->set_trial_expires_at(expiresAt);
	if (playerId != kInvalidGuid)
	{
		tlsActiveTrials[playerId] = expiresAt;
	}

	client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kStartAppearanceTrial,
								   std::move(fingerprint), ClientRequestResult{});
	RequestSave(player, "appearance_trial_start");

	LOG_INFO << "[PlayerAppearance] metric=appearance_trial_started player_id=" << playerId
			 << " appearance_id=" << request.appearance_id() << " config_id=" << targetConfigId
			 << " expires_at=" << expiresAt << " client_request_id=" << clientRequestId;

	BroadcastAppearanceChanged(player, playerId, request.appearance_id(), /*isTrial=*/true, expiresAt);
	FillInfo(player, nowUnixSec, *response.mutable_info());
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::CancelTrial(entt::entity player, uint64_t nowUnixSec, PlayerAppearanceInfo &info)
{
	info.Clear();
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;

	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] cancel trial refused: missing PlayerProfileComp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (!player_appearance::IsTrialActive(profile->trial_appearance_id(), profile->trial_expires_at(), nowUnixSec))
	{
		return static_cast<uint32_t>(kAppearanceNoActiveTrial);
	}

	const std::string cancelledTrial = profile->trial_appearance_id();
	profile->clear_trial_appearance_id();
	profile->set_trial_expires_at(0);
	if (playerId != kInvalidGuid)
	{
		tlsActiveTrials.erase(playerId);
	}
	RequestSave(player, "appearance_trial_cancel");

	LOG_INFO << "[PlayerAppearance] metric=appearance_trial_cancelled player_id=" << playerId
			 << " trial_appearance_id=" << cancelledTrial << " reverted_to=" << profile->appearance_id();

	BroadcastAppearanceChanged(player, playerId, profile->appearance_id(), /*isTrial=*/false, /*trialExpiresAt=*/0);
	FillInfo(player, nowUnixSec, info);
	return kSuccess;
}

void PlayerAppearanceSystem::ResolvePetCosmeticVisual(entt::entity player,
													  const PetInstance &pet,
													  uint32_t &outCosmeticId,
													  std::string &outArtKey)
{
	outCosmeticId = 0;
	outArtKey.clear();
	if (pet.cosmetic_id() == 0)
	{
		return;
	}
	const auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	const auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		return;
	}
	const auto *row = PetCosmeticTableManager::Instance().FindByIdSilent(pet.cosmetic_id()).first;
	if (row == nullptr || row->enabled() == 0)
	{
		return;
	}
	if (!player_appearance::IsPetSpeciesCompatible(row->target_pet_table_id(), pet.pet_table_id()))
	{
		return;
	}
	if (!player_appearance::IsPetCosmeticOwned(profile->pet_cosmetic_owned_bits(), row->id()))
	{
		return;
	}
	outCosmeticId = row->id();
	outArtKey = row->visual_art_key();
}

void PlayerAppearanceSystem::FillBattleSkillVfx(entt::entity player, BattlePlayerSnapshot &snapshot)
{
	snapshot.clear_equipped_skill_vfx();
	const auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	const auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr || profile->equipped_skill_vfx().empty())
	{
		return;
	}
	const std::string_view ownedBits = profile->skill_vfx_owned_bits();
	for (const auto &[skillId, cosmeticId] : profile->equipped_skill_vfx())
	{
		if (skillId == 0 || cosmeticId == 0)
		{
			continue;
		}
		const auto *row = SkillVfxCosmeticTableManager::Instance().FindByIdSilent(cosmeticId).first;
		if (row == nullptr || row->enabled() == 0 || row->skill_table_id() != skillId)
		{
			continue;
		}
		if (!player_appearance::IsSkillVfxOwned(ownedBits, cosmeticId))
		{
			continue;
		}
		(*snapshot.mutable_equipped_skill_vfx())[skillId] = cosmeticId;
	}
}

uint32_t PlayerAppearanceSystem::EquipPetCosmetic(entt::entity player,
												  const EquipPetCosmeticRequest &request,
												  uint64_t nowUnixSec,
												  EquipPetCosmeticResponse &response)
{
	response.Clear();
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (request.client_request_id() == 0 || request.pet_id() == 0)
	{
		return kInvalidParameter;
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;
	const auto *profile = registry.try_get<PlayerProfileComp>(player);
	auto *petComp = registry.try_get<PlayerPetComp>(player);
	if (profile == nullptr || petComp == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] equip pet cosmetic refused: missing profile or pet comp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}

	int petIndex = -1;
	for (int i = 0; i < petComp->pets_size(); ++i)
	{
		if (petComp->pets(i).pet_id() == request.pet_id())
		{
			petIndex = i;
			break;
		}
	}
	if (petIndex < 0)
	{
		return static_cast<uint32_t>(kPetNotFound);
	}
	auto *pet = petComp->mutable_pets(petIndex);

	const uint32_t targetCosmeticId = request.cosmetic_id();
	if (targetCosmeticId != 0)
	{
		const auto *row = PetCosmeticTableManager::Instance().FindByIdSilent(targetCosmeticId).first;
		if (row == nullptr || row->enabled() == 0)
		{
			return static_cast<uint32_t>(kAppearanceInvalid);
		}
		if (!player_appearance::PetCosmeticBitIndex(row->id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] equip pet cosmetic refused: missing bit_index id=" << row->id()
					  << " player_id=" << playerId;
			return static_cast<uint32_t>(kAppearanceUnavailable);
		}
		if (!player_appearance::IsPetCosmeticOwned(profile->pet_cosmetic_owned_bits(), row->id()))
		{
			return static_cast<uint32_t>(kAppearanceNotOwned);
		}
		if (!player_appearance::IsPetSpeciesCompatible(row->target_pet_table_id(), pet->pet_table_id()))
		{
			return static_cast<uint32_t>(kAppearanceInvalid);
		}
	}

	response.set_pet_id(pet->pet_id());
	if (pet->cosmetic_id() == targetCosmeticId)
	{
		response.set_equipped_cosmetic_id(targetCosmeticId);
		response.set_replayed(true);
		FillInfo(player, nowUnixSec, *response.mutable_info());
		return kSuccess;
	}

	pet->set_cosmetic_id(targetCosmeticId);
	RequestSave(player, targetCosmeticId == 0 ? "pet_cosmetic_cleared" : "pet_cosmetic_equipped");
	LOG_INFO << "[PlayerAppearance] metric=pet_cosmetic_updated player_id=" << playerId
			 << " pet_id=" << pet->pet_id() << " cosmetic_id=" << targetCosmeticId;

	response.set_equipped_cosmetic_id(targetCosmeticId);
	FillInfo(player, nowUnixSec, *response.mutable_info());
	AppearanceStateNotify notify;
	*notify.mutable_info() = response.info();
	PushStateNotify(player, notify);
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::EquipSkillVfx(entt::entity player,
											   const EquipSkillVfxRequest &request,
											   uint64_t nowUnixSec,
											   EquipSkillVfxResponse &response)
{
	response.Clear();
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (request.client_request_id() == 0 || request.skill_table_id() == 0)
	{
		return kInvalidParameter;
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	const Guid playerId = guid != nullptr ? guid->value : kInvalidGuid;
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] equip skill vfx refused: missing PlayerProfileComp player_id=" << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}

	const uint32_t skillTableId = request.skill_table_id();
	const uint32_t targetCosmeticId = request.cosmetic_id();
	if (targetCosmeticId != 0)
	{
		const auto *row = SkillVfxCosmeticTableManager::Instance().FindByIdSilent(targetCosmeticId).first;
		if (row == nullptr || row->enabled() == 0 || row->skill_table_id() != skillTableId)
		{
			return static_cast<uint32_t>(kAppearanceInvalid);
		}
		if (!player_appearance::SkillVfxBitIndex(row->id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] equip skill vfx refused: missing bit_index id=" << row->id()
					  << " player_id=" << playerId;
			return static_cast<uint32_t>(kAppearanceUnavailable);
		}
		if (!player_appearance::IsSkillVfxOwned(profile->skill_vfx_owned_bits(), row->id()))
		{
			return static_cast<uint32_t>(kAppearanceNotOwned);
		}
		if (!PlayerHasUsableSkill(player, skillTableId))
		{
			return static_cast<uint32_t>(kAppearanceInvalid);
		}
	}

	const auto it = profile->equipped_skill_vfx().find(skillTableId);
	const uint32_t currentId = it != profile->equipped_skill_vfx().end() ? it->second : 0;
	response.set_skill_table_id(skillTableId);
	if (currentId == targetCosmeticId)
	{
		response.set_equipped_cosmetic_id(targetCosmeticId);
		response.set_replayed(true);
		FillInfo(player, nowUnixSec, *response.mutable_info());
		return kSuccess;
	}

	if (targetCosmeticId == 0)
	{
		profile->mutable_equipped_skill_vfx()->erase(skillTableId);
	}
	else
	{
		(*profile->mutable_equipped_skill_vfx())[skillTableId] = targetCosmeticId;
	}
	RequestSave(player, targetCosmeticId == 0 ? "skill_vfx_cleared" : "skill_vfx_equipped");
	LOG_INFO << "[PlayerAppearance] metric=skill_vfx_updated player_id=" << playerId
			 << " skill_table_id=" << skillTableId << " cosmetic_id=" << targetCosmeticId;

	response.set_equipped_cosmetic_id(targetCosmeticId);
	FillInfo(player, nowUnixSec, *response.mutable_info());
	AppearanceStateNotify notify;
	*notify.mutable_info() = response.info();
	PushStateNotify(player, notify);
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::GrantPetCosmetic(entt::entity player,
												  uint32_t cosmeticId,
												  uint64_t nowUnixSec,
												  bool *alreadyOwned)
{
	if (alreadyOwned != nullptr)
	{
		*alreadyOwned = false;
	}
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	const auto *row = PetCosmeticTableManager::Instance().FindByIdSilent(cosmeticId).first;
	if (row == nullptr || row->enabled() == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::PetCosmeticBitIndex(row->id()).has_value())
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (player_appearance::IsPetCosmeticOwned(profile->pet_cosmetic_owned_bits(), row->id()))
	{
		if (alreadyOwned != nullptr)
		{
			*alreadyOwned = true;
		}
		return kSuccess;
	}
	if (!player_appearance::MarkPetCosmeticOwned(*profile->mutable_pet_cosmetic_owned_bits(), row->id()))
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	RequestSave(player, "pet_cosmetic_granted");
	AppearanceStateNotify notify;
	FillInfo(player, nowUnixSec, *notify.mutable_info());
	PushStateNotify(player, notify);
	return kSuccess;
}

uint32_t PlayerAppearanceSystem::GrantSkillVfx(entt::entity player,
											   uint32_t cosmeticId,
											   uint64_t nowUnixSec,
											   bool *alreadyOwned)
{
	if (alreadyOwned != nullptr)
	{
		*alreadyOwned = false;
	}
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return kEntityIsNull;
	}
	if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
	{
		return mutTip;
	}
	const auto *row = SkillVfxCosmeticTableManager::Instance().FindByIdSilent(cosmeticId).first;
	if (row == nullptr || row->enabled() == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::SkillVfxBitIndex(row->id()).has_value())
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (profile == nullptr)
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (player_appearance::IsSkillVfxOwned(profile->skill_vfx_owned_bits(), row->id()))
	{
		if (alreadyOwned != nullptr)
		{
			*alreadyOwned = true;
		}
		return kSuccess;
	}
	if (!player_appearance::MarkSkillVfxOwned(*profile->mutable_skill_vfx_owned_bits(), row->id()))
	{
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	RequestSave(player, "skill_vfx_granted");
	AppearanceStateNotify notify;
	FillInfo(player, nowUnixSec, *notify.mutable_info());
	PushStateNotify(player, notify);
	return kSuccess;
}

void PlayerAppearanceSystem::OnPlayerLoaded(entt::entity player, uint64_t nowUnixSec)
{
	auto &registry = tlsEcs.actorRegistry;
	if (!registry.valid(player))
	{
		return;
	}
	const auto *guid = registry.try_get<PlayerGuidComp>(player);
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	if (guid == nullptr || profile == nullptr)
	{
		return;
	}
	registry.emplace_or_replace<PlayerAppearanceCommittedComp>(
		player, PlayerAppearanceCommittedComp{profile->appearance_id()});

	if (!profile->trial_appearance_id().empty())
	{
		if (profile->trial_expires_at() <= nowUnixSec)
		{
			// 离线期间试用已到期:入场即清理并触发一次存盘,不发过时试用状态给场景。
			LOG_INFO << "[PlayerAppearance] metric=appearance_trial_expired_on_load player_id=" << guid->value
					 << " trial_appearance_id=" << profile->trial_appearance_id()
					 << " expired_at=" << profile->trial_expires_at();
			profile->clear_trial_appearance_id();
			profile->set_trial_expires_at(0);
			tlsActiveTrials.erase(guid->value);
			RequestSave(player, "appearance_trial_expired_on_load");
		}
		else
		{
			tlsActiveTrials[guid->value] = profile->trial_expires_at();
		}
	}
	else
	{
		tlsActiveTrials.erase(guid->value);
	}
}

void PlayerAppearanceSystem::OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowMs)
{
	const entt::entity player = tlsEcs.GetPlayer(playerId);
	auto &registry = tlsEcs.actorRegistry;
	auto *committedComp = registry.valid(player) ? registry.try_get<PlayerAppearanceCommittedComp>(player) : nullptr;
	if (committedComp == nullptr)
	{
		return;
	}
	const std::string &committedAppearance = committed.player_database_data().profile_component().appearance_id();
	if (committedAppearance.empty() || committedAppearance == committedComp->permanentAppearanceId)
	{
		return;
	}
	committedComp->permanentAppearanceId = committedAppearance;

	auto &sync = tlsDirectorySyncs[playerId];
	sync.targetAppearanceId = committedAppearance;
	sync.retryAttempts = 0;
	sync.nextRetryAtMs = 0;
	if (!sync.rpcInFlight)
	{
		DispatchDirectorySync(playerId, nowMs);
	}
}

void PlayerAppearanceSystem::OnPlayerDestroyed(Guid playerId)
{
	tlsActiveTrials.erase(playerId);
	tlsDirectorySyncs.erase(playerId);
}

void PlayerAppearanceSystem::Tick(uint64_t nowUnixSec, uint64_t nowMs)
{
	// 1. 试用到期扫描(表空 O(1) 返回)。
	if (!tlsActiveTrials.empty())
	{
		std::vector<Guid> expiredPlayers;
		for (const auto &[playerId, expiresAt] : tlsActiveTrials)
		{
			if (expiresAt <= nowUnixSec)
			{
				expiredPlayers.push_back(playerId);
			}
		}
		for (Guid playerId : expiredPlayers)
		{
			tlsActiveTrials.erase(playerId);
			const entt::entity player = tlsEcs.GetPlayer(playerId);
			auto &registry = tlsEcs.actorRegistry;
			if (!registry.valid(player))
			{
				continue;
			}
			auto *profile = registry.try_get<PlayerProfileComp>(player);
			if (profile == nullptr || profile->trial_appearance_id().empty() || profile->trial_expires_at() > nowUnixSec)
			{
				continue;
			}
			// 冻结/交接期间不改持久组件,重新挂回待下一秒(或目标节点加载时清理)。
			if (!player_mutability::IsPlayerMutable(player))
			{
				tlsActiveTrials[playerId] = profile->trial_expires_at();
				continue;
			}

			const std::string expiredTrial = profile->trial_appearance_id();
			profile->clear_trial_appearance_id();
			profile->set_trial_expires_at(0);
			RequestSave(player, "appearance_trial_expired");

			LOG_INFO << "[PlayerAppearance] metric=appearance_trial_expired player_id=" << playerId
					 << " trial_appearance_id=" << expiredTrial << " reverted_to=" << profile->appearance_id();

			BroadcastAppearanceChanged(
				player, playerId, profile->appearance_id(), /*isTrial=*/false, /*trialExpiresAt=*/0);
			AppearanceStateNotify notify;
			notify.set_trial_expired(true);
			FillInfo(player, nowUnixSec, *notify.mutable_info());
			PushStateNotify(player, notify);
		}
	}

	// 2. 目录永久外观同步退避重试(表空 O(1) 返回)。
	if (!tlsDirectorySyncs.empty())
	{
		std::vector<Guid> dueSyncs;
		for (const auto &[playerId, state] : tlsDirectorySyncs)
		{
			if (!state.rpcInFlight && state.nextRetryAtMs != 0 && state.nextRetryAtMs <= nowMs)
			{
				dueSyncs.push_back(playerId);
			}
		}
		for (Guid playerId : dueSyncs)
		{
			DispatchDirectorySync(playerId, nowMs);
		}
	}
}

void PlayerAppearanceSystem::SetTestHooks(TestHooks hooks)
{
	tlsTestHooks = std::move(hooks);
}

void PlayerAppearanceSystem::ResetTestState()
{
	tlsActiveTrials.clear();
	tlsDirectorySyncs.clear();
	tlsTestHooks = TestHooks{};
}
