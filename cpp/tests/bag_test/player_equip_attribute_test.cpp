// 装备加成接进角色属性计算与属性面板(PlayerAttributeSystem::Recalculate / BuildPanel)。
// 契约见 docs/design/equipment-attributes.md §3.2 / §3.3 / §4.4 / §5(不变量 2 / 3 / 4)。
//
// 这里钉的是「装备栏里有什么 -> 二级属性 / 面板是多少」,不是穿脱流程:用例用搬运原语
// Bag::PutInstance / TakeInstance 直接把手工构造的装备实例放进 / 拿出 bags[kEquipment],再显式调
// Recalculate(kEquipmentChanged) —— 这正是 PlayerEquipSystem::Equip / Unequip 成功后做的那两步。
// 汇总装备加成的 PlayerEquipSystem::CollectBonus 与反查显示信息的 DescribeCombatStat 是真实现,
// 所以用的是真实表行:样例装备 1105 / 1205 / 1305 / 1405(80 级档)与 EquipAttribute 1-26。
//
// 手算口径:装备的基础属性值、每点固定系数都现读配表(策划调数值不必改用例);
// 属性 id 的含义(伤害 = 物伤法伤各加、准确只加物伤 ...)按设计文档 §2.1 写死,并在夹具里逐行核对。
//
// 夹具自带:SetUp 自己加载用到的全部表并核对夹具行(缺行 = 还没导表,失败信息里写明),
// 不依赖别的文件的用例先跑或后跑;不经 BagService,所以不需要号段、也不碰掷属性回调。

#include <gtest/gtest.h>

#include <cmath>
#include <cstdint>
#include <map>
#include <set>
#include <string>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "services/scene/player/system/equip_attribute_rules.h"
#include "services/scene/player/system/player_attribute.h"
#include "services/scene/player/system/player_data_loader.h"
#include "services/scene/player/system/player_equip.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/condition_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

using RecalcReason = PlayerAttributeSystem::RecalcReason;
using equiprules::CombatStat;

const char *const kExportHint = "run the data table exporter first (docs/design/equipment-attributes.md 2.6)";

// ── 夹具数据 ─────────────────────────────────────────────────────────────
constexpr uint32_t kLevel = 80;
// 角色自带的暴击率 / 抗性(整数百分点)。夹具自己写进 BaseAttributesComp,不取职业表:
// Recalculate 不碰这两个字段,用例要的只是「面板终值 = 它 + 装备加成」。
constexpr uint64_t kBaseCritChance = 10;
constexpr uint64_t kBaseResistance = 5;
// 远大于任何上限:加载分支(旧上限为 0)只向下夹,于是新造的玩家满血满蓝。
constexpr uint64_t kFarAboveAnyMax = 1000000000;

// 样例装备(设计文档 §2.6):80 级档,部位 11-14,槽 3-6。
constexpr uint32_t kEquipLevel = 80;
constexpr uint32_t kWeapon = 1105;
constexpr uint32_t kHat = 1205;
constexpr uint32_t kClothes = 1305;
constexpr uint32_t kShoes = 1405;

// EquipAttribute.id(设计文档 §2.1,id 一经使用不再改义)。
constexpr uint32_t kAttrDamage = 1;
constexpr uint32_t kAttrAccuracy = 2;
constexpr uint32_t kAttrStrength = 3;
constexpr uint32_t kAttrConstitution = 4;
constexpr uint32_t kAttrSpirit = 5;
constexpr uint32_t kAttrAgility = 6;
constexpr uint32_t kAttrAllPrimary = 7;
constexpr uint32_t kAttrDefense = 8;
constexpr uint32_t kAttrHealth = 9;
constexpr uint32_t kAttrMana = 10;
constexpr uint32_t kAttrSpeed = 11;
constexpr uint32_t kAttrPhysicalCrit = 12;
constexpr uint32_t kAttrMagicCrit = 13;
constexpr uint32_t kAttrCombo = 14;
constexpr uint32_t kAttrCounter = 15;
constexpr uint32_t kAttrReflect = 16;
constexpr uint32_t kAttrSkillLevel = 17;
constexpr uint32_t kAttrIgnoreAilment = 18;
constexpr uint32_t kAttrResistPoison = 19;
constexpr uint32_t kAttrResistFreeze = 20;
constexpr uint32_t kAttrResistSleep = 21;
constexpr uint32_t kAttrResistForget = 22;
constexpr uint32_t kAttrResistConfusion = 23;
constexpr uint32_t kAttrResistAllAilment = 24;
constexpr uint32_t kAttrMagicResist = 25;
constexpr uint32_t kAttrPhysicalResist = 26;

// AttributeDimension.id(角色属性点池的四个维度)。
constexpr uint32_t kDimConstitution = 101;
constexpr uint32_t kDimSpirit = 102;
constexpr uint32_t kDimStrength = 103;
constexpr uint32_t kDimAgility = 104;
constexpr uint32_t kPlayerDimensions[] = {kDimConstitution, kDimSpirit, kDimStrength, kDimAgility};

// 颜色档(EquipAffix.tier)。
constexpr uint32_t kBlue = 1;
constexpr uint32_t kPink = 2;
constexpr uint32_t kYellow = 3;

// 预设 guid:远离号段范围;同一用例里的第二个玩家用 +100 的那一组。
constexpr Guid kGuidWeapon = (uint64_t{1} << 60) + 9301;
constexpr Guid kGuidHat = (uint64_t{1} << 60) + 9302;
constexpr Guid kGuidClothes = (uint64_t{1} << 60) + 9303;
constexpr Guid kGuidShoes = (uint64_t{1} << 60) + 9304;
constexpr Guid kSecondSetOffset = 100;
constexpr uint64_t kPlayerIdBase = (uint64_t{1} << 58) + 9300;

// 「气血 / 法力」平加用例里的数值(都在 80 级档上限之内)。
constexpr uint32_t kHealthAffix = 300;
constexpr uint32_t kManaAffix = 200;

// 设计文档 §2.1 的属性清单:夹具逐行核对,表一漂就在 SetUp 里说清楚。
struct AttrRowExpectation
{
	uint32_t id;
	uint32_t effect;
	uint32_t param;
	uint32_t percent;
};
constexpr AttrRowExpectation kAttrRows[] = {
	{kAttrDamage, 4, 0, 0},
	{kAttrAccuracy, 3, 3, 0},
	{kAttrStrength, 1, kDimStrength, 0},
	{kAttrConstitution, 1, kDimConstitution, 0},
	{kAttrSpirit, 1, kDimSpirit, 0},
	{kAttrAgility, 1, kDimAgility, 0},
	{kAttrAllPrimary, 2, 0, 0},
	{kAttrDefense, 3, 6, 0},
	{kAttrHealth, 3, 1, 0},
	{kAttrMana, 3, 2, 0},
	{kAttrSpeed, 3, 5, 0},
	{kAttrPhysicalCrit, 5, 1, 1},
	{kAttrMagicCrit, 5, 2, 1},
	{kAttrCombo, 5, 3, 1},
	{kAttrCounter, 5, 4, 1},
	{kAttrReflect, 5, 5, 1},
	{kAttrSkillLevel, 5, 6, 0},
	{kAttrIgnoreAilment, 5, 7, 1},
	{kAttrResistPoison, 5, 8, 1},
	{kAttrResistFreeze, 5, 9, 1},
	{kAttrResistSleep, 5, 10, 1},
	{kAttrResistForget, 5, 11, 1},
	{kAttrResistConfusion, 5, 12, 1},
	{kAttrResistAllAilment, 5, 13, 1},
	{kAttrMagicResist, 5, 14, 1},
	{kAttrPhysicalResist, 5, 15, 1},
};

// ── 六项二级属性 ─────────────────────────────────────────────────────────
struct Six
{
	uint64_t maxHealth{0};
	uint64_t maxMana{0};
	uint64_t physicalAttack{0};
	uint64_t magicAttack{0};
	uint64_t speed{0};
	uint64_t defense{0};
};

