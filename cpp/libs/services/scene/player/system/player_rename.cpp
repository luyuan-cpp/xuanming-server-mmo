#include "player_rename.h"

#include <cstdlib>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h"

#include "core/security/service_call_auth.h"
#include "engine/core/time/system/time.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "grpc_client/data_service/data_service_grpc_client.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_hold.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "network/client_message_sender.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "player/system/content_guard_client.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_mutability.h"
#include "player/system/player_rename_rules.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/content_guard/content_guard.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "table/code/item_table.h"
#include "table/code/rolenamerule_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "thread_context/node_context_manager.h"

namespace
{
	constexpr const char *kSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE";

	struct RuntimeOp
	{
		bool checkingContent{false};
		bool rpcInFlight{false};
		uint64_t inFlightSeq{0};
		uint32_t retryAttempts{0};
		uint64_t nextRetryAtMs{0};
	};

	thread_local std::unordered_map<Guid, RuntimeOp> tlsRuntimeOps;
	thread_local PlayerRenameSystem::TestHooks tlsTestHooks;

	bool LoadCurrentRule(player_rename::Rule &rule, const char *&problem)
	{
		const auto &rows = RoleNameRuleTableManager::Instance().FindAll().data();
		if (rows.empty())
		{
			problem = "RoleNameRule table is empty";
			return false;
		}
		const auto &row = *rows.begin();
		rule = player_rename::Rule{
			row.min_chars(),
			row.max_chars(),
			row.rename_cooldown_seconds(),
			row.old_name_hold_seconds(),
			row.rename_item_id(),
			row.rename_item_count(),
		};
		problem = player_rename::RuleProblem(rule);
		if (problem != nullptr)
		{
			return false;
		}
		if (!ItemTableManager::Instance().Exists(rule.renameItemId))
		{
			problem = "rename_item_id does not exist in Item table";
			return false;
		}
		return true;
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
			return static_cast<uint32_t>(kRoleRenameFrozen);
		case player_mutability::PlayerMutability::kOwnerEpochUnknown:
		default:
			return static_cast<uint32_t>(kServiceUnavailable);
		}
	}

	void PushNotify(entt::entity player, const PlayerRenamedNotify &notify)
	{
		if (tlsTestHooks.notify)
		{
			tlsTestHooks.notify(player, notify);
			return;
		}
		SendToPlayerClient(SceneClientPlayerCommonNotifyPlayerRenamedMessageId, notify, player);
	}

	void PushFailure(entt::entity player, uint32_t tipId, uint64_t nextRenameTimeSec = 0)
	{
		PlayerRenamedNotify notify;
		notify.mutable_error_message()->set_id(tipId);
		notify.set_next_rename_time(nextRenameTimeSec);
		PushNotify(player, notify);
	}

	void RequestSave(entt::entity player)
	{
		if (tlsTestHooks.save)
		{
			tlsTestHooks.save(player);
			return;
		}
		if (!PlayerLifecycleSystem::SavePlayerForAssetCommit(player))
		{
			PlayerLifecycleSystem::RequestKeyProgressSave(player, "player_rename");
		}
	}

	bool AllocateHoldId(Guid &outHoldId)
	{
		if (tlsTestHooks.nextHoldId)
		{
			return tlsTestHooks.nextHoldId(outHoldId);
		}
		return tlsGuidSegmentRegistry.Get(GuidKind::kItem).TryNext(outHoldId);
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

	bool SendRenameRoleRpc(data_service::RenameRoleRequest &request, uint64_t nowMs, PlayerRenameSystem::RenameRoleDone done)
	{
		if (tlsTestHooks.transport)
		{
			return tlsTestHooks.transport(request, std::move(done));
		}
		const char *secret = std::getenv(kSecretEnv);
		if (secret == nullptr || !service_call_auth::Sign(
									 data_service::kDataServiceRenameRoleGrpcPath,
									 request,
									 *request.mutable_auth(),
									 "scene",
									 secret,
									 nowMs))
		{
			LOG_ERROR << "[PlayerRename] failed to sign RenameRole request: missing or invalid " << kSecretEnv;
			return false;
		}
		const entt::entity node = PickReadyDataServiceNode();
		if (node == entt::null)
		{
			LOG_WARN << "[PlayerRename] no ready data_service node for RenameRole: player_id=" << request.player_id()
					 << " seq=" << request.seq();
			return false;
		}
		data_service::SendDataServiceRenameRole(
			tlsNodeContextManager.GetRegistry(common::base::DataServiceNodeService),
			node,
			request,
			[done = std::move(done)](const GrpcCallOutcome &outcome, const data_service::RenameRoleResponse *reply)
			{
				done(outcome.ok(), reply);
			});
		return true;
	}

	void ScheduleRetry(Guid playerId, uint64_t nowMs)
	{
		auto &rt = tlsRuntimeOps[playerId];
		rt.rpcInFlight = false;
		rt.inFlightSeq = 0;
		++rt.retryAttempts;
		rt.nextRetryAtMs = nowMs + player_rename::RetryDelayMs(rt.retryAttempts);
	}

	void HandleRenameRoleReply(Guid playerId, uint64_t seq, bool callOk,
							   const data_service::RenameRoleResponse *reply, uint64_t nowMs)
	{
		if (const auto it = tlsRuntimeOps.find(playerId); it != tlsRuntimeOps.end())
		{
			it->second.rpcInFlight = false;
			it->second.inFlightSeq = 0;
		}

		const entt::entity entity = tlsEcs.GetPlayer(playerId);
		if (entity == entt::null || !tlsEcs.actorRegistry.valid(entity))
		{
			return;
		}
		auto *profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(entity);
		if (profile == nullptr || !profile->has_rename_op() || profile->rename_op().seq() != seq)
		{
			return;
		}

		if (!callOk || reply == nullptr || player_rename::IsTransientResult(reply->result()))
		{
			ScheduleRetry(playerId, nowMs);
			LOG_WARN << "[PlayerRename] transient RenameRole reply, will retry: player_id=" << playerId
					 << " seq=" << seq << " call_ok=" << callOk
					 << " result=" << (reply != nullptr ? static_cast<int>(reply->result()) : -1);
			return;
		}
		// 落结局是存档改动(扣卡、改名、清记录、解除占用):此刻不可改(交接、退出中、没有归属围栏)就一个字都不动,
		// 记录和占用都留着。稍后(或交接之后的下一个持有者在加载时)用同一个 seq 重发,data_service 回同一个结局。
		if (!player_mutability::IsPlayerMutable(entity))
		{
			ScheduleRetry(playerId, nowMs);
			LOG_INFO << "[PlayerRename] RenameRole answered while the player is not mutable, outcome kept for a later retry: player_id="
					 << playerId << " seq=" << seq << " result=" << static_cast<int>(reply->result());
			return;
		}

		const PlayerRenameOp op = profile->rename_op();
		player_rename::Rule rule;
		const char *ruleProblem = nullptr;
		const bool hasRule = LoadCurrentRule(rule, ruleProblem);

		if (reply->result() == data_service::RENAME_ROLE_OK)
		{
			if (op.card_item_id() != 0 && op.card_count() != 0)
			{
				const ItemCountMap consumed{{op.card_item_id(), op.card_count()}};
				const ItemCountMap gained;
				const uint32_t consumeErr = BagService::ConsumeInventoryAndGrant(
					entity, consumed, gained, op.hold_id(), "player_rename", {}, op.hold_id());
				if (consumeErr != kSuccess)
				{
					// 角色在 data_service 已经改了名,卡却没扣成:不能就这样落结局(等于白改一次)。记录和占用都留着,
					// 退避后重发同一个 seq(data_service 回同一个「已提交」),直到扣成。占用挡着别的入口拿走这张卡,
					// 正常走不到这里;走到了每次重试都报一行,要人看。
					LOG_ERROR << "[PlayerRename] metric=player_rename_card_not_consumed player_id=" << playerId << " seq=" << seq
							  << " hold_id=" << op.hold_id() << " err=" << consumeErr
							  << ",the role is already renamed in data_service but the rename card could not be consumed;"
								 " the rename stays pending and is retried";
					ScheduleRetry(playerId, nowMs);
					return;
				}
			}
			BagService::ReleaseHold(entity, op.hold_id());

			const std::string finalName = reply->name().empty() ? op.name() : reply->name();
			const uint64_t renamedMs = reply->renamed_ms() != 0 ? reply->renamed_ms() : nowMs;
			profile->set_name(finalName);
			profile->set_renamed_ms(renamedMs);
			profile->clear_rename_op();
			tlsRuntimeOps.erase(playerId);
			RequestSave(entity);

			PlayerRenamedNotify notify;
			notify.set_name(finalName);
			if (hasRule)
			{
				notify.set_next_rename_time(player_rename::NextAllowedMs(renamedMs, rule.cooldownSeconds) / 1000ull);
			}
			PushNotify(entity, notify);
			LOG_INFO << "[PlayerRename] rename completed: player_id=" << playerId << " seq=" << seq;
			return;
		}

		if (reply->result() == data_service::RENAME_ROLE_STALE_SEQ && reply->current_seq() > profile->rename_seq())
		{
			profile->set_rename_seq(reply->current_seq());
		}
		BagService::ReleaseHold(entity, op.hold_id());
		profile->clear_rename_op();
		tlsRuntimeOps.erase(playerId);
		RequestSave(entity);

		const uint64_t nextRenameSec =
			reply->result() == data_service::RENAME_ROLE_COOLDOWN ? reply->next_allowed_ms() / 1000ull : 0;
		PushFailure(entity, player_rename::TipForRenameResult(reply->result()), nextRenameSec);
		LOG_INFO << "[PlayerRename] rename rejected: player_id=" << playerId << " seq=" << seq
				 << " result=" << static_cast<int>(reply->result());
	}

	void DispatchRenameRole(Guid playerId, uint64_t nowMs)
	{
		const entt::entity entity = tlsEcs.GetPlayer(playerId);
		if (entity == entt::null || !tlsEcs.actorRegistry.valid(entity))
		{
			return;
		}
		const auto *profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(entity);
		if (profile == nullptr || !profile->has_rename_op())
		{
			// 记录已经不在(结局落过了):运行态一起清掉,否则 Tick 每一拍都会再来问一次。
			tlsRuntimeOps.erase(playerId);
			return;
		}
		const auto &op = profile->rename_op();
		auto &rt = tlsRuntimeOps[playerId];
		if (rt.rpcInFlight && rt.inFlightSeq == op.seq())
		{
			return;
		}
		// 结局回来要改存档,此刻不可改就先不发(发了也落不了),退避后再看。交接走了的由下一个持有者在加载时接着发。
		if (!player_mutability::IsPlayerMutable(entity))
		{
			ScheduleRetry(playerId, nowMs);
			return;
		}

		player_rename::Rule rule;
		const char *ruleProblem = nullptr;
		if (!LoadCurrentRule(rule, ruleProblem))
		{
			LOG_ERROR << "[PlayerRename] invalid RoleNameRule while dispatching RenameRole: player_id=" << playerId
					  << " problem=" << ruleProblem;
			ScheduleRetry(playerId, nowMs);
			return;
		}

		data_service::RenameRoleRequest request;
		request.set_player_id(playerId);
		request.set_seq(op.seq());
		request.set_name(op.name());
		request.set_cooldown_seconds(rule.cooldownSeconds);
		request.set_hold_seconds(rule.oldNameHoldSeconds);
		request.set_min_chars(rule.minChars);
		request.set_max_chars(rule.maxChars);

		rt.rpcInFlight = true;
		rt.inFlightSeq = op.seq();
		rt.nextRetryAtMs = 0;

		const uint64_t seq = op.seq();
		const bool sent = SendRenameRoleRpc(
			request, nowMs,
			[playerId, seq](bool callOk, const data_service::RenameRoleResponse *reply)
			{
				HandleRenameRoleReply(playerId, seq, callOk, reply, TimeSystem::NowMillisecondsUTC());
			});
		if (!sent)
		{
			ScheduleRetry(playerId, nowMs);
		}
	}

	// 没有进行中的改名,却留着改名的占用:两者在同一帧里一起改、同一笔存盘,正常不会分家。真分家了改名卡会永远转不走也用不掉,
	// 所以加载时放掉并报错。先收集再释放:释放会改 PlayerItemHoldComp,不能边遍历边改。
	void ReleaseOrphanRenameHolds(entt::entity player, Guid playerId)
	{
		const auto *holds = tlsEcs.actorRegistry.try_get<PlayerItemHoldComp>(player);
		if (holds == nullptr)
		{
			return;
		}
		std::vector<uint64_t> orphans;
		for (const auto &hold : holds->holds())
		{
			if (hold.kind() == static_cast<uint32_t>(item_hold::ItemHoldKind::kRename))
			{
				orphans.push_back(hold.hold_id());
			}
		}
		for (const uint64_t holdId : orphans)
		{
			LOG_ERROR << "[PlayerRename] metric=player_rename_orphan_hold player_id=" << playerId << " hold_id=" << holdId
					  << ",a rename card hold has no pending rename; releasing it";
			BagService::ReleaseHold(player, holdId);
		}
	}

	uint32_t PrecheckRename(entt::entity player, const std::string &rawName, uint64_t nowMs,
							player_rename::Rule &outRule, std::string &outTrimmed)
	{
		auto &registry = tlsEcs.actorRegistry;
		if (!registry.valid(player))
		{
			return static_cast<uint32_t>(kThisEntityIsInvalid);
		}
		if (const uint32_t mutTip = CheckMutabilityTip(player); mutTip != kSuccess)
		{
			return mutTip;
		}
		const auto *guid = registry.try_get<PlayerGuidComp>(player);
		const auto *profile = registry.try_get<PlayerProfileComp>(player);
		const auto *bags = registry.try_get<PlayerBagsComp>(player);
		const auto *holds = registry.try_get<PlayerItemHoldComp>(player);
		if (guid == nullptr || guid->value == kInvalidGuid || profile == nullptr || bags == nullptr || holds == nullptr)
		{
			return static_cast<uint32_t>(kServiceUnavailable);
		}
		if (profile->has_rename_op())
		{
			return static_cast<uint32_t>(kRoleRenameInProgress);
		}
		const char *ruleProblem = nullptr;
		if (!LoadCurrentRule(outRule, ruleProblem))
		{
			LOG_ERROR << "[PlayerRename] RoleNameRule invalid: " << ruleProblem;
			return static_cast<uint32_t>(kServiceUnavailable);
		}
		if (!player_rename::TrimAndValidateBasic(rawName, outRule.minChars, outRule.maxChars, outTrimmed))
		{
			return static_cast<uint32_t>(kRoleNameInvalid);
		}
		if (outTrimmed == profile->name())
		{
			return static_cast<uint32_t>(kRoleRenameSameName);
		}
		if (player_rename::IsInCooldown(profile->renamed_ms(), outRule.cooldownSeconds, nowMs))
		{
			return static_cast<uint32_t>(kRoleRenameCooldown);
		}
		if (BagService::RemovableCount(player, bags->bags[kInventory], outRule.renameItemId) < outRule.renameItemCount)
		{
			return static_cast<uint32_t>(kRoleRenameNoCard);
		}
		return kSuccess;
	}
} // namespace

