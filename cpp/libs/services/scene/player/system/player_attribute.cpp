#include "player_attribute.h"

#include <algorithm>
#include <cmath>
#include <limits>
#include <vector>

#include <muduo/base/Logging.h>

#include "engine/core/time/system/time.h"
#include "network/player_message_utils.h"
#include "thread_context/ecs_context.h"

#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "battle/system/player_battle.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/attribute_allocation_rules.h"

#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "table/code/attributeautoplan_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/proto/tip/attribute_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/event/player_event.pb.h"
#include "proto/scene/player_attribute.pb.h"

namespace {

using attributerules::AllocError;
using attributerules::PoolRule;

constexpr uint32_t kDefaultSchemeId = 1;
constexpr const char* kDefaultSchemeName = "方案一";

// 方案自动命名:方案一 ~ 方案九,再往后用数字
const char* const kSchemeOrdinals[] = {"一", "二", "三", "四", "五", "六", "七", "八", "九"};

std::string DefaultSchemeName(uint32_t ordinal) {
	if (ordinal >= 1 && ordinal <= 9) {
		return std::string("方案") + kSchemeOrdinals[ordinal - 1];
	}
	return "方案" + std::to_string(ordinal);
}

uint64_t GuidForLog(entt::entity player) {
	const auto* guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	return guid != nullptr ? *guid : 0;
}

uint32_t PlayerLevel(entt::entity player) {
	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	return (levelComp != nullptr && levelComp->level() > 0) ? levelComp->level() : 1;
}

uint32_t PlayerClassId(entt::entity player) {
	const auto* uint32Comp = tlsEcs.actorRegistry.try_get<PlayerUint32Comp>(player);
	return uint32Comp != nullptr ? uint32Comp->class_() : 0;
}

const ClassTable* ResolveClassRow(entt::entity player) {
	// class_id 打通前全职业同首行(与 player_database_loader.cpp 的初始属性口径一致)
	const auto classId = PlayerClassId(player);
	if (classId != 0) {
		if (const auto [row, err] = ClassTableManager::Instance().FindByIdSilent(classId); row != nullptr) {
			return row;
		}
	}
	const auto& rows = ClassTableManager::Instance().FindAll().data();
	return rows.size() > 0 ? &rows.Get(0) : nullptr;
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

const AttributeRuleTable* RuleRow() {
	const auto& rows = AttributeRuleTableManager::Instance().FindAll().data();
	return rows.size() > 0 ? &rows.Get(0) : nullptr;
}

uint32_t MaxSchemes() {
	const auto* rule = RuleRow();
	return (rule != nullptr && rule->max_schemes() > 0) ? rule->max_schemes() : 1;
}

uint32_t BonusPoints(const PlayerAttributeComp& comp, uint32_t poolId) {
	const auto it = comp.bonus_points().find(poolId);
	return it == comp.bonus_points().end() ? 0 : it->second;
}

uint32_t BonusValue(const PlayerAttributeComp& comp, uint32_t dimensionId) {
	const auto it = comp.bonus_values().find(dimensionId);
	return it == comp.bonus_values().end() ? 0 : it->second;
}

AttributeScheme* FindScheme(PlayerAttributeComp& comp, uint32_t schemeId) {
	for (auto& scheme : *comp.mutable_schemes()) {
		if (scheme.scheme_id() == schemeId) {
			return &scheme;
		}
	}
	return nullptr;
}

const AttributeScheme* ActiveScheme(const PlayerAttributeComp& comp) {
	for (const auto& scheme : comp.schemes()) {
		if (scheme.scheme_id() == comp.active_scheme_id()) {
			return &scheme;
		}
	}
	return nullptr;
}

AttributeScheme* ActiveSchemeMutable(PlayerAttributeComp& comp) {
	return FindScheme(comp, comp.active_scheme_id());
}

uint32_t AllocatedIn(const AttributeScheme& scheme, uint32_t dimensionId) {
	const auto it = scheme.allocated().find(dimensionId);
	return it == scheme.allocated().end() ? 0 : it->second;
}

// 当前方案里某池已用点数
uint32_t UsedPoints(const AttributeScheme& scheme, uint32_t poolId) {
	uint64_t used = 0;
	for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
		used += AllocatedIn(scheme, dim->id());
	}
	return used > std::numeric_limits<uint32_t>::max() ? std::numeric_limits<uint32_t>::max()
													  : static_cast<uint32_t>(used);
}

uint32_t TotalPoints(entt::entity player, const PlayerAttributeComp& comp, const AttributePoolTable& pool) {
	return attributerules::TotalPoints(ToRule(pool), PlayerLevel(player), BonusPoints(comp, pool.id()));
}

// 维度面板值 = 每级自然成长 × 等级 + 已分配 + 外部加成
uint64_t DimensionValue(entt::entity player, const PlayerAttributeComp& comp,
						const AttributeScheme* scheme, const AttributeDimensionTable& dim) {
	uint64_t value = static_cast<uint64_t>(dim.base_per_level()) * PlayerLevel(player);
	if (scheme != nullptr) {
		value += AllocatedIn(*scheme, dim.id());
	}
	value += BonusValue(comp, dim.id());
	return value;
}

uint32_t MapAllocError(AllocError err) {
	switch (err) {
	case AllocError::kOk: return kSuccess;
	case AllocError::kPoolLocked: return kAttributePoolLocked;
	case AllocError::kDimensionNotInPool: return kAttributeDimensionNotFound;
	case AllocError::kCannotDecrease: return kAttributePointsCannotDecrease;
	case AllocError::kCapExceeded: return kAttributeDimensionCapExceeded;
	case AllocError::kNotEnoughPoints: return kAttributePointsNotEnough;
	case AllocError::kNothingToChange: return kAttributeNothingToChange;
	}
	return kInvalidParameter;
}

// 写操作统一前置:实体有效 / 未跨 zone 冻结 / 不在战斗
uint32_t CheckWritable(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player)) {
		LOG_WARN << "[PlayerAttribute] 拒绝: 跨 zone 冻结中 player_id=" << GuidForLog(player);
		return kInvalidParameter;
	}
	if (PlayerBattleSystem::IsInBattle(player)) {
		return kAttributeInBattle;
	}
	return kSuccess;
}

