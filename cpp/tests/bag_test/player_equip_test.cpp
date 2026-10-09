// 装备编排层(PlayerEquipSystem):新装备掷随机属性、穿上 / 卸下、装备加成汇总、tooltip 显示数据、
// 三个客户端 RPC 的 handler。契约见 docs/design/equipment-attributes.md §4.3 / §4.6 / §5。
//
// 分工:
//   * 容器层的搬运原语与初始化回调本身(TakeInstance / PutInstance / ItemInstanceInitializer)
//     在 bag_instance_data_test.cpp;这里验的是编排 —— 选槽、校验顺序、拒绝时两个包不变、
//     同一 guid 换包后实例数据不变。
//   * 「怎么掷、怎么累加」的纯规则在 turn_battle_engine_test/equip_attribute_rules_test.cpp;
//     这里验的是表行 / 实例被正确翻译成规则的输入。
//   * 装备加成进面板 / 进战斗的数值在 player_equip_attribute_test.cpp;这里只确认穿脱之后
//     触发了重算(DerivedAttributesComp 出现)。
//
// 数值一律现读配表(上限、基础属性都是占位建议,策划随时会改),只把「哪个属性 id 是什么效果」
// 当作契约写死(设计文档 §2.1:id 一经使用不再改义)。
//
// 夹具自带:SetUp 自己加载用到的每一张表、自己装 item 号段;TearDown 卸掉 BagService 的
// 初始化回调、把改过的表重新加载、恢复其它用例使用的号段基线 —— 不依赖别的文件的用例先跑或后跑。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <limits>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_gm_guard.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/system/equip_attribute_rules.h"
#include "services/scene/player/system/player_equip.h"
#include "services/scene/player/system/player_feature_snapshot.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

// ── 夹具数据 ────────────────────────────────────────────────────────────────
// 既有行(generated/tables/item.json、equipslot.json):
constexpr uint32_t kOldEquipA = 1;        // 不可叠加,部位 1,没有属性池
constexpr uint32_t kOldEquipB = 2;        // 不可叠加,部位 1,没有属性池
constexpr uint32_t kPlainNonStack = 3;    // 不可叠加,不是装备
constexpr uint32_t kStackable = 10;       // 可叠加
constexpr uint32_t kUnknownConfig = 987654;
constexpr uint32_t kOldKind = 1;          // 槽 0 / 1 接部位 1
constexpr uint32_t kOldSlotFirst = 0;
constexpr uint32_t kOldSlotSecond = 1;

// 本次新增的行(设计文档 §2.6;需先导表):
constexpr uint32_t kWeaponL1 = 1101;
constexpr uint32_t kWeaponL80 = 1105;
constexpr uint32_t kHatL1 = 1201;
constexpr uint32_t kShoesL1 = 1401;
constexpr uint32_t kWeaponKind = 11;
constexpr uint32_t kHatKind = 12;
constexpr uint32_t kClothesKind = 13;
constexpr uint32_t kShoesKind = 14;
constexpr uint32_t kWeaponSlot = 3;
constexpr uint32_t kHatSlot = 4;
constexpr uint32_t kClothesSlot = 5;
constexpr uint32_t kShoesSlot = 6;
constexpr uint32_t kWeaponPool = 1;
constexpr uint32_t kLevel1 = 1;
constexpr uint32_t kLevel80 = 80;

// EquipAttribute 的 id(设计文档 §2.1)。
constexpr uint32_t kAttrDamage = 1;        // 伤害:物伤与法伤各加
constexpr uint32_t kAttrAccuracy = 2;      // 准确:只加物伤
constexpr uint32_t kAttrStrength = 3;      // 力量:一级属性点
constexpr uint32_t kAttrConstitution = 4;  // 体质:一级属性点
constexpr uint32_t kAttrAllPrimary = 7;    // 所有属性
constexpr uint32_t kAttrDefense = 8;       // 防御
constexpr uint32_t kAttrHealth = 9;        // 气血
constexpr uint32_t kAttrSpeed = 11;        // 速度:只作基础属性,没有上限行
constexpr uint32_t kAttrPhysicalCrit = 12; // 物理必杀率(百分比)
constexpr uint32_t kAttrReflect = 16;      // 反震率(百分比)
constexpr uint32_t kAttrMagicResist = 25;  // 抗法术
constexpr uint32_t kAttrPhysicalResist = 26; // 抗物理
constexpr uint32_t kUnknownAttr = 987654;

constexpr uint32_t kBlue = equiprules::ToRaw(equiprules::AffixTier::kBlue);
constexpr uint32_t kPink = equiprules::ToRaw(equiprules::AffixTier::kPink);
constexpr uint32_t kYellow = equiprules::ToRaw(equiprules::AffixTier::kYellow);

// 预设 guid:远离号段范围,也避开 bag_instance_data_test.cpp 用的那一组。
constexpr Guid kGuidA = (uint64_t{1} << 60) + 7101;
constexpr Guid kGuidB = (uint64_t{1} << 60) + 7102;
constexpr Guid kGuidC = (uint64_t{1} << 60) + 7103;
constexpr Guid kGuidD = (uint64_t{1} << 60) + 7104;
constexpr Guid kGuidMissing = (uint64_t{1} << 60) + 7199;

constexpr uint64_t kPlayerGuid = 717171;
constexpr uint64_t kSegmentLo = 1200000;
constexpr uint64_t kSegmentHi = 1300000;
constexpr uint32_t kMaxLevel = 85;

const char *const kExportFirst =
	"tables not exported yet: run the table exporter first (data/*.xlsx -> generated tables)";
const char *const kProtoGenFirst = "SceneBagClientPlayer method missing: run proto-gen first";

// 与 bag_instance_data_test.cpp 同一个做法:无 I/O 的假传输 + 直接投一段号。
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

// ── 现读配表 ────────────────────────────────────────────────────────────────

const ItemTable *ItemRowOrNull(uint32_t configId)
{
	return ItemTableManager::Instance().FindByIdSilent(configId).first;
}

const EquipAttributeTable *AttrRowOrNull(uint32_t attrId)
{
	return EquipAttributeTableManager::Instance().FindByIdSilent(attrId).first;
}

// 上限行的主键 = attr_id * 1000 + level(设计文档 §2.2)。刻意按主键直取,不走被测代码用的
// 「按属性取全部档再挑」那条路,免得测试只是把实现重抄一遍。只对等级恰好等于某一档起点的装备成立,
// 夹具在 SetUp 里核对过这一点。
const EquipAttributeCapTable *CapRowOrNull(uint32_t attrId, uint32_t level)
{
	return EquipAttributeCapTableManager::Instance().FindByIdSilent(attrId * 1000 + level).first;
}

uint32_t CapOf(uint32_t attrId, uint32_t level)
{
	const auto *row = CapRowOrNull(attrId, level);
	return row != nullptr ? row->cap() : 0;
}

uint32_t MinOf(uint32_t attrId, uint32_t level)
{
	const auto *row = CapRowOrNull(attrId, level);
	return row != nullptr ? row->min_value() : 0;
}

// Item 行里某条基础属性的数值;没有这条返回 0。
uint64_t BaseValueOf(uint32_t configId, uint32_t attrId)
{
	const auto *row = ItemRowOrNull(configId);
	if (row == nullptr)
	{
		return 0;
	}
	for (const auto &base : row->base_attr())
	{
		if (base.base_attr_id() == attrId)
		{
			return base.base_attr_value();
		}
	}
	return 0;
}

// 一个不属于角色池的一级属性维度(宝宝池的维度;AttributePool.owner_type:0 = 角色,非 0 = 其它,
// 见 data/schema/attributepool_table.proto)。现找而不写死 id;一个都没有返回 0。
uint32_t NonPlayerDimensionOrZero()
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

// 内存里改一行表(照 player_feature_snapshot_test.cpp 的做法)。夹具的 TearDown 会把改过的表
// 重新加载,所以改动不会漏进后面的用例。
template <typename Row>
Row &Editable(const Row *row)
{
	return *const_cast<Row *>(row);
}

// ── 实例构造 ────────────────────────────────────────────────────────────────

ItemComp MakeInstance(Guid guid, uint32_t configId)
{
	ItemComp item;
	item.set_item_id(guid);
	item.set_config_id(configId);
	item.set_size(1);
	return item;
}

void AddAffix(ItemComp &item, uint32_t attrId, uint32_t tier, uint32_t value, uint32_t seq)
{
	auto *affix = item.mutable_equip()->add_affixes();
	affix->set_attr_id(attrId);
	affix->set_tier(tier);
	affix->set_value(value);
	affix->set_seq(seq);
}

// 一件「已经掷过属性」的装备:两条属性,字段取互不相同的值,丢哪个字段都看得出来。
ItemComp MakeRolledInstance(Guid guid, uint32_t configId)
{
	ItemComp item = MakeInstance(guid, configId);
	AddAffix(item, kAttrStrength, kBlue, /*value=*/1, /*seq=*/0);
	AddAffix(item, kAttrPhysicalCrit, kYellow, /*value=*/2, /*seq=*/0);
	return item;
}

// 身份(guid / config / size)与实例数据(equip,含 presence)逐项相等。
// 刻意不比 acquire_seq:那是「本包内的入包先后」,换包时由目标包重新盖章。
void ExpectSameInstance(const ItemComp &expected, const ItemComp &actual)
{
	EXPECT_EQ(expected.item_id(), actual.item_id());
	EXPECT_EQ(expected.config_id(), actual.config_id());
	EXPECT_EQ(expected.size(), actual.size());
	ASSERT_EQ(expected.has_equip(), actual.has_equip()) << "equip presence must survive";
	EXPECT_EQ(expected.equip().SerializeAsString(), actual.equip().SerializeAsString())
		<< "affixes must be carried unchanged";
}

void ExpectStoredInstance(Bag &bag, const ItemComp &expected)
{
	const ItemComp *stored = bag.GetItemCompByGuid(expected.item_id());
	ASSERT_NE(nullptr, stored) << "guid " << expected.item_id() << " missing from the bag";
	ASSERT_NO_FATAL_FAILURE(ExpectSameInstance(expected, *stored));
}

// 一个包两层的可观察状态:guid -> (槽位, 整份实例的序列化字节)。
using BagState = std::map<Guid, std::pair<uint32_t, std::string>>;

BagState Capture(const Bag &bag)
{
	BagState state;
	bag.ForEachItem([&state, &bag](Guid guid, const ItemComp &item)
					{ state[guid] = {bag.GetItemPosByGuid(guid), item.SerializeAsString()}; });
	return state;
}

// 池里全部属性 id。
std::set<uint32_t> PoolAttrs(uint32_t poolId)
{
	std::set<uint32_t> attrs;
	for (const auto *row : EquipAffixPoolTableManager::Instance().GetByPoolId(poolId))
	{
		attrs.insert(row->attr_id());
	}
	return attrs;
}

