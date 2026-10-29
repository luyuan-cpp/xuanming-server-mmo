// 装备加成接进角色属性计算与属性面板(PlayerAttributeSystem::Recalculate / BuildPanel)。
// 契约见 docs/design/equipment-attributes.md §3.2 / §3.3 / §4.4 / §5(不变量 2 / 3 / 4);
// 强化与镶嵌(二期)见 docs/design/equipment-enhance-inlay.md §4.3(消费侧)/ §5(不变量 2 / 3 / 4)。
//
// 这里钉的是「装备栏里有什么 -> 二级属性 / 面板是多少」,不是穿脱流程:用例用搬运原语
// Bag::PutInstance / TakeInstance 直接把手工构造的装备实例放进 / 拿出 bags[kEquipment],再显式调
// Recalculate(kEquipmentChanged) —— 这正是 PlayerEquipSystem::Equip / Unequip 成功后做的那两步。
// 汇总装备加成的 PlayerEquipSystem::CollectBonus 与反查显示信息的 DescribeCombatStat 是真实现,
// 所以用的是真实表行:样例装备 1105 / 1205 / 1305 / 1405(80 级档)与 EquipAttribute 1-26。
//
// 二期的用例有两种写法:
//   * 「实例上写着 +N / 嵌着哪颗宝石 -> 属性是多少」仍旧手工构造实例(SetEnhanceLevel / AddGemRecord)再穿上;
//   * 「穿着时强化 / 镶嵌 / 取下,属性立刻跟上」直接调 PlayerEquipSystem::Enhance / InlayGem / RemoveGem ——
//     重算与推面板是这三个入口自己做的,用例不替它们调 Recalculate。扣费对账、拒绝码、流水、tooltip 在
//     player_equip_test.cpp,这里只看属性侧。
//
// 手算口径:装备的基础属性值、每点固定系数都现读配表(策划调数值不必改用例);
// 属性 id 的含义(伤害 = 物伤法伤各加、准确只加物伤 ...)按设计文档 §2.1 写死,并在夹具里逐行核对。
// 强化每一级的万分比、强化消耗、宝石的属性值同样现读表(EquipEnhance / EquipGem);
// 「哪种宝石给哪条属性、哪个孔收哪种」按二期设计文档 §2.2 / §2.3 写死,由 RequireForgeRows 核对。
//
// 夹具自带:SetUp 自己加载用到的全部表并核对夹具行(缺行 = 还没导表,失败信息里写明),
// 不依赖别的文件的用例先跑或后跑。进程级状态各自归位:item 号段(取下宝石退回的那一颗要铸号)在 SetUp
// 投一段、TearDown 恢复基线;强化掷骰的脚本随机源在 TearDown 复原;内存里改过的表行在 TearDown 重新加载。
// 不碰掷属性回调(用例不铸装备:装备实例都是手工构造后走搬运原语进包的)。

#include <gtest/gtest.h>
#include <google/protobuf/util/message_differencer.h>
#include "engine/core/type_define/player_guid_comp.h"

#include <cmath>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <limits>
#include <map>
#include <set>
#include <string>
#include <string_view>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/mission/comp/mission_comp.h" // MissionsContainerComp(存档往返用例补齐持久化组件)
#include "services/scene/player/comp/player_frozen_comp.h"
#include "muduo/base/Logging.h" // Logger::setOutput:数「推面板」留下的那条 WARN(见 PanelPushProbe)
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/asset_op_ledger_comp.pb.h"
#include "proto/common/component/battle_settlement_ledger_comp.pb.h"
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "services/scene/player/system/bag_marshal.h"
#include "services/scene/player/system/equip_attribute_rules.h"
#include "services/scene/player/system/player_attribute.h"
#include "services/scene/player/system/player_data_loader.h"
#include "services/scene/player/comp/player_persist_carry_comp.h"
#include "services/scene/player/system/player_equip.h"
#include "services/scene/player/system/player_mission.h"
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
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/code/mission_table.h"
#include "table/proto/tip/attribute_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

using RecalcReason = PlayerAttributeSystem::RecalcReason;
using equiprules::CombatStat;
// 比较两份 proto:map 字段的序列化顺序不保证,两个不同实例的 SerializeAsString 不能直接比
using google::protobuf::util::MessageDifferencer;

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
// 本版本表里没有的维度号,代表「新版本加的维度」:回滚后它是未知键 —— 原样携带、永不生效、计入已用
// (加点统一规则,attribute_allocation_rules.h)。用例里先断言表里确实没有这一行。
constexpr uint32_t kFutureDimension = 987654;

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

// ── 强化 / 镶嵌的夹具数据(docs/design/equipment-enhance-inlay.md §2)────────
const char *const kForgeExportHint =
	"run the data table exporter first (docs/design/equipment-enhance-inlay.md 2.1-2.4)";

// 部位(Item.equip_kind;孔位表按它分组)。
constexpr uint32_t kWeaponKind = 11;
constexpr uint32_t kHatKind = 12;
constexpr uint32_t kClothesKind = 13;
constexpr uint32_t kShoesKind = 14;

// 一级宝石(Item.id = EquipGem 表主键,§2.2)。属性值现读表;「哪种宝石给哪条属性」是契约,夹具逐行核对。
constexpr uint32_t kGemAttack = 2101;    // 伤害:物伤与法伤各加
constexpr uint32_t kGemDefense = 2201;   // 防御
constexpr uint32_t kGemHealth = 2301;    // 气血
constexpr uint32_t kGemSpeed = 2401;     // 速度
constexpr uint32_t kUnknownGem = 987654; // EquipGem 表里没有这一行

// 孔位号(§2.3):武器 0 号只收攻击宝石、1 号通用;帽子 0 号气血;衣服 0 号防御;鞋子 0 号迅捷。
constexpr uint32_t kSocketFirst = 0;
constexpr uint32_t kSocketSecond = 1;

// 人物背包里的夹具堆(强化材料、宝石):预设 guid + 固定槽位,走还原入口放进去,不铸号。
// 槽位避开背包前部 —— 放进背包的装备走自动找位,从 0 号槽起占。一个用例要几堆时在基数上加下标。
constexpr Guid kGuidMaterialStack = (uint64_t{1} << 60) + 9311;
constexpr Guid kGuidGemStack = (uint64_t{1} << 60) + 9321;
constexpr uint32_t kMaterialPos = 40;
constexpr uint32_t kGemPos = 60;

// 取下宝石退回的那一颗是新堆,要铸号(BagService::AddItems):夹具自己投一段 item 号段。
constexpr uint64_t kItemSegmentLo = 1400000;
constexpr uint64_t kItemSegmentHi = 1500000;

// 强化掷骰的脚本值:equiprules::RollEnhanceSuccess 掷一个 [0, 10000) 的数,小于成功率即成功。
constexpr uint64_t kRollAlwaysSucceeds = 0;                                  // 成功率 > 0 就成
constexpr uint64_t kRollFailsUnlessCertain = equiprules::kPermyriadBase - 1; // 成功率 < 10000 就败

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

