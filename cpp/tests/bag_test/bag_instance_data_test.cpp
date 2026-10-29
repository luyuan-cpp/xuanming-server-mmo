// 背包实例层:物品实例携带装备数据(ItemComp.equip)时的"不丢、不重掷"与穿脱搬运原语。
// 契约见 docs/design/equipment-attributes.md §3.1 / §4.1 / §5(不变量 1 / 5 / 7);
// 二期的强化等级与已镶宝石同在 equip 段里,见 docs/design/equipment-enhance-inlay.md
// §3.1 / §5(不变量 1 / 2)。
//
// 这一层**不认识属性**,只认识"整份实例":用例里的 attr_id / tier / value、强化等级、
// 孔位号、宝石 config 都是随手写的数,容器不解释它们,只负责原样带着走。所以这里只用
// 既有的 Item / EquipSlot 夹具行(item 1 / 2 = 不可叠加、部位 1;槽 0 / 1 接部位 1,
// 槽 2 接部位 2),不依赖新装备行,也不依赖强化 / 宝石那三张表。
//
// 夹具自带:SetUp 自己加载两张表、自己装 item 号段,TearDown 卸掉 BagService 的初始化回调
// 并恢复其它用例使用的号段基线 —— 不依赖别的文件的用例先跑或后跑。

#include <gtest/gtest.h>
#include <google/protobuf/unknown_field_set.h>

#include <cstdint>
#include <functional>
#include <map>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "engine/core/type_define/type_define.h"
#include "engine/core/utils/proto/unknown_field_carry.h"

#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_store.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"
#include "services/scene/player/system/bag_marshal.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

// ── 夹具数据(generated/tables/item.json、equipslot.json 的既有行)──────────
constexpr uint32_t kEquipA = 1;        // 不可叠加,部位 1
constexpr uint32_t kEquipB = 2;        // 不可叠加,部位 1
constexpr uint32_t kPlainNonStack = 3; // 不可叠加,不是装备(部位 0)
constexpr uint32_t kStackable = 10;    // 可叠加
constexpr uint32_t kUnknownConfig = 987654;

constexpr uint32_t kKind1 = 1;
constexpr uint32_t kKind2 = 2;
constexpr uint32_t kUnknownKind = 987654;
constexpr uint32_t kSlotKind1First = 0;
constexpr uint32_t kSlotKind1Second = 1;
constexpr uint32_t kSlotKind2 = 2;

// 搬运 / 还原用的预设 guid:远离号段范围,不会与铸出来的号相撞。
constexpr Guid kGuidA = (uint64_t{1} << 60) + 9101;
constexpr Guid kGuidB = (uint64_t{1} << 60) + 9102;
constexpr Guid kGuidC = (uint64_t{1} << 60) + 9103;
constexpr Guid kGuidD = (uint64_t{1} << 60) + 9104;
constexpr Guid kGuidE = (uint64_t{1} << 60) + 9105;

constexpr uint64_t kSegmentLo = 900000;
constexpr uint64_t kSegmentHi = 1000000;
constexpr uint64_t kDynamicBagId = 4242;

constexpr uint32_t kRolledAttrId = 3;
constexpr uint32_t kRolledValueBase = 100;

// 二期的实例数据。kOutOfTableEnhanceLevel 远超任何强化表的上限:等级按表上限夹取是消费侧
// 的事,容器层必须把存档里的原值带着走(「不洗存档」)。
constexpr uint32_t kRolledEnhanceLevel = 7;
constexpr uint32_t kOutOfTableEnhanceLevel = 4000000000u;

// 与 player_mission_system_test.cpp 同一个做法:无 I/O 的假传输 + 直接投一段号。
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

InitItemParam MakeParam(uint32_t configId, uint32_t size = 1)
{
	InitItemParam param;
	param.itemPBComp.set_config_id(configId);
	param.itemPBComp.set_size(size);
	return param;
}

// 一件既有实例(带预设 guid、size = 1、没有 equip 段)。
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

void AddGem(ItemComp &item, uint32_t socket, uint32_t gemConfigId)
{
	auto *gem = item.mutable_equip()->add_gems();
	gem->set_socket(socket);
	gem->set_gem_config_id(gemConfigId);
}

// 一件"已经掷过属性"的装备:两条属性,四个字段都取互不相同的值,丢哪个字段都看得出来。
// 二期起它同时是"强化过、镶过宝石"的:强化等级与两颗宝石也全取非默认、互不相同的值。
// 宝石故意不按孔位号升序写 —— 记录的先后是数据的一部分(消费侧对同一孔位「取第一条」),
// 整份搬运必须连顺序一起保住。
ItemComp MakeRolledInstance(Guid guid, uint32_t configId)
{
	ItemComp item = MakeInstance(guid, configId);
	AddAffix(item, /*attrId=*/3, /*tier=*/1, /*value=*/17, /*seq=*/0);
	AddAffix(item, /*attrId=*/12, /*tier=*/3, /*value=*/4, /*seq=*/1);
	item.mutable_equip()->set_enhance_level(kRolledEnhanceLevel);
	AddGem(item, /*socket=*/1, /*gemConfigId=*/2203);
	AddGem(item, /*socket=*/0, /*gemConfigId=*/2101);
	return item;
}

// 只强化 / 镶嵌过、一条随机属性都没有的装备:新字段不能靠"反正有 affixes"才被带上。
// 形状故意取消费侧不会接受的 —— 等级远超表上限、同一孔位两条记录、孔位号不升序、
// 宝石 config 查不到表。容器层不解释 equip 段:不夹取、不去重、不排序、不校验
// (那些是消费侧现查的事,docs/design/equipment-enhance-inlay.md §4.3)。
ItemComp MakeForgedOnlyInstance(Guid guid, uint32_t configId)
{
	ItemComp item = MakeInstance(guid, configId);
	item.mutable_equip()->set_enhance_level(kOutOfTableEnhanceLevel);
	AddGem(item, /*socket=*/6, /*gemConfigId=*/kUnknownConfig);
	AddGem(item, /*socket=*/2, /*gemConfigId=*/2301);
	AddGem(item, /*socket=*/6, /*gemConfigId=*/2102); // 同一孔位的第二条(脏数据的形状)
	return item;
}

// equip 段逐字段相等,**含 presence**:has_equip() 且 0 条属性 != 没有 equip。
//
// 逐字段比是为了失败时说得出丢的是哪个字段;末尾再整段比一次字节,兜住 EquipInstanceData
// 日后再加字段而这里忘了跟 —— 二期加强化等级 / 宝石之前,这个函数就只看得见 affixes,
// 搬运那批用例对新字段丢失是瞎的。
void ExpectSameEquip(const ItemComp &expected, const ItemComp &actual)
{
	ASSERT_EQ(expected.has_equip(), actual.has_equip()) << "equip presence must survive";
	ASSERT_EQ(expected.equip().affixes_size(), actual.equip().affixes_size());
	for (int i = 0; i < expected.equip().affixes_size(); ++i)
	{
		const auto &want = expected.equip().affixes(i);
		const auto &got = actual.equip().affixes(i);
		EXPECT_EQ(want.attr_id(), got.attr_id()) << "affix " << i;
		EXPECT_EQ(want.tier(), got.tier()) << "affix " << i;
		EXPECT_EQ(want.value(), got.value()) << "affix " << i;
		EXPECT_EQ(want.seq(), got.seq()) << "affix " << i;
	}
	EXPECT_EQ(expected.equip().enhance_level(), actual.equip().enhance_level());
	ASSERT_EQ(expected.equip().gems_size(), actual.equip().gems_size());
	for (int i = 0; i < expected.equip().gems_size(); ++i)
	{
		const auto &want = expected.equip().gems(i);
		const auto &got = actual.equip().gems(i);
		EXPECT_EQ(want.socket(), got.socket()) << "gem " << i;
		EXPECT_EQ(want.gem_config_id(), got.gem_config_id()) << "gem " << i;
	}
	EXPECT_EQ(expected.equip().SerializeAsString(), actual.equip().SerializeAsString())
		<< "the equip section travels as a whole; a field this helper does not name was lost";
}

void ExpectStoredInstance(Bag &bag, const ItemComp &expected)
{
	const ItemComp *stored = bag.GetItemCompByGuid(expected.item_id());
	ASSERT_NE(nullptr, stored) << "guid " << expected.item_id() << " missing from the bag";
	EXPECT_EQ(expected.item_id(), stored->item_id());
	EXPECT_EQ(expected.config_id(), stored->config_id());
	EXPECT_EQ(expected.size(), stored->size());
	ExpectSameEquip(expected, *stored);
}

// 背包两层的可观察状态:guid -> (槽位, 整份实例的序列化字节)。
// "失败时两层都不变"就拿调用前后的这份抓拍比。
using BagState = std::map<Guid, std::pair<uint32_t, std::string>>;

BagState Capture(const Bag &bag)
{
	BagState state;
	bag.ForEachItem([&state, &bag](Guid guid, const ItemComp &item)
					{ state[guid] = {bag.GetItemPosByGuid(guid), item.SerializeAsString()}; });
	return state;
}

void ExpectUnchanged(const BagState &before, const Bag &bag)
{
	EXPECT_EQ(before, Capture(bag)) << "a refused call must leave both layers untouched";
	EXPECT_EQ(before.size(), bag.OccupiedGridCount());
	EXPECT_EQ(before.size(), bag.GridSlotCount());
	EXPECT_TRUE(bag.IsLayerConsistent());
}

template <typename Entries>
const ItemEntry *FindEntry(const Entries &entries, Guid guid)
{
	for (const auto &entry : entries)
	{
		if (entry.item_uuid() == guid)
		{
			return &entry;
		}
	}
	return nullptr;
}

// 存盘记录上的三种形态:两条属性 / 有 equip 但 0 条 / 没有 equip。
template <typename Entries>
void ExpectEntriesCarryInstanceData(const Entries &entries, const ItemComp &twoRows,
									const ItemComp &zeroRows, const ItemComp &plain)
{
	const ItemEntry *rolled = FindEntry(entries, twoRows.item_id());
	const ItemEntry *emptyRolled = FindEntry(entries, zeroRows.item_id());
	const ItemEntry *plainEntry = FindEntry(entries, plain.item_id());
	ASSERT_NE(nullptr, rolled);
	ASSERT_NE(nullptr, emptyRolled);
	ASSERT_NE(nullptr, plainEntry);

	ASSERT_TRUE(rolled->has_equip());
	EXPECT_EQ(twoRows.equip().SerializeAsString(), rolled->equip().SerializeAsString());
	ASSERT_TRUE(emptyRolled->has_equip()) << "zero rows is still 'already rolled'";
	EXPECT_EQ(0, emptyRolled->equip().affixes_size());
	EXPECT_FALSE(plainEntry->has_equip()) << "a plain item must not grow an equip section";
}

class BagInstanceDataTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		EquipSlotTableManager::Instance().Load();
		ASSERT_TRUE(ArmItemSegment(kSegmentLo, kSegmentHi));
		// 进程级状态:先卸一次,别的文件的用例若忘了卸也不会漏进来。
		BagService::SetItemInstanceInitializer({});
		ASSERT_NO_FATAL_FAILURE(RequireFixtureRows());
	}

	void TearDown() override
	{
		BagService::SetItemInstanceInitializer({});
		for (const auto player : players)
		{
			if (tlsEcs.actorRegistry.valid(player))
			{
				tlsEcs.actorRegistry.destroy(player);
			}
		}
		// 恢复其它背包用例使用的无 I/O 号段基线(与 bag_test.cpp 的 main() 同一段范围)。
		EXPECT_TRUE(ArmItemSegment(1, uint64_t{1} << 54));
	}

	// 前置条件:下面每条用例都建立在这几行夹具数据上。表一漂就在这里说清楚,
	// 而不是让后面的断言以莫名其妙的方式变红。先断言行指针非空再解引用。
	static void RequireItemRow(uint32_t configId, bool stackable, uint32_t equipKind)
	{
		const auto *row = ItemTableManager::Instance().FindByIdSilent(configId).first;
		ASSERT_NE(nullptr, row) << "fixture Item row " << configId << " is missing";
		if (stackable)
		{
			ASSERT_GT(row->max_stack_size(), 1u) << "Item row " << configId;
		}
		else
		{
			ASSERT_EQ(1u, row->max_stack_size()) << "Item row " << configId;
		}
		ASSERT_EQ(equipKind, row->equip_kind()) << "Item row " << configId;
	}

	static void RequireSlotRow(uint32_t slot, uint32_t equipKind)
	{
		const auto *row = EquipSlotTableManager::Instance().FindByIdSilent(slot).first;
		ASSERT_NE(nullptr, row) << "fixture EquipSlot row " << slot << " is missing";
		ASSERT_EQ(equipKind, row->equip_kind()) << "EquipSlot row " << slot;
	}

	static void RequireFixtureRows()
	{
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kEquipA, /*stackable=*/false, kKind1));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kEquipB, /*stackable=*/false, kKind1));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kPlainNonStack, /*stackable=*/false, 0));
		ASSERT_NO_FATAL_FAILURE(RequireItemRow(kStackable, /*stackable=*/true, 0));
		ASSERT_EQ(nullptr, ItemTableManager::Instance().FindByIdSilent(kUnknownConfig).first);
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kSlotKind1First, kKind1));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kSlotKind1Second, kKind1));
		ASSERT_NO_FATAL_FAILURE(RequireSlotRow(kSlotKind2, kKind2));
	}

	entt::entity NewPlayer()
	{
		const auto player = tlsEcs.actorRegistry.create();
		players.push_back(player);
		return player;
	}

	// 存档往返:source 的背包 Marshal -> 字节 -> Unmarshal 到一个新玩家身上,返回那个新玩家。
	// 真的过一遍编解码,而不是把内存里的 BagAllData 直接递过去。
	entt::entity RoundTripBags(entt::entity source)
	{
		BagAllData wire;
		bag_marshal::Marshal(source, wire);
		BagAllData parsed;
		EXPECT_TRUE(parsed.ParseFromString(wire.SerializeAsString()));
		const auto dest = NewPlayer();
		bag_marshal::Unmarshal(dest, parsed);
		return dest;
	}

	// 记账用的回调:每调一次写一条属性,value 逐次不同。于是"调了几次""每件是不是各拿各的
	// ItemComp"都能从包里读回来 —— 若多件共用同一份 ItemComp,属性条数会累加、value 会重复。
	void InstallCountingInitializer()
	{
		BagService::SetItemInstanceInitializer(
			[this](ItemComp &item)
			{
				sawEquipOnEntry = sawEquipOnEntry || item.has_equip();
				sawInvalidGuid = sawInvalidGuid || ItemStore::IsInvalidGuid(item);
				initializedGuids.push_back(item.item_id());
				AddAffix(item, kRolledAttrId, /*tier=*/1,
						 kRolledValueBase + static_cast<uint32_t>(initializerCalls), /*seq=*/0);
				++initializerCalls;
			});
	}

	std::vector<entt::entity> players;
	int initializerCalls{0};
	std::vector<Guid> initializedGuids;
	bool sawEquipOnEntry{false};
	bool sawInvalidGuid{false};
};

// ── 搬运原语:TakeInstance / PutInstance ──────────────────────────────────

// 穿上再脱下:同一 guid 换包,equip 段一个字段都不变;源包两层一致且少一件,目标包多一件。
TEST_F(BagInstanceDataTest, TakeThenPutMovesTheWholeInstanceBetweenBags)
{
	Bag inventory; // FlatLayout(kDefaultCapacity)
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));

	const ItemComp original = MakeRolledInstance(kGuidA, kEquipA);
	inventory.RestoreItems({{original, 4}});
	inventory.RestoreItems({{MakeInstance(kGuidB, kEquipB), 5}});
	ASSERT_EQ(2u, inventory.OccupiedGridCount());
	ASSERT_EQ(4u, inventory.GetItemPosByGuid(kGuidA));

	ItemComp carried;
	ASSERT_EQ(kSuccess, inventory.TakeInstance(kGuidA, carried));
	EXPECT_EQ(kGuidA, carried.item_id()) << "the guid is the instance's identity; it never changes";
	EXPECT_EQ(kEquipA, carried.config_id());
	EXPECT_EQ(1u, carried.size());
	ExpectSameEquip(original, carried);

	EXPECT_EQ(1u, inventory.OccupiedGridCount()) << "one instance fewer";
	EXPECT_EQ(1u, inventory.GridSlotCount()) << "and its slot is released too";
	EXPECT_TRUE(inventory.IsLayerConsistent());
	EXPECT_EQ(nullptr, inventory.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(kInvalidU32Id, inventory.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kInvalidGuid, inventory.Layout().At(4));
	EXPECT_NE(nullptr, inventory.GetItemCompByGuid(kGuidB)) << "the bystander stays";

	ASSERT_EQ(kSuccess, equipment.PutInstance(carried));
	EXPECT_EQ(1u, equipment.OccupiedGridCount()) << "one instance more";
	EXPECT_TRUE(equipment.IsLayerConsistent());
	EXPECT_EQ(kSlotKind1First, equipment.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kGuidA, equipment.Layout().At(kSlotKind1First));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, original));

	// 脱下:原路回去,属性仍然逐字段相等。
	ItemComp takenOff;
	ASSERT_EQ(kSuccess, equipment.TakeInstance(kGuidA, takenOff));
	EXPECT_EQ(0u, equipment.OccupiedGridCount());
	EXPECT_EQ(kInvalidGuid, equipment.Layout().At(kSlotKind1First));
	EXPECT_TRUE(equipment.IsLayerConsistent());

	ASSERT_EQ(kSuccess, inventory.PutInstance(takenOff));
	EXPECT_EQ(2u, inventory.OccupiedGridCount());
	EXPECT_TRUE(inventory.IsLayerConsistent());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(inventory, original));
}

TEST_F(BagInstanceDataTest, PutInstanceHonoursAnExplicitSlot)
{
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));

	// 部位 1 的第二个槽:自动找位会落到 0 号,指定槽必须真的落到 1 号。
	const ItemComp ring = MakeRolledInstance(kGuidA, kEquipA);
	ASSERT_EQ(kSuccess, equipment.PutInstance(ring, kSlotKind1Second));
	EXPECT_EQ(kSlotKind1Second, equipment.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kGuidA, equipment.Layout().At(kSlotKind1Second));
	EXPECT_EQ(kInvalidGuid, equipment.Layout().At(kSlotKind1First));
	EXPECT_TRUE(equipment.IsLayerConsistent());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, ring));

	// 自由格布局同样可以指定格子。
	Bag inventory;
	ASSERT_EQ(kSuccess, inventory.PutInstance(MakeInstance(kGuidB, kEquipB), uint32_t{7}));
	EXPECT_EQ(7u, inventory.GetItemPosByGuid(kGuidB));
	EXPECT_TRUE(inventory.IsLayerConsistent());
}

TEST_F(BagInstanceDataTest, PutInstanceWithoutASlotUsesTheSamePlacementAsAddItem)
{
	// 具名槽:按部位找第一个接受的空槽;该部位的槽用完就拒。
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	ASSERT_EQ(kSuccess, equipment.PutInstance(MakeInstance(kGuidA, kEquipA)));
	ASSERT_EQ(kSuccess, equipment.PutInstance(MakeInstance(kGuidB, kEquipB)));
	EXPECT_EQ(kSlotKind1First, equipment.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kSlotKind1Second, equipment.GetItemPosByGuid(kGuidB));

	const BagState before = Capture(equipment);
	EXPECT_EQ(kBagAddItemBagFull, equipment.PutInstance(MakeInstance(kGuidC, kEquipA)))
		<< "kind 1 only has two slots even though the bag has ten cells";
	ExpectUnchanged(before, equipment);
	EXPECT_NE(kSuccess, equipment.PutInstance(MakeInstance(kGuidD, kPlainNonStack)))
		<< "an item that declares no equip kind has no slot in a named-slot bag";
	ExpectUnchanged(before, equipment);

	// 自由格:first-fit。
	Bag inventory;
	inventory.RestoreItems({{MakeInstance(kGuidC, kPlainNonStack), 0}});
	ASSERT_EQ(kSuccess, inventory.PutInstance(MakeInstance(kGuidD, kEquipA)));
	EXPECT_EQ(1u, inventory.GetItemPosByGuid(kGuidD));
	EXPECT_TRUE(inventory.IsLayerConsistent());
}

TEST_F(BagInstanceDataTest, PutInstanceRefusesAnOccupiedSlotAndChangesNothing)
{
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	ASSERT_EQ(kSuccess, equipment.PutInstance(MakeRolledInstance(kGuidA, kEquipA), kSlotKind1First));
	const BagState before = Capture(equipment);

	// 绝不顶替:"替换"是编排层先把旧的 Take 走、再 Put 新的。
	EXPECT_EQ(kBagAddItemBagFull,
			  equipment.PutInstance(MakeInstance(kGuidB, kEquipB), kSlotKind1First));
	ExpectUnchanged(before, equipment);
	EXPECT_EQ(kGuidA, equipment.Layout().At(kSlotKind1First));
	EXPECT_EQ(nullptr, equipment.GetItemCompByGuid(kGuidB));
}