// 一件刚掷出来的装备必须满足的全部不变量(设计文档 §2.3 / §2.4 / §4.2):
// 属性全部来自它的池、蓝属性条数在规则区间内且互不重复、seq 连续、粉黄各至多一条、
// 顺序蓝 -> 粉 -> 黄、每条的值落在它那一档的 [min_value, cap] 内。
void ExpectValidRoll(const ItemComp &item, uint32_t configId, uint32_t equipLevel)
{
	const ItemTable *row = ItemRowOrNull(configId);
	ASSERT_NE(nullptr, row) << kExportFirst;
	ASSERT_TRUE(item.has_equip()) << "a rolled piece of equipment is marked as initialised";
	const auto *rule = EquipAffixRuleTableManager::Instance().FindByIdSilent(row->affix_rule()).first;
	ASSERT_NE(nullptr, rule) << kExportFirst;
	const std::set<uint32_t> poolAttrs = PoolAttrs(row->affix_pool());
	ASSERT_FALSE(poolAttrs.empty()) << kExportFirst;

	std::vector<uint32_t> blueAttrs;
	uint32_t pinkCount = 0;
	uint32_t yellowCount = 0;
	uint32_t previousTier = 0;
	for (const auto &affix : item.equip().affixes())
	{
		EXPECT_EQ(1u, poolAttrs.count(affix.attr_id()))
			<< "attr " << affix.attr_id() << " is not in pool " << row->affix_pool();
		EXPECT_GE(affix.tier(), previousTier) << "stored order is blue, then pink, then yellow";
		previousTier = affix.tier();

		const auto *capRow = CapRowOrNull(affix.attr_id(), equipLevel);
		ASSERT_NE(nullptr, capRow) << "attr " << affix.attr_id() << " has no cap row at level "
								   << equipLevel;
		EXPECT_GE(affix.value(), 1u);
		EXPECT_GE(affix.value(), capRow->min_value()) << "attr " << affix.attr_id();
		EXPECT_LE(affix.value(), capRow->cap()) << "attr " << affix.attr_id();

		if (affix.tier() == kBlue)
		{
			EXPECT_EQ(static_cast<uint32_t>(blueAttrs.size()), affix.seq()) << "blue seq is 0..n-1";
			blueAttrs.push_back(affix.attr_id());
		}
		else if (affix.tier() == kPink)
		{
			++pinkCount;
			EXPECT_EQ(0u, affix.seq());
		}
		else if (affix.tier() == kYellow)
		{
			++yellowCount;
			EXPECT_EQ(0u, affix.seq());
		}
		else
		{
			ADD_FAILURE() << "unexpected tier " << affix.tier();
		}
	}
	EXPECT_GE(static_cast<uint32_t>(blueAttrs.size()), rule->blue_min());
	EXPECT_LE(static_cast<uint32_t>(blueAttrs.size()), rule->blue_max());
	EXPECT_EQ(blueAttrs.size(), std::set<uint32_t>(blueAttrs.begin(), blueAttrs.end()).size())
		<< "blue affixes never repeat";
	EXPECT_LE(pinkCount, 1u);
	EXPECT_LE(yellowCount, 1u);
}

const google::protobuf::MethodDescriptor *BagMethod(const char *name)
{
	return SceneBagClientPlayer::descriptor()->FindMethodByName(name);
}

template <typename Items>
bool ContainsItem(const Items &items, Guid guid)
{
	for (const auto &item : items)
	{
		if (item.item_id() == guid)
		{
			return true;
		}
	}
	return false;
}

class TestBagService : public SceneBagClientPlayer
{
};

class PlayerEquipTest : public testing::Test
{
protected:
	// 被测代码与 PlayerAttributeSystem::Recalculate / BuildPanel 读到的每一张表。
	static void LoadTables()
	{
		ItemTableManager::Instance().Load();
		EquipSlotTableManager::Instance().Load();
		EquipAttributeTableManager::Instance().Load();
		EquipAttributeCapTableManager::Instance().Load();
		EquipAffixPoolTableManager::Instance().Load();
		EquipAffixRuleTableManager::Instance().Load();
		ClassTableManager::Instance().Load();
		AttributeDimensionTableManager::Instance().Load();
		AttributePoolTableManager::Instance().Load();
		AttributeRuleTableManager::Instance().Load();
		AttributeAllocRatioTableManager::Instance().Load();
	}

	void SetUp() override
	{
		LoadTables();
		ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));
		// 进程级状态:先卸一次,别的文件的用例若忘了卸也不会漏进来。
		BagService::SetItemInstanceInitializer({});
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());
		Tip().Clear();
		player = NewPlayer(kPlayerGuid, kMaxLevel);
	}

	void TearDown() override
	{
		BagService::SetItemInstanceInitializer({});
		Tip().Clear();
		for (const auto entity : players)
		{
			if (tlsEcs.actorRegistry.valid(entity))
			{
				tlsEcs.actorRegistry.destroy(entity);
			}
		}
		// 个别用例在内存里改过表行:重新加载,别把改动留给后面的用例。
		LoadTables();
		// 恢复其它背包用例使用的无 I/O 号段基线(与 bag_test.cpp 的 main() 同一段范围)。
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	// ── 前置条件:每条用例都建立在这些行上。表没导 / 被改了就在这里说清楚 ──────
	static void RequireItemRow(uint32_t configId, uint32_t equipKind, uint32_t maxStack)
	{
		const auto *row = ItemRowOrNull(configId);
		ASSERT_NE(nullptr, row) << "Item row " << configId << " is missing; " << kExportFirst;
		ASSERT_EQ(equipKind, row->equip_kind()) << "Item row " << configId;
		ASSERT_EQ(maxStack == 1, row->max_stack_size() == 1) << "Item row " << configId;
	}

	static void RequireSampleEquipment(uint32_t configId, uint32_t equipKind, uint32_t level)
	{
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(configId, equipKind, 1));
		const auto *row = ItemRowOrNull(configId);
		ASSERT_EQ(level, row->equip_level()) << "Item row " << configId << "; " << kExportFirst;
		ASSERT_EQ(0u, row->equip_class()) << "Item row " << configId;
		ASSERT_NE(0u, row->affix_pool()) << "Item row " << configId;
		ASSERT_NE(0u, row->affix_rule()) << "Item row " << configId;
		ASSERT_FALSE(row->name().empty()) << "Item row " << configId;
		ASSERT_GT(row->base_attr_size(), 0) << "Item row " << configId;
	}

	static void RequireSlotRow(uint32_t slot, uint32_t equipKind)
	{
		const auto *row = EquipSlotTableManager::Instance().FindByIdSilent(slot).first;
		ASSERT_NE(nullptr, row) << "EquipSlot row " << slot << " is missing; " << kExportFirst;
		ASSERT_EQ(equipKind, row->equip_kind()) << "EquipSlot row " << slot;
	}

	// 属性 id 的含义是契约(设计文档 §2.1);这里核对一遍,后面的期望值才站得住。
	static void RequireAttr(uint32_t attrId, equiprules::EffectKind effect, uint32_t param,
							bool hasCapRows)
	{
		const auto *row = AttrRowOrNull(attrId);
		ASSERT_NE(nullptr, row) << "EquipAttribute row " << attrId << " is missing; " << kExportFirst;
		ASSERT_EQ(equiprules::ToRaw(effect), row->effect()) << "EquipAttribute row " << attrId;
		if (effect != equiprules::EffectKind::kPrimaryPoint)
		{
			ASSERT_EQ(param, row->effect_param()) << "EquipAttribute row " << attrId;
		}
		else
		{
			ASSERT_NE(0u, row->effect_param()) << "EquipAttribute row " << attrId;
		}
		ASSERT_FALSE(row->name().empty()) << "EquipAttribute row " << attrId;
		if (hasCapRows)
		{
			for (const uint32_t level : {kLevel1, kLevel80})
			{
				const auto *capRow = CapRowOrNull(attrId, level);
				ASSERT_NE(nullptr, capRow) << "EquipAttributeCap row for attr " << attrId << " level "
										   << level << " is missing; " << kExportFirst;
				ASSERT_GE(capRow->min_value(), 1u);
				ASSERT_GE(capRow->cap(), capRow->min_value());
			}
		}
	}

	static void RequireFixtureRows()
	{
		using equiprules::CombatStat;
		using equiprules::DerivedStat;
		using equiprules::EffectKind;

		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kOldEquipA, kOldKind, 1));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kOldEquipB, kOldKind, 1));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kPlainNonStack, 0, 1));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kStackable, 0, 999));
		ASSERT_EQ(nullptr, ItemRowOrNull(kUnknownConfig));
		ASSERT_NO_FATAL_FAILURE(RequireSampleEquipment(kWeaponL1, kWeaponKind, kLevel1));
		ASSERT_NO_FATAL_FAILURE(RequireSampleEquipment(kWeaponL80, kWeaponKind, kLevel80));
		ASSERT_NO_FATAL_FAILURE(RequireSampleEquipment(kHatL1, kHatKind, kLevel1));
		ASSERT_NO_FATAL_FAILURE(RequireSampleEquipment(kShoesL1, kShoesKind, kLevel1));
		ASSERT_EQ(kWeaponPool, ItemRowOrNull(kWeaponL80)->affix_pool());

		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kOldSlotFirst, kOldKind));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kOldSlotSecond, kOldKind));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kWeaponSlot, kWeaponKind));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kHatSlot, kHatKind));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kClothesSlot, kClothesKind));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kShoesSlot, kShoesKind));

		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrDamage, EffectKind::kBothAttacks, 0, true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrAccuracy, EffectKind::kDerivedFlat,
											equiprules::ToRaw(DerivedStat::kPhysicalAttack), true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrStrength, EffectKind::kPrimaryPoint, 0, true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrAllPrimary, EffectKind::kAllPrimaryPoints, 0, true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrDefense, EffectKind::kDerivedFlat,
											equiprules::ToRaw(DerivedStat::kDefense), true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrHealth, EffectKind::kDerivedFlat,
											equiprules::ToRaw(DerivedStat::kMaxHealth), true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrSpeed, EffectKind::kDerivedFlat,
											equiprules::ToRaw(DerivedStat::kSpeed), false));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrPhysicalCrit, EffectKind::kCombat,
											equiprules::ToRaw(CombatStat::kPhysicalCritRate), true));
		ASSERT_NO_FATAL_FAILURE(RequireAttr(kAttrReflect, EffectKind::kCombat,
											equiprules::ToRaw(CombatStat::kReflectRate), true));
		ASSERT_EQ(nullptr, AttrRowOrNull(kUnknownAttr));
		// 速度只作基础属性:它在上限表里一档都没有,「查不到上限」的用例靠这一点。
		ASSERT_TRUE(EquipAttributeCapTableManager::Instance().GetByAttrId(kAttrSpeed).empty());

		ASSERT_GT(BaseValueOf(kWeaponL1, kAttrDamage), 0u) << kExportFirst;
		ASSERT_GT(BaseValueOf(kHatL1, kAttrDefense), 0u) << kExportFirst;
		ASSERT_GT(BaseValueOf(kShoesL1, kAttrDefense), 0u) << kExportFirst;
		ASSERT_GT(BaseValueOf(kShoesL1, kAttrSpeed), 0u) << kExportFirst;
	}

	// 一个能让 PlayerAttributeSystem::Recalculate 跑通的最小玩家:身份、四个背包、等级、职业、基础属性。
	// 没有会话组件,所以 PushPanel 只会打一条 WARN 然后返回。
	entt::entity NewPlayer(uint64_t guid, uint32_t level)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<Guid>(entity, Guid{guid});
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		for (auto &bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{guid});
		}
		tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(level);
		tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(entity);
		auto &attributes = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
		attributes.set_health(100);
		attributes.set_mana(50);
		return entity;
	}

	TipInfoMessage &Tip()
	{
		return tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity());
	}
	Bag &Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
	Bag &Equipment() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kEquipment]; }
	Bag &Warehouse() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kWarehouse]; }

	void SetLevel(uint32_t level) { tlsEcs.actorRegistry.get<LevelComp>(player).set_level(level); }
	void SetClass(uint32_t classId)
	{
		tlsEcs.actorRegistry.get<PlayerUint32Comp>(player).set_class_(classId);
	}

	// 两个包都没变:拒绝路径的统一断言。
	void ExpectBagsUnchanged(const BagState &inventoryBefore, const BagState &equipmentBefore)
	{
		EXPECT_EQ(inventoryBefore, Capture(Inventory())) << "a refused call must not touch the inventory";
		EXPECT_EQ(equipmentBefore, Capture(Equipment())) << "a refused call must not touch the equipment";
		EXPECT_TRUE(Inventory().IsLayerConsistent());
		EXPECT_TRUE(Equipment().IsLayerConsistent());
	}

	equiprules::EquipBonus Collect()
	{
		equiprules::EquipBonus bonus;
		PlayerEquipSystem::CollectBonus(player, bonus);
		return bonus;
	}

	static void ExpectSameBonus(const equiprules::EquipBonus &expected,
								const equiprules::EquipBonus &actual)
	{
		EXPECT_EQ(expected.primaryPoints, actual.primaryPoints);
		EXPECT_EQ(expected.allPrimaryPoints, actual.allPrimaryPoints);
		EXPECT_EQ(expected.derivedFlat, actual.derivedFlat);
		EXPECT_EQ(expected.combat, actual.combat);
	}

	std::vector<entt::entity> players;
	entt::entity player{entt::null};
	SceneBagClientPlayerHandler handler{std::make_unique<TestBagService>()};
};