Six operator+(const Six &lhs, const Six &rhs)
{
	Six sum;
	sum.maxHealth = lhs.maxHealth + rhs.maxHealth;
	sum.maxMana = lhs.maxMana + rhs.maxMana;
	sum.physicalAttack = lhs.physicalAttack + rhs.physicalAttack;
	sum.magicAttack = lhs.magicAttack + rhs.magicAttack;
	sum.speed = lhs.speed + rhs.speed;
	sum.defense = lhs.defense + rhs.defense;
	return sum;
}

// 增量(调用点保证 after >= before;不满足时用例本来就该红)。
Six Gain(const Six &before, const Six &after)
{
	Six gain;
	gain.maxHealth = after.maxHealth - before.maxHealth;
	gain.maxMana = after.maxMana - before.maxMana;
	gain.physicalAttack = after.physicalAttack - before.physicalAttack;
	gain.magicAttack = after.magicAttack - before.magicAttack;
	gain.speed = after.speed - before.speed;
	gain.defense = after.defense - before.defense;
	return gain;
}

Six ReadSix(const DerivedAttributesComp &derived)
{
	Six six;
	six.maxHealth = derived.max_health();
	six.maxMana = derived.max_mana();
	six.physicalAttack = derived.physical_attack();
	six.magicAttack = derived.magic_attack();
	six.speed = derived.speed();
	six.defense = derived.defense();
	return six;
}

void ExpectSix(const Six &expected, const Six &actual)
{
	EXPECT_EQ(expected.maxHealth, actual.maxHealth) << "max_health";
	EXPECT_EQ(expected.maxMana, actual.maxMana) << "max_mana";
	EXPECT_EQ(expected.physicalAttack, actual.physicalAttack) << "physical_attack";
	EXPECT_EQ(expected.magicAttack, actual.magicAttack) << "magic_attack";
	EXPECT_EQ(expected.speed, actual.speed) << "speed";
	EXPECT_EQ(expected.defense, actual.defense) << "defense";
}

// 点数 x 表里的每点系数必须是整数:二级属性向下取整,系数若配成小数,「增量逐项相等」就得另算,
// 那时这里先红,而不是让后面的断言莫名其妙差 1。
uint64_t Whole(double value)
{
	EXPECT_EQ(std::floor(value), value)
		<< "a per-point coefficient no longer yields whole numbers; rework the expected deltas";
	return static_cast<uint64_t>(value);
}

// 某个一级属性维度的 points 点,按每点固定系数换算成六项(系数取自 AttributeDimension 表)。
Six FromPoints(uint32_t dimensionId, uint64_t points)
{
	Six six;
	const auto *row = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId).first;
	if (row == nullptr)
	{
		ADD_FAILURE() << "AttributeDimension row " << dimensionId << " is missing";
		return six;
	}
	const auto count = static_cast<double>(points);
	six.maxHealth = Whole(row->max_health() * count);
	six.maxMana = Whole(row->max_mana() * count);
	six.physicalAttack = Whole(row->physical_attack() * count);
	six.magicAttack = Whole(row->magic_attack() * count);
	six.speed = Whole(row->speed() * count);
	six.defense = Whole(row->defense() * count);
	return six;
}

// 四个角色维度各 points 点(「所有属性」)。
Six FromAllDimensions(uint64_t points)
{
	Six six;
	for (const auto dimensionId : kPlayerDimensions)
	{
		six = six + FromPoints(dimensionId, points);
	}
	return six;
}

// 每级自然成长给的点数(不占点)。
uint64_t NaturalPoints(uint32_t dimensionId)
{
	const auto *row = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId).first;
	return row != nullptr ? static_cast<uint64_t>(row->base_per_level()) * kLevel : 0;
}

// 没穿装备、没加点的 80 级角色 = 职业初值 + 四个维度的自然成长。
Six NoGearBaseline()
{
	Six six;
	const auto &classRows = ClassTableManager::Instance().FindAll().data();
	if (classRows.size() == 0)
	{
		ADD_FAILURE() << "Class table is empty";
		return six;
	}
	const auto &classRow = classRows.Get(0);
	six.maxHealth = classRow.init_health();
	six.maxMana = classRow.init_mana();
	six.speed = classRow.init_speed();
	for (const auto dimensionId : kPlayerDimensions)
	{
		six = six + FromPoints(dimensionId, NaturalPoints(dimensionId));
	}
	return six;
}

// 样例装备某条基础属性的值(取自 Item 表;行与属性 id 已由夹具核对)。
uint64_t BaseAttrValue(uint32_t configId, uint32_t attrId)
{
	const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
	if (row == nullptr)
	{
		return 0;
	}
	uint64_t total = 0;
	for (const auto &base : row->base_attr())
	{
		if (base.base_attr_id() == attrId)
		{
			total += base.base_attr_value();
		}
	}
	return total;
}

// ── 战斗类属性 ───────────────────────────────────────────────────────────
struct CombatValues
{
	uint32_t physicalCritRate{0};
	uint32_t magicCritRate{0};
	uint32_t comboRate{0};
	uint32_t counterRate{0};
	uint32_t reflectRate{0};
	uint32_t skillLevelBonus{0};
	uint32_t ignoreAilmentResist{0};
	uint32_t resistPoison{0};
	uint32_t resistFreeze{0};
	uint32_t resistSleep{0};
	uint32_t resistForget{0};
	uint32_t resistConfusion{0};
	uint32_t resistAllAilment{0};
	uint32_t magicResist{0};
	uint32_t physicalResist{0};
};

// 15 项两两不同(写错格 / 读错格都看得出来),且都在各自 80 级档的上限之内。
constexpr CombatValues kCombatSet{
	/*physicalCritRate=*/10, /*magicCritRate=*/4,     /*comboRate=*/6,         /*counterRate=*/7,
	/*reflectRate=*/8,       /*skillLevelBonus=*/3,   /*ignoreAilmentResist=*/16, /*resistPoison=*/11,
	/*resistFreeze=*/12,     /*resistSleep=*/13,      /*resistForget=*/14,     /*resistConfusion=*/15,
	/*resistAllAilment=*/9,  /*magicResist=*/2,       /*physicalResist=*/5};

void ExpectCombat(const CombatValues &expected, const CombatAttributes &actual)
{
	EXPECT_EQ(expected.physicalCritRate, actual.physical_crit_rate()) << "physical_crit_rate";
	EXPECT_EQ(expected.magicCritRate, actual.magic_crit_rate()) << "magic_crit_rate";
	EXPECT_EQ(expected.comboRate, actual.combo_rate()) << "combo_rate";
	EXPECT_EQ(expected.counterRate, actual.counter_rate()) << "counter_rate";
	EXPECT_EQ(expected.reflectRate, actual.reflect_rate()) << "reflect_rate";
	EXPECT_EQ(expected.skillLevelBonus, actual.skill_level_bonus()) << "skill_level_bonus";
	EXPECT_EQ(expected.ignoreAilmentResist, actual.ignore_ailment_resist()) << "ignore_ailment_resist";
	EXPECT_EQ(expected.resistPoison, actual.resist_poison()) << "resist_poison";
	EXPECT_EQ(expected.resistFreeze, actual.resist_freeze()) << "resist_freeze";
	EXPECT_EQ(expected.resistSleep, actual.resist_sleep()) << "resist_sleep";
	EXPECT_EQ(expected.resistForget, actual.resist_forget()) << "resist_forget";
	EXPECT_EQ(expected.resistConfusion, actual.resist_confusion()) << "resist_confusion";
	EXPECT_EQ(expected.resistAllAilment, actual.resist_all_ailment()) << "resist_all_ailment";
	EXPECT_EQ(expected.magicResist, actual.magic_resist()) << "magic_resist";
	EXPECT_EQ(expected.physicalResist, actual.physical_resist()) << "physical_resist";
}

