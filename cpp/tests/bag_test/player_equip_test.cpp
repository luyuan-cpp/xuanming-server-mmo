// 装备编排层(PlayerEquipSystem):新装备掷随机属性、穿上 / 卸下、装备加成汇总、tooltip 显示数据、
// 三个客户端 RPC 的 handler。契约见 docs/design/equipment-attributes.md §4.3 / §4.6 / §5。
// 二期(强化 / 镶嵌 / 取下,以及它们的表校验、加成、tooltip、三个新 RPC 的 handler)也在这个文件里,
// 契约见 docs/design/equipment-enhance-inlay.md §4.3 / §4.4 / §5;用例集中在文件后半「二期」那几节。
//
// 分工:
//   * 容器层的搬运原语与初始化回调本身(TakeInstance / PutInstance / ItemInstanceInitializer)
//     在 bag_instance_data_test.cpp;这里验的是编排 —— 选槽、校验顺序、拒绝时两个包不变、
//     同一 guid 换包后实例数据不变。
//   * 「怎么掷、怎么累加」的纯规则在 turn_battle_engine_test/equip_attribute_rules_test.cpp;
//     这里验的是表行 / 实例被正确翻译成规则的输入。
//   * 装备加成进面板 / 进战斗的数值在 player_equip_attribute_test.cpp;这里只确认穿脱之后
//     触发了重算(max_health 被写成非 0)。强化 / 镶嵌 / 取下同理:这里钉的是校验顺序、
//     扣了什么、实例上写了什么、哪些情况重算;「强化多少级 = 面板多几点」的六项增量在那个文件。
//   * 按 config 全或无扣物的原语(BagService::RemoveItemsExact)在 bag_remove_exact_test.cpp。
//
// 数值一律现读配表(上限、基础属性、强化的加成 / 成功率 / 消耗、宝石数值都是占位建议,策划随时会改),
// 只把「哪个属性 id 是什么效果」当作契约写死(设计文档 §2.1:id 一经使用不再改义)。
// 强化的成功率不靠表:要哪种结果,就把那一级的成功率在内存里钉成 50%,再用脚本随机数卡在边界两侧。
//
// 夹具自带:SetUp 自己加载用到的每一张表、自己装 item 号段;TearDown 卸掉 BagService 的
// 初始化回调、复原被测文件的随机源、把改过的表重新加载、恢复其它用例使用的号段基线 ——
// 不依赖别的文件的用例先跑或后跑。

#include <gtest/gtest.h>
#include "engine/core/type_define/player_guid_comp.h"

#include <cstdint>
#include <cstdio>
#include <functional>
#include <iterator>
#include <limits>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include <google/protobuf/unknown_field_set.h>

#include "../../nodes/scene/handler/rpc/player/player_bag_handler.h"
#include "../../nodes/scene/handler/rpc/player/player_gm_guard.h"
#include "engine/core/error_handling/error_handling.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
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
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "muduo/base/Logging.h" // Logger::setOutput:数容器层不足分支留下的调用栈(见 StackTraceDumpProbe)

namespace
{
class ScopedGlobalItemBlock
{
public:
	explicit ScopedGlobalItemBlock(uint32_t configId)
		: configId_(configId), wasBlocked_(GainBlockService::IsGloballyBlocked(GainBlockService::GainType::kItem, configId))
	{
		GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, configId_);
	}
	~ScopedGlobalItemBlock()
	{
		if (wasBlocked_) GainBlockService::BlockGlobal(GainBlockService::GainType::kItem, configId_);
		else GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, configId_);
	}
	void Unblock() { GainBlockService::UnblockGlobal(GainBlockService::GainType::kItem, configId_); }
	ScopedGlobalItemBlock(const ScopedGlobalItemBlock&) = delete;
	ScopedGlobalItemBlock& operator=(const ScopedGlobalItemBlock&) = delete;
private:
	uint32_t configId_;
	bool wasBlocked_;
};


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

// 二期新增的行(docs/design/equipment-enhance-inlay.md §2;需先导表):
constexpr uint32_t kEnhanceStone = 2001;     // 强化石:可叠加;夹具用到的几级强化都拿它当材料
constexpr uint32_t kAttackGem = 2101;        // 攻击宝石·一级:类型 1,提供「伤害」
constexpr uint32_t kDefenseGem = 2201;       // 防御宝石·一级:类型 2,提供「防御」
constexpr uint32_t kGemTypeAttack = 1;
constexpr uint32_t kGemTypeDefense = 2;
constexpr uint32_t kGemTypeAny = 0;          // 孔位表里的「任意类型」
constexpr uint32_t kWeaponAttackSocket = 0;  // 武器 0 号孔:只收攻击宝石
constexpr uint32_t kWeaponAnySocket = 1;     // 武器 1 号孔:任意类型
constexpr uint32_t kUndefinedSocket = 7;     // 武器没有的孔位号(仍小于 equiprules::kMaxGemSockets)
// 夹具用到「强化到 +1 / +2 / +4」三行,以及「+3 的装备」,所以强化表至少要从 1 连续到 4。
constexpr uint32_t kFixtureEnhanceLevels = 4;

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
// 二期用例摆进人物背包的堆(材料 / 宝石 / 占格子的杂物)。
constexpr Guid kGuidStone = (uint64_t{1} << 60) + 7111;
constexpr Guid kGuidGemA = (uint64_t{1} << 60) + 7121;
constexpr Guid kGuidGemB = (uint64_t{1} << 60) + 7122;
constexpr Guid kGuidFiller = (uint64_t{1} << 60) + 7131;
// 它们固定摆的格子:离装备用的 0–6 号格远一点,断言「原格没动」时不会互相挡。
constexpr uint32_t kStonePos = 20;
constexpr uint32_t kGemPosA = 30;
constexpr uint32_t kGemPosB = 31;

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

// ── 现读配表(二期的三张表)──────────────────────────────────────────────────

// 「强化到 +level」那一行(主键 = 目标等级)。
const EquipEnhanceTable *EnhanceRowOrNull(uint32_t level)
{
	return EquipEnhanceTableManager::Instance().FindByIdSilent(level).first;
}

// 宝石行(主键 = 宝石物品的 Item.id)。
const EquipGemTable *GemRowOrNull(uint32_t gemConfigId)
{
	return EquipGemTableManager::Instance().FindByIdSilent(gemConfigId).first;
}

// 孔位行的主键 = equip_kind * 100 + socket(二期设计文档 §2.3)。与 CapRowOrNull 同一个考虑:
// 按主键直取,不走被测代码用的「按部位取全部行」那条路。
const EquipGemSocketTable *SocketRowOrNull(uint32_t equipKind, uint32_t socket)
{
	return EquipGemSocketTableManager::Instance().FindByIdSilent(equipKind * 100 + socket).first;
}

// 强化到 level 级后基础属性的总加成(万分比);0 级、缺行返回 0。
uint32_t EnhanceBonusOf(uint32_t level)
{
	const auto *row = EnhanceRowOrNull(level);
	return row != nullptr ? row->bonus_permyriad() : 0;
}

// 一条基础属性在 level 级强化下应该是多少。**刻意不调 equiprules::ScaleByPermyriad**(被测代码用的
// 就是它,它自己的取整 / 饱和在规则用例里验):夹具数值很小,按定义式直接算不会溢出,期望值与实现
// 各走各的路 —— 这里验的是「拿对了哪一行的万分比、乘在了哪个数上」。
uint64_t EnhancedValue(uint64_t tableValue, uint32_t level)
{
	return tableValue + tableValue * EnhanceBonusOf(level) / equiprules::kPermyriadBase;
}

// 宝石提供的数值;查不到返回 0。
uint64_t GemValueOf(uint32_t gemConfigId)
{
	const auto *row = GemRowOrNull(gemConfigId);
	return row != nullptr ? row->value() : 0;
}

// 强化表的主键索引,可写。强化上限与取行走的都是它,Editable 原地改行上的 id 骗不过它们,所以
// 「表里没有这一级」只能从索引里摘;行本身还留在 FindAll() 里(表校验会把它们报成「不在连续等级里」)。
// 与 Editable 是同一类手段,夹具的 TearDown 重新加载整张表,改动不外漏。
EquipEnhanceTableManager::IdMapType &EditableEnhanceIndex()
{
	return const_cast<EquipEnhanceTableManager::IdMapType &>(EquipEnhanceTableManager::Instance().GetIdMap());
}