// ── 表校验 ──────────────────────────────────────────────────────────────────

// 导出的六张表互相自洽:池里的属性都有定义和上限、装备都不可叠加、基础属性都指向真实的属性行、
// 15 项战斗属性各有一条显示行(缺了属性面板会静默少一行)。
// 这些引用大多没有导表期外键,填错只会在启动日志里出一行 ERROR —— 钉成用例才不会被忽略。
TEST_F(PlayerEquipTest, ShippedTablesPassValidation)
{
	EXPECT_EQ(0u, PlayerEquipSystem::ValidateTables());
}

// ValidateTables 的每一项检查各注入一种坏数据,每次恰好多报一条:哪一项判断写反了(比如 > 写成 <),
// 上面那条「导出的表自洽」会红;哪一项压根没在查,这一条会红。改动是累加的,所以每一步都挑还没动过的行。
TEST_F(PlayerEquipTest, ValidationCountsEachBrokenRow)
{
	ASSERT_EQ(0u, PlayerEquipSystem::ValidateTables());
	uint32_t expected = 0;

	// 装备被配成可堆叠(设计文档 §5 不变量 7)。
	Editable(ItemRowOrNull(kWeaponL80)).set_max_stack_size(5);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 基础属性指向不存在的属性(子消息列没有导表期外键)。
	Editable(ItemRowOrNull(kWeaponL1)).mutable_base_attr(0)->set_base_attr_id(kUnknownAttr);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 属性池号指向一个不存在的池(指向的不是主键,同样没有外键)。
	Editable(ItemRowOrNull(kHatL1)).set_affix_pool(777);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 效果种类 / 参数不被规则层接受:战斗属性号越界。
	// 「反震率」这一项从此也没有显示行了,但那是这一行填错的直接后果 —— 同一个根因,只计这一条。
	Editable(AttrRowOrNull(kAttrReflect)).set_effect_param(99);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 战斗属性缺显示行:把「抗法术」那一行也指向抗物理。两行各自都合法(上面那一项查不出来),
	// 但「抗法术」这一项从此没有显示行,属性面板会静默少一行。上一步被拒的那一行只抵掉它自己
	// 造成的那一项缺行,抵不掉这一项。
	const auto *magicResist = AttrRowOrNull(kAttrMagicResist);
	const auto *physicalResist = AttrRowOrNull(kAttrPhysicalResist);
	ASSERT_NE(nullptr, magicResist) << kExportFirst;
	ASSERT_NE(nullptr, physicalResist) << kExportFirst;
	ASSERT_EQ(equiprules::ToRaw(equiprules::EffectKind::kCombat), magicResist->effect());
	ASSERT_EQ(equiprules::ToRaw(equiprules::EffectKind::kCombat), physicalResist->effect());
	ASSERT_NE(magicResist->effect_param(), physicalResist->effect_param());
	Editable(magicResist).set_effect_param(physicalResist->effect_param());
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 一级属性点指向不存在的维度。
	Editable(AttrRowOrNull(kAttrStrength)).set_effect_param(987654);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 一级属性点指向宝宝池的维度:维度存在、规则层也收,但角色的属性重算只读角色池维度,
	// 加的点同样不进任何属性。
	const uint32_t petDimension = NonPlayerDimensionOrZero();
	ASSERT_NE(0u, petDimension) << "the fixture needs a dimension outside the player pool";
	const auto *constitution = AttrRowOrNull(kAttrConstitution);
	ASSERT_NE(nullptr, constitution) << kExportFirst;
	ASSERT_EQ(equiprules::ToRaw(equiprules::EffectKind::kPrimaryPoint), constitution->effect());
	Editable(constitution).set_effect_param(petDimension);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 上限区间倒挂。
	const auto *capRow = CapRowOrNull(kAttrDamage, kLevel80);
	Editable(capRow).set_min_value(capRow->cap() + 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 属性池的三项,各用一行:权重为 0;指向不存在的属性;指向一个在上限表里一档都没有的属性。
	// 第二种在上限表里自然也查不到,那是同一个根因,只计一条。
	const auto &poolRows = EquipAffixPoolTableManager::Instance().GetByPoolId(kWeaponPool);
	ASSERT_GE(poolRows.size(), 3u) << kExportFirst;
	Editable(poolRows[0]).set_weight(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	Editable(poolRows[1]).set_attr_id(kUnknownAttr);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	Editable(poolRows[2]).set_attr_id(kAttrSpeed); // 速度只作基础属性,没有上限行(夹具核对过)
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 掷值规则的两项落在同一行上,互不相干,各计一条:蓝属性条数区间倒挂;概率超过万分之一万。
	const auto *ruleRow =
		EquipAffixRuleTableManager::Instance().FindByIdSilent(ItemRowOrNull(kWeaponL80)->affix_rule()).first;
	ASSERT_NE(nullptr, ruleRow) << kExportFirst;
	Editable(ruleRow).set_blue_min(ruleRow->blue_max() + 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	Editable(ruleRow).set_pink_rate(equiprules::kPermyriadBase + 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 装备的掷值规则号指向不存在的规则。
	constexpr uint32_t kMissingRule = 777;
	ASSERT_FALSE(EquipAffixRuleTableManager::Instance().Exists(kMissingRule));
	Editable(ItemRowOrNull(kShoesL1)).set_affix_rule(kMissingRule);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
}

// ── 新铸实例:RollNewInstance ───────────────────────────────────────────────

TEST_F(PlayerEquipTest, RollNewInstanceDrawsFromTheWeaponPoolWithinCaps)
{
	std::set<std::string> distinctRolls;
	for (uint64_t i = 0; i < 200; ++i)
	{
		ItemComp item = MakeInstance(kGuidA + i, kWeaponL80);
		PlayerEquipSystem::RollNewInstance(item);
		ASSERT_NO_FATAL_FAILURE(ExpectValidRoll(item, kWeaponL80, kLevel80)) << "roll " << i;
		distinctRolls.insert(item.equip().SerializeAsString());
		if (testing::Test::HasFailure())
		{
			break; // 一次失败就够说明问题,别刷两百遍
		}
	}
	EXPECT_GT(distinctRolls.size(), 1u) << "two hundred rolls must not all come out identical";
}

TEST_F(PlayerEquipTest, RollNewInstanceNeverRerolls)
{
	ItemComp item = MakeInstance(kGuidA, kWeaponL80);
	PlayerEquipSystem::RollNewInstance(item);
	ASSERT_TRUE(item.has_equip());
	const std::string first = item.SerializeAsString();
	for (int i = 0; i < 20; ++i)
	{
		PlayerEquipSystem::RollNewInstance(item);
	}
	EXPECT_EQ(first, item.SerializeAsString()) << "an initialised instance is never rolled again";

	// 「已初始化」只看 presence:一条都没有的既有实例同样不许补掷。
	ItemComp emptyRolled = MakeInstance(kGuidB, kWeaponL80);
	emptyRolled.mutable_equip();
	PlayerEquipSystem::RollNewInstance(emptyRolled);
	EXPECT_TRUE(emptyRolled.has_equip());
	EXPECT_EQ(0, emptyRolled.equip().affixes_size()) << "zero rows is not 'not rolled yet'";

	// 带着别处来的属性(邮件回流等)进来的实例原样保留。
	ItemComp carried = MakeRolledInstance(kGuidC, kWeaponL80);
	const std::string carriedBefore = carried.SerializeAsString();
	PlayerEquipSystem::RollNewInstance(carried);
	EXPECT_EQ(carriedBefore, carried.SerializeAsString());
}

TEST_F(PlayerEquipTest, RollNewInstanceLeavesNonEquipmentUntouched)
{
	for (const uint32_t configId : {kPlainNonStack, kStackable, kUnknownConfig})
	{
		ItemComp item = MakeInstance(kGuidA, configId);
		const std::string before = item.SerializeAsString();
		PlayerEquipSystem::RollNewInstance(item);
		EXPECT_FALSE(item.has_equip()) << "config " << configId << " must not grow an equip section";
		EXPECT_EQ(before, item.SerializeAsString()) << "config " << configId;
	}
}

// 既有装备行没有属性池:照样置位(以后给它配了池也不会补掷),只是一条都不掷。
TEST_F(PlayerEquipTest, RollNewInstanceMarksPoolLessEquipmentAsInitialised)
{
	ItemComp item = MakeInstance(kGuidA, kOldEquipA);
	PlayerEquipSystem::RollNewInstance(item);
	EXPECT_TRUE(item.has_equip());
	EXPECT_EQ(0, item.equip().affixes_size());
}

// 回调只许写 equip 段:身份、模板、数量、入包序号一个都不碰。
TEST_F(PlayerEquipTest, RollNewInstanceOnlyWritesTheEquipSection)
{
	ItemComp item = MakeInstance(kGuidA, kWeaponL80);
	item.set_acquire_seq(77);
	PlayerEquipSystem::RollNewInstance(item);
	ASSERT_TRUE(item.has_equip());
	EXPECT_EQ(kGuidA, item.item_id());
	EXPECT_EQ(kWeaponL80, item.config_id());
	EXPECT_EQ(1u, item.size());
	EXPECT_EQ(77u, item.acquire_seq());
}

// 装备被误配成可堆叠:不置位(置位会让它从此不能并堆),也不掷。
TEST_F(PlayerEquipTest, RollNewInstanceSkipsEquipmentMisconfiguredAsStackable)
{
	Editable(ItemRowOrNull(kWeaponL80)).set_max_stack_size(5);
	ItemComp item = MakeInstance(kGuidA, kWeaponL80);
	PlayerEquipSystem::RollNewInstance(item);
	EXPECT_FALSE(item.has_equip());
}

// 属性池 / 规则只配了一边 = 策划配的「不掷」:置位、零条。
TEST_F(PlayerEquipTest, RollNewInstanceWithoutPoolOrRuleRollsNothing)
{
	Editable(ItemRowOrNull(kWeaponL80)).set_affix_rule(0);
	ItemComp noRule = MakeInstance(kGuidA, kWeaponL80);
	PlayerEquipSystem::RollNewInstance(noRule);
	EXPECT_TRUE(noRule.has_equip());
	EXPECT_EQ(0, noRule.equip().affixes_size());

	// 池号指向一个没有任何行的池:表不一致,降级成不带随机属性,仍然置位。
	Editable(ItemRowOrNull(kWeaponL1)).set_affix_pool(777);
	ItemComp noPool = MakeInstance(kGuidB, kWeaponL1);
	PlayerEquipSystem::RollNewInstance(noPool);
	EXPECT_TRUE(noPool.has_equip());
	EXPECT_EQ(0, noPool.equip().affixes_size());
}

// ── 挂点:经 BagService 入包的新装备自带属性 ─────────────────────────────────

TEST_F(PlayerEquipTest, InstalledInitializerRollsEquipmentGrantedThroughBagService)
{
	PlayerItemBlockList blockList;

	// 没装回调时,入包的装备没有 equip 段 —— 属性确实是回调掷的,不是别处顺手写的。
	ItemCountMap one;
	one[kWeaponL80] = 1;
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), blockList, one));
	Inventory().ForEachItem([](Guid, const ItemComp &item) { EXPECT_FALSE(item.has_equip()); });
	const BagState unrolled = Capture(Inventory());

	PlayerEquipSystem::InstallItemInitializer();
	ItemCountMap batch;
	batch[kWeaponL80] = 3;
	batch[kPlainNonStack] = 1;
	batch[kStackable] = 5;
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), blockList, batch));

	uint32_t rolledWeapons = 0;
	Inventory().ForEachItem(
		[&](Guid guid, const ItemComp &item)
		{
			if (unrolled.count(guid) != 0)
			{
				EXPECT_FALSE(item.has_equip()) << "installing the initializer never rolls existing items";
				return;
			}
			if (item.config_id() == kWeaponL80)
			{
				++rolledWeapons;
				ASSERT_NO_FATAL_FAILURE(ExpectValidRoll(item, kWeaponL80, kLevel80));
			}
			else
			{
				EXPECT_FALSE(item.has_equip()) << "config " << item.config_id() << " is not equipment";
			}
		});
	EXPECT_EQ(3u, rolledWeapons) << "each piece of a count = 3 grant is rolled on its own";
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 安装不带「已安装」标志:回调被卸掉之后再调 InstallItemInitializer,必须重新装上。
// 背包测试夹具每个用例的 SetUp / TearDown 都会卸回调;靠静态标志做幂等的话,同一个 bag_test.exe 里
// 排在后面的用例(以及任何卸过回调又想装回去的进程)从此发出去的装备都没有属性,且没有任何报错。
// 这条在单个用例里把「装 → 卸 → 再装」走完,单独过滤跑它也能发现。
TEST_F(PlayerEquipTest, InstallItemInitializerReinstallsAfterTheCallbackWasRemoved)
{
	PlayerItemBlockList blockList;
	ItemCountMap one;
	one[kWeaponL80] = 1;

	PlayerEquipSystem::InstallItemInitializer();
	BagService::SetItemInstanceInitializer({}); // 卸载
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), blockList, one));
	Inventory().ForEachItem([](Guid, const ItemComp &item)
							{ EXPECT_FALSE(item.has_equip()) << "the callback is really gone"; });
	const BagState unrolled = Capture(Inventory());

	PlayerEquipSystem::InstallItemInitializer(); // 卸掉之后再装:必须真的装上
	PlayerEquipSystem::InstallItemInitializer(); // 连调两次无害
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), blockList, one));

	uint32_t rolledWeapons = 0;
	Inventory().ForEachItem(
		[&](Guid guid, const ItemComp &item)
		{
			if (unrolled.count(guid) != 0)
			{
				return;
			}
			++rolledWeapons;
			ASSERT_NO_FATAL_FAILURE(ExpectValidRoll(item, kWeaponL80, kLevel80));
		});
	EXPECT_EQ(1u, rolledWeapons) << "the piece granted after re-installing is rolled";
}