uint64_t ResetCostFor(entt::entity player, const AttributePoolTable& pool) {
	if (pool.reset_free_below_level() > 0 && PlayerLevel(player) < pool.reset_free_below_level()) {
		return 0;
	}
	return pool.reset_cost_gold();
}

uint64_t CreateSchemeCostFor(const PlayerAttributeComp& comp) {
	const auto* rule = RuleRow();
	if (rule == nullptr) {
		return 0;
	}
	const auto freeCount = rule->free_scheme_count();
	return static_cast<uint32_t>(comp.schemes_size()) < freeCount ? 0 : rule->create_scheme_cost_gold();
}

// 方案名校验:非空、不超表定字符数(按 UTF-8 码点数)、不含控制字符、至少一个可见码点
// (全空格 / U+200B 零宽空格 / U+FEFF 之类在列表里是一片空白,玩家自己都分不清;与客户端
// IsNullOrWhiteSpace 校验对齐,评审 2026-09-04)。
bool IsInvisibleCodePoint(uint32_t cp) {
	return cp == 0x20 || cp == 0xA0 || (cp >= 0x2000 && cp <= 0x200F) || cp == 0x2028 || cp == 0x2029 ||
		   cp == 0x202F || cp == 0x205F || cp == 0x2060 || cp == 0x3000 || cp == 0xFEFF;
}