// 把强化表在内存里截到只剩前 keep 级(0 = 一级都不剩 = 强化未开放)。
void TruncateEnhanceTable(uint32_t keep)
{
	auto &idMap = EditableEnhanceIndex();
	for (auto it = idMap.begin(); it != idMap.end();)
	{
		it = it->first > keep ? idMap.erase(it) : std::next(it);
	}
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

// 往实例上记一颗已镶嵌的宝石(原始记录:不查表、不判孔位,脏数据也照写 —— 用例要的就是这个)。
void AddGem(ItemComp &item, uint32_t socket, uint32_t gemConfigId)
{
	auto *gem = item.mutable_equip()->add_gems();
	gem->set_socket(socket);
	gem->set_gem_config_id(gemConfigId);
}

// 一件「已经掷过属性」的装备:两条属性,字段取互不相同的值,丢哪个字段都看得出来。
// enhanceLevel 是二期加的:默认 0 = 未强化,与一期造出来的实例逐字节相同;宝石用 AddGem 追加。
ItemComp MakeRolledInstance(Guid guid, uint32_t configId, uint32_t enhanceLevel = 0)
{
	ItemComp item = MakeInstance(guid, configId);
	AddAffix(item, kAttrStrength, kBlue, /*value=*/1, /*seq=*/0);
	AddAffix(item, kAttrPhysicalCrit, kYellow, /*value=*/2, /*seq=*/0);
	if (enhanceLevel != 0)
	{
		item.mutable_equip()->set_enhance_level(enhanceLevel);
	}
	return item;
}

// 一件已初始化、但一条随机属性都没有的装备,可带强化等级;宝石用 AddGem 追加。
// 加成 / tooltip 的用例用它:期望值里只剩基础属性、强化与宝石,逐项手算得出来。
ItemComp MakeForgedInstance(Guid guid, uint32_t configId, uint32_t enhanceLevel = 0)
{
	ItemComp item = MakeInstance(guid, configId);
	item.mutable_equip()->set_enhance_level(enhanceLevel);
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

// 快照里按 guid 找一件物品;没有返回 nullptr。
template <typename Items>
const BagItemInfo *FindItem(const Items &items, Guid guid)
{
	for (const auto &item : items)
	{
		if (item.item_id() == guid)
		{
			return &item;
		}
	}
	return nullptr;
}

// 快照里某个 config 的总数量(同一种可叠加物品可能分几堆)。
template <typename Items>
uint64_t SnapshotCountOf(const Items &items, uint32_t configId)
{
	uint64_t total = 0;
	for (const auto &item : items)
	{
		if (item.config_id() == configId)
		{
			total += item.count();
		}
	}
	return total;
}

// 快照里不许有 count == 0 的条目:被扣光的堆必须在回包之前回收掉,否则客户端会画出一个「×0」的格子。
template <typename Items>
void ExpectNoEmptyEntries(const Items &items)
{
	for (const auto &item : items)
	{
		EXPECT_GT(item.count(), 0u) << "item " << item.item_id() << " (config " << item.config_id()
									<< ") is an emptied stack that was not reclaimed";
	}
}

// 包里没有被扣光却还占着格子的空堆。
void ExpectNoEmptyStacks(const Bag &bag)
{
	bag.ForEachItem([](Guid guid, const ItemComp &item)
					{ EXPECT_GT(item.size(), 0u) << "guid " << guid << " is an emptied stack"; });
}

// 脚本化随机源:按顺序吐出预先写好的数,并记下每次被要的区间(做法同规则用例里的 ScriptedRand)。
// 经 PlayerEquipSystem::SetRandomSourceForTest 装进被测文件,强化的成败因此是确定的。
// 脚本用完了还来要 = 被测代码多掷了一次:记一条失败并返回 0。所以**空脚本 = 这条用例里不许掷骰**,
// 拒绝路径靠它钉住「被拒的强化不消耗随机数」。
class ScriptedDice
{
public:
	explicit ScriptedDice(std::vector<uint64_t> script) : script_(std::move(script)) {}

	uint64_t Roll(uint64_t range)
	{
		requested_.push_back(range);
		if (next_ >= script_.size())
		{
			ADD_FAILURE() << "unexpected roll #" << requested_.size() << " (out of " << range
						  << "): the script only has " << script_.size() << " value(s)";
			return 0;
		}
		return script_[next_++];
	}

	// 每次被要的区间,按先后。
	const std::vector<uint64_t> &Requested() const { return requested_; }

private:
	std::vector<uint64_t> script_;
	std::size_t next_{0};
	std::vector<uint64_t> requested_;
};

// 强化 / 镶嵌 / 取下的拒绝路径要钉三样东西都没动:金币、人物背包、装备栏。
struct ForgeState
{
	uint64_t gold{0};
	BagState inventory;
	BagState equipment;
};

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
		// 二期三张表。不加载 = 空表 = 强化未开放、没有宝石、没有孔:强化 / 镶嵌的用例全都会被拒。
		EquipEnhanceTableManager::Instance().Load();
		EquipGemTableManager::Instance().Load();
		EquipGemSocketTableManager::Instance().Load();
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
		PlayerEquipSystem::SetRandomSourceForTest({});
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());
		player = NewPlayer(kPlayerGuid, kMaxLevel);
	}

	void TearDown() override
	{
		BagService::SetItemInstanceInitializer({});
		// 脚本随机源是进程级静态:不复原的话,排在后面的用例掷属性时吃到的是用完了的脚本。
		PlayerEquipSystem::SetRandomSourceForTest({});
		dice.reset();
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

		ASSERT_NO_FATAL_FAILURE(RequireForgeRows());
	}

	// ── 二期的前置条件(强化 / 镶嵌的三张表与 Item 表新行)──────────────────────
	// 只核对「形状」(这一级收不收金币 / 材料、这颗宝石是什么类型、这个孔收什么);
	// 具体收多少、加多少一律由用例现读,策划调数值不会把用例调红。

	// 强化材料 / 宝石物品:可叠加、不是装备、不是战斗道具(按 config 扣发数量的前提),有名字(要下发给 tooltip)。
	static void RequireForgeItem(uint32_t configId)
	{
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(configId, 0, 999));
		const auto *row = ItemRowOrNull(configId);
		ASSERT_GE(row->max_stack_size(), 99u) << "Item row " << configId << "; " << kExportFirst;
		ASSERT_EQ(0u, row->battle_usable()) << "Item row " << configId;
		ASSERT_FALSE(row->name().empty()) << "Item row " << configId;
	}

	// 「强化到 +level」那一行:成功率合法,收金币,材料是强化石。
	static void RequireEnhanceRow(uint32_t level)
	{
		const auto *row = EnhanceRowOrNull(level);
		ASSERT_NE(nullptr, row) << "EquipEnhance row " << level << " is missing; " << kExportFirst;
		ASSERT_GE(row->success_rate(), 1u) << "EquipEnhance row " << level;
		ASSERT_LE(row->success_rate(), equiprules::kPermyriadBase) << "EquipEnhance row " << level;
		ASSERT_GE(row->cost_gold(), 1u) << "EquipEnhance row " << level << " is expected to cost gold";
		// 用例会在这个数上再加几枚金币当零头,别让它靠近 64 位上限。
		ASSERT_LE(row->cost_gold(), uint64_t{1} << 40) << "EquipEnhance row " << level;
		ASSERT_EQ(kEnhanceStone, row->material_id()) << "EquipEnhance row " << level << "; " << kExportFirst;
		ASSERT_GE(row->material_count(), 1u) << "EquipEnhance row " << level;
		ASSERT_LE(row->material_count(), 50u) << "EquipEnhance row " << level;
	}

	static void RequireGem(uint32_t gemConfigId, uint32_t gemType, uint32_t attrId)
	{
		ASSERT_NO_FATAL_FAILURE(RequireForgeItem(gemConfigId));
		const auto *row = GemRowOrNull(gemConfigId);
		ASSERT_NE(nullptr, row) << "EquipGem row " << gemConfigId << " is missing; " << kExportFirst;
		ASSERT_EQ(gemType, row->gem_type()) << "EquipGem row " << gemConfigId;
		ASSERT_EQ(attrId, row->attr_id()) << "EquipGem row " << gemConfigId;
		ASSERT_GT(row->value(), 0u) << "EquipGem row " << gemConfigId;
	}

	static void RequireSocket(uint32_t equipKind, uint32_t socket, uint32_t gemType)
	{
		const auto *row = SocketRowOrNull(equipKind, socket);
		ASSERT_NE(nullptr, row) << "EquipGemSocket row " << equipKind * 100 + socket << " is missing; "
								<< kExportFirst;
		ASSERT_EQ(equipKind, row->equip_kind()) << "EquipGemSocket row " << row->id();
		ASSERT_EQ(socket, row->socket()) << "EquipGemSocket row " << row->id();
		ASSERT_EQ(gemType, row->gem_type()) << "EquipGemSocket row " << row->id();
	}

	static void RequireForgeRows()
	{
		ASSERT_NO_FATAL_FAILURE(RequireForgeItem(kEnhanceStone));
		// 「不是宝石」的两种夹具物品:强化石,和一期就有的可叠加杂物。
		ASSERT_EQ(nullptr, GemRowOrNull(kEnhanceStone)) << "the enhance stone must not be a gem";
		ASSERT_EQ(nullptr, GemRowOrNull(kStackable)) << "fixture item " << kStackable << " must not be a gem";

		for (uint32_t level = 1; level <= kFixtureEnhanceLevels; ++level)
		{
			ASSERT_TRUE(EquipEnhanceTableManager::Instance().Exists(level))
				<< "EquipEnhance row " << level << " is missing; " << kExportFirst;
		}
		for (const uint32_t level : {1u, 2u, 4u})
		{
			ASSERT_NO_FATAL_FAILURE(RequireEnhanceRow(level));
		}

		ASSERT_NO_FATAL_FAILURE(RequireGem(kAttackGem, kGemTypeAttack, kAttrDamage));
		ASSERT_NO_FATAL_FAILURE(RequireGem(kDefenseGem, kGemTypeDefense, kAttrDefense));

		// 武器恰好两个孔:0 号只收攻击宝石,1 号任意。
		ASSERT_NO_FATAL_FAILURE(RequireSocket(kWeaponKind, kWeaponAttackSocket, kGemTypeAttack));
		ASSERT_NO_FATAL_FAILURE(RequireSocket(kWeaponKind, kWeaponAnySocket, kGemTypeAny));
		ASSERT_EQ(2u, EquipGemSocketTableManager::Instance().GetByEquipKind(kWeaponKind).size())
			<< "the weapon is expected to have exactly two sockets; " << kExportFirst;
		ASSERT_EQ(nullptr, SocketRowOrNull(kWeaponKind, kUndefinedSocket));
		ASSERT_LT(kUndefinedSocket, equiprules::kMaxGemSockets);
		// 一期的夹具部位(部位 1)一个孔都没有:「这个部位没有孔」的用例靠这一点。
		ASSERT_TRUE(EquipGemSocketTableManager::Instance().GetByEquipKind(kOldKind).empty());
	}

	// 一个能让 PlayerAttributeSystem::Recalculate 跑通的最小玩家:身份、四个背包、等级、职业、基础属性、货币。
	// 没有会话组件,所以 PushPanel 只会打一条 WARN 然后返回。
	entt::entity NewPlayer(uint64_t guid, uint32_t level)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		players.push_back(entity);
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, Guid{guid});
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
		// 强化要问余额、扣金币:余额在 CurrencyComp,欠款与脏标记在 PlayerCurrencyComp
		// (做法同 player_battle_settlement_test.cpp)。余额从 0 起,要用金币的用例自己 SetGold。
		tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(entity);
		tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(entity);
		return entity;
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

	// 只穿一把武器时的期望加成:「伤害」物伤、法伤各加一份,另加若干防御(防御宝石);其余全是 0。
	// 二期的加成用例都只穿一把武器,期望值因此只有这两个数。
	static equiprules::EquipBonus WeaponBonus(uint64_t damage, uint64_t defense = 0)
	{
		using equiprules::DerivedStat;
		using equiprules::Index;
		equiprules::EquipBonus bonus;
		bonus.derivedFlat[Index(DerivedStat::kPhysicalAttack)] = damage;
		bonus.derivedFlat[Index(DerivedStat::kMagicAttack)] = damage;
		bonus.derivedFlat[Index(DerivedStat::kDefense)] = defense;
		return bonus;
	}

	// ── 二期:金币、材料 / 宝石堆、脚本随机数 ──────────────────────────────────

	uint64_t Gold() { return CurrencySystem::GetBalance(player, kCurrencyGold); }

	// 把金币余额直接摆成 amount(照 player_feature_snapshot_test.cpp 的做法写组件)。
	// 刻意不走 CurrencySystem::AddCurrency:那条路要过欠款抵扣、封禁闸与异常获得统计,而夹具要的只是
	// 「此刻恰好有这么多金币」;扣金币走不走正式入口是被测代码的事,由用例里的余额断言来验。
	void SetGold(uint64_t amount)
	{
		auto &currency = tlsEcs.actorRegistry.get<CurrencyComp>(player);
		currency.clear_values();
		for (uint32_t type = 0; type < kCurrencyMax; ++type)
		{
			currency.add_values(type == kCurrencyGold ? amount : 0);
		}
	}

	// 人物背包里某个 config 的总数量(几堆加在一起)。
	uint64_t CountOf(uint32_t configId) { return Inventory().GetTotalItemCount(configId); }

	// 往人物背包的 pos 号格摆一堆可叠加物品(预设 guid,不铸号、不过发放闸、不写流水)。
	void PutStack(Guid guid, uint32_t configId, uint32_t count, uint32_t pos)
	{
		Inventory().InsertItemForRestore(guid, configId, count, pos);
	}

	// 把「强化到 +targetLevel」那一行要的金币与材料摆好:金币恰好够,材料一堆、恰好够,各再多给 spare 份。
	// 返回那一行(期望值从它现读);行不存在记一条失败并返回 nullptr。
	// 想改这一行(成功率、掉级、消耗)的用例要**先改再调**,摆出来的才是改过之后的数。
	const EquipEnhanceTable *StockForEnhance(uint32_t targetLevel, uint64_t spareGold = 0,
											 uint32_t spareMaterial = 0)
	{
		const auto *row = EnhanceRowOrNull(targetLevel);
		if (row == nullptr)
		{
			ADD_FAILURE() << "EquipEnhance row " << targetLevel << " is missing; " << kExportFirst;
			return nullptr;
		}
		SetGold(row->cost_gold() + spareGold);
		const uint32_t stones = row->material_count() + spareMaterial;
		if (row->material_id() != 0 && stones > 0)
		{
			PutStack(kGuidStone, row->material_id(), stones, kStonePos);
		}
		return row;
	}

	// 把被测文件的全部掷骰(掷属性、强化判定)换成脚本;TearDown 复原生产随机源。
	// 闭包自己持有一份脚本,所以哪怕夹具先析构,被测文件手里的也不是悬空引用。
	ScriptedDice &UseDice(std::vector<uint64_t> script)
	{
		dice = std::make_shared<ScriptedDice>(std::move(script));
		PlayerEquipSystem::SetRandomSourceForTest(
			[scripted = dice](uint64_t range) { return scripted->Roll(range); });
		return *dice;
	}

	ForgeState CaptureForge()
	{
		ForgeState state;
		state.gold = Gold();
		state.inventory = Capture(Inventory());
		state.equipment = Capture(Equipment());
		return state;
	}

	// 金币、人物背包、装备栏都没变:强化 / 镶嵌 / 取下拒绝路径的统一断言。
	// 实例的强化等级与宝石记录都在整份实例的序列化字节里,所以「没改实例」也一并钉住了。
	void ExpectForgeUnchanged(const ForgeState &before)
	{
		EXPECT_EQ(before.gold, Gold()) << "a refused call must not charge gold";
		ExpectBagsUnchanged(before.inventory, before.equipment);
	}

	// 派生组件在夹具初始化时已经存在;重算会把 max_health 从默认 0 写成至少 1。
	// 用值的变化判断是否触发重算(与一期穿脱用例的判据相同)。
	bool AttributesWereRecalculated()
	{
		const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
		return derived != nullptr && derived->max_health() != 0;
	}

	std::vector<entt::entity> players;
	entt::entity player{entt::null};
	SceneBagClientPlayerHandler handler{std::make_unique<TestBagService>()};
	std::shared_ptr<ScriptedDice> dice;
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

	// 没装回调时,入包的装备没有 equip 段 —— 属性确实是回调掷的,不是别处顺手写的。
	ItemCountMap one;
	one[kWeaponL80] = 1;
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), one));
	Inventory().ForEachItem([](Guid, const ItemComp &item) { EXPECT_FALSE(item.has_equip()); });
	const BagState unrolled = Capture(Inventory());

	PlayerEquipSystem::InstallItemInitializer();
	ItemCountMap batch;
	batch[kWeaponL80] = 3;
	batch[kPlainNonStack] = 1;
	batch[kStackable] = 5;
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), batch));

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
	ItemCountMap one;
	one[kWeaponL80] = 1;

	PlayerEquipSystem::InstallItemInitializer();
	BagService::SetItemInstanceInitializer({}); // 卸载
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), one));
	Inventory().ForEachItem([](Guid, const ItemComp &item)
							{ EXPECT_FALSE(item.has_equip()) << "the callback is really gone"; });
	const BagState unrolled = Capture(Inventory());

	PlayerEquipSystem::InstallItemInitializer(); // 卸掉之后再装:必须真的装上
	PlayerEquipSystem::InstallItemInitializer(); // 连调两次无害
	ASSERT_EQ(kSuccess, BagService::AddItems(player, Inventory(), one));

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
	Inventory().RestoreItems({{weapon, 5}});
	Inventory().RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 6}});
	ASSERT_FALSE(AttributesWereRecalculated());

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
	EXPECT_TRUE(AttributesWereRecalculated());
}

TEST_F(PlayerEquipTest, EquipChecksTheLevelRequirement)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL80), 0}});
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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
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
	Inventory().RestoreItems({{MakeInstance(kGuidA, kPlainNonStack), 0}});
	Inventory().InsertItemForRestore(kGuidB, kStackable, /*stackSize=*/5, /*pos=*/1);
	Warehouse().RestoreItems({{MakeRolledInstance(kGuidC, kWeaponL1), 0}});
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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
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
	Inventory().RestoreItems({{first, 0}});
	Inventory().RestoreItems({{second, 1}});
	Inventory().RestoreItems({{third, 2}});

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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
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
	ASSERT_FALSE(AttributesWereRecalculated());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidA));

	EXPECT_EQ(nullptr, Equipment().GetItemCompByGuid(kGuidA));
	EXPECT_EQ(kInvalidGuid, Equipment().Layout().At(kWeaponSlot));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	EXPECT_EQ(0u, Equipment().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
	EXPECT_TRUE(Equipment().IsLayerConsistent());
	EXPECT_TRUE(AttributesWereRecalculated())
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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
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
	Inventory().RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 0}});
	Inventory().RestoreItems({{weapon, 2}});
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
	Inventory().RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 0}}); // 与旧武器同 guid
	Inventory().RestoreItems({{newWeapon, 3}});
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
	Inventory().RestoreItems({{MakeInstance(kGuidA, kPlainNonStack), 0}}); // 与穿着的武器同 guid
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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidD, kWeaponL80), 0}});

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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL80), 0}});
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
	Inventory().RestoreItems({{MakeRolledInstance(kGuidB, kHatL1), 4}});

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

// ════════════════════════════════════════════════════════════════════════════
// 二期:强化 / 镶嵌 / 取下(docs/design/equipment-enhance-inlay.md §4.3)
// ════════════════════════════════════════════════════════════════════════════

// ── 二期:三张表的校验 ───────────────────────────────────────────────────────
//
// 「导出的表自洽」仍由前面的 ShippedTablesPassValidation 钉着(LoadTables 现在连这三张一起加载)。
// 下面照 ValidationCountsEachBrokenRow 的写法:契约里每个「恰好计一条」的条件各注入一种坏数据,
// 改动是累加的。Editable 只改行、不改索引 —— 表校验遍历的是行,所以原地改 id / equip_kind / socket
// 它都看得见(运行期按主键 / 按部位取行走的是索引,那是另一回事)。

// 强化表不是空的,而且每一行都在从 1 起连续的等级里。没有这一条,ShippedTablesPassValidation 的
// 「零问题」可能只是空表的零问题(三张表为空都不计问题:那是功能没开放,不是填错)。
TEST_F(PlayerEquipTest, MaxEnhanceLevelCoversEveryShippedRow)
{
	const std::size_t rows = EquipEnhanceTableManager::Instance().Count();
	ASSERT_GE(rows, std::size_t{kFixtureEnhanceLevels}) << kExportFirst;
	EXPECT_EQ(rows, PlayerEquipSystem::MaxEnhanceLevel());
}

// 上限只数到第一个缺口:中间缺了一级,后面的行再多也用不上,表校验把它们逐行报出来。
TEST_F(PlayerEquipTest, MaxEnhanceLevelStopsAtTheFirstGap)
{
	ASSERT_EQ(0u, PlayerEquipSystem::ValidateTables());
	const uint32_t rows = static_cast<uint32_t>(EquipEnhanceTableManager::Instance().Count());

	EditableEnhanceIndex().erase(2); // 表里没有「强化到 +2」这一级了

	EXPECT_EQ(1u, PlayerEquipSystem::MaxEnhanceLevel());
	// 从第 2 行起每一行都不在连续等级里了(被摘掉的那一行还留在整表数据里,同样算),各计一条。
	EXPECT_EQ(rows - 1, PlayerEquipSystem::ValidateTables());

	TruncateEnhanceTable(0);
	EXPECT_EQ(0u, PlayerEquipSystem::MaxEnhanceLevel()) << "no row for +1 means enhancement is closed";
}