TEST_F(PlayerEquipTest, GmGrantItemGrantsRolledEquipmentIntoTheInventory)
{
	PlayerEquipSystem::InstallItemInitializer();
	ASSERT_EQ(kSuccess, PlayerEquipSystem::GmGrantItem(player, kWeaponL80, 2));
	EXPECT_EQ(2u, Inventory().GetTotalItemCount(kWeaponL80));
	EXPECT_EQ(2u, Inventory().OccupiedGridCount()) << "two separate instances";
	Inventory().ForEachItem([](Guid, const ItemComp &item)
							{ ASSERT_NO_FATAL_FAILURE(ExpectValidRoll(item, kWeaponL80, kLevel80)); });
	EXPECT_EQ(0u, Equipment().OccupiedGridCount()) << "granted items are not worn";

	// 不限于装备:可叠加物品发的是数量。
	ASSERT_EQ(kSuccess,
			  PlayerEquipSystem::GmGrantItem(player, kStackable, PlayerEquipSystem::kGmGrantMaxCount));
	EXPECT_EQ(uint64_t{PlayerEquipSystem::kGmGrantMaxCount}, Inventory().GetTotalItemCount(kStackable));
}

TEST_F(PlayerEquipTest, GmGrantItemRejectsInvalidArgumentsAndBlockedStates)
{
	PlayerEquipSystem::InstallItemInitializer();
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	EXPECT_EQ(kEquipGrantInvalid, PlayerEquipSystem::GmGrantItem(player, 0, 1));
	EXPECT_EQ(kEquipGrantInvalid, PlayerEquipSystem::GmGrantItem(player, kUnknownConfig, 1));
	EXPECT_EQ(kEquipGrantInvalid, PlayerEquipSystem::GmGrantItem(player, kWeaponL80, 0));
	EXPECT_EQ(kEquipGrantInvalid,
			  PlayerEquipSystem::GmGrantItem(player, kWeaponL80, PlayerEquipSystem::kGmGrantMaxCount + 1));
	EXPECT_EQ(kEquipGrantInvalid,
			  PlayerEquipSystem::GmGrantItem(player, kWeaponL80, std::numeric_limits<uint32_t>::max()));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	EXPECT_EQ(kEntityIsNull, PlayerEquipSystem::GmGrantItem(entt::null, kWeaponL80, 1));

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::GmGrantItem(player, kWeaponL80, 1));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::GmGrantItem(player, kWeaponL80, 1));
	tlsEcs.actorRegistry.remove<InBattleComp>(player);
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	// 背包放不下:整批拒绝(原样透传容器的错误码),一件都不进包。
	Inventory().SetCapacityForRestore(1);
	EXPECT_NE(kSuccess, PlayerEquipSystem::GmGrantItem(player, kWeaponL80, 2));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	// 还没加载出背包的玩家。
	const auto bagless = tlsEcs.actorRegistry.create();
	players.push_back(bagless);
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::GmGrantItem(bagless, kWeaponL80, 1));
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(bagless));
}

// ── 穿上 ────────────────────────────────────────────────────────────────────

TEST_F(PlayerEquipTest, EquipMovesTheSameInstanceIntoTheWeaponSlot)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL80);
	Inventory().InsertItemForRestore(weapon, 5);
	Inventory().InsertItemForRestore(MakeInstance(kGuidB, kPlainNonStack), 6);
	ASSERT_EQ(nullptr, tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));

	EXPECT_EQ(nullptr, Inventory().GetItemCompByGuid(kGuidA)) << "it left the inventory";
	EXPECT_EQ(kInvalidGuid, Inventory().Layout().At(5)) << "and released its cell";
	EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kGuidB)) << "the bystander stays";
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kGuidA, Equipment().Layout().At(kWeaponSlot));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	EXPECT_EQ(1u, Equipment().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());

	// 穿上之后属性重算过(数值本身由 player_equip_attribute_test.cpp 验)。
	EXPECT_NE(nullptr, tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player));
}

TEST_F(PlayerEquipTest, EquipChecksTheLevelRequirement)
{
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL80), 0);
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	SetLevel(kLevel80 - 1);
	EXPECT_EQ(kEquipLevelNotEnough, PlayerEquipSystem::Equip(player, kGuidA));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	SetLevel(kLevel80); // 恰好够
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
}

TEST_F(PlayerEquipTest, EquipChecksTheClassRequirement)
{
	// 样例装备都不限职业,内存里给这一行加上职业要求。
	Editable(ItemRowOrNull(kWeaponL1)).set_equip_class(7);
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL1), 0);
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	SetClass(3);
	EXPECT_EQ(kEquipClassMismatch, PlayerEquipSystem::Equip(player, kGuidA));
	SetClass(0); // 职业未定的角色也不满足具体的职业要求
	EXPECT_EQ(kEquipClassMismatch, PlayerEquipSystem::Equip(player, kGuidA));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	SetClass(7);
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
}

TEST_F(PlayerEquipTest, EquipRejectsWhatCannotBeWornAndChangesNothing)
{
	Inventory().InsertItemForRestore(MakeInstance(kGuidA, kPlainNonStack), 0);
	Inventory().InsertItemForRestore(kGuidB, kStackable, /*stackSize=*/5, /*pos=*/1);
	Warehouse().InsertItemForRestore(MakeRolledInstance(kGuidC, kWeaponL1), 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeRolledInstance(kGuidD, kWeaponL1)));
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());
	const BagState warehouseBefore = Capture(Warehouse());

	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Equip(player, kGuidA)) << "not equipment";
	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Equip(player, kGuidB)) << "a stack is not equipment";
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, kGuidMissing));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, 0));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, kInvalidGuid));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, kGuidC))
		<< "only items in the character inventory can be worn, not the warehouse";
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Equip(player, kGuidD)) << "already worn";

	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));
}

TEST_F(PlayerEquipTest, EquipWithoutAnAcceptingSlotIsRefused)
{
	// 一个没有任何槽位接受的部位(槽位表里没有这个部位的行)。
	Editable(ItemRowOrNull(kWeaponL1)).set_equip_kind(987);
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL1), 0);
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());
	EXPECT_EQ(kEquipNoSlot, PlayerEquipSystem::Equip(player, kGuidA));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);
}

