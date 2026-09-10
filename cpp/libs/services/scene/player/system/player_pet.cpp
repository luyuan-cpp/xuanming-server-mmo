#include "player_pet.h"

#include <algorithm>
#include <limits>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/utils/random/random.h"
#include "engine/core/time/system/time.h"
#include "network/player_message_utils.h"
#include "thread_context/ecs_context.h"
#include "modules/id_segment/guid_segment_registry.h"

#include "battle/system/player_battle.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/attribute_allocation_rules.h"
#include "player/system/pet_rules.h"

#include "rpc/service_metadata/player_pet_service_metadata.h"
#include "table/code/attributeautoplan_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/pet_table.h"
#include "table/code/petrule_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/pet_error_tip.pb.h"

#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/scene/player_pet.pb.h"

namespace {

using attributerules::AllocError;
using attributerules::PoolRule;

uint64_t GuidForLog(entt::entity player) {
	const auto* guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	return guid != nullptr ? *guid : 0;
}

uint32_t OwnerLevel(entt::entity player) {
	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	return (levelComp != nullptr && levelComp->level() > 0) ? levelComp->level() : 1;
}

const PetRuleTable* RuleRow() {
	const auto& rows = PetRuleTableManager::Instance().FindAll().data();
	return rows.size() > 0 ? &rows.Get(0) : nullptr;
}

uint32_t MaxPets() {
	const auto* rule = RuleRow();
	return (rule != nullptr && rule->max_pets() > 0) ? rule->max_pets() : 1;
}

// 宝宝属性点池:表里 owner_type=1 的第一行。核心线只有一个宝宝池,
// 多池(如二期的宝宝相性)时这里改成按 pool_id 参数取即可。
const AttributePoolTable* PetPool() {
	const auto& pools = AttributePoolTableManager::Instance().FindAll().data();
	for (const auto& pool : pools) {
		if (pool.owner_type() == PetSystem::kPoolOwnerPet) {
			return &pool;
		}
	}
	return nullptr;
}

// 宝宝维度,**按 dimension_id 升序**。这个顺序是对外契约:Pet.xlsx 的
// aptitude_min/aptitude_max 四个槽位按它一一对应(401 体质 / 402 灵力 / 403 力量 / 404 敏捷)。
// 用表行迭代序的话,策划在表中间插一行或调换两行,所有存量宝宝的资质就会整体错位 ——
// 而且是静默错位,没有任何报错。面板排序另有 sort 列,交给客户端。
std::vector<const AttributeDimensionTable*> PetDimensions(uint32_t poolId) {
	std::vector<const AttributeDimensionTable*> result;
	for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
		result.push_back(dim);
	}
	std::sort(result.begin(), result.end(),
			  [](const AttributeDimensionTable* lhs, const AttributeDimensionTable* rhs) {
				  return lhs->id() < rhs->id();
			  });
	return result;
}

// 宝宝实际可用的技能 = 种类自带(PetTable.skill)∪ 实例已学(PetInstance.skill_table_ids)。
// 面板和战斗快照必须走这同一个函数:早先面板现读表、快照读实例副本,改表之后两边就分叉,
// 玩家在面板上看到的技能和它在战斗里真正会放的不是一回事。
// (核心线还没有技能书,实例那半边恒空;字段留着是给二期的学习入口。)
std::vector<uint32_t> ResolveSkills(const PetInstance& pet, const PetTable& row) {
	std::vector<uint32_t> skills;
	for (int i = 0; i < row.skill_size(); ++i) {
		if (row.skill(i) != 0) {
			skills.push_back(row.skill(i));
		}
	}
	for (const auto skillTableId : pet.skill_table_ids()) {
		if (skillTableId != 0 &&
			std::find(skills.begin(), skills.end(), skillTableId) == skills.end()) {
			skills.push_back(skillTableId);
		}
	}
	return skills;
}

PoolRule ToRule(const AttributePoolTable& row) {
	PoolRule rule;
	rule.poolId = row.id();
	rule.unlockLevel = row.unlock_level();
	rule.pointsPerLevel = row.points_per_level();
	rule.basePoints = row.base_points();
	rule.dimensionCap = row.dimension_cap();
	return rule;
}

