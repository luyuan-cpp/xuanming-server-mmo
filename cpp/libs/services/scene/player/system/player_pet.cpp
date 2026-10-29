#include "player_pet.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <algorithm>
#include <limits>
#include <optional>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/utils/random/random.h"
#include "engine/core/time/system/time.h"
#include "network/client_message_sender.h"
#include "thread_context/ecs_context.h"
#include "modules/id_segment/guid_segment_registry.h"

#include "battle/system/player_battle.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/attribute_allocation_rules.h"
#include "player/system/pet_rules.h"
#include "player/system/player_level_rules.h"

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


// 依赖主人等级的初始化、重算、召唤、加点、洗点和发放入口必须先取到 LevelComp,
// 再执行任何持久数据修补、扣费或铸号。列表与交易快照还原只读宠物已存等级,不加此依赖。
std::optional<uint32_t> OwnerLevel(entt::entity player) {
	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	if (levelComp == nullptr) {
		LOG_ERROR << "[PlayerPet] LevelComp missing (load invariant broken); owner-level operation skipped: entity="
				  << entt::to_integral(player) << " player_id=" << tlsEcs.GetPlayerId(player);
		return std::nullopt;
	}
	// 玩法读的是主人的生效等级:playerlevel::CappedLevel(新角色加载期间的 0 按 1,超上限的存储等级按上限),
	// 存储值本身不改。组件缺失不能走这条换算分支。
	return playerlevel::CappedLevel(levelComp->level());
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

// 该键是不是本版本宝宝池的已知维度(加点统一规则 2):表里有这一行且属于宝宝池。
bool IsPetPoolDimension(uint32_t dimensionId, uint32_t poolId) {
	const auto [dim, err] = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId);
	return dim != nullptr && dim->pool_id() == poolId;
}

// 宝宝在宝宝池上的存储加点汇总(加点统一规则 2,attribute_allocation_rules.h):
//   已知 = 本版本表里属于宝宝池的维度;未知 = 其余的键(新版本加的维度、角色维度、表里查不到的维度),
//   不生效、计入已用。
// PetInstance 本层的未知字段(新版本给实例加的字段)不在这里判、也不加 HasUnknown 守卫:那会把退役字段的残留
// 误判成永久冻结;与加点语义耦合的新字段按混版本持久字段保护的 R5 走评审(规则 8)。
attributerules::StoredPoolAllocation StoredAllocationOf(const PetInstance& pet, uint32_t poolId) {
	attributerules::StoredPoolAllocation stored;
	for (const auto& [dimensionId, points] : pet.allocated()) {
		if (IsPetPoolDimension(dimensionId, poolId)) {
			stored.AddKnown(points);
		} else {
			stored.AddForeign(points);
		}
	}
	return stored;
}

// 宝宝在宝宝池上的判定结果(加点统一规则 3 / 4),与角色侧 PoolAllocationState 同构。
// 列表、加点、洗点、自动加点与生效输入都从这里取,不再各抄一份"总量 − 已用"。
struct PetPoolState {
	uint32_t total = 0;
	attributerules::StoredPoolAllocation stored;
	bool effective = true;
	uint32_t remaining = 0;
};

// 总量按宝宝已存等级算(等级只升不降,见 RecalculateOne)。
PetPoolState EvaluatePetPool(const PetInstance& pet, const AttributePoolTable& pool) {
	PetPoolState state;
	const auto rule = ToRule(pool);
	state.total = attributerules::TotalPoints(rule, pet.level(), 0);
	state.stored = StoredAllocationOf(pet, pool.id());
	state.effective = attributerules::PoolAllocationEffective(rule, state.total, state.stored);
	state.remaining = attributerules::RemainingPoints(rule, state.total, state.stored);
	return state;
}