// 部位 1 有 0 / 1 两个槽:先填槽号小的空槽;都占着就替换槽号最小的那只,另一只不动。
TEST_F(PlayerEquipTest, EquipFillsTheFirstEmptySlotThenReplacesTheLowestSlot)
{
	const ItemComp first = MakeRolledInstance(kGuidA, kOldEquipA);
	ItemComp second = MakeInstance(kGuidB, kOldEquipB);
	second.mutable_equip();
	const ItemComp third = MakeInstance(kGuidC, kOldEquipA);
	Inventory().InsertItemForRestore(first, 0);
	Inventory().InsertItemForRestore(second, 1);
	Inventory().InsertItemForRestore(third, 2);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kOldSlotFirst, Equipment().GetItemPosByGuid(kGuidA));
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidB));
	EXPECT_EQ(kOldSlotSecond, Equipment().GetItemPosByGuid(kGuidB)) << "the first empty slot";
	EXPECT_EQ(kOldSlotFirst, Equipment().GetItemPosByGuid(kGuidA)) << "nothing was replaced";

	// 两个槽都占着:第三件替换 0 号槽,原来那件回背包,实例数据不变。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidC));
	EXPECT_EQ(kOldSlotFirst, Equipment().GetItemPosByGuid(kGuidC));
	EXPECT_EQ(kOldSlotSecond, Equipment().GetItemPosByGuid(kGuidB)) << "the higher slot is untouched";
	EXPECT_EQ(nullptr, Equipment().GetItemCompByGuid(kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), first));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), second));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), third));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	EXPECT_EQ(2u, Equipment().OccupiedGridCount());

	// 腾出 0 号槽之后再穿:落回空着的 0 号槽,而不是去替换 1 号槽。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidC));
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kOldSlotFirst, Equipment().GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kOldSlotSecond, Equipment().GetItemPosByGuid(kGuidB));
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
}

// 只有一个武器槽:换武器时旧武器回背包。背包此刻是满的 —— 新武器刚腾出的那一格正好给它。
TEST_F(PlayerEquipTest, EquipReplacingTheWeaponReturnsTheOldOneEvenWhenTheInventoryIsFull)
{
	const ItemComp oldWeapon = MakeRolledInstance(kGuidA, kWeaponL80);
	ItemComp newWeapon = MakeInstance(kGuidB, kWeaponL1);
	AddAffix(newWeapon, kAttrDamage, kBlue, /*value=*/3, /*seq=*/0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(oldWeapon, kWeaponSlot));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(newWeapon));
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidB));

	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidB));
	EXPECT_EQ(nullptr, Equipment().GetItemCompByGuid(kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), newWeapon));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), oldWeapon));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	EXPECT_EQ(1u, Equipment().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
}

TEST_F(PlayerEquipTest, EquipAndUnequipAreRefusedWhileFrozenOrInBattle)
{
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL1), 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeRolledInstance(kGuidB, kHatL1)));
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	// 跨区冻结:存盘已经出去了,此刻换装目标节点读不到。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::Unequip(player, kGuidB));
	// 两个状态同时在:先报冻结(与 PetSystem / PlayerAttributeSystem 的判定顺序一致)。
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::Equip(player, kGuidA));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);

	// 战斗在途:快照已出,换装会让局内数值与面板分叉。
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::Unequip(player, kGuidB));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);

	tlsEcs.actorRegistry.remove<InBattleComp>(player);
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidB));
}

TEST_F(PlayerEquipTest, WriteEntriesRejectInvalidEntitiesAndPlayersWithoutBags)
{
	EXPECT_EQ(kEntityIsNull, PlayerEquipSystem::Equip(entt::null, kGuidA));
	EXPECT_EQ(kEntityIsNull, PlayerEquipSystem::Unequip(entt::null, kGuidA));

	const auto bagless = tlsEcs.actorRegistry.create();
	players.push_back(bagless);
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Equip(bagless, kGuidA));
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Unequip(bagless, kGuidA));
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(bagless))
		<< "a write entry must not create the bags as a side effect";
}

// ── 卸下 ────────────────────────────────────────────────────────────────────

TEST_F(PlayerEquipTest, UnequipReturnsTheSameInstanceToTheInventory)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL80);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_EQ(nullptr, tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidA));

	EXPECT_EQ(nullptr, Equipment().GetItemCompByGuid(kGuidA));
	EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(kWeaponSlot));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	EXPECT_EQ(0u, Equipment().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
	EXPECT_NE(nullptr, tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player))
		<< "taking equipment off recalculates attributes too";

	// 穿回去再脱下来:几趟往返属性都不变(穿脱不重掷)。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
}

TEST_F(PlayerEquipTest, UnequipIsRefusedWhenTheInventoryIsFull)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL80);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeInstance(kGuidB, kPlainNonStack)));
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	EXPECT_EQ(kEquipBagFull, PlayerEquipSystem::Unequip(player, kGuidA));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA)) << "still worn, in the same slot";

	// 腾出一格之后就能卸了。
	ASSERT_EQ(kSuccess, Inventory().RemoveItem(kGuidB));
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
}

TEST_F(PlayerEquipTest, UnequipRejectsItemsThatAreNotWorn)
{
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL1), 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeRolledInstance(kGuidB, kHatL1)));
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	EXPECT_EQ(kEquipNotEquipped, PlayerEquipSystem::Unequip(player, kGuidA)) << "it is in the inventory";
	EXPECT_EQ(kEquipNotEquipped, PlayerEquipSystem::Unequip(player, kGuidMissing));
	EXPECT_EQ(kEquipNotEquipped, PlayerEquipSystem::Unequip(player, 0));
	ExpectBagsUnchanged(inventoryBefore, equipmentBefore);
}

// ── 回滚:搬到一半目标包拒收 ────────────────────────────────────────────────
//
// 纯校验挡不住的失败只有一种能从公开接口稳定造出来:同一个 guid 同时出现在两个包里(数据腐化)。
// 穿脱的校验不查「目标包里有没有同 guid」,而 Bag::PutInstance 会拒 —— 此时源包的那件已经取出来了,
// 正好落进回滚。三条用例各走一个失败点,钉的是同一件事:被搬的实例回到原包原槽、实例数据一字不差,
// 没参与搬运的东西纹丝不动。
// 被搬过的包不能拿整份序列化去比:放回时 acquire_seq 由该包重新盖章,所以逐项比身份 / 槽位 / equip 段。

// 穿上的第三步(新装备放进装备栏)失败:新装备回人物背包原格。
TEST_F(PlayerEquipTest, EquipPutsTheItemBackWhenTheEquipmentRefusesIt)
{
	const ItemComp hat = MakeRolledInstance(kGuidA, kHatL1);
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL1); // 与穿着的帽子同 guid
	AddAffix(weapon, kAttrDamage, kBlue, /*value=*/3, /*seq=*/0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(hat));
	ASSERT_EQ(kHatSlot, Equipment().GetItemPosByGuid(kGuidA));
	Inventory().InsertItemForRestore(MakeInstance(kGuidB, kPlainNonStack), 0);
	Inventory().InsertItemForRestore(weapon, 2);
	ASSERT_EQ(2u, Inventory().GetItemPosByGuid(kGuidA));
	const BagState inventoryBefore = Capture(Inventory());
	const BagState equipmentBefore = Capture(Equipment());

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Equip(player, kGuidA));

	EXPECT_EQ(2u, Inventory().GetItemPosByGuid(kGuidA)) << "back in the cell it came from";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
	const BagState inventoryAfter = Capture(Inventory());
	ASSERT_EQ(1u, inventoryAfter.count(kGuidB));
	EXPECT_EQ(inventoryBefore.at(kGuidB), inventoryAfter.at(kGuidB)) << "the bystander is untouched";
	EXPECT_EQ(2u, Inventory().OccupiedGridCount());
	EXPECT_EQ(equipmentBefore, Capture(Equipment())) << "the equipment refused it and was never modified";
	EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(kWeaponSlot));
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
}

// 穿上的第四步(被替换的旧装备回人物背包)失败:新装备从装备栏取回、旧装备回原槽、新装备回原格。
TEST_F(PlayerEquipTest, EquipRestoresBothPiecesWhenTheReplacedOneCannotEnterTheInventory)
{
	const ItemComp oldWeapon = MakeRolledInstance(kGuidB, kWeaponL80);
	ItemComp newWeapon = MakeInstance(kGuidA, kWeaponL1);
	AddAffix(newWeapon, kAttrDamage, kBlue, /*value=*/3, /*seq=*/0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(oldWeapon, kWeaponSlot));
	Inventory().InsertItemForRestore(MakeInstance(kGuidB, kPlainNonStack), 0); // 与旧武器同 guid
	Inventory().InsertItemForRestore(newWeapon, 3);
	ASSERT_EQ(3u, Inventory().GetItemPosByGuid(kGuidA));
	const BagState inventoryBefore = Capture(Inventory());

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Equip(player, kGuidA));

	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidB)) << "the old weapon is back in its slot";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), oldWeapon));
	EXPECT_EQ(nullptr, Equipment().GetItemCompByGuid(kGuidA)) << "the new weapon was taken back off";
	EXPECT_EQ(1u, Equipment().OccupiedGridCount());
	EXPECT_EQ(3u, Inventory().GetItemPosByGuid(kGuidA)) << "the new weapon is back in the cell it came from";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), newWeapon));
	const BagState inventoryAfter = Capture(Inventory());
	ASSERT_EQ(1u, inventoryAfter.count(kGuidB));
	EXPECT_EQ(inventoryBefore.at(kGuidB), inventoryAfter.at(kGuidB))
		<< "the inventory item that shares the old weapon's guid is untouched";
	EXPECT_EQ(2u, Inventory().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
}

// 卸下的第二步(放进人物背包)失败:装备回装备栏原槽。
TEST_F(PlayerEquipTest, UnequipPutsTheItemBackWhenTheInventoryRefusesIt)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL80);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	Inventory().InsertItemForRestore(MakeInstance(kGuidA, kPlainNonStack), 0); // 与穿着的武器同 guid
	const BagState inventoryBefore = Capture(Inventory());

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Unequip(player, kGuidA));

	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA)) << "still worn, in the same slot";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));
	EXPECT_EQ(1u, Equipment().OccupiedGridCount());
	EXPECT_EQ(inventoryBefore, Capture(Inventory())) << "the inventory refused it and was never modified";
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
}

// ── 装备加成:CollectBonus ──────────────────────────────────────────────────

