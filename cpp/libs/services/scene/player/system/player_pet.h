#pragma once

#include <cstdint>
#include <map>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

class PetListInfo;
class PetInstance;
class BattlePetSnapshot;
class BattlePetSettlementData;

// 宝宝(宠物)系统,设计文档 docs/design/player-pet.md。
//
// 唯一入口不变量:PlayerPetComp 的任何写入只经本类静态函数;每个写操作成功后都
// Recalculate(同步等级 + 按新上限处理当前 HP/MP)。
// 返回值:kSuccess 以外一律为 tip id(table/proto/tip/pet_error_tip.pb.h),handler 直接搬进
// TipInfoMessage。
//
// 与角色属性加点的关系:点数总量、"目标已分配"校验、按比例保持 HP/MP 全部复用
// attributerules;维度与池取表中 owner_type=1 的行。宝宝的二级属性**不落库、不缓存**,
// 需要时由 ComputeDerived 现算,全仓只有"已分配点 + 资质 + 主人等级"三个输入是真相。
//
// 跨 zone 冻结(PlayerFrozenComp)与战斗在途(InBattleComp)期间所有写操作拒绝:
// 快照已出,改宝宝会让局内单位与面板分叉(与 PlayerAttributeSystem::CheckWritable 同口径)。
//
// 线程模型:全部方法都在 scene 节点 loop 线程调用。
class PetSystem {
public:
	// 从 DB 加载 / 首登后调用:清理已失效引用、同步等级、按上限夹当前 HP/MP。
	static void InitializeOnLoad(entt::entity player);

	// 重算的触发原因(与 PlayerAttributeSystem::RecalcReason 同语义):
	//   kLevelChanged 宝宝升级(主人升级带动):上限抬高按绝对增量补当前值,降级只夹;
	//   其它:按比例保持,一升一降往返零净得失(不能把加点/洗点变成宝宝的治疗手段)。
	enum class RecalcReason : uint8_t { kLoad, kLevelChanged, kAllocate, kReset };

	// 重算全部宝宝(同步等级 + 处理当前 HP/MP);不落库。
	static void RecalculateAll(entt::entity player, RecalcReason reason = RecalcReason::kLoad);

	// 面板全量(客户端零配表:维度名/说明/上限/剩余点/资质/成长率都在这里)
	static void BuildList(entt::entity player, PetListInfo& list);

	// 出战 / 收回。出战同时只能有一只(PetRule.max_pets 管的是携带量,不是出战量)。
	static uint32_t Summon(entt::entity player, uint64_t petId);
	static uint32_t Recall(entt::entity player);

	// 确认加点:target 为该宝宝"目标已分配"(全量、幂等;缺省维度视为不变)
	static uint32_t Allocate(entt::entity player, uint64_t petId,
							 const std::map<uint32_t, uint32_t>& target);

	// 洗点:清空该宝宝全部分配,按 AttributePool 表扣金币
	static uint32_t Reset(entt::entity player, uint64_t petId);

	// 自动加点:按 AttributeAutoPlan(宝宝池)算建议"目标已分配",只算不落
	static uint32_t AutoAllocate(entt::entity player, uint64_t petId,
								 std::map<uint32_t, uint32_t>& suggested);

	static uint32_t Rename(entt::entity player, uint64_t petId, const std::string& name);

	// GM / 未来的捕捉与购买共用的发放入口:铸 pet_id、掷资质、落 PlayerPetComp。
	// petIdOut 仅在返回 kSuccess 时有效。
	static uint32_t GrantPet(entt::entity player, uint32_t petTableId, uint64_t& petIdOut);

	// —— 战斗接缝(scene/battle/system/player_battle.cpp 调用)——

	// 出战宝宝的战斗快照。没有出战宝宝返回 false(不是错误)。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePetSnapshot& snapshot);

	// 结算回写:HP/MP 终值按现算上限夹后落实例;阵亡宝宝回满(与玩家同口径,
	// 否则 0 血宝宝会一直被快照进新局、开局即倒)。未知 pet_id 忽略并告警。
	static void ApplyBattleSettlement(entt::entity player, const ::BattlePetSettlementData& settlement);

	// 主动推列表(主人升级带动宝宝升级、结算回写等)
	static void PushList(entt::entity player);

	// 宝宝属性点池的 owner_type 值(AttributePool.owner_type)
	static constexpr uint32_t kPoolOwnerPet = 1;
};