// 同上,但按 level 级算(降级 / 超上限等级的用例用)。
uint64_t NaturalPointsAt(uint32_t dimensionId, uint32_t level)
{
	const auto *row = AttributeDimensionTableManager::Instance().FindByIdSilent(dimensionId).first;
	return row != nullptr ? static_cast<uint64_t>(row->base_per_level()) * level : 0;
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

// ── 强化 / 镶嵌:现读配表 ─────────────────────────────────────────────────

// EquipEnhance 第 level 行 = 「强化到 +level」的规则与 +level 的总加成;没有返回 nullptr。
const EquipEnhanceTable *EnhanceRowOrNull(uint32_t level)
{
	return EquipEnhanceTableManager::Instance().FindByIdSilent(level).first;
}

// +level 时基础属性的总加成(万分比)。0 级 = 0(未强化)。
uint32_t EnhancePermyriad(uint32_t level)
{
	if (level == 0)
	{
		return 0;
	}
	const auto *row = EnhanceRowOrNull(level);
	if (row == nullptr)
	{
		ADD_FAILURE() << "EquipEnhance row " << level << " is missing: " << kForgeExportHint;
		return 0;
	}
	return row->bonus_permyriad();
}

// 样例装备某条基础属性在 +level 时的值:表值按该级的万分比放大。
// 放大算法只有规则层那一份(equiprules::ScaleByPermyriad;取整与饱和由 equip_attribute_rules_test.cpp 钉住),
// 这里对的是「编排层把哪个表值、哪一级的万分比喂给了它」。夹具核对过样例装备的每条基础属性 id 只出现一次,
// 所以「先合计再放大」与「逐条放大再合计」是同一个数。
uint64_t EnhancedBaseAttr(uint32_t configId, uint32_t attrId, uint32_t level)
{
	return equiprules::ScaleByPermyriad(BaseAttrValue(configId, attrId), EnhancePermyriad(level));
}

const EquipGemTable *GemRowOrNull(uint32_t gemConfigId)
{
	return EquipGemTableManager::Instance().FindByIdSilent(gemConfigId).first;
}

// 一颗宝石提供的属性值(取自 EquipGem 表;行与属性 id 已由 RequireForgeRows 核对)。
uint64_t GemValue(uint32_t gemConfigId)
{
	const auto *row = GemRowOrNull(gemConfigId);
	if (row == nullptr)
	{
		ADD_FAILURE() << "EquipGem row " << gemConfigId << " is missing: " << kForgeExportHint;
		return 0;
	}
	return row->value();
}

// 某部位的 socket 号孔的定义行(EquipGemSocket 表);没有返回 nullptr。
const EquipGemSocketTable *SocketRowOrNull(uint32_t equipKind, uint32_t socket)
{
	for (const auto *row : EquipGemSocketTableManager::Instance().GetByEquipKind(equipKind))
	{
		if (row->socket() == socket)
		{
			return row;
		}
	}
	return nullptr;
}

// 内存里改一行表(照 player_equip_test.cpp 的做法)。夹具的 TearDown 会把改过的表重新加载,
// 所以改动不会漏进后面的用例。
template <typename Row>
Row &Editable(const Row *row)
{
	return *const_cast<Row *>(row);
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

// 把实例的强化等级写成 +level。直接写实例、不核对表上限:
// 「存档里的等级比表上限还高」正是其中一条用例要造的形状。
void SetEnhanceLevel(ItemComp &item, uint32_t level)
{
	item.mutable_equip()->set_enhance_level(level);
}

// 往实例上追加一条宝石记录(socket 号孔里嵌着 gemConfigId)。原始记录:不核对孔位与宝石是否存在、
// 也不去重 —— 入包边界整份信任 equip 段,脏记录的几条用例靠它造数据。
void AddGemRecord(ItemComp &item, uint32_t socket, uint32_t gemConfigId)
{
	auto *gem = item.mutable_equip()->add_gems();
	gem->set_socket(socket);
	gem->set_gem_config_id(gemConfigId);
}

// 衣服:气血 +300、法力 +200(当前气血 / 法力跟随上限的几条用例共用)。
ItemComp MakeVitalityClothes(Guid guid)
{
	ItemComp clothes = MakeEquip(guid, kClothes);
	AddAffix(clothes, kAttrHealth, kBlue, kHealthAffix, 0);
	AddAffix(clothes, kAttrMana, kBlue, kManaAffix, 1);
	return clothes;
}

// ── 进程级状态:item 号段、面板推送的探针 ─────────────────────────────────

// 与 player_equip_test.cpp / bag_instance_data_test.cpp 同一个做法:无 I/O 的假传输 + 直接投一段号。
bool ArmItemSegment(uint64_t lo, uint64_t hi)
{
	auto &client = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
	client.Reset();
	GuidSegmentClient::Options options;
	options.kindName = "item";
	options.initialStep = 100000;
	if (!client.Enable(options, [](const std::string &, uint32_t) { return true; },
					   [](GuidSegmentClient::TimerKind, double, std::function<void()>) {},
					   [] { return 0.0; }))
	{
		return false;
	}
	client.Warm();
	client.OnResponse(0, lo, hi);
	return client.IsReady();
}

// 夹具里的玩家没有会话组件(PlayerSessionSnapshotComp),PlayerAttributeSystem::PushPanel 走到
// SendMessageToClientViaGate 时留下一条 WARN 就返回 —— 本测试进程里没有 gate 连接可以收包,这条 WARN
// 是「推过一次面板」唯一看得见的痕迹。照 cross_zone_test.cpp 数 WARN 条数的做法:临时接管 muduo 的
// 日志出口,数这句原文出现了几次(muduo 的 WARN 不受日志级别开关影响)。原文在
// engine/core/network/client_message_sender.cpp;那边改了措辞,用到探针的用例会红在各自开头的校准断言上
// (直接推一次必须恰好数到 1),而不是把「没推」和「数不到」混为一谈。
const char *const kPanelPushTrace = "Player node info not found for player entity";
int gPanelPushTraces = 0;

void WriteLogToStdout(const char *msg, int len)
{
	std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void CountPanelPushTrace(const char *msg, int len)
{
	if (std::string_view(msg, static_cast<std::size_t>(len)).find(kPanelPushTrace) != std::string_view::npos)
	{
		++gPanelPushTraces;
	}
	WriteLogToStdout(msg, len);
}

// 作用域守卫:构造时接管日志出口并清零,析构时还给 stdout(用例中途 ASSERT 退出也会还)。
// muduo 不提供「取当前出口」的接口,它的默认出口同样是写 stdout,所以还成等价的这一个。
class PanelPushProbe
{
public:
	PanelPushProbe()
	{
		gPanelPushTraces = 0;
		muduo::Logger::setOutput(CountPanelPushTrace);
	}
	~PanelPushProbe() { muduo::Logger::setOutput(WriteLogToStdout); }
	PanelPushProbe(const PanelPushProbe &) = delete;
	PanelPushProbe &operator=(const PanelPushProbe &) = delete;

	int Count() const { return gPanelPushTraces; }
	void Reset() { gPanelPushTraces = 0; }
};

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
		// 二期(强化 / 镶嵌)的三张表:不加载 = 空表 = 强化未开放、没有宝石、没有孔。
		EquipEnhanceTableManager::Instance().Load();
		EquipGemTableManager::Instance().Load();
		EquipGemSocketTableManager::Instance().Load();
		// 存档往返那条用例走整条 PlayerAllData 装卸入口,任务还原要这两张表。
		MissionTableManager::Instance().Load();
		ConditionTableManager::Instance().Load();
		// 取下宝石走 BagService::AddItems 退回一颗,新堆要铸号。
		ASSERT_TRUE(ArmItemSegment(kItemSegmentLo, kItemSegmentHi));
		// 进程级状态:先复原一次,别的文件的用例若忘了复原也不会漏进来。
		PlayerEquipSystem::SetRandomSourceForTest({});
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());
		player = NewPlayer();
	}

	void TearDown() override
	{
		// 进程级状态:强化掷骰的脚本随机源必须复原,否则后面所有掷属性 / 强化的用例都在吃这份脚本。
		PlayerEquipSystem::SetRandomSourceForTest({});
		for (const auto entity : players)
		{
			if (tlsEcs.actorRegistry.valid(entity))
			{
				tlsEcs.actorRegistry.destroy(entity);
			}
		}
		// 个别用例在内存里改过表行(样例衣服的基础属性、EquipEnhance 的成功率与失败掉级):
		// 重新加载,别把改动留给后面的用例。
		ItemTableManager::Instance().Load();
		EquipEnhanceTableManager::Instance().Load();
		// 恢复其它背包用例使用的无 I/O 号段基线(与 bag_test.cpp 的 main() 同一段范围)。
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
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

	// ── 强化 / 镶嵌的前置条件 ────────────────────────────────────────────
	// 只由二期的用例调(各自的第一行),不放进 SetUp:一期的 20 条不依赖这三张表里有什么。

	// 宝石行:提供的是 attrId 这条属性、数值非 0,并且编排层认它是宝石(Item 行存在、可叠加、不是装备)。
	static void RequireGemRow(uint32_t gemConfigId, uint32_t attrId)
	{
		const auto *gem = GemRowOrNull(gemConfigId);
		ASSERT_NE(nullptr, gem) << "EquipGem row " << gemConfigId << " is missing: " << kForgeExportHint;
		ASSERT_EQ(attrId, gem->attr_id()) << "EquipGem row " << gemConfigId;
		ASSERT_GT(gem->gem_type(), 0u) << "EquipGem row " << gemConfigId;
		ASSERT_GT(gem->value(), 0u) << "EquipGem row " << gemConfigId;
		const auto *item = ItemTableManager::Instance().FindByIdSilent(gemConfigId).first;
		ASSERT_NE(nullptr, item) << "Item row " << gemConfigId << " is missing: " << kForgeExportHint;
		ASSERT_EQ(0u, item->equip_kind()) << "Item row " << gemConfigId;
		ASSERT_GT(item->max_stack_size(), 1u) << "Item row " << gemConfigId;
		ASSERT_TRUE(PlayerEquipSystem::IsGem(gemConfigId)) << "config " << gemConfigId;
	}

	// 该部位有 socket 号孔,且它收得下 gemConfigId 这种宝石(孔位类型 0 = 任意)。
	static void RequireSocketAccepting(uint32_t equipKind, uint32_t socket, uint32_t gemConfigId)
	{
		const auto *gem = GemRowOrNull(gemConfigId);
		ASSERT_NE(nullptr, gem) << "EquipGem row " << gemConfigId << " is missing: " << kForgeExportHint;
		const auto *socketRow = SocketRowOrNull(equipKind, socket);
		ASSERT_NE(nullptr, socketRow) << "EquipGemSocket has no socket " << socket << " for equip_kind " << equipKind
									  << ": " << kForgeExportHint;
		ASSERT_TRUE(socketRow->gem_type() == 0 || socketRow->gem_type() == gem->gem_type())
			<< "socket " << socket << " of equip_kind " << equipKind << " no longer accepts gem " << gemConfigId;
	}

	static void RequireForgeRows()
	{
		ASSERT_GT(PlayerEquipSystem::MaxEnhanceLevel(), 0u)
			<< "EquipEnhance has no level 1 row (enhancement is closed): " << kForgeExportHint;
		const auto *firstLevel = EnhanceRowOrNull(1);
		ASSERT_NE(nullptr, firstLevel);
		// 用例靠脚本随机源掷 0 来「必成」;成功率为 0 的行会被强化入口直接拒掉。
		ASSERT_GT(firstLevel->success_rate(), 0u) << "EquipEnhance row 1";

		ASSERT_NO_FATAL_FAILURE(RequireGemRow(kGemAttack, kAttrDamage));
		ASSERT_NO_FATAL_FAILURE(RequireGemRow(kGemDefense, kAttrDefense));
		ASSERT_NO_FATAL_FAILURE(RequireGemRow(kGemHealth, kAttrHealth));
		ASSERT_NO_FATAL_FAILURE(RequireGemRow(kGemSpeed, kAttrSpeed));
		ASSERT_EQ(nullptr, GemRowOrNull(kUnknownGem));

		ASSERT_NO_FATAL_FAILURE(RequireSocketAccepting(kWeaponKind, kSocketFirst, kGemAttack));
		ASSERT_NO_FATAL_FAILURE(RequireSocketAccepting(kWeaponKind, kSocketSecond, kGemAttack));
		ASSERT_NO_FATAL_FAILURE(RequireSocketAccepting(kHatKind, kSocketFirst, kGemHealth));
		ASSERT_NO_FATAL_FAILURE(RequireSocketAccepting(kClothesKind, kSocketFirst, kGemDefense));
		ASSERT_NO_FATAL_FAILURE(RequireSocketAccepting(kShoesKind, kSocketFirst, kGemSpeed));
	}

	// 最小可重算玩家:基础属性、等级、职业和背包,走一遍加载期初始化。缺背包专供失败回归。
	entt::entity NewPlayer(bool withBags = true)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, kPlayerIdBase + players.size());
		auto &base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
		base.set_health(kFarAboveAnyMax);
		base.set_mana(kFarAboveAnyMax);
		base.set_critchance(kBaseCritChance);
		base.set_resistance(kBaseResistance);
		tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(kLevel);
		tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(entity);
		if (withBags)
		{
			tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		}
		// 强化要问余额、扣金币:余额在 CurrencyComp,欠款与脏标记在 PlayerCurrencyComp
		// (做法同 player_battle_settlement_test.cpp)。余额从 0 起,要强化的用例自己 FundEnhance。
		tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		// 与加载链路同序:存档组件由 Unmarshal 挂上(这里是一份空存档),InitializeOnLoad 只补默认方案、
		// 挂二级属性缓存并重算 —— 它不再替调用方补建 PlayerAttributeComp。
		tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(entity);
		PlayerAttributeSystem::InitializeOnLoad(entity);
		return entity;
	}

	// 只挂基础属性与等级、不走加载期初始化的「半成品」实体:用来验证读写入口缺必需组件时
	// 只取不建、fail-closed。登记进 players,由 TearDown 统一销毁。
	entt::entity NewUnloadedEntity()
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, kPlayerIdBase + players.size());
		auto &base = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
		base.set_health(kFarAboveAnyMax);
		base.set_mana(kFarAboveAnyMax);
		tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(kLevel);
		tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		return entity;
	}

	// 把夹具玩家补成「加载创建点走完」的形状,只给存档往返的用例用:生产里这些持久化组件由
	// PlayerDatabaseMessageFieldsUnmarshal 一次性挂上,存盘 Marshal 只取不建、缺一个就整份拒绝。
	// 已挂(带用例数据)的保持原样,只补缺的空组件,不影响属性计算的任何输入。
	template <typename Comp>
	static void EmplaceIfAbsent(entt::entity entity)
	{
		if (!tlsEcs.actorRegistry.all_of<Comp>(entity))
		{
			tlsEcs.actorRegistry.emplace<Comp>(entity);
		}
	}
	static void CompletePersistedComps(entt::entity entity)
	{
		EmplaceIfAbsent<Transform>(entity);
		EmplaceIfAbsent<PlayerUint64Comp>(entity);
		EmplaceIfAbsent<PlayerSkillListComp>(entity);
		EmplaceIfAbsent<PlayerUint32Comp>(entity);
		EmplaceIfAbsent<PlayerPetComp>(entity);
		EmplaceIfAbsent<PlayerProfileComp>(entity);
		EmplaceIfAbsent<MissionsContainerComp>(entity);
		EmplaceIfAbsent<PlayerAssetOpLedgerComp>(entity);
		EmplaceIfAbsent<BattleSettlementLedgerComp>(entity);
		EmplaceIfAbsent<PlayerInboxLedgerComp>(entity);
		EmplaceIfAbsent<PlayerMysteryMerchantComp>(entity);
		// 两表顶层未知字段的载体(混版本持久字段保护 §4.2 L1),生产里由两张表的 Unmarshal 创建。
		EmplaceIfAbsent<PlayerDatabaseCarryComp>(entity);
		EmplaceIfAbsent<PlayerDatabase1CarryComp>(entity);
	}

	template <typename Comp>
	static std::string ProtoBytesIfPresent(entt::entity entity)
	{
		const auto* comp = tlsEcs.actorRegistry.try_get<Comp>(entity);
		return comp != nullptr ? comp->SerializeAsString() : std::string{};
	}

	// 每次只摘一个真实输入,所有读写入口共用同一道完整性门。故意放一份超额分配并把当前 HP/MP 抬到远超上限:
	// 缺输入时任何入口都必须在扣费、改方案与 HP/MP 夹取之前拒绝,下面逐个组件比对「什么都没动」。
	// (输入齐全时重算也不改写这份超额分配,只按不生效处理,见 OverAllocatedPoolIsCarriedButIneffective...)
	template <typename MissingComp>
	void ExpectMissingInputRejected(const char* name)
	{
		SCOPED_TRACE(name);
		const auto entity = NewPlayer();
		SetGold(entity, 1000000000);
		auto& attribute = tlsEcs.actorRegistry.get<PlayerAttributeComp>(entity);
		ASSERT_GT(attribute.schemes_size(), 0);
		(*attribute.mutable_schemes(0)->mutable_allocated())[kDimStrength] = UINT32_MAX;
		Base(entity).set_health(kFarAboveAnyMax);
		Base(entity).set_mana(kFarAboveAnyMax);
		tlsEcs.actorRegistry.remove<MissingComp>(entity);
		const auto baseBefore = ProtoBytesIfPresent<BaseAttributesComp>(entity);
		const auto derivedBefore = ProtoBytesIfPresent<DerivedAttributesComp>(entity);
		const auto attributeBefore = ProtoBytesIfPresent<PlayerAttributeComp>(entity);
		const auto levelBefore = ProtoBytesIfPresent<LevelComp>(entity);
		const auto classBefore = ProtoBytesIfPresent<PlayerUint32Comp>(entity);
		const auto currencyBefore = ProtoBytesIfPresent<CurrencyComp>(entity);
		BagAllData bagsBefore;
		bag_marshal::Marshal(entity, bagsBefore);

		AttributePanelInfo panel;
		panel.set_level(99);
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::BuildPanel(entity, panel));
		EXPECT_EQ(0u, panel.ByteSizeLong());
		PlayerAttributeSystem::Recalculate(entity, RecalcReason::kLevelChanged);
		const auto poolId = PlayerPoolId();
		EXPECT_EQ(kServiceUnavailable,
			PlayerAttributeSystem::Allocate(entity, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, 1}}));
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::Reset(entity, poolId));
		std::map<uint32_t, uint32_t> suggested{{kDimStrength, 1}};
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::AutoAllocate(entity, poolId, suggested));
		EXPECT_TRUE(suggested.empty());
		uint32_t schemeId = 99;
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::CreateScheme(entity, "", schemeId));
		EXPECT_EQ(0u, schemeId);
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::SwitchScheme(entity, 2));
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::RenameScheme(entity, 1, "方案甲"));
		EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::GmSetLevel(entity, 10));

		EXPECT_FALSE(tlsEcs.actorRegistry.any_of<MissingComp>(entity));
		EXPECT_EQ(baseBefore, ProtoBytesIfPresent<BaseAttributesComp>(entity));
		EXPECT_EQ(derivedBefore, ProtoBytesIfPresent<DerivedAttributesComp>(entity));
		EXPECT_EQ(attributeBefore, ProtoBytesIfPresent<PlayerAttributeComp>(entity));
		EXPECT_EQ(levelBefore, ProtoBytesIfPresent<LevelComp>(entity));
		EXPECT_EQ(classBefore, ProtoBytesIfPresent<PlayerUint32Comp>(entity));
		EXPECT_EQ(currencyBefore, ProtoBytesIfPresent<CurrencyComp>(entity));
		BagAllData bagsAfter;
		bag_marshal::Marshal(entity, bagsAfter);
		EXPECT_EQ(bagsBefore.SerializeAsString(), bagsAfter.SerializeAsString());
	}

	static Bag &Equipment(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<PlayerBagsComp>(entity).bags[kEquipment];
	}
	static Bag &Inventory(entt::entity entity)
	{
		return tlsEcs.actorRegistry.get<PlayerBagsComp>(entity).bags[kInventory];
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

	// ── 存储加点生效判定的帮手(加点统一规则,attribute_allocation_rules.h)──────────────

	static const AttributePoolTable *PlayerPoolRow()
	{
		return AttributePoolTableManager::Instance().FindByIdSilent(PlayerPoolId()).first;
	}

	// 角色属性点池在 level 级的总点数,按表独立算一遍(不经被测代码):基础点 + 每级点数 × (等级 − 解锁等级 + 1)。
	static uint32_t ExpectedTotalAt(uint32_t level)
	{
		const auto *pool = PlayerPoolRow();
		if (pool == nullptr || pool->unlock_level() == 0 || level < pool->unlock_level())
		{
			return 0;
		}
		return pool->base_points() + pool->points_per_level() * (level - pool->unlock_level() + 1);
	}

	// 同职业、没穿装备、没有加点的对照玩家在 level 级时的六项:「这份加点没有生效」就是与它逐项相等。
	Six NoAllocationSixAt(uint32_t level)
	{
		const auto twin = NewPlayer();
		tlsEcs.actorRegistry.get<LevelComp>(twin).set_level(level);
		PlayerAttributeSystem::Recalculate(twin, RecalcReason::kLoad);
		return ReadSix(Derived(twin));
	}

	// 表里任意一个非角色池(宝宝池)的维度:它在角色方案里是「本版本归不到角色」的未知键。按表独立找,不经被测代码。
	static uint32_t AnyNonPlayerDimension()
	{
		for (const auto &dimension : AttributeDimensionTableManager::Instance().FindAll().data())
		{
			const auto *pool = AttributePoolTableManager::Instance().FindByIdSilent(dimension.pool_id()).first;
			if (pool != nullptr && pool->owner_type() != 0)
			{
				return dimension.id();
			}
		}
		return 0;
	}

	// 夹具玩家只有加载时补上的方案一,且已激活(InitializeOnLoadFillsTheDefaultSchemeAndCreatesTheDerivedCache)。
	static auto &FirstSchemeAllocation(entt::entity entity)
	{
		return *AttributeComp(entity).mutable_schemes(0)->mutable_allocated();
	}

	// ── 强化 / 镶嵌的帮手 ────────────────────────────────────────────────

	// 穿着的那件实例;找不到返回 nullptr。指针只在装备栏下一次增删之前有效:现取现用。
	static const ItemComp *Worn(entt::entity entity, Guid guid)
	{
		return Equipment(entity).GetItemCompByGuid(guid);
	}

	// 穿上 -> 读六项 -> 脱下:给「同一名玩家轮着穿几件手工构造的实例」的用例用。
	static void WearReadTakeOff(entt::entity entity, const ItemComp &item, Six &out)
	{
		ASSERT_NO_FATAL_FAILURE(Wear(entity, item));
		out = ReadSix(Derived(entity));
		ASSERT_NO_FATAL_FAILURE(TakeOff(entity, item.item_id()));
	}

	// 把金币余额直接摆成 amount(做法同 player_equip_test.cpp / player_feature_snapshot_test.cpp:写组件)。
	// 刻意不走 CurrencySystem::AddCurrency:那条路要过欠款抵扣、封禁闸与异常获得统计,而夹具要的只是
	// 「此刻恰好有这么多金币」;扣金币走不走正式入口是被测代码的事。
	static void SetGold(entt::entity entity, uint64_t amount)
	{
		auto &currency = tlsEcs.actorRegistry.get<CurrencyComp>(entity);
		currency.clear_values();
		for (uint32_t type = 0; type < kCurrencyMax; ++type)
		{
			currency.add_values(type == kCurrencyGold ? amount : 0);
		}
	}

	// 备齐「强化到 +targetLevel」这一次要的金币与材料,恰好够;数量现读 EquipEnhance 表(策划改消耗不必改用例)。
	// 材料按预设 guid 走还原入口放进人物背包(不经 BagService、不铸号)。这里只读消耗那几列,所以与
	// RigEnhanceRowToFail 谁先谁后都行。同一用例要强化多次时,每次给不同的 stackIndex(各占一堆)。
	static void FundEnhance(entt::entity entity, uint32_t targetLevel, uint32_t stackIndex = 0)
	{
		const auto *row = EnhanceRowOrNull(targetLevel);
		ASSERT_NE(nullptr, row) << "EquipEnhance row " << targetLevel << " is missing: " << kForgeExportHint;
		SetGold(entity, CurrencySystem::GetBalance(entity, kCurrencyGold) + row->cost_gold());
		if (row->material_id() == 0 || row->material_count() == 0)
		{
			return; // 这一级不要材料
		}
		const auto *material = ItemTableManager::Instance().FindByIdSilent(row->material_id()).first;
		ASSERT_NE(nullptr, material) << "EquipEnhance row " << targetLevel << " points at a missing material "
									 << row->material_id();
		ASSERT_GE(material->max_stack_size(), row->material_count())
			<< "one fixture stack must hold the materials of enhance level " << targetLevel;
		Inventory(entity).InsertItemForRestore(kGuidMaterialStack + stackIndex, row->material_id(),
											   row->material_count(), kMaterialPos + stackIndex);
		ASSERT_NE(nullptr, Inventory(entity).GetItemCompByGuid(kGuidMaterialStack + stackIndex))
			<< "the material stack for enhance level " << targetLevel << " did not land in the inventory";
	}

	// 往人物背包放一颗宝石(同上:预设 guid + 还原入口)。一个用例里放几种宝石时各给一个 stackIndex。
	static void StockGem(entt::entity entity, uint32_t gemConfigId, uint32_t stackIndex = 0)
	{
		Inventory(entity).InsertItemForRestore(kGuidGemStack + stackIndex, gemConfigId, 1, kGemPos + stackIndex);
		ASSERT_NE(nullptr, Inventory(entity).GetItemCompByGuid(kGuidGemStack + stackIndex))
			<< "gem " << gemConfigId << " did not land in the inventory";
	}

	// 把被测文件的掷骰换成「恒掷出 roll」(夹进 rand(n) 约定的 [0, n));TearDown 复原生产随机源。
	// 传 kRollAlwaysSucceeds / kRollFailsUnlessCertain。
	static void ScriptEnhanceRolls(uint64_t roll)
	{
		PlayerEquipSystem::SetRandomSourceForTest([roll](uint64_t n) { return roll < n ? roll : n - 1; });
	}

	// 把样例衣服的基础属性在内存里换成「气血 + 法力」,数值沿用它原来那条防御的表值。
	// 样例装备的基础属性只有伤害 / 防御 / 速度,强化放大不到气血上限;要验「强化加血装不回血」,
	// 得有一件基础属性是气血的装备。TearDown 会重新加载 Item 表。
	static void GiveClothesVitalityBaseAttributes()
	{
		const auto *row = ItemTableManager::Instance().FindByIdSilent(kClothes).first;
		ASSERT_NE(nullptr, row) << "Item row " << kClothes << " is missing: " << kExportHint;
		ASSERT_EQ(1, row->base_attr_size()) << "Item row " << kClothes;
		const auto value = row->base_attr(0).base_attr_value();
		auto &editable = Editable(row);
		editable.mutable_base_attr(0)->set_base_attr_id(kAttrHealth);
		auto *mana = editable.add_base_attr();
		mana->set_base_attr_id(kAttrMana);
		mana->set_base_attr_value(value);
	}

	// 在内存里把「强化到 +targetLevel」这一行改成万分之一的成功率(配合 kRollFailsUnlessCertain 就是确定的失败;
	// 不能填 0:必败的行会被强化入口直接拒掉、不收费),并指定失败掉几级。出厂表全是不掉级
	// (设计 §1.4 E3),掉级分支只有改表才走得到。TearDown 会重新加载 EquipEnhance 表。
	static void RigEnhanceRowToFail(uint32_t targetLevel, uint32_t failLevelLoss)
	{
		const auto *row = EnhanceRowOrNull(targetLevel);
		ASSERT_NE(nullptr, row) << "EquipEnhance row " << targetLevel << " is missing: " << kForgeExportHint;
		auto &editable = Editable(row);
		editable.set_success_rate(1);
		editable.set_fail_level_loss(failLevelLoss);
	}

	// 探针校准:直接推一次面板必须恰好数到 1(见 PanelPushProbe 的说明),然后清零。
	static void CalibrateProbe(PanelPushProbe &probe, entt::entity entity)
	{
		probe.Reset();
		PlayerAttributeSystem::PushPanel(entity);
		ASSERT_EQ(1, probe.Count())
			<< "a direct PushPanel did not leave exactly one countable trace: the WARN text in "
			   "client_message_sender.cpp changed, or the fixture player gained a session component";
		probe.Reset();
	}

	std::vector<entt::entity> players;
	entt::entity player{entt::null};
};