TEST_F(BagInstanceDataTest, PutInstanceRefusesASlotThatDoesNotAcceptTheKind)
{
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	ASSERT_EQ(kSuccess, equipment.PutInstance(MakeInstance(kGuidA, kEquipA), kSlotKind1First));
	const BagState before = Capture(equipment);

	EXPECT_EQ(kBagAddItemInvalidParam,
			  equipment.PutInstance(MakeInstance(kGuidB, kEquipB), kSlotKind2))
		<< "slot 2 is empty but accepts kind 2, not kind 1";
	ExpectUnchanged(before, equipment);

	EXPECT_EQ(kBagAddItemInvalidParam,
			  equipment.PutInstance(MakeInstance(kGuidB, kEquipB),
									static_cast<uint32_t>(equipment.Capacity())))
		<< "a slot beyond the capacity is refused before anything is written";
	ExpectUnchanged(before, equipment);

	EXPECT_EQ(kBagAddItemInvalidParam,
			  equipment.PutInstance(MakeInstance(kGuidC, kPlainNonStack), kSlotKind1Second))
		<< "kind 0 fits no named slot, not even an empty one";
	ExpectUnchanged(before, equipment);

	// 对照:自由格布局的格子没有部位语义,同一个槽号放什么都行。
	Bag inventory;
	EXPECT_EQ(kSuccess, inventory.PutInstance(MakeInstance(kGuidB, kEquipB), kSlotKind2));
	EXPECT_EQ(kSlotKind2, inventory.GetItemPosByGuid(kGuidB));
}

TEST_F(BagInstanceDataTest, PutInstanceRefusesAGuidAlreadyInTheBag)
{
	Bag inventory;
	ASSERT_EQ(kSuccess, inventory.PutInstance(MakeRolledInstance(kGuidA, kEquipA)));
	const BagState before = Capture(inventory);

	const ItemComp impostor = MakeInstance(kGuidA, kEquipB);
	EXPECT_EQ(kBagDeleteItemAlreadyHasGuid, inventory.PutInstance(impostor));
	EXPECT_EQ(kBagDeleteItemAlreadyHasGuid, inventory.PutInstance(impostor, uint32_t{3}));
	ExpectUnchanged(before, inventory);
}

TEST_F(BagInstanceDataTest, PutInstanceRefusesInvalidIdentityOrShape)
{
	Bag inventory;
	inventory.RestoreItems({{MakeInstance(kGuidA, kEquipA), 0}});
	const BagState before = Capture(inventory);

	// 搬运的是既有实例,必须带着自己的 guid 来:这里绝不替它铸号。
	EXPECT_EQ(kBagAddItemInvalidGuid, inventory.PutInstance(MakeInstance(kInvalidGuid, kEquipA)));
	EXPECT_EQ(kBagAddItemInvalidGuid, inventory.PutInstance(MakeInstance(0, kEquipA)));

	ItemComp twoPieces = MakeInstance(kGuidB, kEquipA);
	twoPieces.set_size(2);
	EXPECT_EQ(kBagAddItemInvalidParam, inventory.PutInstance(twoPieces));
	EXPECT_EQ(kBagAddItemInvalidParam, inventory.PutInstance(MakeInstance(kGuidB, kStackable)))
		<< "stackable items would bypass stack merging";
	EXPECT_EQ(kInvalidTableId, inventory.PutInstance(MakeInstance(kGuidB, kUnknownConfig)));
	ExpectUnchanged(before, inventory);
}

// 准入照常问:搬运不是绕过"这个包收不收这种东西"的后门。
TEST_F(BagInstanceDataTest, PutInstanceStillAsksAdmission)
{
	Bag festival;
	festival.SetProfile(BagProfile::Festival(5, {kStackable}));
	EXPECT_EQ(kBagAddItemInvalidParam, festival.PutInstance(MakeInstance(kGuidA, kEquipA)));
	EXPECT_EQ(0u, festival.OccupiedGridCount());
	EXPECT_TRUE(festival.IsLayerConsistent());
}

// 搬运绝不为了腾位销毁东西 —— 哪怕目标是配了先进先出淘汰的临时格。
TEST_F(BagInstanceDataTest, PutInstanceNeverEvictsEvenOnAnEvictingBag)
{
	Bag temporary;
	temporary.SetProfile(BagProfile::Temporary(1));
	temporary.RestoreItems({{MakeInstance(kGuidA, kEquipA), 0}});
	const BagState before = Capture(temporary);

	EXPECT_EQ(kBagAddItemBagFull, temporary.PutInstance(MakeInstance(kGuidB, kEquipB)));
	ExpectUnchanged(before, temporary);
	EXPECT_NE(nullptr, temporary.GetItemCompByGuid(kGuidA)) << "the old item must not be evicted";
}

// acquire_seq 是"本包内的入包先后",不随实例搬家:沿用源包的序号会抬高目标包水位、
// 让先进先出按别的包的时间线挤人。
TEST_F(BagInstanceDataTest, PutInstanceRestampsAcquireSeq)
{
	Bag source;
	ItemComp veteran = MakeInstance(kGuidA, kEquipA);
	veteran.set_acquire_seq(5000);
	source.RestoreItems({{veteran, 0}});
	ASSERT_NE(nullptr, source.GetItemCompByGuid(kGuidA));
	ASSERT_EQ(5000u, source.GetItemCompByGuid(kGuidA)->acquire_seq());

	Bag target;
	target.RestoreItems({{MakeInstance(kGuidB, kEquipB), 0}});       // 盖 1
	target.RestoreItems({{MakeInstance(kGuidC, kPlainNonStack), 1}}); // 盖 2

	ItemComp carried;
	ASSERT_EQ(kSuccess, source.TakeInstance(kGuidA, carried));
	EXPECT_EQ(5000u, carried.acquire_seq()) << "Take copies the instance out whole";
	ASSERT_EQ(kSuccess, target.PutInstance(carried));

	const ItemComp *landed = target.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, landed);
	EXPECT_EQ(3u, landed->acquire_seq()) << "restamped by the target bag, not carried over";

	// 水位也没被源包的 5000 抬高:下一件新物品接着盖 4。
	std::vector<Guid> written;
	ASSERT_EQ(kSuccess, target.AddItem(MakeParam(kEquipB), &written));
	ASSERT_EQ(1u, written.size());
	const ItemComp *next = target.GetItemCompByGuid(written.front());
	ASSERT_NE(nullptr, next);
	EXPECT_EQ(4u, next->acquire_seq());
}

TEST_F(BagInstanceDataTest, TakeInstanceRefusesStackablesMissingGuidsAndSkippedZeroRestore)
{
	Bag bag;
	std::vector<Guid> written;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kStackable, 5), &written));
	ASSERT_EQ(1u, written.size());
	const Guid stackGuid = written.front();
	// 旧存档的零数量实例在加载时跳过,不能留下槽位或变成可搬运资产。
	bag.InsertItemForRestore(kGuidA, kEquipA, /*stackSize=*/0, /*pos=*/3);
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA));
	EXPECT_EQ(nullptr, bag.GetItemCompByPos(3));
	EXPECT_EQ(kInvalidSlot, bag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(1u, bag.OccupiedGridCount());
	EXPECT_EQ(1u, bag.GridSlotCount());
	EXPECT_TRUE(bag.IsLayerConsistent());
	const BagState before = Capture(bag);

	ItemComp out = MakeInstance(kGuidD, kPlainNonStack); // 哨兵:失败不许动它
	const std::string sentinel = out.SerializeAsString();

	EXPECT_EQ(kBagDelItemConfig, bag.TakeInstance(stackGuid, out))
		<< "a stackable guid is not a stable identity";
	EXPECT_EQ(kBagDeleteItemFindGuid, bag.TakeInstance(kGuidB, out)) << "same code as RemoveItem";
	EXPECT_EQ(kBagDeleteItemFindGuid, bag.TakeInstance(kGuidA, out));

	EXPECT_EQ(sentinel, out.SerializeAsString()) << "a refused Take must not touch the out param";
	ExpectUnchanged(before, bag);
}

TEST_F(BagInstanceDataTest, SlotsAcceptingKindListsTableSlotsWithinCapacity)
{
	const std::vector<uint32_t> kind1Slots{kSlotKind1First, kSlotKind1Second};
	const std::vector<uint32_t> kind2Slots{kSlotKind2};
	const std::vector<uint32_t> firstSlotOnly{kSlotKind1First};

	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	EXPECT_EQ(kind1Slots, equipment.SlotsAcceptingKind(kKind1)) << "ascending slot ids";
	EXPECT_EQ(kind2Slots, equipment.SlotsAcceptingKind(kKind2));
	EXPECT_TRUE(equipment.SlotsAcceptingKind(0).empty()) << "kind 0 is not equipment";
	EXPECT_TRUE(equipment.SlotsAcceptingKind(kUnknownKind).empty());

	// 不看占用:穿了一只之后,该部位仍然是这两个槽;占用者另用 Layout().At 查。
	ASSERT_EQ(kSuccess, equipment.PutInstance(MakeInstance(kGuidA, kEquipA)));
	EXPECT_EQ(kind1Slots, equipment.SlotsAcceptingKind(kKind1));
	EXPECT_EQ(kGuidA, equipment.Layout().At(kSlotKind1First));
	EXPECT_EQ(kInvalidGuid, equipment.Layout().At(kSlotKind1Second));

	// 槽号必须落在当前容量内(容量可以被快照还原改小)。
	Bag narrow;
	narrow.SetProfile(BagProfile::Equipment(1));
	EXPECT_EQ(firstSlotOnly, narrow.SlotsAcceptingKind(kKind1));
	EXPECT_TRUE(narrow.SlotsAcceptingKind(kKind2).empty());

	// 自由格布局的槽位没有部位语义。
	Bag inventory;
	EXPECT_TRUE(inventory.SlotsAcceptingKind(kKind1).empty());
}

// ── 新铸实例初始化回调(ItemInstanceInitializer)──────────────────────────

// count = 3 的批量:回调恰好 3 次,每件各拿各的 ItemComp(各自的 guid、各自的一条属性)。
TEST_F(BagInstanceDataTest, InitializerRunsOncePerNewNonStackableInstanceInABatch)
{
	InstallCountingInitializer();
	Bag bag;
	ItemCountMap batch;
	batch[kEquipA] = 3;
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, batch));

	EXPECT_EQ(3, initializerCalls);
	EXPECT_FALSE(sawEquipOnEntry) << "a freshly minted instance arrives without an equip section";
	EXPECT_FALSE(sawInvalidGuid) << "the callback runs after the guid is minted";
	ASSERT_EQ(3u, bag.OccupiedGridCount());

	std::set<Guid> storedGuids;
	std::set<uint32_t> storedValues;
	bag.ForEachItem([&storedGuids, &storedValues](Guid guid, const ItemComp &item)
					{
						storedGuids.insert(guid);
						EXPECT_TRUE(item.has_equip());
						EXPECT_EQ(1, item.equip().affixes_size())
							<< "pieces sharing one ItemComp would accumulate rows";
						if (item.equip().affixes_size() == 1)
						{
							storedValues.insert(item.equip().affixes(0).value());
						}
					});
	const std::set<Guid> seenGuids(initializedGuids.begin(), initializedGuids.end());
	const std::set<uint32_t> expectedValues{kRolledValueBase, kRolledValueBase + 1,
											kRolledValueBase + 2};
	EXPECT_EQ(3u, seenGuids.size()) << "three calls, three different instances";
	EXPECT_EQ(seenGuids, storedGuids) << "what the callback saw is what went into the store";
	EXPECT_EQ(expectedValues, storedValues) << "each piece keeps its own roll";
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// BagService 的三个入包入口各有一套自己的循环,漏接任何一套那条入口发的装备就没有属性。
TEST_F(BagInstanceDataTest, InitializerIsWiredIntoEveryBagServiceAddEntry)
{
	InstallCountingInitializer();
	Bag bag;

	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, MakeParam(kEquipA)));
	EXPECT_EQ(1, initializerCalls) << "AddItem(InitItemParam)";

	ItemCountMap byCount;
	byCount[kEquipB] = 2;
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, byCount));
	EXPECT_EQ(3, initializerCalls) << "AddItems(ItemCountMap)";

	const std::vector<InitItemParam> pieces{MakeParam(kEquipA), MakeParam(kPlainNonStack)};
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, pieces));
	EXPECT_EQ(5, initializerCalls) << "AddItems(vector<InitItemParam>)";

	ASSERT_EQ(5u, bag.OccupiedGridCount());
	bag.ForEachItem([](Guid, const ItemComp &item) { EXPECT_TRUE(item.has_equip()); });
}

