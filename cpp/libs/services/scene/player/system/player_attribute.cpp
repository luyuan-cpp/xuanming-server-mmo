#include "player_attribute.h"

#include <algorithm>
#include <cmath>
#include <limits>
#include <utility>
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
#include "player/system/equip_attribute_rules.h"
#include "player/system/player_equip.h"
#include "player/system/player_level_rules.h"

#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "table/code/attributeallocratio_table.h"
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

// 角色池 / 宝宝池分流:AttributePool.owner_type(0=角色,1=宝宝)。
// 两边共用同一套表与纯规则,但**面板与写入口互不可见** —— 角色面板不出宝宝池,
// 角色的 Allocate/Reset/AutoAllocate 拿到宝宝池一律按"池不存在"拒绝(宝宝走 PetSystem)。
constexpr uint32_t kPoolOwnerPlayer = 0;

bool IsPlayerPool(const AttributePoolTable& pool) { return pool.owner_type() == kPoolOwnerPlayer; }

// 已经拿着维度行时用这个;只有维度 id 的调用方(含别的系统)用公开的
// PlayerAttributeSystem::IsPlayerDimension(id),它查到行之后转到这里 —— 判定只有这一份。
// 名字刻意与那个公开成员不同:成员函数体内的非限定名先找到类成员,同名的话这里会被遮住。
bool IsPlayerDimensionRow(const AttributeDimensionTable& dim) {
	const auto [pool, err] = AttributePoolTableManager::Instance().FindByIdSilent(dim.pool_id());
	return pool != nullptr && IsPlayerPool(*pool);
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

// 装备加成在 equiprules 里是饱和到 uint64 上限的整数;这里再与别的量相加也保持饱和,不回绕
// (回绕会把一个巨大的加成变成接近 0 的数且零报错)。
uint64_t SaturatingAdd(uint64_t a, uint64_t b) {
	return b > std::numeric_limits<uint64_t>::max() - a ? std::numeric_limits<uint64_t>::max() : a + b;
}

uint32_t SaturateToU32(uint64_t value) {
	return value > std::numeric_limits<uint32_t>::max() ? std::numeric_limits<uint32_t>::max()
														: static_cast<uint32_t>(value);
}

// 维度的外部加成点数 = 落库的 bonus_values(丹药等,尚无写入方)+ 装备的一级属性点(单项 + 所有属性)。
// 装备那部分每次从装备栏现算、不落库(equipment-attributes.md §5 不变量 3),所以不写回 bonus_values。
// Recalculate 与 BuildPanel 共用这一个函数:面板显示的点数与实际参与换算的点数不会分叉。
// 只对角色池维度调用(调用点都已先过 IsPlayerDimensionRow):equiprules 不认识池,「所有属性」不该加到宝宝维度上。
uint64_t ExternalBonusPoints(const PlayerAttributeComp& comp, const equiprules::EquipBonus& equipBonus,
							 uint32_t dimensionId) {
	return SaturatingAdd(BonusValue(comp, dimensionId), equiprules::PrimaryPointsFor(equipBonus, dimensionId));
}

// 维度面板值 = 每级自然成长 × 等级 + 已分配 + 外部加成(面板显示的是点数,与换算口径无关)。
// externalBonus 由调用方用 ExternalBonusPoints 算好传入,同一个数也作为 AttributeDimensionInfo.bonus 下发。
uint64_t DimensionValue(entt::entity player, const AttributeScheme* scheme, const AttributeDimensionTable& dim,
						uint64_t externalBonus) {
	uint64_t value = static_cast<uint64_t>(dim.base_per_level()) * PlayerLevel(player);
	if (scheme != nullptr) {
		value += AllocatedIn(*scheme, dim.id());
	}
	return SaturatingAdd(value, externalBonus);
}

// 六项二级属性的临时累加器(与 DerivedAttributesComp 同六项)
struct DerivedAccumulator {
	double maxHealth = 0.0;
	double maxMana = 0.0;
	double physicalAttack = 0.0;
	double magicAttack = 0.0;
	double speed = 0.0;
	double defense = 0.0;

	void AddLinear(const AttributeDimensionTable& dim, double points) {
		maxHealth += dim.max_health() * points;
		maxMana += dim.max_mana() * points;
		physicalAttack += dim.physical_attack() * points;
		magicAttack += dim.magic_attack() * points;
		speed += dim.speed() * points;
		defense += dim.defense() * points;
	}

	// 装备的二级属性平加(EquipAttribute.effect = 3 的六项,以及 effect = 4「伤害」摊到物伤 / 法伤的那两份)。
	// 必须按名字逐项加:equiprules::DerivedStat 的编号跟的是配表(kSpeed = 5、kDefense = 6),与 proto
	// DerivedAttributesComp 的字段号(defense = 5、speed = 6)正好相反,按号对拷会把速度加到防御上且零报错。
	// 加的是整数:累加器里原有的小数部分不受影响,向下取整后恰好多出这些点。
	void AddEquipFlat(const equiprules::EquipBonus& bonus) {
		using equiprules::DerivedFlatOf;
		using equiprules::DerivedStat;
		maxHealth += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kMaxHealth));
		maxMana += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kMaxMana));
		physicalAttack += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kPhysicalAttack));
		magicAttack += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kMagicAttack));
		speed += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kSpeed));
		defense += static_cast<double>(DerivedFlatOf(bonus, DerivedStat::kDefense));
	}
};

