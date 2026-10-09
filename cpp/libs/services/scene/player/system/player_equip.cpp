#include "player_equip.h"

#include <algorithm>
#include <cstddef>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/utils/random/random.h"
#include "thread_context/ecs_context.h"

#include "battle/system/player_battle.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_attribute.h"

#include "table/code/attributedimension_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"

#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"

namespace {

using equiprules::AffixTier;
using equiprules::EffectKind;

uint64_t GuidForLog(entt::entity player) {
	const auto* guid = tlsEcs.actorRegistry.try_get<Guid>(player);
	return guid != nullptr ? *guid : 0;
}

// 等级 / 职业的取法与 player_attribute.cpp 的同名函数同口径(那两个是它的文件内私有函数,
// 这里照 player_pet.cpp 的 OwnerLevel 的做法各留一份;改组件归属时三处一起改)。
uint32_t PlayerLevel(entt::entity player) {
	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	return (levelComp != nullptr && levelComp->level() > 0) ? levelComp->level() : 1;
}

uint32_t PlayerClassId(entt::entity player) {
	const auto* uint32Comp = tlsEcs.actorRegistry.try_get<PlayerUint32Comp>(player);
	return uint32Comp != nullptr ? uint32Comp->class_() : 0;
}

// 行指针只在下一次 Load() 之前有效:下面所有用法都是「现查现用」,不出函数、不进成员。
const ItemTable* ItemRow(uint32_t configId) {
	const auto [row, err] = ItemTableManager::Instance().FindByIdSilent(configId);
	return row;
}

const EquipAttributeTable* AttributeRow(uint32_t attrId) {
	const auto [row, err] = EquipAttributeTableManager::Instance().FindByIdSilent(attrId);
	return row;
}

// 该属性在「装备等级 = equipLevel」的这件装备上的取值区间:EquipAttributeCap 里 level <= equipLevel
// 的最大那一档。一档都不满足(或该属性压根没有上限行)返回 cap == 0,即「本装备上不可用」。
// 掷值、计入加成、tooltip 显示三处都从这里取上限 —— 上限只有这一个出处。
equiprules::ValueRange RangeFor(uint32_t attrId, uint32_t equipLevel) {
	const auto& capRows = EquipAttributeCapTableManager::Instance().GetByAttrId(attrId);
	std::vector<uint32_t> levels;
	levels.reserve(capRows.size());
	for (const auto* capRow : capRows) {
		levels.push_back(capRow->level());
	}
	const int tier = equiprules::PickLevelTier(levels, equipLevel);
	if (tier < 0) {
		return equiprules::ValueRange{};
	}
	const auto* capRow = capRows[static_cast<std::size_t>(tier)];
	equiprules::ValueRange range;
	range.minValue = capRow->min_value();
	range.cap = capRow->cap();
	return range;
}

// 一条随机属性「此刻算多少」:存档值按当前表上限夹取(设计文档 §5 不变量 2:改表下调上限立即
// 生效、不洗存档)。CollectBonus(进面板)与 FillItemDisplay(进 tooltip)都只从这里取,
// 两处口径因此不可能分叉。
struct AffixNow {
	uint64_t value{0};
	uint64_t cap{0};
};

// 该属性在这件装备的等级上没有任何上限档(表被改过)→ value = cap = 0:这条属性暂时不生效,
// tooltip 上显示 0;存档里的原值不动,表补回来就恢复。
AffixNow ResolveAffixNow(const EquipAffix& affix, const ItemComp& item, uint32_t equipLevel) {
	AffixNow now;
	const auto range = RangeFor(affix.attr_id(), equipLevel);
	if (range.cap == 0) {
		LOG_WARN << "[PlayerEquip] 随机属性在本装备等级没有上限档,按 0 计: item_guid=" << item.item_id()
				 << " config=" << item.config_id() << " attr_id=" << affix.attr_id()
				 << " equip_level=" << equipLevel << " stored_value=" << affix.value();
		return now;
	}
	now.cap = range.cap;
	now.value = std::min<uint64_t>(affix.value(), range.cap);
	return now;
}

// 一条 EquipAttribute 行的 effect / effect_param 能否被规则层接受。借 ApplyEffect 自己来判
// (往一份用完即弃的加成上加 1),「接受」因此只有规则层那一处定义。
// ApplyEffect 接不接受只看 effect / effect_param、与数值无关,所以这里的答案与 CollectBonus 里
// 真正累加时的返回值恒一致 —— FillItemDisplay 靠这一点保证「不计入加成的行也不显示」。
bool IsEffectAccepted(const EquipAttributeTable& row) {
	equiprules::EquipBonus scratch;
	return equiprules::ApplyEffect(scratch, row.effect(), row.effect_param(), 1);
}

// 与 Bag::TakeInstance / PutInstance 的前置条件同口径:只有「不可叠加、size == 1」的实例能整件搬。
// 写成本地谓词是为了把这类拒绝提前到任何修改之前(搬运原语自己还会再查一遍)。
bool IsCarriable(const ItemComp& item, const ItemTable& row) {
	return row.max_stack_size() == 1 && item.size() == 1;
}

// 生产随机源。equiprules::RollAffixes 约定 rand(n) 返回 [0, n) 的均匀整数、n >= 1,
// 形参必须是 64 位(池的权重和按 64 位累加)。
uint64_t RandBelow(uint64_t n) {
	return tlsRandom.Rand<uint64_t>(0, n - 1);
}

// 写入口统一前置:实体有效 / 未跨 zone 冻结 / 不在战斗(与 PetSystem、PlayerAttributeSystem 同口径,
// 只是冻结与战斗各有自己的 tip,玩家看得懂为什么换不了)。
uint32_t CheckWritable(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player)) {
		LOG_WARN << "[PlayerEquip] 拒绝: 跨 zone 冻结中 player_id=" << GuidForLog(player);
		return kEquipFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player)) {
		return kEquipInBattle;
	}
	return kSuccess;
}