// has_equip() 是唯一的幂等判据:带着装备数据来的既有实例(邮件回流等)绝不重掷,
// 哪怕它一条属性都没有。
TEST_F(BagInstanceDataTest, InitializerSkipsInstancesThatAlreadyCarryEquip)
{
	InstallCountingInitializer();
	Bag bag;

	InitItemParam returning;
	returning.itemPBComp = MakeRolledInstance(kGuidA, kEquipA);
	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, returning));
	EXPECT_EQ(0, initializerCalls);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(bag, returning.itemPBComp));

	InitItemParam emptyRolled;
	emptyRolled.itemPBComp = MakeInstance(kGuidB, kEquipB);
	emptyRolled.itemPBComp.mutable_equip(); // 已初始化,只是一条都没掷出
	const std::vector<InitItemParam> pieces{emptyRolled, MakeParam(kEquipA)};
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, pieces));
	EXPECT_EQ(1, initializerCalls) << "only the fresh piece of the mixed batch is initialised";

	const ItemComp *kept = bag.GetItemCompByGuid(kGuidB);
	ASSERT_NE(nullptr, kept);
	EXPECT_TRUE(kept->has_equip());
	EXPECT_EQ(0, kept->equip().affixes_size()) << "zero rows must not be mistaken for 'not rolled'";
	ASSERT_EQ(1u, initializedGuids.size());
	EXPECT_NE(kGuidA, initializedGuids.front());
	EXPECT_NE(kGuidB, initializedGuids.front());
}

TEST_F(BagInstanceDataTest, InitializerSkipsStackableItems)
{
	InstallCountingInitializer();
	Bag bag;

	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, MakeParam(kStackable, 5)));
	ItemCountMap byCount;
	byCount[kStackable] = 7;
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, byCount));
	const std::vector<InitItemParam> pieces{MakeParam(kStackable, 2)};
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, pieces));

	EXPECT_EQ(0, initializerCalls);
	EXPECT_EQ(14u, bag.GetTotalItemCount(kStackable));
	bag.ForEachItem([](Guid, const ItemComp &item) { EXPECT_FALSE(item.has_equip()); });
}

// 还原与搬运都不是"新铸":没有 equip 段就是它本来就没有,不是还没掷。
TEST_F(BagInstanceDataTest, InitializerNeverRunsOnRestoreOrCarry)
{
	InstallCountingInitializer();
	Bag bag;
	bag.InsertItemForRestore(kGuidA, kEquipA, /*stackSize=*/1, /*pos=*/0); // 位置参数重载
	bag.RestoreItems({{MakeInstance(kGuidB, kEquipB), 1}});            // 整份重载
	EXPECT_EQ(0, initializerCalls);
	ASSERT_EQ(2u, bag.OccupiedGridCount());
	bag.ForEachItem([](Guid, const ItemComp &item) { EXPECT_FALSE(item.has_equip()); });

	Bag other;
	ItemComp carried;
	ASSERT_EQ(kSuccess, bag.TakeInstance(kGuidA, carried));
	ASSERT_EQ(kSuccess, other.PutInstance(carried));
	EXPECT_EQ(0, initializerCalls);
	const ItemComp *moved = other.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, moved);
	EXPECT_FALSE(moved->has_equip()) << "carrying must not roll attributes";
}

TEST_F(BagInstanceDataTest, UninstalledInitializerIsNotCalled)
{
	InstallCountingInitializer();
	Bag bag;
	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, MakeParam(kEquipA)));
	ASSERT_EQ(1, initializerCalls);

	BagService::SetItemInstanceInitializer({}); // 传空 = 卸载
	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, MakeParam(kEquipB)));
	ItemCountMap byCount;
	byCount[kEquipA] = 2;
	ASSERT_EQ(kSuccess, BagService::AddItems(entt::null, bag, byCount));
	EXPECT_EQ(1, initializerCalls);

	uint32_t withoutEquip = 0;
	bag.ForEachItem([&withoutEquip](Guid, const ItemComp &item)
					{
						if (!item.has_equip())
						{
							++withoutEquip;
						}
					});
	EXPECT_EQ(3u, withoutEquip);
}

// Bag 自己不持有全局状态:直接调 Bag 的路径不传回调就是不调(既有用例行为不变);
// 显式传进来的才调,空 function 等同没传。
TEST_F(BagInstanceDataTest, BagItselfHoldsNoInitializer)
{
	InstallCountingInitializer();
	Bag bag;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kEquipA, 2)));
	ItemCountMap byCount;
	byCount[kEquipB] = 1;
	ASSERT_EQ(kSuccess, bag.AddItems(byCount));
	EXPECT_EQ(0, initializerCalls) << "the process-wide initializer lives in BagService only";

	int directCalls = 0;
	const ItemInstanceInitializer direct = [&directCalls](ItemComp &item)
	{
		item.mutable_equip();
		++directCalls;
	};
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kEquipA, 2), nullptr, nullptr, &direct));
	EXPECT_EQ(2, directCalls);

	const ItemInstanceInitializer empty;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kEquipA), nullptr, nullptr, &empty));
	EXPECT_EQ(2, directCalls);
	EXPECT_EQ(0, initializerCalls);
	EXPECT_EQ(6u, bag.OccupiedGridCount());
}

// 回调只许写实例数据。它若改了容器负责的字段(身份 / 模板 / 数量 / 入包序号),
// 这一件会带着没做过 reserve / 准入 / 撞号预检的身份进包 —— 容器把它们钉回去。
TEST_F(BagInstanceDataTest, InitializerCannotRewriteContainerOwnedFields)
{
	BagService::SetItemInstanceInitializer(
		[](ItemComp &item)
		{
			item.set_item_id(kGuidD);
			item.set_config_id(kEquipB);
			item.set_size(9);
			item.set_acquire_seq(777);
			item.mutable_equip()->add_affixes()->set_value(5);
		});
	Bag bag;
	ASSERT_EQ(kSuccess, BagService::AddItem(entt::null, bag, MakeParam(kEquipA)));

	ASSERT_EQ(1u, bag.OccupiedGridCount());
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidD)) << "the minted guid must win";
	bag.ForEachItem([](Guid guid, const ItemComp &item)
					{
						EXPECT_EQ(guid, item.item_id());
						EXPECT_EQ(kEquipA, item.config_id());
						EXPECT_EQ(1u, item.size());
						EXPECT_EQ(1u, item.acquire_seq()) << "stamped by the store, not by the callback";
						EXPECT_EQ(1, item.equip().affixes_size()) << "instance data is kept";
					});
	EXPECT_EQ(1u, bag.GetTotalItemCount(kEquipA));
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 存档往返(bag_marshal)────────────────────────────────────────────────

// 固定包:三种形态分别放在人物背包 / 装备栏 / 仓库,Marshal -> 字节 -> Unmarshal 后
// presence 与内容都保持。
TEST_F(BagInstanceDataTest, FixedBagMarshalRoundTripKeepsEquipPresenceAndContent)
{
	const ItemComp twoRows = MakeRolledInstance(kGuidA, kEquipA);
	ItemComp zeroRows = MakeInstance(kGuidB, kEquipB);
	zeroRows.mutable_equip();
	const ItemComp plain = MakeInstance(kGuidC, kPlainNonStack);

	const auto source = NewPlayer();
	uint64_t sourceSeq = 0;
	{
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
		bags.bags[kInventory].RestoreItems({{twoRows, 3}});
		bags.bags[kEquipment].RestoreItems({{zeroRows, kSlotKind1First}});
		bags.bags[kWarehouse].RestoreItems({{plain, 7}});
		ASSERT_NE(nullptr, bags.bags[kInventory].GetItemCompByGuid(kGuidA));
		sourceSeq = bags.bags[kInventory].GetItemCompByGuid(kGuidA)->acquire_seq();
		ASSERT_NE(0u, sourceSeq);
	}

	BagAllData wire;
	bag_marshal::Marshal(source, wire);
	ASSERT_EQ(3, wire.items_size());
	ASSERT_NO_FATAL_FAILURE(ExpectEntriesCarryInstanceData(wire.items(), twoRows, zeroRows, plain));

	// 真的过一遍编解码:presence 必须撑得过字节。
	std::string bytes;
	ASSERT_TRUE(wire.SerializeToString(&bytes));
	BagAllData parsed;
	ASSERT_TRUE(parsed.ParseFromString(bytes));
	ASSERT_NO_FATAL_FAILURE(ExpectEntriesCarryInstanceData(parsed.items(), twoRows, zeroRows, plain));

	const auto dest = NewPlayer();
	bag_marshal::Unmarshal(dest, parsed);
	auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
	ASSERT_NE(nullptr, restored);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kInventory], twoRows));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kEquipment], zeroRows));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kWarehouse], plain));
	EXPECT_EQ(3u, restored->bags[kInventory].GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kSlotKind1First, restored->bags[kEquipment].GetItemPosByGuid(kGuidB));
	EXPECT_EQ(7u, restored->bags[kWarehouse].GetItemPosByGuid(kGuidC));
	EXPECT_EQ(sourceSeq, restored->bags[kInventory].GetItemCompByGuid(kGuidA)->acquire_seq())
		<< "acquire_seq still round-trips through the shared conversion";
	for (auto &bag : restored->bags)
	{
		EXPECT_TRUE(bag.IsLayerConsistent());
	}
}