// 把表行 + 实例翻译成纯规则输入(纯规则不认识表也不认识 proto)。这是宝宝「生效加点」的唯一出处:
// 二级属性、列表 value、战斗快照、结算夹取都经它,按加点统一规则 3 过门 —— 池不生效时已知维度一律按 0,
// 未知键根本不进输入。存储的 allocated 原样不动。
std::vector<petrules::DimensionInput> BuildDimensionInputs(const PetInstance& pet, const AttributePoolTable& pool) {
	const bool effective = EvaluatePetPool(pet, pool).effective;
	std::vector<petrules::DimensionInput> inputs;
	for (const auto* dim : PetDimensions(pool.id())) {
		petrules::DimensionInput input;
		input.dimensionId = dim->id();
		input.coefficients.basePerLevel = dim->base_per_level();
		input.coefficients.maxHealth = dim->max_health();
		input.coefficients.maxMana = dim->max_mana();
		input.coefficients.physicalAttack = dim->physical_attack();
		input.coefficients.magicAttack = dim->magic_attack();
		input.coefficients.speed = dim->speed();
		input.coefficients.defense = dim->defense();
		input.allocated = effective ? AllocatedIn(pet, dim->id()) : 0;
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

// PlayerPetComp 是玩家必需组件(存档字段),唯一创建点是加载时的 PlayerDatabaseMessageFieldsUnmarshal。
// 所有入口都经这里取,只取不建:缺了说明加载链路没走完或组件被误删,记错误并由调用方拒绝 / 跳过。
// 以前这里 get_or_emplace,连战斗快照、结算回写、只读列表都会给实体补一个空的宝宝组件,
// 下一次存盘就把「没有宝宝」当真数据写回,而且零报错。
PlayerPetComp* FindComp(entt::entity player) {
	auto* comp = tlsEcs.actorRegistry.try_get<PlayerPetComp>(player);
	if (comp == nullptr) {
		LOG_ERROR << "[PlayerPet] PlayerPetComp missing (load path incomplete?): entity=" << entt::to_integral(player)
				  << " player_id=" << tlsEcs.GetPlayerId(player);
	}
	return comp;
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
		LOG_WARN << "[PlayerPet] Rejected: cross-zone freeze in progress player_id=" << tlsEcs.GetPlayerId(player);
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

// 洗点金币:列表下发的 reset_cost_gold 与 Reset 实际扣的是这同一个函数。
// 加点不生效时免费(加点统一规则 6,理由同角色侧 ResetCostFor:不生效不是玩家造成的,而那时已知维度本就按 0 计)。
uint64_t ResetCostFor(const PetInstance& pet, const AttributePoolTable& pool, const PetPoolState& state) {
	if (!state.effective) {
		return 0;
	}
	if (pool.reset_free_below_level() > 0 && pet.level() < pool.reset_free_below_level()) {
		return 0;
	}
	return pool.reset_cost_gold();
}

// 一只宝宝的二级属性:现算,不缓存(见头文件"没有第二份真相")。加点按 BuildDimensionInputs 过门后的生效值算,
// 所以从不生效状态洗点前后上限不变,HP/MP 也不变(加点统一规则 7)。
petrules::DerivedAttributes ComputeDerived(const PetInstance& pet, const PetTable& row, const AttributePoolTable& pool) {
	return petrules::ComputeDerived(BaseValuesOf(row), BuildDimensionInputs(pet, pool), pet.level());
}

// 加载期可观测性(只读,不写任何数据):加点不生效、或带着本版本解释不了的键时各打一条 WARN。
// 加载不再清理 / 返还宝宝的加点(以前超额整只清空、查不到的维度直接删,D3 / D4),这是线上唯一看得见的痕迹。
void WarnStoredAllocationOnLoad(entt::entity player, const PetInstance& pet, const AttributePoolTable& pool) {
	const auto state = EvaluatePetPool(pet, pool);
	if (!state.effective) {
		LOG_WARN << "[PlayerPet] metric=allocation_not_effective kind=pool player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << pet.pet_id() << " pool=" << pool.id() << " known_used=" << state.stored.knownUsed
				 << " known_largest=" << state.stored.knownLargest << " total=" << state.total
				 << " dimension_cap=" << pool.dimension_cap() << "; stored allocation carried as-is, pool counts as 0";
	}
	uint32_t unknownKeys = 0;
	for (const auto& entry : pet.allocated()) {
		if (!IsPetPoolDimension(entry.first, pool.id())) {
			++unknownKeys;
		}
	}
	if (unknownKeys > 0) {
		LOG_WARN << "[PlayerPet] metric=allocation_not_effective kind=unknown_dimension player_id="
				 << tlsEcs.GetPlayerId(player) << " pet_id=" << pet.pet_id() << " unknown_keys=" << unknownKeys
				 << " unknown_points=" << state.stored.foreignUsed
				 << "; unknown dimensions carried as-is, never effective, counted as spent";
	}
}

// 单只宝宝重算:同步等级 + 按新上限处理当前 HP/MP。不改写、不返还已分配点(加点统一规则 1):
// 超额的加点整池按 0 生效(ComputeDerived 过门),等级涨回来或洗点之后再按本版本解释。
// 返回是否发生了变化(仅用于日志)。
//
// previous:调用方在**改动 allocated 之前**拿到的旧二级属性。宝宝的二级属性是现算的
// (不像角色有 DerivedAttributesComp 存着上一份),加点 / 洗点改完 allocated 再进来算,
// "旧上限"就已经等于新上限了 —— 按比例保持会退化成恒等式:洗点把上限打下去时当前血只被夹,
// 再把点加回来血也不涨,一趟往返白掉一截血。所以这两条路径必须由调用方传旧值进来。
// kLoad / kLevelChanged 传 nullptr:那两条路径改的是 level,旧值在本函数内 set_level 之前算得到。
bool RecalculateOne(entt::entity player, uint32_t ownerLevel, PetInstance& pet, const AttributePoolTable& pool,
					PetSystem::RecalcReason reason,
					const petrules::DerivedAttributes* previous = nullptr) {
	const auto* row = PetRow(pet.pet_table_id());
	if (row == nullptr) {
		// 种类行被删/改表:保留实例(玩家资产不能悄悄没),召唤时拒绝,这里只告警
		LOG_WARN << "[PlayerPet] Species row missing;recalculation skipped: player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << pet.pet_id() << " pet_table_id=" << pet.pet_table_id();
		return false;
	}

	const auto oldLevel = pet.level();
	// 成长属于实例，换低级主人或主人降级不能删除已获得等级与加点。
	const auto newLevel = std::max(oldLevel, petrules::EffectiveLevel(ownerLevel, row->level_cap()));
	const auto oldDerived = previous != nullptr ? *previous : ComputeDerived(pet, *row, pool);
	pet.set_level(newLevel);

	// 配表缩点造成的超额只让这份加点不生效(总量按同步后的等级算),不再整只清空返还。
	if (reason == PetSystem::RecalcReason::kLoad) {
		WarnStoredAllocationOnLoad(player, pet, pool);
	}

	const auto newDerived = ComputeDerived(pet, *row, pool);
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
		// 加载 / 加点 / 洗点:按比例保持,一升一降往返不净得(极低血量的有界例外见 RescaleCurrent)
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

	// 总量 / 剩余 / 洗点费用出自同一个 EvaluatePetPool:不生效时剩余 0、洗点免费,与 Allocate / Reset 的实际判定一致。
	const auto state = EvaluatePetPool(pet, pool);
	info.set_total_points(state.total);
	info.set_remaining_points(state.remaining);
	info.set_reset_cost_gold(ResetCostFor(pet, pool, state));

	// value 复用这份已过门的输入(与二级属性、战斗快照同一个判定),不另写一份生效判断。
	const auto inputs = BuildDimensionInputs(pet, pool);
	info.set_growth(petrules::GrowthPermyriad(inputs));
	for (const auto* dim : PetDimensions(pool.id())) {
		auto* dimensionInfo = info.add_dimensions();
		dimensionInfo->set_dimension_id(dim->id());
		dimensionInfo->set_pool_id(dim->pool_id());
		dimensionInfo->set_name(dim->name());
		dimensionInfo->set_desc(dim->desc());
		// allocated 下发存储值(加点不生效时也照实显示);value 只算生效的那部分。
		dimensionInfo->set_allocated(AllocatedIn(pet, dim->id()));
		dimensionInfo->set_cap(pool.dimension_cap());
		dimensionInfo->set_sort(dim->sort());
		dimensionInfo->set_aptitude(AptitudeIn(pet, dim->id()));
		const auto input = std::find_if(inputs.begin(), inputs.end(), [dim](const petrules::DimensionInput& one) {
			return one.dimensionId == dim->id();
		});
		dimensionInfo->set_value(input != inputs.end() ? petrules::DimensionValue(*input, pet.level()) : 0);
	}

	auto* derivedInfo = info.mutable_derived();
	if (row != nullptr) {
		const auto derived = ComputeDerived(pet, *row, pool);
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
	// 存档组件由 Unmarshal 挂上;这里只做加载期的数据修补,不替加载链路补建(FindComp 已记错误)。
	// 已分配点原样携带,不按本版本表清理(加点统一规则 1,mixed-version-persist-field-protection.md §12.1 D3):
	// 本版本解释不了的键不生效、计入已用,由 BuildDimensionInputs / EvaluatePetPool 解释。
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return;
	}
	// 初始化会修补出战引用并同步等级,必须在任何修补之前确认等级已经载入。
	if (!OwnerLevel(player).has_value()) {
		return;
	}
	auto& comp = *petComp;
	// 出战指向一只已不存在的宝宝(异常存档 / 二期放生后的残留):就地纠正为未出战。
	// 与版本无关的坏档自愈(指向的实例根本不在这份存档里),不属于"按本版本配置改写"。
	if (comp.active_pet_id() != 0 && FindPet(comp, comp.active_pet_id()) == nullptr) {
		LOG_WARN << "[PlayerPet] active_pet_id points to nonexistent pet;cleared: player_id=" << tlsEcs.GetPlayerId(player)
				 << " active_pet_id=" << comp.active_pet_id();
		comp.set_active_pet_id(0);
	}
	RecalculateAll(player, RecalcReason::kLoad);
}

void PetSystem::RecalculateAll(entt::entity player, RecalcReason reason) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return;
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value()) {
		return;
	}
	auto& comp = *petComp;
	if (comp.pets_size() == 0) {
		return;
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		LOG_ERROR << "[PlayerPet] AttributePool lacks pet pool with owner_type=1;pet attributes cannot be recalculated";
		return;
	}
	for (auto& pet : *comp.mutable_pets()) {
		RecalculateOne(player, *ownerLevel, pet, *pool, reason);
	}
	// 主人等级低于出战宝宝时取消出战,只在等级真的变化(kLevelChanged)时做。加载(kLoad)不写 active_pet_id:
	// 主人的生效等级可能只是这一版本的解读(超上限的存储等级按上限读、混版本带来的等级差),按它改写出战状态会随
	// 存盘落库;而运行期 Summon 与 BuildBattleSnapshot 本来就按主人等级拦截,留着的出战指针带不进战斗。
	if (reason == RecalcReason::kLevelChanged) {
		if (const auto* active = FindPet(comp, comp.active_pet_id()); active != nullptr && *ownerLevel < active->level()) {
			comp.set_active_pet_id(0);
		}
	}
}

uint32_t PetSystem::BuildList(entt::entity player, PetListInfo& list) {
	list.Clear();
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	// 只读路径:不改变实体的组件集合。缺组件时列表保持清空并返回故障码(FindComp 已记错误),
	// 调用方据返回值回错误码,不能把空列表当成功发给客户端(那等于告诉玩家宝宝全没了)。
	const auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;
	}
	const auto& comp = *petComp;
	list.set_active_pet_id(comp.active_pet_id());
	list.set_max_pets(MaxPets());
	if (const auto* rule = RuleRow(); rule != nullptr) {
		list.set_rename_cost_gold(rule->rename_cost_gold());
	}
	const auto* pool = PetPool();
	if (pool == nullptr) {
		return kSuccess;  // 宝宝池缺配是配表问题(RecalculateAll 会记 ERROR),列表照旧只缺宝宝明细
	}
	for (const auto& pet : comp.pets()) {
		FillPetInfo(player, comp, pet, *pool, *list.add_pets());
	}
	return kSuccess;
}