// 一件实例此刻不在任何包里(回滚也没放回去)。这是资产丢失,不能静默:把整份实例(含全部随机属性)
// 打进日志,供人工按 guid 补偿。标签 [INSTANCE_LOST] 固定,便于告警规则按它匹配。
//
// 刻意用 LOG_ERROR 而不是 LOG_FATAL:muduo 的 FATAL 会 abort 整个 scene 进程,连带所有在线玩家
// 尚未落盘的状态 —— 为一件装备拖垮整台节点,损失只会更大。
void ReportLostInstance(const ItemComp& item, const char* where, entt::entity player) {
	LOG_ERROR << "[PlayerEquip][INSTANCE_LOST] 回滚失败,实例已不在任何背包里,需人工补偿: player_id="
			  << GuidForLog(player) << " where=" << where << " guid=" << item.item_id()
			  << " config=" << item.config_id() << " instance={ " << item.ShortDebugString() << " }";
}

// 回滚用:把一件已经从包里取出来的实例放回 preferredSlot;原槽放不回再让容器自动找位
// (位置可以变,物品不能丢)。两次都失败就报丢失。返回是否放回成功。
bool PutBack(Bag& bag, const ItemComp& item, uint32_t preferredSlot, const char* bagName,
			 entt::entity player) {
	if (preferredSlot != kInvalidU32Id && bag.PutInstance(item, preferredSlot) == kSuccess) {
		return true;
	}
	if (bag.PutInstance(item) == kSuccess) {
		LOG_ERROR << "[PlayerEquip] 回滚时原槽放不回,已改放到自动找到的位置: player_id=" << GuidForLog(player)
				  << " bag=" << bagName << " guid=" << item.item_id() << " config=" << item.config_id()
				  << " preferred_slot=" << preferredSlot;
		return true;
	}
	ReportLostInstance(item, bagName, player);
	return false;
}

// 装备栏变了:重算二级属性并推面板。kEquipmentChanged 走「按比例保持当前气血 / 法力」,
// 穿脱不能当治疗(设计文档 §5 不变量 4)。
void OnEquipmentChanged(entt::entity player) {
	PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kEquipmentChanged);
	PlayerAttributeSystem::PushPanel(player);
}

// 把一件穿着的装备累加进 out:基础属性(表)+ 随机属性(实例)。
void AccumulateWornItem(const ItemComp& item, uint64_t playerIdForLog, equiprules::EquipBonus& out) {
	const ItemTable* row = ItemRow(item.config_id());
	if (row == nullptr) {
		LOG_ERROR << "[PlayerEquip] 穿着的装备查不到 Item 行,本件不计加成: player_id=" << playerIdForLog
				  << " item_guid=" << item.item_id() << " config=" << item.config_id();
		return;
	}
	if (row->equip_kind() == 0) {
		// 装备栏里躺着一件表里已不是装备的东西:策划把这一行从装备改成了非装备,老存档经还原兜底
		// (Bag::InsertItemForRestore 的落位规则 ②)仍落回装备栏。FillItemDisplay 对非装备一行属性
		// 都不出,这里也整件不计 —— 加成与 tooltip 同口径,不会出现「面板有数、tooltip 没字」。
		LOG_ERROR << "[PlayerEquip] 穿着的物品在 Item 表里不是装备(equip_kind=0),本件不计加成: player_id="
				  << playerIdForLog << " item_guid=" << item.item_id() << " config=" << item.config_id();
		return;
	}

	for (const auto& base : row->base_attr()) {
		const EquipAttributeTable* attr = AttributeRow(base.base_attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] 基础属性指向不存在的 EquipAttribute,跳过: config=" << item.config_id()
					  << " attr_id=" << base.base_attr_id();
			continue;
		}
		if (!equiprules::ApplyEffect(out, attr->effect(), attr->effect_param(), base.base_attr_value())) {
			LOG_ERROR << "[PlayerEquip] 基础属性的 effect 不被规则层接受,跳过: config=" << item.config_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
		}
	}

	// has_equip() 为假时 equip() 返回默认实例(0 条),不必单独分支。
	for (const auto& affix : item.equip().affixes()) {
		if (!equiprules::IsRandomAffixTier(affix.tier())) {
			LOG_ERROR << "[PlayerEquip] 随机属性的颜色档非法,跳过: player_id=" << playerIdForLog
					  << " item_guid=" << item.item_id() << " attr_id=" << affix.attr_id()
					  << " tier=" << affix.tier();
			continue;
		}
		const EquipAttributeTable* attr = AttributeRow(affix.attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] 随机属性指向不存在的 EquipAttribute,跳过: player_id=" << playerIdForLog
					  << " item_guid=" << item.item_id() << " attr_id=" << affix.attr_id();
			continue;
		}
		const AffixNow now = ResolveAffixNow(affix, item, row->equip_level());
		if (!equiprules::ApplyEffect(out, attr->effect(), attr->effect_param(), now.value)) {
			LOG_ERROR << "[PlayerEquip] 随机属性的 effect 不被规则层接受,跳过: item_guid=" << item.item_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
		}
	}
}

}  // namespace

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