uint32_t AllocatedIn(const PetInstance& pet, uint32_t dimensionId) {
	const auto it = pet.allocated().find(dimensionId);
	return it == pet.allocated().end() ? 0 : it->second;
}

uint32_t AptitudeIn(const PetInstance& pet, uint32_t dimensionId) {
	const auto it = pet.aptitude().find(dimensionId);
	return it == pet.aptitude().end() ? petrules::kAptitudeBase : it->second;
}

uint32_t UsedPoints(const PetInstance& pet) {
	uint64_t used = 0;
	for (const auto& [_, value] : pet.allocated()) {
		used += value;
	}
	return used > std::numeric_limits<uint32_t>::max() ? std::numeric_limits<uint32_t>::max()
													  : static_cast<uint32_t>(used);
}

// 把表行 + 实例翻译成纯规则输入(纯规则不认识表也不认识 proto)
std::vector<petrules::DimensionInput> BuildDimensionInputs(const PetInstance& pet, uint32_t poolId) {
	std::vector<petrules::DimensionInput> inputs;
	for (const auto* dim : PetDimensions(poolId)) {
		petrules::DimensionInput input;
		input.dimensionId = dim->id();
		input.coefficients.basePerLevel = dim->base_per_level();
		input.coefficients.maxHealth = dim->max_health();
		input.coefficients.maxMana = dim->max_mana();
		input.coefficients.physicalAttack = dim->physical_attack();
		input.coefficients.magicAttack = dim->magic_attack();
		input.coefficients.speed = dim->speed();
		input.coefficients.defense = dim->defense();
		input.allocated = AllocatedIn(pet, dim->id());
		input.aptitude = AptitudeIn(pet, dim->id());
		inputs.push_back(input);
	}
	return inputs;
}

petrules::BaseValues BaseValuesOf(const PetTable& row) {
	petrules::BaseValues base;
	base.maxHealth = row.init_health();
	base.maxMana = row.init_mana();
	base.speed = row.init_speed();
	return base;
}

const PetTable* PetRow(uint32_t petTableId) {
	const auto [row, err] = PetTableManager::Instance().FindByIdSilent(petTableId);
	return row;
}

PetInstance* FindPet(PlayerPetComp& comp, uint64_t petId) {
	for (auto& pet : *comp.mutable_pets()) {
		if (pet.pet_id() == petId) {
			return &pet;
		}
	}
	return nullptr;
}

const PetInstance* FindPet(const PlayerPetComp& comp, uint64_t petId) {
	for (const auto& pet : comp.pets()) {
		if (pet.pet_id() == petId) {
			return &pet;
		}
	}
	return nullptr;
}

// 清掉表里已不存在(或已不属于宝宝池)的维度。不清的话 UsedPoints 会把这些点一直算进
// "已用",玩家的宝宝点数凭空少一截且永远拿不回来 —— 角色侧 SanitizeSchemes 的对等物。
void SanitizeAllocations(PlayerPetComp& comp, uint32_t poolId) {
	std::vector<uint32_t> valid;
	for (const auto* dim : PetDimensions(poolId)) {
		valid.push_back(dim->id());
	}
	for (auto& pet : *comp.mutable_pets()) {
		std::vector<uint32_t> stale;
		for (const auto& [dimensionId, _] : pet.allocated()) {
			if (std::find(valid.begin(), valid.end(), dimensionId) == valid.end()) {
				stale.push_back(dimensionId);
			}
		}
		for (const auto dimensionId : stale) {
			pet.mutable_allocated()->erase(dimensionId);
		}
	}
}

PlayerPetComp& EnsureComp(entt::entity player) {
	// 初始化路径允许 get_or_emplace(ecs-component-access-rules.md)
	return tlsEcs.actorRegistry.get_or_emplace<PlayerPetComp>(player);
}

uint32_t MapAllocError(AllocError err) {
	switch (err) {
	case AllocError::kOk: return kSuccess;
	case AllocError::kPoolLocked: return kPetOwnerLevelNotEnough;
	case AllocError::kDimensionNotInPool: return kPetDimensionNotFound;
	case AllocError::kCannotDecrease: return kPetPointsCannotDecrease;
	case AllocError::kCapExceeded: return kPetDimensionCapExceeded;
	case AllocError::kNotEnoughPoints: return kPetPointsNotEnough;
	case AllocError::kNothingToChange: return kPetNothingToChange;
	}
	return kInvalidParameter;
}

