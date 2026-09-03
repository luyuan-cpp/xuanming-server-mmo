#pragma once

#include <cstdint>
#include <map>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

class AttributePanelInfo;

// 角色属性加点系统(问道式三池:属性点 / 相性点 / 仙魔点),设计文档
// docs/design/player-attribute-allocation.md。
//
// 唯一入口不变量:PlayerAttributeComp 的任何写入只经本类静态函数;
// 每个写操作成功后都 Recalculate(重算 DerivedAttributesComp)。
// 返回值:0 / kSuccess 语义以外一律为 tip id(table/proto/tip/attribute_error_tip.pb.h),
// handler 直接搬进 TipInfoMessage。
//
// 跨 zone 冻结(PlayerFrozenComp)期间所有写操作拒绝(cross-zone-readiness-audit.md §11.1);
// 战斗在途(InBattleComp)期间同样拒绝:快照已出,改属性会让局内数值与面板分叉。
class PlayerAttributeSystem {
public:
	// 从 DB 加载 / 首登后调用:补默认方案、清理不存在的维度、重算二级属性。
	static void InitializeOnLoad(entt::entity player);

	// 重算二级属性(DerivedAttributesComp + BaseAttributesComp.speed),夹当前 HP/MP。
	// 上限提升时当前 HP/MP 同步加同样的增量(升级手感);不落库。
	static void Recalculate(entt::entity player);

	// 面板全量(客户端零配表,维度名/说明/上限/剩余点都在这里)
	static void BuildPanel(entt::entity player, AttributePanelInfo& panel);

	// 确认加点:target 为该池"目标已分配"(全量、幂等;缺省维度视为不变)
	static uint32_t Allocate(entt::entity player, uint32_t poolId,
							 const std::map<uint32_t, uint32_t>& target);

	// 重置(洗点):清空当前方案里该池全部分配;按 AttributePool 表扣金币
	static uint32_t Reset(entt::entity player, uint32_t poolId);

	// 自动加点:按 AttributeAutoPlan(职业 → 通用兜底)算建议"目标已分配",只算不落
	static uint32_t AutoAllocate(entt::entity player, uint32_t poolId,
								 std::map<uint32_t, uint32_t>& suggested);

	static uint32_t CreateScheme(entt::entity player, const std::string& name, uint32_t& schemeId);
	static uint32_t SwitchScheme(entt::entity player, uint32_t schemeId);
	static uint32_t RenameScheme(entt::entity player, uint32_t schemeId, const std::string& name);

	// GM:设等级(1..kMaxLevel),触发 PlayerUpgradeEvent → 重算 + 推面板
	static uint32_t GmSetLevel(entt::entity player, uint32_t level);

	// 主动推面板(升级 / GM / 外部加成变化)
	static void PushPanel(entt::entity player);

	// 等级上限(表未定义前的保守值)
	static constexpr uint32_t kMaxLevel = 200;
};