void PlayerEquipSystem::InstallItemInitializer() {
	// 每次调用都真的重装,不要加「已安装」静态标志:回调会被别处卸掉(单测夹具),有标志就再也装不回去。
	BagService::SetItemInstanceInitializer([](ItemComp& item) { PlayerEquipSystem::RollNewInstance(item); });
	const uint32_t problems = ValidateTables();
	LOG_INFO << "[PlayerEquip] 新铸实例初始化回调已安装; 装备表校验问题数=" << problems;
}

uint32_t PlayerEquipSystem::ValidateTables() {
	uint32_t problems = 0;

	// EquipAttribute:效果必须是规则层认识的;一级属性点必须指向真实存在的**角色池**维度。
	// rejectedCombatRows:被规则层拒绝的 effect = 5 行数。这样的行本来是要承载某一项战斗属性的
	// (effect_param 填越界了),下面「15 项各有显示行」那一段靠它不把同一个根因再计一遍。
	uint32_t rejectedCombatRows = 0;
	for (const auto& row : EquipAttributeTableManager::Instance().FindAll().data()) {
		if (!IsEffectAccepted(row)) {
			++problems;
			if (row.effect() == equiprules::ToRaw(EffectKind::kCombat)) {
				++rejectedCombatRows;
			}
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAttribute id=" << row.id() << " 的 effect=" << row.effect()
					  << " effect_param=" << row.effect_param() << " 不被规则层接受,这条属性不会生效";
			continue;
		}
		if (row.effect() != equiprules::ToRaw(EffectKind::kPrimaryPoint)) {
			continue;
		}
		const AttributeDimensionTable* dimension =
			AttributeDimensionTableManager::Instance().FindByIdSilent(row.effect_param()).first;
		if (dimension == nullptr) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAttribute id=" << row.id()
					  << " 是一级属性点,但 effect_param=" << row.effect_param()
					  << " 不是 AttributeDimension 里的维度,加的点不会进任何属性";
		} else if (!PlayerAttributeSystem::IsPlayerDimension(row.effect_param())) {
			// 维度存在但属于宝宝池(或它的池查不到):规则层照收、点数照记,只是角色的属性重算
			// 只读角色池维度 —— 后果与「维度不存在」完全相同,却不会有任何别的报错。
			// 「是不是角色池维度」问的就是属性重算自己用的那个判定,这里不另抄一份 owner_type 口径。
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAttribute id=" << row.id()
					  << " 是一级属性点,但 effect_param=" << row.effect_param() << " 这个维度(pool_id="
					  << dimension->pool_id() << ")不属于角色池,加的点不会进任何属性";
		}
	}

	// 战斗属性 1..15 每一项都要有显示行(effect = 5 且 effect_param = 该项、name 非空)。
	// 属性面板的 combat 区逐项用 DescribeCombatStat 取名字,取不到就整行不下发,而且那里刻意不打日志
	// (每次开面板都会刷一遍)—— 所以缺行只能在这里报。判据直接用 DescribeCombatStat:它就是面板决定
	// 「这一行出不出」的那个函数,不会出现这里说不缺、面板却少一行。
	// 同一根因只计一次:上面每一条被拒的 effect = 5 行本来就该是某一项的显示行,它已经报过;这里按项号
	// 从小到大,先让这些行各抵掉一项,抵完还缺的才计数、才打日志(「只报前者」)。被抵掉的是哪一项无从
	// 对号(越界的 effect_param 看不出原意)—— 修好被拒的行再校验一遍,真正还缺的项会原样报出来。
	uint32_t explainedByRejectedRows = rejectedCombatRows;
	for (uint32_t raw = equiprules::ToRaw(equiprules::CombatStat::kNone) + 1;
		 raw < equiprules::ToRaw(equiprules::CombatStat::kCount); ++raw) {
		if (!DescribeCombatStat(equiprules::ToCombatStat(raw)).name.empty()) {
			continue;
		}
		if (explainedByRejectedRows > 0) {
			--explainedByRejectedRows;
			continue;
		}
		++problems;
		LOG_ERROR << "[PlayerEquip] 表校验: 战斗属性 " << raw << " 在 EquipAttribute 里没有显示行(需要一行 effect="
				  << equiprules::ToRaw(EffectKind::kCombat) << "、effect_param=" << raw
				  << " 且 name 非空),属性面板的战斗属性区会少这一项";
	}

	// EquipAttributeCap:区间必须满足 1 <= min_value <= cap(规则层会夹取,但那已经不是策划填的数了)。
	for (const auto& row : EquipAttributeCapTableManager::Instance().FindAll().data()) {
		if (row.cap() == 0 || row.min_value() == 0 || row.min_value() > row.cap()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAttributeCap id=" << row.id() << " 的区间非法: min_value="
					  << row.min_value() << " cap=" << row.cap() << "(应满足 1 <= min_value <= cap)";
		}
	}

	// EquipAffixPool:池里的属性必须存在、权重 > 0,并且至少有一档上限(否则任何等级的装备都掷不出它)。
	for (const auto& row : EquipAffixPoolTableManager::Instance().FindAll().data()) {
		const bool attrExists = AttributeRow(row.attr_id()) != nullptr;
		if (!attrExists) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAffixPool id=" << row.id() << " 的 attr_id=" << row.attr_id()
					  << " 不在 EquipAttribute 里";
		}
		if (row.weight() == 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAffixPool id=" << row.id() << " 权重为 0,永远掷不出";
		}
		// 属性行都不存在时,它在上限表里必然也一档都没有(EquipAttributeCap.attr_id 有导表期外键):
		// 那是同一个根因,上面已经报过,不再多计一条。
		if (attrExists && EquipAttributeCapTableManager::Instance().GetByAttrId(row.attr_id()).empty()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAffixPool id=" << row.id() << " 的 attr_id=" << row.attr_id()
					  << " 在 EquipAttributeCap 里一档上限都没有,永远掷不出";
		}
	}

	// EquipAffixRule:条数区间不能倒挂,概率不能超过万分之一万。
	for (const auto& row : EquipAffixRuleTableManager::Instance().FindAll().data()) {
		if (row.blue_min() > row.blue_max()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAffixRule id=" << row.id() << " 的蓝属性条数倒挂: blue_min="
					  << row.blue_min() << " blue_max=" << row.blue_max() << "(规则层会按 blue_min 掷)";
		}
		if (row.pink_rate() > equiprules::kPermyriadBase || row.yellow_rate() > equiprules::kPermyriadBase) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipAffixRule id=" << row.id() << " 的概率超过 "
					  << equiprules::kPermyriadBase << ": pink_rate=" << row.pink_rate()
					  << " yellow_rate=" << row.yellow_rate() << "(万分比,超过即必出)";
		}
	}

	// Item:只看装备行。这几条都没有导表期外键(子消息列 / 指向非主键),只能在这里发现。
	for (const auto& row : ItemTableManager::Instance().FindAll().data()) {
		if (row.equip_kind() == 0) {
			continue;
		}
		if (row.max_stack_size() != 1) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: Item id=" << row.id() << " 是装备(equip_kind=" << row.equip_kind()
					  << ")但 max_stack_size=" << row.max_stack_size()
					  << ";可堆叠会把不同属性的实例并成一堆,这一行不会掷属性也穿不上";
		}
		if (row.affix_pool() != 0 &&
			EquipAffixPoolTableManager::Instance().GetByPoolId(row.affix_pool()).empty()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: Item id=" << row.id() << " 的 affix_pool=" << row.affix_pool()
					  << " 在 EquipAffixPool 里没有任何行,这件装备掷不出随机属性";
		}
		if (row.affix_rule() != 0 && !EquipAffixRuleTableManager::Instance().Exists(row.affix_rule())) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: Item id=" << row.id() << " 的 affix_rule=" << row.affix_rule()
					  << " 不在 EquipAffixRule 里,这件装备掷不出随机属性";
		}
		for (const auto& base : row.base_attr()) {
			// 属性行自身 effect 是否合法已在上面的 EquipAttribute 段报过,这里只查引用是否存在。
			if (AttributeRow(base.base_attr_id()) == nullptr) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: Item id=" << row.id() << " 的基础属性 base_attr_id="
						  << base.base_attr_id() << " 不在 EquipAttribute 里(用不到的槽要留空,不能填 0)";
			}
		}
	}

	return problems;
}