// 动态包走的是另一对 Marshal / Unmarshal 循环,必须与固定包共用同一对转换函数。
TEST_F(BagInstanceDataTest, DynamicBagMarshalRoundTripKeepsEquipPresenceAndContent)
{
	const ItemComp twoRows = MakeRolledInstance(kGuidA, kEquipA);
	ItemComp zeroRows = MakeInstance(kGuidB, kEquipB);
	zeroRows.mutable_equip();
	const ItemComp plain = MakeInstance(kGuidC, kPlainNonStack);

	const auto source = NewPlayer();
	{
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
		Bag &dynamicBag = bags.dynamicBags_[kDynamicBagId];
		dynamicBag.RestoreItems({{twoRows, 2}});
		dynamicBag.RestoreItems({{zeroRows, 5}});
		dynamicBag.RestoreItems({{plain, 6}});
		ASSERT_EQ(3u, dynamicBag.OccupiedGridCount());
	}

	BagAllData wire;
	bag_marshal::Marshal(source, wire);
	EXPECT_EQ(0, wire.items_size()) << "nothing was put into the fixed bags";
	ASSERT_EQ(1, wire.dynamic_bags_size());
	ASSERT_EQ(3, wire.dynamic_bags(0).items_size());

	std::string bytes;
	ASSERT_TRUE(wire.SerializeToString(&bytes));
	BagAllData parsed;
	ASSERT_TRUE(parsed.ParseFromString(bytes));
	ASSERT_EQ(1, parsed.dynamic_bags_size());
	ASSERT_NO_FATAL_FAILURE(
		ExpectEntriesCarryInstanceData(parsed.dynamic_bags(0).items(), twoRows, zeroRows, plain));

	const auto dest = NewPlayer();
	bag_marshal::Unmarshal(dest, parsed);
	auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
	ASSERT_NE(nullptr, restored);
	const auto dynamicIt = restored->dynamicBags_.find(kDynamicBagId);
	ASSERT_NE(restored->dynamicBags_.end(), dynamicIt);
	Bag &restoredBag = dynamicIt->second;
	ASSERT_EQ(3u, restoredBag.OccupiedGridCount());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restoredBag, twoRows));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restoredBag, zeroRows));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restoredBag, plain));
	EXPECT_EQ(2u, restoredBag.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(5u, restoredBag.GetItemPosByGuid(kGuidB));
	EXPECT_EQ(6u, restoredBag.GetItemPosByGuid(kGuidC));
	EXPECT_TRUE(restoredBag.IsLayerConsistent());
}

TEST_F(BagInstanceDataTest, FixedAndDynamicSqlSnapshotRoundTripKeepsCompleteInstanceUnknownFields)
{
	const auto source = NewPlayer();
	auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
	auto item = MakeRolledInstance(kGuidA, kEquipA);
	item.GetReflection()->MutableUnknownFields(&item)->AddVarint(100, 456);
	item.mutable_equip()->GetReflection()->MutableUnknownFields(item.mutable_equip())->AddLengthDelimited(101, "future-growth");
	bags.bags[kInventory].RestoreItems({{item, 3}});
	item.set_item_id(kGuidB);
	bags.dynamicBags_[kDynamicBagId].RestoreItems({{item, 5}});
	const auto fixedBefore = Capture(bags.bags[kInventory]);
	const auto dynamicBefore = Capture(bags.dynamicBags_[kDynamicBagId]);
	BagAllData wire;
	bag_marshal::Marshal(source, wire);
	ASSERT_EQ(1, wire.items_size());
	ASSERT_TRUE(wire.items(0).has_instance());
	ASSERT_EQ(1, wire.dynamic_bags_size());
	ASSERT_TRUE(wire.dynamic_bags(0).items(0).has_instance());
	BagAllData parsed;
	ASSERT_TRUE(parsed.ParseFromString(wire.SerializeAsString()));
	const auto dest = NewPlayer();
	bag_marshal::Unmarshal(dest, parsed);
	const auto& restored = tlsEcs.actorRegistry.get<PlayerBagsComp>(dest);
	EXPECT_EQ(fixedBefore, Capture(restored.bags[kInventory]));
	const auto dynamic = restored.dynamicBags_.find(kDynamicBagId);
	ASSERT_NE(restored.dynamicBags_.end(), dynamic);
	EXPECT_EQ(dynamicBefore, Capture(dynamic->second));
}

TEST_F(BagInstanceDataTest, LegacyItemEntryIsUpgradedWithoutChangingInstanceOrLayout)
{
	const auto player = NewPlayer();
	BagAllData legacy;
	legacy.add_capacities(20);
	auto* entry = legacy.add_items();
	entry->set_item_uuid(kGuidA);
	entry->set_config_id(kEquipA);
	entry->set_stack_size(1);
	entry->set_pos(3);
	entry->set_acquire_seq(41);
	entry->mutable_equip()->set_enhance_level(7);
	bag_marshal::Unmarshal(player, legacy);
	BagAllData upgraded;
	bag_marshal::Marshal(player, upgraded);
	ASSERT_EQ(1, upgraded.items_size());
	const auto& saved = upgraded.items(0);
	ASSERT_TRUE(saved.has_instance());
	EXPECT_EQ(kGuidA, saved.instance().item_id());
	EXPECT_EQ(kEquipA, saved.instance().config_id());
	EXPECT_EQ(41u, saved.instance().acquire_seq());
	EXPECT_EQ(7u, saved.instance().equip().enhance_level());
	EXPECT_EQ(3u, saved.pos());
}

// 还原的整份重载:equip 与 acquire_seq 原样保留;位置参数重载仍然可用且等价于"没有 equip"。
TEST_F(BagInstanceDataTest, RestoreOverloadKeepsTheWholeInstance)
{
	Bag bag;
	ItemComp rolled = MakeRolledInstance(kGuidA, kEquipA);
	rolled.set_acquire_seq(41);
	bag.RestoreItems({{rolled, 2}});
	bag.InsertItemForRestore(kGuidB, kEquipB, /*stackSize=*/1, /*pos=*/6, /*acquireSeq=*/42);

	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(bag, rolled));
	const ItemComp *kept = bag.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, kept);
	EXPECT_EQ(41u, kept->acquire_seq());
	EXPECT_EQ(2u, bag.GetItemPosByGuid(kGuidA));

	const ItemComp *legacy = bag.GetItemCompByGuid(kGuidB);
	ASSERT_NE(nullptr, legacy);
	EXPECT_EQ(kEquipB, legacy->config_id());
	EXPECT_EQ(1u, legacy->size());
	EXPECT_EQ(42u, legacy->acquire_seq());
	EXPECT_FALSE(legacy->has_equip());
	EXPECT_EQ(6u, bag.GetItemPosByGuid(kGuidB));
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── CanStack 兜底(不变量 7:装备被误配成可堆叠时)──────────────────────────

TEST_F(BagInstanceDataTest, CanStackRefusesAnySideCarryingEquip)
{
	const ItemComp plainA = MakeInstance(kGuidA, kStackable);
	const ItemComp plainB = MakeInstance(kGuidB, kStackable);
	EXPECT_TRUE(ItemStore::CanStack(plainA, plainB));
	EXPECT_FALSE(ItemStore::CanStack(plainA, MakeInstance(kGuidC, kEquipA))) << "different config";

	ItemComp rolled = plainB;
	rolled.mutable_equip(); // 只有 presence、0 条属性,同样不许并
	EXPECT_FALSE(ItemStore::CanStack(plainA, rolled));
	EXPECT_FALSE(ItemStore::CanStack(rolled, plainA));
	EXPECT_FALSE(ItemStore::CanStack(rolled, rolled));
}

// 行为级:一个带 equip 的堆(模拟"装备被误配成可堆叠")既不吸收入包的数量,也不被整理并掉。
// CanStack 只管入包并堆那一条路;另有三处不经过它、各自内联了等价判断 —— reserve 侧的空余
// 估算(MeasureFreeRoomPerConfig)、整理的早退判据(HasMergeablePartials)、整理的合并
// (MergePartialStacks)。下面四段各钉一处:删掉哪一处的判断,对应那一段就红。
TEST_F(BagInstanceDataTest, StacksCarryingEquipAreNeverMergedInto)
{
	Bag bag;
	ItemComp misconfigured = MakeInstance(kGuidE, kStackable);
	AddAffix(misconfigured, /*attrId=*/3, /*tier=*/1, /*value=*/9, /*seq=*/0);
	bag.RestoreItems({{misconfigured, 0}});

	// ① 入包并堆(PlanStackIntoExistingStacks -> CanStack):进来的数量另开一堆。
	std::vector<Guid> written;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kStackable, 4), &written));
	ASSERT_EQ(1u, written.size());
	const Guid plainStackGuid = written.front();
	EXPECT_NE(kGuidE, plainStackGuid) << "incoming units must open a new stack";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(bag, misconfigured));

	// ② 整理的早退判据(HasMergeablePartials):同 config 有两个未满堆,但其中一个带实例
	//    数据,所以"没什么可合并的";只合并不重排的整理应当在早退处就返回。
	EXPECT_FALSE(bag.MergeAndCompact(nullptr, CompactPolicy::kMergeOnly))
		<< "two partial stacks of one config, but one of them carries instance data";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(bag, misconfigured));
	EXPECT_EQ(5u, bag.GetTotalItemCount(kStackable));
	EXPECT_TRUE(bag.IsLayerConsistent());

	// ③ 整理的合并(MergePartialStacks):上面那次调用在早退处就返回了,根本没走到合并。
	//    换成会重排的整理 —— 0 号槽是 size 1 的带属性堆、1 号槽是 size 4 的普通堆,不满足
	//    (config 升序, size 降序),流程必然越过早退、真的执行一遍合并。不跳过带属性的堆,
	//    两堆就会并成一堆 5 个:数量守恒,属性只剩一份或一份都不剩,另一个实例被回收。
	std::vector<DestroyedInstance> destroyed;
	ASSERT_TRUE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeAndReorder))
		<< "the layout is out of order, so this call must get past the early return";
	EXPECT_TRUE(destroyed.empty()) << "nothing may be merged away";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(bag, misconfigured));
	const ItemComp *plainStack = bag.GetItemCompByGuid(plainStackGuid);
	ASSERT_NE(nullptr, plainStack);
	EXPECT_EQ(4u, plainStack->size()) << "the plain stack keeps its own units";
	EXPECT_EQ(5u, bag.GetTotalItemCount(kStackable));
	EXPECT_TRUE(bag.IsLayerConsistent());

	// ④ reserve 侧的空余估算(MeasureFreeRoomPerConfig):只有一格的包,这一格被带属性的堆
	//    占着。把它的空余算给进来的数量,预检就会答"并进去即可、不用新格子"而放行,
	//    commit 侧却不往里填 —— reserve 比 commit 乐观。只问 1 个单位:任何 > 1 的堆叠
	//    上限下,不跳过时它都"并得进去"。
	Bag oneCell;
	oneCell.SetProfile(BagProfile::Flat(1));
	oneCell.RestoreItems({{misconfigured, 0}});
	ASSERT_EQ(1u, oneCell.OccupiedGridCount());
	const BagState before = Capture(oneCell);
	ItemCountMap oneMoreUnit;
	oneMoreUnit[kStackable] = 1;
	EXPECT_EQ(kBagItemNotStacked, oneCell.CheckSpaceFor(oneMoreUnit))
		<< "an equip-carrying stack offers no free room; one more unit needs a cell of its own";
	ExpectUnchanged(before, oneCell);
}

