
#include "player_bag_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include "modules/bag/bag_system.h" // BagType:kInventory / kEquipment
#include "player_gm_guard.h"        // GM 客户端指令的 scene 侧第二道锁
#include "services/scene/player/system/player_equip.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	// 失败码统一写全局 tip 实体,由生成层 TRANSFER_ERROR_MESSAGE 搬进响应并清空
	void SetTip(uint32_t err)
	{
		tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(err);
	}

	// 穿 / 脱成功之后把人物背包与装备栏的全量带回去,客户端整体覆盖两份缓存。
	//
	// 走到这里时穿脱已经生效,不存在"回滚"这回事。任何一份快照建不出来(两层不一致等)都不发半份:
	// 清掉响应里的两个包并回 tip,客户端按失败处理后重新 GetBag 就能看到真实状态。
	// EquipItemResponse 与 UnequipItemResponse 字段同形,所以写成模板而不是抄两遍。
	template <typename Response>
	void ReplyInventoryAndEquipment(entt::entity player, Response &response)
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
			SetTip(result);
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
        tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
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
        tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity()).set_id(result);
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
		SetTip(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::Equip(player, request->item_id()); err != kSuccess)
	{
		SetTip(err);
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
		SetTip(kInvalidParameter);
		return;
	}
	if (const auto err = PlayerEquipSystem::Unequip(player, request->item_id()); err != kSuccess)
	{
		SetTip(err);
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
		SetTip(kFeatureUnavailable);
		return;
	}
	// 参数范围(config 是否存在、数量上限)由 PlayerEquipSystem::GmGrantItem 统一判,不在这里抄第二份。
	if (const auto err = PlayerEquipSystem::GmGrantItem(player, request->config_id(), request->count());
		err != kSuccess)
	{
		SetTip(err);
		return;
	}
	if (const auto result = PlayerBagSystem::BuildSnapshot(player, kInventory, *response->mutable_bag());
		result != kSuccess)
	{
		response->clear_bag();
		SetTip(result);
	}
///<<< END WRITING YOUR CODE

}