uint32_t PetSystem::Summon(entt::entity player, uint64_t petId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value()) {
		return kServiceUnavailable;
	}
	auto& comp = *petComp;
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
	if (*ownerLevel < row->unlock_level() || *ownerLevel < pet->level()) {
		return kPetOwnerLevelNotEnough;
	}
	comp.set_active_pet_id(petId);
	LOG_INFO << "[PlayerPet] Deployed: player_id=" << tlsEcs.GetPlayerId(player) << " pet_id=" << petId
			 << " pet_table_id=" << pet->pet_table_id();
	return kSuccess;
}

uint32_t PetSystem::Recall(entt::entity player) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	auto& comp = *petComp;
	if (comp.active_pet_id() == 0) {
		return kPetNotActive;
	}
	LOG_INFO << "[PlayerPet] Recalled: player_id=" << tlsEcs.GetPlayerId(player)
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value()) {
		return kServiceUnavailable;
	}
	auto& comp = *petComp;
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	const auto* row = PetRow(pet->pet_table_id());
	if (row == nullptr) {
		return kPetTableRowMissing;
	}

	// current 只含宝宝池已知维度:目标里出现别的键(未知键、角色维度)一律 kDimensionNotInPool。
	std::map<uint32_t, uint32_t> current;
	for (const auto* dim : PetDimensions(pool->id())) {
		current[dim->id()] = AllocatedIn(*pet, dim->id());
	}
	// 剩余点按加点统一规则 4:未知键计入已用;加点不生效时剩余为 0,任何增量都回「点数不足」。
	const auto state = EvaluatePetPool(*pet, *pool);

	uint32_t delta = 0;
	const auto err = attributerules::ValidateAllocation(ToRule(*pool), pet->level(), current, target,
														state.remaining, delta);
	if (err != AllocError::kOk) {
		return MapAllocError(err);
	}
	// 旧上限必须在改 allocated 之前取(见 RecalculateOne 的 previous 参数注释)
	const auto before = ComputeDerived(*pet, *row, *pool);
	// 只写真正变化的键(同角色侧 Allocate):want == current 的键不落,存档里不会凭空多出 0 值新键。
	for (const auto& [dimensionId, want] : target) {
		if (const auto it = current.find(dimensionId); it != current.end() && it->second != want) {
			(*pet->mutable_allocated())[dimensionId] = want;
		}
	}
	RecalculateOne(player, *ownerLevel, *pet, *pool, RecalcReason::kAllocate, &before);
	LOG_INFO << "[PlayerPet] Points allocated: player_id=" << tlsEcs.GetPlayerId(player) << " pet_id=" << petId
			 << " delta=" << delta << " remaining=" << (state.remaining - delta);
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value()) {
		return kServiceUnavailable;
	}
	auto& comp = *petComp;
	auto* pet = FindPet(comp, petId);
	if (pet == nullptr) {
		return kPetNotFound;
	}
	const auto* row = PetRow(pet->pet_table_id());
	if (row == nullptr) {
		return kPetTableRowMissing;
	}
	// 洗点只管宝宝池已知维度(加点统一规则 6):只剩未知键时也是「无可洗」,不扣费 ——
	// 未知键不归宝宝池,洗点不碰它们,收了钱却什么都没清掉。
	const auto state = EvaluatePetPool(*pet, *pool);
	if (state.stored.knownUsed == 0) {
		return kPetNothingToChange;
	}
	// 先扣费再清点:扣费失败什么都不动(货币系统内部有冻结/封禁校验)。
	// 费用与列表下发的 reset_cost_gold 同一个函数:加点不生效时免费。
	const auto cost = ResetCostFor(*pet, *pool, state);
	if (cost > 0) {
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(cost))) {
			return kPetGoldNotEnough;
		}
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(cost));
			err != kSuccess) {
			return err;
		}
	}
	if (!state.effective) {
		LOG_WARN << "[PlayerPet] metric=allocation_reset_free_ineffective player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << petId << " pool=" << pool->id() << " known_used=" << state.stored.knownUsed
				 << " known_largest=" << state.stored.knownLargest << " total=" << state.total
				 << " dimension_cap=" << pool->dimension_cap() << "; ineffective allocation reset for free";
	}
	// 同 Allocate:洗点会把上限打下去,旧上限只能在清点之前取。旧上限是生效值:从不生效状态洗点,
	// 前后上限相同,HP/MP 不变(加点统一规则 7)。
	const auto before = ComputeDerived(*pet, *row, *pool);
	// 只清宝宝池已知维度,未知键原样保留。
	for (const auto* dim : PetDimensions(pool->id())) {
		pet->mutable_allocated()->erase(dim->id());
	}
	RecalculateOne(player, *ownerLevel, *pet, *pool, RecalcReason::kReset, &before);
	LOG_INFO << "[PlayerPet] Points reset: player_id=" << tlsEcs.GetPlayerId(player) << " pet_id=" << petId
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	auto& comp = *petComp;
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
	// 剩余点与 Allocate 同一个判定(加点统一规则 4):建议能原样提交通过校验。
	const auto remaining = EvaluatePetPool(*pet, *pool).remaining;
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	auto& comp = *petComp;
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value()) {
		return kServiceUnavailable;
	}
	auto& comp = *petComp;
	if (static_cast<uint32_t>(comp.pets_size()) >= MaxPets()) {
		return kPetSlotFull;
	}

	// 宠物沿用 item 身份域;scene 已退出 Snowflake 初始化,统一经号段取号。
	// 铸号成功后才新增宠物,段未就绪或已耗尽时不留下半只宝宝。
	Guid petId = kInvalidGuid;
	if (!tlsGuidSegmentRegistry.Get(GuidKind::kItem).TryNext(petId)) {
		LOG_ERROR << "[PlayerPet] Failed to allocate pet_id(item segment not ready or exhausted): player_id="
				  << tlsEcs.GetPlayerId(player);
		return kPetIdGenerateFailed;
	}

	auto* pet = comp.add_pets();
	pet->set_pet_id(petId);
	pet->set_pet_table_id(petTableId);
	pet->set_level(petrules::EffectiveLevel(*ownerLevel, row->level_cap()));
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
	const auto derived = ComputeDerived(*pet, *row, *pool);
	pet->set_health(derived.maxHealth);
	pet->set_mana(derived.maxMana);
	// 种类自带技能**不抄进实例**:抄了就是第二份真相,策划改表之后这只宝宝会永远
	// 停在发放那一刻的技能表。实例里的 skill_table_ids 只留给二期的技能书学习。

	petIdOut = petId;
	LOG_INFO << "[PlayerPet] Pet granted: player_id=" << tlsEcs.GetPlayerId(player) << " pet_id=" << petId
			 << " pet_table_id=" << petTableId << " level=" << pet->level();
	return kSuccess;
}

