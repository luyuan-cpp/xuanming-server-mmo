
#include "player_bag_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <string>
#include <utility>

#include "modules/bag/bag_system.h" // BagType:kInventory / kEquipment / kTemporary
#include "player_gm_guard.h"        // GM 客户端指令的 scene 侧第二道锁
#include "services/scene/player/system/client_request_ledger.h"
#include "services/scene/player/system/player_equip.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 穿 / 脱 / 强化 / 镶嵌 / 取下成功之后把人物背包与装备栏的全量带回去,客户端整体覆盖两份缓存。
	//
	// 走到这里时操作已经生效,不存在"回滚"这回事。任何一份快照建不出来(两层不一致等)都不发半份:
	// 清掉响应里的两个包并回 tip,客户端按失败处理后重新 GetBag 就能看到真实状态。
	// 五个响应的 bag / equipment 字段同号同名,所以写成模板而不是抄五遍。
	// 返回两份快照是否都带上了:强化要靠它决定写不写结果字段(回了 tip 的响应不带任何结果)。
	template <typename Response>
	bool ReplyInventoryAndEquipment(entt::entity player, Response &response)
	{
		auto result = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		if (result == kSuccess)
		{
			result = PlayerBagSystem::BuildSnapshot(player, kEquipment, *response.mutable_equipment());
		}
		if (result != kSuccess)
		{
			response.clear_bag();
			response.clear_equipment();
			response.mutable_error_message()->set_id(result);
			return false;
		}
		return true;
	}

	// 临时格取回成功之后把人物背包与临时格的全量带回去,口径同上:搬运已经生效,任何一份快照建不出来
	// 都不发半份,清掉两个包并回 tip,客户端重新 GetBag 就能看到真实状态。
	void ReplyInventoryAndTemporary(entt::entity player, ::RetrieveTemporaryItemsResponse &response)
	{
		auto result = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		if (result == kSuccess)
		{
			result = PlayerBagSystem::BuildSnapshot(player, kTemporary, *response.mutable_temporary());
		}
		if (result != kSuccess)
		{
			response.clear_bag();
			response.clear_temporary();
			response.mutable_error_message()->set_id(result);
		}
	}
} // namespace
///<<< END WRITING YOUR CODE