// ── 二级属性:六项的增量逐项等于手算值 ────────────────────────────────────

// 完整玩家的空装备栏按无装备基线计算;缺背包表示加载故障,不能冒充空装备继续计算。
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

	// 加载创建了派生缓存,但缺背包的玩家不能重算,HP/MP 与缓存均保留原值。
	const auto withoutBags = NewPlayer(/*withBags=*/false);
	ASSERT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(withoutBags));
	EXPECT_EQ(kFarAboveAnyMax, Base(withoutBags).health());
	EXPECT_EQ(0u, Derived(withoutBags).max_health());
	const auto baseBefore = Base(withoutBags).SerializeAsString();
	const auto derivedBefore = Derived(withoutBags).SerializeAsString();

	// 读和重算都拒绝缺失输入,不能补建一张空背包把加载故障藏起来。
	PlayerAttributeSystem::Recalculate(withoutBags, RecalcReason::kEquipmentChanged);
	AttributePanelInfo panel;
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::BuildPanel(withoutBags, panel));
	EXPECT_EQ(0u, panel.ByteSizeLong());
	EXPECT_EQ(baseBefore, Base(withoutBags).SerializeAsString());
	EXPECT_EQ(derivedBefore, Derived(withoutBags).SerializeAsString());
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

// ── 存储加点的生效判定(mixed-version-persist-field-protection.md §12.1 D1 / D2;
//    player-attribute-allocation.md §3.3):加载与重算不改写已分配,只决定它在本版本生不生效 ──────────

// 等级降低(GM / 回档)或改表缩点后,已分配可能超过本版本总量:存储值原样携带,整池按 0 生效。
// 以前 kLoad / kLevelChanged 会把它整池清零返还(ConvergeOverAllocation),回滚一次就删、升回也恢复不了;
// 现在任何原因的重算都不改写它,面板照实显示存储值、value 与六项只算生效部分,等级回来了同一份加点原样生效。
TEST_F(PlayerEquipAttributeTest, OverAllocatedPoolIsCarriedButIneffectiveOnEveryRecalculation)
{
	constexpr uint32_t kAllocated = 100;
	constexpr uint32_t kLoweredLevel = 10;
	const uint32_t poolId = PlayerPoolId();
	ASSERT_LT(ExpectedTotalAt(kLoweredLevel), kAllocated) << "the lowered level must leave fewer points than were allocated";
	const Six loweredBaseline = NoAllocationSixAt(kLoweredLevel);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));
	const Six allocatedSix = ReadSix(Derived(player));
	const PlayerAttributeComp stored = AttributeComp(player);

	// 绕开升级事件直接压低等级,造出「已分配超过总量」的状态。
	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLoweredLevel);
	for (const auto reason : {RecalcReason::kLoad, RecalcReason::kLevelChanged, RecalcReason::kAllocate,
							  RecalcReason::kSchemeSwitch, RecalcReason::kReset, RecalcReason::kEquipmentChanged,
							  RecalcReason::kSkillChanged})
	{
		SCOPED_TRACE(static_cast<int>(reason));
		PlayerAttributeSystem::Recalculate(player, reason);
		EXPECT_TRUE(MessageDifferencer::Equals(stored, AttributeComp(player)))
			<< "no recalculation may rewrite or refund the stored allocation";
		ExpectSix(loweredBaseline, ReadSix(Derived(player)));

		AttributePanelInfo panel;
		ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
		const auto *pool = FindPool(panel, poolId);
		ASSERT_NE(nullptr, pool);
		EXPECT_EQ(ExpectedTotalAt(kLoweredLevel), pool->total());
		EXPECT_EQ(0u, pool->remaining()) << "an ineffective pool has nothing left to allocate";
		const auto *strength = FindDimension(panel, kDimStrength);
		ASSERT_NE(nullptr, strength);
		EXPECT_EQ(kAllocated, strength->allocated()) << "the panel still shows the stored allocation";
		EXPECT_EQ(NaturalPointsAt(kDimStrength, kLoweredLevel), strength->value())
			<< "but the ineffective points add nothing to the value";
	}

	// 等级回来了,同一份加点原样生效。
	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLevel);
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLevelChanged);
	ExpectSix(allocatedSix, ReadSix(Derived(player)));
	EXPECT_TRUE(MessageDifferencer::Equals(stored, AttributeComp(player)));
}

// 新版本加的维度(本版本表里没有)和本版本归不到角色的键(宝宝维度)都是未知键:走真实的存档往返仍原样携带,
// 永不生效 —— 六项等于只有已知加点的那一份。以前 SanitizeSchemes 在加载时把查不到表的维度删掉(D1)。
TEST_F(PlayerEquipAttributeTest, UnknownDimensionSurvivesLoadAndIsNotEffective)
{
	constexpr uint32_t kKnown = 50;
	constexpr uint32_t kFuturePoints = 30;
	constexpr uint32_t kForeignPoints = 20;
	const uint32_t foreignDimension = AnyNonPlayerDimension();
	ASSERT_NE(0u, foreignDimension) << "the table has no dimension outside the player pools";
	ASSERT_EQ(nullptr, AttributeDimensionTableManager::Instance().FindByIdSilent(kFutureDimension).first);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, PlayerPoolId(), std::map<uint32_t, uint32_t>{{kDimStrength, kKnown}}));
	const Six knownOnly = ReadSix(Derived(player));

	FirstSchemeAllocation(player)[kFutureDimension] = kFuturePoints;
	FirstSchemeAllocation(player)[foreignDimension] = kForeignPoints;
	const PlayerAttributeComp stored = AttributeComp(player);
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);
	EXPECT_TRUE(MessageDifferencer::Equals(stored, AttributeComp(player)));
	ExpectSix(knownOnly, ReadSix(Derived(player)));

	// 真实的存档往返:存盘 → 冷读 → 加载链路(PlayerDatabaseMessageFieldsUnmarshal → InitializeOnLoad)。
	CompletePersistedComps(player);
	PlayerAllData saved;
	ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(player, saved));
	std::string bytes;
	ASSERT_TRUE(saved.player_database_data().SerializeToString(&bytes));
	PlayerAllData coldSnapshot;
	ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));
	const auto restored = tlsEcs.actorRegistry.create();
	players.push_back(restored);
	tlsEcs.actorRegistry.emplace<PlayerGuidComp>(restored, kPlayerIdBase + players.size());
	PlayerAllDataMessageFieldsUnmarshal(restored, coldSnapshot);

	const auto *restoredComp = tlsEcs.actorRegistry.try_get<PlayerAttributeComp>(restored);
	ASSERT_NE(nullptr, restoredComp);
	EXPECT_TRUE(MessageDifferencer::Equals(stored, *restoredComp)) << "unknown keys must survive the load path untouched";
	const auto *derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(restored);
	ASSERT_NE(nullptr, derived);
	ExpectSix(knownOnly, ReadSix(*derived));

	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(restored, panel));
	EXPECT_EQ(nullptr, FindDimension(panel, kFutureDimension)) << "the panel only lists dimensions this version knows";
	EXPECT_EQ(nullptr, FindDimension(panel, foreignDimension));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(panel, kDimStrength, kKnown, 0));
}