uint32_t PetSystem::RemovePetForTrade(entt::entity player, uint64_t petId, PetInstance& snapshotOut) {
	snapshotOut.Clear();
	// 刻意不走 CheckWritable:冻结 / 传送在途 / 战斗中是暂时条件,由资产 RPC 入口层判 RETRY
	// 并且不记账(见头文件)。这里只判结构性条件。
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (petId == 0) {
		return kInvalidParameter;
	}
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	auto& comp = *petComp;
	// 出战中先拒:先判它再找宝宝,这样"出战中"永远压过"找不到"的可能歧义。
	if (comp.active_pet_id() == petId) {
		return kPetAlreadyActive;
	}
	int index = -1;
	for (int i = 0; i < comp.pets_size(); ++i) {
		if (comp.pets(i).pet_id() == petId) {
			index = i;
			break;
		}
	}
	if (index < 0) {
		return kPetNotFound;
	}

	// 先整条拷出再删:删完就没有第二次机会取快照了,而快照是托管期间唯一的权威副本。
	snapshotOut = comp.pets(index);
	comp.mutable_pets()->DeleteSubrange(index, 1);
	LOG_INFO << "[PlayerPet] Pet removed for trade: player_id=" << tlsEcs.GetPlayerId(player) << " pet_id=" << petId
			 << " pet_table_id=" << snapshotOut.pet_table_id() << " level=" << snapshotOut.level()
			 << " left=" << comp.pets_size();
	return kSuccess;
}