// ---------------------------------------------------------------------------
// 新铸实例
// ---------------------------------------------------------------------------

void PlayerEquipSystem::RollNewInstance(ItemComp& item) {
	if (item.has_equip()) {
		return;  // 已初始化过(哪怕 0 条):绝不重掷
	}
	const ItemTable* row = ItemRow(item.config_id());
	if (row == nullptr || row->equip_kind() == 0) {
		return;  // 不是装备:不置位,普通物品不该长出 equip 段
	}
	if (row->max_stack_size() != 1) {
		// 表配错的「可堆叠装备」:置位会让它从此不能并堆(ItemStore::CanStack 对 has_equip() 一律拒),
		// 掷了属性也会被并堆吃掉。按普通物品处理;启动表校验已经报过这一行。
		return;
	}

	// 先置位再掷:从这一行起,这件实例在任何路径上都不会被第二次初始化。
	EquipInstanceData* equip = item.mutable_equip();
	if (row->affix_pool() == 0 || row->affix_rule() == 0) {
		return;  // 策划配的「不掷」
	}

	const auto [ruleRow, ruleErr] = EquipAffixRuleTableManager::Instance().FindByIdSilent(row->affix_rule());
	const auto& poolRows = EquipAffixPoolTableManager::Instance().GetByPoolId(row->affix_pool());
	if (ruleRow == nullptr || poolRows.empty()) {
		LOG_ERROR << "[PlayerEquip] 掷属性所需的表行缺失,本件不带随机属性: item_guid=" << item.item_id()
				  << " config=" << item.config_id() << " affix_pool=" << row->affix_pool()
				  << " pool_rows=" << poolRows.size() << " affix_rule=" << row->affix_rule()
				  << " rule_found=" << (ruleRow != nullptr);
		return;
	}

	std::vector<equiprules::PoolEntry> pool;
	pool.reserve(poolRows.size());
	for (const auto* poolRow : poolRows) {
		if (AttributeRow(poolRow->attr_id()) == nullptr) {
			// 掷出一条查不到定义的属性 = 一条既不生效也显示不出来的死数据,宁可不让它进池。
			LOG_ERROR << "[PlayerEquip] 属性池里有不存在的属性,已排除: pool=" << row->affix_pool()
					  << " attr_id=" << poolRow->attr_id();
			continue;
		}
		equiprules::PoolEntry entry;
		entry.attrId = poolRow->attr_id();
		entry.weight = poolRow->weight();
		pool.push_back(entry);
	}

	equiprules::RollRule rule;
	rule.blueMin = ruleRow->blue_min();
	rule.blueMax = ruleRow->blue_max();
	rule.pinkRatePermyriad = ruleRow->pink_rate();
	rule.yellowRatePermyriad = ruleRow->yellow_rate();

	const uint32_t equipLevel = row->equip_level();
	const auto rolled = equiprules::RollAffixes(
		pool, rule, [](uint64_t n) { return RandBelow(n); },
		[equipLevel](uint32_t attrId) { return RangeFor(attrId, equipLevel); });
	for (const auto& one : rolled) {
		EquipAffix* affix = equip->add_affixes();
		affix->set_attr_id(one.attrId);
		affix->set_tier(equiprules::ToRaw(one.tier));
		affix->set_value(one.value);
		affix->set_seq(one.seq);
	}
}