bool IsSchemeNameValid(const std::string& name) {
	if (name.empty()) {
		return false;
	}
	const auto* rule = RuleRow();
	const uint32_t maxLen = (rule != nullptr && rule->scheme_name_max_len() > 0) ? rule->scheme_name_max_len() : 12;
	uint32_t codePoints = 0;
	bool hasVisible = false;
	for (size_t i = 0; i < name.size();) {
		const auto c = static_cast<unsigned char>(name[i]);
		if (c < 0x20 || c == 0x7f) {
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

PlayerAttributeComp& EnsureComp(entt::entity player) {
	// 初始化路径允许 get_or_emplace(ecs-component-access-rules.md)
	auto& comp = tlsEcs.actorRegistry.get_or_emplace<PlayerAttributeComp>(player);
	if (comp.schemes_size() == 0 || comp.active_scheme_id() == 0) {
		if (comp.schemes_size() == 0) {
			auto* scheme = comp.add_schemes();
			scheme->set_scheme_id(kDefaultSchemeId);
			scheme->set_name(kDefaultSchemeName);
		}
		comp.set_active_scheme_id(comp.schemes(0).scheme_id());
	}
	if (comp.next_scheme_id() == 0) {
		uint32_t maxId = 0;
		for (const auto& scheme : comp.schemes()) {
			maxId = std::max(maxId, scheme.scheme_id());
		}
		comp.set_next_scheme_id(maxId + 1);
	}
	return comp;
}

// 清掉表里已不存在的维度(改表后老存档自愈,点数自动返还)
void SanitizeSchemes(PlayerAttributeComp& comp) {
	for (auto& scheme : *comp.mutable_schemes()) {
		std::vector<uint32_t> stale;
		for (const auto& [dimensionId, _] : scheme.allocated()) {
			if (!AttributeDimensionTableManager::Instance().Exists(dimensionId)) {
				stale.push_back(dimensionId);
			}
		}
		for (const auto dimensionId : stale) {
			scheme.mutable_allocated()->erase(dimensionId);
		}
	}
}

}  // namespace

void PlayerAttributeSystem::InitializeOnLoad(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto& comp = EnsureComp(player);
	SanitizeSchemes(comp);
	Recalculate(player);
}

namespace {

// 已分配 > 总量的收敛:等级下降(GM / 回档)或改表缩点后,某方案某池的已分配可能超过该池总量。
// 不收敛的话低等级号会一直带着高等级面板进战斗快照。整池清零返还(不做按比例裁剪:裁剪结果不可预期,
// 玩家更容易理解"点数退回来了")。
void ConvergeOverAllocation(entt::entity player, PlayerAttributeComp& comp) {
	const auto& pools = AttributePoolTableManager::Instance().FindAll().data();
	for (const auto& pool : pools) {
		const auto total = TotalPoints(player, comp, pool);
		for (auto& scheme : *comp.mutable_schemes()) {
			if (UsedPoints(scheme, pool.id()) <= total) {
				continue;
			}
			for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(pool.id())) {
				scheme.mutable_allocated()->erase(dim->id());
			}
			LOG_WARN << "[PlayerAttribute] 已分配超总量,整池清零返还: player_id=" << GuidForLog(player)
					 << " pool=" << pool.id() << " scheme=" << scheme.scheme_id() << " total=" << total;
		}
	}
}

// 当前值随上限变化:按比例保持,活着的至少留 1(往返切方案不能把人切死)
uint64_t RescaleCurrent(uint64_t current, uint64_t oldMax, uint64_t newMax) {
	if (current == 0 || newMax == 0) {
		return 0;
	}
	if (oldMax == 0 || oldMax == newMax) {
		return std::min(current, newMax);
	}
	const auto scaled = static_cast<uint64_t>(static_cast<double>(current) * static_cast<double>(newMax) /
											   static_cast<double>(oldMax));
	return std::clamp<uint64_t>(scaled, 1, newMax);
}

}  // namespace

void PlayerAttributeSystem::Recalculate(entt::entity player, RecalcReason reason) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	auto* baseAttrs = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);
	if (baseAttrs == nullptr) {
		return;
	}
	auto& comp = EnsureComp(player);
	if (reason == RecalcReason::kLoad || reason == RecalcReason::kLevelChanged) {
		ConvergeOverAllocation(player, comp);
	}
	const auto* scheme = ActiveScheme(comp);
	const auto* classRow = ResolveClassRow(player);

	double maxHealth = classRow != nullptr ? static_cast<double>(classRow->init_health()) : 0.0;
	double maxMana = classRow != nullptr ? static_cast<double>(classRow->init_mana()) : 0.0;
	double physicalAttack = 0.0;
	double magicAttack = 0.0;
	double speed = classRow != nullptr ? static_cast<double>(classRow->init_speed()) : 0.0;
	double defense = 0.0;

	const auto& dims = AttributeDimensionTableManager::Instance().FindAll().data();
	for (const auto& dim : dims) {
		const auto value = static_cast<double>(DimensionValue(player, comp, scheme, dim));
		if (value == 0.0) {
			continue;
		}
		maxHealth += dim.max_health() * value;
		maxMana += dim.max_mana() * value;
		physicalAttack += dim.physical_attack() * value;
		magicAttack += dim.magic_attack() * value;
		speed += dim.speed() * value;
		defense += dim.defense() * value;
	}

	auto toU64 = [](double v) -> uint64_t {
		if (v <= 0.0) return 0;
		return static_cast<uint64_t>(std::floor(v));
	};

	auto& derived = tlsEcs.actorRegistry.get_or_emplace<DerivedAttributesComp>(player);
	const uint64_t oldMaxHealth = derived.max_health();
	const uint64_t oldMaxMana = derived.max_mana();
	derived.set_max_health(std::max<uint64_t>(toU64(maxHealth), 1));
	derived.set_max_mana(toU64(maxMana));
	derived.set_physical_attack(toU64(physicalAttack));
	derived.set_magic_attack(toU64(magicAttack));
	derived.set_speed(toU64(speed));
	derived.set_defense(toU64(defense));

	// 速度直写基础属性:回合引擎出手序 / 逃跑判定只读 BaseAttributesComp.speed
	baseAttrs->set_speed(derived.speed());

	// 当前 HP/MP 跟随上限(见 RecalcReason 注释):
	//   升级:抬高按绝对增量补(降级只夹);
	//   加载/加点/切方案/洗点:按比例保持 —— 降后再升往返零净得失,堵住"切方案/洗点当治疗"。
	if (reason == RecalcReason::kLevelChanged) {
		if (baseAttrs->health() > 0 && derived.max_health() > oldMaxHealth && oldMaxHealth > 0) {
			baseAttrs->set_health(baseAttrs->health() + (derived.max_health() - oldMaxHealth));
		}
		if (derived.max_mana() > oldMaxMana && oldMaxMana > 0) {
			baseAttrs->set_mana(baseAttrs->mana() + (derived.max_mana() - oldMaxMana));
		}
		baseAttrs->set_health(std::min(baseAttrs->health(), derived.max_health()));
		if (derived.max_mana() > 0) {
			baseAttrs->set_mana(std::min(baseAttrs->mana(), derived.max_mana()));
		}
	} else {
		baseAttrs->set_health(RescaleCurrent(baseAttrs->health(), oldMaxHealth, derived.max_health()));
		baseAttrs->set_mana(RescaleCurrent(baseAttrs->mana(), oldMaxMana, derived.max_mana()));
	}

	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kHealth);
	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kEnergy);
}