bool PlayerRenameSystem::ValidateTables()
{
	player_rename::Rule rule;
	const char *problem = nullptr;
	if (!LoadCurrentRule(rule, problem))
	{
		LOG_ERROR << "[PlayerRename] startup table check failed: " << problem;
		return false;
	}
	LOG_INFO << "[PlayerRename] table check passed: min_chars=" << rule.minChars
			 << " max_chars=" << rule.maxChars
			 << " cooldown_sec=" << rule.cooldownSeconds
			 << " hold_sec=" << rule.oldNameHoldSeconds
			 << " card_item_id=" << rule.renameItemId
			 << " card_count=" << rule.renameItemCount;
	return true;
}

uint32_t PlayerRenameSystem::RequestRename(entt::entity player, const std::string &rawName, uint64_t nowMs)
{
	player_rename::Rule rule;
	std::string trimmed;
	if (const uint32_t err = PrecheckRename(player, rawName, nowMs, rule, trimmed); err != kSuccess)
	{
		return err;
	}

	const Guid playerId = tlsEcs.GetPlayerId(player);
	auto &rt = tlsRuntimeOps[playerId];
	if (rt.checkingContent)
	{
		return static_cast<uint32_t>(kRoleRenameInProgress);
	}
	rt.checkingContent = true;

	const bool sent = ContentGuardClient::Check(
		content_guard::TEXT_SCENE_ROLE_NAME,
		playerId,
		trimmed,
		[playerId, trimmed, nowMs](const ContentGuardClient::Result &result)
		{
			if (const auto it = tlsRuntimeOps.find(playerId); it != tlsRuntimeOps.end())
			{
				it->second.checkingContent = false;
			}
			const entt::entity current = tlsEcs.GetPlayer(playerId);
			if (current == entt::null || !tlsEcs.actorRegistry.valid(current))
			{
				return;
			}
			if (!result.Passed())
			{
				const uint32_t tip = result.verdict == ContentGuardClient::Verdict::kUnavailable
										 ? static_cast<uint32_t>(kRoleNameCheckUnavailable)
										 : static_cast<uint32_t>(kRoleNameSensitive);
				PushFailure(current, tip);
				return;
			}

			// 异步回调回来时状态可能已变化(进入交易冻结、改名卡被移走、重复发起等),全量复核。
			player_rename::Rule latestRule;
			std::string latestTrimmed;
			if (const uint32_t recheckErr = PrecheckRename(current, trimmed, nowMs, latestRule, latestTrimmed);
				recheckErr != kSuccess)
			{
				PushFailure(current, recheckErr);
				return;
			}

			Guid holdId = kInvalidGuid;
			if (!AllocateHoldId(holdId) || holdId == kInvalidGuid)
			{
				LOG_ERROR << "[PlayerRename] failed to allocate hold_id (item segment unavailable): player_id=" << playerId;
				PushFailure(current, static_cast<uint32_t>(kServiceUnavailable));
				return;
			}

			const item_hold::HoldLines lines{{latestRule.renameItemId, latestRule.renameItemCount}};
			const uint32_t holdErr = BagService::PlaceHold(
				current, holdId, item_hold::ItemHoldKind::kRename, lines, nowMs, nowMs + player_rename::kHoldReviewAfterMs);
			if (holdErr != kSuccess)
			{
				const uint32_t tip = holdErr == static_cast<uint32_t>(kBagInsufficientItems)
										 ? static_cast<uint32_t>(kRoleRenameNoCard)
										 : static_cast<uint32_t>(kServiceUnavailable);
				PushFailure(current, tip);
				return;
			}

			auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(current);
			const uint64_t nextSeq = profile.rename_seq() + 1;
			profile.set_rename_seq(nextSeq);
			auto *op = profile.mutable_rename_op();
			op->set_seq(nextSeq);
			op->set_name(latestTrimmed);
			op->set_hold_id(holdId);
			op->set_card_item_id(latestRule.renameItemId);
			op->set_card_count(latestRule.renameItemCount);
			op->set_started_ms(nowMs);

			// 必须先将 rename_op 与 item_holds 一起持久化,待 OnSaveCommitted 再发 DataService.RenameRole。
			RequestSave(current);
			LOG_INFO << "[PlayerRename] rename_op staged and save requested: player_id=" << playerId
					 << " seq=" << nextSeq << " hold_id=" << holdId;
		});

	if (!sent)
	{
		rt.checkingContent = false;
		if (!rt.rpcInFlight && rt.nextRetryAtMs == 0)
		{
			tlsRuntimeOps.erase(playerId);
		}
		return static_cast<uint32_t>(kRoleNameCheckUnavailable);
	}
	return kSuccess;
}