// 写操作统一前置:实体有效 / 未跨 zone 冻结 / 不在战斗(与角色属性同口径)
uint32_t CheckWritable(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player)) {
		LOG_WARN << "[PlayerPet] 拒绝: 跨 zone 冻结中 player_id=" << GuidForLog(player);
		return kInvalidParameter;
	}
	if (PlayerBattleSystem::IsInBattle(player)) {
		return kPetInBattle;
	}
	return kSuccess;
}

// 宝宝名校验:非空、不超表定码点数、无控制字符、至少一个可见码点
// (口径与 player_attribute.cpp 的方案名一致 —— 全空格/零宽空格在列表里是一片空白)
bool IsInvisibleCodePoint(uint32_t cp) {
	return cp == 0x20 || cp == 0xA0 || (cp >= 0x2000 && cp <= 0x200F) || cp == 0x2028 ||
		   cp == 0x2029 || cp == 0x202F || cp == 0x205F || cp == 0x2060 || cp == 0x3000 || cp == 0xFEFF;
}

bool IsPetNameValid(const std::string& name) {
	if (name.empty()) {
		return false;
	}
	const auto* rule = RuleRow();
	const uint32_t maxLen = (rule != nullptr && rule->name_max_len() > 0) ? rule->name_max_len() : 8;
	uint32_t codePoints = 0;
	bool hasVisible = false;
	for (size_t i = 0; i < name.size();) {
		const auto c = static_cast<unsigned char>(name[i]);
		if (c < 0x20 || c == 0x7f) {
			return false;
		}
		// 非法 UTF-8 前导字节:孤立的续字节(0x80-0xBF)、以及 0xC0/0xC1 与 0xF8 以上
		// (它们不构成任何合法序列)。放过去的话名字会带着非法字节进 proto string,
		// 到存盘那一步才炸,而且是这个玩家**每一次**存盘都炸。
		if ((c >= 0x80 && c < 0xC2) || c >= 0xF5) {
			return false;
		}
		uint32_t cp = c;
		size_t len = 1;
		if (c >= 0xF0) { cp = c & 0x07; len = 4; }
		else if (c >= 0xE0) { cp = c & 0x0F; len = 3; }
		else if (c >= 0xC0) { cp = c & 0x1F; len = 2; }
		if (i + len > name.size()) {
			return false;  // 截断的 UTF-8 序列
		}
		for (size_t k = 1; k < len; ++k) {
			const auto cc = static_cast<unsigned char>(name[i + k]);
			if ((cc & 0xC0) != 0x80) {
				return false;
			}
			cp = (cp << 6) | (cc & 0x3F);
		}
		i += len;
		++codePoints;
		if (!IsInvisibleCodePoint(cp)) {
			hasVisible = true;
		}
	}
	return hasVisible && codePoints <= maxLen;
}

uint64_t ResetCostFor(const PetInstance& pet, const AttributePoolTable& pool) {
	if (pool.reset_free_below_level() > 0 && pet.level() < pool.reset_free_below_level()) {
		return 0;
	}
	return pool.reset_cost_gold();
}

// 一只宝宝的二级属性:现算,不缓存(见头文件"没有第二份真相")
petrules::DerivedAttributes ComputeDerived(const PetInstance& pet, const PetTable& row, uint32_t poolId) {
	return petrules::ComputeDerived(BaseValuesOf(row), BuildDimensionInputs(pet, poolId), pet.level());
}