// ── 面板 ─────────────────────────────────────────────────────────────────
const AttributeDimensionInfo *FindDimension(const AttributePanelInfo &panel, uint32_t dimensionId)
{
	for (const auto &info : panel.dimensions())
	{
		if (info.dimension_id() == dimensionId)
		{
			return &info;
		}
	}
	return nullptr;
}

const AttributePoolInfo *FindPool(const AttributePanelInfo &panel, uint32_t poolId)
{
	for (const auto &info : panel.pools())
	{
		if (info.pool_id() == poolId)
		{
			return &info;
		}
	}
	return nullptr;
}

const CombatAttributeInfo *FindCombat(const AttributePanelInfo &panel, CombatStat stat)
{
	for (const auto &info : panel.combat())
	{
		if (info.combat_id() == equiprules::ToRaw(stat))
		{
			return &info;
		}
	}
	return nullptr;
}

void ExpectDimension(const AttributePanelInfo &panel, uint32_t dimensionId, uint32_t allocated, uint32_t bonus)
{
	const auto *info = FindDimension(panel, dimensionId);
	ASSERT_NE(nullptr, info) << "dimension " << dimensionId << " missing from the panel";
	EXPECT_EQ(allocated, info->allocated()) << "dimension " << dimensionId;
	EXPECT_EQ(bonus, info->bonus()) << "dimension " << dimensionId;
	// bonus 已含在 value 内:value = 自然成长 + 已分配 + 外部加成
	EXPECT_EQ(NaturalPoints(dimensionId) + allocated + bonus, info->value()) << "dimension " << dimensionId;
}

void ExpectCombatLine(const AttributePanelInfo &panel, CombatStat stat, uint64_t expectedValue)
{
	const auto *line = FindCombat(panel, stat);
	ASSERT_NE(nullptr, line) << "combat_id " << equiprules::ToRaw(stat) << " missing from the panel";
	EXPECT_EQ(expectedValue, line->value()) << "combat_id " << equiprules::ToRaw(stat);
}

// 15 项全量、combat_id 不重复、名称 / 百分比 / 排序与表一致、按 (sort, combat_id) 严格升序。
void ExpectCombatListWellFormed(const AttributePanelInfo &panel)
{
	ASSERT_EQ(15, panel.combat_size()) << "the panel must list every combat stat, geared or not";
	std::set<uint32_t> seen;
	for (int i = 0; i < panel.combat_size(); ++i)
	{
		const auto &line = panel.combat(i);
		EXPECT_TRUE(seen.insert(line.combat_id()).second) << "duplicate combat_id " << line.combat_id();
		const auto stat = equiprules::ToCombatStat(line.combat_id());
		EXPECT_TRUE(stat != CombatStat::kNone) << "combat_id " << line.combat_id() << " is not a CombatStat";
		const auto display = PlayerEquipSystem::DescribeCombatStat(stat);
		EXPECT_FALSE(line.name().empty()) << "combat_id " << line.combat_id();
		EXPECT_EQ(display.name, line.name()) << "combat_id " << line.combat_id();
		EXPECT_EQ(display.percent, line.percent()) << "combat_id " << line.combat_id();
		EXPECT_EQ(display.sort, line.sort()) << "combat_id " << line.combat_id();
		if (i > 0)
		{
			const auto &previous = panel.combat(i - 1);
			EXPECT_TRUE(previous.sort() < line.sort() ||
						(previous.sort() == line.sort() && previous.combat_id() < line.combat_id()))
				<< "combat lines must be ordered by (sort, combat_id); broken at index " << i;
		}
	}
}

// ── 装备实例 ─────────────────────────────────────────────────────────────

// 一件「已完成初始化、0 条随机属性」的装备实例。
ItemComp MakeEquip(Guid guid, uint32_t configId)
{
	ItemComp item;
	item.set_item_id(guid);
	item.set_config_id(configId);
	item.set_size(1);
	item.mutable_equip();
	return item;
}

const EquipAttributeCapTable *CapRowAtEquipLevel(uint32_t attrId)
{
	for (const auto *row : EquipAttributeCapTableManager::Instance().GetByAttrId(attrId))
	{
		if (row->level() == kEquipLevel)
		{
			return row;
		}
	}
	return nullptr;
}

// 往实例上写一条随机属性。顺带核对数值没超过该属性在 80 级档的上限:CollectBonus 对超上限的值
// 按上限计,而这里的手算值都假定「写多少就算多少」。
void AddAffix(ItemComp &item, uint32_t attrId, uint32_t tier, uint32_t value, uint32_t seq)
{
	const auto *cap = CapRowAtEquipLevel(attrId);
	EXPECT_NE(nullptr, cap) << "EquipAttributeCap has no level-" << kEquipLevel << " row for attr " << attrId
							<< ": " << kExportHint;
	if (cap != nullptr)
	{
		EXPECT_LE(value, cap->cap()) << "attr " << attrId
									 << ": the test value exceeds the table cap; pick a smaller value";
	}
	auto *affix = item.mutable_equip()->add_affixes();
	affix->set_attr_id(attrId);
	affix->set_tier(tier);
	affix->set_value(value);
	affix->set_seq(seq);
}

// 衣服:气血 +300、法力 +200(当前气血 / 法力跟随上限的几条用例共用)。
ItemComp MakeVitalityClothes(Guid guid)
{
	ItemComp clothes = MakeEquip(guid, kClothes);
	AddAffix(clothes, kAttrHealth, kBlue, kHealthAffix, 0);
	AddAffix(clothes, kAttrMana, kBlue, kManaAffix, 1);
	return clothes;
}

class PlayerEquipAttributeTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ClassTableManager::Instance().Load();
		AttributePoolTableManager::Instance().Load();
		AttributeDimensionTableManager::Instance().Load();
		AttributeRuleTableManager::Instance().Load();
		AttributeAllocRatioTableManager::Instance().Load();
		ItemTableManager::Instance().Load();
		EquipSlotTableManager::Instance().Load();
		EquipAttributeTableManager::Instance().Load();
		EquipAttributeCapTableManager::Instance().Load();
		EquipAffixPoolTableManager::Instance().Load();
		EquipAffixRuleTableManager::Instance().Load();
		// 存档往返那条用例走整条 PlayerAllData 装卸入口,任务还原要这两张表。
		MissionTableManager::Instance().Load();
		ConditionTableManager::Instance().Load();
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());
		player = NewPlayer();
	}

	void TearDown() override
	{
		for (const auto entity : players)
		{
			if (tlsEcs.actorRegistry.valid(entity))
			{
				tlsEcs.actorRegistry.destroy(entity);
			}
		}
	}

	// 前置条件:下面每条用例都建立在这些表行上。先断言行指针非空再解引用。
	static void RequireEquipItemRow(uint32_t configId, uint32_t equipKind, const std::vector<uint32_t> &baseAttrIds)
	{
		const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
		ASSERT_NE(nullptr, row) << "Item row " << configId << " is missing: " << kExportHint;
		ASSERT_EQ(1u, row->max_stack_size()) << "Item row " << configId;
		ASSERT_EQ(equipKind, row->equip_kind()) << "Item row " << configId;
		ASSERT_EQ(kEquipLevel, row->equip_level()) << "Item row " << configId;
		std::vector<uint32_t> actualIds;
		for (const auto &base : row->base_attr())
		{
			actualIds.push_back(base.base_attr_id());
			ASSERT_GT(base.base_attr_value(), 0u) << "Item row " << configId << " base attr " << base.base_attr_id();
		}
		ASSERT_EQ(baseAttrIds, actualIds) << "Item row " << configId << " base_attr ids drifted from 2.6";
	}

	static void RequireSlotRow(uint32_t slot, uint32_t equipKind)
	{
		const auto *row = EquipSlotTableManager::Instance().FindByIdSilent(slot).first;
		ASSERT_NE(nullptr, row) << "EquipSlot row " << slot << " is missing: " << kExportHint;
		ASSERT_EQ(equipKind, row->equip_kind()) << "EquipSlot row " << slot;
	}

	static void RequireAttrRow(const AttrRowExpectation &expected)
	{
		const auto *row = EquipAttributeTableManager::Instance().FindByIdSilent(expected.id).first;
		ASSERT_NE(nullptr, row) << "EquipAttribute row " << expected.id << " is missing: " << kExportHint;
		ASSERT_EQ(expected.effect, row->effect()) << "EquipAttribute row " << expected.id;
		ASSERT_EQ(expected.param, row->effect_param()) << "EquipAttribute row " << expected.id;
		ASSERT_EQ(expected.percent, row->percent()) << "EquipAttribute row " << expected.id;
		ASSERT_FALSE(row->name().empty()) << "EquipAttribute row " << expected.id;
	}

	static void RequireDimensionRow(uint32_t dimensionId)
	{
		const auto *row = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId).first;
		ASSERT_NE(nullptr, row) << "AttributeDimension row " << dimensionId << " is missing";
		const auto *pool = AttributePoolTableManager::Instance().FindByIdSilent(row->pool_id()).first;
		ASSERT_NE(nullptr, pool) << "AttributePool row " << row->pool_id() << " is missing";
		ASSERT_EQ(0u, pool->owner_type()) << "dimension " << dimensionId << " must belong to a player pool";
	}

	static void RequireFixtureRows()
	{
		ASSERT_GT(ClassTableManager::Instance().FindAll().data_size(), 0) << "Class table is empty";
		for (const auto dimensionId : kPlayerDimensions)
		{
			ASSERT_NO_FATAL_FAILURE(RequireDimensionRow(dimensionId));
		}
		for (const auto &expected : kAttrRows)
		{
			ASSERT_NO_FATAL_FAILURE(RequireAttrRow(expected));
		}
		ASSERT_NO_FATAL_FAILURE(RequireEquipItemRow(kWeapon, 11, {kAttrDamage}));
		ASSERT_NO_FATAL_FAILURE(RequireEquipItemRow(kHat, 12, {kAttrDefense}));
		ASSERT_NO_FATAL_FAILURE(RequireEquipItemRow(kClothes, 13, {kAttrDefense}));
		ASSERT_NO_FATAL_FAILURE(RequireEquipItemRow(kShoes, 14, {kAttrDefense, kAttrSpeed}));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(3, 11));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(4, 12));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(5, 13));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(6, 14));
	}

	// 最小可重算玩家:基础属性 + 等级 +(可选)背包,走一遍加载期的初始化。满血满蓝。
	entt::entity NewPlayer(bool withBags = true)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<Guid>(entity, kPlayerIdBase + players.size());
		auto &base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
		base.set_health(kFarAboveAnyMax);
		base.set_mana(kFarAboveAnyMax);
		base.set_critchance(kBaseCritChance);
		base.set_resistance(kBaseResistance);
		tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(kLevel);
		if (withBags)
		{
			tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		}
		PlayerAttributeSystem::InitializeOnLoad(entity);
		return entity;
	}

	static Bag &Equipment(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<PlayerBagsComp>(entity).bags[kEquipment];
	}
	static BaseAttributesComp &Base(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<BaseAttributesComp>(entity);
	}
	static const DerivedAttributesComp &Derived(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<DerivedAttributesComp>(entity);
	}
	static PlayerAttributeComp &AttributeComp(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<PlayerAttributeComp>(entity);
	}

	// 穿上 = 放进装备栏(按部位自动找槽)+ 以「换装」为由重算。
	static void Wear(entt::entity entity, const ItemComp &item)
	{
		ASSERT_EQ(kSuccess, Equipment(entity).PutInstance(item))
			<< "config " << item.config_id() << " did not fit the equipment bar";
		PlayerAttributeSystem::Recalculate(entity, RecalcReason::kEquipmentChanged);
	}

	static void TakeOff(entt::entity entity, Guid guid)
	{
		ItemComp carried;
		ASSERT_EQ(kSuccess, Equipment(entity).TakeInstance(guid, carried)) << "guid " << guid << " is not worn";
		PlayerAttributeSystem::Recalculate(entity, RecalcReason::kEquipmentChanged);
	}

	// 四件各带若干战斗类属性,合起来覆盖全部 15 项(数值见 kCombatSet)。
	// 武器池的 6 条都只能放在武器上,所以它带了 4 条蓝 —— 掷值规则掷不出这种形状,但 CollectBonus 只管
	// 累加实例上写着的属性行,不复核条数与池(那是 RollAffixes 的事)。
	static void WearCombatSet(entt::entity entity, Guid guidOffset = 0)
	{
		ItemComp weapon = MakeEquip(kGuidWeapon + guidOffset, kWeapon);
		AddAffix(weapon, kAttrPhysicalCrit, kBlue, kCombatSet.physicalCritRate, 0);
		AddAffix(weapon, kAttrCombo, kBlue, kCombatSet.comboRate, 1);
		AddAffix(weapon, kAttrCounter, kBlue, kCombatSet.counterRate, 2);
		AddAffix(weapon, kAttrSkillLevel, kBlue, kCombatSet.skillLevelBonus, 3);
		AddAffix(weapon, kAttrMagicCrit, kPink, kCombatSet.magicCritRate, 0);
		AddAffix(weapon, kAttrIgnoreAilment, kYellow, kCombatSet.ignoreAilmentResist, 0);

		ItemComp hat = MakeEquip(kGuidHat + guidOffset, kHat);
		AddAffix(hat, kAttrReflect, kBlue, kCombatSet.reflectRate, 0);
		AddAffix(hat, kAttrResistPoison, kBlue, kCombatSet.resistPoison, 1);
		AddAffix(hat, kAttrResistFreeze, kBlue, kCombatSet.resistFreeze, 2);

		ItemComp clothes = MakeEquip(kGuidClothes + guidOffset, kClothes);
		AddAffix(clothes, kAttrResistSleep, kBlue, kCombatSet.resistSleep, 0);
		AddAffix(clothes, kAttrResistForget, kBlue, kCombatSet.resistForget, 1);
		AddAffix(clothes, kAttrResistConfusion, kBlue, kCombatSet.resistConfusion, 2);

		ItemComp shoes = MakeEquip(kGuidShoes + guidOffset, kShoes);
		AddAffix(shoes, kAttrResistAllAilment, kBlue, kCombatSet.resistAllAilment, 0);
		AddAffix(shoes, kAttrMagicResist, kPink, kCombatSet.magicResist, 0);
		AddAffix(shoes, kAttrPhysicalResist, kYellow, kCombatSet.physicalResist, 0);

		ASSERT_NO_FATAL_FAILURE(Wear(entity, weapon));
		ASSERT_NO_FATAL_FAILURE(Wear(entity, hat));
		ASSERT_NO_FATAL_FAILURE(Wear(entity, clothes));
		ASSERT_NO_FATAL_FAILURE(Wear(entity, shoes));
	}

	// 四件样例装备的基础属性合计(不含任何随机属性)。
	static Six FullSetBaseAttributes()
	{
		Six six;
		const uint64_t damage = BaseAttrValue(kWeapon, kAttrDamage);
		six.physicalAttack = damage;
		six.magicAttack = damage;
		six.defense = BaseAttrValue(kHat, kAttrDefense) + BaseAttrValue(kClothes, kAttrDefense) +
					  BaseAttrValue(kShoes, kAttrDefense);
		six.speed = BaseAttrValue(kShoes, kAttrSpeed);
		return six;
	}

	static uint32_t PlayerPoolId()
	{
		const auto *row = AttributeDimensionTableManager::Instance().FindByIdSilent(kDimStrength).first;
		return row != nullptr ? row->pool_id() : 0;
	}

	std::vector<entt::entity> players;
	entt::entity player{entt::null};
};

// ── 二级属性:六项的增量逐项等于手算值 ────────────────────────────────────