void PlayerRenameSystem::OnPlayerLoaded(entt::entity player, uint64_t nowMs)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
	const auto *profile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(player);
	if (guid == nullptr || guid->value == kInvalidGuid || profile == nullptr)
	{
		return;
	}
	if (!profile->has_rename_op())
	{
		ReleaseOrphanRenameHolds(player, guid->value);
		return;
	}
	LOG_INFO << "[PlayerRename] resuming persisted rename_op on login: player_id=" << guid->value
			 << " seq=" << profile->rename_op().seq();
	DispatchRenameRole(guid->value, nowMs);
}

void PlayerRenameSystem::OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowMs)
{
	const auto &committedProfile = committed.player_database_data().profile_component();
	if (!committedProfile.has_rename_op())
	{
		return;
	}
	const entt::entity entity = tlsEcs.GetPlayer(playerId);
	if (entity == entt::null || !tlsEcs.actorRegistry.valid(entity))
	{
		return;
	}
	const auto *liveProfile = tlsEcs.actorRegistry.try_get<PlayerProfileComp>(entity);
	if (liveProfile == nullptr || !liveProfile->has_rename_op())
	{
		return;
	}
	if (liveProfile->rename_op().seq() != committedProfile.rename_op().seq())
	{
		return;
	}
	DispatchRenameRole(playerId, nowMs);
}

void PlayerRenameSystem::OnPlayerDestroyed(Guid playerId)
{
	tlsRuntimeOps.erase(playerId);
}

void PlayerRenameSystem::Tick(uint64_t nowMs)
{
	if (tlsRuntimeOps.empty())
	{
		return;
	}
	std::vector<Guid> due;
	for (const auto &[playerId, rt] : tlsRuntimeOps)
	{
		if (!rt.rpcInFlight && rt.nextRetryAtMs != 0 && nowMs >= rt.nextRetryAtMs)
		{
			due.push_back(playerId);
		}
	}
	for (const Guid playerId : due)
	{
		DispatchRenameRole(playerId, nowMs);
	}
}

void PlayerRenameSystem::SetTestHooks(TestHooks hooks)
{
	tlsTestHooks = std::move(hooks);
}

void PlayerRenameSystem::ResetTestState()
{
	tlsRuntimeOps.clear();
	tlsTestHooks = {};
}