// 单只宝宝重算:同步等级 + 按新上限处理当前 HP/MP + 收敛超额分配。
// 返回是否发生了变化(仅用于日志)。
//
// previous:调用方在**改动 allocated 之前**拿到的旧二级属性。宝宝的二级属性是现算的
// (不像角色有 DerivedAttributesComp 存着上一份),加点 / 洗点改完 allocated 再进来算,
// "旧上限"就已经等于新上限了 —— 按比例保持会退化成恒等式:洗点把上限打下去时当前血只被夹,
// 再把点加回来血也不涨,一趟往返白掉一截血。所以这两条路径必须由调用方传旧值进来。
// kLoad / kLevelChanged 传 nullptr:那两条路径改的是 level,旧值在本函数内 set_level 之前算得到。
bool RecalculateOne(entt::entity player, PetInstance& pet, const AttributePoolTable& pool,
					PetSystem::RecalcReason reason,
					const petrules::DerivedAttributes* previous = nullptr) {
	const auto* row = PetRow(pet.pet_table_id());
	if (row == nullptr) {
		// 种类行被删/改表:保留实例(玩家资产不能悄悄没),召唤时拒绝,这里只告警
		LOG_WARN << "[PlayerPet] 种类行缺失,跳过重算: player_id=" << GuidForLog(player)
				 << " pet_id=" << pet.pet_id() << " pet_table_id=" << pet.pet_table_id();
		return false;
	}

	const auto oldLevel = pet.level();
	const auto newLevel = petrules::EffectiveLevel(OwnerLevel(player), row->level_cap());
	const auto oldDerived = previous != nullptr ? *previous : ComputeDerived(pet, *row, pool.id());
	pet.set_level(newLevel);

	// 已分配 > 总量的收敛(等级下降 / 改表缩点):整池清零返还,口径与角色一致
	const auto total = attributerules::TotalPoints(ToRule(pool), newLevel, 0);
	if (UsedPoints(pet) > total) {
		pet.mutable_allocated()->clear();
		LOG_WARN << "[PlayerPet] 已分配超总量,清零返还: player_id=" << GuidForLog(player)
				 << " pet_id=" << pet.pet_id() << " total=" << total;
	}

	const auto newDerived = ComputeDerived(pet, *row, pool.id());
	if (reason == PetSystem::RecalcReason::kLevelChanged) {
		// 与角色 PlayerAttributeSystem::Recalculate 逐条对齐:抬高按绝对增量补当前值,
		// **降低只夹**(不按比例缩)。早先这里把降级也丢给按比例分支,主人被 GM 降级 /
		// 回档时宝宝会连着掉一截当前血,和文档 §3.3 承诺的口径分叉。
		if (pet.health() > 0 && newDerived.maxHealth > oldDerived.maxHealth && oldDerived.maxHealth > 0) {
			pet.set_health(pet.health() + (newDerived.maxHealth - oldDerived.maxHealth));
		}
		if (newDerived.maxMana > oldDerived.maxMana && oldDerived.maxMana > 0) {
			pet.set_mana(pet.mana() + (newDerived.maxMana - oldDerived.maxMana));
		}
		pet.set_health(std::min(pet.health(), newDerived.maxHealth));
		if (newDerived.maxMana > 0) {
			pet.set_mana(std::min(pet.mana(), newDerived.maxMana));
		}
	} else {
		// 加载 / 加点 / 洗点:按比例保持,一升一降往返零净得失
		// (否则"洗点降上限 → 再加回来"就是宝宝的免费回血,和角色那条被堵掉的路一模一样)
		pet.set_health(attributerules::RescaleCurrent(pet.health(), oldDerived.maxHealth, newDerived.maxHealth));
		pet.set_mana(attributerules::RescaleCurrent(pet.mana(), oldDerived.maxMana, newDerived.maxMana));
	}
	return oldLevel != newLevel;
}