// 强化表:只动夹具核对过存在的 1–4 级这四行。同一行上互不相干的问题各计一条(所以几种坏数据
// 叠在同一行上),同一个根因只计一次。
TEST_F(PlayerEquipTest, EnhanceTableValidationCountsEachBrokenRow)
{
	ASSERT_EQ(0u, PlayerEquipSystem::ValidateTables());
	uint32_t expected = 0;
	const auto *row1 = EnhanceRowOrNull(1);
	const auto *row2 = EnhanceRowOrNull(2);
	const auto *row3 = EnhanceRowOrNull(3);
	const auto *row4 = EnhanceRowOrNull(4);
	ASSERT_NE(nullptr, row3) << kExportFirst;
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();

	// 成功率 0:这一级永远强化不了。
	Editable(row1).set_success_rate(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 成功率超过万分之一万。
	Editable(row2).set_success_rate(equiprules::kPermyriadBase + 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 总加成比上一级还低(这一列是「强化到本级后的总加成」,不是相对上一级的增量)。
	ASSERT_GE(row1->bonus_permyriad(), 1u) << "the fixture needs room below the +1 bonus";
	Editable(row2).set_bonus_permyriad(row1->bonus_permyriad() - 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 金币消耗超过 INT64_MAX:货币接口收的是 int64,这一行没人付得起。
	Editable(row1).set_cost_gold(static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) + 1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 配了材料却没配数量 / 没配材料却填了数量:两种写法都是「这一级实际不扣材料」,各是一条。
	Editable(row1).set_material_count(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	Editable(row2).set_material_id(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 材料指向不存在的物品:只计这一条,「是不是装备 / 是不是战斗道具」无从谈起,不再查。
	Editable(row3).set_material_id(kUnknownConfig);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 材料是装备:按 config 扣数量会把带属性的装备实例当材料销毁。
	ASSERT_EQ(0u, ItemRowOrNull(kWeaponL1)->battle_usable()) << "the fixture weapon is not a battle item";
	Editable(row4).set_material_id(kWeaponL1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 材料同时还是战斗道具:与上一条互不相干,同一行再计一条。
	Editable(ItemRowOrNull(kWeaponL1)).set_battle_usable(1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 等级号不在从 1 起的连续等级里:是 0,或者比上限还大。这两行上面已经报过的问题照旧各算各的。
	Editable(row4).set_id(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	Editable(row3).set_id(maxLevel + 7);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
}

// 宝石表:夹具核对过的两行(攻击宝石、防御宝石)各承担几种坏数据。
TEST_F(PlayerEquipTest, GemTableValidationCountsEachBrokenRow)
{
	ASSERT_EQ(0u, PlayerEquipSystem::ValidateTables());
	uint32_t expected = 0;
	const auto *attackGem = GemRowOrNull(kAttackGem);
	const auto *defenseGem = GemRowOrNull(kDefenseGem);

	// 类型 0:孔位表里 0 表示「任意」,这种宝石哪个孔都嵌不进。
	Editable(attackGem).set_gem_type(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 数值 0:嵌上没有任何效果。
	Editable(attackGem).set_value(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 属性指向不存在的 EquipAttribute:嵌上也不生效。
	Editable(attackGem).set_attr_id(kUnknownAttr);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 宝石 id 在 Item 表里查不到:只计这一条,不再查「是不是装备 / 是不是战斗道具」。
	Editable(attackGem).set_id(kUnknownConfig);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 宝石 id 指向一件装备:按 config 扣发会销毁 / 重铸装备实例。这里原地把这一行的 id 改成武器的
	// config,而不去改 Item 行的 equip_kind —— 那会让 Item 段多报一条「装备可堆叠」,分不清是谁计的。
	ASSERT_EQ(0u, ItemRowOrNull(kWeaponL1)->battle_usable()) << "the fixture weapon is not a battle item";
	Editable(defenseGem).set_id(kWeaponL1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
	// 它同时还是战斗道具:与上一条互不相干,同一行再计一条。
	Editable(ItemRowOrNull(kWeaponL1)).set_battle_usable(1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 属性行存在、但它的 effect 不被规则层接受:EquipAttribute 段已经报过那一行,宝石这边不再计。
	// 所以整体恰好多一条,而不是「属性行一条 + 每颗用它的宝石各一条」(防御宝石那一行此刻仍指着它)。
	ASSERT_EQ(kAttrDefense, defenseGem->attr_id());
	Editable(AttrRowOrNull(kAttrDefense)).set_effect(99);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
}

// 孔位表:发货表的六个孔是 武器 0 / 1、帽子 0、衣服 0 / 1、鞋子 0(二期设计文档 §2.3),
// 每一步动一个还没动过的部位。
TEST_F(PlayerEquipTest, GemSocketTableValidationCountsEachBrokenRow)
{
	ASSERT_EQ(0u, PlayerEquipSystem::ValidateTables());
	uint32_t expected = 0;
	const auto &sockets = EquipGemSocketTableManager::Instance();
	const auto *shoesSocket = SocketRowOrNull(kShoesKind, 0);
	const auto *hatSocket = SocketRowOrNull(kHatKind, 0);
	const auto *clothesSecond = SocketRowOrNull(kClothesKind, 1);
	const auto *weaponSecond = SocketRowOrNull(kWeaponKind, kWeaponAnySocket);
	ASSERT_NE(nullptr, shoesSocket) << kExportFirst;
	ASSERT_NE(nullptr, hatSocket) << kExportFirst;
	ASSERT_NE(nullptr, clothesSecond) << kExportFirst;
	ASSERT_EQ(1u, sockets.GetByEquipKind(kShoesKind).size()) << kExportFirst;
	ASSERT_EQ(1u, sockets.GetByEquipKind(kHatKind).size()) << kExportFirst;
	ASSERT_EQ(2u, sockets.GetByEquipKind(kClothesKind).size()) << kExportFirst;

	// 孔位号不从 0 起:鞋子唯一的孔成了 1 号。
	Editable(shoesSocket).set_socket(1);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 部位填成 0:这个孔不属于任何装备。只计这一条。
	Editable(hatSocket).set_equip_kind(0);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 孔位号超出单件装备的上限。它已经逐行报过,所以衣服剩下的 0 号孔不会再被报成「不连续」
	// (同一个根因只计一次)。
	Editable(clothesSecond).set_socket(equiprules::kMaxGemSockets);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());

	// 同一部位重复的孔位号:武器的两个孔都成了 0 号。去重之后只剩 0 号,不再另报「不连续」。
	Editable(weaponSecond).set_socket(kWeaponAttackSocket);
	EXPECT_EQ(++expected, PlayerEquipSystem::ValidateTables());
}

// 「是不是宝石」:宝石表有这一行,而且它的物品行存在、不是装备。
TEST_F(PlayerEquipTest, IsGemNeedsAGemRowOnAnItemThatIsNotEquipment)
{
	EXPECT_TRUE(PlayerEquipSystem::IsGem(kAttackGem));
	EXPECT_TRUE(PlayerEquipSystem::IsGem(kDefenseGem));
	EXPECT_FALSE(PlayerEquipSystem::IsGem(kEnhanceStone)) << "a forge material is not a gem";
	EXPECT_FALSE(PlayerEquipSystem::IsGem(kStackable));
	EXPECT_FALSE(PlayerEquipSystem::IsGem(kWeaponL1));
	EXPECT_FALSE(PlayerEquipSystem::IsGem(kUnknownConfig));
	EXPECT_FALSE(PlayerEquipSystem::IsGem(0));

	// 宝石行还在,但它的物品被配成了装备:不再算宝石。镶嵌 / 取下按 config 扣发数量,放它过去的话
	// 嵌进去吃掉的是一件装备实例,取下来铸出的是一件重新掷属性的新装备。
	Editable(ItemRowOrNull(kAttackGem)).set_equip_kind(kWeaponKind);
	EXPECT_FALSE(PlayerEquipSystem::IsGem(kAttackGem));
}

// ── 二期:实例数据随穿脱走 ───────────────────────────────────────────────────

// 强化等级与宝石只存在于实例的 equip 段,穿脱是整份搬运:穿上、卸下各一趟,一个字节都不变。
TEST_F(PlayerEquipTest, EquipAndUnequipCarryEnhanceLevelAndGems)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem);
	Inventory().RestoreItems({{weapon, 5}});

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Equip(player, kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::Unequip(player, kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
}

// ── 二期:强化 ───────────────────────────────────────────────────────────────

// 成功:等级 +1,结果里带成败与新等级;这件装备的其它实例数据(随机属性)与所在格子都不变。
TEST_F(PlayerEquipTest, EnhanceRaisesTheLevelByOneOnSuccess)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	Inventory().RestoreItems({{weapon, 5}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({0}); // 0 小于任何合法的成功率:必成

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_TRUE(outcome.success);
	EXPECT_EQ(1u, outcome.level);
	ItemComp expected = weapon;
	expected.mutable_equip()->set_enhance_level(1);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA)) << "enhancing does not move the item";
	EXPECT_EQ(0u, Equipment().OccupiedGridCount());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 已经 +1 的装备再强化,用的是「强化到 +2」那一行(按当前等级 + 1 取行,而不是总用第一行)。
TEST_F(PlayerEquipTest, EnhanceUsesTheRowOfTheNextLevel)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/1), 0}});
	const auto *nextRow = StockForEnhance(2); // 金币、材料恰好够「强化到 +2」
	ASSERT_NE(nullptr, nextRow);
	// 把「强化到 +1」那一行改得付不起:误用了它的话,这次强化会被拒成金币不足。
	Editable(EnhanceRowOrNull(1)).set_cost_gold(nextRow->cost_gold() + 1000);
	UseDice({0});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_EQ(2u, outcome.level);
	EXPECT_EQ(0u, Gold()) << "exactly the cost of the +2 row was charged";
	EXPECT_EQ(0u, CountOf(kEnhanceStone));
}

// 扣的恰好是这一行的数:金币与材料各剩下多给的那点零头,别的东西一个不少。
TEST_F(PlayerEquipTest, EnhanceChargesExactlyTheGoldAndMaterialOfTheRow)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	PutStack(kGuidFiller, kStackable, 5, 1); // 旁观者:别的可叠加物品
	ASSERT_NE(nullptr, StockForEnhance(1, /*spareGold=*/7, /*spareMaterial=*/2));
	UseDice({0});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_EQ(7u, Gold());
	EXPECT_EQ(2u, CountOf(kEnhanceStone));
	EXPECT_EQ(5u, CountOf(kStackable)) << "nothing else is consumed";
	EXPECT_EQ(3u, Inventory().OccupiedGridCount());
	ExpectNoEmptyStacks(Inventory());
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 材料恰好用光:被抽空的那一堆当场回收,不留一个数量为 0 的格子(它会原样出现在回给客户端的快照里)。
// 回收空堆是一次整理,被强化的那件必须还在原格、等级也已经写上。
TEST_F(PlayerEquipTest, EnhanceReclaimsTheMaterialStackItEmptied)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	ASSERT_NE(nullptr, StockForEnhance(1)); // 一堆强化石,恰好够
	UseDice({0});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_EQ(0u, CountOf(kEnhanceStone));
	EXPECT_EQ(nullptr, Inventory().GetItemCompByGuid(kGuidStone)) << "the emptied stack is gone";
	EXPECT_EQ(kInvalidGuid, Inventory().Layout().At(kStonePos)) << "and released its cell";
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	ExpectNoEmptyStacks(Inventory());
	EXPECT_TRUE(Inventory().IsLayerConsistent());

	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA));
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_EQ(1u, stored->equip().enhance_level());
}

// 躺在背包里的装备不提供加成,强化它不必重算属性。
TEST_F(PlayerEquipTest, EnhanceInTheInventoryDoesNotRecalculateAttributes)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({0});
	ASSERT_FALSE(AttributesWereRecalculated());

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_FALSE(AttributesWereRecalculated());
}

// 强化的是穿在身上的那一件:它留在原槽、等级写在它上面,属性重算过(数值由 player_equip_attribute_test.cpp 验)。
TEST_F(PlayerEquipTest, EnhanceOnWornEquipmentRecalculatesAttributes)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({0});
	ASSERT_FALSE(AttributesWereRecalculated());

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_TRUE(AttributesWereRecalculated());
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
	ItemComp expected = weapon;
	expected.mutable_equip()->set_enhance_level(1);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), expected));
	EXPECT_EQ(nullptr, Inventory().GetItemCompByGuid(kGuidA)) << "it was enhanced in place, not moved";
}

// 掷骰:每次强化恰好掷一次、区间是一万;掷出的数小于成功率即成功。
TEST_F(PlayerEquipTest, EnhanceRollsExactlyOnceAndSucceedsBelowTheRate)
{
	Editable(EnhanceRowOrNull(1)).set_success_rate(5000);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	const ScriptedDice &rolls = UseDice({4999});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_TRUE(outcome.success) << "4999 is below a rate of 5000";
	EXPECT_EQ(1u, outcome.level);
	EXPECT_EQ((std::vector<uint64_t>{equiprules::kPermyriadBase}), rolls.Requested())
		<< "exactly one roll, out of ten thousand";
}

// 掷出的数等于成功率 = 没中。失败不是错误:返回成功码、结果里 success 为假;
// 金币与材料照扣,等级与整份实例原样不动。
TEST_F(PlayerEquipTest, EnhanceFailureStillChargesAndKeepsTheLevel)
{
	auto &row = Editable(EnhanceRowOrNull(4));
	row.set_success_rate(5000);
	row.set_fail_level_loss(0);
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3);
	Inventory().RestoreItems({{weapon, 5}});
	ASSERT_NE(nullptr, StockForEnhance(4, /*spareGold=*/7, /*spareMaterial=*/2));
	const ScriptedDice &rolls = UseDice({5000});

	PlayerEquipSystem::EnhanceOutcome outcome;
	outcome.success = true; // 一进函数就该被重置
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_FALSE(outcome.success) << "a roll equal to the rate is a miss";
	EXPECT_EQ(3u, outcome.level);
	EXPECT_EQ(7u, Gold()) << "a failed roll still costs the gold";
	EXPECT_EQ(2u, CountOf(kEnhanceStone)) << "and the material";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA));
	EXPECT_EQ(1u, rolls.Requested().size());
}

// 失败掉级:掉这一行的 fail_level_loss 级(表里默认 0,这里改成 2)。
TEST_F(PlayerEquipTest, EnhanceFailureDropsTheLevelByFailLevelLoss)
{
	auto &row = Editable(EnhanceRowOrNull(4));
	row.set_success_rate(5000);
	row.set_fail_level_loss(2);
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3);
	Inventory().RestoreItems({{weapon, 0}});
	ASSERT_NE(nullptr, StockForEnhance(4));
	UseDice({9999});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_FALSE(outcome.success);
	EXPECT_EQ(1u, outcome.level);
	ItemComp expected = weapon;
	expected.mutable_equip()->set_enhance_level(1);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
}

// 掉级最多掉到 0,不会回绕成一个巨大的等级;掉到 0 的装备仍是「已初始化」的那一件,随机属性都在。
TEST_F(PlayerEquipTest, EnhanceFailureNeverDropsTheLevelBelowZero)
{
	auto &row = Editable(EnhanceRowOrNull(2));
	row.set_success_rate(5000);
	row.set_fail_level_loss(5);
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/1);
	Inventory().RestoreItems({{weapon, 0}});
	ASSERT_NE(nullptr, StockForEnhance(2));
	UseDice({9999});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_FALSE(outcome.success);
	EXPECT_EQ(0u, outcome.level);
	ItemComp expected = weapon;
	expected.mutable_equip()->set_enhance_level(0);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
}

// 穿着的装备强化失败、又不掉级:等级没变,加成也就没变,不重算。
TEST_F(PlayerEquipTest, EnhanceFailureWithoutLevelLossOnWornEquipmentDoesNotRecalculate)
{
	auto &row = Editable(EnhanceRowOrNull(4));
	row.set_success_rate(5000);
	row.set_fail_level_loss(0);
	ASSERT_EQ(kSuccess,
			  Equipment().PutInstance(MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3), kWeaponSlot));
	ASSERT_NE(nullptr, StockForEnhance(4));
	UseDice({9999});
	ASSERT_FALSE(AttributesWereRecalculated());

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_FALSE(outcome.success);
	EXPECT_FALSE(AttributesWereRecalculated());
}

// 穿着的装备强化失败并掉了级:加成变小了,必须重算,否则面板停在掉级之前。
TEST_F(PlayerEquipTest, EnhanceFailureWithLevelLossOnWornEquipmentRecalculates)
{
	auto &row = Editable(EnhanceRowOrNull(4));
	row.set_success_rate(5000);
	row.set_fail_level_loss(1);
	ASSERT_EQ(kSuccess,
			  Equipment().PutInstance(MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3), kWeaponSlot));
	ASSERT_NE(nullptr, StockForEnhance(4));
	UseDice({9999});
	ASSERT_FALSE(AttributesWereRecalculated());

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_EQ(2u, outcome.level);
	EXPECT_TRUE(AttributesWereRecalculated());
}