// 空装备栏、以及压根没有背包组件(尚未完成加载)的实体:结果就是装备系统落地之前的那个数。
TEST_F(PlayerEquipAttributeTest, EmptyEquipmentBarLeavesAttributesAtTheNoGearBaseline)
{
	const Six baseline = NoGearBaseline();
	ExpectSix(baseline, ReadSix(Derived(player)));
	ExpectCombat(CombatValues{}, Derived(player).combat());
	EXPECT_EQ(baseline.maxHealth, Base(player).health()) << "a freshly loaded player starts at full health";
	EXPECT_EQ(baseline.maxMana, Base(player).mana());

	PlayerAttributeSystem::Recalculate(player, RecalcReason::kEquipmentChanged);
	ExpectSix(baseline, ReadSix(Derived(player)));
	ExpectCombat(CombatValues{}, Derived(player).combat());
	EXPECT_EQ(baseline.maxHealth, Base(player).health()) << "an equipment recalculation with nothing worn is a no-op";
	EXPECT_EQ(baseline.maxMana, Base(player).mana());

	// 没有背包组件的实体:加载期那次重算(NewPlayer 里)已经把它当成「什么都没穿」。
	const auto withoutBags = NewPlayer(/*withBags=*/false);
	ASSERT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(withoutBags));
	ExpectSix(baseline, ReadSix(Derived(withoutBags)));
	ExpectCombat(CombatValues{}, Derived(withoutBags).combat());

	// 再各走一遍会调 CollectBonus 的两条路(换装重算、建面板):结果不变,并且谁都不许顺手造出背包组件
	// (属性同步路径禁 get_or_emplace;造出来的空背包会让「尚未完成加载」的玩家看起来像已加载)。
	PlayerAttributeSystem::Recalculate(withoutBags, RecalcReason::kEquipmentChanged);
	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(withoutBags, panel);
	ExpectSix(baseline, ReadSix(Derived(withoutBags)));
	ExpectCombat(CombatValues{}, Derived(withoutBags).combat());
	for (const auto dimensionId : kPlayerDimensions)
	{
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(panel, dimensionId, 0, 0));
	}
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(withoutBags))
		<< "neither recalculating nor building the panel may create a bag component as a side effect";
}

// 伤害 +N:物伤与法伤各 +N,其余四项不动。
TEST_F(PlayerEquipAttributeTest, WeaponBaseDamageAddsToPhysicalAndMagicAttack)
{
	const Six before = ReadSix(Derived(player));
	const uint64_t damage = BaseAttrValue(kWeapon, kAttrDamage);

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidWeapon, kWeapon)));

	Six expected = before;
	expected.physicalAttack += damage;
	expected.magicAttack += damage;
	ExpectSix(expected, ReadSix(Derived(player)));
}

// 准确只加物伤(设计文档 D2);随机属性里的「伤害」与基础属性同样物伤法伤各加一份。
TEST_F(PlayerEquipAttributeTest, AccuracyAddsPhysicalAttackOnlyWhileDamageAffixAddsBoth)
{
	constexpr uint32_t kAccuracy = 500;
	constexpr uint32_t kDamageAffix = 400;
	const Six before = ReadSix(Derived(player));
	const uint64_t baseDamage = BaseAttrValue(kWeapon, kAttrDamage);

	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	AddAffix(weapon, kAttrAccuracy, kBlue, kAccuracy, 0);
	AddAffix(weapon, kAttrDamage, kBlue, kDamageAffix, 1);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));

	Six expected = before;
	expected.physicalAttack += baseDamage + kDamageAffix + kAccuracy;
	expected.magicAttack += baseDamage + kDamageAffix;
	ExpectSix(expected, ReadSix(Derived(player)));
}

// 力量 / 体质 / 灵力 / 敏捷 +k:各自维度 +k 点,按该维度的每点固定系数换算(不走加点公式)。
TEST_F(PlayerEquipAttributeTest, PrimaryPointAffixesUseThePerPointCoefficientOfTheirOwnDimension)
{
	constexpr uint32_t kConstitution = 7;
	constexpr uint32_t kSpirit = 9;
	constexpr uint32_t kStrength = 10;
	constexpr uint32_t kAgility = 11;
	const Six before = ReadSix(Derived(player));

	ItemComp hat = MakeEquip(kGuidHat, kHat);
	AddAffix(hat, kAttrConstitution, kBlue, kConstitution, 0);
	AddAffix(hat, kAttrSpirit, kBlue, kSpirit, 1);
	AddAffix(hat, kAttrStrength, kBlue, kStrength, 2);
	AddAffix(hat, kAttrAgility, kBlue, kAgility, 3);
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));

	Six hatBase;
	hatBase.defense = BaseAttrValue(kHat, kAttrDefense);
	const Six expected = before + hatBase + FromPoints(kDimConstitution, kConstitution) +
						 FromPoints(kDimSpirit, kSpirit) + FromPoints(kDimStrength, kStrength) +
						 FromPoints(kDimAgility, kAgility);
	ExpectSix(expected, ReadSix(Derived(player)));

	// 这几条属性都确实落到了数值上(系数表若把某一维配成全 0,上面的等式会空转)。
	const Six gain = Gain(before, ReadSix(Derived(player)));
	EXPECT_GT(gain.maxHealth, 0u);
	EXPECT_GT(gain.maxMana, 0u);
	EXPECT_GT(gain.physicalAttack, 0u);
	EXPECT_GT(gain.magicAttack, 0u);
	EXPECT_GT(gain.speed, 0u);
}

// 所有属性 +k:四个角色维度各 +k 点;与同一维度的单项属性叠加。
TEST_F(PlayerEquipAttributeTest, AllPrimaryPointsAddsToEachPlayerDimensionAndStacksWithSinglePoints)
{
	constexpr uint32_t kAll = 8;
	constexpr uint32_t kStrength = 10;
	const Six before = ReadSix(Derived(player));

	ItemComp clothes = MakeEquip(kGuidClothes, kClothes);
	AddAffix(clothes, kAttrAllPrimary, kBlue, kAll, 0);
	ASSERT_NO_FATAL_FAILURE(Wear(player, clothes));

	Six gearBase;
	gearBase.defense = BaseAttrValue(kClothes, kAttrDefense);
	ExpectSix(before + gearBase + FromAllDimensions(kAll), ReadSix(Derived(player)));

	ItemComp hat = MakeEquip(kGuidHat, kHat);
	AddAffix(hat, kAttrStrength, kBlue, kStrength, 0);
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));

	gearBase.defense += BaseAttrValue(kHat, kAttrDefense);
	ExpectSix(before + gearBase + FromAllDimensions(kAll) + FromPoints(kDimStrength, kStrength),
			  ReadSix(Derived(player)));
}

// 气血 / 法力 / 防御 / 速度平加;两件的防御累加;速度同步进基础属性,护甲不被装备的「防御」带动。
TEST_F(PlayerEquipAttributeTest, FlatAffixesAndBaseAttributesAddHealthManaDefenseAndSpeed)
{
	constexpr uint32_t kDefenseAffix = 100;
	const Six before = ReadSix(Derived(player));
	const uint64_t armorBefore = Base(player).armor();
	ASSERT_EQ(before.speed, Base(player).speed());

	ItemComp clothes = MakeVitalityClothes(kGuidClothes);
	AddAffix(clothes, kAttrDefense, kBlue, kDefenseAffix, 2);
	ASSERT_NO_FATAL_FAILURE(Wear(player, clothes));
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidShoes, kShoes)));

	Six expected = before;
	expected.maxHealth += kHealthAffix;
	expected.maxMana += kManaAffix;
	expected.defense += BaseAttrValue(kClothes, kAttrDefense) + kDefenseAffix + BaseAttrValue(kShoes, kAttrDefense);
	expected.speed += BaseAttrValue(kShoes, kAttrSpeed);
	ExpectSix(expected, ReadSix(Derived(player)));

	EXPECT_EQ(expected.speed, Base(player).speed())
		<< "turn order reads BaseAttributesComp.speed; gear speed must reach it";
	EXPECT_EQ(armorBefore, Base(player).armor()) << "gear defense goes to DerivedAttributesComp.defense, not armor";
}