// ---------------------------------------------------------------------------
// 写入口
// ---------------------------------------------------------------------------

uint32_t PlayerEquipSystem::Equip(entt::entity player, Guid itemId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}
	Bag& inventory = bags->bags[kInventory];
	Bag& equipment = bags->bags[kEquipment];

	// ---- 纯校验:以下任何一条失败,两个包都没有动过 ----
	const ItemComp* item = inventory.GetItemCompByGuid(itemId);
	if (item == nullptr) {
		return kEquipItemNotFound;
	}
	const uint32_t configId = item->config_id();
	const ItemTable* row = ItemRow(configId);
	if (row == nullptr || row->equip_kind() == 0 || row->max_stack_size() != 1) {
		return kEquipNotEquipment;  // 可堆叠的「装备」是表配错,搬运原语不搬它,按非装备拒
	}
	if (!IsCarriable(*item, *row)) {
		return kEquipItemNotFound;  // size != 1:被扣光还没回收的空实例,对玩家而言这件东西不存在
	}
	if (PlayerLevel(player) < row->equip_level()) {
		return kEquipLevelNotEnough;
	}
	if (row->equip_class() != 0 && row->equip_class() != PlayerClassId(player)) {
		return kEquipClassMismatch;
	}
	const std::vector<uint32_t> slots = equipment.SlotsAcceptingKind(row->equip_kind());
	if (slots.empty()) {
		return kEquipNoSlot;
	}

	// 选槽:槽号升序里第一个空槽;都占着就替换槽号最小的那个。
	// 选定的槽显式传给 PutInstance —— 自动找位取的是表行序里的第一个空槽,不保证是槽号最小的。
	uint32_t targetSlot = slots.front();
	Guid replacedGuid = equipment.Layout().At(targetSlot);
	for (const uint32_t slot : slots) {
		if (equipment.Layout().At(slot) == kInvalidGuid) {
			targetSlot = slot;
			replacedGuid = kInvalidGuid;
			break;
		}
	}
	const bool replacing = replacedGuid != kInvalidGuid;
	if (replacing) {
		// 要被换下来的那件也得搬得动,否则换到一半才发现。
		const ItemComp* occupant = equipment.GetItemCompByGuid(replacedGuid);
		const ItemTable* occupantRow = occupant != nullptr ? ItemRow(occupant->config_id()) : nullptr;
		if (occupant == nullptr || occupantRow == nullptr || !IsCarriable(*occupant, *occupantRow)) {
			LOG_ERROR << "[PlayerEquip] 目标槽的占用者无法搬运,拒绝替换: player_id=" << GuidForLog(player)
					  << " slot=" << targetSlot << " occupant_guid=" << replacedGuid
					  << " occupant_in_store=" << (occupant != nullptr)
					  << " occupant_row_found=" << (occupantRow != nullptr);
			return kEquipInternalError;
		}
	}
	const uint32_t inventorySlot = inventory.GetItemPosByGuid(itemId);

	// ---- 搬运:每一步失败都按相反顺序放回。预检已经排除了全部数据可触发的原因,
	//      走进任何一个失败分支都是两层状态不自洽的编程错误 ----
	ItemComp incoming;
	if (const auto err = inventory.TakeInstance(itemId, incoming); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 穿上失败(从背包取出): player_id=" << GuidForLog(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		return kEquipInternalError;  // TakeInstance 失败零副作用,无需回滚
	}

	ItemComp replaced;
	if (replacing) {
		if (const auto err = equipment.TakeInstance(replacedGuid, replaced); err != kSuccess) {
			LOG_ERROR << "[PlayerEquip] 穿上失败(取下旧装备): player_id=" << GuidForLog(player)
					  << " guid=" << itemId << " config=" << configId << " replaced_guid=" << replacedGuid
					  << " slot=" << targetSlot << " err=" << err;
			PutBack(inventory, incoming, inventorySlot, "inventory", player);
			return kEquipInternalError;
		}
	}

	if (const auto err = equipment.PutInstance(incoming, targetSlot); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 穿上失败(放进装备栏): player_id=" << GuidForLog(player) << " guid=" << itemId
				  << " config=" << configId << " slot=" << targetSlot << " err=" << err;
		// 回滚时只有「装备栏一侧放不回去」才改变了穿戴:那件旧装备已不在装备栏(丢失由 PutBack 报告),
		// 面板必须跟上,否则数值里还留着它的加成。放回成功哪怕换了槽,加成都不变,不必重算;
		// 人物背包一侧放不回去不影响穿戴。Equip 的两处回滚与 Unequip 的一处都按这一条处理。
		bool equipmentRestored = true;
		if (replacing) {
			equipmentRestored = PutBack(equipment, replaced, targetSlot, "equipment", player);
		}
		PutBack(inventory, incoming, inventorySlot, "inventory", player);
		if (!equipmentRestored) {
			OnEquipmentChanged(player);
		}
		return kEquipInternalError;
	}

	if (replacing) {
		// 新装备刚从背包腾出一格,旧装备必然放得下;不指定槽,让背包自己找位。
		if (const auto err = inventory.PutInstance(replaced); err != kSuccess) {
			LOG_ERROR << "[PlayerEquip] 穿上失败(旧装备回背包): player_id=" << GuidForLog(player)
					  << " guid=" << itemId << " config=" << configId << " replaced_guid=" << replacedGuid
					  << " replaced_config=" << replaced.config_id() << " err=" << err;
			ItemComp undo;
			if (equipment.TakeInstance(itemId, undo) == kSuccess) {
				const bool equipmentRestored = PutBack(equipment, replaced, targetSlot, "equipment", player);
				PutBack(inventory, undo, inventorySlot, "inventory", player);
				if (!equipmentRestored) {
					OnEquipmentChanged(player);  // 旧装备没能回到装备栏:穿戴变了,见上一处回滚的说明
				}
			} else {
				// 连新装备都取不下来:它留在装备栏(没有丢),而旧装备的原槽被它占着、背包又刚拒收,
				// 已经无处可放。装备栏确实变了,面板必须跟上,否则数值与实际穿戴分叉。
				ReportLostInstance(replaced, "equip-replace", player);
				OnEquipmentChanged(player);
			}
			return kEquipInternalError;
		}
	}

	OnEquipmentChanged(player);
	LOG_INFO << "[PlayerEquip] 穿上: player_id=" << GuidForLog(player) << " guid=" << itemId
			 << " config=" << configId << " slot=" << targetSlot << " replaced_guid="
			 << (replacing ? replacedGuid : 0);
	return kSuccess;
}