// 未知键计入已用:剩余点与加点上限都扣掉它;洗点按表收费、只清本池已知维度,未知键原样留下,
// 洗完的剩余点 = 总量 − 未知键。
TEST_F(PlayerEquipAttributeTest, UnknownDimensionReducesRemainingAndSurvivesReset)
{
	constexpr uint32_t kKnown = 50;
	constexpr uint32_t kFuturePoints = 30;
	constexpr uint64_t kGold = 100000;
	const uint32_t poolId = PlayerPoolId();
	const uint32_t total = ExpectedTotalAt(kLevel);
	ASSERT_GT(total, kKnown + kFuturePoints);
	const auto *poolRow = PlayerPoolRow();
	ASSERT_NE(nullptr, poolRow);
	ASSERT_GT(poolRow->reset_cost_gold(), 0u);
	ASSERT_LE(poolRow->reset_free_below_level(), kLevel) << "the fixture level must be past the free-reset threshold";
	ASSERT_GT(kGold, poolRow->reset_cost_gold());
	ASSERT_EQ(nullptr, AttributeDimensionTableManager::Instance().FindByIdSilent(kFutureDimension).first);

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, kKnown}}));
	FirstSchemeAllocation(player)[kFutureDimension] = kFuturePoints;
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);

	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	const auto *pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	const uint32_t remaining = total - kKnown - kFuturePoints;
	EXPECT_EQ(total, pool->total());
	EXPECT_EQ(remaining, pool->remaining()) << "points parked on an unknown dimension stay spent";

	// 加点上限同样扣掉未知键:多 1 点拒绝且什么都不动,恰好用完放行。
	const PlayerAttributeComp beforeRejected = AttributeComp(player);
	EXPECT_EQ(kAttributePointsNotEnough,
			  PlayerAttributeSystem::Allocate(player, poolId,
											  std::map<uint32_t, uint32_t>{{kDimStrength, kKnown + remaining + 1}}));
	EXPECT_TRUE(MessageDifferencer::Equals(beforeRejected, AttributeComp(player)));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId,
														 std::map<uint32_t, uint32_t>{{kDimStrength, kKnown + remaining}}));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(0u, pool->remaining());

	// 洗点:按表收费,只清本池已知维度,未知键原样留下。
	SetGold(player, kGold);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Reset(player, poolId));
	EXPECT_EQ(kGold - poolRow->reset_cost_gold(), CurrencySystem::GetBalance(player, kCurrencyGold));
	const auto &allocated = AttributeComp(player).schemes(0).allocated();
	EXPECT_FALSE(allocated.contains(kDimStrength));
	ASSERT_TRUE(allocated.contains(kFutureDimension)) << "a reset must not touch keys this version cannot place in the pool";
	EXPECT_EQ(kFuturePoints, allocated.at(kFutureDimension));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(total - kFuturePoints, pool->remaining());
}

// 0 值的未知键:不占点、不生效、不算「可洗」,面板与六项和没有它时一模一样;它自己原样留着。
// 顺带钉住「加点只写真正变化的键」:目标里带着 0 的已知维度不会在存档里落一个 0 值新键。
TEST_F(PlayerEquipAttributeTest, ZeroValueUnknownKeyChangesNothing)
{
	constexpr uint32_t kStrengthPoints = 10;
	constexpr uint64_t kGold = 100000;
	const uint32_t poolId = PlayerPoolId();
	AttributePanelInfo before;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, before));
	const Six sixBefore = ReadSix(Derived(player));

	FirstSchemeAllocation(player)[kFutureDimension] = 0;
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);
	AttributePanelInfo after;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, after));
	EXPECT_TRUE(MessageDifferencer::Equals(before, after)) << "a zero-valued unknown key must not show anywhere on the panel";
	ExpectSix(sixBefore, ReadSix(Derived(player)));

	// 方案里只有它一个键:洗点是「无可洗」,不扣费,它也还在。
	SetGold(player, kGold);
	EXPECT_EQ(kAttributeNothingToChange, PlayerAttributeSystem::Reset(player, poolId));
	EXPECT_EQ(kGold, CurrencySystem::GetBalance(player, kCurrencyGold));
	EXPECT_TRUE(AttributeComp(player).schemes(0).allocated().contains(kFutureDimension));

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId,
														 std::map<uint32_t, uint32_t>{{kDimStrength, kStrengthPoints},
																					  {kDimConstitution, 0},
																					  {kDimSpirit, 0}}));
	const auto &allocated = AttributeComp(player).schemes(0).allocated();
	EXPECT_EQ(2u, allocated.size()) << "only the changed key is written next to the carried unknown key";
	EXPECT_EQ(kStrengthPoints, allocated.at(kDimStrength));
	EXPECT_FALSE(allocated.contains(kDimConstitution));
	EXPECT_FALSE(allocated.contains(kDimSpirit));
	ASSERT_TRUE(allocated.contains(kFutureDimension));
	EXPECT_EQ(0u, allocated.at(kFutureDimension));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, after));
	const auto *pool = FindPool(after, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(ExpectedTotalAt(kLevel) - kStrengthPoints, pool->remaining());
}

// GM 降级再设回原等级(回档 / 混版本同理):降级时超额的池按 0 生效、存储加点一点不动;
// 设回原等级,同一份加点原样生效。以前降级时整池清零返还(D2),设回去也回不来。
TEST_F(PlayerEquipAttributeTest, LoweringAndRestoringTheLevelKeepsTheAllocation)
{
	constexpr uint32_t kAllocated = 300;
	constexpr uint32_t kLoweredLevel = 10;
	ASSERT_TRUE(playerlevel::IsValidLevel(kLoweredLevel));
	ASSERT_LT(ExpectedTotalAt(kLoweredLevel), kAllocated);
	ASSERT_GE(ExpectedTotalAt(kLevel), kAllocated);
	// GmSetLevel 会顺带重算宝宝并推任务事件(同 GmSetLevelRecalculatesDerivedAttributesWithoutEventWiring)。
	tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
	ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
	SetGold(player, 1000000);
	const Six loweredBaseline = NoAllocationSixAt(kLoweredLevel);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, PlayerPoolId(), std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));
	const Six allocatedSix = ReadSix(Derived(player));
	const PlayerAttributeComp stored = AttributeComp(player);

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::GmSetLevel(player, kLoweredLevel));
	EXPECT_TRUE(MessageDifferencer::Equals(stored, AttributeComp(player))) << "lowering the level must not refund the allocation";
	ExpectSix(loweredBaseline, ReadSix(Derived(player)));

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::GmSetLevel(player, kLevel));
	EXPECT_TRUE(MessageDifferencer::Equals(stored, AttributeComp(player)));
	ExpectSix(allocatedSix, ReadSix(Derived(player)));
	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	ASSERT_NO_FATAL_FAILURE(ExpectDimension(panel, kDimStrength, kAllocated, 0));
}

// 不生效的池洗点免费(不生效来自改表缩点 / 降级 / 混版本,不是玩家的操作),面板下发的费用同样是 0;
// 旧上限 / 新上限都是生效值,洗点前后不变,当前气血 / 法力也就不变 —— 不会先按存储值抬一次再压回来。
TEST_F(PlayerEquipAttributeTest, ResetOfAnIneffectivePoolIsFreeAndKeepsHealth)
{
	constexpr uint32_t kStrengthPoints = 150;
	constexpr uint32_t kConstitutionPoints = 150;  // 体质进气血上限:「上限没变」才有意义
	constexpr uint32_t kLoweredLevel = 40;
	constexpr uint64_t kGold = 100000;
	const uint32_t poolId = PlayerPoolId();
	const auto *poolRow = PlayerPoolRow();
	ASSERT_NE(nullptr, poolRow);
	ASSERT_GT(poolRow->reset_cost_gold(), 0u);
	ASSERT_LE(poolRow->reset_free_below_level(), kLoweredLevel) << "an effective reset at the lowered level must cost gold";
	ASSERT_LT(ExpectedTotalAt(kLoweredLevel), kStrengthPoints + kConstitutionPoints);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId,
														 std::map<uint32_t, uint32_t>{{kDimStrength, kStrengthPoints},
																					  {kDimConstitution, kConstitutionPoints}}));

	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kLoweredLevel);
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLevelChanged);
	const Six ineffective = ReadSix(Derived(player));
	ExpectSix(NoAllocationSixAt(kLoweredLevel), ineffective);
	const uint64_t health = Derived(player).max_health() / 2;
	const uint64_t mana = Derived(player).max_mana() / 3;
	ASSERT_GT(health, 0u);
	ASSERT_GT(mana, 0u);
	Base(player).set_health(health);
	Base(player).set_mana(mana);

	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	const auto *pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(0u, pool->reset_cost_gold()) << "the panel quotes the same price the reset will charge";
	EXPECT_EQ(0u, pool->remaining());

	SetGold(player, kGold);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Reset(player, poolId));
	EXPECT_EQ(kGold, CurrencySystem::GetBalance(player, kCurrencyGold)) << "resetting an ineffective pool is free";
	EXPECT_TRUE(AttributeComp(player).schemes(0).allocated().empty());
	ExpectSix(ineffective, ReadSix(Derived(player)));
	EXPECT_EQ(health, Base(player).health()) << "the effective maximum did not move, so neither does the health";
	EXPECT_EQ(mana, Base(player).mana());

	// 洗完池就生效了(一点没加):面板恢复按表报价。
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(poolRow->reset_cost_gold(), pool->reset_cost_gold());
	EXPECT_EQ(ExpectedTotalAt(kLoweredLevel), pool->remaining());
}

// 方案之间互不牵连:切到一个超额的方案(另一版本存下的 / 降级前加满的),它照样不生效、存储值不动,
// 在它上面加点回「点数不足」;切回原方案,原方案的加点原样生效。
TEST_F(PlayerEquipAttributeTest, SwitchingToAnOverAllocatedSchemeKeepsItIneffective)
{
	constexpr uint32_t kFirstSchemePoints = 100;
	constexpr uint32_t kSecondSchemeId = 2;
	const uint32_t poolId = PlayerPoolId();
	const uint32_t total = ExpectedTotalAt(kLevel);
	const Six noAllocation = ReadSix(Derived(player));
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, poolId,
														 std::map<uint32_t, uint32_t>{{kDimStrength, kFirstSchemePoints}}));
	const Six firstSchemeSix = ReadSix(Derived(player));

	// 直接往存档里塞第二个方案:已知维度合计超过本版本总量。
	auto &comp = AttributeComp(player);
	const uint32_t firstSchemeId = comp.active_scheme_id();
	ASSERT_NE(kSecondSchemeId, firstSchemeId);
	auto *second = comp.add_schemes();
	second->set_scheme_id(kSecondSchemeId);
	second->set_name("方案二");
	(*second->mutable_allocated())[kDimConstitution] = total + 1;
	comp.set_next_scheme_id(kSecondSchemeId + 1);
	comp.set_last_switch_time(0);  // 越过切换冷却
	const AttributeScheme secondStored = *second;

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::SwitchScheme(player, kSecondSchemeId));
	ExpectSix(noAllocation, ReadSix(Derived(player)));
	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	const auto *pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(0u, pool->remaining());
	EXPECT_EQ(0u, pool->reset_cost_gold());
	const auto *constitution = FindDimension(panel, kDimConstitution);
	ASSERT_NE(nullptr, constitution);
	EXPECT_EQ(total + 1, constitution->allocated());
	EXPECT_EQ(NaturalPoints(kDimConstitution), constitution->value());
	EXPECT_EQ(kAttributePointsNotEnough,
			  PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, 1}}));
	ASSERT_EQ(2, AttributeComp(player).schemes_size());
	EXPECT_TRUE(MessageDifferencer::Equals(secondStored, AttributeComp(player).schemes(1)));

	// 切回:SwitchScheme 按当前时间记过一次冷却,手动拨回去。
	AttributeComp(player).set_last_switch_time(0);
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::SwitchScheme(player, firstSchemeId));
	ExpectSix(firstSchemeSix, ReadSix(Derived(player)));
	EXPECT_TRUE(MessageDifferencer::Equals(secondStored, AttributeComp(player).schemes(1)));
}