void FillPetInfo(entt::entity player, const PlayerPetComp& comp, const PetInstance& pet,
				 const AttributePoolTable& pool, PetInfo& info) {
	info.set_pet_id(pet.pet_id());
	info.set_pet_table_id(pet.pet_table_id());
	info.set_level(pet.level());
	info.set_is_active(comp.active_pet_id() == pet.pet_id());
	info.set_pool_id(pool.id());

	const auto* row = PetRow(pet.pet_table_id());
	info.set_name(!pet.name().empty() ? pet.name() : (row != nullptr ? row->name() : std::string()));
	if (row != nullptr) {
		info.set_quality(row->quality());
		info.set_model_id(row->model_id());
		info.set_desc(row->desc());
		for (const auto skillTableId : ResolveSkills(pet, *row)) {
			info.add_skill_table_ids(skillTableId);
		}
	}

	const auto total = attributerules::TotalPoints(ToRule(pool), pet.level(), 0);
	const auto used = UsedPoints(pet);
	info.set_total_points(total);
	info.set_remaining_points(total > used ? total - used : 0);
	info.set_reset_cost_gold(ResetCostFor(pet, pool));

	const auto inputs = BuildDimensionInputs(pet, pool.id());
	info.set_growth(petrules::GrowthPermyriad(inputs));
	for (const auto* dim : PetDimensions(pool.id())) {
		auto* dimensionInfo = info.add_dimensions();
		dimensionInfo->set_dimension_id(dim->id());
		dimensionInfo->set_pool_id(dim->pool_id());
		dimensionInfo->set_name(dim->name());
		dimensionInfo->set_desc(dim->desc());
		dimensionInfo->set_allocated(AllocatedIn(pet, dim->id()));
		dimensionInfo->set_cap(pool.dimension_cap());
		dimensionInfo->set_sort(dim->sort());
		dimensionInfo->set_aptitude(AptitudeIn(pet, dim->id()));
		petrules::DimensionInput one;
		one.coefficients.basePerLevel = dim->base_per_level();
		one.allocated = AllocatedIn(pet, dim->id());
		dimensionInfo->set_value(petrules::DimensionValue(one, pet.level()));
	}

	auto* derivedInfo = info.mutable_derived();
	if (row != nullptr) {
		const auto derived = ComputeDerived(pet, *row, pool.id());
		derivedInfo->set_max_health(derived.maxHealth);
		derivedInfo->set_max_mana(derived.maxMana);
		derivedInfo->set_physical_attack(derived.physicalAttack);
		derivedInfo->set_magic_attack(derived.magicAttack);
		derivedInfo->set_speed(derived.speed);
		derivedInfo->set_defense(derived.defense);
	}
	derivedInfo->set_health(pet.health());
	derivedInfo->set_mana(pet.mana());
}

}  // namespace

void PetSystem::InitializeOnLoad(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto& comp = EnsureComp(player);
	// 出战指向一只已不存在的宝宝(异常存档 / 二期放生后的残留):就地纠正为未出战
	if (comp.active_pet_id() != 0 && FindPet(comp, comp.active_pet_id()) == nullptr) {
		LOG_WARN << "[PlayerPet] active_pet_id 指向不存在的宝宝,已清空: player_id=" << GuidForLog(player)
				 << " active_pet_id=" << comp.active_pet_id();
		comp.set_active_pet_id(0);
	}
	if (const auto* pool = PetPool(); pool != nullptr) {
		SanitizeAllocations(comp, pool->id());
	}
	RecalculateAll(player, RecalcReason::kLoad);
}

void PetSystem::RecalculateAll(entt::entity player, RecalcReason reason) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto& comp = EnsureComp(player);
	if (comp.pets_size() == 0) {
		return;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		LOG_ERROR << "[PlayerPet] AttributePool 缺 owner_type=1 的宝宝池,宝宝属性无法重算";
		return;
	}
	for (auto& pet : *comp.mutable_pets()) {
		RecalculateOne(player, pet, *pool, reason);
	}
}

void PetSystem::BuildList(entt::entity player, PetListInfo& list) {
	list.Clear();
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	const auto& comp = EnsureComp(player);
	list.set_active_pet_id(comp.active_pet_id());
	list.set_max_pets(MaxPets());
	if (const auto* rule = RuleRow(); rule != nullptr) {
		list.set_rename_cost_gold(rule->rename_cost_gold());
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return;
	}
	for (const auto& pet : comp.pets()) {
		FillPetInfo(player, comp, pet, *pool, *list.add_pets());
	}
}

uint32_t PetSystem::Summon(entt::entity player, uint64_t petId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto& comp = EnsureComp(player);
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	if (comp.active_pet_id() == petId) {
		return kPetAlreadyActive;
	}
	const auto* row = PetRow(pet->pet_table_id());
	if (row == nullptr) {
		return kPetTableRowMissing;
	}
	if (OwnerLevel(player) < row->unlock_level()) {
		return kPetOwnerLevelNotEnough;
	}
	comp.set_active_pet_id(petId);
	LOG_INFO << "[PlayerPet] 出战: player_id=" << GuidForLog(player) << " pet_id=" << petId
			 << " pet_table_id=" << pet->pet_table_id();
	return kSuccess;
}