uint32_t PlayerEquipSystem::Unequip(entt::entity player, Guid itemId) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}
	Bag& inventory = bags->bags[kInventory];
	Bag& equipment = bags->bags[kEquipment];

	// ---- 纯校验 ----
	const ItemComp* item = equipment.GetItemCompByGuid(itemId);
	if (item == nullptr) {
		return kEquipNotEquipped;
	}
	const uint32_t configId = item->config_id();
	const ItemTable* row = ItemRow(configId);
	if (row == nullptr || !IsCarriable(*item, *row)) {
		// 穿着的东西查不到表 / 变成了可堆叠 / size 不是 1:数据或表坏了,搬运原语会拒,这里先说清楚。
		LOG_ERROR << "[PlayerEquip] 穿着的装备无法搬运,拒绝卸下: player_id=" << GuidForLog(player)
				  << " guid=" << itemId << " config=" << configId << " row_found=" << (row != nullptr)
				  << " size=" << item->size();
		return kEquipInternalError;
	}
	// 「背包满」是玩家随手就能触发的普通拒绝,先用不打日志的格子数判定挡掉 —— CheckSpaceFor 失败
	// 会打 ERROR 加调用栈,不适合承担这条常规路径。人物背包是自由格布局,「还有没有一格」就是完整答案。
	if (inventory.IsSpaceInsufficient(1)) {
		return kEquipBagFull;
	}
	// 其余会让 PutInstance 拒绝的条件(准入不收、表行异常)用与它同源的预检在动手之前问清楚。
	ItemCountMap needed;
	needed[configId] = 1;
	if (const auto err = inventory.CheckSpaceFor(needed); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 人物背包有空格却不收这件装备,拒绝卸下: player_id=" << GuidForLog(player)
				  << " guid=" << itemId << " config=" << configId << " err=" << err;
		return kEquipInternalError;
	}
	const uint32_t wornSlot = equipment.GetItemPosByGuid(itemId);

	// ---- 搬运 ----
	ItemComp carried;
	if (const auto err = equipment.TakeInstance(itemId, carried); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 卸下失败(从装备栏取出): player_id=" << GuidForLog(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		return kEquipInternalError;  // 零副作用,无需回滚
	}
	if (const auto err = inventory.PutInstance(carried); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 卸下失败(放回背包): player_id=" << GuidForLog(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		if (!PutBack(equipment, carried, wornSlot, "equipment", player)) {
			// 没能放回装备栏:这件装备已不在身上(丢失由 PutBack 报告),面板必须跟上。
			OnEquipmentChanged(player);
		}
		return kEquipInternalError;
	}

	OnEquipmentChanged(player);
	LOG_INFO << "[PlayerEquip] 卸下: player_id=" << GuidForLog(player) << " guid=" << itemId
			 << " config=" << configId << " slot=" << wornSlot;
	return kSuccess;
}