// 帽子 + 鞋子 + 武器,每件带几条手工写好的随机属性。期望值逐项精确:基础属性读 Item 行,
// 随机属性的值取自上限行(下限 / 上限 / 超上限),所以策划调数值不会把这条用例调红,
// 而任何一项加错了字段(物伤加到法伤、防御加到速度)都会红。
TEST_F(PlayerEquipTest, CollectBonusSumsBaseAttributesAndAffixesExactly)
{
	const uint32_t strengthCap = CapOf(kAttrStrength, kLevel1);
	const uint32_t allPrimaryCap = CapOf(kAttrAllPrimary, kLevel1);
	const uint32_t healthMin = MinOf(kAttrHealth, kLevel1);
	const uint32_t reflectCap = CapOf(kAttrReflect, kLevel1);
	const uint32_t damageMin = MinOf(kAttrDamage, kLevel1);
	const uint32_t accuracyCap = CapOf(kAttrAccuracy, kLevel1);
	const uint32_t critCap = CapOf(kAttrPhysicalCrit, kLevel1);

	ItemComp hat = MakeInstance(kGuidA, kHatL1);
	hat.mutable_equip(); // 掷过、一条都没有:只有基础属性

	ItemComp shoes = MakeInstance(kGuidB, kShoesL1);
	AddAffix(shoes, kAttrStrength, kBlue, strengthCap, 0);
	AddAffix(shoes, kAttrAllPrimary, kBlue, allPrimaryCap + 9, 1); // 超上限:按上限计
	AddAffix(shoes, kAttrHealth, kPink, healthMin, 0);
	AddAffix(shoes, kAttrReflect, kYellow, reflectCap, 0);

	ItemComp weapon = MakeInstance(kGuidC, kWeaponL1);
	AddAffix(weapon, kAttrDamage, kBlue, damageMin, 0);
	AddAffix(weapon, kAttrAccuracy, kBlue, accuracyCap, 1);
	AddAffix(weapon, kAttrPhysicalCrit, kPink, std::numeric_limits<uint32_t>::max(), 0); // 超上限

	ASSERT_EQ(kSuccess, Equipment().PutInstance(hat));
	ASSERT_EQ(kSuccess, Equipment().PutInstance(shoes));
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon));
	ASSERT_EQ(kHatSlot, Equipment().GetItemPosByGuid(kGuidA));
	ASSERT_EQ(kShoesSlot, Equipment().GetItemPosByGuid(kGuidB));
	ASSERT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidC));
	// 躺在背包里的装备不算:加成只看装备栏。
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidD, kWeaponL80), 0);

	using equiprules::CombatStat;
	using equiprules::DerivedStat;
	using equiprules::Index;
	equiprules::EquipBonus expected;
	expected.primaryPoints[AttrRowOrNull(kAttrStrength)->effect_param()] = strengthCap;
	expected.allPrimaryPoints = allPrimaryCap;
	expected.derivedFlat[Index(DerivedStat::kMaxHealth)] = healthMin;
	expected.derivedFlat[Index(DerivedStat::kPhysicalAttack)] =
		BaseValueOf(kWeaponL1, kAttrDamage) + damageMin + accuracyCap;
	expected.derivedFlat[Index(DerivedStat::kMagicAttack)] = BaseValueOf(kWeaponL1, kAttrDamage) + damageMin;
	expected.derivedFlat[Index(DerivedStat::kDefense)] =
		BaseValueOf(kHatL1, kAttrDefense) + BaseValueOf(kShoesL1, kAttrDefense);
	expected.derivedFlat[Index(DerivedStat::kSpeed)] = BaseValueOf(kShoesL1, kAttrSpeed);
	expected.combat[Index(CombatStat::kReflectRate)] = reflectCap;
	expected.combat[Index(CombatStat::kPhysicalCritRate)] = critCap;

	// 传一份脏的进去:CollectBonus 自己重置,不在旧值上累加。
	equiprules::EquipBonus actual;
	actual.allPrimaryPoints = 999;
	actual.primaryPoints[55] = 7;
	actual.derivedFlat[Index(DerivedStat::kMaxMana)] = 123;
	actual.combat[Index(CombatStat::kMagicResist)] = 4;
	PlayerEquipSystem::CollectBonus(player, actual);
	ExpectSameBonus(expected, actual);

	// 纯读:再算一遍结果一样,实例也没被改写(超上限的存档值原样留着)。
	ExpectSameBonus(expected, Collect());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), shoes));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));
}

TEST_F(PlayerEquipTest, CollectBonusIsEmptyWithoutBagsOrWornEquipment)
{
	const equiprules::EquipBonus empty;

	// 装备栏空着(背包里有装备也不算)。
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL80), 0);
	ExpectSameBonus(empty, Collect());

	// 无效实体 / 还没加载出背包的玩家:出参被重置为空,且不会顺手造出背包组件。
	equiprules::EquipBonus dirty;
	dirty.allPrimaryPoints = 5;
	PlayerEquipSystem::CollectBonus(entt::null, dirty);
	ExpectSameBonus(empty, dirty);

	const auto bagless = tlsEcs.actorRegistry.create();
	players.push_back(bagless);
	dirty.allPrimaryPoints = 5;
	PlayerEquipSystem::CollectBonus(bagless, dirty);
	ExpectSameBonus(empty, dirty);
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(bagless));
}

// 穿着即生效:汇总时不复核佩戴要求(等级 / 职业只在穿上那一刻校验)。
TEST_F(PlayerEquipTest, CollectBonusDoesNotRecheckWearRequirements)
{
	SetLevel(1);
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL80);
	weapon.mutable_equip();
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon));
	const uint64_t damage = BaseValueOf(kWeaponL80, kAttrDamage);
	ASSERT_GT(damage, 0u);

	const auto bonus = Collect();
	EXPECT_EQ(damage, equiprules::DerivedFlatOf(bonus, equiprules::DerivedStat::kPhysicalAttack));
	EXPECT_EQ(damage, equiprules::DerivedFlatOf(bonus, equiprules::DerivedStat::kMagicAttack));
}

// 坏数据只跳过那一条:查不到定义的属性、非法颜色档、没有上限档的属性都不计,其余照算。
TEST_F(PlayerEquipTest, CollectBonusSkipsBrokenAffixesButKeepsTheRest)
{
	const uint32_t reflectCap = CapOf(kAttrReflect, kLevel1);
	ItemComp hat = MakeInstance(kGuidA, kHatL1);
	AddAffix(hat, kUnknownAttr, kBlue, 50, 0);                // 属性行不存在
	AddAffix(hat, kAttrHealth, /*tier=*/0, 50, 0);            // 0 是基础属性档,不该出现在存档里
	AddAffix(hat, kAttrHealth, /*tier=*/9, 50, 0);            // 越界的颜色档
	AddAffix(hat, kAttrSpeed, kBlue, 50, 1);                  // 速度在上限表里一档都没有:按 0 计
	AddAffix(hat, kAttrReflect, kYellow, reflectCap, 0);      // 唯一一条好的
	ASSERT_EQ(kSuccess, Equipment().PutInstance(hat));

	equiprules::EquipBonus expected;
	expected.derivedFlat[equiprules::Index(equiprules::DerivedStat::kDefense)] =
		BaseValueOf(kHatL1, kAttrDefense);
	expected.combat[equiprules::Index(equiprules::CombatStat::kReflectRate)] = reflectCap;
	ExpectSameBonus(expected, Collect());
}

// 面板与 tooltip 对「哪些行算数」必须同口径:规则层不认识的效果(表被填坏)既不计入加成,
// 也不出现在 tooltip 上 —— 否则玩家看到一行数值,面板却没有它。
TEST_F(PlayerEquipTest, RejectedEffectsAreNeitherCountedNorShown)
{
	ASSERT_EQ(1, ItemRowOrNull(kHatL1)->base_attr_size()) << "the hat carries only defense; " << kExportFirst;
	const uint32_t healthCap = CapOf(kAttrHealth, kLevel1);
	const uint32_t reflectCap = CapOf(kAttrReflect, kLevel1);
	ItemComp hat = MakeInstance(kGuidA, kHatL1);
	AddAffix(hat, kAttrReflect, kYellow, reflectCap, 0);
	AddAffix(hat, kAttrHealth, kPink, healthCap, 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(hat));

	// 随机属性那条:战斗属性号越界;基础属性那条:效果种类越界。
	Editable(AttrRowOrNull(kAttrReflect)).set_effect_param(99);
	Editable(AttrRowOrNull(kAttrDefense)).set_effect(99);

	equiprules::EquipBonus expected;
	expected.derivedFlat[equiprules::Index(equiprules::DerivedStat::kMaxHealth)] = healthCap;
	ExpectSameBonus(expected, Collect());

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(hat, info);
	EXPECT_EQ(0, info.base_attrs_size()) << "a base attribute that is not counted is not shown either";
	ASSERT_EQ(1, info.affixes_size()) << "only the affix that still counts is shown";
	EXPECT_EQ(kAttrHealth, info.affixes(0).attr_id());
	EXPECT_EQ(uint64_t{healthCap}, info.affixes(0).value());
}

// 装备栏里躺着一件表里已不是装备的东西(策划把这一行改成了非装备,老存档还原回来仍在装备栏):
// tooltip 对非装备不出属性行,加成也整件不计。
TEST_F(PlayerEquipTest, WornItemsThatAreNoLongerEquipmentGiveNoBonusAndShowNoLines)
{
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL1);
	AddAffix(weapon, kAttrDamage, kBlue, MinOf(kAttrDamage, kLevel1), 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_GT(equiprules::DerivedFlatOf(Collect(), equiprules::DerivedStat::kPhysicalAttack), 0u)
		<< "while the row is equipment the piece does count";

	Editable(ItemRowOrNull(kWeaponL1)).set_equip_kind(0);

	ExpectSameBonus(equiprules::EquipBonus{}, Collect());
	const ItemComp *stored = Equipment().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored) << "the piece itself is still in the equipment bag";
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(*stored, info);
	EXPECT_EQ(ItemRowOrNull(kWeaponL1)->name(), info.name());
	EXPECT_EQ(0, info.base_attrs_size());
	EXPECT_EQ(0, info.affixes_size());
}

// 改表下调上限立即对存量装备生效,而且不洗存档:面板与 tooltip 一起按新上限算,实例里的值不动。
TEST_F(PlayerEquipTest, LoweringACapClampsBonusAndTooltipWithoutTouchingTheInstance)
{
	const auto *capRow = CapRowOrNull(kAttrDamage, kLevel80);
	ASSERT_NE(nullptr, capRow);
	const uint32_t oldCap = capRow->cap();
	ASSERT_GE(oldCap, 2u) << "the fixture needs room to lower the cap";
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL80);
	AddAffix(weapon, kAttrDamage, kBlue, oldCap, 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon));
	const uint64_t base = BaseValueOf(kWeaponL80, kAttrDamage);

	EXPECT_EQ(base + oldCap,
			  equiprules::DerivedFlatOf(Collect(), equiprules::DerivedStat::kPhysicalAttack));

	const uint32_t newCap = oldCap / 2;
	Editable(capRow).set_cap(newCap);
	Editable(capRow).set_min_value(1);

	EXPECT_EQ(base + newCap,
			  equiprules::DerivedFlatOf(Collect(), equiprules::DerivedStat::kPhysicalAttack));
	EXPECT_EQ(base + newCap, equiprules::DerivedFlatOf(Collect(), equiprules::DerivedStat::kMagicAttack));

	const ItemComp *stored = Equipment().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(*stored, info);
	ASSERT_EQ(1, info.affixes_size());
	EXPECT_EQ(uint64_t{newCap}, info.affixes(0).value()) << "the tooltip shows the clamped value";
	EXPECT_EQ(uint64_t{newCap}, info.affixes(0).cap());

	ASSERT_EQ(1, stored->equip().affixes_size());
	EXPECT_EQ(oldCap, stored->equip().affixes(0).value()) << "the saved value is never rewritten";
}

// ── tooltip 显示数据 ────────────────────────────────────────────────────────