// 超上限的存储等级(新版本提高上限后回滚 / 混版本跨 zone 带进来)原样携带,玩法按上限读:面板等级、总点数、
// 自然成长与六项都等于满级号;重算(含加载)不改写 LevelComp,加点也按上限算出的总量封顶。
TEST_F(PlayerEquipAttributeTest, StoredLevelAboveCapUsesCappedLevelForTotals)
{
	constexpr uint32_t kStoredLevel = 200;
	ASSERT_GT(kStoredLevel, playerlevel::kMaxLevel);
	const uint32_t poolId = PlayerPoolId();
	const uint32_t totalAtCap = ExpectedTotalAt(playerlevel::kMaxLevel);
	const Six atCap = NoAllocationSixAt(playerlevel::kMaxLevel);

	tlsEcs.actorRegistry.get<LevelComp>(player).set_level(kStoredLevel);
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLoad);
	EXPECT_EQ(kStoredLevel, tlsEcs.actorRegistry.get<LevelComp>(player).level()) << "the stored level is carried as-is";
	ExpectSix(atCap, ReadSix(Derived(player)));

	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	EXPECT_EQ(playerlevel::kMaxLevel, panel.level());
	const auto *pool = FindPool(panel, poolId);
	ASSERT_NE(nullptr, pool);
	EXPECT_EQ(totalAtCap, pool->total());
	EXPECT_EQ(totalAtCap, pool->remaining());
	for (const auto dimensionId : kPlayerDimensions)
	{
		const auto *dimension = FindDimension(panel, dimensionId);
		ASSERT_NE(nullptr, dimension) << "dimension " << dimensionId;
		EXPECT_EQ(NaturalPointsAt(dimensionId, playerlevel::kMaxLevel), dimension->value()) << "dimension " << dimensionId;
	}

	EXPECT_EQ(kAttributePointsNotEnough,
			  PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, totalAtCap + 1}}));
	EXPECT_EQ(kSuccess,
			  PlayerAttributeSystem::Allocate(player, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, totalAtCap}}));
	EXPECT_EQ(kStoredLevel, tlsEcs.actorRegistry.get<LevelComp>(player).level());
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

	// 存盘只认走完加载创建点的实体(缺任何持久化组件就整份拒绝、什么都不写):先把夹具没挂的补成空存档。
	CompletePersistedComps(player);
	PlayerAllData saved;
	ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(player, saved));
	std::string bytes;
	ASSERT_TRUE(saved.player_database_data().SerializeToString(&bytes));
	PlayerAllData coldSnapshot;
	ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));

	const auto restored = tlsEcs.actorRegistry.create();
	players.push_back(restored);
	tlsEcs.actorRegistry.emplace<PlayerGuidComp>(restored, kPlayerIdBase + players.size());
	PlayerAllDataMessageFieldsUnmarshal(restored, coldSnapshot);

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

// 旧快照只有顶层背包时，也必须先恢复装备再重算派生属性，保留当前气血与法力。
TEST_F(PlayerEquipAttributeTest, LegacyTopLevelBagReloadRecomputesTheEquipmentBonus)
{
	ASSERT_NO_FATAL_FAILURE(WearCombatSet(player));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	ItemComp clothes = MakeVitalityClothes(kGuidClothes);
	AddAffix(clothes, kAttrAllPrimary, kBlue, 8, 2);
	ASSERT_NO_FATAL_FAILURE(Wear(player, clothes));
	ASSERT_EQ(Derived(player).max_health(), Base(player).health());
	ASSERT_GT(Derived(player).max_health(), NoGearBaseline().maxHealth);

	CompletePersistedComps(player);
	PlayerAllData saved;
	ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(player, saved));
	ASSERT_TRUE(saved.has_bag_data());
	saved.mutable_player_database_data()->clear_bag_component();
	PlayerAllData parsed;
	ASSERT_TRUE(parsed.ParseFromString(saved.SerializeAsString()));
	ASSERT_FALSE(parsed.player_database_data().has_bag_component());

	const auto restored = tlsEcs.actorRegistry.create();
	players.push_back(restored);
	tlsEcs.actorRegistry.emplace<PlayerGuidComp>(restored, kPlayerIdBase + players.size());
	PlayerAllDataMessageFieldsUnmarshal(restored, parsed);

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
	EXPECT_EQ(Base(player).health(), base->health());
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

// ═════════════════════════════════════════════════════════════════════════
// 二期:强化与镶嵌接进属性(docs/design/equipment-enhance-inlay.md §4.3 消费侧 / §5)
// ═════════════════════════════════════════════════════════════════════════

// ── 强化:实例上写着 +N,基础属性按该级的万分比放大 ───────────────────────

// 武器的每一级:物伤 / 法伤的增量 = 表里的伤害按该级总加成放大后的值。+0 就是表值(与一期逐位一致)。
TEST_F(PlayerEquipAttributeTest, EnhancedWeaponAddsItsScaledBaseDamageAtEveryLevel)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six before = ReadSix(Derived(player));
	const uint64_t tableDamage = BaseAttrValue(kWeapon, kAttrDamage);
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();

	uint64_t previousDamage = 0;
	for (uint32_t level = 0; level <= maxLevel; ++level)
	{
		SCOPED_TRACE("enhance level " + std::to_string(level));
		ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
		SetEnhanceLevel(weapon, level);
		ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));

		const uint64_t damage = EnhancedBaseAttr(kWeapon, kAttrDamage, level);
		Six expected = before;
		expected.physicalAttack += damage;
		expected.magicAttack += damage;
		ExpectSix(expected, ReadSix(Derived(player)));

		// 同一个数再手算一遍:表值 + floor(表值 x 万分比 / 10000)(样例数值离溢出线很远,直接乘)。
		EXPECT_EQ(tableDamage + tableDamage * EnhancePermyriad(level) / equiprules::kPermyriadBase, damage);
		if (level == 0)
		{
			EXPECT_EQ(tableDamage, damage) << "+0 must be exactly the table value";
		}
		EXPECT_GE(damage, previousDamage) << "a higher enhance level never gives less";
		previousDamage = damage;

		ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidWeapon));
	}
	EXPECT_GT(previousDamage, tableDamage)
		<< "the top level adds nothing to this weapon; every equality above would hold without enhancement";
	ExpectSix(before, ReadSix(Derived(player)));
}

// 防具:每件按自己的等级放大自己的每一条基础属性(鞋子的防御与速度各放大各的),三件互不串级。
TEST_F(PlayerEquipAttributeTest, EnhancedArmorScalesEachBaseAttributeAtItsOwnLevel)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six before = ReadSix(Derived(player));
	const uint64_t armorBefore = Base(player).armor();
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	// 三件取三个等级(表只有一级时退化成同一级,等式仍成立):哪一件的等级被套到了别的件上,和就对不上。
	const uint32_t hatLevel = maxLevel;
	const uint32_t clothesLevel = (maxLevel + 1) / 2;
	const uint32_t shoesLevel = 1;

	ItemComp hat = MakeEquip(kGuidHat, kHat);
	SetEnhanceLevel(hat, hatLevel);
	ItemComp clothes = MakeEquip(kGuidClothes, kClothes);
	SetEnhanceLevel(clothes, clothesLevel);
	ItemComp shoes = MakeEquip(kGuidShoes, kShoes);
	SetEnhanceLevel(shoes, shoesLevel);
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));
	ASSERT_NO_FATAL_FAILURE(Wear(player, clothes));
	ASSERT_NO_FATAL_FAILURE(Wear(player, shoes));

	Six expected = before;
	expected.defense += EnhancedBaseAttr(kHat, kAttrDefense, hatLevel) +
						EnhancedBaseAttr(kClothes, kAttrDefense, clothesLevel) +
						EnhancedBaseAttr(kShoes, kAttrDefense, shoesLevel);
	expected.speed += EnhancedBaseAttr(kShoes, kAttrSpeed, shoesLevel);
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(expected.speed, Base(player).speed()) << "the enhanced speed must reach turn order as well";
	EXPECT_EQ(armorBefore, Base(player).armor()) << "enhanced defense still is not armor";

	// 确实放大了:三件未强化时的防御合计比这个数小。
	const uint64_t unenhancedDefense = BaseAttrValue(kHat, kAttrDefense) + BaseAttrValue(kClothes, kAttrDefense) +
									   BaseAttrValue(kShoes, kAttrDefense);
	EXPECT_GT(expected.defense, before.defense + unenhancedDefense)
		<< "no armor piece gained anything from its enhance level; the equality above is vacuous";
}

// 存档里的等级超过表上限(策划下调了上限):按上限计,存档原值不动 —— 表补回来即恢复(§5 不变量 2)。
TEST_F(PlayerEquipAttributeTest, EnhanceLevelBeyondTheTableCountsAsTheTopLevelWithoutRewritingTheInstance)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six before = ReadSix(Derived(player));
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	const uint64_t topDamage = EnhancedBaseAttr(kWeapon, kAttrDamage, maxLevel);
	Six expected = before;
	expected.physicalAttack += topDamage;
	expected.magicAttack += topDamage;

	for (const uint32_t storedLevel : {maxLevel + 1u, (std::numeric_limits<uint32_t>::max)()})
	{
		SCOPED_TRACE("stored enhance level " + std::to_string(storedLevel));
		ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
		SetEnhanceLevel(weapon, storedLevel);
		ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));

		ExpectSix(expected, ReadSix(Derived(player)));
		const ItemComp *worn = Worn(player, kGuidWeapon);
		ASSERT_NE(nullptr, worn);
		EXPECT_EQ(storedLevel, worn->equip().enhance_level())
			<< "recalculating clamps the level it reads; it must not rewrite the stored one";

		ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidWeapon));
	}
}

// 强化只放大基础属性(设计 §1.4 E1):同一件上的随机属性与宝石按原值计。
TEST_F(PlayerEquipAttributeTest, EnhancementScalesBaseAttributesOnlyNotAffixesOrGems)
{
	constexpr uint32_t kAccuracy = 500;
	constexpr uint32_t kDamageAffix = 400;
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six before = ReadSix(Derived(player));
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	const uint64_t enhancedBase = EnhancedBaseAttr(kWeapon, kAttrDamage, maxLevel);
	ASSERT_GT(enhancedBase, BaseAttrValue(kWeapon, kAttrDamage))
		<< "the top level adds nothing to this weapon; nothing here could tell scaled from unscaled";
	const uint64_t gemValue = GemValue(kGemAttack);

	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	AddAffix(weapon, kAttrAccuracy, kBlue, kAccuracy, 0);
	AddAffix(weapon, kAttrDamage, kBlue, kDamageAffix, 1);
	AddGemRecord(weapon, kSocketFirst, kGemAttack);
	SetEnhanceLevel(weapon, maxLevel);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));

	// 放大的只有 enhancedBase 这一项;「伤害」随机属性与伤害宝石和它是同一条属性 id,也不跟着放大。
	Six expected = before;
	expected.physicalAttack += enhancedBase + kDamageAffix + kAccuracy + gemValue;
	expected.magicAttack += enhancedBase + kDamageAffix + gemValue;
	ExpectSix(expected, ReadSix(Derived(player)));
}

// ── 强化:穿着 / 在背包里时调 PlayerEquipSystem::Enhance ───────────────────

// 穿着的武器强化成功:不必有人再调重算,六项当场就是 +1 的值,并且推了一次面板。
TEST_F(PlayerEquipAttributeTest, EnhancingWornGearUpdatesTheSixAtOnceAndPushesThePanel)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const uint64_t plain = BaseAttrValue(kWeapon, kAttrDamage);
	const uint64_t enhanced = EnhancedBaseAttr(kWeapon, kAttrDamage, 1);
	ASSERT_GT(enhanced, plain) << "enhance level 1 adds nothing to this weapon; pick a sample that moves";

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidWeapon, kWeapon)));
	const Six wornPlain = ReadSix(Derived(player));
	const uint64_t healthBefore = Base(player).health();
	const uint64_t manaBefore = Base(player).mana();
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 1));
	ScriptEnhanceRolls(kRollAlwaysSucceeds);
	PanelPushProbe probe;
	ASSERT_NO_FATAL_FAILURE(CalibrateProbe(probe, player));

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidWeapon, outcome));
	EXPECT_TRUE(outcome.success);
	EXPECT_EQ(1u, outcome.level);

	Six expected = wornPlain;
	expected.physicalAttack += enhanced - plain;
	expected.magicAttack += enhanced - plain;
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(1, probe.Count()) << "enhancing worn gear pushes the attribute panel exactly once";

	// 推出去的面板(同一个 BuildPanel)带的就是新值。
	AttributePanelInfo panel;
	PlayerAttributeSystem::BuildPanel(player, panel);
	EXPECT_EQ(expected.physicalAttack, panel.derived().physical_attack());
	EXPECT_EQ(expected.magicAttack, panel.derived().magic_attack());

	// 加成来自实例上的等级,不是别处记的一笔:实例此刻写着 +1。武器不动气血 / 法力上限,当前值原样。
	const ItemComp *worn = Worn(player, kGuidWeapon);
	ASSERT_NE(nullptr, worn);
	EXPECT_EQ(1u, worn->equip().enhance_level());
	EXPECT_EQ(healthBefore, Base(player).health());
	EXPECT_EQ(manaBefore, Base(player).mana());
}