// ── 战斗类属性:15 项写进 DerivedAttributesComp.combat,卸下后清零 ──────────

TEST_F(PlayerEquipAttributeTest, CombatBonusesAreWrittenByNameAndClearedWhenTheGearComesOff)
{
	const Six before = ReadSix(Derived(player));

	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));
	ExpectCombat(kCombatSet, Derived(player).combat());
	// 战斗类属性不串进六项:六项只多出四件的基础属性。
	ExpectSix(before + FullSetBaseAttributes(), ReadSix(Derived(player)));

	// 整块覆盖而不是累加:再怎么重算都是同一组数。
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kEquipmentChanged);
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);
	ExpectCombat(kCombatSet, Derived(player).combat());
	ExpectSix(before + FullSetBaseAttributes(), ReadSix(Derived(player)));

	// 只脱鞋:鞋上的三项归零,其余原样。
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidShoes));
	CombatValues withoutShoes = kCombatSet;
	withoutShoes.resistAllAilment = 0;
	withoutShoes.magicResist = 0;
	withoutShoes.physicalResist = 0;
	ExpectCombat(withoutShoes, Derived(player).combat());

	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidWeapon));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidHat));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	ExpectCombat(CombatValues{}, Derived(player).combat());
	ExpectSix(before, ReadSix(Derived(player)));
}

// ── kEquipmentChanged:当前气血 / 法力按比例保持,穿脱不能当治疗 ──────────

TEST_F(PlayerEquipAttributeTest, FullHealthAndManaStayFullWhenGearRaisesTheMaximum)
{
	const uint64_t oldMaxHealth = Derived(player).max_health();
	const uint64_t oldMaxMana = Derived(player).max_mana();
	ASSERT_EQ(oldMaxHealth, Base(player).health());
	ASSERT_EQ(oldMaxMana, Base(player).mana());

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
	ASSERT_EQ(oldMaxHealth + kHealthAffix, Derived(player).max_health());
	ASSERT_EQ(oldMaxMana + kManaAffix, Derived(player).max_mana());
	EXPECT_EQ(Derived(player).max_health(), Base(player).health()) << "full stays full";
	EXPECT_EQ(Derived(player).max_mana(), Base(player).mana()) << "full stays full";

	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	EXPECT_EQ(oldMaxHealth, Base(player).health()) << "and comes back down with the maximum";
	EXPECT_EQ(oldMaxMana, Base(player).mana());
}

TEST_F(PlayerEquipAttributeTest, PartialHealthAndManaKeepTheirRatioInsteadOfGainingTheDelta)
{
	const uint64_t oldMaxHealth = Derived(player).max_health();
	const uint64_t oldMaxMana = Derived(player).max_mana();
	const uint64_t health = oldMaxHealth / 2;
	const uint64_t mana = oldMaxMana / 4;
	ASSERT_GT(health, 0u);
	ASSERT_GT(mana, 0u);
	Base(player).set_health(health);
	Base(player).set_mana(mana);

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
	const uint64_t newMaxHealth = Derived(player).max_health();
	const uint64_t newMaxMana = Derived(player).max_mana();
	ASSERT_EQ(oldMaxHealth + kHealthAffix, newMaxHealth);
	ASSERT_EQ(oldMaxMana + kManaAffix, newMaxMana);

	// hp = floor(hp x newMax / oldMax)
	EXPECT_EQ(health * newMaxHealth / oldMaxHealth, Base(player).health());
	EXPECT_EQ(mana * newMaxMana / oldMaxMana, Base(player).mana());
	// 不是「上限涨多少、当前值补多少」:那条分支只属于升级。
	EXPECT_LT(Base(player).health(), health + kHealthAffix);
	EXPECT_LT(Base(player).mana(), mana + kManaAffix);
	EXPECT_LT(Base(player).health(), newMaxHealth);
}

TEST_F(PlayerEquipAttributeTest, TakingGearOffAndPuttingItBackNeverHeals)
{
	constexpr uint64_t kWoundedHealth = 1234;
	constexpr uint64_t kSpentMana = 987;
	ASSERT_GT(Derived(player).max_health(), kWoundedHealth);
	ASSERT_GT(Derived(player).max_mana(), kSpentMana);
	Base(player).set_health(kWoundedHealth);
	Base(player).set_mana(kSpentMana);

	// 先穿后脱:回到原上限时不比出发时多。
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	EXPECT_LE(Base(player).health(), kWoundedHealth);
	EXPECT_LE(Base(player).mana(), kSpentMana);

	// 穿着受伤,再反复脱下穿回:每个来回都不净得。
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
	Base(player).set_health(kWoundedHealth);
	Base(player).set_mana(kSpentMana);
	for (int round = 0; round < 5; ++round)
	{
		const uint64_t healthBefore = Base(player).health();
		const uint64_t manaBefore = Base(player).mana();
		ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
		EXPECT_LE(Base(player).health(), healthBefore) << "round " << round;
		ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
		EXPECT_LE(Base(player).health(), healthBefore) << "round " << round;
		EXPECT_LE(Base(player).mana(), manaBefore) << "round " << round;
		EXPECT_GT(Base(player).health(), 0u) << "a living player keeps at least 1 hp; round " << round;
	}
}

TEST_F(PlayerEquipAttributeTest, DeadPlayerStaysDeadThroughEquipmentChanges)
{
	Base(player).set_health(0);

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeVitalityClothes(kGuidClothes)));
	EXPECT_EQ(0u, Base(player).health()) << "wearing health gear must not revive";

	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	EXPECT_EQ(0u, Base(player).health());
}

// 换装不改等级、不改总点数:「已分配 > 总量」的整池清零只属于加载与等级变化。
TEST_F(PlayerEquipAttributeTest, EquipmentChangeDoesNotTriggerAllocationConvergence)
{
	constexpr uint32_t kAllocated = 100;
	constexpr uint32_t kLoweredLevel = 10;
	const uint32_t poolId = PlayerPoolId();
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));

	// 绕开升级事件直接压低等级,造出「已分配超过总量」的状态。
	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLoweredLevel);
	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	const auto *pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	ASSERT_LT(pool->total(), kAllocated) << "the lowered level must leave fewer points than were allocated";

	PlayerAttributeSystem::Recalculate(player, RecalcReason::kEquipmentChanged);
	PlayerAttributeSystem::BuildPanel(player, panel);
	const auto *strength = FindDimension(panel, kDimStrength);
	ASSERT_NE(nullptr, strength);
	EXPECT_EQ(kAllocated, strength->allocated()) << "an equipment change must leave the allocation alone";

	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);
	PlayerAttributeSystem::BuildPanel(player, panel);
	strength = FindDimension(panel, kDimStrength);
	ASSERT_NE(nullptr, strength);
	EXPECT_EQ(0u, strength->allocated()) << "the load path is the one that converges";
}

// ── 面板 ─────────────────────────────────────────────────────────────────