// ── 带未知字段的实例同样不并堆(docs/design/mixed-version-persist-field-protection.md §4.4)──────
//
// 新版本给 ItemComp 加的属性(例如绑定、限时),本版本解析时落在未知字段里。并堆只加数量,会把带属性的物品
// 并进普通堆、或把属性抹成其中一份。判定与 equip 共用 ItemStore::IsIndependentInstance,下面两段各钉一层。
namespace
{
constexpr int kNewerVersionFieldNumber = 412345;

ItemComp MakeInstanceFromNewerVersion(Guid guid, uint32_t configId)
{
	ItemComp item = MakeInstance(guid, configId);
	item.mutable_unknown_fields()->AddVarint(kNewerVersionFieldNumber, 1);
	return item;
}
} // namespace

TEST_F(BagInstanceDataTest, CanStackRefusesAnySideCarryingUnknownFields)
{
	const ItemComp plain = MakeInstance(kGuidA, kStackable);
	const ItemComp fromNewerVersion = MakeInstanceFromNewerVersion(kGuidB, kStackable);
	EXPECT_TRUE(ItemStore::CanStack(plain, MakeInstance(kGuidC, kStackable)));
	EXPECT_FALSE(ItemStore::CanStack(plain, fromNewerVersion));
	EXPECT_FALSE(ItemStore::CanStack(fromNewerVersion, plain));
	EXPECT_FALSE(ItemStore::CanStack(fromNewerVersion, fromNewerVersion));
}

// 守卫只认"更新版本才有的字段";已退役字段(台账 retired 行)的残留字节在之后每个版本里都是未知字段、照样携带,
// 但不能让对象被永远当成新版本数据(设计 §3.1、§4.4)。生产里的退役清单来自生成产物,目前为空,
// 这里用 ContainsUnretiredFields 的谓词参数给出非空清单,钉住过滤本身。
TEST(UnknownFieldCarryTest, OnlyUnretiredUnknownFieldsCountAsNewerVersionFields)
{
	constexpr int kRetiredNumber = 7;
	constexpr int kNewNumber = 412345;
	const auto isRetired = [](int fieldNumber) { return fieldNumber == kRetiredNumber; };

	google::protobuf::UnknownFieldSet onlyRetired;
	onlyRetired.AddVarint(kRetiredNumber, 1);
	EXPECT_FALSE(unknown_field_carry::ContainsUnretiredFields(onlyRetired, isRetired))
		<< "只有退役残留:不算更新版本的数据";

	google::protobuf::UnknownFieldSet mixed;
	mixed.AddVarint(kRetiredNumber, 1);
	mixed.AddVarint(kNewNumber, 2);
	EXPECT_TRUE(unknown_field_carry::ContainsUnretiredFields(mixed, isRetired));

	EXPECT_FALSE(unknown_field_carry::ContainsUnretiredFields(google::protobuf::UnknownFieldSet{}, isRetired));

	// 消息与字节两种形态口径一致;生产退役清单为空时,任何未知字段都算。
	ItemComp item = MakeInstance(kGuidA, kStackable);
	EXPECT_FALSE(unknown_field_carry::CarriesNewerVersionFields(item));
	item.mutable_unknown_fields()->AddVarint(kNewNumber, 1);
	EXPECT_TRUE(unknown_field_carry::CarriesNewerVersionFields(item));
	const std::string captured = unknown_field_carry::Capture(item);
	EXPECT_TRUE(unknown_field_carry::CarriesNewerVersionFields("ItemComp", captured));
	EXPECT_FALSE(unknown_field_carry::CarriesNewerVersionFields("ItemComp", std::string{}));
	// 携带的字节坏了只可能是内存被破坏:按"带着"处理,守卫拒绝即 fail-closed。
	EXPECT_TRUE(unknown_field_carry::CarriesNewerVersionFields("ItemComp", std::string("\x0b", 1)));
}

// 行为级:入包并堆、整理(早退判据与合并)、reserve 侧的空余估算都不碰这种堆,未知字段原样留在实例上。
TEST_F(BagInstanceDataTest, StacksCarryingUnknownFieldsAreNeverMergedInto)
{
	const auto expectUnknownKept = [](Bag &bag)
	{
		const ItemComp *kept = bag.GetItemCompByGuid(kGuidE);
		ASSERT_NE(nullptr, kept);
		EXPECT_EQ(1u, kept->size());
		const auto &unknown = kept->unknown_fields();
		ASSERT_EQ(1, unknown.field_count());
		EXPECT_EQ(kNewerVersionFieldNumber, unknown.field(0).number());
		EXPECT_EQ(1u, unknown.field(0).varint());
	};

	Bag bag;
	bag.RestoreItems({{MakeInstanceFromNewerVersion(kGuidE, kStackable), 0}});

	// ① 入包并堆:进来的数量另开一堆。
	std::vector<Guid> written;
	ASSERT_EQ(kSuccess, bag.AddItem(MakeParam(kStackable, 4), &written));
	ASSERT_EQ(1u, written.size());
	EXPECT_NE(kGuidE, written.front()) << "incoming units must open a new stack";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	ASSERT_NO_FATAL_FAILURE(expectUnknownKept(bag));

	// ② 整理的早退判据:同 config 两个未满堆,但其中一个是独立实例,没什么可合并的。
	EXPECT_FALSE(bag.MergeAndCompact(nullptr, CompactPolicy::kMergeOnly));
	// ③ 整理的合并:布局不满足 (config 升序, size 降序),必然越过早退真的跑一遍合并。
	std::vector<DestroyedInstance> destroyed;
	ASSERT_TRUE(bag.MergeAndCompact(&destroyed, CompactPolicy::kMergeAndReorder));
	EXPECT_TRUE(destroyed.empty()) << "nothing may be merged away";
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	// 按 config 计数只认可互换的普通单位(第 6 批):带新版本字段的那 1 个不计,普通堆的 4 个照计。
	EXPECT_EQ(4u, bag.GetTotalItemCount(kStackable));
	ASSERT_NO_FATAL_FAILURE(expectUnknownKept(bag));
	EXPECT_TRUE(bag.IsLayerConsistent());

	// ④ reserve 侧的空余估算:唯一一格被这种堆占着,再来 1 个单位也要新格子。
	Bag oneCell;
	oneCell.SetProfile(BagProfile::Flat(1));
	oneCell.RestoreItems({{MakeInstanceFromNewerVersion(kGuidE, kStackable), 0}});
	const BagState before = Capture(oneCell);
	ItemCountMap oneMoreUnit;
	oneMoreUnit[kStackable] = 1;
	EXPECT_EQ(kBagItemNotStacked, oneCell.CheckSpaceFor(oneMoreUnit));
	ExpectUnchanged(before, oneCell);
}

// 传入物品一侧:Bag::AddItems 的规划与提交侧 CanStack 用同一个判定(ItemStore::IsIndependentInstance)。
// 规划若仍只看 has_equip(),带未知字段、可叠加的传入物品会被算成"并进既有半堆、不用新实例",少铸号;
// 提交时 CanStack 却不让它并,另开新堆要的号不够 —— Debug 撞"规划与预取身份数不一致"的 assert,Release 整批失败。
// vector<InitItemParam> 入口本来就是为搬完整实例准备的,这条路迟早会有调用方。
TEST_F(BagInstanceDataTest, AddItemsPlansIncomingInstancesCarryingUnknownFieldsAsSeparate)
{
	Bag bag;
	bag.RestoreItems({{MakeInstance(kGuidA, kStackable), 0}}); // 同 config 的普通半堆

	InitItemParam fromNewerVersion = MakeParam(kStackable, 3);
	fromNewerVersion.itemPBComp.mutable_unknown_fields()->AddVarint(kNewerVersionFieldNumber, 5);

	std::vector<GuidVector> written;
	ASSERT_EQ(kSuccess, bag.AddItems(std::vector<InitItemParam>{fromNewerVersion}, nullptr, &written));
	ASSERT_EQ(1u, written.size());
	ASSERT_EQ(1u, written.front().size());
	const Guid opened = written.front().front();
	EXPECT_NE(kGuidA, opened) << "带未知字段的传入物品不得并进既有半堆";
	const ItemComp *halfStack = bag.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, halfStack);
	EXPECT_EQ(1u, halfStack->size()) << "既有半堆不变";
	const ItemComp *carried = bag.GetItemCompByGuid(opened);
	ASSERT_NE(nullptr, carried);
	EXPECT_EQ(3u, carried->size());
	ASSERT_EQ(1, carried->unknown_fields().field_count()) << "未知字段跟着新实例走";
	EXPECT_EQ(kNewerVersionFieldNumber, carried->unknown_fields().field(0).number());
	EXPECT_EQ(5u, carried->unknown_fields().field(0).varint());
	EXPECT_EQ(2u, bag.OccupiedGridCount());
	EXPECT_TRUE(bag.IsLayerConsistent());

	// 同批两条同 config、都带未知字段:彼此不并,也不并进上一批开的那堆,建出 2 个实例。
	InitItemParam another = MakeParam(kStackable, 2);
	another.itemPBComp.mutable_unknown_fields()->AddVarint(kNewerVersionFieldNumber, 6);
	written.clear();
	ASSERT_EQ(kSuccess, bag.AddItems(std::vector<InitItemParam>{fromNewerVersion, another}, nullptr, &written));
	ASSERT_EQ(2u, written.size());
	ASSERT_EQ(1u, written[0].size());
	ASSERT_EQ(1u, written[1].size());
	EXPECT_NE(written[0].front(), written[1].front());
	EXPECT_NE(opened, written[0].front());
	EXPECT_NE(opened, written[1].front());
	EXPECT_EQ(4u, bag.OccupiedGridCount());
	// 按 config 计数只认普通单位(第 6 批):三堆带新版本字段的(3、3、2)不计,只剩普通半堆的 1 个。
	EXPECT_EQ(1u, bag.GetTotalItemCount(kStackable));
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 按 config 计数和扣数量:带更新版本字段的单位不当普通单位(混版本设计 §4.4 第 6 批)──────
//
// 新版本给可叠加物品加的属性(例如绑定),旧代码看不懂;按数量计数、预检、扣除三处同口径跳过这种堆,
// 预检三态区分"版本错位"(可重试)与"确定不足"(可记终态)。has_equip 的实例照算(由调用方的配置表排除)。
TEST_F(BagInstanceDataTest, QuantityPathsSkipStacksCarryingNewerVersionFields)
{
	ItemComp plain = MakeInstance(kGuidA, kStackable);
	plain.set_size(3);
	ItemComp fromNewerVersion = MakeInstanceFromNewerVersion(kGuidB, kStackable);
	fromNewerVersion.set_size(5);
	ASSERT_TRUE(ItemStore::IsCountableByConfig(plain));
	ASSERT_FALSE(ItemStore::IsCountableByConfig(fromNewerVersion));
	Bag bag;
	bag.RestoreItems({{plain, 0}, {fromNewerVersion, 1}});

	EXPECT_EQ(3u, bag.GetTotalItemCount(kStackable)) << "只计普通单位";
	EXPECT_EQ(ItemStore::QuantityVerdict::kEnough, bag.CheckQuantities({{kStackable, 3}}));
	EXPECT_EQ(ItemStore::QuantityVerdict::kVersionSkew, bag.CheckQuantities({{kStackable, 4}}))
		<< "算上带新字段的 5 个才够:版本错位,不能记成终态不足";
	EXPECT_EQ(ItemStore::QuantityVerdict::kVersionSkew, bag.CheckQuantities({{kStackable, 8}}));
	EXPECT_EQ(ItemStore::QuantityVerdict::kInsufficient, bag.CheckQuantities({{kStackable, 9}}))
		<< "连算上都不够:确定不足";
	EXPECT_EQ(ItemStore::QuantityVerdict::kInsufficient,
			  bag.CheckQuantities({{kStackable, 4}, {kPlainNonStack, 1}}))
		<< "多项时确定不足优先于版本错位";
	EXPECT_EQ(ItemStore::QuantityVerdict::kEnough, bag.CheckQuantities({{kStackable, 0}})) << "需求为 0 视为满足";
	EXPECT_EQ(kSuccess, bag.CheckItemsAvailable({{kStackable, 3}}));
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), bag.CheckItemsAvailable({{kStackable, 4}}));
	EXPECT_EQ(static_cast<uint32_t>(kBagInsufficientItems), bag.CheckItemsAvailable({{kStackable, 9}}));

	// 全或无的扣除:版本错位时整体失败、一个单位都不动。
	const BagState before = Capture(bag);
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), bag.RemoveItems({{kStackable, 4}}));
	ExpectUnchanged(before, bag);

	// 夹紧的扣除(战斗消耗)只从普通堆抽,带新字段的堆原样保留(未知字段也在)。
	std::vector<DrainedInstance> drained;
	ASSERT_EQ(kSuccess, bag.RemoveItemsClamped({{kStackable, 8}}, &drained));
	ASSERT_EQ(1u, drained.size());
	EXPECT_EQ(kGuidA, drained.front().guid);
	EXPECT_EQ(3u, drained.front().amount);
	EXPECT_EQ(nullptr, bag.GetItemCompByGuid(kGuidA)) << "普通堆抽光后成对销毁";
	const ItemComp *kept = bag.GetItemCompByGuid(kGuidB);
	ASSERT_NE(nullptr, kept);
	EXPECT_EQ(5u, kept->size());
	EXPECT_TRUE(unknown_field_carry::CarriesNewerVersionFields(*kept));
	EXPECT_TRUE(bag.IsLayerConsistent());
}