// 强化一件加气血 / 法力的装备:上限涨了,当前值按比例跟(hp = floor(hp x newMax / oldMax)),
// 不是「上限涨多少、当前值补多少」—— 强化不能当治疗(与穿脱同走 kEquipmentChanged)。
TEST_F(PlayerEquipAttributeTest, EnhancingHealthGearKeepsTheHealthAndManaRatioInsteadOfHealing)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six bare = ReadSix(Derived(player));
	ASSERT_NO_FATAL_FAILURE(GiveClothesVitalityBaseAttributes());
	const uint64_t plain = BaseAttrValue(kClothes, kAttrHealth);
	ASSERT_EQ(plain, BaseAttrValue(kClothes, kAttrMana));
	const uint64_t enhanced = EnhancedBaseAttr(kClothes, kAttrHealth, 1);
	ASSERT_GT(enhanced, plain) << "enhance level 1 adds nothing to a base value of " << plain;
	const uint64_t gain = enhanced - plain;

	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidClothes, kClothes)));
	const uint64_t oldMaxHealth = Derived(player).max_health();
	const uint64_t oldMaxMana = Derived(player).max_mana();
	ASSERT_EQ(bare.maxHealth + plain, oldMaxHealth);
	ASSERT_EQ(bare.maxMana + plain, oldMaxMana);
	const uint64_t health = oldMaxHealth / 2;
	const uint64_t mana = oldMaxMana / 4;
	ASSERT_GT(health, 0u);
	ASSERT_GT(mana, 0u);
	Base(player).set_health(health);
	Base(player).set_mana(mana);

	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 1));
	ScriptEnhanceRolls(kRollAlwaysSucceeds);
	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidClothes, outcome));
	ASSERT_TRUE(outcome.success);

	const uint64_t newMaxHealth = Derived(player).max_health();
	const uint64_t newMaxMana = Derived(player).max_mana();
	ASSERT_EQ(oldMaxHealth + gain, newMaxHealth);
	ASSERT_EQ(oldMaxMana + gain, newMaxMana);

	EXPECT_EQ(health * newMaxHealth / oldMaxHealth, Base(player).health());
	EXPECT_EQ(mana * newMaxMana / oldMaxMana, Base(player).mana());
	EXPECT_GE(Base(player).health(), health);
	EXPECT_GE(Base(player).mana(), mana);
	// 不是「上限涨多少、当前值补多少」:那条分支只属于升级。
	EXPECT_LT(Base(player).health(), health + gain);
	EXPECT_LT(Base(player).mana(), mana + gain);
	EXPECT_LT(Base(player).health(), newMaxHealth);
}

// 强化 / 镶嵌的是背包里的装备:实例变了,属性与面板不动(没穿着);穿上之后才计入。
TEST_F(PlayerEquipAttributeTest, ForgingGearInTheInventoryChangesNothingUntilItIsWorn)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	ASSERT_EQ(kSuccess, Inventory(player).PutInstance(MakeEquip(kGuidWeapon, kWeapon)));
	const Six before = ReadSix(Derived(player));
	const uint64_t health = before.maxHealth / 2;
	ASSERT_GT(health, 0u);
	Base(player).set_health(health);
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 1));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemAttack));
	ScriptEnhanceRolls(kRollAlwaysSucceeds);
	PanelPushProbe probe;
	ASSERT_NO_FATAL_FAILURE(CalibrateProbe(probe, player));

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidWeapon, outcome));
	ASSERT_TRUE(outcome.success);
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidWeapon, kSocketFirst, kGemAttack));

	ExpectSix(before, ReadSix(Derived(player)));
	EXPECT_EQ(health, Base(player).health());
	EXPECT_EQ(0, probe.Count()) << "nothing worn changed: no recalculation to announce, no panel push";

	// 改的是实例:+1、0 号孔里一颗伤害宝石。
	const ItemComp *stored = Inventory(player).GetItemCompByGuid(kGuidWeapon);
	ASSERT_NE(nullptr, stored);
	EXPECT_EQ(1u, stored->equip().enhance_level());
	ASSERT_EQ(1, stored->equip().gems_size());
	EXPECT_EQ(kGemAttack, stored->equip().gems(0).gem_config_id());

	// 穿上(同一 guid 换包):强化后的基础伤害与宝石一起计入。
	ItemComp carried;
	ASSERT_EQ(kSuccess, Inventory(player).TakeInstance(kGuidWeapon, carried));
	ASSERT_NO_FATAL_FAILURE(Wear(player, carried));
	Six expected = before;
	expected.physicalAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, 1) + GemValue(kGemAttack);
	expected.magicAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, 1) + GemValue(kGemAttack);
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(health, Base(player).health()) << "a weapon does not move the health maximum";
}

// 穿着时强化失败且不掉级:等级没变,六项、当前气血都不动,也不推面板(金币与材料照扣是另一回事)。
TEST_F(PlayerEquipAttributeTest, FailedEnhanceWithoutLevelLossLeavesTheSixHealthAndPanelAlone)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	ASSERT_GE(PlayerEquipSystem::MaxEnhanceLevel(), 2u) << "this case enhances a +1 weapon towards +2";
	ASSERT_NO_FATAL_FAILURE(RigEnhanceRowToFail(2, /*failLevelLoss=*/0));
	const Six before = ReadSix(Derived(player));

	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	SetEnhanceLevel(weapon, 1);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));
	const uint64_t health = Derived(player).max_health() / 2;
	ASSERT_GT(health, 0u);
	Base(player).set_health(health);
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 2));
	ScriptEnhanceRolls(kRollFailsUnlessCertain);
	PanelPushProbe probe;
	ASSERT_NO_FATAL_FAILURE(CalibrateProbe(probe, player));

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidWeapon, outcome))
		<< "a failed roll is still a successful call";
	EXPECT_FALSE(outcome.success);
	EXPECT_EQ(1u, outcome.level);

	// 还是 +1 的数值:没涨到 +2,也没被打回 +0。
	Six expected = before;
	expected.physicalAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, 1);
	expected.magicAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, 1);
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(health, Base(player).health());
	EXPECT_EQ(0, probe.Count()) << "the level did not change: nothing to recalculate, nothing to push";
	const ItemComp *worn = Worn(player, kGuidWeapon);
	ASSERT_NE(nullptr, worn);
	EXPECT_EQ(1u, worn->equip().enhance_level());
}

// 穿着时强化失败并掉级(表配了 fail_level_loss):六项当场回落到掉级后的值,并推一次面板。
TEST_F(PlayerEquipAttributeTest, FailedEnhanceThatDropsALevelLowersTheSixAndPushesThePanel)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	ASSERT_GE(PlayerEquipSystem::MaxEnhanceLevel(), 2u) << "this case enhances a +1 weapon towards +2";
	const uint64_t plain = BaseAttrValue(kWeapon, kAttrDamage);
	ASSERT_GT(EnhancedBaseAttr(kWeapon, kAttrDamage, 1), plain)
		<< "enhance level 1 adds nothing to this weapon; falling back to +0 would be invisible";
	ASSERT_NO_FATAL_FAILURE(RigEnhanceRowToFail(2, /*failLevelLoss=*/1));
	const Six before = ReadSix(Derived(player));

	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	SetEnhanceLevel(weapon, 1);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 2));
	ScriptEnhanceRolls(kRollFailsUnlessCertain);
	PanelPushProbe probe;
	ASSERT_NO_FATAL_FAILURE(CalibrateProbe(probe, player));

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidWeapon, outcome));
	EXPECT_FALSE(outcome.success);
	EXPECT_EQ(0u, outcome.level);

	// 掉回 +0:基础伤害回到表值。
	Six expected = before;
	expected.physicalAttack += plain;
	expected.magicAttack += plain;
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(1, probe.Count()) << "a level drop on worn gear changes the panel and must be pushed";
	const ItemComp *worn = Worn(player, kGuidWeapon);
	ASSERT_NE(nullptr, worn);
	EXPECT_EQ(0u, worn->equip().enhance_level());
}

// ── 镶嵌 / 取下:穿着时调 PlayerEquipSystem::InlayGem / RemoveGem ───────────

// 伤害宝石(effect = 4):嵌上物伤与法伤各加一份宝石值,取下各减回去;每次各推一次面板。
TEST_F(PlayerEquipAttributeTest, InlayingAndRemovingADamageGemMovesBothAttacksByItsValue)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const uint64_t gemValue = GemValue(kGemAttack);
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidWeapon, kWeapon)));
	const Six wornPlain = ReadSix(Derived(player));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemAttack));
	PanelPushProbe probe;
	ASSERT_NO_FATAL_FAILURE(CalibrateProbe(probe, player));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidWeapon, kSocketFirst, kGemAttack));
	Six inlaid = wornPlain;
	inlaid.physicalAttack += gemValue;
	inlaid.magicAttack += gemValue;
	ExpectSix(inlaid, ReadSix(Derived(player)));
	EXPECT_EQ(1, probe.Count()) << "inlaying into worn gear pushes the attribute panel once";
	const ItemComp *worn = Worn(player, kGuidWeapon);
	ASSERT_NE(nullptr, worn);
	ASSERT_EQ(1, worn->equip().gems_size());
	EXPECT_EQ(kSocketFirst, worn->equip().gems(0).socket());
	EXPECT_EQ(kGemAttack, worn->equip().gems(0).gem_config_id());
	EXPECT_EQ(0u, Inventory(player).GetTotalItemCount(kGemAttack)) << "the gem left the inventory";

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidWeapon, kSocketFirst));
	ExpectSix(wornPlain, ReadSix(Derived(player)));
	EXPECT_EQ(2, probe.Count()) << "removing it pushes the panel once more";
	worn = Worn(player, kGuidWeapon);
	ASSERT_NE(nullptr, worn);
	EXPECT_EQ(0, worn->equip().gems_size());
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(kGemAttack)) << "and it came back";
}

// 四种宝石各落在自己的属性上(效果走 EquipAttribute 的 effect,与随机属性同一套,C++ 不按宝石类型分支):
// 伤害宝石物伤法伤各加,防御 / 气血 / 速度宝石只加各自那一项。
TEST_F(PlayerEquipAttributeTest, EachGemKindAddsToItsOwnAttributeWhenInlaidIntoWornGear)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidWeapon, kWeapon)));
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidHat, kHat)));
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidClothes, kClothes)));
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidShoes, kShoes)));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemAttack, 0));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemDefense, 1));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemHealth, 2));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemSpeed, 3));
	Six expected = ReadSix(Derived(player));

	{
		SCOPED_TRACE("damage gem into the weapon's any-type socket");
		ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidWeapon, kSocketSecond, kGemAttack));
		expected.physicalAttack += GemValue(kGemAttack);
		expected.magicAttack += GemValue(kGemAttack);
		ExpectSix(expected, ReadSix(Derived(player)));
	}
	{
		SCOPED_TRACE("defense gem into the clothes");
		ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidClothes, kSocketFirst, kGemDefense));
		expected.defense += GemValue(kGemDefense);
		ExpectSix(expected, ReadSix(Derived(player)));
	}
	{
		SCOPED_TRACE("health gem into the hat");
		ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidHat, kSocketFirst, kGemHealth));
		expected.maxHealth += GemValue(kGemHealth);
		ExpectSix(expected, ReadSix(Derived(player)));
		EXPECT_EQ(expected.maxHealth, Base(player).health()) << "full stays full";
	}
	{
		SCOPED_TRACE("speed gem into the shoes");
		ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidShoes, kSocketFirst, kGemSpeed));
		expected.speed += GemValue(kGemSpeed);
		ExpectSix(expected, ReadSix(Derived(player)));
		EXPECT_EQ(expected.speed, Base(player).speed()) << "gem speed must reach turn order as well";
	}
}

// 「嵌 -> 取」往返:六项逐位回到原值;当前气血嵌上时按比例涨、取下时按比例降,一个来回绝不净得
// (两次向下取整最多让它少 1 点)。满血时两头都贴着上限走,取下后恰好回到原值。
TEST_F(PlayerEquipAttributeTest, GemRoundTripsReturnTheSixAndNeverGainHealth)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const uint64_t gemValue = GemValue(kGemHealth);
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidHat, kHat)));
	const Six worn = ReadSix(Derived(player));
	const uint64_t manaBefore = Base(player).mana();
	const uint64_t startHealth = worn.maxHealth / 2;
	ASSERT_GT(startHealth, 10u) << "leave room for a few one-point floor losses";
	Base(player).set_health(startHealth);
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemHealth));
	Six inlaid = worn;
	inlaid.maxHealth += gemValue;

	for (int round = 0; round < 3; ++round)
	{
		SCOPED_TRACE("round " + std::to_string(round));
		const uint64_t healthBefore = Base(player).health();

		ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidHat, kSocketFirst, kGemHealth));
		ExpectSix(inlaid, ReadSix(Derived(player)));
		// hp = floor(hp x newMax / oldMax):跟着上限按比例涨,不是「上限涨多少补多少」。
		EXPECT_EQ(healthBefore * inlaid.maxHealth / worn.maxHealth, Base(player).health());
		EXPECT_LT(Base(player).health(), healthBefore + gemValue);

		ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidHat, kSocketFirst));
		ExpectSix(worn, ReadSix(Derived(player)));
		EXPECT_LE(Base(player).health(), healthBefore) << "a round trip must never be a heal";
		EXPECT_GE(Base(player).health() + 1, healthBefore) << "and costs at most the one point lost to flooring";
		EXPECT_EQ(manaBefore, Base(player).mana()) << "a health gem does not touch mana";
	}
	EXPECT_LE(Base(player).health(), startHealth);
	EXPECT_EQ(1u, Inventory(player).GetTotalItemCount(kGemHealth)) << "the one gem came back every time";

	// 满血:嵌上跟着上限涨满,取下恰好回到原上限 —— 当前气血逐位回到原值。
	Base(player).set_health(worn.maxHealth);
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidHat, kSocketFirst, kGemHealth));
	EXPECT_EQ(inlaid.maxHealth, Base(player).health()) << "full stays full";
	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidHat, kSocketFirst));
	EXPECT_EQ(worn.maxHealth, Base(player).health()) << "and comes back down with the maximum";
	ExpectSix(worn, ReadSix(Derived(player)));
}

// ── 镶嵌:实例上的宝石记录怎么读(入包边界整份信任 equip 段,消费侧每次现查)──