uint32_t PetSystem::Recall(entt::entity player) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto& comp = EnsureComp(player);
	if (comp.active_pet_id() == 0) {
		return kPetNotActive;
	}
	LOG_INFO << "[PlayerPet] 收回: player_id=" << GuidForLog(player)
			 << " pet_id=" << comp.active_pet_id();
	comp.set_active_pet_id(0);
	return kSuccess;
}

uint32_t PetSystem::Allocate(entt::entity player, uint64_t petId,
							 const std::map<uint32_t, uint32_t>& target) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return kInvalidTableData;
	}
	auto& comp = EnsureComp(player);
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	const auto* row = PetRow(pet->pet_table_id());
	if (row == nullptr) {
		return kPetTableRowMissing;
	}

	std::map<uint32_t, uint32_t> current;
	for (const auto* dim : PetDimensions(pool->id())) {
		current[dim->id()] = AllocatedIn(*pet, dim->id());
	}
	const auto total = attributerules::TotalPoints(ToRule(*pool), pet->level(), 0);
	const auto used = UsedPoints(*pet);
	const uint32_t remaining = total > used ? total - used : 0;

	uint32_t delta = 0;
	const auto err = attributerules::ValidateAllocation(ToRule(*pool), pet->level(), current, target,
														remaining, delta);
	if (err != AllocError::kOk) {
		return MapAllocError(err);
	}
	// 旧上限必须在改 allocated 之前取(见 RecalculateOne 的 previous 参数注释)
	const auto before = ComputeDerived(*pet, *row, pool->id());
	for (const auto& [dimensionId, want] : target) {
		(*pet->mutable_allocated())[dimensionId] = want;
	}
	RecalculateOne(player, *pet, *pool, RecalcReason::kAllocate, &before);
	LOG_INFO << "[PlayerPet] 加点: player_id=" << GuidForLog(player) << " pet_id=" << petId
			 << " delta=" << delta << " remaining=" << (remaining - delta);
	return kSuccess;
}

uint32_t PetSystem::Reset(entt::entity player, uint64_t petId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return kInvalidTableData;
	}
	auto& comp = EnsureComp(player);
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	const auto* row = PetRow(pet->pet_table_id());
	if (row == nullptr) {
		return kPetTableRowMissing;
	}
	if (UsedPoints(*pet) == 0) {
		return kPetNothingToChange;
	}
	// 先扣费再清点:扣费失败什么都不动(货币系统内部有冻结/封禁校验)
	const auto cost = ResetCostFor(*pet, *pool);
	if (cost > 0) {
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(cost))) {
			return kPetGoldNotEnough;
		}
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(cost));
			err != kSuccess) {
			return err;
		}
	}
	// 同 Allocate:洗点会把上限打下去,旧上限只能在清点之前取
	const auto before = ComputeDerived(*pet, *row, pool->id());
	pet->mutable_allocated()->clear();
	RecalculateOne(player, *pet, *pool, RecalcReason::kReset, &before);
	LOG_INFO << "[PlayerPet] 洗点: player_id=" << GuidForLog(player) << " pet_id=" << petId
			 << " cost_gold=" << cost;
	return kSuccess;
}

uint32_t PetSystem::AutoAllocate(entt::entity player, uint64_t petId,
								 std::map<uint32_t, uint32_t>& suggested) {
	suggested.clear();
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return kInvalidTableData;
	}
	auto& comp = EnsureComp(player);
	const auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}

	// 宝宝没有职业,只用 class_id=0 的通用方案
	const AttributeAutoPlanTable* plan = nullptr;
	for (const auto* row : AttributeAutoPlanTableManager::Instance().GetByClassId(0)) {
		if (row->pool_id() == pool->id()) {
			plan = row;
			break;
		}
	}
	if (plan == nullptr || plan->dimension_size() == 0) {
		return kPetNoAutoPlan;
	}

	for (const auto* dim : PetDimensions(pool->id())) {
		suggested[dim->id()] = AllocatedIn(*pet, dim->id());
	}
	std::vector<uint32_t> order;
	std::vector<uint32_t> weights;
	for (int i = 0; i < plan->dimension_size(); ++i) {
		const auto dimensionId = plan->dimension(i);
		if (suggested.find(dimensionId) == suggested.end()) {
			continue;  // 表配错池的维度直接忽略
		}
		order.push_back(dimensionId);
		weights.push_back(i < plan->weight_size() ? plan->weight(i) : 1);
	}
	const auto total = attributerules::TotalPoints(ToRule(*pool), pet->level(), 0);
	const auto used = UsedPoints(*pet);
	const uint32_t remaining = total > used ? total - used : 0;
	if (remaining == 0) {
		return kPetNothingToChange;
	}
	attributerules::DistributePoints(ToRule(*pool), order, weights, remaining, suggested);
	return kSuccess;
}