// 这一级既不收金币也不要材料(表里可以这么配):身无分文、包里一颗强化石都没有,照样能强化。
TEST_F(PlayerEquipTest, EnhanceRowsWithoutCostsNeedNeitherGoldNorMaterial)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_cost_gold(0);
	row.set_material_id(0);
	row.set_material_count(0);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_EQ(0u, Gold());
	UseDice({0});

	PlayerEquipSystem::EnhanceOutcome outcome;
	ASSERT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));

	EXPECT_TRUE(outcome.success);
	EXPECT_EQ(1u, outcome.level);
	EXPECT_EQ(0u, Gold());
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
}

// ── 二期:强化的拒绝 ─────────────────────────────────────────────────────────
//
// 每一条都把别的条件摆成「本来能强化」(金币、材料都够),被拒只可能是要钉的那一个原因。
// 脚本随机数是空的(掷了就记一条失败):掷骰排在全部纯校验之后,被拒的强化不消耗随机数。
// 金币、人物背包、装备栏(含被强化那件的整份实例)都不许变。

// 强化表一行都没有 = 强化未开放。
TEST_F(PlayerEquipTest, EnhanceIsUnavailableWhileTheEnhanceTableIsEmpty)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({});
	TruncateEnhanceTable(0);
	ASSERT_EQ(0u, PlayerEquipSystem::MaxEnhanceLevel());
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceUnavailable, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, EnhanceIsRefusedAtTheMaximumLevel)
{
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1, maxLevel), 0}});
	ASSERT_NE(nullptr, StockForEnhance(maxLevel)); // 手里的金币与材料够再付一次最贵的那一级
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceMaxLevel, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 存档里的等级比表上限还高(策划把上限下调了):按上限算,所以是「已满级」;存档值不改写。
TEST_F(PlayerEquipTest, EnhanceTreatsAStoredLevelAboveTheCapAsTheCap)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3), 0}});
	ASSERT_NE(nullptr, StockForEnhance(4));
	UseDice({});
	TruncateEnhanceTable(2); // 上限下调到 +2
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceMaxLevel, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_EQ(3u, stored->equip().enhance_level()) << "the saved level is never rewritten";
}

// 材料只认人物背包,而且要够数。金币也不够时先报的是材料(校验顺序:材料在金币之前)。
TEST_F(PlayerEquipTest, EnhanceIsRefusedWithoutEnoughMaterial)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_count(3);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	PutStack(kGuidStone, kEnhanceStone, 2, kStonePos);                   // 差一颗
	Warehouse().InsertItemForRestore(kGuidFiller, kEnhanceStone, 9, 0); // 仓库里的不算
	SetGold(row.cost_gold());
	UseDice({});
	const ForgeState before = CaptureForge();
	const BagState warehouseBefore = Capture(Warehouse());

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceMaterialNotEnough, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));

	SetGold(0);
	EXPECT_EQ(kEquipEnhanceMaterialNotEnough, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
}

// 一堆带着更新版本字段(新版本给物品加的属性,本版本不认识)的可叠加物品。按 config 计数和扣数量时它不算普通单位
// (ItemStore::IsCountableByConfig,混版本设计 §4.4 第 6 批)。
ItemComp MakeStackFromNewerVersion(Guid guid, uint32_t configId, uint32_t count)
{
	ItemComp stack;
	stack.set_item_id(guid);
	stack.set_config_id(configId);
	stack.set_size(count);
	stack.mutable_unknown_fields()->AddVarint(412345, 1);
	return stack;
}

// 材料只有算上带更新版本字段的那堆才够:版本错位,回「暂不可用」而不是「材料不足」,金币与材料都不动 ——
// 预检与提交同一个判据,不会扣了金币才发现材料扣不出。
TEST_F(PlayerEquipTest, EnhanceWithMaterialOnlyEnoughCountingNewerVersionStacksIsAVersionSkew)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_count(3);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0},
							  {MakeStackFromNewerVersion(kGuidFiller, kEnhanceStone, 5), kStonePos + 1}});
	PutStack(kGuidStone, kEnhanceStone, 1, kStonePos); // 普通的只有 1 颗
	SetGold(row.cost_gold());
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kFeatureUnavailable, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, EnhanceIsRefusedWithoutEnoughGold)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	const auto *row = StockForEnhance(1);
	ASSERT_NE(nullptr, row);
	SetGold(row->cost_gold() - 1); // 差一枚
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceGoldNotEnough, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 表里的金币消耗超过 INT64_MAX(货币接口收的是 int64):不管玩家有多少钱都按付不起拒,金币与背包不动。
// (这条只钉对外行为。实现里另有一道显式的上限判断,免得把它转成负数递给货币接口 —— 那道闸去掉之后
// 货币接口自己也会拒,返回值相同,所以这里验不到它。)
TEST_F(PlayerEquipTest, EnhanceTreatsAnUnpayableGoldCostAsNotEnoughGold)
{
	Editable(EnhanceRowOrNull(1))
		.set_cost_gold(static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) + 1);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	SetGold(std::numeric_limits<uint64_t>::max());
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceGoldNotEnough, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, EnhanceRejectsItemsThatAreNotEquipment)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kPlainNonStack), 0}});
	PutStack(kGuidB, kStackable, 5, 1);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidC, kWeaponL80), 2}});
	// 被误配成可堆叠的「装备」:搬运原语不认它,强化也按非装备拒。
	Editable(ItemRowOrNull(kWeaponL80)).set_max_stack_size(5);
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Enhance(player, kGuidA, outcome)) << "not equipment";
	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Enhance(player, kGuidB, outcome))
		<< "a stack is not equipment";
	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Enhance(player, kGuidStone, outcome))
		<< "the material itself is not equipment";
	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::Enhance(player, kGuidC, outcome))
		<< "equipment misconfigured as stackable";
	ExpectForgeUnchanged(before);
}

// 只找人物背包与装备栏:仓库里的装备强化不了。
TEST_F(PlayerEquipTest, EnhanceCannotFindItemsOutsideTheInventoryAndEquipment)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}}); // 背包里确实有一件能强化的
	Warehouse().RestoreItems({{MakeRolledInstance(kGuidC, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({});
	const ForgeState before = CaptureForge();
	const BagState warehouseBefore = Capture(Warehouse());

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Enhance(player, kGuidMissing, outcome));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Enhance(player, 0, outcome));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Enhance(player, kInvalidGuid, outcome));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::Enhance(player, kGuidC, outcome))
		<< "equipment in the warehouse cannot be enhanced";
	ExpectForgeUnchanged(before);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));
}

// 强化是玩家可触发的背包扣减入口:跨区冻结中、战斗中都拒绝(战斗闸必须在这一层)。
TEST_F(PlayerEquipTest, EnhanceIsRefusedWhileFrozenOrInBattle)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	const ScriptedDice &rolls = UseDice({0});
	const ForgeState before = CaptureForge();
	PlayerEquipSystem::EnhanceOutcome outcome;

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	// 两个状态同时在:先报冻结(与穿脱同一个判定顺序)。
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
	EXPECT_TRUE(rolls.Requested().empty()) << "a refused enhance never rolls";

	tlsEcs.actorRegistry.remove<InBattleComp>(player);
	EXPECT_EQ(kSuccess, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	EXPECT_TRUE(outcome.success);
}

// 没掷过属性的实例(has_equip() 为假)是一期定义的合法状态:它没有 equip 段可写,强化入口回业务码,
// 而且绝不能顺手替它建出 equip 段 —— 那会把「没初始化」变成「已初始化、零条属性」。
TEST_F(PlayerEquipTest, EnhanceIsUnavailableForUninitialisedInstances)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceUnavailable, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_FALSE(stored->has_equip()) << "a refused enhance must not initialise the instance";
}

// 下一级的成功率是 0:必败的强化不收费,直接说「无法强化」。
TEST_F(PlayerEquipTest, EnhanceIsUnavailableWhenTheNextLevelCanNeverSucceed)
{
	Editable(EnhanceRowOrNull(1)).set_success_rate(0);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	ASSERT_NE(nullptr, StockForEnhance(1));
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipEnhanceUnavailable, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 材料被配成了装备:按 config 扣数量不看是不是装备,放行的话被销毁的是玩家包里带属性的装备实例。
// 表校验只打日志,所以写入口必须自己挡住(fail-closed)。
TEST_F(PlayerEquipTest, EnhanceFailsClosedWhenTheMaterialIsEquipment)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_id(kOldEquipA);
	row.set_material_count(1);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	// 背包里恰好有一件那种「材料」:数量是够的,拦不住它就会被扣掉。
	Inventory().RestoreItems({{MakeRolledInstance(kGuidB, kOldEquipA), 1}});
	SetGold(row.cost_gold());
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 更糟的一种:材料就是被强化的这件装备自己的 config —— 放行的话被抽成 0 的正是它自己。
TEST_F(PlayerEquipTest, EnhanceFailsClosedWhenTheMaterialIsTheTargetItself)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_id(kWeaponL1);
	row.set_material_count(1);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	SetGold(row.cost_gold());
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 材料指向一个不存在的物品:这一级无法强化,而且不能只扣金币。
TEST_F(PlayerEquipTest, EnhanceFailsClosedWhenTheMaterialRowIsMissing)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_id(kUnknownConfig);
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	SetGold(row.cost_gold());
	UseDice({});
	const ForgeState before = CaptureForge();

	PlayerEquipSystem::EnhanceOutcome outcome;
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Enhance(player, kGuidA, outcome));
	ExpectForgeUnchanged(before);
}

// 三个新写入口共用的前置:无效实体、还没加载出背包的玩家;而且不许顺手造出背包或货币组件。
TEST_F(PlayerEquipTest, ForgeEntriesRejectInvalidEntitiesAndPlayersWithoutBags)
{
	PlayerEquipSystem::EnhanceOutcome outcome;
	outcome.success = true;
	outcome.level = 9;
	EXPECT_EQ(kEntityIsNull, PlayerEquipSystem::Enhance(entt::null, kGuidA, outcome));
	EXPECT_FALSE(outcome.success) << "the outcome is reset even when the call is refused";
	EXPECT_EQ(0u, outcome.level);
	EXPECT_EQ(kEntityIsNull,
			  PlayerEquipSystem::InlayGem(entt::null, kGuidA, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(kEntityIsNull, PlayerEquipSystem::RemoveGem(entt::null, kGuidA, kWeaponAttackSocket));

	const auto bagless = tlsEcs.actorRegistry.create();
	players.push_back(bagless);
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::Enhance(bagless, kGuidA, outcome));
	EXPECT_EQ(kEquipInternalError,
			  PlayerEquipSystem::InlayGem(bagless, kGuidA, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::RemoveGem(bagless, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<PlayerBagsComp>(bagless))
		<< "a write entry must not create the bags as a side effect";
	EXPECT_EQ(nullptr, tlsEcs.actorRegistry.try_get<CurrencyComp>(bagless))
		<< "nor the currency component";
}

// ── 二期:镶嵌 ───────────────────────────────────────────────────────────────

// 成功:实例上多一条 {孔位, 宝石 config};强化等级、随机属性、所在格子都不变。
TEST_F(PlayerEquipTest, InlayGemRecordsTheGemOnTheSocket)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/2);
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));

	ItemComp expected = weapon;
	AddGem(expected, kWeaponAttackSocket, kAttackGem);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA)) << "inlaying does not move the item";
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 恰好消耗 1 颗:这种宝石少一颗,别的宝石、材料、金币都不动。
TEST_F(PlayerEquipTest, InlayGemConsumesExactlyOneGem)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	PutStack(kGuidGemB, kDefenseGem, 4, kGemPosB);
	PutStack(kGuidStone, kEnhanceStone, 6, kStonePos);
	SetGold(777);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));

	EXPECT_EQ(2u, CountOf(kAttackGem));
	EXPECT_EQ(4u, CountOf(kDefenseGem));
	EXPECT_EQ(6u, CountOf(kEnhanceStone));
	EXPECT_EQ(777u, Gold()) << "inlaying costs no gold";
	EXPECT_EQ(4u, Inventory().OccupiedGridCount());
	ExpectNoEmptyStacks(Inventory());
}

// 嵌掉的是最后一颗:被抽空的那一堆当场回收,装备还在原格、宝石已经记上。
TEST_F(PlayerEquipTest, InlayGemReclaimsTheStackItEmptied)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 1, kGemPosA);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));

	EXPECT_EQ(0u, CountOf(kAttackGem));
	EXPECT_EQ(nullptr, Inventory().GetItemCompByGuid(kGuidGemA)) << "the emptied stack is gone";
	EXPECT_EQ(kInvalidGuid, Inventory().Layout().At(kGemPosA)) << "and released its cell";
	EXPECT_EQ(1u, Inventory().OccupiedGridCount());
	ExpectNoEmptyStacks(Inventory());
	EXPECT_TRUE(Inventory().IsLayerConsistent());

	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA));
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	ASSERT_EQ(1, stored->equip().gems_size());
	EXPECT_EQ(kAttackGem, stored->equip().gems(0).gem_config_id());
}

// 通用孔(类型 0)收任意类型的宝石;两个孔互不相干,0 号孔嵌着也不妨碍 1 号孔。
TEST_F(PlayerEquipTest, InlayGemAcceptsAnyTypeInAnAnySocket)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	Inventory().RestoreItems({{MakeRolledInstance(kGuidB, kWeaponL1), 6}});
	PutStack(kGuidGemA, kAttackGem, 2, kGemPosA);
	PutStack(kGuidGemB, kDefenseGem, 2, kGemPosB);

	// 防御宝石进不了攻击孔,但进得了通用孔。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAnySocket, kDefenseGem));
	ItemComp expected = weapon;
	AddGem(expected, kWeaponAnySocket, kDefenseGem);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));

	// 攻击宝石同样进得了通用孔。
	EXPECT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidB, kWeaponAnySocket, kAttackGem));
}

// 镶在背包里的装备上:它不提供加成,不必重算属性。
TEST_F(PlayerEquipTest, InlayGemInTheInventoryDoesNotRecalculateAttributes)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 1, kGemPosA);
	ASSERT_FALSE(AttributesWereRecalculated());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));

	EXPECT_FALSE(AttributesWereRecalculated());
}

// 镶在穿着的装备上:记录写在装备栏里的那一件上(它留在原槽),宝石仍从人物背包扣,属性重算过。
TEST_F(PlayerEquipTest, InlayGemOnWornEquipmentRecalculatesAttributes)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	PutStack(kGuidGemA, kAttackGem, 2, kGemPosA);
	ASSERT_FALSE(AttributesWereRecalculated());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));

	EXPECT_TRUE(AttributesWereRecalculated());
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
	ItemComp expected = weapon;
	AddGem(expected, kWeaponAttackSocket, kAttackGem);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), expected));
	EXPECT_EQ(1u, CountOf(kAttackGem));
}

// ── 二期:镶嵌的拒绝(金币、两个包、被镶那件的整份实例都不许变)────────────────