uint32_t PlayerEquipSystem::GmGrantItem(entt::entity player, uint32_t configId, uint32_t count) {
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	if (configId == 0 || count == 0 || count > kGmGrantMaxCount || ItemRow(configId) == nullptr) {
		return kEquipGrantInvalid;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}

	ItemCountMap items;
	items[configId] = count;
	const PlayerItemBlockList emptyBlockList;
	const auto* blockList = tlsEcs.actorRegistry.try_get<PlayerItemBlockList>(player);
	// 走正式发放入口:封禁闸、获得流水(TX_GM_GRANT,审计能一眼分出「GM 发的」)、异常检测都照常过;
	// 新装备的掷属性挂在这条路上,不需要在这里再调 RollNewInstance。
	const auto err = BagService::AddItems(player, bags->bags[kInventory],
										  blockList == nullptr ? emptyBlockList : *blockList, items, TX_GM_GRANT);
	LOG_INFO << "[PlayerEquip] GM 发物: player_id=" << GuidForLog(player) << " config=" << configId
			 << " count=" << count << " err=" << err;
	return err;
}

// ---------------------------------------------------------------------------
// 只读
// ---------------------------------------------------------------------------

void PlayerEquipSystem::CollectBonus(entt::entity player, equiprules::EquipBonus& out) {
	out = equiprules::EquipBonus{};
	if (!tlsEcs.actorRegistry.valid(player)) {
		return;
	}
	// try_get:属性重算路径,绝不在这里造组件。
	const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return;
	}
	const uint64_t playerId = GuidForLog(player);
	bags->bags[kEquipment].ForEachItem(
		[&out, playerId](Guid, const ItemComp& item) { AccumulateWornItem(item, playerId, out); });
}