TEST_F(PlayerEquipTest, FillItemDisplayDescribesEquipmentForTheTooltip)
{
	const ItemTable *row = ItemRowOrNull(kShoesL1);
	ASSERT_NE(nullptr, row);
	ASSERT_EQ(2, row->base_attr_size()) << "shoes carry defense and speed; " << kExportFirst;
	const uint32_t strengthCap = CapOf(kAttrStrength, kLevel1);
	const uint32_t healthCap = CapOf(kAttrHealth, kLevel1);
	const uint32_t reflectCap = CapOf(kAttrReflect, kLevel1);
	const uint32_t allPrimaryCap = CapOf(kAttrAllPrimary, kLevel1);

	// 故意按乱序存:repeated 的下标不承载语义,显示顺序由 (tier, seq) 决定。
	ItemComp shoes = MakeInstance(kGuidA, kShoesL1);
	AddAffix(shoes, kAttrReflect, kYellow, reflectCap, 0);
	AddAffix(shoes, kAttrAllPrimary, kBlue, allPrimaryCap + 100, 1); // 超上限:显示上限
	AddAffix(shoes, kAttrHealth, kPink, healthCap, 0);
	AddAffix(shoes, kAttrStrength, kBlue, strengthCap, 0);

	BagItemInfo info;
	// 1–5 号字段归 BuildSnapshot 填:这里预先写上值,确认 FillItemDisplay 不碰它们。
	info.set_item_id(4242);
	info.set_config_id(77);
	info.set_count(3);
	info.set_max_stack(9);
	info.set_equip_kind(55);
	PlayerEquipSystem::FillItemDisplay(shoes, info);

	EXPECT_EQ(4242u, info.item_id());
	EXPECT_EQ(77u, info.config_id());
	EXPECT_EQ(3u, info.count());
	EXPECT_EQ(9u, info.max_stack());
	EXPECT_EQ(55u, info.equip_kind());

	EXPECT_FALSE(info.name().empty());
	EXPECT_EQ(row->name(), info.name());
	EXPECT_EQ(row->description(), info.description());
	EXPECT_EQ(row->icon_key(), info.icon_key());
	EXPECT_EQ(kLevel1, info.equip_level());
	EXPECT_EQ(0u, info.equip_class());
	EXPECT_TRUE(info.equip_class_name().empty()) << "the Class table has no name column yet";

	// 基础属性:表里的顺序,tier 0、seq 递增、没有上限。
	ASSERT_EQ(2, info.base_attrs_size());
	for (int i = 0; i < info.base_attrs_size(); ++i)
	{
		const auto &line = info.base_attrs(i);
		const auto &base = row->base_attr(i);
		const auto *attr = AttrRowOrNull(base.base_attr_id());
		ASSERT_NE(nullptr, attr);
		EXPECT_EQ(base.base_attr_id(), line.attr_id()) << "base line " << i;
		EXPECT_EQ(attr->name(), line.name()) << "base line " << i;
		EXPECT_FALSE(line.name().empty()) << "base line " << i;
		EXPECT_EQ(0u, line.tier()) << "base line " << i;
		EXPECT_EQ(uint64_t{base.base_attr_value()}, line.value()) << "base line " << i;
		EXPECT_EQ(0u, line.cap()) << "base line " << i;
		EXPECT_EQ(attr->percent() != 0, line.percent()) << "base line " << i;
		EXPECT_EQ(static_cast<uint32_t>(i), line.seq()) << "base line " << i;
	}

	// 随机属性:按 (tier, seq) 排好;值按上限夹取;上限、名称、是否百分比来自表。
	struct ExpectedLine
	{
		uint32_t attrId;
		uint32_t tier;
		uint32_t seq;
		uint64_t value;
		uint64_t cap;
	};
	const std::vector<ExpectedLine> expectedLines{
		{kAttrStrength, kBlue, 0, strengthCap, strengthCap},
		{kAttrAllPrimary, kBlue, 1, allPrimaryCap, allPrimaryCap},
		{kAttrHealth, kPink, 0, healthCap, healthCap},
		{kAttrReflect, kYellow, 0, reflectCap, reflectCap},
	};
	ASSERT_EQ(static_cast<int>(expectedLines.size()), info.affixes_size());
	for (int i = 0; i < info.affixes_size(); ++i)
	{
		const auto &line = info.affixes(i);
		const auto &want = expectedLines[static_cast<std::size_t>(i)];
		const auto *attr = AttrRowOrNull(want.attrId);
		ASSERT_NE(nullptr, attr);
		EXPECT_EQ(want.attrId, line.attr_id()) << "affix line " << i;
		EXPECT_EQ(want.tier, line.tier()) << "affix line " << i;
		EXPECT_EQ(want.seq, line.seq()) << "affix line " << i;
		EXPECT_EQ(want.value, line.value()) << "affix line " << i;
		EXPECT_EQ(want.cap, line.cap()) << "affix line " << i;
		EXPECT_EQ(attr->name(), line.name()) << "affix line " << i;
		EXPECT_EQ(attr->percent() != 0, line.percent()) << "affix line " << i;
	}
	// 反震率是百分比属性,力量不是:两种都得出现过,percent 才算真的验到了。
	EXPECT_TRUE(info.affixes(3).percent());
	EXPECT_FALSE(info.affixes(0).percent());
}

TEST_F(PlayerEquipTest, FillItemDisplayForNonEquipmentAndBrokenRows)
{
	// 不是装备:只填名称 / 描述 / 图标(既有行这三列都是空的,不编造);实例上就算有属性也不显示。
	ItemComp plain = MakeInstance(kGuidA, kPlainNonStack);
	AddAffix(plain, kAttrStrength, kBlue, 1, 0);
	BagItemInfo plainInfo;
	PlayerEquipSystem::FillItemDisplay(plain, plainInfo);
	EXPECT_EQ(ItemRowOrNull(kPlainNonStack)->name(), plainInfo.name());
	EXPECT_EQ(0u, plainInfo.equip_level());
	EXPECT_EQ(0, plainInfo.base_attrs_size());
	EXPECT_EQ(0, plainInfo.affixes_size());

	// 查不到表:什么都不写。
	BagItemInfo unknownInfo;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidB, kUnknownConfig), unknownInfo);
	EXPECT_EQ(0u, unknownInfo.ByteSizeLong());

	// 既有装备行(没有名称、没有基础属性、没掷过):是装备,但没有任何一行可显示。
	BagItemInfo oldInfo;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidC, kOldEquipA), oldInfo);
	EXPECT_TRUE(oldInfo.name().empty());
	EXPECT_EQ(0, oldInfo.base_attrs_size());
	EXPECT_EQ(0, oldInfo.affixes_size());

	// 坏掉的随机属性不显示;没有上限档的显示成 0 / 0(与 CollectBonus 按 0 计同口径)。
	ItemComp hat = MakeInstance(kGuidD, kHatL1);
	AddAffix(hat, kUnknownAttr, kBlue, 50, 0);
	AddAffix(hat, kAttrHealth, /*tier=*/0, 50, 0);
	AddAffix(hat, kAttrSpeed, kBlue, 50, 1);
	BagItemInfo hatInfo;
	PlayerEquipSystem::FillItemDisplay(hat, hatInfo);
	ASSERT_EQ(1, hatInfo.affixes_size());
	EXPECT_EQ(kAttrSpeed, hatInfo.affixes(0).attr_id());
	EXPECT_EQ(0u, hatInfo.affixes(0).value());
	EXPECT_EQ(0u, hatInfo.affixes(0).cap());
}

TEST_F(PlayerEquipTest, SnapshotsCarrySlotDefinitionsAndItemDisplay)
{
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL80);
	AddAffix(weapon, kAttrDamage, kBlue, CapOf(kAttrDamage, kLevel80), 0);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidB, kHatL1), 4);

	// 装备栏:有部位名的槽位定义全部下发(含空槽),槽号升序,带部位与名称。
	BagInfo equipment;
	ASSERT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, kEquipment, equipment));
	std::map<uint32_t, EquipSlotInfo> slots;
	uint32_t previousSlot = 0;
	for (int i = 0; i < equipment.layout().equip_slots_size(); ++i)
	{
		const auto &slot = equipment.layout().equip_slots(i);
		if (i > 0)
		{
			EXPECT_GT(slot.slot(), previousSlot) << "ascending, no duplicates";
		}
		previousSlot = slot.slot();
		slots[slot.slot()] = slot;
		const auto *row = EquipSlotTableManager::Instance().FindByIdSilent(slot.slot()).first;
		ASSERT_NE(nullptr, row) << "slot " << slot.slot();
		EXPECT_EQ(row->equip_kind(), slot.equip_kind()) << "slot " << slot.slot();
		EXPECT_EQ(row->name(), slot.name()) << "slot " << slot.slot();
		EXPECT_LT(slot.slot(), equipment.layout().capacity());
	}
	// 这里只有武器槽被占着,它有名字;所以下发的恰好是「容量内、有部位名」的那些行。
	std::size_t namedRowsWithinCapacity = 0;
	for (const auto &row : EquipSlotTableManager::Instance().FindAll().data())
	{
		if (row.id() < equipment.layout().capacity() && !row.name().empty())
		{
			++namedRowsWithinCapacity;
		}
	}
	EXPECT_EQ(namedRowsWithinCapacity, slots.size()) << "every named slot row within capacity, empty or not";
	for (const auto &[slot, kind] : std::map<uint32_t, uint32_t>{{kWeaponSlot, kWeaponKind},
																  {kHatSlot, kHatKind},
																  {kClothesSlot, kClothesKind},
																  {kShoesSlot, kShoesKind}})
	{
		ASSERT_EQ(1u, slots.count(slot)) << "slot " << slot;
		EXPECT_EQ(kind, slots[slot].equip_kind()) << "slot " << slot;
		EXPECT_FALSE(slots[slot].name().empty()) << "slot " << slot << " shows its part name when empty";
	}
	// 没有部位名的夹具槽空着:不下发(见 UnnamedSlotsAreListedOnlyWhileOccupied)。
	EXPECT_EQ(0u, slots.count(kOldSlotFirst));

	// 穿着的那件带齐 tooltip 数据。
	ASSERT_EQ(1, equipment.items_size());
	const auto &worn = equipment.items(0);
	EXPECT_EQ(kGuidA, worn.item_id());
	EXPECT_EQ(kWeaponL80, worn.config_id());
	EXPECT_EQ(kWeaponKind, worn.equip_kind());
	EXPECT_FALSE(worn.name().empty());
	EXPECT_EQ(kLevel80, worn.equip_level());
	ASSERT_EQ(ItemRowOrNull(kWeaponL80)->base_attr_size(), worn.base_attrs_size());
	EXPECT_EQ(kAttrDamage, worn.base_attrs(0).attr_id());
	ASSERT_EQ(1, worn.affixes_size());
	EXPECT_EQ(uint64_t{CapOf(kAttrDamage, kLevel80)}, worn.affixes(0).cap());

	// 人物背包:没有槽位定义;躺在背包里的装备同样带 tooltip 数据。
	BagInfo inventory;
	ASSERT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, kInventory, inventory));
	EXPECT_EQ(0, inventory.layout().equip_slots_size());
	ASSERT_EQ(1, inventory.items_size());
	EXPECT_EQ(kGuidB, inventory.items(0).item_id());
	EXPECT_FALSE(inventory.items(0).name().empty());
	EXPECT_EQ(2, inventory.items(0).affixes_size());
	EXPECT_GT(inventory.items(0).base_attrs_size(), 0);
}