// ── 具名槽的还原:PutInstance 指定的槽位撑得过存档往返 ──────────────────────
//
// 部位 1 有 0 / 1 两个槽。PutInstance 让"落在同部位的哪个槽"成了编排层可指定的东西
// (穿上规则 = 都占着就替换槽号最小的那只),所以它必须随存档保留。

// 容器级:快照里的槽位只要今天仍被槽位表认可(接受该部位、在容量内)且空着,就原样落回;
// 不再认可或已被占,才退回"该部位第一个空槽"—— 位置可以变,物品不能丢。
TEST_F(BagInstanceDataTest, NamedSlotRestoreKeepsAnAcceptingSnapshotSlot)
{
	// 只戴在同部位的第二个槽:不许被挪到第一个空槽。
	Bag single;
	single.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	single.RestoreItems({{MakeInstance(kGuidA, kEquipA), kSlotKind1Second}});
	EXPECT_EQ(kSlotKind1Second, single.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kInvalidGuid, single.Layout().At(kSlotKind1First));
	EXPECT_TRUE(single.IsLayerConsistent());

	// 两个槽都戴着,还原顺序与槽号相反(存档按实例层遍历序写出,不保证按槽号)。
	Bag bothWorn;
	bothWorn.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	bothWorn.RestoreItems({{MakeInstance(kGuidB, kEquipB), kSlotKind1Second}});
	bothWorn.RestoreItems({{MakeInstance(kGuidA, kEquipA), kSlotKind1First}});
	EXPECT_EQ(kSlotKind1First, bothWorn.GetItemPosByGuid(kGuidA));
	EXPECT_EQ(kSlotKind1Second, bothWorn.GetItemPosByGuid(kGuidB));
	EXPECT_TRUE(bothWorn.IsLayerConsistent());

	// 快照槽位不再接受该部位(策划改了表):落到该部位的第一个空槽,原槽位保持空着。
	Bag fallback;
	fallback.SetProfile(BagProfile::Equipment(kEquipmentCapacity));
	fallback.RestoreItems({{MakeInstance(kGuidC, kEquipA), kSlotKind2}});
	EXPECT_EQ(kSlotKind1First, fallback.GetItemPosByGuid(kGuidC));
	EXPECT_EQ(kInvalidGuid, fallback.Layout().At(kSlotKind2));
	// 快照槽位接受该部位但已被占(两件声称同一个槽):后来的那件去下一个空槽,两件都在。
	fallback.RestoreItems({{MakeInstance(kGuidD, kEquipB), kSlotKind1First}});
	EXPECT_EQ(kSlotKind1First, fallback.GetItemPosByGuid(kGuidC)) << "the occupant is not displaced";
	EXPECT_EQ(kSlotKind1Second, fallback.GetItemPosByGuid(kGuidD));
	EXPECT_EQ(2u, fallback.OccupiedGridCount());
	EXPECT_TRUE(fallback.IsLayerConsistent());

	// 容量缩到只剩 0 号槽:1 号槽不在容量内,同样退回第一个空槽。
	Bag narrow;
	narrow.SetProfile(BagProfile::Equipment(1));
	narrow.RestoreItems({{MakeInstance(kGuidA, kEquipA), kSlotKind1Second}});
	EXPECT_EQ(kSlotKind1First, narrow.GetItemPosByGuid(kGuidA));
	EXPECT_TRUE(narrow.IsLayerConsistent());
}

// 端到端:PutInstance 指定槽 -> Marshal -> 字节 -> Unmarshal,槽号不变。
//   * 只戴在 1 号槽 —— 还原若仍取"该部位第一个空槽",重登后它会跑到 0 号槽;
//   * 两个槽都戴着 —— 后写出的那件会被挤到另一个槽,每往返一次两件互换。连续往返两次
//     是因为还原后的实例层遍历序与源相反,两种写出顺序都要过。
TEST_F(BagInstanceDataTest, WornSlotSurvivesTheMarshalRoundTrip)
{
	const ItemComp first = MakeRolledInstance(kGuidA, kEquipA);
	ItemComp second = MakeInstance(kGuidB, kEquipB);
	second.mutable_equip();

	{
		const auto source = NewPlayer();
		{
			auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
			ASSERT_EQ(kSuccess, bags.bags[kEquipment].PutInstance(first, kSlotKind1Second));
		}
		const auto dest = RoundTripBags(source);
		auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
		ASSERT_NE(nullptr, restored);
		Bag &equipment = restored->bags[kEquipment];
		ASSERT_EQ(1u, equipment.OccupiedGridCount());
		EXPECT_EQ(kSlotKind1Second, equipment.GetItemPosByGuid(kGuidA));
		EXPECT_EQ(kInvalidGuid, equipment.Layout().At(kSlotKind1First));
		ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, first));
		EXPECT_TRUE(equipment.IsLayerConsistent());
	}

	{
		const auto source = NewPlayer();
		{
			auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
			ASSERT_EQ(kSuccess, bags.bags[kEquipment].PutInstance(first, kSlotKind1First));
			ASSERT_EQ(kSuccess, bags.bags[kEquipment].PutInstance(second, kSlotKind1Second));
		}
		const auto once = RoundTripBags(source);
		const auto twice = RoundTripBags(once);
		for (const auto player : {once, twice})
		{
			auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
			ASSERT_NE(nullptr, restored);
			Bag &equipment = restored->bags[kEquipment];
			ASSERT_EQ(2u, equipment.OccupiedGridCount());
			EXPECT_EQ(kSlotKind1First, equipment.GetItemPosByGuid(kGuidA));
			EXPECT_EQ(kSlotKind1Second, equipment.GetItemPosByGuid(kGuidB));
			ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, first));
			ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, second));
			EXPECT_TRUE(equipment.IsLayerConsistent());
		}
	}
}

// ── 强化等级与已镶宝石(二期:EquipInstanceData.enhance_level / gems)──────────
//
// 它们没有自己的搬运代码:随 equip 段整份走(TakeInstance 的整份拷贝、bag_marshal 带
// presence 的 CopyFrom)。上面的用例经 MakeRolledInstance / ExpectSameEquip 已经顺带盖住了
// "掷过属性又强化过"的装备;这一节补的是只有新字段时的形态、脏数据原样保留、以及 presence。

// 穿脱搬运:只带强化等级与宝石(0 条随机属性)的实例,穿上再脱下一个字段都不变 ——
// 等级不被夹取,同孔位的重复记录不被去重,记录的先后不被重排。
TEST_F(BagInstanceDataTest, EnhanceLevelAndGemsTravelWithTakeAndPut)
{
	Bag inventory;
	Bag equipment;
	equipment.SetProfile(BagProfile::Equipment(kEquipmentCapacity));

	const ItemComp original = MakeForgedOnlyInstance(kGuidA, kEquipA);
	inventory.RestoreItems({{original, 2}});
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(inventory, original));

	// 穿上(指定同部位的第二个槽)。
	ItemComp carried;
	ASSERT_EQ(kSuccess, inventory.TakeInstance(kGuidA, carried));
	ASSERT_NO_FATAL_FAILURE(ExpectSameEquip(original, carried));
	ASSERT_EQ(kSuccess, equipment.PutInstance(carried, kSlotKind1Second));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, original));

	// 脱下。
	ItemComp takenOff;
	ASSERT_EQ(kSuccess, equipment.TakeInstance(kGuidA, takenOff));
	ASSERT_EQ(kSuccess, inventory.PutInstance(takenOff));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(inventory, original));

	// 逐条点名:不是"差不多一样",是连脏数据的形状都原样。
	const ItemComp *stored = inventory.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, stored);
	EXPECT_EQ(0, stored->equip().affixes_size());
	EXPECT_EQ(kOutOfTableEnhanceLevel, stored->equip().enhance_level())
		<< "the container never clamps the level; consumers do, at read time";
	ASSERT_EQ(3, stored->equip().gems_size()) << "nor does it de-duplicate sockets";
	EXPECT_EQ(6u, stored->equip().gems(0).socket()) << "nor reorder the records";
	EXPECT_EQ(kUnknownConfig, stored->equip().gems(0).gem_config_id());
	EXPECT_EQ(2u, stored->equip().gems(1).socket());
	EXPECT_EQ(2301u, stored->equip().gems(1).gem_config_id());
	EXPECT_EQ(6u, stored->equip().gems(2).socket());
	EXPECT_EQ(2102u, stored->equip().gems(2).gem_config_id());
	EXPECT_TRUE(inventory.IsLayerConsistent());
	EXPECT_TRUE(equipment.IsLayerConsistent());
}