TEST_F(PlayerEquipTest, InlayGemRejectsConfigsThatAreNotGems)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	Inventory().RestoreItems({{MakeRolledInstance(kGuidB, kWeaponL1), 6}});
	PutStack(kGuidStone, kEnhanceStone, 3, kStonePos);
	PutStack(kGuidFiller, kStackable, 3, 1);
	const ForgeState before = CaptureForge();

	// 这几样背包里都有,被拒不是因为「没有」。
	EXPECT_EQ(kEquipGemNotGem,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kEnhanceStone))
		<< "a forge material is not a gem";
	EXPECT_EQ(kEquipGemNotGem, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kStackable));
	EXPECT_EQ(kEquipGemNotGem, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kWeaponL1))
		<< "equipment is not a gem";
	EXPECT_EQ(kEquipGemNotGem,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kUnknownConfig));
	EXPECT_EQ(kEquipGemNotGem, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, 0));
	ExpectForgeUnchanged(before);
}

// 宝石行指向的物品被配成了装备:不把它当宝石(放行的话,按 config 扣掉的是一件装备实例)。
TEST_F(PlayerEquipTest, InlayGemRejectsGemRowsWhoseItemIsEquipment)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	Editable(ItemRowOrNull(kAttackGem)).set_equip_kind(kWeaponKind);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemNotGem, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
}

// 宝石只认人物背包里的:包里一颗都没有就拒,仓库里的不算。
TEST_F(PlayerEquipTest, InlayGemIsRefusedWhenTheGemIsNotInTheInventory)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemB, kDefenseGem, 2, kGemPosB); // 有别的宝石,没有要嵌的那一种
	Warehouse().InsertItemForRestore(kGuidGemA, kAttackGem, 5, 0);
	const ForgeState before = CaptureForge();
	const BagState warehouseBefore = Capture(Warehouse());

	EXPECT_EQ(kEquipGemNotOwned, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));
}

// 要嵌的那种宝石只有带更新版本字段的一堆:版本错位,回「暂不可用」,不当普通宝石嵌进去(装备上只记 config,
// 拆下时会生成普通宝石,新属性就此洗掉),装备与宝石都不动(混版本设计 §4.4 第 6 批)。
TEST_F(PlayerEquipTest, InlayGemWithOnlyNewerVersionGemsIsAVersionSkew)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5},
							  {MakeStackFromNewerVersion(kGuidGemA, kAttackGem, 3), kGemPosA}});
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kFeatureUnavailable, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
}

// 孔位不存在:这个部位的孔位定义里没有这个号。没有任何孔的部位,哪个号都不存在。
TEST_F(PlayerEquipTest, InlayGemRejectsSocketsThePartDoesNotHave)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	ItemComp oldEquip = MakeInstance(kGuidB, kOldEquipA);
	oldEquip.mutable_equip(); // 已初始化;它的部位(部位 1)一个孔都没配
	Inventory().RestoreItems({{oldEquip, 6}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	PutStack(kGuidGemB, kDefenseGem, 3, kGemPosB);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketInvalid,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kUndefinedSocket, kAttackGem));
	EXPECT_EQ(kEquipGemSocketInvalid,
			  PlayerEquipSystem::InlayGem(player, kGuidA, equiprules::kMaxGemSockets, kAttackGem));
	EXPECT_EQ(kEquipGemSocketInvalid,
			  PlayerEquipSystem::InlayGem(player, kGuidA, std::numeric_limits<uint32_t>::max(), kAttackGem));
	EXPECT_EQ(kEquipGemSocketInvalid,
			  PlayerEquipSystem::InlayGem(player, kGuidB, kWeaponAttackSocket, kAttackGem))
		<< "a part without socket rows has no socket 0 either";
	ExpectForgeUnchanged(before);
}

// 孔里已有宝石:拒绝,要先取下(不做「顶替并退回」)。判定顺序是 孔位存在 → 已占用 → 类型,
// 所以拿一颗类型也不对的宝石去嵌已占的孔,报的仍是「已占用」。
TEST_F(PlayerEquipTest, InlayGemRejectsAnOccupiedSocket)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	PutStack(kGuidGemB, kDefenseGem, 3, kGemPosB);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketOccupied,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(kEquipGemSocketOccupied,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kDefenseGem))
		<< "occupancy is reported before the type mismatch";
	ExpectForgeUnchanged(before);
}

// 「已占」看的是实例上的原始记录:占着孔的那条记录此刻不生效(这里它的 config 压根不是宝石),
// 孔照样算占着 —— 否则会在同一个孔上追加第二条记录,取下时后一颗被无声清掉。
TEST_F(PlayerEquipTest, InlayGemTreatsADeadRecordAsOccupyingItsSocket)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kStackable);
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketOccupied,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, InlayGemRejectsAGemOfTheWrongType)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemB, kDefenseGem, 3, kGemPosB);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemTypeMismatch,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kDefenseGem))
		<< "the attack socket only takes attack gems";
	ExpectForgeUnchanged(before);
}

// 没掷过属性的实例没有 equip 段可写:对玩家而言这件装备此刻没有可用的孔。绝不替它建出 equip 段。
TEST_F(PlayerEquipTest, InlayGemIsRefusedForUninitialisedInstances)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketInvalid,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_FALSE(stored->has_equip()) << "a refused inlay must not initialise the instance";
}

// 脏数据:实例上的宝石记录已经到了上限(全在孔位定义之外的孔上),目标孔本身是空的、类型也对。
// 消费侧只看前 kMaxGemSockets 条,再追加的那一条永远不会生效 —— 所以拒绝,宝石不扣。
TEST_F(PlayerEquipTest, InlayGemFailsClosedWhenTheInstanceAlreadyHoldsTooManyRecords)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	for (uint32_t i = 0; i < equiprules::kMaxGemSockets; ++i)
	{
		AddGem(weapon, 100 + i, kAttackGem);
	}
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipInternalError,
			  PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, InlayGemRejectsTargetsThatAreMissingOrNotEquipment)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kPlainNonStack), 0}});
	Warehouse().RestoreItems({{MakeRolledInstance(kGuidC, kWeaponL1), 0}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	const ForgeState before = CaptureForge();
	const BagState warehouseBefore = Capture(Warehouse());

	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(kEquipNotEquipment,
			  PlayerEquipSystem::InlayGem(player, kGuidGemA, kWeaponAttackSocket, kAttackGem))
		<< "a gem cannot be inlaid into a gem";
	EXPECT_EQ(kEquipItemNotFound,
			  PlayerEquipSystem::InlayGem(player, kGuidMissing, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(kEquipItemNotFound,
			  PlayerEquipSystem::InlayGem(player, kGuidC, kWeaponAttackSocket, kAttackGem))
		<< "equipment in the warehouse cannot be inlaid";
	ExpectForgeUnchanged(before);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));
}

TEST_F(PlayerEquipTest, InlayGemIsRefusedWhileFrozenOrInBattle)
{
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);
	const ForgeState before = CaptureForge();

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ExpectForgeUnchanged(before);

	tlsEcs.actorRegistry.remove<InBattleComp>(player);
	EXPECT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
}

// ── 二期:取下 ───────────────────────────────────────────────────────────────

// 成功:退回 1 颗同 config 的宝石到人物背包。退回来的是一颗普通的可叠加物品(没有 equip 段),
// 走正式发放入口但不掷骰(装着掷属性回调也一样);取下免费。
TEST_F(PlayerEquipTest, RemoveGemReturnsTheGemToTheInventory)
{
	PlayerEquipSystem::InstallItemInitializer(); // 与生产一致:新铸的装备会掷属性,宝石不会
	UseDice({});
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	SetGold(777);
	ASSERT_EQ(0u, CountOf(kAttackGem));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_EQ(1u, CountOf(kAttackGem));
	EXPECT_EQ(2u, Inventory().OccupiedGridCount());
	Inventory().ForEachItem(
		[](Guid, const ItemComp &item)
		{
			if (item.config_id() == kAttackGem)
			{
				EXPECT_EQ(1u, item.size());
				EXPECT_FALSE(item.has_equip()) << "a returned gem is a plain stackable item";
			}
		});
	EXPECT_EQ(777u, Gold()) << "taking a gem out is free";
	EXPECT_EQ(5u, Inventory().GetItemPosByGuid(kGuidA));
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 只抹被取下的那个孔:另一个孔的记录、强化等级、随机属性都原样。
TEST_F(PlayerEquipTest, RemoveGemClearsOnlyTheRequestedSocket)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/2);
	ItemComp expected = weapon;
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem);
	AddGem(expected, kWeaponAnySocket, kDefenseGem);
	Inventory().RestoreItems({{weapon, 5}});

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
	EXPECT_EQ(1u, CountOf(kAttackGem));
	EXPECT_EQ(0u, CountOf(kDefenseGem)) << "the other socket keeps its gem";
}

// 镶上再取下:装备回到镶嵌之前的样子(逐字节),宝石数量也回到原数。
TEST_F(PlayerEquipTest, InlayThenRemoveRestoresTheInstanceAndTheGemCount)
{
	const ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/1);
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 3, kGemPosA);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	ASSERT_EQ(2u, CountOf(kAttackGem));
	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), weapon));
	EXPECT_EQ(3u, CountOf(kAttackGem));
	EXPECT_EQ(2u, Inventory().OccupiedGridCount()) << "the gem went back onto its stack";
	// 取下之后这个孔又能嵌了。
	EXPECT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
}

TEST_F(PlayerEquipTest, RemoveGemInTheInventoryDoesNotRecalculateAttributes)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	ASSERT_FALSE(AttributesWereRecalculated());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_FALSE(AttributesWereRecalculated());
}

// 从穿着的装备上取下:装备留在原槽、记录抹掉,宝石进人物背包,属性重算过。
TEST_F(PlayerEquipTest, RemoveGemOnWornEquipmentRecalculatesAttributes)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_FALSE(AttributesWereRecalculated());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_TRUE(AttributesWereRecalculated());
	EXPECT_EQ(kWeaponSlot, Equipment().GetItemPosByGuid(kGuidA));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), bare));
	EXPECT_EQ(1u, CountOf(kAttackGem));
}

// 空孔:实例上没有这个孔的记录(孔本身存在与否都一样)。
TEST_F(PlayerEquipTest, RemoveGemFromAnEmptySocketIsRefused)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem);
	Inventory().RestoreItems({{weapon, 5}});
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketEmpty, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(kEquipGemSocketEmpty, PlayerEquipSystem::RemoveGem(player, kGuidA, kUndefinedSocket));
	ExpectForgeUnchanged(before);
}

// 没掷过属性的实例上不可能有宝石:报「空孔」,而且不替它建出 equip 段。
TEST_F(PlayerEquipTest, RemoveGemFromAnUninitialisedInstanceReportsAnEmptySocket)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kWeaponL1), 5}});
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemSocketEmpty, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);
	const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_FALSE(stored->has_equip());
}

// 人物背包没有空格、也没有同种宝石的堆可并:拒绝,宝石仍在装备上。腾出一格之后就取得下来。
TEST_F(PlayerEquipTest, RemoveGemIsRefusedWhenTheInventoryIsFullAndNoStackCanTakeIt)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_EQ(kSuccess, Inventory().PutInstance(MakeInstance(kGuidB, kPlainNonStack)));
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemBagFull, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);

	ASSERT_EQ(kSuccess, Inventory().RemoveItem(kGuidB));
	EXPECT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(1u, CountOf(kAttackGem));
}

// 人物背包没有空格,但有一堆没满的同种宝石:并进去,取得下来。
TEST_F(PlayerEquipTest, RemoveGemMergesIntoAPartialStackWhenTheInventoryIsFull)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	PutStack(kGuidGemA, kAttackGem, 5, 0);
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_EQ(6u, CountOf(kAttackGem));
	EXPECT_EQ(1u, Inventory().OccupiedGridCount()) << "it joined the existing stack";
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), bare));
	EXPECT_TRUE(Inventory().IsLayerConsistent());
}

// 人物背包没有空格,同种宝石的堆也都满了:没有哪一堆还塞得下,同样是「背包已满」,宝石仍在装备上。
// (静默预检按「有没有还塞得下的堆」判,不是按「有没有这种宝石」判;即使预检放过去,发放那一步的返回码
// 也映射成同一个码。)
TEST_F(PlayerEquipTest, RemoveGemIsRefusedWhenEveryStackOfThatGemIsFull)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	PutStack(kGuidGemA, kAttackGem, ItemRowOrNull(kAttackGem)->max_stack_size(), 0);
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipGemBagFull, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);
}

// 容器层的不足分支经 PrintStackAndReturnError 打「Stack trace (error code: …)」再加一段调用栈。
// 照 player_equip_attribute_test.cpp 的 PanelPushProbe:临时接管 muduo 日志出口,数这句原文出现了几次。
// 原文在 engine/core/error_handling/error_handling.h;那边改了措辞,用例开头的校准断言(直接打一次必须恰好数到 1)
// 会先红,不会把「没走到容器层」和「数不到」混为一谈。
const char *const kStackTraceDump = "Stack trace (error code: ";
int gStackTraceDumps = 0;

void EchoLogToStdout(const char *msg, int len)
{
	std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void CountStackTraceDumps(const char *msg, int len)
{
	if (std::string_view(msg, static_cast<std::size_t>(len)).find(kStackTraceDump) != std::string_view::npos)
	{
		++gStackTraceDumps;
	}
	EchoLogToStdout(msg, len);
}

// 作用域守卫:构造时接管日志出口并清零,析构时还给 stdout(用例中途 ASSERT 退出也会还)。
class StackTraceDumpProbe
{
public:
	StackTraceDumpProbe()
	{
		gStackTraceDumps = 0;
		muduo::Logger::setOutput(CountStackTraceDumps);
	}
	~StackTraceDumpProbe() { muduo::Logger::setOutput(EchoLogToStdout); }
	StackTraceDumpProbe(const StackTraceDumpProbe &) = delete;
	StackTraceDumpProbe &operator=(const StackTraceDumpProbe &) = delete;

	int Count() const { return gStackTraceDumps; }
	void Reset() { gStackTraceDumps = 0; }
};

// 混版本保护(docs/design/mixed-version-persist-field-protection.md §4.4):人物背包没有空格,唯一没满的同种宝石堆
// 带着本版本不认识的字段 —— 容器层不会把退回的宝石并进这种堆。静默预检必须与容器层同一个判据(ItemStore::CanStack),
// 在预检就答「背包已满」;放行到发放那一步,返回码虽然也映射成同一个码,却会先打一条 ERROR + 调用栈。
TEST_F(PlayerEquipTest, RemoveGemPrecheckSkipsAStackCarryingUnknownFields)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	constexpr int kNewerVersionFieldNumber = 412345; // 本版本不认识的字段号
	ItemComp gemsFromNewerVersion;
	gemsFromNewerVersion.set_item_id(kGuidGemA);
	gemsFromNewerVersion.set_config_id(kAttackGem);
	gemsFromNewerVersion.set_size(5);
	gemsFromNewerVersion.mutable_unknown_fields()->AddVarint(kNewerVersionFieldNumber, 1);
	Inventory().RestoreItems({{gemsFromNewerVersion, 0}});
	Inventory().SetCapacityForRestore(1);
	ASSERT_TRUE(Inventory().IsFull());
	const auto *gemItemRow = ItemRowOrNull(kAttackGem);
	ASSERT_NE(nullptr, gemItemRow);
	ASSERT_LT(5u, gemItemRow->max_stack_size()) << "这一堆按数量还塞得下,只是带着未知字段";
	const ForgeState before = CaptureForge();

	StackTraceDumpProbe probe;
	(void)PrintStackAndReturnError(kEquipGemBagFull, 1);
	ASSERT_EQ(1, probe.Count()) << "校准:探针必须数得到容器层不足分支打的那句原文";
	probe.Reset();

	EXPECT_EQ(kEquipGemBagFull, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(0, probe.Count()) << "静默预检就该拒绝;走到 BagService::AddItems 才拒会打 ERROR + 调用栈";
	ExpectForgeUnchanged(before);
	const ItemComp *kept = Inventory().GetItemCompByGuid(kGuidGemA);
	ASSERT_NE(nullptr, kept);
	EXPECT_EQ(5u, kept->size());
	ASSERT_EQ(1, kept->unknown_fields().field_count()) << "未知字段原样留在那一堆上";
	EXPECT_EQ(kNewerVersionFieldNumber, kept->unknown_fields().field(0).number());
}

// 脏数据:同一个孔上有两条记录。退回的是第一条那颗,两条都清掉(第二条不退物,只留日志);
// 别的孔不受牵连。
TEST_F(PlayerEquipTest, RemoveGemWithDuplicateRecordsReturnsTheFirstAndClearsBoth)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	AddGem(weapon, kWeaponAnySocket, kAttackGem);
	AddGem(weapon, kWeaponAttackSocket, kDefenseGem); // 同孔的第二条
	Inventory().RestoreItems({{weapon, 5}});

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_EQ(1u, CountOf(kAttackGem)) << "the first record's gem is returned";
	EXPECT_EQ(0u, CountOf(kDefenseGem)) << "the duplicate is cleared without a refund";
	ItemComp expected = bare;
	AddGem(expected, kWeaponAnySocket, kAttackGem);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), expected));
	EXPECT_EQ(kEquipGemSocketEmpty, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket))
		<< "nothing is left on that socket";
}