// 不合格的记录不计入:宝石行不存在、孔位不在这件装备部位的定义里。合格的记录不受旁边脏记录的影响。
TEST_F(PlayerEquipAttributeTest, GemRecordsWithoutAGemRowOrADefinedSocketAreNotCounted)
{
	// 武器只有 0 / 1 两个孔;拿规则允许的最大孔位号当「这件装备没有的孔」。
	constexpr uint32_t kUndefinedSocket = equiprules::kMaxGemSockets - 1;
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	ASSERT_EQ(nullptr, SocketRowOrNull(kWeaponKind, kUndefinedSocket));
	const Six before = ReadSix(Derived(player));
	Six baseOnly = before;
	baseOnly.physicalAttack += BaseAttrValue(kWeapon, kAttrDamage);
	baseOnly.magicAttack += BaseAttrValue(kWeapon, kAttrDamage);
	Six actual;

	ItemComp junkOnly = MakeEquip(kGuidWeapon, kWeapon);
	AddGemRecord(junkOnly, kSocketFirst, kUnknownGem);    // 宝石行不存在
	AddGemRecord(junkOnly, kUndefinedSocket, kGemAttack); // 孔位不存在
	ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, junkOnly, actual));
	ExpectSix(baseOnly, actual);

	// 同一件再多一条合格的(1 号通用孔嵌伤害宝石):只多出这一颗。
	ItemComp mixed = junkOnly;
	AddGemRecord(mixed, kSocketSecond, kGemAttack);
	ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, mixed, actual));
	Six withOneGem = baseOnly;
	withOneGem.physicalAttack += GemValue(kGemAttack);
	withOneGem.magicAttack += GemValue(kGemAttack);
	ExpectSix(withOneGem, actual);
}

// 读法(设计 §4.3 ResolveGemsNow):同一个孔只认第一条,第一条不合格就整个孔不计(不回退到后一条);
// 重复记录不重复计;不复核宝石类型与孔位类型(类型是嵌入时的规则);只读实例上前 kMaxGemSockets 条。
TEST_F(PlayerEquipAttributeTest, GemRecordsCountOncePerSocketWithoutTypeRecheckWithinTheFirstEight)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const Six before = ReadSix(Derived(player));
	Six baseOnly = before;
	baseOnly.physicalAttack += BaseAttrValue(kWeapon, kAttrDamage);
	baseOnly.magicAttack += BaseAttrValue(kWeapon, kAttrDamage);
	Six withAttackGem = baseOnly;
	withAttackGem.physicalAttack += GemValue(kGemAttack);
	withAttackGem.magicAttack += GemValue(kGemAttack);
	Six actual;

	{
		SCOPED_TRACE("an unqualified first record shadows a qualified later one on the same socket");
		ItemComp shadowed = MakeEquip(kGuidWeapon, kWeapon);
		AddGemRecord(shadowed, kSocketFirst, kUnknownGem);
		AddGemRecord(shadowed, kSocketFirst, kGemAttack);
		ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, shadowed, actual));
		ExpectSix(baseOnly, actual);
	}
	{
		SCOPED_TRACE("two qualified records on one socket count once");
		ItemComp duplicated = MakeEquip(kGuidWeapon, kWeapon);
		AddGemRecord(duplicated, kSocketFirst, kGemAttack);
		AddGemRecord(duplicated, kSocketFirst, kGemAttack);
		ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, duplicated, actual));
		ExpectSix(withAttackGem, actual);
	}
	{
		SCOPED_TRACE("a gem whose type the socket would refuse today still counts once it is recorded");
		// 武器 0 号孔只收攻击宝石,记录里却是一颗防御宝石:照样计入,加的是防御。
		const auto *attackSocket = SocketRowOrNull(kWeaponKind, kSocketFirst);
		const auto *defenseGem = GemRowOrNull(kGemDefense);
		ASSERT_NE(nullptr, attackSocket);
		ASSERT_NE(nullptr, defenseGem);
		ASSERT_NE(0u, attackSocket->gem_type()) << "socket 0 of the weapon became an any-type socket";
		ASSERT_NE(attackSocket->gem_type(), defenseGem->gem_type());
		ItemComp mismatched = MakeEquip(kGuidWeapon, kWeapon);
		AddGemRecord(mismatched, kSocketFirst, kGemDefense);
		ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, mismatched, actual));
		Six withDefenseGem = baseOnly;
		withDefenseGem.defense += GemValue(kGemDefense);
		ExpectSix(withDefenseGem, actual);
	}
	{
		SCOPED_TRACE("the eighth record is still read");
		// 前 7 条占着表里不可能有的孔位号(>= kMaxGemSockets),第 8 条才是合格的那一颗。
		ItemComp eighth = MakeEquip(kGuidWeapon, kWeapon);
		for (uint32_t i = 0; i + 1 < equiprules::kMaxGemSockets; ++i)
		{
			ASSERT_EQ(nullptr, SocketRowOrNull(kWeaponKind, equiprules::kMaxGemSockets + i));
			AddGemRecord(eighth, equiprules::kMaxGemSockets + i, kGemAttack);
		}
		AddGemRecord(eighth, kSocketFirst, kGemAttack);
		ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, eighth, actual));
		ExpectSix(withAttackGem, actual);
	}
	{
		SCOPED_TRACE("the ninth record is not");
		ItemComp ninth = MakeEquip(kGuidWeapon, kWeapon);
		for (uint32_t i = 0; i < equiprules::kMaxGemSockets; ++i)
		{
			ASSERT_EQ(nullptr, SocketRowOrNull(kWeaponKind, equiprules::kMaxGemSockets + i));
			AddGemRecord(ninth, equiprules::kMaxGemSockets + i, kGemAttack);
		}
		AddGemRecord(ninth, kSocketFirst, kGemAttack);
		ASSERT_NO_FATAL_FAILURE(WearReadTakeOff(player, ninth, actual));
		ExpectSix(baseOnly, actual);
	}
}

// ── 不落库、存档往返、面板 ───────────────────────────────────────────────

// 强化与宝石的数值同样不进属性组件:入库的只有实例上的「+几」和「哪个孔嵌了哪种宝石」(§5 不变量 2),
// PlayerAttributeComp 的序列化字节前后相同,BaseAttributesComp 的成长字段不带这些数值。
TEST_F(PlayerEquipAttributeTest, EnhancementAndGemsAreNeverWrittenToPersistedAttributeComponents)
{
	constexpr uint32_t kStoredBonus = 5;
	constexpr uint32_t kAllocated = 20;
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	(*AttributeComp(player).mutable_bonus_values())[kDimStrength] = kStoredBonus;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::Allocate(player, PlayerPoolId(), std::map<uint32_t, uint32_t>{{kDimStrength, kAllocated}}));

	// 同一个对象、期间不改它:map 字段的迭代序不变,字节可以直接比。
	const std::string attributeBefore = AttributeComp(player).SerializeAsString();
	const BaseAttributesComp baseBefore = Base(player);
	const Six sixBefore = ReadSix(Derived(player));

	// 三件是手工构造的 +N 带宝石(从存档读出来就是这个形状)……
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	SetEnhanceLevel(weapon, maxLevel);
	AddGemRecord(weapon, kSocketFirst, kGemAttack);
	ItemComp hat = MakeEquip(kGuidHat, kHat);
	SetEnhanceLevel(hat, maxLevel);
	AddGemRecord(hat, kSocketFirst, kGemHealth);
	ItemComp shoes = MakeEquip(kGuidShoes, kShoes);
	SetEnhanceLevel(shoes, maxLevel);
	AddGemRecord(shoes, kSocketFirst, kGemSpeed);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));
	ASSERT_NO_FATAL_FAILURE(Wear(player, shoes));
	// ……衣服则穿着经写入口强化、镶嵌(这两个入口自己触发重算)。
	ASSERT_NO_FATAL_FAILURE(Wear(player, MakeEquip(kGuidClothes, kClothes)));
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 1));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemDefense));
	ScriptEnhanceRolls(kRollAlwaysSucceeds);
	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidClothes, outcome));
	ASSERT_TRUE(outcome.success);
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidClothes, kSocketFirst, kGemDefense));

	// 强化与四种宝石此刻都在算(否则下面的「没写进去」是空转)。
	const Six forged = ReadSix(Derived(player));
	EXPECT_GT(forged.physicalAttack, sixBefore.physicalAttack);
	EXPECT_GT(forged.magicAttack, sixBefore.magicAttack);
	EXPECT_GT(forged.maxHealth, sixBefore.maxHealth);
	EXPECT_GT(forged.speed, sixBefore.speed);
	EXPECT_GT(forged.defense, sixBefore.defense);

	EXPECT_EQ(attributeBefore, AttributeComp(player).SerializeAsString())
		<< "enhance levels and gems must not leak into PlayerAttributeComp (bonus_values is persisted)";
	ASSERT_EQ(1, AttributeComp(player).bonus_values_size());
	EXPECT_EQ(kStoredBonus, AttributeComp(player).bonus_values().at(kDimStrength));
	EXPECT_EQ(0, AttributeComp(player).bonus_points_size());

	// BaseAttributesComp 的成长字段不带这些数值;speed 仍是重算结果的镜像(含强化后的鞋与速度宝石)。
	EXPECT_EQ(baseBefore.strength(), Base(player).strength());
	EXPECT_EQ(baseBefore.stamina(), Base(player).stamina());
	EXPECT_EQ(baseBefore.critchance(), Base(player).critchance());
	EXPECT_EQ(baseBefore.armor(), Base(player).armor());
	EXPECT_EQ(baseBefore.resistance(), Base(player).resistance());
	EXPECT_EQ(Derived(player).speed(), Base(player).speed());

	// 取下宝石、脱光之后什么都不剩。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidClothes, kSocketFirst));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidWeapon));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidHat));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidClothes));
	ASSERT_NO_FATAL_FAILURE(TakeOff(player, kGuidShoes));
	EXPECT_EQ(attributeBefore, AttributeComp(player).SerializeAsString());
	ExpectSix(sixBefore, ReadSix(Derived(player)));
	EXPECT_EQ(baseBefore.speed(), Base(player).speed()) << "nothing of the forged gear is left behind once it is off";
	EXPECT_EQ(baseBefore.health(), Base(player).health()) << "full health before, full health after";
}

// 存档往返:强化等级与宝石记录随装备实例整份落库、整份回来,重登时按还原出的装备栏重新算出同样的加成。
// 气血宝石抬高的上限也在 —— 满血带着它下线的玩家重登后仍是那个血量。
TEST_F(PlayerEquipAttributeTest, ReloadFromTheDatabaseRecordRecomputesEnhanceAndGemBonuses)
{
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	SetEnhanceLevel(weapon, maxLevel);
	AddGemRecord(weapon, kSocketFirst, kGemAttack);
	ItemComp hat = MakeEquip(kGuidHat, kHat);
	SetEnhanceLevel(hat, 1);
	AddGemRecord(hat, kSocketFirst, kGemHealth);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));

	// 出发点先对一遍:无装备基线 + 强化后的基础属性 + 两颗宝石。
	Six expected = NoGearBaseline();
	expected.physicalAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, maxLevel) + GemValue(kGemAttack);
	expected.magicAttack += EnhancedBaseAttr(kWeapon, kAttrDamage, maxLevel) + GemValue(kGemAttack);
	expected.defense += EnhancedBaseAttr(kHat, kAttrDefense, 1);
	expected.maxHealth += GemValue(kGemHealth);
	ExpectSix(expected, ReadSix(Derived(player)));
	ASSERT_EQ(expected.maxHealth, Base(player).health());

	// 存盘只认走完加载创建点的实体(缺任何持久化组件就整份拒绝、什么都不写):先把夹具没挂的补成空存档。
	CompletePersistedComps(player);
	PlayerAllData saved;
	ASSERT_TRUE(PlayerAllDataMessageFieldsMarshal(player, saved));
	std::string bytes;
	ASSERT_TRUE(saved.player_database_data().SerializeToString(&bytes));
	PlayerAllData coldSnapshot;
	ASSERT_TRUE(coldSnapshot.mutable_player_database_data()->ParseFromString(bytes));

	const auto restored = tlsEcs.actorRegistry.create();
	players.push_back(restored);
	tlsEcs.actorRegistry.emplace<PlayerGuidComp>(restored, kPlayerIdBase + players.size());
	PlayerAllDataMessageFieldsUnmarshal(restored, coldSnapshot);

	const auto *bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(restored);
	ASSERT_NE(nullptr, bags);
	ASSERT_EQ(2u, bags->bags[kEquipment].OccupiedGridCount());

	// 入库的只有实例上这两样,原样回来;数值是下面重算出来的。
	const ItemComp *restoredWeapon = Worn(restored, kGuidWeapon);
	ASSERT_NE(nullptr, restoredWeapon);
	ASSERT_TRUE(restoredWeapon->has_equip());
	EXPECT_EQ(maxLevel, restoredWeapon->equip().enhance_level());
	ASSERT_EQ(1, restoredWeapon->equip().gems_size());
	EXPECT_EQ(kSocketFirst, restoredWeapon->equip().gems(0).socket());
	EXPECT_EQ(kGemAttack, restoredWeapon->equip().gems(0).gem_config_id());
	const ItemComp *restoredHat = Worn(restored, kGuidHat);
	ASSERT_NE(nullptr, restoredHat);
	ASSERT_TRUE(restoredHat->has_equip());
	EXPECT_EQ(1u, restoredHat->equip().enhance_level());
	ASSERT_EQ(1, restoredHat->equip().gems_size());
	EXPECT_EQ(kSocketFirst, restoredHat->equip().gems(0).socket());
	EXPECT_EQ(kGemHealth, restoredHat->equip().gems(0).gem_config_id());

	const auto *derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(restored);
	const auto *base = tlsEcs.actorRegistry.try_get<BaseAttributesComp>(restored);
	ASSERT_NE(nullptr, derived);
	ASSERT_NE(nullptr, base);
	ExpectSix(expected, ReadSix(*derived));
	EXPECT_EQ(expected.maxHealth, base->health())
		<< "the gem's health must survive the reload: bags are restored before attributes are initialised";
	EXPECT_EQ(Base(player).mana(), base->mana());
}