// 存档往返:新字段在盘上记录(ItemEntry.equip)里,四种形态各自保持 ——
// 只有强化等级 / 只有宝石 / 有 equip 但全是默认值 / 没有 equip。
//
// proto3 的标量 0 不上线,所以"有 equip、等级 0、没有宝石、没有属性"在字节上是一段空的
// 子消息,它与"没有 equip"的区别只剩 presence。而 presence 正是「已初始化」的判据:
// 强化 / 镶嵌只收 has_equip() 的实例(docs/design/equipment-enhance-inlay.md §3.1),
// 读盘时把它弄丢,这件装备就再也强化不了;反过来凭空长出来,一件普通物品就再也并不了堆。
TEST_F(BagInstanceDataTest, MarshalRoundTripKeepsEnhanceLevelGemsAndPresence)
{
	ItemComp enhancedOnly = MakeInstance(kGuidA, kEquipA);
	enhancedOnly.mutable_equip()->set_enhance_level(kRolledEnhanceLevel);
	ItemComp gemsOnly = MakeInstance(kGuidB, kEquipB);
	AddGem(gemsOnly, /*socket=*/1, /*gemConfigId=*/2401);
	ItemComp allDefault = MakeInstance(kGuidC, kEquipA);
	allDefault.mutable_equip(); // 已初始化、未强化、未镶嵌
	const ItemComp uninitialised = MakeInstance(kGuidD, kEquipB); // 没有 equip 段
	const ItemComp forged = MakeForgedOnlyInstance(kGuidE, kEquipA);

	const auto source = NewPlayer();
	{
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
		bags.bags[kInventory].RestoreItems({{enhancedOnly, 0}});
		bags.bags[kInventory].RestoreItems({{allDefault, 1}});
		bags.bags[kInventory].RestoreItems({{uninitialised, 2}});
		bags.bags[kEquipment].RestoreItems({{gemsOnly, kSlotKind1Second}});
		bags.bags[kWarehouse].RestoreItems({{forged, 7}});
	}

	BagAllData wire;
	bag_marshal::Marshal(source, wire);
	// 真的过一遍编解码:新字段与 presence 都必须撑得过字节。
	std::string bytes;
	ASSERT_TRUE(wire.SerializeToString(&bytes));
	BagAllData parsed;
	ASSERT_TRUE(parsed.ParseFromString(bytes));
	ASSERT_EQ(5, parsed.items_size());

	// 盘上记录。
	const ItemEntry *enhancedEntry = FindEntry(parsed.items(), kGuidA);
	const ItemEntry *gemsEntry = FindEntry(parsed.items(), kGuidB);
	const ItemEntry *allDefaultEntry = FindEntry(parsed.items(), kGuidC);
	const ItemEntry *uninitialisedEntry = FindEntry(parsed.items(), kGuidD);
	const ItemEntry *forgedEntry = FindEntry(parsed.items(), kGuidE);
	ASSERT_NE(nullptr, enhancedEntry);
	ASSERT_NE(nullptr, gemsEntry);
	ASSERT_NE(nullptr, allDefaultEntry);
	ASSERT_NE(nullptr, uninitialisedEntry);
	ASSERT_NE(nullptr, forgedEntry);

	ASSERT_TRUE(enhancedEntry->has_equip());
	EXPECT_EQ(kRolledEnhanceLevel, enhancedEntry->equip().enhance_level());
	EXPECT_EQ(0, enhancedEntry->equip().gems_size());
	EXPECT_EQ(0, enhancedEntry->equip().affixes_size());

	ASSERT_TRUE(gemsEntry->has_equip());
	EXPECT_EQ(0u, gemsEntry->equip().enhance_level());
	ASSERT_EQ(1, gemsEntry->equip().gems_size());
	EXPECT_EQ(1u, gemsEntry->equip().gems(0).socket());
	EXPECT_EQ(2401u, gemsEntry->equip().gems(0).gem_config_id());

	ASSERT_TRUE(allDefaultEntry->has_equip()) << "all-default is still 'initialised'";
	EXPECT_EQ(0u, allDefaultEntry->equip().enhance_level());
	EXPECT_EQ(0, allDefaultEntry->equip().gems_size());
	EXPECT_EQ(0, allDefaultEntry->equip().affixes_size());

	EXPECT_FALSE(uninitialisedEntry->has_equip())
		<< "saving must not grow an equip section on an instance that has none";

	ASSERT_TRUE(forgedEntry->has_equip());
	EXPECT_EQ(forged.equip().SerializeAsString(), forgedEntry->equip().SerializeAsString());

	// 读回来。
	const auto dest = NewPlayer();
	bag_marshal::Unmarshal(dest, parsed);
	auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
	ASSERT_NE(nullptr, restored);
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kInventory], enhancedOnly));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kInventory], allDefault));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kInventory], uninitialised));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kEquipment], gemsOnly));
	ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(restored->bags[kWarehouse], forged));
	EXPECT_EQ(kSlotKind1Second, restored->bags[kEquipment].GetItemPosByGuid(kGuidB));
	EXPECT_EQ(7u, restored->bags[kWarehouse].GetItemPosByGuid(kGuidE));
	for (auto &bag : restored->bags)
	{
		EXPECT_TRUE(bag.IsLayerConsistent());
	}
}

// 跨区往返:去程与回程各是一次 Marshal -> 字节 -> Unmarshal(跨 zone 传送、同 zone 跨节点
// 换图、登录读盘走的都是 bag_marshal 这一对函数,没有第二条搬运路径)。穿在身上的、背包里的、
// 动态包里的强化等级与宝石连过两跳都不丢,也不被"规范化":超上限的等级、同孔位的重复记录
// 到了对面还是原样 —— 夹取与现查在消费侧,搬运不洗存档(契约 §5 不变量 2)。
TEST_F(BagInstanceDataTest, EnhanceLevelAndGemsSurviveACrossZoneRoundTrip)
{
	const ItemComp worn = MakeRolledInstance(kGuidA, kEquipA);
	const ItemComp inInventory = MakeForgedOnlyInstance(kGuidB, kEquipB);
	const ItemComp inDynamicBag = MakeRolledInstance(kGuidC, kEquipB);

	const auto home = NewPlayer();
	{
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(home);
		ASSERT_EQ(kSuccess, bags.bags[kEquipment].PutInstance(worn, kSlotKind1Second));
		bags.bags[kInventory].RestoreItems({{inInventory, 4}});
		bags.dynamicBags_[kDynamicBagId].RestoreItems({{inDynamicBag, 3}});
	}

	const auto away = RoundTripBags(home); // 去程
	const auto back = RoundTripBags(away); // 回程
	for (const auto player : {away, back})
	{
		auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
		ASSERT_NE(nullptr, restored);

		Bag &equipment = restored->bags[kEquipment];
		ASSERT_EQ(1u, equipment.OccupiedGridCount());
		ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(equipment, worn));
		EXPECT_EQ(kSlotKind1Second, equipment.GetItemPosByGuid(kGuidA));

		Bag &inventory = restored->bags[kInventory];
		ASSERT_EQ(1u, inventory.OccupiedGridCount());
		ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(inventory, inInventory));
		EXPECT_EQ(4u, inventory.GetItemPosByGuid(kGuidB));

		const auto dynamicIt = restored->dynamicBags_.find(kDynamicBagId);
		ASSERT_NE(restored->dynamicBags_.end(), dynamicIt);
		ASSERT_EQ(1u, dynamicIt->second.OccupiedGridCount());
		ASSERT_NO_FATAL_FAILURE(ExpectStoredInstance(dynamicIt->second, inDynamicBag));
		EXPECT_EQ(3u, dynamicIt->second.GetItemPosByGuid(kGuidC));

		for (auto &bag : restored->bags)
		{
			EXPECT_TRUE(bag.IsLayerConsistent());
		}
		EXPECT_TRUE(dynamicIt->second.IsLayerConsistent());
	}
}

// 强化 / 镶嵌是**原地改** equip 段:编排层按 guid 取到仓库里的那一份再写
// (docs/design/equipment-enhance-inlay.md §4.3)。这里钉容器这一侧的三件事:
//   * 改到的就是存盘会写出去的那一份,不是副本;
//   * 槽位与入包序号不因此变化 —— 它们不在实例数据里;
//   * 没有 equip 段的实例读新字段只读到默认值,而且"读"不会把 presence 读出来
//     (取下宝石对未初始化的实例就是这么读的,不特判)。
TEST_F(BagInstanceDataTest, InPlaceEquipEditIsWhatGetsSavedAndMovesNothing)
{
	const auto source = NewPlayer();
	uint64_t seqBefore = 0;
	{
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(source);
		Bag &inventory = bags.bags[kInventory];
		ItemComp initialised = MakeInstance(kGuidA, kEquipA);
		initialised.mutable_equip(); // 已初始化、未强化、未镶嵌
		inventory.RestoreItems({{initialised, 6}});
		inventory.RestoreItems({{MakeInstance(kGuidB, kPlainNonStack), 7}});

		// 两件都放好之后才取指针:实例指针在下一次 Insert / Erase(含整理)后失效。
		ItemComp *target = inventory.GetItemCompByGuid(kGuidA);
		ASSERT_NE(nullptr, target);
		ASSERT_TRUE(target->has_equip());
		seqBefore = target->acquire_seq();
		ASSERT_NE(0u, seqBefore);
		target->mutable_equip()->set_enhance_level(3);
		AddGem(*target, /*socket=*/0, /*gemConfigId=*/2101);

		EXPECT_EQ(6u, inventory.GetItemPosByGuid(kGuidA));
		EXPECT_EQ(seqBefore, target->acquire_seq());
		EXPECT_EQ(2u, inventory.OccupiedGridCount());
		EXPECT_TRUE(inventory.IsLayerConsistent());

		const ItemComp *plain = inventory.GetItemCompByGuid(kGuidB);
		ASSERT_NE(nullptr, plain);
		EXPECT_EQ(0u, plain->equip().enhance_level());
		EXPECT_EQ(0, plain->equip().gems_size());
		EXPECT_FALSE(plain->has_equip()) << "reading the default equip must not create presence";
	}

	const auto dest = RoundTripBags(source);
	auto *restored = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(dest);
	ASSERT_NE(nullptr, restored);
	Bag &inventory = restored->bags[kInventory];

	const ItemComp *saved = inventory.GetItemCompByGuid(kGuidA);
	ASSERT_NE(nullptr, saved);
	ASSERT_TRUE(saved->has_equip());
	EXPECT_EQ(3u, saved->equip().enhance_level());
	ASSERT_EQ(1, saved->equip().gems_size());
	EXPECT_EQ(0u, saved->equip().gems(0).socket());
	EXPECT_EQ(2101u, saved->equip().gems(0).gem_config_id());
	EXPECT_EQ(seqBefore, saved->acquire_seq());
	EXPECT_EQ(6u, inventory.GetItemPosByGuid(kGuidA));

	const ItemComp *plainSaved = inventory.GetItemCompByGuid(kGuidB);
	ASSERT_NE(nullptr, plainSaved);
	EXPECT_FALSE(plainSaved->has_equip());
	EXPECT_EQ(7u, inventory.GetItemPosByGuid(kGuidB));
	EXPECT_TRUE(inventory.IsLayerConsistent());
}

} // namespace