// 脏数据:孔上有一条 gem_config_id == 0 的空壳记录(只可能来自外来的 equip 段或存档损坏;镶嵌入口自己
// 写不出 0)。协议里 0 就是「空孔」,这种记录没有东西可退 —— 取下 / 镶嵌 / tooltip / 加成四处都把它当成
// 不存在,否则服务器说「已占」、客户端显示空孔、取下必败,这个孔对玩家永久作废。重新嵌一次即自愈。
TEST_F(PlayerEquipTest, GemRecordWithConfigZeroCountsAsAnEmptySocketAndHealsOnTheNextInlay)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, 0);
	Inventory().RestoreItems({{weapon, 5}});
	PutStack(kGuidGemA, kAttackGem, 1, 0);

	// 取下:这个孔是空的,什么都不动。
	const ForgeState before = CaptureForge();
	EXPECT_EQ(kEquipGemSocketEmpty, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);

	// tooltip:按空孔下发(孔位按 socket 升序,武器的攻击孔是 0 号,排第一个)。
	{
		const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
		ASSERT_NE(nullptr, stored);
		BagItemInfo info;
		PlayerEquipSystem::FillItemDisplay(*stored, info);
		ASSERT_GT(info.gem_sockets_size(), 0);
		EXPECT_EQ(kWeaponAttackSocket, info.gem_sockets(0).socket());
		EXPECT_EQ(0u, info.gem_sockets(0).gem_config_id());
		EXPECT_FALSE(info.gem_sockets(0).has_attr());
	}

	// 镶嵌:不算占用,嵌得进去;新嵌的这颗生效(空壳记录不占「该孔第一条」的位置)。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::InlayGem(player, kGuidA, kWeaponAttackSocket, kAttackGem));
	EXPECT_EQ(0u, CountOf(kAttackGem));
	{
		const ItemComp *stored = Inventory().GetItemCompByGuid(kGuidA);
		ASSERT_NE(nullptr, stored);
		BagItemInfo info;
		PlayerEquipSystem::FillItemDisplay(*stored, info);
		ASSERT_GT(info.gem_sockets_size(), 0);
		EXPECT_EQ(kAttackGem, info.gem_sockets(0).gem_config_id());
		EXPECT_TRUE(info.gem_sockets(0).has_attr()) << "the freshly inlaid gem must be effective";
	}

	// 再取下:退回的是新嵌的那颗;空壳记录一并清掉,实例回到没有任何宝石记录的样子。
	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(1u, CountOf(kAttackGem));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), bare));
}

// 策划删掉了这个孔的定义:孔里的宝石不再生效,但照样取得下来(取下不要求孔位仍在表里)。
TEST_F(PlayerEquipTest, RemoveGemStillWorksAfterTheSocketDefinitionIsGone)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	// 「删掉」武器的 0 号孔:把那一行的孔位号改成一个别的号。运行期按部位取孔位走索引、读的是行上的
	// socket,所以这么改等于 0 号孔没有定义了(改 equip_kind 不行,索引不跟着变)。
	Editable(SocketRowOrNull(kWeaponKind, kWeaponAttackSocket)).set_socket(5);

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_EQ(1u, CountOf(kAttackGem));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), bare));
}

// 策划删掉了宝石表里的这一行(物品行还在):它不再是宝石、也不再生效,但嵌着的那颗照样取得下来,
// 退回的就是记录里的那个 config。这里用一个从来不是宝石的夹具物品充当「宝石行被删」的 config。
TEST_F(PlayerEquipTest, RemoveGemStillWorksAfterTheGemRowIsGone)
{
	const ItemComp bare = MakeRolledInstance(kGuidA, kWeaponL1);
	ItemComp weapon = bare;
	AddGem(weapon, kWeaponAttackSocket, kStackable);
	Inventory().RestoreItems({{weapon, 5}});
	ASSERT_FALSE(PlayerEquipSystem::IsGem(kStackable));

	ASSERT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));

	EXPECT_EQ(1u, CountOf(kStackable));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Inventory(), bare));
}

// 连物品行都没了:退不回去,宝石留在装备上等表修好。在纯校验段就拒(不等到发放那一步失败),
// 这样背包满的玩家不会先收到一句「背包已满」、腾出格子之后才发现根本退不了。
TEST_F(PlayerEquipTest, RemoveGemKeepsTheGemWhenItsItemRowIsGone)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kUnknownConfig);
	Inventory().RestoreItems({{weapon, 5}});
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);
}

// 这种宝石被 GM 封禁了发放:取下走的是正式发放入口,会被封禁闸拒掉。对玩家是「操作失败」,
// 宝石留在装备上;解封之后照常取得下来。
TEST_F(PlayerEquipTest, RemoveGemKeepsTheGemWhileItsGrantIsBlocked)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	ScopedGlobalItemBlock blocked(kAttackGem);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);

	blocked.Unblock();
	EXPECT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(1u, CountOf(kAttackGem));
}

// 宝石的物品行此刻被配成了装备:退回去的会是一件新铸的装备实例(表改回来之后它就是一颗永远并不了堆的
// 「宝石」),所以拒绝,宝石留在装备上等表修好。取下入口刻意不要求「仍是宝石」,这一种情形要单独挡。
TEST_F(PlayerEquipTest, RemoveGemFailsClosedWhenTheGemItemBecameEquipment)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	auto &gemItem = Editable(ItemRowOrNull(kAttackGem));
	gemItem.set_equip_kind(kWeaponKind);
	gemItem.set_max_stack_size(1);
	const ForgeState before = CaptureForge();

	EXPECT_EQ(kEquipInternalError, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);
}

TEST_F(PlayerEquipTest, RemoveGemRejectsTargetsThatAreMissingOrNotEquipment)
{
	Inventory().RestoreItems({{MakeInstance(kGuidA, kPlainNonStack), 0}});
	ItemComp stored = MakeRolledInstance(kGuidC, kWeaponL1);
	AddGem(stored, kWeaponAttackSocket, kAttackGem);
	Warehouse().RestoreItems({{stored, 0}});
	const ForgeState before = CaptureForge();
	const BagState warehouseBefore = Capture(Warehouse());

	EXPECT_EQ(kEquipNotEquipment, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::RemoveGem(player, kGuidMissing, kWeaponAttackSocket));
	EXPECT_EQ(kEquipItemNotFound, PlayerEquipSystem::RemoveGem(player, kGuidC, kWeaponAttackSocket))
		<< "equipment in the warehouse is out of reach";
	ExpectForgeUnchanged(before);
	EXPECT_EQ(warehouseBefore, Capture(Warehouse()));
}

TEST_F(PlayerEquipTest, RemoveGemIsRefusedWhileFrozenOrInBattle)
{
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	Inventory().RestoreItems({{weapon, 5}});
	const ForgeState before = CaptureForge();

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	tlsEcs.actorRegistry.emplace<InBattleComp>(player).set_battle_id(4242);
	EXPECT_EQ(kEquipFrozen, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player);
	EXPECT_EQ(kEquipInBattle, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
	ExpectForgeUnchanged(before);

	tlsEcs.actorRegistry.remove<InBattleComp>(player);
	EXPECT_EQ(kSuccess, PlayerEquipSystem::RemoveGem(player, kGuidA, kWeaponAttackSocket));
}

// ── 二期:加成(CollectBonus)────────────────────────────────────────────────
//
// 都只穿一把 1 级武器:它的基础属性只有「伤害」一条(物伤、法伤各加一份),期望值逐项手算得出来。
// 强化与宝石的脏数据(等级超上限、宝石行 / 孔位定义不在表里、同孔多条)在读路径上一律静默跳过。

// 强化按万分比放大基础属性,用的是「这一级」那一行的总加成。
TEST_F(PlayerEquipTest, CollectBonusScalesBaseAttributesByTheEnhanceLevel)
{
	ASSERT_EQ(1, ItemRowOrNull(kWeaponL1)->base_attr_size()) << "the weapon carries only damage; " << kExportFirst;
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	const uint64_t enhanced = EnhancedValue(damage, 2);
	ASSERT_GT(enhanced, EnhancedValue(damage, 1))
		<< "the fixture needs +2 to be visibly stronger than +1, or a wrong row would go unnoticed";
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeForgedInstance(kGuidA, kWeaponL1, 2), kWeaponSlot));

	ExpectSameBonus(WeaponBonus(enhanced), Collect());
}

// 强化只放大基础属性:随机属性与宝石按原值计。
TEST_F(PlayerEquipTest, CollectBonusDoesNotScaleAffixesOrGems)
{
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	const uint32_t affixValue = CapOf(kAttrDamage, kLevel1);
	const uint64_t gemValue = GemValueOf(kAttackGem);
	ASSERT_GT(EnhancedValue(damage, 2), damage);
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1, 2);
	AddAffix(weapon, kAttrDamage, kBlue, affixValue, 0);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(EnhancedValue(damage, 2) + affixValue + gemValue), Collect());
}

// 宝石计入加成,走与随机属性同一套效果:攻击宝石 = 伤害(物伤法伤各加),防御宝石 = 防御。
TEST_F(PlayerEquipTest, CollectBonusCountsInlaidGems)
{
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem); // 故意乱序存
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	const equiprules::EquipBonus expected =
		WeaponBonus(damage + GemValueOf(kAttackGem), GemValueOf(kDefenseGem));

	ExpectSameBonus(expected, Collect());

	// 纯读:再算一遍结果一样,实例也没被改写。
	ExpectSameBonus(expected, Collect());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));
}

// 宝石表里没有这一行(策划删了,或记录本来就是脏的):这颗不计。
TEST_F(PlayerEquipTest, CollectBonusIgnoresGemsWithoutAGemRow)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kStackable);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage)), Collect());
}

// 孔位定义没了:原来计入的那颗不再计入;存档里的记录不动,定义补回来就恢复。
TEST_F(PlayerEquipTest, CollectBonusIgnoresGemsOnSocketsThePartNoLongerDefines)
{
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	const uint64_t gemValue = GemValueOf(kAttackGem);
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ExpectSameBonus(WeaponBonus(damage + gemValue), Collect());

	// 「删掉」武器的 0 号孔(做法见 RemoveGemStillWorksAfterTheSocketDefinitionIsGone)。
	const auto *socketRow = SocketRowOrNull(kWeaponKind, kWeaponAttackSocket);
	Editable(socketRow).set_socket(5);
	ExpectSameBonus(WeaponBonus(damage), Collect());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(Equipment(), weapon));

	Editable(socketRow).set_socket(kWeaponAttackSocket);
	ExpectSameBonus(WeaponBonus(damage + gemValue), Collect());
}

// 同一个孔上有两条记录(脏数据):只认第一条。
TEST_F(PlayerEquipTest, CollectBonusCountsOnlyTheFirstRecordOfASocket)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	AddGem(weapon, kWeaponAttackSocket, kDefenseGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage) + GemValueOf(kAttackGem)), Collect());
}

// 第一条不合格时不回退到同孔的后一条:否则「占着孔的是 A、算数的却是 B」,取下时退回 A、B 被无声清掉。
TEST_F(PlayerEquipTest, CollectBonusDoesNotFallBackToALaterRecordOfTheSameSocket)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kStackable);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage)), Collect());
}

// 不复核宝石类型与孔位类型:类型是嵌入那一刻的规则,策划事后改孔位类型不让已嵌的宝石失效。
TEST_F(PlayerEquipTest, CollectBonusDoesNotRecheckTheGemTypeAgainstTheSocket)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kDefenseGem); // 防御宝石躺在攻击孔里
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage), GemValueOf(kDefenseGem)), Collect());
}

// 只看实例上前 kMaxGemSockets 条记录(遍历长度不受存档里的条数摆布):排在后面的那条不计入加成。
// 但它是该孔的原始记录,所以孔仍算占着 —— tooltip 上是「已镶但未生效」,可以取下。
TEST_F(PlayerEquipTest, GemRecordsBeyondTheLimitOccupyTheirSocketButDoNotCount)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	for (uint32_t i = 0; i < equiprules::kMaxGemSockets; ++i)
	{
		AddGem(weapon, 100 + i, kAttackGem); // 孔位定义之外的记录,把遍历名额占满
	}
	AddGem(weapon, kWeaponAttackSocket, kAttackGem); // 第 9 条:孔是对的、宝石也是对的
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage)), Collect());

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);
	ASSERT_EQ(2, info.gem_sockets_size());
	EXPECT_EQ(kAttackGem, info.gem_sockets(0).gem_config_id());
	EXPECT_FALSE(info.gem_sockets(0).has_attr());
}

// 宝石的属性行不被规则层接受(表被填坏):不计入加成,tooltip 上这个孔也不显示属性 ——
// 与一期「不计入加成的行不上 tooltip」是同一条纪律。
TEST_F(PlayerEquipTest, GemsWithARejectedAttributeAreNeitherCountedNorShown)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));
	ASSERT_EQ(GemValueOf(kDefenseGem),
			  equiprules::DerivedFlatOf(Collect(), equiprules::DerivedStat::kDefense))
		<< "while the attribute row is valid the gem does count";

	Editable(AttrRowOrNull(kAttrDefense)).set_effect(99);

	ExpectSameBonus(WeaponBonus(BaseValueOf(kWeaponL1, kAttrDamage)), Collect());
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);
	ASSERT_EQ(2, info.gem_sockets_size());
	EXPECT_EQ(kDefenseGem, info.gem_sockets(1).gem_config_id()) << "the socket is still occupied";
	EXPECT_FALSE(info.gem_sockets(1).has_attr()) << "but it shows no attribute";
}

// 策划下调强化上限:立即对存量装备生效(加成与 tooltip 一起按新上限算),存档里的等级不洗。
TEST_F(PlayerEquipTest, LoweringTheEnhanceCapClampsBonusAndTooltipWithoutTouchingTheInstance)
{
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	const uint64_t atThree = EnhancedValue(damage, 3);
	const uint64_t atTwo = EnhancedValue(damage, 2);
	ASSERT_GT(atThree, atTwo) << "the fixture needs +3 to be visibly stronger than +2";
	ASSERT_EQ(kSuccess, Equipment().PutInstance(MakeForgedInstance(kGuidA, kWeaponL1, 3), kWeaponSlot));
	ExpectSameBonus(WeaponBonus(atThree), Collect());

	TruncateEnhanceTable(2); // 上限下调到 +2

	ExpectSameBonus(WeaponBonus(atTwo), Collect());
	const ItemComp *stored = Equipment().GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(*stored, info);
	EXPECT_EQ(2u, info.enhance_level()) << "the tooltip shows the clamped level";
	EXPECT_EQ(2u, info.enhance_max_level());
	EXPECT_EQ(EnhanceBonusOf(2), info.enhance_bonus_permyriad());
	EXPECT_FALSE(info.has_enhance_next()) << "at the new cap there is nothing to preview";
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(atTwo, info.base_attrs(0).value());
	EXPECT_EQ(3u, stored->equip().enhance_level()) << "the saved level is never rewritten";
}