void PlayerAttributeSystem::BuildPanel(entt::entity player, AttributePanelInfo& panel) {
	panel.Clear();
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	const auto& comp = EnsureComp(player);
	const auto* scheme = ActiveScheme(comp);
	const auto level = PlayerLevel(player);
	panel.set_level(level);
	panel.set_active_scheme_id(comp.active_scheme_id());
	panel.set_max_schemes(MaxSchemes());
	panel.set_create_scheme_cost_gold(CreateSchemeCostFor(comp));
	if (const auto* rule = RuleRow(); rule != nullptr && rule->switch_cooldown_seconds() > 0) {
		panel.set_switch_cooldown_until(comp.last_switch_time() + rule->switch_cooldown_seconds());
	}

	const auto& pools = AttributePoolTableManager::Instance().FindAll().data();
	for (const auto& pool : pools) {
		auto* info = panel.add_pools();
		info->set_pool_id(pool.id());
		info->set_name(pool.name());
		const auto total = TotalPoints(player, comp, pool);
		const auto used = scheme != nullptr ? UsedPoints(*scheme, pool.id()) : 0;
		info->set_total(total);
		info->set_remaining(total > used ? total - used : 0);
		info->set_dimension_cap(pool.dimension_cap());
		info->set_unlocked(attributerules::IsUnlocked(ToRule(pool), level));
		info->set_unlock_level(pool.unlock_level());
		info->set_reset_cost_gold(ResetCostFor(player, pool));
	}

	const auto& dims = AttributeDimensionTableManager::Instance().FindAll().data();
	for (const auto& dim : dims) {
		auto* info = panel.add_dimensions();
		info->set_dimension_id(dim.id());
		info->set_pool_id(dim.pool_id());
		info->set_name(dim.name());
		info->set_desc(dim.desc());
		info->set_allocated(scheme != nullptr ? AllocatedIn(*scheme, dim.id()) : 0);
		info->set_value(DimensionValue(player, comp, scheme, dim));
		if (const auto [pool, err] = AttributePoolTableManager::Instance().FindByIdSilent(dim.pool_id()); pool != nullptr) {
			info->set_cap(pool->dimension_cap());
		}
		info->set_sort(dim.sort());
	}

	for (const auto& s : comp.schemes()) {
		auto* info = panel.add_schemes();
		info->set_scheme_id(s.scheme_id());
		info->set_name(s.name());
	}

	auto* derivedInfo = panel.mutable_derived();
	if (const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player)) {
		derivedInfo->set_max_health(derived->max_health());
		derivedInfo->set_max_mana(derived->max_mana());
		derivedInfo->set_physical_attack(derived->physical_attack());
		derivedInfo->set_magic_attack(derived->magic_attack());
		derivedInfo->set_speed(derived->speed());
		derivedInfo->set_defense(derived->defense());
	}
	if (const auto* base = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player)) {
		derivedInfo->set_health(base->health());
		derivedInfo->set_mana(base->mana());
	}
}