uint32_t PetSystem::RestorePetFromSnapshot(entt::entity player, const PetInstance& snapshot) {
	// 同 RemovePetForTrade:暂时条件不在这一层判。
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (snapshot.pet_id() == 0 || snapshot.pet_table_id() == 0) {
		LOG_ERROR << "[PlayerPet] Restore rejected: snapshot fields missing, player_id=" << tlsEcs.GetPlayerId(player)
				  << " pet_id=" << snapshot.pet_id() << " pet_table_id=" << snapshot.pet_table_id();
		return kInvalidParameter;
	}
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return kServiceUnavailable;  // 缺必需组件 = 服务端故障,FindComp 已记错误
	}
	auto& comp = *petComp;
	// 撞号:同一 pet_id 已在名下。资产通道的 seq 账本负责幂等,所以走到这里的重复只可能是
	// 数据事故(号段回退 / 快照被复制过),必须拒绝而不是覆盖 —— 覆盖会静默吃掉一只宝宝。
	if (FindPet(comp, snapshot.pet_id()) != nullptr) {
		LOG_ERROR << "[PlayerPet] Restore rejected: ID collision, player_id=" << tlsEcs.GetPlayerId(player)
				  << " pet_id=" << snapshot.pet_id();
		return kInvalidParameter;
	}
	if (static_cast<uint32_t>(comp.pets_size()) >= MaxPets()) {
		// 槽位满不是终局:调用方应映射成 RETRY,让买家腾位后自动到账(见头文件)。
		return kPetSlotFull;
	}

	auto* pet = comp.add_pets();
	*pet = snapshot;

	// 还原不触发升级或重掷；后续重算也不会因主人等级较低而降低既有成长。
	// 召唤和战斗入口独立复核主人等级，不能借交易让低级主人使用高级宝宝。
	// 这里是已有实例移动，不是创建。配置调低cap不能把托管的合法资产改写成另一只。
	// 身份、配置存在、字节上限等不可信输入校验由AssetOp在任何副作用前完成。

	LOG_INFO << "[PlayerPet] Traded pet restored: player_id=" << tlsEcs.GetPlayerId(player)
			 << " pet_id=" << pet->pet_id() << " pet_table_id=" << pet->pet_table_id()
			 << " level=" << pet->level()
			 << " total=" << comp.pets_size();
	return kSuccess;
}