uint32_t PetSystem::Rename(entt::entity player, uint64_t petId, const std::string& name) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	if (!IsPetNameValid(name)) {
		return kPetNameInvalid;
	}
	auto& comp = EnsureComp(player);
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	// 名字没变就不收钱(客户端连点 / 重发同一个名字不该反复扣 rename_cost_gold)
	std::string currentName = pet->name();
	if (currentName.empty()) {
		if (const auto* petRow = PetRow(pet->pet_table_id()); petRow != nullptr) {
			currentName = petRow->name();
		}
	}
	if (currentName == name) {
		return kPetNothingToChange;
	}
	const auto* rule = RuleRow();
	const uint64_t cost = rule != nullptr ? rule->rename_cost_gold() : 0;
	if (cost > 0) {
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(cost))) {
			return kPetGoldNotEnough;
		}
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(cost));
			err != kSuccess) {
			return err;
		}
	}
	pet->set_name(name);
	return kSuccess;
}

uint32_t PetSystem::GrantPet(entt::entity player, uint32_t petTableId, uint64_t& petIdOut) {
	petIdOut = 0;
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto* row = PetRow(petTableId);
	if (row == nullptr) {
		return kPetTableRowMissing;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return kInvalidTableData;
	}
	auto& comp = EnsureComp(player);
	if (static_cast<uint32_t>(comp.pets_size()) >= MaxPets()) {
		return kPetSlotFull;
	}

	// 宠物沿用 item 身份域;scene 已退出 Snowflake 初始化,统一经号段取号。
	// 铸号成功后才新增宠物,段未就绪或已耗尽时不留下半只宝宝。
	Guid petId = kInvalidGuid;
	if (!tlsGuidSegmentRegistry.Get(GuidKind::kItem).TryNext(petId)) {
		LOG_ERROR << "[PlayerPet] 铸 pet_id 失败(item 号段未就绪或已耗尽): player_id="
				  << GuidForLog(player);
		return kPetIdGenerateFailed;
	}

	auto* pet = comp.add_pets();
	pet->set_pet_id(petId);
	pet->set_pet_table_id(petTableId);
	pet->set_level(petrules::EffectiveLevel(OwnerLevel(player), row->level_cap()));
	pet->set_created_at(TimeSystem::NowSecondsUTC());

	// 资质:每个维度在 [aptitude_min, aptitude_max] 闭区间独立随机,出生即定终身(洗资质是二期)。
	// 表缺列/配反时退回基准 10000,宝宝仍然可用,只是没有资质差异。
	const auto dimensions = PetDimensions(pool->id());
	for (size_t i = 0; i < dimensions.size(); ++i) {
		const auto index = static_cast<int>(i);
		uint32_t low = index < row->aptitude_min_size() ? row->aptitude_min(index) : petrules::kAptitudeBase;
		uint32_t high = index < row->aptitude_max_size() ? row->aptitude_max(index) : petrules::kAptitudeBase;
		// 单边缺配(只填了 min 或只填了 max)按另一边取定值:0 资质等于这一维完全不产出,
		// 绝不是策划留空想要的结果;两边都缺才退回基准
		if (low == 0 && high == 0) {
			low = high = petrules::kAptitudeBase;
		} else if (low == 0) {
			low = high;
		} else if (high == 0) {
			high = low;
		}
		if (low > high) {
			std::swap(low, high);
		}
		(*pet->mutable_aptitude())[dimensions[i]->id()] =
			low == high ? low : tlsRandom.Rand<uint32_t>(low, high);
	}

	// 出生满状态
	const auto derived = ComputeDerived(*pet, *row, pool->id());
	pet->set_health(derived.maxHealth);
	pet->set_mana(derived.maxMana);
	// 种类自带技能**不抄进实例**:抄了就是第二份真相,策划改表之后这只宝宝会永远
	// 停在发放那一刻的技能表。实例里的 skill_table_ids 只留给二期的技能书学习。

	petIdOut = petId;
	LOG_INFO << "[PlayerPet] 发放宝宝: player_id=" << GuidForLog(player) << " pet_id=" << petId
			 << " pet_table_id=" << petTableId << " level=" << pet->level();
	return kSuccess;
}