// ── 二期:tooltip(FillItemDisplay)──────────────────────────────────────────

// 基础属性行的数值与计入加成的数值是同一个数。鞋子有两条基础属性(防御、速度),各自放大。
TEST_F(PlayerEquipTest, TooltipBaseValuesMatchTheCollectedBonus)
{
	const ItemComp shoes = MakeForgedInstance(kGuidA, kShoesL1, 2);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(shoes));
	const equiprules::EquipBonus bonus = Collect();

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(shoes, info);
	ASSERT_EQ(2, info.base_attrs_size()) << "shoes carry defense and speed; " << kExportFirst;
	for (const auto &line : info.base_attrs())
	{
		const auto *attr = AttrRowOrNull(line.attr_id());
		ASSERT_NE(nullptr, attr);
		ASSERT_EQ(equiprules::ToRaw(equiprules::EffectKind::kDerivedFlat), attr->effect());
		EXPECT_EQ(equiprules::DerivedFlatOf(bonus, equiprules::ToDerivedStat(attr->effect_param())), line.value())
			<< "attr " << line.attr_id();
		EXPECT_EQ(EnhancedValue(BaseValueOf(kShoesL1, line.attr_id()), 2), line.value())
			<< "attr " << line.attr_id();
	}
}

// 基础属性行另带「其中强化加了多少」:enhance_bonus = value − 表值,已含在 value 里。
// 随机属性行不吃强化,不带这个增量。
TEST_F(PlayerEquipTest, TooltipBaseLinesCarryTheEnhanceIncrement)
{
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	const uint64_t enhanced = EnhancedValue(damage, 2);
	ASSERT_GT(enhanced, damage) << "the fixture needs a +2 bonus that is visible on the weapon's damage";
	const uint32_t affixValue = MinOf(kAttrDamage, kLevel1);
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1, 2);
	AddAffix(weapon, kAttrDamage, kBlue, affixValue, 0);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);

	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(enhanced, info.base_attrs(0).value());
	EXPECT_EQ(enhanced - damage, info.base_attrs(0).enhance_bonus());
	ASSERT_EQ(1, info.affixes_size());
	EXPECT_EQ(uint64_t{affixValue}, info.affixes(0).value());
	EXPECT_EQ(0u, info.affixes(0).enhance_bonus());
	EXPECT_EQ(0u, info.affixes(0).enhance_next_value());
}

TEST_F(PlayerEquipTest, TooltipReportsTheEnhanceLevelTheCapAndTheCurrentBonus)
{
	ASSERT_GT(EnhanceBonusOf(2), 0u) << kExportFirst;
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1, 2), info);

	EXPECT_EQ(2u, info.enhance_level());
	EXPECT_EQ(PlayerEquipSystem::MaxEnhanceLevel(), info.enhance_max_level());
	EXPECT_EQ(EnhanceBonusOf(2), info.enhance_bonus_permyriad());
}

// 下一级预览:取的是「当前等级 + 1」那一行;材料名与图标来自材料的物品行(客户端零配表)。
// 每条基础属性行另带强化到下一级之后的数值。
TEST_F(PlayerEquipTest, TooltipPreviewsTheNextEnhanceLevel)
{
	const auto *nextRow = EnhanceRowOrNull(2);
	Editable(nextRow).set_fail_level_loss(3); // 表里默认 0,改成非 0 才看得出这个字段真的下发了
	const auto *stone = ItemRowOrNull(kEnhanceStone);
	// 发货表里强化石没配图标:不补一个的话,下面那条图标断言两边都是空串,删掉实现里那一行也照绿。
	Editable(stone).set_icon_key("UI/test/enhance_stone");
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1, 1), info);

	ASSERT_TRUE(info.has_enhance_next());
	const auto &next = info.enhance_next();
	EXPECT_EQ(2u, next.next_level());
	EXPECT_EQ(nextRow->success_rate(), next.success_rate());
	EXPECT_EQ(nextRow->cost_gold(), next.cost_gold());
	EXPECT_EQ(kEnhanceStone, next.material_config_id());
	EXPECT_EQ(stone->name(), next.material_name());
	EXPECT_FALSE(next.material_name().empty());
	EXPECT_EQ(stone->icon_key(), next.material_icon_key());
	EXPECT_FALSE(next.material_icon_key().empty());
	EXPECT_EQ(nextRow->material_count(), next.material_count());
	EXPECT_EQ(nextRow->bonus_permyriad(), next.bonus_permyriad());
	EXPECT_EQ(3u, next.fail_level_loss());

	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(EnhancedValue(damage, 1), info.base_attrs(0).value());
	EXPECT_EQ(EnhancedValue(damage, 2), info.base_attrs(0).enhance_next_value());
}

// 满级:没有下一级预览,基础属性行也不带预览值;当前等级的加成照常下发。
TEST_F(PlayerEquipTest, TooltipHasNoPreviewAtTheMaximumLevel)
{
	const uint32_t maxLevel = PlayerEquipSystem::MaxEnhanceLevel();
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1, maxLevel), info);

	EXPECT_EQ(maxLevel, info.enhance_level());
	EXPECT_EQ(maxLevel, info.enhance_max_level());
	EXPECT_EQ(EnhanceBonusOf(maxLevel), info.enhance_bonus_permyriad());
	EXPECT_FALSE(info.has_enhance_next());
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(EnhancedValue(damage, maxLevel), info.base_attrs(0).value());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_next_value());
}

// 下一级的成功率是 0:强化入口会拒绝,所以也不下发预览(判据与强化入口相同)。上限照常下发。
TEST_F(PlayerEquipTest, TooltipHasNoPreviewWhenTheNextLevelCanNeverSucceed)
{
	Editable(EnhanceRowOrNull(1)).set_success_rate(0);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1), info);

	EXPECT_FALSE(info.has_enhance_next());
	EXPECT_EQ(PlayerEquipSystem::MaxEnhanceLevel(), info.enhance_max_level());
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_next_value());
}

// 强化未开放(强化表为空):上限下发 0、没有预览;存档里的等级按 0 级显示,基础属性是表值。
TEST_F(PlayerEquipTest, TooltipShowsEnhancementAsClosedWhenTheTableIsEmpty)
{
	TruncateEnhanceTable(0);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1, 3), info);

	EXPECT_EQ(0u, info.enhance_max_level());
	EXPECT_EQ(0u, info.enhance_level());
	EXPECT_EQ(0u, info.enhance_bonus_permyriad());
	EXPECT_FALSE(info.has_enhance_next());
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(BaseValueOf(kWeaponL1, kAttrDamage), info.base_attrs(0).value());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_bonus());
}

// 这一级不要材料:预览里材料的几个字段都留空 / 0(客户端不会画出「某材料 ×0」),金币照常下发。
TEST_F(PlayerEquipTest, TooltipPreviewOmitsTheMaterialWhenNoneIsNeeded)
{
	auto &row = Editable(EnhanceRowOrNull(1));
	row.set_material_id(0);
	row.set_material_count(0);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1), info);

	ASSERT_TRUE(info.has_enhance_next());
	EXPECT_EQ(0u, info.enhance_next().material_config_id());
	EXPECT_EQ(0u, info.enhance_next().material_count());
	EXPECT_TRUE(info.enhance_next().material_name().empty());
	EXPECT_TRUE(info.enhance_next().material_icon_key().empty());
	EXPECT_EQ(row.cost_gold(), info.enhance_next().cost_gold());
}

// 没掷过属性的实例强化不了、也嵌不了:不带预览、不下发孔位;基础属性照表显示,上限照常下发。
TEST_F(PlayerEquipTest, TooltipForUninitialisedEquipmentHasNoPreviewAndNoSockets)
{
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidA, kWeaponL1), info);

	EXPECT_EQ(0u, info.enhance_level());
	EXPECT_EQ(PlayerEquipSystem::MaxEnhanceLevel(), info.enhance_max_level());
	EXPECT_FALSE(info.has_enhance_next());
	EXPECT_EQ(0, info.gem_sockets_size());
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(BaseValueOf(kWeaponL1, kAttrDamage), info.base_attrs(0).value());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_bonus());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_next_value());
}

// 0 级、没镶宝石的装备与一期逐位一致:加成只有表里的基础属性;tooltip 上二期只给它添了
// 「可以强化到几级 / 下一级什么样 / 有哪些孔」这几项说明,把它们拿掉之后剩下的字节与一期下发的相同。
TEST_F(PlayerEquipTest, UnforgedEquipmentKeepsItsPhaseOneBonusAndTooltip)
{
	const ItemTable *row = ItemRowOrNull(kWeaponL1);
	ASSERT_EQ(1, row->base_attr_size()) << "the weapon carries only damage; " << kExportFirst;
	const auto *damageAttr = AttrRowOrNull(kAttrDamage);
	const uint64_t damage = BaseValueOf(kWeaponL1, kAttrDamage);
	ItemComp weapon = MakeInstance(kGuidA, kWeaponL1);
	weapon.mutable_equip(); // 已初始化:0 级、0 条随机属性、0 颗宝石
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	ExpectSameBonus(WeaponBonus(damage), Collect());

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);
	EXPECT_EQ(0u, info.enhance_level());
	EXPECT_EQ(0u, info.enhance_bonus_permyriad());
	ASSERT_EQ(1, info.base_attrs_size());
	EXPECT_EQ(0u, info.base_attrs(0).enhance_bonus());

	info.clear_enhance_max_level();
	info.clear_enhance_next();
	info.clear_gem_sockets();
	info.mutable_base_attrs(0)->clear_enhance_next_value();
	BagItemInfo phaseOne;
	phaseOne.set_name(row->name());
	phaseOne.set_description(row->description());
	phaseOne.set_icon_key(row->icon_key());
	phaseOne.set_equip_level(row->equip_level());
	phaseOne.set_equip_class(row->equip_class());
	auto *line = phaseOne.add_base_attrs();
	line->set_attr_id(kAttrDamage);
	line->set_name(damageAttr->name());
	line->set_value(damage);
	line->set_percent(damageAttr->percent() != 0);
	EXPECT_EQ(phaseOne.SerializeAsString(), info.SerializeAsString());
}

// 孔位:该部位的全部孔逐个下发(含空孔),孔位号升序,带接纳的宝石类型与显示名。
// 没有任何孔位定义的部位一个孔都不下发。
TEST_F(PlayerEquipTest, TooltipListsEverySocketOfThePartIncludingEmptyOnes)
{
	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidA, kWeaponL1), info);

	ASSERT_EQ(2, info.gem_sockets_size());
	for (const uint32_t socket : {kWeaponAttackSocket, kWeaponAnySocket})
	{
		const auto &shown = info.gem_sockets(static_cast<int>(socket));
		const auto *socketRow = SocketRowOrNull(kWeaponKind, socket);
		EXPECT_EQ(socket, shown.socket());
		EXPECT_EQ(socketRow->gem_type(), shown.gem_type()) << "socket " << socket;
		EXPECT_EQ(socketRow->name(), shown.name()) << "socket " << socket;
		EXPECT_EQ(0u, shown.gem_config_id()) << "socket " << socket << " is empty";
		EXPECT_TRUE(shown.gem_name().empty()) << "socket " << socket;
		EXPECT_TRUE(shown.gem_icon_key().empty()) << "socket " << socket;
		EXPECT_FALSE(shown.has_attr()) << "socket " << socket;
	}
	EXPECT_EQ(kGemTypeAttack, info.gem_sockets(0).gem_type());
	EXPECT_EQ(kGemTypeAny, info.gem_sockets(1).gem_type());

	BagItemInfo socketless;
	PlayerEquipSystem::FillItemDisplay(MakeForgedInstance(kGuidB, kOldEquipA), socketless);
	EXPECT_EQ(0, socketless.gem_sockets_size());
}

// 已镶且生效的孔:带宝石的 config / 名称 / 图标,以及它此刻提供的属性。
// 宝石没有颜色档、没有「/上限」、一颗只有一条属性,所以 tier / cap / seq 恒为 0。
TEST_F(PlayerEquipTest, TooltipShowsAnInlaidGemWithItsAttribute)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/2);
	AddGem(weapon, kWeaponAnySocket, kDefenseGem); // 故意乱序存:下发顺序由孔位号决定
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	// 发货表里宝石没配图标:各补一个不同的,否则下面的图标断言两边都是空串,什么都没验。
	Editable(ItemRowOrNull(kAttackGem)).set_icon_key("UI/test/attack_gem");
	Editable(ItemRowOrNull(kDefenseGem)).set_icon_key("UI/test/defense_gem");

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);

	ASSERT_EQ(2, info.gem_sockets_size());
	struct ExpectedSocket
	{
		uint32_t socket;
		uint32_t gemConfigId;
		uint32_t attrId;
	};
	const std::vector<ExpectedSocket> expectedSockets{
		{kWeaponAttackSocket, kAttackGem, kAttrDamage},
		{kWeaponAnySocket, kDefenseGem, kAttrDefense},
	};
	for (int i = 0; i < info.gem_sockets_size(); ++i)
	{
		const auto &shown = info.gem_sockets(i);
		const auto &want = expectedSockets[static_cast<std::size_t>(i)];
		const auto *gemItem = ItemRowOrNull(want.gemConfigId);
		const auto *attr = AttrRowOrNull(want.attrId);
		EXPECT_EQ(want.socket, shown.socket()) << "socket line " << i;
		EXPECT_EQ(want.gemConfigId, shown.gem_config_id()) << "socket line " << i;
		EXPECT_EQ(gemItem->name(), shown.gem_name()) << "socket line " << i;
		EXPECT_FALSE(shown.gem_name().empty()) << "socket line " << i;
		EXPECT_EQ(gemItem->icon_key(), shown.gem_icon_key()) << "socket line " << i;
		EXPECT_FALSE(shown.gem_icon_key().empty()) << "socket line " << i;
		ASSERT_TRUE(shown.has_attr()) << "socket line " << i;
		EXPECT_EQ(want.attrId, shown.attr().attr_id()) << "socket line " << i;
		EXPECT_EQ(attr->name(), shown.attr().name()) << "socket line " << i;
		// 宝石不吃强化:这件是 +2,显示的仍是宝石表里的原值。
		EXPECT_EQ(GemValueOf(want.gemConfigId), shown.attr().value()) << "socket line " << i;
		EXPECT_EQ(attr->percent() != 0, shown.attr().percent()) << "socket line " << i;
		EXPECT_EQ(0u, shown.attr().tier()) << "socket line " << i;
		EXPECT_EQ(0u, shown.attr().cap()) << "socket line " << i;
		EXPECT_EQ(0u, shown.attr().seq()) << "socket line " << i;
		EXPECT_EQ(0u, shown.attr().enhance_bonus()) << "socket line " << i;
	}
}