void SceneBagClientPlayerHandler::GetBag(entt::entity player,const ::GetBagRequest* request,
	::GetBagResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
const auto result = PlayerBagSystem::BuildSnapshot(player, request->bag_type(), *response->mutable_bag());
    if (result != kSuccess) {
        response->clear_bag();
        response->mutable_error_message()->set_id(result);
    }
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::SortBag(entt::entity player,const ::SortBagRequest* request,
	::SortBagResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
bool changed = false;
    const auto result = PlayerBagSystem::Sort(player, request->bag_type(), *response->mutable_bag(), changed);
    if (result != kSuccess) {
        response->clear_bag();
        response->mutable_error_message()->set_id(result);
        return;
    }
    response->set_changed(changed);
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::EquipItem(entt::entity player,const ::EquipItemRequest* request,
	::EquipItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 规则(等级 / 职业 / 选槽 / 战斗中与冻结中拒绝)全在 PlayerEquipSystem,这里只做传输层的事。
	if (request->item_id() == 0)
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::Equip(player, request->item_id()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	ReplyInventoryAndEquipment(player, *response);
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::UnequipItem(entt::entity player,const ::UnequipItemRequest* request,
	::UnequipItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request->item_id() == 0)
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::Unequip(player, request->item_id()); err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	ReplyInventoryAndEquipment(player, *response);
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::GmGrantItem(entt::entity player,const ::GmGrantItemRequest* request,
	::GmGrantItemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 这是"prod 关掉"而不是鉴权:gate 按消息号的 GM 清单(gate_gm_client_messages.h)在非 dev/test
	// 直接拒绝;这里是防绕开 gate 直连 scene 的第二道锁,判据 SCENE_RUN_MODE(默认 prod = 拒绝)。
	// 见 docs/design/equipment-attributes.md §4.6 与 cpp/nodes/gate/SECURITY.md §3。
	if (scene_gm_guard::RejectGmClientRpc("GmGrantItem"))
	{
		response->mutable_error_message()->set_id(kFeatureUnavailable);
		return;
	}
	// 参数范围(config 是否存在、数量上限)由 PlayerEquipSystem::GmGrantItem 统一判,不在这里抄第二份。
	if (const auto err = PlayerEquipSystem::GmGrantItem(player, request->config_id(), request->count());
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	if (const auto result = PlayerBagSystem::BuildSnapshot(player, kInventory, *response->mutable_bag());
		result != kSuccess)
	{
		response->clear_bag();
		response->mutable_error_message()->set_id(result);
	}
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::EnhanceEquip(entt::entity player,const ::EnhanceEquipRequest* request,
	::EnhanceEquipResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 规则(对象在哪、上限、成功率、金币与材料、战斗中与冻结中拒绝、expected_level 乐观比较)全在 PlayerEquipSystem,
	// 这里只做传输层的事。expected_level 的 0 是合法值(未强化),不在这里判。见 docs/design/equipment-enhance-inlay.md §4.4。
	if (request->item_id() == 0)
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	// 客户端幂等键(docs/design/client-request-idempotency.md):查账排在 Enhance 的一切校验之前,包括 expected_level ——
	// 首次已经成功或掉过级的话等级已经变了,重放先比等级会被拒成 kEquipEnhanceLevelChanged,客户端就以为第一次没成。
	// 指纹里带着 item_id 与 expected_level:合法的重发每个字段都原样不变,换了任何一个都是换了参数。
	const uint64_t clientRequestId = request->client_request_id();
	std::string fingerprint;
	ClientRequestResult first;
	auto verdict = ClientRequestVerdict::kNew;
	if (clientRequestId != 0)
	{
		fingerprint = ClientRequestFingerprint(*request);
		verdict = client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kEnhanceEquip, fingerprint, first);
	}
	if (verdict == ClientRequestVerdict::kMismatch)
	{
		// 同一个键换了装备或期望等级:客户端把旧键用在了新操作上,拒绝且不执行。
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (verdict == ClientRequestVerdict::kUnavailable)
	{
		// 这名玩家的幂等键账本缺失或已判坏:分不清是不是重发,不执行(fail-closed)。
		response->mutable_error_message()->set_id(kServiceUnavailable);
		return;
	}
	PlayerEquipSystem::EnhanceOutcome outcome;
	if (verdict == ClientRequestVerdict::kReplay)
	{
		// 首次已经扣过、掷过:不再扣、不再掷,回首次的成败与等级,两份快照取此刻的。
		outcome.success = first.enhanceSuccess;
		outcome.level = first.enhanceLevel;
		response->set_replayed(true);
	}
	else
	{
		if (const auto err = PlayerEquipSystem::Enhance(player, request->item_id(), request->expected_level(), outcome);
			err != kSuccess)
		{
			response->mutable_error_message()->set_id(err);
			return;
		}
		// 受理了才记键(掷骰没过也是受理:钱和材料已经扣了)。下面快照组不出来也照样记,强化已经生效。
		ClientRequestResult result;
		result.enhanceSuccess = outcome.success;
		result.enhanceLevel = outcome.level;
		client_request_dedup::Remember(player, clientRequestId, ClientRequestKind::kEnhanceEquip, std::move(fingerprint),
									   result);
	}
	// 走到这里金币与材料已经扣了(首次)或早就扣过了(重放)。掷骰失败**不是错误**:不回 tip,两份快照照常带回,
	// 靠 success 区分 —— 客户端见到 tip 会保留旧快照,而旧快照里的金币 / 材料数已经不对了。
	if (ReplyInventoryAndEquipment(player, *response))
	{
		response->set_success(outcome.success);
		response->set_enhance_level(outcome.level);
	}
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::InlayGem(entt::entity player,const ::InlayGemRequest* request,
	::InlayGemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// socket 可以是 0(孔位号从 0 起),不在这里判;孔位是否存在、宝石是否合适由 PlayerEquipSystem 判。
	if (request->item_id() == 0 || request->gem_config_id() == 0)
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::InlayGem(player, request->item_id(), request->socket(),
													 request->gem_config_id());
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	ReplyInventoryAndEquipment(player, *response);
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::RemoveGem(entt::entity player,const ::RemoveGemRequest* request,
	::RemoveGemResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	if (request->item_id() == 0)
	{
		response->mutable_error_message()->set_id(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::RemoveGem(player, request->item_id(), request->socket());
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	ReplyInventoryAndEquipment(player, *response);
///<<< END WRITING YOUR CODE

}

void SceneBagClientPlayerHandler::RetrieveTemporaryItems(entt::entity player,const ::RetrieveTemporaryItemsRequest* request,
	::RetrieveTemporaryItemsResponse* response)
{
///<<< BEGIN WRITING YOUR CODE
	// 规则(二选一的参数、取哪几件、放不放得下、传送途中拒绝;战斗中放行,2026-11-06 评审修正)全在 PlayerBagSystem::RetrieveTemporary,
	// 这里只做传输层的事。见 docs/design/bag-rule-policy-layering.md §6.6。
	if (const auto err = PlayerBagSystem::RetrieveTemporary(player, request->item_id(), request->take_all());
		err != kSuccess)
	{
		response->mutable_error_message()->set_id(err);
		return;
	}
	ReplyInventoryAndTemporary(player, *response);
///<<< END WRITING YOUR CODE

}