// 面板口径不变:强化放大的是基础属性,不是随机属性 —— 一级属性的 bonus(装备给的点)与 15 行战斗类属性
// 在强化 / 镶嵌前后逐项相同;变的只有六项里强化的增量与宝石值,面板的二级属性仍是同一次重算的产出。
TEST_F(PlayerEquipAttributeTest, PanelBonusAndCombatLinesStayTheSameAfterEnhanceAndInlay)
{
	constexpr uint32_t kStrengthAffix = 10;
	constexpr uint32_t kAllAffix = 8;
	ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	const uint32_t poolId = PlayerPoolId();
	const uint64_t enhanceGain = EnhancedBaseAttr(kWeapon, kAttrDamage, 1) - BaseAttrValue(kWeapon, kAttrDamage);
	ASSERT_GT(enhanceGain, 0u) << "enhance level 1 adds nothing to this weapon; pick a sample that moves";

	ItemComp weapon = MakeEquip(kGuidWeapon, kWeapon);
	AddAffix(weapon, kAttrStrength, kBlue, kStrengthAffix, 0);
	AddAffix(weapon, kAttrPhysicalCrit, kBlue, kCombatSet.physicalCritRate, 1);
	ItemComp hat = MakeEquip(kGuidHat, kHat);
	AddAffix(hat, kAttrAllPrimary, kBlue, kAllAffix, 0);
	ASSERT_NO_FATAL_FAILURE(Wear(player, weapon));
	ASSERT_NO_FATAL_FAILURE(Wear(player, hat));
	AttributePanelInfo plain;
	PlayerAttributeSystem::BuildPanel(player, plain);
	const Six plainSix = ReadSix(Derived(player));

	// 武器强化一级并在 0 号孔嵌伤害宝石;帽子 0 号孔嵌气血宝石。
	ASSERT_NO_FATAL_FAILURE(FundEnhance(player, 1));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemAttack, 0));
	ASSERT_NO_FATAL_FAILURE(StockGem(player, kGemHealth, 1));
	ScriptEnhanceRolls(kRollAlwaysSucceeds);
	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidWeapon, outcome));
	ASSERT_TRUE(outcome.success);
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidWeapon, kSocketFirst, kGemAttack));
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidHat, kSocketFirst, kGemHealth));
	AttributePanelInfo forged;
	PlayerAttributeSystem::BuildPanel(player, forged);

	// 一级属性:装备给的点还是随机属性写的那几点 —— 没被强化放大,宝石也没往里加;点池同样不动。
	for (const AttributePanelInfo *panel : {&plain, &forged})
	{
		SCOPED_TRACE(panel == &plain ? "before enhancing and inlaying" : "after enhancing and inlaying");
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(*panel, kDimStrength, 0, kStrengthAffix + kAllAffix));
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(*panel, kDimConstitution, 0, kAllAffix));
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(*panel, kDimSpirit, 0, kAllAffix));
		ASSERT_NO_FATAL_FAILURE(ExpectDimension(*panel, kDimAgility, 0, kAllAffix));
	}
	const auto *plainPool = FindPool(plain, poolId);
	const auto *forgedPool = FindPool(forged, poolId);
	ASSERT_NE(nullptr, plainPool);
	ASSERT_NE(nullptr, forgedPool);
	EXPECT_EQ(plainPool->total(), forgedPool->total());
	EXPECT_EQ(plainPool->remaining(), forgedPool->remaining());

	// 战斗类属性:15 行逐项相同(强化不放大「物理必杀率」这条随机属性;出厂的宝石也不给战斗类属性)。
	ASSERT_NO_FATAL_FAILURE(ExpectCombatListWellFormed(forged));
	for (uint32_t raw = equiprules::ToRaw(CombatStat::kNone) + 1; raw < equiprules::ToRaw(CombatStat::kCount); ++raw)
	{
		const auto *before = FindCombat(plain, equiprules::ToCombatStat(raw));
		const auto *after = FindCombat(forged, equiprules::ToCombatStat(raw));
		ASSERT_NE(nullptr, before) << "combat_id " << raw;
		ASSERT_NE(nullptr, after) << "combat_id " << raw;
		EXPECT_EQ(before->value(), after->value()) << "combat_id " << raw;
	}
	ASSERT_NO_FATAL_FAILURE(
		ExpectCombatLine(forged, CombatStat::kPhysicalCritRate, kBaseCritChance + kCombatSet.physicalCritRate));
	CombatValues expectedCombat;
	expectedCombat.physicalCritRate = kCombatSet.physicalCritRate;
	ExpectCombat(expectedCombat, Derived(player).combat());

	// 六项:只多出强化的增量与两颗宝石;面板上的二级属性就是这一份。
	Six expected = plainSix;
	expected.physicalAttack += enhanceGain + GemValue(kGemAttack);
	expected.magicAttack += enhanceGain + GemValue(kGemAttack);
	expected.maxHealth += GemValue(kGemHealth);
	ExpectSix(expected, ReadSix(Derived(player)));
	EXPECT_EQ(expected.physicalAttack, forged.derived().physical_attack());
	EXPECT_EQ(expected.magicAttack, forged.derived().magic_attack());
	EXPECT_EQ(expected.maxHealth, forged.derived().max_health());
}

// ═════════════════════════════════════════════════════════════════════════
// 组件归属(docs/design/ecs-entt-misuse-audit-20261015.md F057):PlayerAttributeComp / LevelComp 由加载链路
// (Unmarshal)挂上,DerivedAttributesComp 由 InitializeOnLoad 挂上;其余入口只取不建,缺了 fail-closed。
// 修复前 EnsureComp / Recalculate / GmSetLevel 都用 get_or_emplace,下面除第一条外每条都会因为
// 「组件被悄悄补建」或「返回码不是 kServiceUnavailable」而失败。错误码口径与 player_mission.cpp /
// skill.cpp 一致:实体无效 → kEntityIsNull,实体有效但缺必需组件(加载链路故障)→ kServiceUnavailable。
// ═════════════════════════════════════════════════════════════════════════

// 默认方案的修补从「每次访问顺手修」挪到了「加载时修一次」,口径不能丢:空存档加载后有方案一且已激活,
// next_scheme_id 接在后面,二级属性缓存也挂上了。(修复前后都应成立。)
TEST_F(PlayerEquipAttributeTest, InitializeOnLoadFillsTheDefaultSchemeAndCreatesTheDerivedCache)
{
	const auto &comp = AttributeComp(player);
	ASSERT_EQ(1, comp.schemes_size());
	EXPECT_EQ(1u, comp.schemes(0).scheme_id());
	EXPECT_EQ(comp.schemes(0).scheme_id(), comp.active_scheme_id());
	EXPECT_EQ(2u, comp.next_scheme_id());
	ASSERT_NE(nullptr, tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player));
	EXPECT_GE(Derived(player).max_health(), 1u) << "InitializeOnLoad must recalculate right after creating the cache";
}

// 存档组件没挂上就进 InitializeOnLoad:只记错误,既不补建存档组件,也不挂二级属性缓存。
// 补建出来的是一份带默认方案、看起来完全正常的空组件,下一次存盘就会把玩家的加点清零写回。
TEST_F(PlayerEquipAttributeTest, InitializeOnLoadDoesNotCreateTheSaveComponentOnBehalfOfTheLoadPath)
{
	const auto entity = NewUnloadedEntity();
	PlayerAttributeSystem::InitializeOnLoad(entity);
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAttributeComp>(entity));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(entity));
}

// 重算只写不建:缺存档组件、或缺二级属性缓存(没走 InitializeOnLoad)时整次跳过 ——
// 不给实体添组件,也不按「旧上限 0」去夹当前血量。
TEST_F(PlayerEquipAttributeTest, RecalculateSkipsAndCreatesNothingWhenARequiredComponentIsMissing)
{
	const auto unloaded = NewUnloadedEntity();
	PlayerAttributeSystem::Recalculate(unloaded, RecalcReason::kEquipmentChanged);
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAttributeComp>(unloaded));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(unloaded));
	EXPECT_EQ(kFarAboveAnyMax, Base(unloaded).health()) << "a skipped recalculation must not clamp the current health";

	const auto noCache = NewUnloadedEntity();
	tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(noCache);
	PlayerAttributeSystem::Recalculate(noCache, RecalcReason::kEquipmentChanged);
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(noCache))
		<< "the derived cache is created only by InitializeOnLoad";
	EXPECT_EQ(kFarAboveAnyMax, Base(noCache).health());
}

TEST_F(PlayerEquipAttributeTest, EveryRealInputIsRequiredBeforePanelRecalculationAndWrites)
{
	ExpectMissingInputRejected<BaseAttributesComp>("BaseAttributesComp");
	ExpectMissingInputRejected<PlayerAttributeComp>("PlayerAttributeComp");
	ExpectMissingInputRejected<LevelComp>("LevelComp");
	ExpectMissingInputRejected<PlayerUint32Comp>("PlayerUint32Comp");
	ExpectMissingInputRejected<PlayerBagsComp>("PlayerBagsComp");
	ExpectMissingInputRejected<DerivedAttributesComp>("DerivedAttributesComp");
}

TEST_F(PlayerEquipAttributeTest, ReadOnlyQueriesDoNotInheritTheWriteFreezeRestriction)
{
	const auto poolId = PlayerPoolId();
	AttributePanelInfo before;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, before));
	std::map<uint32_t, uint32_t> beforeSuggested;
	const auto beforeResult = PlayerAttributeSystem::AutoAllocate(player, poolId, beforeSuggested);
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);

	AttributePanelInfo frozenPanel;
	EXPECT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, frozenPanel));
	EXPECT_EQ(before.SerializeAsString(), frozenPanel.SerializeAsString());
	std::map<uint32_t, uint32_t> frozenSuggested;
	EXPECT_EQ(beforeResult, PlayerAttributeSystem::AutoAllocate(player, poolId, frozenSuggested));
	EXPECT_EQ(beforeSuggested, frozenSuggested);
}

// 面板是只读路径:缺存档组件时返回故障码、面板清空(先清掉调用方传进来的旧内容),不改变实体的组件集合。
// 返回码让 handler 回错误,而不是把空面板当成功发给客户端(客户端按整体覆盖,空面板等于抹掉真实数据)。
// 修复前 BuildPanel 没有返回值,还会补建组件并填出完整面板。
TEST_F(PlayerEquipAttributeTest, PanelOfAnUnloadedEntityIsEmptyAndCreatesNothing)
{
	const auto entity = NewUnloadedEntity();
	AttributePanelInfo panel;
	panel.set_level(99);
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::BuildPanel(entity, panel));
	EXPECT_EQ(0u, panel.level());
	EXPECT_EQ(0, panel.pools_size());
	EXPECT_EQ(0, panel.dimensions_size());
	EXPECT_EQ(0, panel.schemes_size());
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAttributeComp>(entity));
}

// 写入口(含只算不落的自动加点)缺存档组件:一律在扣费 / 改动之前拒绝,返回服务端故障码,不补建组件。
TEST_F(PlayerEquipAttributeTest, WriteEntriesRejectAnEntityWithoutTheSaveComponent)
{
	const auto entity = NewUnloadedEntity();
	const uint32_t poolId = PlayerPoolId();
	ASSERT_NE(0u, poolId);

	EXPECT_EQ(kServiceUnavailable,
			  PlayerAttributeSystem::Allocate(entity, poolId, std::map<uint32_t, uint32_t>{{kDimStrength, 1}}));
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::Reset(entity, poolId));
	std::map<uint32_t, uint32_t> suggested{{kDimStrength, 1}};
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::AutoAllocate(entity, poolId, suggested));
	EXPECT_TRUE(suggested.empty());
	uint32_t schemeId = 123;
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::CreateScheme(entity, "", schemeId));
	EXPECT_EQ(0u, schemeId);
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::SwitchScheme(entity, 1));
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::RenameScheme(entity, 1, "方案甲"));

	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<PlayerAttributeComp>(entity));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(entity));
}

// GM 设等级:LevelComp 是存档组件,缺了就拒绝,不补建一个再改(补建的等级会随下一次存盘写回)。
TEST_F(PlayerEquipAttributeTest, GmSetLevelRejectsAnEntityWithoutALevelComponent)
{
	const auto entity = NewUnloadedEntity();
	tlsEcs.actorRegistry.remove<LevelComp>(entity);
	ASSERT_TRUE(playerlevel::IsValidLevel(10));
	EXPECT_EQ(kServiceUnavailable, PlayerAttributeSystem::GmSetLevel(entity, 10));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<LevelComp>(entity));
}

TEST_F(PlayerEquipAttributeTest, GmSetLevelRecalculatesDerivedAttributesWithoutEventWiring)
{
	constexpr uint32_t newLevel = kLevel + 1;
	ASSERT_TRUE(playerlevel::IsValidLevel(newLevel));
	tlsEcs.actorRegistry.emplace<PlayerPetComp>(player);
	ASSERT_EQ(kSuccess, PlayerMissionSystem::Initialize(player));
	const auto oldMax = tlsEcs.actorRegistry.get<DerivedAttributesComp>(player).max_health();

	ASSERT_EQ(kSuccess, PlayerAttributeSystem::GmSetLevel(player, newLevel));

	const auto derivedAfter = tlsEcs.actorRegistry.get<DerivedAttributesComp>(player);
	EXPECT_GT(derivedAfter.max_health(), oldMax);
	AttributePanelInfo panel;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	EXPECT_EQ(newLevel, panel.level());
	PlayerAttributeSystem::Recalculate(player, RecalcReason::kLevelChanged);
	EXPECT_EQ(derivedAfter.SerializeAsString(),
		tlsEcs.actorRegistry.get<DerivedAttributesComp>(player).SerializeAsString());
	AttributePanelInfo recalculated;
	ASSERT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, recalculated));
	EXPECT_EQ(panel.derived().SerializeAsString(), recalculated.derived().SerializeAsString());
}

// 面板接口的另外两种结果:实体无效 → kEntityIsNull;走完加载的玩家 → kSuccess 且面板有内容。
TEST_F(PlayerEquipAttributeTest, BuildPanelReportsInvalidEntityAndSucceedsForALoadedPlayer)
{
	AttributePanelInfo panel;
	EXPECT_EQ(kEntityIsNull, PlayerAttributeSystem::BuildPanel(entt::null, panel));

	EXPECT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
	EXPECT_EQ(kLevel, panel.level());
	EXPECT_GT(panel.pools_size(), 0);
}

} // namespace