// 没有部位名的槽(表里 0–2 号是单测夹具槽)不对玩家展示:空着不下发;一旦被占着就照常下发,
// 否则身上那件装备在客户端无处显示、也就卸不下来。判据是表里的 name,不是槽号。
TEST_F(PlayerEquipTest, UnnamedSlotsAreListedOnlyWhileOccupied)
{
	for (const uint32_t slot : {kOldSlotFirst, kOldSlotSecond})
	{
		const auto *row = EquipSlotTableManager::Instance().FindByIdSilent(slot).first;
		ASSERT_NE(nullptr, row) << kExportFirst;
		ASSERT_TRUE(row->name().empty()) << "fixture slot " << slot << " is expected to have no part name";
	}

	// 下发的槽位定义:槽号 -> 名称。
	const auto listedSlots = [this]()
	{
		BagInfo equipment;
		EXPECT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, kEquipment, equipment));
		std::map<uint32_t, std::string> listed;
		for (const auto &slot : equipment.layout().equip_slots())
		{
			listed[slot.slot()] = slot.name();
		}
		return listed;
	};

	const std::map<uint32_t, std::string> emptyBar = listedSlots();
	EXPECT_EQ(0u, emptyBar.count(kOldSlotFirst));
	EXPECT_EQ(0u, emptyBar.count(kOldSlotSecond));
	EXPECT_EQ(1u, emptyBar.count(kWeaponSlot)) << "named slots are listed even when empty";

	// 夹具装备(部位 1)穿在 0 号槽:这个槽现在必须下发,名称仍为空。
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeInstance(kGuidA, kOldEquipA), kOldSlotFirst));
	const std::map<uint32_t, std::string> worn = listedSlots();
	ASSERT_EQ(1u, worn.count(kOldSlotFirst)) << "an occupied slot is always listed, named or not";
	EXPECT_TRUE(worn.at(kOldSlotFirst).empty());
	EXPECT_EQ(0u, worn.count(kOldSlotSecond)) << "the other unnamed slot is still empty";
	EXPECT_EQ(emptyBar.size() + 1, worn.size());

	// 卸下之后又不下发了。
	ItemComp carried;
	ASSERT_EQ(kSuccess, Equipment().TakeInstance(kGuidA, carried));
	EXPECT_EQ(emptyBar, listedSlots());
}

// 槽位定义只下发容量内的:客户端画出来的槽必须是真能穿的槽。
TEST_F(PlayerEquipTest, SlotDefinitionsStopAtTheEquipmentCapacity)
{
	Equipment().SetCapacityForRestore(kWeaponSlot + 1);
	BagInfo equipment;
	ASSERT_EQ(kSuccess, PlayerBagSystem::BuildSnapshot(player, kEquipment, equipment));
	ASSERT_GT(equipment.layout().equip_slots_size(), 0);
	bool sawWeaponSlot = false;
	for (const auto &slot : equipment.layout().equip_slots())
	{
		EXPECT_LE(slot.slot(), kWeaponSlot);
		sawWeaponSlot = sawWeaponSlot || slot.slot() == kWeaponSlot;
	}
	EXPECT_TRUE(sawWeaponSlot);
}

// ── 属性面板「战斗属性」区的显示信息 ────────────────────────────────────────

TEST_F(PlayerEquipTest, DescribeCombatStatReadsTheAttributeRow)
{
	const auto count = equiprules::ToRaw(equiprules::CombatStat::kCount);
	for (uint32_t raw = 1; raw < count; ++raw)
	{
		const auto stat = equiprules::ToCombatStat(raw);
		ASSERT_NE(equiprules::CombatStat::kNone, stat) << raw;
		// 独立地在表里找这一项:effect = 5 且 effect_param = raw。
		const EquipAttributeTable *row = nullptr;
		for (const auto &candidate : EquipAttributeTableManager::Instance().FindAll().data())
		{
			if (candidate.effect() == equiprules::ToRaw(equiprules::EffectKind::kCombat) &&
				candidate.effect_param() == raw)
			{
				ASSERT_EQ(nullptr, row) << "combat stat " << raw << " has more than one attribute row";
				row = &candidate;
			}
		}
		ASSERT_NE(nullptr, row) << "combat stat " << raw << " has no attribute row; " << kExportFirst;

		const auto display = PlayerEquipSystem::DescribeCombatStat(stat);
		EXPECT_FALSE(display.name.empty()) << raw;
		EXPECT_EQ(row->name(), display.name) << raw;
		EXPECT_EQ(row->percent() != 0, display.percent) << raw;
		EXPECT_EQ(row->sort(), display.sort) << raw;
	}

	for (const auto stat : {equiprules::CombatStat::kNone, equiprules::CombatStat::kCount})
	{
		const auto display = PlayerEquipSystem::DescribeCombatStat(stat);
		EXPECT_TRUE(display.name.empty());
		EXPECT_FALSE(display.percent);
		EXPECT_EQ(0u, display.sort);
	}
}

// 同一项战斗属性有多行时取 sort 最小的;没有任何行时返回空名。
TEST_F(PlayerEquipTest, DescribeCombatStatPrefersTheLowestSortAndReportsMissingRows)
{
	const auto *magicResist = AttrRowOrNull(kAttrMagicResist);
	const auto *physicalResist = AttrRowOrNull(kAttrPhysicalResist);
	ASSERT_NE(nullptr, magicResist) << kExportFirst;
	ASSERT_NE(nullptr, physicalResist) << kExportFirst;
	ASSERT_GT(magicResist->sort(), 1u);
	const std::string physicalName = physicalResist->name();
	ASSERT_NE(magicResist->name(), physicalName);

	// 把「抗物理」那一行改成也指向抗法术,并且排在前面。
	Editable(physicalResist).set_effect_param(magicResist->effect_param());
	Editable(physicalResist).set_sort(magicResist->sort() - 1);

	const auto preferred = PlayerEquipSystem::DescribeCombatStat(equiprules::CombatStat::kMagicResist);
	EXPECT_EQ(physicalName, preferred.name);
	EXPECT_EQ(magicResist->sort() - 1, preferred.sort);

	// 抗物理现在一行都没有了。
	const auto missing = PlayerEquipSystem::DescribeCombatStat(equiprules::CombatStat::kPhysicalResist);
	EXPECT_TRUE(missing.name.empty());
	EXPECT_EQ(0u, missing.sort);
}

TEST_F(PlayerEquipTest, IsEquipmentFollowsTheItemTable)
{
	EXPECT_TRUE(PlayerEquipSystem::IsEquipment(kOldEquipA));
	EXPECT_TRUE(PlayerEquipSystem::IsEquipment(kWeaponL80));
	EXPECT_FALSE(PlayerEquipSystem::IsEquipment(kPlainNonStack));
	EXPECT_FALSE(PlayerEquipSystem::IsEquipment(kStackable));
	EXPECT_FALSE(PlayerEquipSystem::IsEquipment(kUnknownConfig));
	EXPECT_FALSE(PlayerEquipSystem::IsEquipment(0));
}

// ── handler(经生成的 CallMethod 分发)───────────────────────────────────────

// 三个新方法只许追加在 service 末尾:CallMethod 按 method index 分发,插在中间会让既有的
// GetBag / SortBag 错位。
TEST_F(PlayerEquipTest, NewBagMethodsAreAppendedAfterTheExistingOnes)
{
	const auto *getBag = BagMethod("GetBag");
	const auto *sortBag = BagMethod("SortBag");
	const auto *equip = BagMethod("EquipItem");
	const auto *unequip = BagMethod("UnequipItem");
	const auto *grant = BagMethod("GmGrantItem");
	ASSERT_NE(nullptr, getBag);
	ASSERT_NE(nullptr, sortBag);
	ASSERT_NE(nullptr, equip) << kProtoGenFirst;
	ASSERT_NE(nullptr, unequip) << kProtoGenFirst;
	ASSERT_NE(nullptr, grant) << kProtoGenFirst;
	EXPECT_EQ(0, getBag->index());
	EXPECT_EQ(1, sortBag->index());
	EXPECT_EQ(2, equip->index());
	EXPECT_EQ(3, unequip->index());
	EXPECT_EQ(4, grant->index());
}

TEST_F(PlayerEquipTest, EquipHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("EquipItem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	Inventory().InsertItemForRestore(MakeRolledInstance(kGuidA, kWeaponL1), 0);
	Inventory().InsertItemForRestore(MakeInstance(kGuidB, kPlainNonStack), 1);

	// 参数粗检:item_id = 0。
	EquipItemRequest request;
	EquipItemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());
	EXPECT_EQ(0u, Tip().id()) << "the tip is moved into the response, not left behind";

	// 业务拒绝:原样带回编排层的码,不带半份快照。
	request.set_item_id(kGuidB);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipNotEquipment, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());
	EXPECT_EQ(0u, Tip().id());

	// 成功:上一次的失败码没有漏进来;两个包的全量都在,且已经是穿上之后的样子。
	request.set_item_id(kGuidA);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_EQ(uint32_t{kInventory}, response.bag().layout().bag_type());
	EXPECT_EQ(uint32_t{kEquipment}, response.equipment().layout().bag_type());
	EXPECT_FALSE(ContainsItem(response.bag().items(), kGuidA));
	EXPECT_TRUE(ContainsItem(response.bag().items(), kGuidB));
	ASSERT_EQ(1, response.equipment().items_size());
	EXPECT_EQ(kGuidA, response.equipment().items(0).item_id());
	EXPECT_EQ(2, response.equipment().items(0).affixes_size());
	ASSERT_EQ(1, response.equipment().layout().slots_size());
	EXPECT_EQ(kWeaponSlot, response.equipment().layout().slots(0).slot());
	EXPECT_GT(response.equipment().layout().equip_slots_size(), 0);
	EXPECT_EQ(0, response.bag().layout().equip_slots_size());
	EXPECT_EQ(0u, Tip().id());
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
}

TEST_F(PlayerEquipTest, UnequipHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("UnequipItem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeRolledInstance(kGuidA, kWeaponL1), kWeaponSlot));

	UnequipItemRequest request;
	UnequipItemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	request.set_item_id(kGuidMissing);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipNotEquipped, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_EQ(0u, Tip().id());

	request.set_item_id(kGuidA);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_TRUE(ContainsItem(response.bag().items(), kGuidA));
	EXPECT_EQ(0, response.equipment().items_size());
	EXPECT_GT(response.equipment().layout().equip_slots_size(), 0) << "empty slots are still listed";
	EXPECT_EQ(0u, Tip().id());
	EXPECT_NE(nullptr, Inventory().GetItemCompByGuid(kGuidA));
}

// GM 发物受 scene 自己的运行模式闸控制(SCENE_RUN_MODE,默认 prod = 关闭)。
// 这是「prod 关掉」不是鉴权:两种模式下的行为都钉住,跑用例的环境是哪一种就验哪一种。
TEST_F(PlayerEquipTest, GmGrantItemHandlerFollowsTheSceneRunMode)
{
	const auto *method = BagMethod("GmGrantItem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	PlayerEquipSystem::InstallItemInitializer();
	const bool gmAllowed = gate_security::ClassifyGmClientMessage(scene_gm_guard::CurrentSceneRunMode()) ==
						   gate_security::GmClientMessageVerdict::kAllow;

	GmGrantItemRequest request;
	request.set_config_id(kWeaponL80);
	request.set_count(1);
	GmGrantItemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, Tip().id());

	if (!gmAllowed)
	{
		EXPECT_EQ(kFeatureUnavailable, response.error_message().id());
		EXPECT_FALSE(response.has_bag());
		EXPECT_EQ(0u, Inventory().OccupiedGridCount()) << "a refused GM command grants nothing";
		return;
	}

	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_EQ(1, response.bag().items_size());
	EXPECT_EQ(kWeaponL80, response.bag().items(0).config_id());
	EXPECT_FALSE(response.bag().items(0).name().empty());
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	// 发出去的那件走的是正式入包路径:已经掷过属性。
	Inventory().ForEachItem([](Guid, const ItemComp &item)
							{ ASSERT_NO_FATAL_FAILURE(ExpectValidRoll(item, kWeaponL80, kLevel80)); });

	// 放行的环境里,参数错误照样被编排层拒绝。
	request.set_count(0);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipGrantInvalid, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
}

} // namespace