void PlayerEquipSystem::FillItemDisplay(const ItemComp& item, ::BagItemInfo& info) {
	const ItemTable* row = ItemRow(item.config_id());
	if (row == nullptr) {
		return;
	}
	info.set_name(row->name());
	info.set_description(row->description());
	info.set_icon_key(row->icon_key());
	if (row->equip_kind() == 0) {
		return;
	}

	info.set_equip_level(row->equip_level());
	info.set_equip_class(row->equip_class());
	// equip_class_name 留空:Class 表目前没有名称列(data/schema/class_table.proto)。
	// 以后加了名称列,在这里按 row->equip_class() 查表填上即可,协议不用动。

	uint32_t baseSeq = 0;
	for (const auto& base : row->base_attr()) {
		const EquipAttributeTable* attr = AttributeRow(base.base_attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] 基础属性指向不存在的 EquipAttribute,不显示: config=" << item.config_id()
					  << " attr_id=" << base.base_attr_id();
			continue;
		}
		if (!IsEffectAccepted(*attr)) {
			// CollectBonus 不会把这一条计入加成(ApplyEffect 拒绝),tooltip 上也就不能出现它的数值。
			LOG_ERROR << "[PlayerEquip] 基础属性的 effect 不被规则层接受,不显示: config=" << item.config_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
			continue;
		}
		auto* line = info.add_base_attrs();
		line->set_attr_id(attr->id());
		line->set_name(attr->name());
		line->set_tier(equiprules::ToRaw(AffixTier::kBase));
		line->set_value(base.base_attr_value());
		line->set_cap(0);  // 基础属性没有「/上限」
		line->set_percent(attr->percent() != 0);
		line->set_seq(baseSeq++);
	}

	// 显示顺序 = (tier, seq);repeated 的下标不承载语义,所以这里排好再下发。
	const auto& affixes = item.equip().affixes();
	std::vector<int> order;
	order.reserve(static_cast<std::size_t>(affixes.size()));
	for (int i = 0; i < affixes.size(); ++i) {
		if (!equiprules::IsRandomAffixTier(affixes.Get(i).tier())) {
			LOG_ERROR << "[PlayerEquip] 随机属性的颜色档非法,不显示: item_guid=" << item.item_id()
					  << " attr_id=" << affixes.Get(i).attr_id() << " tier=" << affixes.Get(i).tier();
			continue;
		}
		order.push_back(i);
	}
	std::stable_sort(order.begin(), order.end(), [&affixes](int lhs, int rhs) {
		const auto& left = affixes.Get(lhs);
		const auto& right = affixes.Get(rhs);
		if (left.tier() != right.tier()) {
			return left.tier() < right.tier();
		}
		return left.seq() < right.seq();
	});
	for (const int index : order) {
		const auto& affix = affixes.Get(index);
		const EquipAttributeTable* attr = AttributeRow(affix.attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] 随机属性指向不存在的 EquipAttribute,不显示: item_guid=" << item.item_id()
					  << " attr_id=" << affix.attr_id();
			continue;
		}
		if (!IsEffectAccepted(*attr)) {
			// 同基础属性:不计入加成的行不显示。
			LOG_ERROR << "[PlayerEquip] 随机属性的 effect 不被规则层接受,不显示: item_guid=" << item.item_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
			continue;
		}
		const AffixNow now = ResolveAffixNow(affix, item, row->equip_level());
		auto* line = info.add_affixes();
		line->set_attr_id(attr->id());
		line->set_name(attr->name());
		line->set_tier(affix.tier());
		line->set_value(now.value);
		line->set_cap(now.cap);
		line->set_percent(attr->percent() != 0);
		line->set_seq(affix.seq());
	}
}

void PlayerEquipSystem::FillEquipSlots(::BagLayoutInfo& layout) {
	// 槽号 >= 容量的槽位行不下发:与 Bag::SlotsAcceptingKind 的「在容量内」同口径,
	// 客户端画出来的每一个槽都必须是真能穿上东西的槽。
	std::vector<uint32_t> slotIds;
	for (const auto& row : EquipSlotTableManager::Instance().FindAll().data()) {
		if (row.id() < layout.capacity()) {
			slotIds.push_back(row.id());
		}
	}
	// 升序是协议契约,不依赖表的行序;去重防脏表里同一个槽号出现两行。
	std::sort(slotIds.begin(), slotIds.end());
	slotIds.erase(std::unique(slotIds.begin(), slotIds.end()), slotIds.end());
	for (const uint32_t slotId : slotIds) {
		const auto [row, err] = EquipSlotTableManager::Instance().FindByIdSilent(slotId);
		if (row == nullptr) {
			continue;
		}
		// 没有部位名的槽 = 不对玩家展示的槽(EquipSlot 表里 0–2 号是单测夹具槽,name 留空):空着就不下发,
		// 否则客户端会画出几个没有名字、也没有任何正式装备能穿的空格子。判据是表里的 name,不是槽号。
		// 但它一旦被占着就照常下发(name 为空,显示什么由客户端兜底)—— 身上穿着的每一件都必须有槽可画,
		// 否则这件装备在客户端无处显示,也就点不到「卸下」。占用关系读调用方已填好的 layout.slots。
		if (row->name().empty()) {
			const auto& placed = layout.slots();
			const bool occupied = std::any_of(placed.begin(), placed.end(), [slotId](const ::BagSlotInfo& one) {
				return one.slot() == slotId;
			});
			if (!occupied) {
				continue;
			}
		}
		auto* slot = layout.add_equip_slots();
		slot->set_slot(slotId);
		slot->set_equip_kind(row->equip_kind());
		slot->set_name(row->name());
	}
}

PlayerEquipSystem::CombatStatDisplay PlayerEquipSystem::DescribeCombatStat(equiprules::CombatStat stat) {
	CombatStatDisplay display;
	const uint32_t rawStat = equiprules::ToRaw(stat);
	if (equiprules::ToCombatStat(rawStat) == equiprules::CombatStat::kNone) {
		return display;  // kNone / kCount:不是一项真实的战斗属性
	}
	const EquipAttributeTable* best = nullptr;
	for (const auto& row : EquipAttributeTableManager::Instance().FindAll().data()) {
		if (row.effect() != equiprules::ToRaw(EffectKind::kCombat) || row.effect_param() != rawStat) {
			continue;
		}
		if (best == nullptr || row.sort() < best->sort() ||
			(row.sort() == best->sort() && row.id() < best->id())) {
			best = &row;
		}
	}
	if (best != nullptr) {
		display.name = best->name();
		display.percent = best->percent() != 0;
		display.sort = best->sort();
	}
	return display;
}

bool PlayerEquipSystem::IsEquipment(uint32_t configId) {
	const ItemTable* row = ItemRow(configId);
	return row != nullptr && row->equip_kind() != 0;
}