// double -> uint64:向下取整,负数与 0 归 0,越过 uint64 上限的夹到上限。
// 上界那一刀是装备接入后才需要的:装备加成会饱和到 UINT64_MAX,转成 double 正好是 2^64,
// 而越界的 double 再转回整数是未定义行为(正常数值远到不了这里)。
constexpr double kUint64Ceiling = 18446744073709551616.0;  // 2^64

uint64_t FloorToU64(double value) {
	if (value <= 0.0) {
		return 0;
	}
	if (value >= kUint64Ceiling) {
		return std::numeric_limits<uint64_t>::max();
	}
	return static_cast<uint64_t>(std::floor(value));
}

// ---- 战斗类属性(必杀 / 连击 / 抗性 … 15 项,equipment-attributes.md §3.3 / §4.4) ----
//
// equiprules::CombatStat 的数值 == proto CombatAttributes 的字段号 == EquipAttribute.effect_param。
// 下面两个函数都按名字逐字段读写,名字对了就不会写错格;但面板的 combat_id、战斗引擎和配表都按「号」
// 认这 15 项,号一旦错位,「抗冰冻」会悄悄变成「抗昏睡」且零报错。所以在唯一写 DerivedAttributesComp.combat
// 的这个文件里把对应关系钉死(纯规则单测 equip_attribute_rules_test.cpp 有同一组断言,但它不编进 scene.lib)。
constexpr bool CombatStatIsField(equiprules::CombatStat stat, int fieldNumber) {
	return static_cast<int>(equiprules::ToRaw(stat)) == fieldNumber;
}
static_assert(CombatStatIsField(equiprules::CombatStat::kPhysicalCritRate, CombatAttributes::kPhysicalCritRateFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kMagicCritRate, CombatAttributes::kMagicCritRateFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kComboRate, CombatAttributes::kComboRateFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kCounterRate, CombatAttributes::kCounterRateFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kReflectRate, CombatAttributes::kReflectRateFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kSkillLevelBonus, CombatAttributes::kSkillLevelBonusFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kIgnoreAilmentResist,
								CombatAttributes::kIgnoreAilmentResistFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistPoison, CombatAttributes::kResistPoisonFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistFreeze, CombatAttributes::kResistFreezeFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistSleep, CombatAttributes::kResistSleepFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistForget, CombatAttributes::kResistForgetFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistConfusion, CombatAttributes::kResistConfusionFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kResistAllAilment,
								CombatAttributes::kResistAllAilmentFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kMagicResist, CombatAttributes::kMagicResistFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");
static_assert(CombatStatIsField(equiprules::CombatStat::kPhysicalResist, CombatAttributes::kPhysicalResistFieldNumber),
			  "CombatStat must equal the CombatAttributes field number");

// 把装备的战斗类加成整块写进 out:先清空再逐字段写。战斗类属性只有装备一个来源,每次重算整块覆盖、
// 不做增量维护 —— 卸下最后一件装备后这里写出的就是全 0。
void WriteCombatAttributes(const equiprules::EquipBonus& bonus, CombatAttributes& out) {
	using equiprules::CombatOf;
	using equiprules::CombatStat;
	out.Clear();
	out.set_physical_crit_rate(CombatOf(bonus, CombatStat::kPhysicalCritRate));
	out.set_magic_crit_rate(CombatOf(bonus, CombatStat::kMagicCritRate));
	out.set_combo_rate(CombatOf(bonus, CombatStat::kComboRate));
	out.set_counter_rate(CombatOf(bonus, CombatStat::kCounterRate));
	out.set_reflect_rate(CombatOf(bonus, CombatStat::kReflectRate));
	out.set_skill_level_bonus(CombatOf(bonus, CombatStat::kSkillLevelBonus));
	out.set_ignore_ailment_resist(CombatOf(bonus, CombatStat::kIgnoreAilmentResist));
	out.set_resist_poison(CombatOf(bonus, CombatStat::kResistPoison));
	out.set_resist_freeze(CombatOf(bonus, CombatStat::kResistFreeze));
	out.set_resist_sleep(CombatOf(bonus, CombatStat::kResistSleep));
	out.set_resist_forget(CombatOf(bonus, CombatStat::kResistForget));
	out.set_resist_confusion(CombatOf(bonus, CombatStat::kResistConfusion));
	out.set_resist_all_ailment(CombatOf(bonus, CombatStat::kResistAllAilment));
	out.set_magic_resist(CombatOf(bonus, CombatStat::kMagicResist));
	out.set_physical_resist(CombatOf(bonus, CombatStat::kPhysicalResist));
}

// 按 CombatStat 读 combat 里的一项(面板用);kNone / kCount 返回 0。
uint64_t CombatBonusOf(const CombatAttributes& combat, equiprules::CombatStat stat) {
	using equiprules::CombatStat;
	switch (stat) {
	case CombatStat::kPhysicalCritRate: return combat.physical_crit_rate();
	case CombatStat::kMagicCritRate: return combat.magic_crit_rate();
	case CombatStat::kComboRate: return combat.combo_rate();
	case CombatStat::kCounterRate: return combat.counter_rate();
	case CombatStat::kReflectRate: return combat.reflect_rate();
	case CombatStat::kSkillLevelBonus: return combat.skill_level_bonus();
	case CombatStat::kIgnoreAilmentResist: return combat.ignore_ailment_resist();
	case CombatStat::kResistPoison: return combat.resist_poison();
	case CombatStat::kResistFreeze: return combat.resist_freeze();
	case CombatStat::kResistSleep: return combat.resist_sleep();
	case CombatStat::kResistForget: return combat.resist_forget();
	case CombatStat::kResistConfusion: return combat.resist_confusion();
	case CombatStat::kResistAllAilment: return combat.resist_all_ailment();
	case CombatStat::kMagicResist: return combat.magic_resist();
	case CombatStat::kPhysicalResist: return combat.physical_resist();
	case CombatStat::kNone:
	case CombatStat::kCount:
		break;
	}
	return 0;
}

// 战斗类属性里「角色自带」的那部分:必杀率叠在基础暴击率上,抗物理 / 抗法术叠在基础抗性上
// (与回合引擎的取值口径一致,equipment-attributes.md §4.5);其余 11 项没有角色基础值。
uint64_t CombatBaseValue(const BaseAttributesComp* baseAttrs, equiprules::CombatStat stat) {
	using equiprules::CombatStat;
	if (baseAttrs == nullptr) {
		return 0;
	}
	switch (stat) {
	case CombatStat::kPhysicalCritRate:
	case CombatStat::kMagicCritRate:
		return baseAttrs->critchance();
	case CombatStat::kMagicResist:
	case CombatStat::kPhysicalResist:
		return baseAttrs->resistance();
	default:
		return 0;
	}
}

// 百分比类战斗属性的面板显示上限(整数百分点)。只管显示:DerivedAttributesComp.combat 里存的是未夹取的
// 加成原值,概率怎么封顶由战斗引擎自己决定。
constexpr uint64_t kPercentDisplayCap = 100;

// 面板「战斗属性」区:15 项逐项下发终值,按 sort 升序(sort 相同按 combat_id)。
// 名称 / 是否百分比 / 排序来自 EquipAttribute 表(PlayerEquipSystem::DescribeCombatStat),客户端零配表。
// 加成部分读 DerivedAttributesComp.combat —— 与六项二级属性同一个来源,也正是开战时拷进战斗快照的那一份,
// 面板与实战不会各算各的。表里没有显示行的项不下发(没有名字,客户端无从显示);表完整时恒为 15 项。
// 缺行在这里是静默的(每次开面板都会走到),由启动表校验 PlayerEquipSystem::ValidateTables 报 ERROR。
void FillCombatPanel(entt::entity player, AttributePanelInfo& panel) {
	struct Line {
		uint32_t combatId{0};
		PlayerEquipSystem::CombatStatDisplay display;
		uint64_t value{0};
	};

	const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
	const auto* baseAttrs = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(player);

	std::vector<Line> lines;
	lines.reserve(static_cast<std::size_t>(equiprules::CombatStat::kCount));
	for (uint32_t raw = equiprules::ToRaw(equiprules::CombatStat::kNone) + 1;
		 raw < equiprules::ToRaw(equiprules::CombatStat::kCount); ++raw) {
		const auto stat = equiprules::ToCombatStat(raw);
		Line line;
		line.combatId = raw;
		line.display = PlayerEquipSystem::DescribeCombatStat(stat);
		if (line.display.name.empty()) {
			continue;
		}
		const uint64_t bonus = derived != nullptr ? CombatBonusOf(derived->combat(), stat) : 0;
		line.value = SaturatingAdd(CombatBaseValue(baseAttrs, stat), bonus);
		if (line.display.percent && line.value > kPercentDisplayCap) {
			line.value = kPercentDisplayCap;
		}
		lines.push_back(std::move(line));
	}
	std::sort(lines.begin(), lines.end(), [](const Line& lhs, const Line& rhs) {
		return lhs.display.sort != rhs.display.sort ? lhs.display.sort < rhs.display.sort
													 : lhs.combatId < rhs.combatId;
	});
	for (const auto& line : lines) {
		auto* info = panel.add_combat();
		info->set_combat_id(line.combatId);
		info->set_name(line.display.name);
		info->set_value(line.value);
		info->set_percent(line.display.percent);
		info->set_sort(line.display.sort);
	}
}

DerivedAccumulator ClassInitialValues(const ClassTable* classRow) {
	DerivedAccumulator acc;
	if (classRow != nullptr) {
		acc.maxHealth = static_cast<double>(classRow->init_health());
		acc.maxMana = static_cast<double>(classRow->init_mana());
		acc.speed = static_cast<double>(classRow->init_speed());
	}
	return acc;
}

// "85 级标准基础属性" = 职业初值 + 每个角色维度的自然成长 × 等级上限(不含加点、不含装备)。
// 加点公式的分母基准。按表现算而不是单独配一套数:改职业初值或自然成长时它自动跟着变,
// 不会出现"标准值还是旧的、实际成长已经改了"的第二份真相(2026-09-13 用户选定)。
DerivedAccumulator StandardBaseAtLevelCap(const ClassTable* classRow) {
	auto acc = ClassInitialValues(classRow);
	const auto& dims = AttributeDimensionTableManager::Instance().FindAll().data();
	for (const auto& dim : dims) {
		if (!IsPlayerDimensionRow(dim) || dim.base_per_level() == 0) {
			continue;
		}
		acc.AddLinear(dim, static_cast<double>(dim.base_per_level()) * playerlevel::kMaxLevel);
	}
	return acc;
}

// 加点公式常量:bonus 取 AttributeRule 表;scale(满投点数)与除数按维度所属池 + 等级上限现算,
// 不进表(那是池表与 kMaxLevel 的第二份真相,见 attributerules 注释)。池若有单项上限,满投按上限算,
// 不会被属性点池的 425 尺度压小。
attributerules::AllocFormulaRule FormulaRuleFor(const AttributePoolTable& pool) {
	const auto* rule = RuleRow();
	const double bonus = rule != nullptr ? rule->alloc_efficiency_bonus() : attributerules::kDefaultEfficiencyBonus;
	return attributerules::MakeFormulaRule(
		bonus, attributerules::FullInvestmentPoints(ToRule(pool), playerlevel::kMaxLevel));
}

// 该维度对该职业的加点收益比例行:先找玩家职业专属行,没有退到 class_id=0 兜底;都没有返回 nullptr
// (= 这个维度不走百分比公式,继续按每点固定加值;2026-09-14 起角色只剩属性点池,四个维度都有比例行,这里只是兜底)
const AttributeAllocRatioTable* FindAllocRatio(uint32_t dimensionId, uint32_t classId) {
	const AttributeAllocRatioTable* fallback = nullptr;
	const auto& rows = AttributeAllocRatioTableManager::Instance().FindAll().data();
	for (const auto& row : rows) {
		if (row.dimension_id() != dimensionId) {
			continue;
		}
		if (classId != 0 && row.class_id() == classId) {
			return &row;
		}
		if (row.class_id() == 0) {
			fallback = &row;
		}
	}
	return fallback;
}

// 玩家分配的点按百分比公式换算(2026-09-13):增量 = 标准基础属性 × 比例 × E(n) ÷ d,逐项累加
void AddAllocatedByFormula(DerivedAccumulator& acc, const DerivedAccumulator& standard,
						   const AttributeAllocRatioTable& ratio, uint32_t allocated,
						   const attributerules::AllocFormulaRule& rule) {
	using attributerules::AllocatedIncrement;
	acc.maxHealth += AllocatedIncrement(standard.maxHealth, ratio.max_health(), allocated, rule);
	acc.maxMana += AllocatedIncrement(standard.maxMana, ratio.max_mana(), allocated, rule);
	acc.physicalAttack += AllocatedIncrement(standard.physicalAttack, ratio.physical_attack(), allocated, rule);
	acc.magicAttack += AllocatedIncrement(standard.magicAttack, ratio.magic_attack(), allocated, rule);
	acc.speed += AllocatedIncrement(standard.speed, ratio.speed(), allocated, rule);
	acc.defense += AllocatedIncrement(standard.defense, ratio.defense(), allocated, rule);
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

bool PlayerAttributeSystem::IsPlayerDimension(uint32_t dimensionId) {
	const auto [dim, err] = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId);
	return dim != nullptr && IsPlayerDimensionRow(*dim);
}

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
		if (!IsPlayerPool(pool)) {
			continue;  // 宝宝池的收敛由 PetSystem 按宝宝等级做
		}
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
	const auto classId = PlayerClassId(player);
	const auto level = PlayerLevel(player);

	// 装备加成每次重算都从装备栏现算(纯读,不写任何组件)。**不写** bonus_values:那是落库字段,写进去就成了
	// 「装备栏」之外的第二份真相,脱下时还得记得减回去(equipment-attributes.md §4.4、§5 不变量 3)。
	equiprules::EquipBonus equipBonus;
	PlayerEquipSystem::CollectBonus(player, equipBonus);

	// 二级属性 = 职业初值
	//          + Σ (自然成长 × 等级 + 外部加成) × 每点固定系数            ← 所有维度,老口径;外部加成 = bonus_values + 装备的一级属性点
	//          + Σ 玩家分配点 → 有比例表的维度走百分比公式(2026-09-13),没有的仍按每点固定系数
	//          + 装备的二级属性平加(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御)
	// 自然成长与装备加成不走公式:策划公式只定义"加点收益",白送的点与装备值保持原样(用户 2026-09-13 选定)。
	// 所以有两处**不能**混进装备:standard(加点收益的分母基准,混进去后同样的加点收益会随装备浮动),
	// 以及下面传给公式的 allocated(装备点不是玩家分配的点,不享受集中投资加成)。
	auto acc = ClassInitialValues(classRow);
	const auto standard = StandardBaseAtLevelCap(classRow);

	const auto& dims = AttributeDimensionTableManager::Instance().FindAll().data();
	for (const auto& dim : dims) {
		if (!IsPlayerDimensionRow(dim)) {
			continue;  // 宝宝维度不进角色二级属性
		}
		const double natural = static_cast<double>(dim.base_per_level()) * level +
							   static_cast<double>(ExternalBonusPoints(comp, equipBonus, dim.id()));
		if (natural > 0.0) {
			acc.AddLinear(dim, natural);
		}
		const uint32_t allocated = scheme != nullptr ? AllocatedIn(*scheme, dim.id()) : 0;
		if (allocated == 0) {
			continue;
		}
		const auto* ratio = FindAllocRatio(dim.id(), classId);
		const auto [pool, poolErr] = AttributePoolTableManager::Instance().FindByIdSilent(dim.pool_id());
		if (ratio != nullptr && pool != nullptr) {
			AddAllocatedByFormula(acc, standard, *ratio, allocated, FormulaRuleFor(*pool));
		} else {
			acc.AddLinear(dim, static_cast<double>(allocated));
		}
	}

	// 装备的二级属性平加:在维度循环之后、向下取整之前进累加器。
	acc.AddEquipFlat(equipBonus);

	auto& derived = tlsEcs.actorRegistry.get_or_emplace<DerivedAttributesComp>(player);
	const uint64_t oldMaxHealth = derived.max_health();
	const uint64_t oldMaxMana = derived.max_mana();
	derived.set_max_health(std::max<uint64_t>(FloorToU64(acc.maxHealth), 1));
	derived.set_max_mana(FloorToU64(acc.maxMana));
	derived.set_physical_attack(FloorToU64(acc.physicalAttack));
	derived.set_magic_attack(FloorToU64(acc.magicAttack));
	derived.set_speed(FloorToU64(acc.speed));
	derived.set_defense(FloorToU64(acc.defense));
	// 战斗类属性(必杀 / 连击 / 抗性 …):装备加成整块覆盖,开战时由 BuildBattleSnapshot 原样拷进快照。
	WriteCombatAttributes(equipBonus, *derived.mutable_combat());

	// 速度直写基础属性:回合引擎出手序 / 逃跑判定只读 BaseAttributesComp.speed。
	// 这里面含装备的速度 —— 它是重算结果的镜像,每次加载都在这里被覆盖、从不作为输入,落库的那份只是缓存。
	baseAttrs->set_speed(derived.speed());

	// 护甲按职业表直写(2026-09-14 防御单位 ×12 时加):护甲原本只在新号初始化时写一次、随存档落库,改表后已建的角色
	// (含本地测试号)不会跟着变;这里每次重算都以 Class.init_armor 为准,与上面 speed 直写同口径。
	// 全仓没有 buff / 装备 / GM 在运行时改护甲,这里直写不会冲掉任何东西;以后装备要加护甲,必须在这里累加,
	// 不能直接改 BaseAttributesComp.armor。装备的「防御」不是护甲:它已经加在上面的 derived.defense 里
	// (伤害公式里护甲与防御本来就相加),这里不碰 armor。
	if (classRow != nullptr) {
		baseAttrs->set_armor(classRow->init_armor());
	}

	// 当前 HP/MP 跟随上限(见 RecalcReason 注释):
	//   升级:抬高按绝对增量补(降级只夹);
	//   加载/加点/切方案/洗点/换装:按比例保持 —— 降后再升往返不净得(极低血量因"至少留 1"有上界的回升例外,
	//   见 RescaleCurrent),堵住"切方案/洗点当治疗"。换装(kEquipmentChanged)必须落在这个分支:
	//   走上面的补增量分支的话,脱下加血装再穿回就是一次免费回血。
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
		// 按比例保持:与宝宝共用 attributerules::RescaleCurrent,两边口径不会分叉
		baseAttrs->set_health(
			attributerules::RescaleCurrent(baseAttrs->health(), oldMaxHealth, derived.max_health()));
		baseAttrs->set_mana(
			attributerules::RescaleCurrent(baseAttrs->mana(), oldMaxMana, derived.max_mana()));
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
		if (!IsPlayerPool(pool)) {
			continue;
		}
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

	// 装备的一级属性点在这里再现算一次,而不是在 Recalculate 时缓存进一个运行时组件:
	//   * 一级属性的输入本来就都是现读的(等级、已分配、bonus_values),装备点与它们同口径;
	//   * 面板只在开窗与写操作之后才建,不在逐帧路径上,CollectBonus 是纯读,多算一次没有副作用;
	//   * 缓存就是装备栏之外的第二份真相,还要多一个「只许 Recalculate 写」的组件要守。
	// 二级属性与战斗类加成是产出,仍读上一次 Recalculate 写下的 DerivedAttributesComp(见下)。
	equiprules::EquipBonus equipBonus;
	PlayerEquipSystem::CollectBonus(player, equipBonus);

	const auto& dims = AttributeDimensionTableManager::Instance().FindAll().data();
	for (const auto& dim : dims) {
		if (!IsPlayerDimensionRow(dim)) {
			continue;
		}
		auto* info = panel.add_dimensions();
		info->set_dimension_id(dim.id());
		info->set_pool_id(dim.pool_id());
		info->set_name(dim.name());
		info->set_desc(dim.desc());
		info->set_allocated(scheme != nullptr ? AllocatedIn(*scheme, dim.id()) : 0);
		// bonus 已含在 value 内(客户端显示成「170(+15)」);它不占点、也不抬高可分配上限。
		const uint64_t externalBonus = ExternalBonusPoints(comp, equipBonus, dim.id());
		info->set_value(DimensionValue(player, scheme, dim, externalBonus));
		info->set_bonus(SaturateToU32(externalBonus));
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

	FillCombatPanel(player, panel);
}

uint32_t PlayerAttributeSystem::Allocate(entt::entity player, uint32_t poolId,
										 const std::map<uint32_t, uint32_t>& target) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	const auto [pool, poolErr] = AttributePoolTableManager::Instance().FindByIdSilent(poolId);
	if (pool == nullptr || !IsPlayerPool(*pool)) {
		return kAttributePoolNotFound;  // 宝宝池走 PetSystem,角色接口按不存在处理
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
	if (pool == nullptr || !IsPlayerPool(*pool)) {
		return kAttributePoolNotFound;  // 宝宝池走 PetSystem,角色接口按不存在处理
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
	if (pool == nullptr || !IsPlayerPool(*pool)) {
		return kAttributePoolNotFound;  // 宝宝池走 PetSystem,角色接口按不存在处理
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
	if (!playerlevel::IsValidLevel(level)) {
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