uint32_t PlayerAttributeSystem::Allocate(entt::entity player, uint32_t poolId,
										 const std::map<uint32_t, uint32_t>& target) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto [pool, poolErr] = AttributePoolTableManager::Instance().FindByIdSilent(poolId);
	if (pool == nullptr) {
		return kAttributePoolNotFound;
	}
	auto& comp = EnsureComp(player);
	auto* scheme = ActiveSchemeMutable(comp);
	if (scheme == nullptr) {
		return kAttributeSchemeNotFound;
	}

	std::map<uint32_t, uint32_t> current;
	for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
		current[dim->id()] = AllocatedIn(*scheme, dim->id());
	}
	const auto total = TotalPoints(player, comp, *pool);
	const auto used = UsedPoints(*scheme, poolId);
	const uint32_t remaining = total > used ? total - used : 0;

	uint32_t delta = 0;
	const auto err = attributerules::ValidateAllocation(ToRule(*pool), PlayerLevel(player), current, target,
														 remaining, delta);
	if (err != AllocError::kOk) {
		return MapAllocError(err);
	}
	for (const auto& [dimensionId, want] : target) {
		(*scheme->mutable_allocated())[dimensionId] = want;
	}
	Recalculate(player, RecalcReason::kAllocate);
	LOG_INFO << "[PlayerAttribute] 加点: player_id=" << GuidForLog(player) << " pool=" << poolId
			 << " scheme=" << scheme->scheme_id() << " delta=" << delta
			 << " remaining=" << (remaining - delta);
	return kSuccess;
}

uint32_t PlayerAttributeSystem::Reset(entt::entity player, uint32_t poolId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto [pool, poolErr] = AttributePoolTableManager::Instance().FindByIdSilent(poolId);
	if (pool == nullptr) {
		return kAttributePoolNotFound;
	}
	auto& comp = EnsureComp(player);
	auto* scheme = ActiveSchemeMutable(comp);
	if (scheme == nullptr) {
		return kAttributeSchemeNotFound;
	}
	if (UsedPoints(*scheme, poolId) == 0) {
		return kAttributeNothingToChange;
	}
	// 先扣费再清点:扣费失败则什么都不动(货币系统内部有冻结/封禁校验)
	const auto cost = ResetCostFor(player, *pool);
	if (cost > 0) {
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(cost))) {
			return kAttributeGoldNotEnough;
		}
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(cost));
			err != kSuccess) {
			return err;
		}
	}
	for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
		scheme->mutable_allocated()->erase(dim->id());
	}
	Recalculate(player, RecalcReason::kReset);
	LOG_INFO << "[PlayerAttribute] 洗点: player_id=" << GuidForLog(player) << " pool=" << poolId
			 << " scheme=" << scheme->scheme_id() << " cost_gold=" << cost;
	return kSuccess;
}

