#include "player_appearance.h"

#include <algorithm>
#include <cstdlib>
#include <limits>
#include <unordered_map>
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
#include "proto/common/base/node.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/player_appearance.pb.h"
#include "rpc/service_metadata/player_appearance_service_metadata.h"
#include "spatial/system/view.h"
#include "table/code/appearancerule_table.h"
#include "table/code/characterappearance_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/appearance_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
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

	// 按 appearance_id 查找已启用的配置行,返回其永久主键 config_id(找不到或未启用返回 0)。
	// 不返回行指针,遵循热更快照边界约定。
	uint32_t FindEnabledAppearanceConfigId(std::string_view appearanceId)
	{
		if (appearanceId.empty())
		{
			return 0;
		}
		const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
		for (const auto &row : rows)
		{
			if (row.enabled() != 0 && row.appearance_id() == appearanceId)
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
	const auto &rows = CharacterAppearanceTableManager::Instance().FindAll().data();
	if (rows.empty())
	{
		LOG_ERROR << "[PlayerAppearance] table validation failed: CharacterAppearance table is empty";
		return false;
	}
	uint32_t enabledCount = 0;
	for (const auto &row : rows)
	{
		if (row.appearance_id().empty())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: empty appearance_id for id=" << row.id();
			return false;
		}
		if (!player_appearance::TrialBitIndex(row.id()).has_value())
		{
			LOG_ERROR << "[PlayerAppearance] table validation failed: missing bit_index mapping for id=" << row.id();
			return false;
		}
		if (row.enabled() != 0)
		{
			++enabledCount;
		}
	}
	if (enabledCount == 0)
	{
		LOG_ERROR << "[PlayerAppearance] table validation failed: no enabled appearance rows";
		return false;
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
	if (profile != nullptr)
	{
		const std::string &permanent = profile->appearance_id();
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

	// 3. 目标形象存在且启用。
	const uint32_t targetConfigId = FindEnabledAppearanceConfigId(request.appearance_id());
	if (targetConfigId == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}

	// 4. 事实源持久组件。
	auto *profile = registry.try_get<PlayerProfileComp>(player);
	auto *bags = registry.try_get<PlayerBagsComp>(player);
	if (profile == nullptr || bags == nullptr)
	{
		LOG_ERROR << "[PlayerAppearance] switch refused: missing PlayerProfileComp or PlayerBagsComp player_id="
				  << playerId;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}
	if (profile->appearance_id() == request.appearance_id())
	{
		return static_cast<uint32_t>(kAppearanceAlreadyCurrent);
	}

	// 5. 形象卡预检(扣掉预留后必须足够)。
	const ItemCountMap consumed{{rule.cardItemId, rule.cardItemCount}};
	if (BagService::CheckRemovable(player, bags->bags[kInventory], consumed) != BagService::RemovalVerdict::kEnough)
	{
		return static_cast<uint32_t>(kAppearanceNoCard);
	}

	// 6. 原子修改:扣形象卡 -> 覆盖永久形象并结束试用 -> 记幂等键 -> 请求关键进度存盘 -> 广播。
	if (const uint32_t removeErr = BagService::RemoveItemsExact(
			player, bags->bags[kInventory], consumed, TX_APPEARANCE_SWITCH, clientRequestId, request.appearance_id());
		removeErr != kSuccess)
	{
		LOG_ERROR << "[PlayerAppearance] switch refused: RemoveItemsExact failed after precheck player_id="
				  << playerId << " item_id=" << rule.cardItemId << " err=" << removeErr;
		return static_cast<uint32_t>(kAppearanceUnavailable);
	}

	const std::string beforePermanent = profile->appearance_id();
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
	RequestSave(player, "appearance_switch");

	LOG_INFO << "[PlayerAppearance] metric=appearance_switched player_id=" << playerId
			 << " before=" << beforePermanent << " after=" << request.appearance_id()
			 << " config_id=" << targetConfigId << " card_item_id=" << rule.cardItemId
			 << " card_count=" << rule.cardItemCount << " client_request_id=" << clientRequestId;

	BroadcastAppearanceChanged(player, playerId, request.appearance_id(), /*isTrial=*/false, /*trialExpiresAt=*/0);
	FillInfo(player, nowUnixSec, *response.mutable_info());
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
	const uint32_t targetConfigId = FindEnabledAppearanceConfigId(request.appearance_id());
	if (targetConfigId == 0)
	{
		return static_cast<uint32_t>(kAppearanceInvalid);
	}
	if (!player_appearance::TrialBitIndex(targetConfigId).has_value())
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