// 「占用」与「生效」是两个口径:孔被一条不生效的记录占着 → 下发它的 config(客户端据此知道这个孔
// 不空、可以取下),但不带属性。连物品行都查不到的记录,名称与图标留空,仍然算占着。
TEST_F(PlayerEquipTest, TooltipShowsAnOccupiedButIneffectiveSocketWithoutAttr)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kStackable);   // 物品行在,但不是宝石
	AddGem(weapon, kWeaponAnySocket, kUnknownConfig);  // 连物品行都没有
	// 夹具物品 10 在发货表里没有名字:补一个,否则「不生效的占用者也下发名称」那条断言是 "" == ""。
	Editable(ItemRowOrNull(kStackable)).set_name("filler");

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);

	ASSERT_EQ(2, info.gem_sockets_size());
	EXPECT_EQ(kStackable, info.gem_sockets(0).gem_config_id());
	EXPECT_EQ(ItemRowOrNull(kStackable)->name(), info.gem_sockets(0).gem_name());
	EXPECT_FALSE(info.gem_sockets(0).gem_name().empty());
	EXPECT_FALSE(info.gem_sockets(0).has_attr());
	EXPECT_EQ(kUnknownConfig, info.gem_sockets(1).gem_config_id());
	EXPECT_TRUE(info.gem_sockets(1).gem_name().empty());
	EXPECT_TRUE(info.gem_sockets(1).gem_icon_key().empty());
	EXPECT_FALSE(info.gem_sockets(1).has_attr());
}

// 同孔两条记录(脏数据):显示的占用者是第一条 —— 与取下时退回的那一颗、与计入加成的那一颗都一致。
TEST_F(PlayerEquipTest, TooltipShowsTheFirstRecordOfADuplicatedSocket)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	AddGem(weapon, kWeaponAttackSocket, kDefenseGem);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);

	ASSERT_EQ(2, info.gem_sockets_size());
	EXPECT_EQ(kAttackGem, info.gem_sockets(0).gem_config_id());
	ASSERT_TRUE(info.gem_sockets(0).has_attr());
	EXPECT_EQ(kAttrDamage, info.gem_sockets(0).attr().attr_id());
	EXPECT_EQ(0u, info.gem_sockets(1).gem_config_id()) << "the other socket stays empty";
}

// 孔位定义之外的孔上的记录不下发(这件装备上画不出那个孔);定义内的孔不受影响。
TEST_F(PlayerEquipTest, TooltipDoesNotListRecordsOnUndefinedSockets)
{
	ItemComp weapon = MakeForgedInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kUndefinedSocket, kAttackGem);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(weapon, info);

	ASSERT_EQ(2, info.gem_sockets_size());
	EXPECT_EQ(kWeaponAttackSocket, info.gem_sockets(0).socket());
	EXPECT_EQ(0u, info.gem_sockets(0).gem_config_id());
	EXPECT_EQ(kWeaponAnySocket, info.gem_sockets(1).socket());
	EXPECT_EQ(0u, info.gem_sockets(1).gem_config_id());
}

// 宝石物品自己的 tooltip:宝石类型(客户端靠它判断能嵌进哪些孔)与它提供的属性。
// 宝石不是装备,装备才有的字段一个都不写。
TEST_F(PlayerEquipTest, FillItemDisplayDescribesGemItems)
{
	const auto *attr = AttrRowOrNull(kAttrDamage);

	BagItemInfo info;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidA, kAttackGem), info);

	EXPECT_EQ(ItemRowOrNull(kAttackGem)->name(), info.name());
	EXPECT_EQ(kGemTypeAttack, info.gem_type());
	ASSERT_TRUE(info.has_gem_attr());
	EXPECT_EQ(kAttrDamage, info.gem_attr().attr_id());
	EXPECT_EQ(attr->name(), info.gem_attr().name());
	EXPECT_EQ(GemValueOf(kAttackGem), info.gem_attr().value());
	EXPECT_EQ(attr->percent() != 0, info.gem_attr().percent());
	EXPECT_EQ(0u, info.gem_attr().tier());
	EXPECT_EQ(0u, info.gem_attr().cap());
	EXPECT_EQ(0u, info.gem_attr().seq());

	EXPECT_EQ(0u, info.enhance_max_level());
	EXPECT_FALSE(info.has_enhance_next());
	EXPECT_EQ(0, info.gem_sockets_size());
	EXPECT_EQ(0, info.base_attrs_size());
	EXPECT_EQ(0, info.affixes_size());
}

// 宝石的属性行不可用(查不到,或 effect 不被规则层接受):嵌上也不计入加成,所以宝石物品上
// 只下发类型、不下发属性。
TEST_F(PlayerEquipTest, FillItemDisplayShowsOnlyTheTypeOfGemsWithAnUnusableAttribute)
{
	Editable(GemRowOrNull(kAttackGem)).set_attr_id(kUnknownAttr);
	BagItemInfo missingAttr;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidA, kAttackGem), missingAttr);
	EXPECT_EQ(kGemTypeAttack, missingAttr.gem_type());
	EXPECT_FALSE(missingAttr.has_gem_attr());

	Editable(AttrRowOrNull(kAttrDefense)).set_effect(99);
	BagItemInfo rejectedAttr;
	PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidB, kDefenseGem), rejectedAttr);
	EXPECT_EQ(kGemTypeDefense, rejectedAttr.gem_type());
	EXPECT_FALSE(rejectedAttr.has_gem_attr());
}

// 既不是装备也不是宝石的物品与一期逐位一致:只有名称 / 描述 / 图标,二期的字段一个都不写。
TEST_F(PlayerEquipTest, FillItemDisplayWritesNothingNewForItemsThatAreNeitherEquipmentNorGems)
{
	for (const uint32_t configId : {kEnhanceStone, kStackable, kPlainNonStack})
	{
		const ItemTable *row = ItemRowOrNull(configId);
		ASSERT_NE(nullptr, row) << "config " << configId;
		BagItemInfo info;
		PlayerEquipSystem::FillItemDisplay(MakeInstance(kGuidA, configId), info);

		BagItemInfo phaseOne;
		phaseOne.set_name(row->name());
		phaseOne.set_description(row->description());
		phaseOne.set_icon_key(row->icon_key());
		EXPECT_EQ(phaseOne.SerializeAsString(), info.SerializeAsString()) << "config " << configId;
	}
}

// ── handler(经生成的 CallMethod 分发)───────────────────────────────────────

// 三个新方法只许追加在 service 末尾:CallMethod 按 method index 分发,插在中间会让既有的
// GetBag / SortBag 错位。二期的强化 / 镶嵌 / 取下同理,接在一期三个方法之后(index 5 / 6 / 7)。
TEST_F(PlayerEquipTest, NewBagMethodsAreAppendedAfterTheExistingOnes)
{
	const auto *getBag = BagMethod("GetBag");
	const auto *sortBag = BagMethod("SortBag");
	const auto *equip = BagMethod("EquipItem");
	const auto *unequip = BagMethod("UnequipItem");
	const auto *grant = BagMethod("GmGrantItem");
	const auto *enhance = BagMethod("EnhanceEquip");
	const auto *inlay = BagMethod("InlayGem");
	const auto *removeGem = BagMethod("RemoveGem");
	ASSERT_NE(nullptr, getBag);
	ASSERT_NE(nullptr, sortBag);
	ASSERT_NE(nullptr, equip) << kProtoGenFirst;
	ASSERT_NE(nullptr, unequip) << kProtoGenFirst;
	ASSERT_NE(nullptr, grant) << kProtoGenFirst;
	ASSERT_NE(nullptr, enhance) << kProtoGenFirst;
	ASSERT_NE(nullptr, inlay) << kProtoGenFirst;
	ASSERT_NE(nullptr, removeGem) << kProtoGenFirst;
	EXPECT_EQ(0, getBag->index());
	EXPECT_EQ(1, sortBag->index());
	EXPECT_EQ(2, equip->index());
	EXPECT_EQ(3, unequip->index());
	EXPECT_EQ(4, grant->index());
	EXPECT_EQ(5, enhance->index());
	EXPECT_EQ(6, inlay->index());
	EXPECT_EQ(7, removeGem->index());
}

TEST_F(PlayerEquipTest, EquipHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("EquipItem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	Inventory().RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 1}});

	// 参数粗检:item_id = 0。
	EquipItemRequest request;
	EquipItemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	// 业务拒绝:原样带回编排层的码,不带半份快照。
	request.set_item_id(kGuidB);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipNotEquipment, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

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

	request.set_item_id(kGuidA);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_TRUE(ContainsItem(response.bag().items(), kGuidA));
	EXPECT_EQ(0, response.equipment().items_size());
	EXPECT_GT(response.equipment().layout().equip_slots_size(), 0) << "empty slots are still listed";
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

// ── 二期的三个 handler:强化 / 镶嵌 / 取下 ────────────────────────────────────
//
// 与穿脱同一个三段式:参数粗检 → 业务拒绝(只回 tip、不带半份快照)→ 成功(两个包的全量,已经是
// 操作之后的样子)。另外钉一件事:回给客户端的快照里没有数量为 0 的条目(被扣光的堆已回收)。

TEST_F(PlayerEquipTest, EnhanceHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("EnhanceEquip");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	Inventory().RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 1}});
	ASSERT_NE(nullptr, StockForEnhance(1, /*spareGold=*/5)); // 强化石恰好够:扣完那一堆是空的
	UseDice({0});

	// 参数粗检:item_id = 0。
	EnhanceEquipRequest request;
	EnhanceEquipResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	// 业务拒绝:原样带回编排层的码,不带半份快照,也不带结果字段。
	request.set_item_id(kGuidB);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipNotEquipment, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());
	EXPECT_FALSE(response.success());
	EXPECT_EQ(0u, response.enhance_level());

	// 成功:上一次的失败码没有漏进来;两个包的全量都在,带成败与新等级。
	request.set_item_id(kGuidA);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_TRUE(response.success());
	EXPECT_EQ(1u, response.enhance_level());
	EXPECT_EQ(uint32_t{kInventory}, response.bag().layout().bag_type());
	EXPECT_EQ(uint32_t{kEquipment}, response.equipment().layout().bag_type());
	// 快照已经是强化之后的样子:等级、金币、材料。
	const BagItemInfo *shown = FindItem(response.bag().items(), kGuidA);
	ASSERT_NE(nullptr, shown);
	EXPECT_EQ(1u, shown->enhance_level());
	ASSERT_GT(response.bag().currency().values_size(), static_cast<int>(kCurrencyGold));
	EXPECT_EQ(5u, response.bag().currency().values(static_cast<int>(kCurrencyGold)));
	EXPECT_EQ(0u, SnapshotCountOf(response.bag().items(), kEnhanceStone));
	ExpectNoEmptyEntries(response.bag().items());
	EXPECT_TRUE(ContainsItem(response.bag().items(), kGuidB));
	EXPECT_EQ(0, response.equipment().items_size());
}

// 掷骰没过不是错误:客户端见到 tip 会保留旧快照,而金币与材料已经扣了 —— 所以这一次照样是
// 不带 tip 的成功响应、带两份新快照,靠 success = false 区分。
TEST_F(PlayerEquipTest, EnhanceHandlerReportsAFailedRollAsASuccessfulResponse)
{
	const auto *method = BagMethod("EnhanceEquip");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	auto &row = Editable(EnhanceRowOrNull(4));
	row.set_success_rate(5000);
	row.set_fail_level_loss(0);
	ASSERT_EQ(kSuccess,
			  Equipment().PutInstance(MakeRolledInstance(kGuidA, kWeaponL1, /*enhanceLevel=*/3), kWeaponSlot));
	ASSERT_NE(nullptr, StockForEnhance(4, /*spareGold=*/5, /*spareMaterial=*/2));
	UseDice({5000});

	EnhanceEquipRequest request;
	request.set_item_id(kGuidA);
	EnhanceEquipResponse response;
	handler.CallMethod(method, player, &request, &response);

	EXPECT_EQ(0u, response.error_message().id()) << "a failed roll is not an error";
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_FALSE(response.success());
	EXPECT_EQ(3u, response.enhance_level());
	// 两份快照照常带回:金币与材料已经是扣过之后的数,穿着的那件还是 +3。
	ASSERT_GT(response.bag().currency().values_size(), static_cast<int>(kCurrencyGold));
	EXPECT_EQ(5u, response.bag().currency().values(static_cast<int>(kCurrencyGold)));
	EXPECT_EQ(2u, SnapshotCountOf(response.bag().items(), kEnhanceStone));
	const BagItemInfo *worn = FindItem(response.equipment().items(), kGuidA);
	ASSERT_NE(nullptr, worn);
	EXPECT_EQ(3u, worn->enhance_level());
}

TEST_F(PlayerEquipTest, InlayGemHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("InlayGem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	Inventory().RestoreItems({{MakeRolledInstance(kGuidA, kWeaponL1), 0}});
	PutStack(kGuidGemA, kAttackGem, 1, kGemPosA); // 只有一颗:嵌掉之后那一堆是空的

	// 参数粗检:item_id = 0;gem_config_id = 0。
	InlayGemRequest request;
	request.set_gem_config_id(kAttackGem);
	InlayGemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	request.set_item_id(kGuidA);
	request.set_gem_config_id(0);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());

	// 业务拒绝:原样带回编排层的码,不带半份快照。
	request.set_gem_config_id(kAttackGem);
	request.set_socket(kUndefinedSocket);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipGemSocketInvalid, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	// 成功。孔位号 0 是合法值,不能被粗检当成「没填」。
	request.set_socket(kWeaponAttackSocket);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_EQ(uint32_t{kInventory}, response.bag().layout().bag_type());
	EXPECT_EQ(uint32_t{kEquipment}, response.equipment().layout().bag_type());
	const BagItemInfo *shown = FindItem(response.bag().items(), kGuidA);
	ASSERT_NE(nullptr, shown);
	ASSERT_EQ(2, shown->gem_sockets_size());
	EXPECT_EQ(kAttackGem, shown->gem_sockets(0).gem_config_id());
	EXPECT_TRUE(shown->gem_sockets(0).has_attr());
	EXPECT_FALSE(ContainsItem(response.bag().items(), kGuidGemA)) << "the emptied gem stack is not listed";
	ExpectNoEmptyEntries(response.bag().items());
}

TEST_F(PlayerEquipTest, RemoveGemHandlerReturnsBothBagsAndDoesNotLeakTips)
{
	const auto *method = BagMethod("RemoveGem");
	ASSERT_NE(nullptr, method) << kProtoGenFirst;
	ItemComp weapon = MakeRolledInstance(kGuidA, kWeaponL1);
	AddGem(weapon, kWeaponAttackSocket, kAttackGem);
	ASSERT_EQ(kSuccess, Equipment().PutInstance(weapon, kWeaponSlot));

	// 参数粗检:item_id = 0。
	RemoveGemRequest request;
	RemoveGemResponse response;
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kInvalidParameter, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	// 业务拒绝:那个孔是空的。
	request.set_item_id(kGuidA);
	request.set_socket(kWeaponAnySocket);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(kEquipGemSocketEmpty, response.error_message().id());
	EXPECT_FALSE(response.has_bag());
	EXPECT_FALSE(response.has_equipment());

	// 成功:宝石回到人物背包,穿着的那件上这个孔空了。
	request.set_socket(kWeaponAttackSocket);
	response.Clear();
	handler.CallMethod(method, player, &request, &response);
	EXPECT_EQ(0u, response.error_message().id());
	ASSERT_TRUE(response.has_bag());
	ASSERT_TRUE(response.has_equipment());
	EXPECT_EQ(1u, SnapshotCountOf(response.bag().items(), kAttackGem));
	const BagItemInfo *worn = FindItem(response.equipment().items(), kGuidA);
	ASSERT_NE(nullptr, worn);
	ASSERT_EQ(2, worn->gem_sockets_size());
	EXPECT_EQ(0u, worn->gem_sockets(0).gem_config_id());
	EXPECT_FALSE(worn->gem_sockets(0).has_attr());
}

} // namespace