uint32_t PlayerAttributeSystem::AutoAllocate(entt::entity player, uint32_t poolId,
											 std::map<uint32_t, uint32_t>& suggested) {
	suggested.clear();
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	const auto [pool, poolErr] = AttributePoolTableManager::Instance().FindByIdSilent(poolId);
	if (pool == nullptr) {
		return kAttributePoolNotFound;
	}
	const auto level = PlayerLevel(player);
	if (!attributerules::IsUnlocked(ToRule(*pool), level)) {
		return kAttributePoolLocked;
	}
	auto& comp = EnsureComp(player);
	const auto* scheme = ActiveScheme(comp);
	if (scheme == nullptr) {
		return kAttributeSchemeNotFound;
	}

	// 职业专属方案优先,缺则用 class_id=0 的通用兜底
	const AttributeAutoPlanTable* plan = nullptr;
	for (const auto classId : {PlayerClassId(player), 0u}) {
		for (const auto* row : AttributeAutoPlanTableManager::Instance().GetByClassId(classId)) {
			if (row->pool_id() == poolId) {
				plan = row;
				break;
			}
		}
		if (plan != nullptr) {
			break;
		}
	}
	if (plan == nullptr || plan->dimension_size() == 0) {
		return kAttributeNoAutoPlan;
	}

	for (const auto* dim : AttributeDimensionTableManager::Instance().GetByPoolId(poolId)) {
		suggested[dim->id()] = AllocatedIn(*scheme, dim->id());
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
	const auto total = TotalPoints(player, comp, *pool);
	const auto used = UsedPoints(*scheme, poolId);
	const uint32_t remaining = total > used ? total - used : 0;
	if (remaining == 0) {
		return kAttributeNothingToChange;
	}
	attributerules::DistributePoints(ToRule(*pool), order, weights, remaining, suggested);
	return kSuccess;
}

uint32_t PlayerAttributeSystem::CreateScheme(entt::entity player, const std::string& name, uint32_t& schemeId) {
	schemeId = 0;
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto& comp = EnsureComp(player);
	if (static_cast<uint32_t>(comp.schemes_size()) >= MaxSchemes()) {
		return kAttributeSchemeLimitReached;
	}
	std::string finalName = name.empty() ? DefaultSchemeName(static_cast<uint32_t>(comp.schemes_size()) + 1) : name;
	if (!IsSchemeNameValid(finalName)) {
		return kAttributeSchemeNameInvalid;
	}
	const auto cost = CreateSchemeCostFor(comp);
	if (cost > 0) {
		if (!CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(cost))) {
			return kAttributeGoldNotEnough;
		}
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(cost));
			err != kSuccess) {
			return err;
		}
	}
	auto* scheme = comp.add_schemes();
	schemeId = comp.next_scheme_id();
	scheme->set_scheme_id(schemeId);
	scheme->set_name(finalName);
	comp.set_next_scheme_id(schemeId + 1);
	LOG_INFO << "[PlayerAttribute] 开新方案: player_id=" << GuidForLog(player) << " scheme=" << schemeId
			 << " cost_gold=" << cost;
	return kSuccess;
}

uint32_t PlayerAttributeSystem::SwitchScheme(entt::entity player, uint32_t schemeId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto& comp = EnsureComp(player);
	if (FindScheme(comp, schemeId) == nullptr) {
		return kAttributeSchemeNotFound;
	}
	if (comp.active_scheme_id() == schemeId) {
		return kAttributeSchemeAlreadyActive;
	}
	const auto now = TimeSystem::NowSecondsUTC();
	if (const auto* rule = RuleRow(); rule != nullptr && rule->switch_cooldown_seconds() > 0 &&
									  comp.last_switch_time() + rule->switch_cooldown_seconds() > now) {
		return kAttributeSchemeSwitchCooldown;
	}
	comp.set_active_scheme_id(schemeId);
	comp.set_last_switch_time(now);
	Recalculate(player, RecalcReason::kSchemeSwitch);
	LOG_INFO << "[PlayerAttribute] 切换方案: player_id=" << GuidForLog(player) << " scheme=" << schemeId;
	return kSuccess;
}

uint32_t PlayerAttributeSystem::RenameScheme(entt::entity player, uint32_t schemeId, const std::string& name) {
	// 与其它写操作同一道门(跨 zone 冻结 / 战斗在途拒绝):改名虽不影响数值,
	// 但设计文档 §5 承诺"战斗在途拒绝一切写",不给协议层留特例
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	if (!IsSchemeNameValid(name)) {
		return kAttributeSchemeNameInvalid;
	}
	auto& comp = EnsureComp(player);
	auto* scheme = FindScheme(comp, schemeId);
	if (scheme == nullptr) {
		return kAttributeSchemeNotFound;
	}
	scheme->set_name(name);
	return kSuccess;
}

uint32_t PlayerAttributeSystem::GmSetLevel(entt::entity player, uint32_t level) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	if (level == 0 || level > kMaxLevel) {
		return kInvalidParameter;
	}
	auto& levelComp = tlsEcs.actorRegistry.get_or_emplace<LevelComp>(player);
	const auto oldLevel = levelComp.level();
	levelComp.set_level(level);
	LOG_INFO << "[PlayerAttribute] GM 设等级: player_id=" << GuidForLog(player) << " " << oldLevel << " -> " << level;

	// 走升级事件(其他系统按同一事件接经验/技能解锁),事件处理里会 Recalculate + PushPanel
	PlayerUpgradeEvent upgrade;
	upgrade.set_actor_entity(entt::to_integral(player));
	upgrade.set_new_level(level);
	tlsEcs.dispatcher.trigger(upgrade);
	return kSuccess;
}

void PlayerAttributeSystem::PushPanel(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	AttributePanelChangedS2C message;
	BuildPanel(player, *message.mutable_panel());
	SendMessageToClientViaGate(SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId, message, player);
}