// value 含装备点;bonus = bonus_values + 装备点(已含在 value 内);装备点不占点也不送点。
TEST_F(PlayerEquipAttributeTest, PanelValueIncludesEquipmentPointsAndBonusReportsTheExternalPart)
{
	constexpr uint32_t kStoredBonus = 5; // 落库的外部加成(bonus_values,留给丹药的那个位)
	constexpr uint32_t kStrengthAffix = 10;
	constexpr uint32_t kAllAffix = 8;
	constexpr uint32_t kAllocated = 20;
	const uint32_t poolId = PlayerPoolId();

	(*AttributeComp(player).mutable_bonus_values())[kDimStrength] = kStoredBonus;
	PlayerAttributeSystem::Recalculate(player);

	AttributePanelInfo bare;
	PlayerAttributeSystem::BuildPanel(player, bare);
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(bare, kDimStrength, 0, kStoredBonus));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(bare, kDimConstitution, 0, 0));
	const auto *barePool = FindPool(bare, poolId);
	ASSERT_NE(nullptr, barePool);
	const uint32_t total = barePool->total();
	ASSERT_EQ(total, barePool->remaining());
	ASSERT_GE(total, kAllocated);

	ItemComp hat = MakeEquip(kGuidHat, kHat);
	AddAffix(hat, kAttrStrength, kBlue, kStrengthAffix, 0);
	AddAffix(hat, kAttrAllPrimary, kBlue, kAllAffix, 1);
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));

	const uint32_t strengthBonus = kStoredBonus + kStrengthAffix + kAllAffix;
	AttributePanelInfo geared;
	PlayerAttributeSystem::BuildPanel(player, geared);
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(geared, kDimStrength, 0, strengthBonus));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(geared, kDimConstitution, 0, kAllAffix));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(geared, kDimSpirit, 0, kAllAffix));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(geared, kDimAgility, 0, kAllAffix));
	const auto *gearedPool = FindPool(geared, poolId);
	ASSERT_NE(nullptr, gearedPool);
	EXPECT_EQ(total, gearedPool->total()) << "gear points are not allocatable points";
	EXPECT_EQ(total, gearedPool->remaining());

	// 面板显示的点数就是实际参与换算的点数:外部加成的每一点都按每点固定系数计。
	Six hatBase;
	hatBase.defense = BaseAttrValue(kHat, kAttrDefense);
	ExpectSix(NoGearBaseline() + hatBase + FromPoints(kDimStrength, strengthBonus) +
				  FromPoints(kDimConstitution, kAllAffix) + FromPoints(kDimSpirit, kAllAffix) +
				  FromPoints(kDimAgility, kAllAffix),
			  ReadSix(Derived(player)));
	EXPECT_EQ(Derived(player).physical_attack(), geared.derived().physical_attack());
	EXPECT_EQ(Derived(player).max_health(), geared.derived().max_health());

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));
	AttributePanelInfo allocated;
	PlayerAttributeSystem::BuildPanel(player, allocated);
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(allocated, kDimStrength, kAllocated, strengthBonus));
	const auto *allocatedPool = FindPool(allocated, poolId);
	ASSERT_NE(nullptr, allocatedPool);
	EXPECT_EQ(total - kAllocated, allocatedPool->remaining()) << "only allocated points are spent";
}

// 没穿装备:15 项照样全量下发,必杀率 = 基础暴击率,抗物理 / 抗法术 = 基础抗性,其余为 0。
TEST_F(PlayerEquipAttributeTest, PanelWithoutGearStillListsEveryCombatStatWithBaseCritAndResist)
{
	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	ASSERT_NO_FATAL_FAILURE(ExpectCombatListWellFormed(panel));

	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kPhysicalCritRate, kBaseCritChance));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kMagicCritRate, kBaseCritChance));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kMagicResist, kBaseResistance));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kPhysicalResist, kBaseResistance));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kComboRate, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kCounterRate, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kReflectRate, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kSkillLevelBonus, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kIgnoreAilmentResist, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistPoison, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistFreeze, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistSleep, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistForget, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistConfusion, 0));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistAllAilment, 0));

	for (const auto dimensionId : kPlayerDimensions)
	{
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(panel, dimensionId, 0, 0));
	}
}

// 穿着全套:15 项的终值 = 角色基础值(只有必杀率与两项抗性有)+ 装备加成,排序与显示信息来自表。
TEST_F(PlayerEquipAttributeTest, PanelListsAllFifteenCombatStatsSortedWithFinalValues)
{
	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));

	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	ASSERT_NO_FATAL_FAILURE(ExpectCombatListWellFormed(panel));

	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kPhysicalCritRate, kBaseCritChance + kCombatSet.physicalCritRate));
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kMagicCritRate, kBaseCritChance + kCombatSet.magicCritRate));
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kMagicResist, kBaseResistance + kCombatSet.magicResist));
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kPhysicalResist, kBaseResistance + kCombatSet.physicalResist));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kComboRate, kCombatSet.comboRate));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kCounterRate, kCombatSet.counterRate));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kReflectRate, kCombatSet.reflectRate));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kSkillLevelBonus, kCombatSet.skillLevelBonus));
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kIgnoreAilmentResist, kCombatSet.ignoreAilmentResist));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistPoison, kCombatSet.resistPoison));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistFreeze, kCombatSet.resistFreeze));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistSleep, kCombatSet.resistSleep));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistForget, kCombatSet.resistForget));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistConfusion, kCombatSet.resistConfusion));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kResistAllAilment, kCombatSet.resistAllAilment));

	// 是否百分比跟着表走:必杀率是百分点,所有技能上升是点数。
	const auto *physicalCrit = FindCombat(panel, CombatStat::kPhysicalCritRate);
	const auto *skillLevel = FindCombat(panel, CombatStat::kSkillLevelBonus);
	ASSERT_NE(nullptr, physicalCrit);
	ASSERT_NE(nullptr, skillLevel);
	EXPECT_TRUE(physicalCrit->percent());
	EXPECT_FALSE(skillLevel->percent());

	// 面板的二级属性仍是 DerivedAttributesComp 的那一份(同一次重算的产出)。
	EXPECT_EQ(Derived(player).physical_attack(), panel.derived().physical_attack());
	EXPECT_EQ(Derived(player).defense(), panel.derived().defense());
	EXPECT_EQ(Derived(player).speed(), panel.derived().speed());
}

// 百分比项的终值夹到 100;夹的只是面板显示,DerivedAttributesComp.combat 里仍是加成原值。
TEST_F(PlayerEquipAttributeTest, PanelClampsPercentStatsToOneHundred)
{
	constexpr uint64_t kHighCritChance = 95;
	constexpr uint64_t kHighResistance = 98;
	Base(player).set_critchance(kHighCritChance);
	Base(player).set_resistance(kHighResistance);
	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));
	ASSERT_GT(kHighCritChance + kCombatSet.physicalCritRate, 100u);
	ASSERT_LT(kHighCritChance + kCombatSet.magicCritRate, 100u);
	ASSERT_GT(kHighResistance + kCombatSet.physicalResist, 100u);

	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kPhysicalCritRate, 100));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kPhysicalResist, 100));
	ASSERT_NO_FATAL_FAILURE(ExpectCombatLine(panel, CombatStat::kMagicResist, kHighResistance + kCombatSet.magicResist));
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(panel, CombatStat::kMagicCritRate, kHighCritChance + kCombatSet.magicCritRate));

	ExpectCombat(kCombatSet, Derived(player).combat());
	EXPECT_EQ(kHighCritChance, Base(player).critchance()) << "building a panel must not write base attributes";
	EXPECT_EQ(kHighResistance, Base(player).resistance());
}

// ── 不落库、加点收益不随装备浮动、重登后重算 ─────────────────────────────