bool PetSystem::BuildBattleSnapshot(entt::entity player, ::BattlePetSnapshot& snapshot) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return false;
	}
	// 缺 PlayerPetComp 是加载故障(FindComp 已记 ERROR)。本函数的 false 分不清缺组件和没有出战宝宝，
	// 调用方须先确认组件存在；唯一调用方 PlayerBattleSystem::BuildBattleSnapshot 已在 all_of 中拒绝缺失。
	const auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return false;
	}
	const auto& comp = *petComp;
	if (comp.active_pet_id() == 0) {
		return false;
	}
	const auto* pet = FindPet(comp, comp.active_pet_id());
	if (pet == nullptr) {
		return false;
	}
	const auto ownerLevel = OwnerLevel(player);
	if (!ownerLevel.has_value() || *ownerLevel < pet->level()) return false;
	const auto* row = PetRow(pet->pet_table_id());
	const auto* pool = PetPool();
	if (row == nullptr || pool == nullptr) {
		LOG_WARN << "[PlayerPet] Deployed pet has no table row;pet excluded from this battle: player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << pet->pet_id() << " pet_table_id=" << pet->pet_table_id();
		return false;
	}
	// 死宝宝不参战(结算会把它回满,但当前这一局它躺着):带进去就是开局即倒的空单位
	if (pet->health() == 0) {
		return false;
	}

	// 快照里的数值与列表同一个来源(ComputeDerived 已按生效加点过门):不生效的加点、未知键都带不进战斗。
	const auto derived = ComputeDerived(*pet, *row, *pool);
	snapshot.set_pet_id(pet->pet_id());
	snapshot.set_owner_player_id(tlsEcs.GetPlayerId(player));
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
	auto* petComp = FindComp(player);
	if (petComp == nullptr) {
		return;
	}
	auto& comp = *petComp;
	auto* pet = FindPet(comp, settlement.pet_id());
	if (pet == nullptr) {
		LOG_WARN << "[PlayerPet] Settlement pet_id does not belong to player;ignored: player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << settlement.pet_id();
		return;
	}
	const auto* row = PetRow(pet->pet_table_id());
	const auto* pool = PetPool();
	if (row == nullptr || pool == nullptr) {
		return;
	}
	const auto derived = ComputeDerived(*pet, *row, *pool);
	pet->set_health(std::min(settlement.health(), derived.maxHealth));
	pet->set_mana(derived.maxMana > 0 ? std::min(settlement.mana(), derived.maxMana) : settlement.mana());
	// 阵亡回满:与玩家同口径(player_battle.cpp 的结算复活)。不回满的话 0 血宝宝
	// 会被 BuildBattleSnapshot 一直挡在门外,玩家再也带不出去,且没有任何提示。
	if (settlement.is_dead() || pet->health() == 0) {
		pet->set_health(derived.maxHealth);
		pet->set_mana(derived.maxMana);
		LOG_INFO << "[PlayerPet] Dead pet restored to full health: player_id=" << tlsEcs.GetPlayerId(player)
				 << " pet_id=" << pet->pet_id();
	}
}

void PetSystem::PushList(entt::entity player) {
	// 列表建不出来(实体无效 / 缺必需组件)就不推:客户端按整体覆盖处理列表,
	// 推一张空列表等于告诉玩家宝宝全没了。缺组件的错误 BuildList 已记过。
	PetListChangedS2C message;
	if (BuildList(player, *message.mutable_pets()) != kSuccess) {
		return;
	}
	SendMessageToClientViaGate(ScenePetClientPlayerNotifyPetListChangedMessageId, message, player);
}
