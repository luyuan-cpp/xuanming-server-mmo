#pragma once

#include <cstdint>
#include <functional>
#include <string_view>

#include <entt/src/entt/entity/entity.hpp>

class EquipLoadoutSnapshot;
class GetEquipLoadoutsResponse;
class SaveEquipLoadoutRequest;
class SaveEquipLoadoutResponse;
class SwitchEquipLoadoutRequest;
class SwitchEquipLoadoutResponse;
class UnlockEquipLoadoutRequest;
class UnlockEquipLoadoutResponse;

// 装备流派方案预设系统（一键多套换装 + 付费扩展套位 + 战法/加点方案联动）。
// 唯一写入口：PlayerEquipLoadoutComp 只经本类读写，严禁复制 ItemComp，严禁另建隐藏仓库。
class PlayerEquipLoadoutSystem
{
public:
	using PersistFn = void (*)(entt::entity);
	using NameFilterFn = std::function<bool(std::string_view)>;

	static uint32_t Get(entt::entity player, GetEquipLoadoutsResponse &response);
	static void Save(entt::entity player, const SaveEquipLoadoutRequest &request, SaveEquipLoadoutResponse &response);
	static void Switch(entt::entity player, const SwitchEquipLoadoutRequest &request,
					   SwitchEquipLoadoutResponse &response);
	static void Unlock(entt::entity player, const UnlockEquipLoadoutRequest &request,
					   UnlockEquipLoadoutResponse &response);

	static uint32_t BuildSnapshot(entt::entity player, EquipLoadoutSnapshot &snapshot);

	// 仅供单测注入：替换落盘回调或敏感词过滤器；传空恢复生产默认。
	static void SetPersistForTest(PersistFn fn);
	static void SetNameFilterForTest(NameFilterFn fn);
};