// 装备加成不进 bonus_values、不进任何落库的属性字段:PlayerAttributeComp 的序列化字节前后相同。
TEST_F(PlayerEquipAttributeTest, EquipmentBonusIsNeverWrittenToPersistedComponents)
{
	constexpr uint32_t kStoredBonus = 5;
	constexpr uint32_t kAllocated = 20;
	(*AttributeComp(player).mutable_bonus_values())[kDimStrength] = kStoredBonus;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, PlayerPoolId(), std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));

	// 同一个对象、期间不改它:map 字段的迭代序不变,字节可以直接比。
	const std::string attributeBefore = AttributeComp(player).SerializeAsString();
	const BaseAttributesComp baseBefore = Base(player);

	// 全套战斗类属性 + 基础属性的平加,再把帽子换成一件带一级属性点的:三类加成都参与了重算。
	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));
	ItemComp extraPoints = MakeEquip(kGuidHat + kSecondSetOffset, kHat);
	AddAffix(extraPoints, kAttrStrength, kBlue, 10, 0);
	AddAffix(extraPoints, kAttrAllPrimary, kBlue, 8, 1);
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidHat));
	ASSERT_NO_FATAL_FAILURE(Wear(player, extraPoints));
	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(panel, kDimStrength, kAllocated, kStoredBonus + 10 + 8));

	EXPECT_EQ(attributeBefore, AttributeComp(player).SerializeAsString())
		<< "equipment must not leak into PlayerAttributeComp (bonus_values is persisted)";
	ASSERT_EQ(1, AttributeComp(player).bonus_values_size());
	EXPECT_EQ(kStoredBonus, AttributeComp(player).bonus_values().at(kDimStrength));
	EXPECT_EQ(0, AttributeComp(player).bonus_points_size());

	// BaseAttributesComp 的成长字段不带装备数值。speed 是唯一的例外:它是重算结果的镜像
	// (每次加载都被覆盖、从不作为输入),当前气血 / 法力则按比例跟随上限。
	EXPECT_EQ(baseBefore.strength(), Base(player).strength());
	EXPECT_EQ(baseBefore.stamina(), Base(player).stamina());
	EXPECT_EQ(baseBefore.critchance(), Base(player).critchance());
	EXPECT_EQ(baseBefore.armor(), Base(player).armor());
	EXPECT_EQ(baseBefore.resistance(), Base(player).resistance());
	EXPECT_EQ(Derived(player).speed(), Base(player).speed());

	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidWeapon));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidHat + kSecondSetOffset));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidShoes));
	EXPECT_EQ(attributeBefore, AttributeComp(player).SerializeAsString());
	EXPECT_EQ(baseBefore.speed(), Base(player).speed()) << "nothing of the gear is left behind once it is off";
	EXPECT_EQ(baseBefore.health(), Base(player).health()) << "full health before, full health after";
}

// 同样的已分配点,有没有装备拿到的增量都一样:「标准基础属性」与加点公式的点数都不含装备。
TEST_F(PlayerEquipAttributeTest, AllocationGainDoesNotDependOnEquipment)
{
	const uint32_t poolId = PlayerPoolId();
	const std::map<uint32_t, uint32_t> target{{kDimStrength, 100}, {kDimConstitution, 50}};

	const auto geared = NewPlayer();
	ItemComp weapon = MakeEquip(kGuidWeapon + kSecondSetOffset, kWeapon);
	AddAffix(weapon, kAttrStrength, kBlue, 10, 0);
	AddAffix(weapon, kAttrAccuracy, kBlue, 500, 1);
	ItemComp hat = MakeEquip(kGuidHat + kSecondSetOffset, kHat);
	AddAffix(hat, kAttrAllPrimary, kBlue, 8, 0);
	AddAffix(hat, kAttrConstitution, kBlue, 7, 1);
	ASSERT_NO_FATAL_FAILURE(Wear(geared, weapon));
	ASSERT_NO_FATAL_FAILURE(Wear(geared, hat));
	ASSERT_NO_FATAL_FAILURE(Wear(geared, MakeVitalityClothes(kGuidClothes + kSecondSetOffset)));

	const Six plainBefore = ReadSix(Derived(player));
	const Six gearedBefore = ReadSix(Derived(geared));
	ASSERT_GT(gearedBefore.physicalAttack, plainBefore.physicalAttack);

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId, target));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(geared, poolId, target));

	const Six plainGain = Gain(plainBefore, ReadSix(Derived(player)));
	const Six gearedGain = Gain(gearedBefore, ReadSix(Derived(geared)));
	ExpectSix(plainGain, gearedGain);
	EXPECT_GT(plainGain.physicalAttack, 0u);
	EXPECT_GT(plainGain.maxHealth, 0u);
	EXPECT_GT(plainGain.defense, 0u);
}

// 存档往返:二级属性不落库,重登时背包先于属性初始化还原,装备加成按还原出的装备栏重新算出来。
// 满血带装备下线的玩家重登后仍是那个血量 —— 若加载时属性先于背包重算,这里会被夹回无装备上限。
TEST_F(PlayerEquipAttributeTest, ReloadFromTheDatabaseRecordRecomputesTheEquipmentBonus)
{
	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	ItemComp clothes = MakeVitalityClothes(kGuidClothes);
	AddAffix(clothes, kAttrAllPrimary, kBlue, 8, 2);
	ASSERT_NO_FATAL_FAILURE(Wear(player, clothes));
	ASSERT_EQ(Derived(player).max_health(), Base(player).health());
	ASSERT_GT(Derived(player).max_health(), NoGearBaseline().maxHealth);

	PlayerAllData saved;
	PlayerAllDataMessageFieldsMarshal(player, saved);
	std::string bytes;
	ASSERT_TRUE(saved.player_database_data().SerializeToString(&bytes));
	PlayerAllData coldSnapshot;
	ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));

	const auto restored = tlsEcs.actorRegistry.create();
	players.push_back(restored);
	tlsEcs.actorRegistry.emplace<Guid>(restored, kPlayerIdBase + players.size());
	PlayerAllDataMessageFieldsUnMarshal(restored, coldSnapshot);

	const auto *bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(restored);
	ASSERT_NE(nullptr, bags);
	ASSERT_EQ(4u, bags->bags[kEquipment].OccupiedGridCount());
	const auto *derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(restored);
	const auto *base = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(restored);
	ASSERT_NE(nullptr, derived);
	ASSERT_NE(nullptr, base);

	ExpectSix(ReadSix(Derived(player)), ReadSix(*derived));
	CombatValues expectedCombat = kCombatSet;
	expectedCombat.resistSleep = 0;
	expectedCombat.resistForget = 0;
	expectedCombat.resistConfusion = 0;
	ExpectCombat(expectedCombat, derived->combat());
	EXPECT_EQ(Base(player).health(), base->health())
		<< "the bag must be restored before attributes are initialised, or geared players lose health on login";
	EXPECT_EQ(Base(player).mana(), base->mana());
}

// ── 角色池维度判定:属性重算与装备表校验共用的那一份 ─────────────────────

// PlayerAttributeSystem::IsPlayerDimension 对表里每一个维度的回答 = 「它所属的池 owner_type == 0」;
// 不存在的维度答 false。装备的一级属性点只加到它答 true 的维度上,装备表校验
// (PlayerEquipSystem::ValidateTables)也拿它核对「一级属性点」行指向的维度 —— 两处问的是同一个函数。
// 期望值按表独立算一遍(AttributePool.owner_type:0 = 角色,非 0 = 其它),不经过被测函数。
TEST_F(PlayerEquipAttributeTest, IsPlayerDimensionFollowsThePoolOwnerType)
{
	uint32_t playerDimensions = 0;
	uint32_t otherDimensions = 0;
	for (const auto &dimension : AttributeDimensionTableManager::Instance().FindAll().data())
	{
		const auto *pool = AttributePoolTableManager::Instance().FindByIdSilent(dimension.pool_id()).first;
		const bool expected = pool != nullptr && pool->owner_type() == 0;
		EXPECT_EQ(expected, PlayerAttributeSystem::IsPlayerDimension(dimension.id())) << "dimension " << dimension.id();
		if (expected)
		{
			++playerDimensions;
		}
		else
		{
			++otherDimensions;
		}
	}
	for (const auto dimensionId : kPlayerDimensions)
	{
		EXPECT_TRUE(PlayerAttributeSystem::IsPlayerDimension(dimensionId)) << "dimension " << dimensionId;
	}
	EXPECT_GE(playerDimensions, 4u);
	EXPECT_GT(otherDimensions, 0u) << "no dimension outside the player pool (pet pool) is left to exercise the false branch";

	constexpr uint32_t kUnknownDimension = 987654;
	ASSERT_EQ(nullptr, AttributeDimensionTableManager::Instance().FindByIdSilent(kUnknownDimension).first);
	EXPECT_FALSE(PlayerAttributeSystem::IsPlayerDimension(kUnknownDimension));
}

} // namespace