bool PetSystem::BuildBattleSnapshot(entt::entity player, ::BattlePetSnapshot& snapshot) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return false;
	}
	auto& comp = EnsureComp(player);
	if (comp.active_pet_id() == 0) {
		return false;
	}
	const auto* pet = FindPet(comp, comp.active_pet_id());
	if (pet == nullptr) {
		return false;
	}
	const auto* row = PetRow(pet->pet_table_id());
	const auto* pool = PetPool();
	if (row == nullptr || pool == nullptr) {
		LOG_WARN << "[PlayerPet] 出战宝宝缺表行,本局不带宝宝: player_id=" << GuidForLog(player)
				 << " pet_id=" << pet->pet_id() << " pet_table_id=" << pet->pet_table_id();
		return false;
	}
	// 死宝宝不参战(结算会把它回满,但当前这一局它躺着):带进去就是开局即倒的空单位
	if (pet->health() == 0) {
		return false;
	}

	const auto derived = ComputeDerived(*pet, *row, pool->id());
	snapshot.set_pet_id(pet->pet_id());
	snapshot.set_owner_player_id(GuidForLog(player));
	snapshot.set_pet_name(!pet->name().empty() ? pet->name() : row->name());
	snapshot.set_pet_table_id(pet->pet_table_id());
	snapshot.set_level(pet->level());
	snapshot.set_max_health(derived.maxHealth);
	snapshot.set_max_mana(derived.maxMana);
	snapshot.set_physical_attack(derived.physicalAttack);
	snapshot.set_magic_attack(derived.magicAttack);
	snapshot.set_defense(derived.defense);

	auto* attributes = snapshot.mutable_base_attributes();
	attributes->set_health(std::min(pet->health(), derived.maxHealth));
	attributes->set_mana(derived.maxMana > 0 ? std::min(pet->mana(), derived.maxMana) : pet->mana());
	// speed 直接用二级属性:回合引擎的出手序与逃跑判定只读 BaseAttributesComp.speed
	attributes->set_speed(derived.speed);
	for (const auto skillTableId : ResolveSkills(*pet, *row)) {
		snapshot.add_skill_table_ids(skillTableId);
	}
	return true;
}

void PetSystem::ApplyBattleSettlement(entt::entity player, const ::BattlePetSettlementData& settlement) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto& comp = EnsureComp(player);
	auto* pet = FindPet(comp, settlement.pet_id());
	if (pet == nullptr) {
		LOG_WARN << "[PlayerPet] 结算里的 pet_id 不在玩家名下,忽略: player_id=" << GuidForLog(player)
				 << " pet_id=" << settlement.pet_id();
		return;
	}
	const auto* row = PetRow(pet->pet_table_id());
	const auto* pool = PetPool();
	if (row == nullptr || pool == nullptr) {
		return;
	}
	const auto derived = ComputeDerived(*pet, *row, pool->id());
	pet->set_health(std::min(settlement.health(), derived.maxHealth));
	pet->set_mana(derived.maxMana > 0 ? std::min(settlement.mana(), derived.maxMana) : settlement.mana());
	// 阵亡回满:与玩家同口径(player_battle.cpp 的结算复活)。不回满的话 0 血宝宝
	// 会被 BuildBattleSnapshot 一直挡在门外,玩家再也带不出去,且没有任何提示。
	if (settlement.is_dead() || pet->health() == 0) {
		pet->set_health(derived.maxHealth);
		pet->set_mana(derived.maxMana);
		LOG_INFO << "[PlayerPet] 宝宝阵亡回满: player_id=" << GuidForLog(player)
				 << " pet_id=" << pet->pet_id();
	}
}

void PetSystem::PushList(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	PetListChangedS2C message;
	BuildList(player, *message.mutable_pets());
	SendMessageToClientViaGate(ScenePetClientPlayerNotifyPetListChangedMessageId, message, player);
}
