#include "player_equip.h"
#include "engine/core/type_define/player_guid_comp.h"

#include <algorithm>
#include <cstddef>
#include <functional>
#include <limits>
#include <map>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/utils/random/random.h"
#include "thread_context/ecs_context.h"

#include "battle/system/player_battle.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_store.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_attribute.h"

#include "table/code/attributedimension_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/equip_error_tip.pb.h"

#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_bag.pb.h"

namespace {

using equiprules::AffixTier;
using equiprules::EffectKind;


// 等级 / 职业的取法与 player_attribute.cpp 的同名函数同口径(那两个是它的文件内私有函数,
// 这里照 player_pet.cpp 的 OwnerLevel 的做法各留一份;改组件归属时三处一起改)。
// 等级按 playerlevel::CappedLevel 生效(超上限的存储等级按上限,0 按 1,player_level_rules.h);缺组件仍按 1 级。
uint32_t PlayerLevel(entt::entity player) {
	const auto* levelComp = tlsEcs.actorRegistry.try_get<LevelComp>(player);
	return levelComp != nullptr ? playerlevel::CappedLevel(levelComp->level()) : 1;
}

uint32_t PlayerClassId(entt::entity player) {
	const auto* uint32Comp = tlsEcs.actorRegistry.try_get<PlayerUint32Comp>(player);
	return uint32Comp != nullptr ? uint32Comp->class_() : 0;
}

// 表行借用只在下一次 Load() 之前有效:下面所有用法都是「现查现用」,不跨请求保存。
const ItemTable* ItemRow(uint32_t configId) {
	const auto [row, err] = ItemTableManager::Instance().FindByIdSilent(configId);
	return row;
}

const EquipAttributeTable* AttributeRow(uint32_t attrId) {
	const auto [row, err] = EquipAttributeTableManager::Instance().FindByIdSilent(attrId);
	return row;
}

// EquipEnhance 第 level 行 = 「从 +(level − 1) 强化到 +level」的规则,以及 +level 的总加成。
const EquipEnhanceTable* EnhanceRow(uint32_t level) {
	const auto [row, err] = EquipEnhanceTableManager::Instance().FindByIdSilent(level);
	return row;
}

// EquipGem 表的原始查找:只回答「表里有没有这一行」。消费侧(已嵌在装备上的宝石算不算数)用它;
// 「这个 config 能不能当宝石嵌进去」是另一个更严的判据,见 GemRowIfGem。
const EquipGemTable* GemRow(uint32_t gemConfigId) {
	const auto [row, err] = EquipGemTableManager::Instance().FindByIdSilent(gemConfigId);
	return row;
}

// 「是不是宝石」的唯一判据(PlayerEquipSystem::IsGem 只转调这里):EquipGem 表有这一行,且它的 Item 行存在、
// 不是装备。是宝石返回宝石行,否则 nullptr。
// Item 行那一半不能省:镶嵌 / 取下按 config 扣发数量、不看 has_equip() —— config 指向装备的话,嵌进去吃掉的是
// 一件装备实例,取下来铸出的是一件重新掷属性的新装备(二期 §5 不变量 9)。表校验只打日志,这里是运行期那一道。
const EquipGemTable* GemRowIfGem(uint32_t configId) {
	const EquipGemTable* gemRow = GemRow(configId);
	if (gemRow == nullptr) {
		return nullptr;
	}
	const ItemTable* itemRow = ItemRow(configId);
	return (itemRow != nullptr && itemRow->equip_kind() == 0) ? gemRow : nullptr;
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
		LOG_WARN << "[PlayerEquip] No random attribute cap tier for this equipment level;using 0: item_guid=" << item.item_id()
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

// ---------------------------------------------------------------------------
// 强化 / 宝石「此刻算多少」(二期 = docs/design/equipment-enhance-inlay.md,§4.3 消费侧)
//
// 与 ResolveAffixNow 同一个纪律:CollectBonus(进面板 / 进战斗)与 FillItemDisplay(进 tooltip)只从这一组
// 函数取值,两处口径因此不可能分叉。入库的只有「+几」和「哪个孔嵌了哪种宝石」,数值每次按表现查。
//
// **这一组全部静默**:它们在每次属性重算、每次开包(每件物品一次)都跑,入包边界又整份信任 equip 段 ——
// 为一条不合格的记录打日志,策划删一行表就是全服日志洪泛。不合格的只跳过;写入口碰到时才打 WARN。
// ---------------------------------------------------------------------------

// 有效强化等级:存档值按表上限夹取(改表下调上限立即生效、不洗存档;表补回来即恢复)。
// 未初始化的实例(!has_equip())恒为 0。
uint32_t EffectiveEnhanceLevel(const ItemComp& item) {
	if (!item.has_equip() || item.equip().enhance_level() == 0) {
		return 0;
	}
	return std::min<uint32_t>(item.equip().enhance_level(), PlayerEquipSystem::MaxEnhanceLevel());
}

// 强化到 level 级后基础属性的总加成(万分比)。0 级 = 0;缺行按 0(level 来自 EffectiveEnhanceLevel 时
// 一定在连续等级内,缺行只可能是两次查表之间换了表)。
uint32_t EnhanceBonusPermyriad(uint32_t level) {
	if (level == 0) {
		return 0;
	}
	const EquipEnhanceTable* row = EnhanceRow(level);
	return row != nullptr ? row->bonus_permyriad() : 0;
}

// 一条基础属性「此刻算多少」:表值按万分比放大。bonusPermyriad == 0 严格恒等(未强化的装备与一期逐位一致)。
// 强化只放大基础属性(二期 §1.4 E1);要改成也放大随机属性 / 宝石,改的是调用它的地方,不是这里。
uint64_t ResolveBaseNow(uint64_t tableValue, uint32_t bonusPermyriad) {
	return equiprules::ScaleByPermyriad(tableValue, bonusPermyriad);
}

// 某部位的孔位行(EquipGemSocket 表里 equip_kind 相同的全部行),按 socket 升序;同号保持表内行序
// (脏表里同一个孔有两行时,排在前面的那一行算数)。孔位是数据:加孔只加行,这里不认识任何具体部位。
std::vector<const EquipGemSocketTable*> SocketRowsFor(uint32_t equipKind) {
	const auto& indexed = EquipGemSocketTableManager::Instance().GetByEquipKind(equipKind);
	std::vector<const EquipGemSocketTable*> rows(indexed.begin(), indexed.end());
	std::stable_sort(rows.begin(), rows.end(), [](const EquipGemSocketTable* lhs, const EquipGemSocketTable* rhs) {
		return lhs->socket() < rhs->socket();
	});
	return rows;
}

// 同上,翻译成规则层的输入(equiprules::CheckGemFit 对同号的多条定义取第一条)。
std::vector<equiprules::GemSocketRule> SocketRulesFor(uint32_t equipKind) {
	std::vector<equiprules::GemSocketRule> rules;
	for (const EquipGemSocketTable* row : SocketRowsFor(equipKind)) {
		equiprules::GemSocketRule rule;
		rule.socket = row->socket();
		rule.gemType = row->gem_type();
		rules.push_back(rule);
	}
	return rules;
}

// 实例上占着 socket 这个孔的那条记录:**原始记录**里的第一条,不经任何过滤;没有返回 nullptr。
// 「孔位是否占用」服务器(镶嵌 / 取下)与客户端(gem_sockets 的 gem_config_id)都只看它,与「是否生效」
// 是两件事(二期 §5 不变量 3)。返回的指针指向 item 内部,随 item 一起失效。
//
// 唯一的例外:gem_config_id == 0 的记录**视同不存在**(镶嵌的占用判定、ResolveGemsNow 同口径)。协议里
// gem_config_id == 0 就是「空孔」,这种记录没有东西可退,当成占用的话这个孔对玩家永久作废(服务器说已占、
// 客户端显示空孔、取下必败)。它只可能来自外来的 equip 段或存档损坏,镶嵌入口自己写不出 0。
const EquipGem* FirstGemRecordOn(const ItemComp& item, uint32_t socket) {
	for (const auto& gem : item.equip().gems()) {
		if (gem.socket() == socket && gem.gem_config_id() != 0) {
			return &gem;
		}
	}
	return nullptr;
}

// 一颗此刻计入加成的宝石。attr 是已校验的非空表行借用,现查现用(不出调用它的那个函数)。
struct ResolvedGem {
	uint32_t socket{0};
	uint32_t gemConfigId{0};
	std::reference_wrapper<const EquipAttributeTable> attr;
	uint64_t value{0};
};

// 一件装备上此刻生效的宝石,按 socket 升序。row 是这件装备的 Item 行。现查规则:
//   * 只看实例上前 kMaxGemSockets 条记录(遍历长度不受存档里的条数摆布);
//   * 先按 socket 取第一条,再对这一条判;第一条不合格则该 socket 整个不计,**不回退**到同孔的后一条
//     (否则「占着孔的是 A、算数的却是 B」,取下时退回 A、B 被无声清掉);
//   * 合格 = ① EquipGem 表有该宝石 ② 该 socket 在这件装备部位的孔位定义里 ③ 宝石的属性行存在且 effect
//     被规则层接受。**不**复核宝石类型与孔位类型:类型是嵌入时的规则,策划事后改孔位类型不让已嵌的宝石失效。
// has_equip() 为假时 equip() 是默认实例(0 条),自然返回空。
std::vector<ResolvedGem> ResolveGemsNow(const ItemComp& item, const ItemTable& row) {
	std::vector<ResolvedGem> resolved;
	const auto& gems = item.equip().gems();
	if (gems.empty()) {
		return resolved;  // 绝大多数装备没有宝石:不为它们去查孔位表
	}
	const std::vector<equiprules::GemSocketRule> sockets = SocketRulesFor(row.equip_kind());
	const int limit = std::min<int>(gems.size(), static_cast<int>(equiprules::kMaxGemSockets));
	std::vector<uint32_t> seenSockets;
	seenSockets.reserve(static_cast<std::size_t>(limit));
	for (int i = 0; i < limit; ++i) {
		const EquipGem& gem = gems.Get(i);
		if (gem.gem_config_id() == 0) {
			continue;  // 视同不存在(见 FirstGemRecordOn):不占「该孔第一条」的位置
		}
		if (std::find(seenSockets.begin(), seenSockets.end(), gem.socket()) != seenSockets.end()) {
			continue;  // 同孔的后续记录:不管第一条合不合格都不看
		}
		seenSockets.push_back(gem.socket());

		const EquipGemTable* gemRow = GemRow(gem.gem_config_id());
		if (gemRow == nullptr) {
			continue;
		}
		const bool socketDefined =
			std::any_of(sockets.begin(), sockets.end(),
						[&gem](const equiprules::GemSocketRule& rule) { return rule.socket == gem.socket(); });
		if (!socketDefined) {
			continue;
		}
		const EquipAttributeTable* attr = AttributeRow(gemRow->attr_id());
		if (attr == nullptr || !IsEffectAccepted(*attr)) {
			continue;
		}
		resolved.push_back(ResolvedGem{gem.socket(), gem.gem_config_id(), std::cref(*attr), gemRow->value()});
	}
	std::sort(resolved.begin(), resolved.end(),
			  [](const ResolvedGem& lhs, const ResolvedGem& rhs) { return lhs.socket < rhs.socket; });
	return resolved;
}

// 写入口用:socket 这个孔上的宝石此刻是否计入加成(= 在 ResolveGemsNow 的结果里)。
// 只用来决定要不要打那条 WARN,不参与任何放行 / 拒绝判定。
bool IsGemEffectiveOn(const ItemComp& item, const ItemTable& row, uint32_t socket) {
	const std::vector<ResolvedGem> resolved = ResolveGemsNow(item, row);
	return std::any_of(resolved.begin(), resolved.end(),
					   [socket](const ResolvedGem& gem) { return gem.socket == socket; });
}

// 宝石属性的显示行(宝石物品的 gem_attr 与孔位的 attr 共用)。tier / cap / seq 不写,保持 0:
// 宝石没有颜色档、没有「/上限」、一颗只有一条属性(协议注释里写的是「恒 0」)。
void FillGemAttrLine(const EquipAttributeTable& attr, uint64_t value, ::EquipAttrLineInfo& line) {
	line.set_attr_id(attr.id());
	line.set_name(attr.name());
	line.set_value(value);
	line.set_percent(attr.percent() != 0);
}

// 与 Bag::TakeInstance / PutInstance 的前置条件同口径:只有「不可叠加、size == 1」的实例能整件搬。
// 写成本地谓词是为了把这类拒绝提前到任何修改之前(搬运原语自己还会再查一遍)。
bool IsCarriable(const ItemComp& item, const ItemTable& row) {
	return row.max_stack_size() == 1 && item.size() == 1;
}

// 仅供单测替换的随机源槽位(PlayerEquipSystem::SetRandomSourceForTest)。空 = 用生产随机源。
// 函数内静态、不带锁:做法与约束同 BagService 的初始化回调槽位 —— 只许在测试的 SetUp / TearDown 里换,
// 生产代码从不写它。
std::function<uint64_t(uint64_t)>& RandomSourceForTestSlot() {
	static std::function<uint64_t(uint64_t)> slot;
	return slot;
}

// 本文件全部掷骰(掷随机属性、强化判定)的随机源:默认是生产随机源 tlsRandom,单测可整体换掉。
// equiprules 约定 rand(n) 返回 [0, n) 的均匀整数、n >= 1,形参必须是 64 位(池的权重和按 64 位累加)。
uint64_t RandBelow(uint64_t n) {
	if (const auto& testSource = RandomSourceForTestSlot(); testSource) {
		return testSource(n);
	}
	return tlsRandom.Rand<uint64_t>(0, n - 1);
}

// 写入口统一前置:实体有效 / 未跨 zone 冻结 / 不在战斗(与 PetSystem、PlayerAttributeSystem 同口径,
// 只是冻结与战斗各有自己的 tip,玩家看得懂为什么换不了)。
uint32_t CheckWritable(entt::entity player) {
	if (!tlsEcs.actorRegistry.valid(player)) {
		return kEntityIsNull;
	}
	if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp>(player)) {
		LOG_WARN << "[PlayerEquip] Rejected: cross-zone freeze in progress player_id=" << tlsEcs.GetPlayerId(player);
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
	LOG_ERROR << "[PlayerEquip][INSTANCE_LOST] Rollback failed;instance is absent from all bags;manual compensation required: player_id="
			  << tlsEcs.GetPlayerId(player) << " where=" << where << " guid=" << item.item_id()
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
		LOG_ERROR << "[PlayerEquip] Original slot unavailable during rollback;placed at automatically selected location: player_id=" << tlsEcs.GetPlayerId(player)
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

// ---------------------------------------------------------------------------
// 强化 / 镶嵌 / 取下三个写入口共用的定位与提交辅助(二期 §4.3)
// ---------------------------------------------------------------------------

// CanAfford / DeductCurrency 收 int64:超过这个数的金币消耗转过去是负数。那样的行本来就没人付得起,
// 写入口按「付不起」拒,表校验报这一行。
constexpr uint64_t kMaxPayableGold = static_cast<uint64_t>((std::numeric_limits<int64_t>::max)());

// LocateEquipment 的结果。仅成功后安装借用;默认空态和失败不改 out 的约定保持一致。
struct LocatedEquipment {
	std::optional<std::reference_wrapper<Bag>> bag; // 所在的包:借用 PlayerBagsComp 内的人物背包或装备栏
	bool worn{false};              // 在装备栏里(改它要重算属性)
	std::optional<std::reference_wrapper<const ItemTable>> row; // Item 行借用;现查现用,不出调用它的那个函数
	// 实例。**只读,且只在本包下一次 Insert / Erase / 整理之前有效**:纯校验段用它读等级 / 宝石记录;
	// 提交段一旦调过 AddItems / MergeAndCompact 就不得再碰,写实例一律走 RefetchInitializedInstance
	// 重新按 guid 取(所以这里是 const 借用 —— 想写就必须重取)。
	std::optional<std::reference_wrapper<const ItemComp>> item;
	bool initialized{false};       // has_equip():实例已完成初始化
};

// 按 guid 定位一件可以强化 / 镶嵌的装备:先人物背包、后装备栏;仓库 / 临时格里的不算(二期 §1.4 E5)。
// 拒绝码与顺序同 Equip:找不到 → kEquipItemNotFound;Item 行为空 / 不是装备 / 被配成可堆叠 →
// kEquipNotEquipment;size != 1(被扣光还没回收的空实例)→ kEquipItemNotFound。
// **未初始化(!has_equip())不在这里拒**:那是一期定义的合法状态,由三个入口各自映射成业务码。
// 纯读;失败时不改 out。
uint32_t LocateEquipment(PlayerBagsComp& bags, Guid itemId, LocatedEquipment& out) {
	Bag* bag = &bags.bags[kInventory];
	bool worn = false;
	const ItemComp* item = bag->GetItemCompByGuid(itemId);
	if (item == nullptr) {
		bag = &bags.bags[kEquipment];
		worn = true;
		item = bag->GetItemCompByGuid(itemId);
	}
	if (item == nullptr) {
		return kEquipItemNotFound;
	}
	const ItemTable* row = ItemRow(item->config_id());
	if (row == nullptr || row->equip_kind() == 0 || row->max_stack_size() != 1) {
		return kEquipNotEquipment;
	}
	if (item->size() != 1) {
		return kEquipItemNotFound;
	}
	out.bag = std::ref(*bag);
	out.worn = worn;
	out.row = std::cref(*row);
	out.item = std::cref(*item);
	out.initialized = item->has_equip();
	return kSuccess;
}

// 提交段写实例之前用:重新按 guid 取可写指针。只认已初始化的实例 —— 绝不对 !has_equip() 的实例调
// mutable_equip()(二期 §5 不变量 7)。取不到 / 未初始化都返回 nullptr,由调用方按「实例丢失」报。
// 返回的指针同样只活到本包下一次 Insert / Erase / 整理。
ItemComp* RefetchInitializedInstance(Bag& bag, Guid itemId) {
	ItemComp* instance = bag.GetItemCompByGuid(itemId);
	return (instance != nullptr && instance->has_equip()) ? instance : nullptr;
}

// 包里有没有哪一堆 configId 还塞得下至少 1 个。判据直接用容器层入包并堆的 ItemStore::CanStack
// (同 config,双方都不是独立实例:不带 equip 段、不带本版本不认识的字段),再加没到堆叠上限。
// 不自己另写一份判据:容器层的判定一变(例如混版本保护让带未知字段的堆不收并入),这里自己写的那份就会放行、
// 落到容器层的不足分支(docs/design/mixed-version-persist-field-protection.md §4.4)。
// 静默、只读:给「背包满」这类玩家随手能触发的常规拒绝做预检用,不让它走到容器层会打 ERROR + 调用栈的不足分支
// (CheckSpaceFor 失败时同样走 PrintStackAndReturnError,不能拿来代替)。
bool HasStackWithRoom(const Bag& bag, uint32_t configId, uint32_t maxStackSize) {
	ItemComp incoming;  // 入包的那 1 颗:新发放的普通物品,只有 config
	incoming.set_config_id(configId);
	bool found = false;
	bag.ForEachItem([&found, &incoming, maxStackSize](Guid, const ItemComp& item) {
		if (ItemStore::CanStack(item, incoming) && item.size() < maxStackSize) {
			found = true;
		}
	});
	return found;
}

// 规则层的孔位判定 → 玩家看得懂的 tip。kOk 映射成 kSuccess。
uint32_t TipForGemFit(equiprules::GemFit fit) {
	switch (fit) {
	case equiprules::GemFit::kOk:
		return kSuccess;
	case equiprules::GemFit::kSocketInvalid:
		return kEquipGemSocketInvalid;
	case equiprules::GemFit::kSocketOccupied:
		return kEquipGemSocketOccupied;
	case equiprules::GemFit::kTypeMismatch:
		return kEquipGemTypeMismatch;
	case equiprules::GemFit::kCount:
		break;
	}
	// kCount 不是一个判定结果;走到这里 = 规则层返回了枚举之外的值。按故障拒,不放行。
	return kEquipInternalError;
}

// 把一件穿着的装备累加进 out:基础属性(表值按强化等级放大)+ 随机属性(实例)+ 当前生效的宝石(实例)。
void AccumulateWornItem(const ItemComp& item, uint64_t playerIdForLog, equiprules::EquipBonus& out) {
	const ItemTable* row = ItemRow(item.config_id());
	if (row == nullptr) {
		LOG_ERROR << "[PlayerEquip] Equipped item has no Item row;bonus ignored: player_id=" << playerIdForLog
				  << " item_guid=" << item.item_id() << " config=" << item.config_id();
		return;
	}
	if (row->equip_kind() == 0) {
		// 装备栏里躺着一件表里已不是装备的东西:策划把这一行从装备改成了非装备,老存档经还原兜底
		// (Bag::InsertItemForRestore 的落位规则 ②)仍落回装备栏。FillItemDisplay 对非装备一行属性
		// 都不出,这里也整件不计 —— 加成与 tooltip 同口径,不会出现「面板有数、tooltip 没字」。
		LOG_ERROR << "[PlayerEquip] Equipped item is not equipment in Item table(equip_kind=0);bonus ignored: player_id="
				  << playerIdForLog << " item_guid=" << item.item_id() << " config=" << item.config_id();
		return;
	}

	// 强化只放大基础属性;0 级的万分比是 0,ResolveBaseNow 严格恒等(未强化的装备与一期逐位一致)。
	const uint32_t enhancePermyriad = EnhanceBonusPermyriad(EffectiveEnhanceLevel(item));
	for (const auto& base : row->base_attr()) {
		const EquipAttributeTable* attr = AttributeRow(base.base_attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] Base attribute references missing EquipAttribute;skipped: config=" << item.config_id()
					  << " attr_id=" << base.base_attr_id();
			continue;
		}
		if (!equiprules::ApplyEffect(out, attr->effect(), attr->effect_param(),
									 ResolveBaseNow(base.base_attr_value(), enhancePermyriad))) {
			LOG_ERROR << "[PlayerEquip] Base attribute effect rejected by rules;skipped: config=" << item.config_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
		}
	}

	// has_equip() 为假时 equip() 返回默认实例(0 条),不必单独分支。
	for (const auto& affix : item.equip().affixes()) {
		if (!equiprules::IsRandomAffixTier(affix.tier())) {
			LOG_ERROR << "[PlayerEquip] Invalid random attribute color tier;skipped: player_id=" << playerIdForLog
					  << " item_guid=" << item.item_id() << " attr_id=" << affix.attr_id()
					  << " tier=" << affix.tier();
			continue;
		}
		const EquipAttributeTable* attr = AttributeRow(affix.attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] Random attribute references missing EquipAttribute;skipped: player_id=" << playerIdForLog
					  << " item_guid=" << item.item_id() << " attr_id=" << affix.attr_id();
			continue;
		}
		const AffixNow now = ResolveAffixNow(affix, item, row->equip_level());
		if (!equiprules::ApplyEffect(out, attr->effect(), attr->effect_param(), now.value)) {
			LOG_ERROR << "[PlayerEquip] Random attribute effect rejected by rules;skipped: item_guid=" << item.item_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
		}
	}

	// 宝石:只累加 ResolveGemsNow 认可的那几颗(它已经筛掉规则层不接受的 effect,所以这里不会被拒,
	// 也不打日志 —— 读路径静默)。宝石不吃强化放大。
	for (const ResolvedGem& gem : ResolveGemsNow(item, *row)) {
		equiprules::ApplyEffect(out, gem.attr.get().effect(), gem.attr.get().effect_param(), gem.value);
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
	LOG_INFO << "[PlayerEquip] New instance initialization callback installed; equipment table validation issues=" << problems;
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
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAttribute id=" << row.id() << " effect=" << row.effect()
					  << " effect_param=" << row.effect_param() << " is rejected by rules;this attribute will not take effect";
			continue;
		}
		if (row.effect() != equiprules::ToRaw(EffectKind::kPrimaryPoint)) {
			continue;
		}
		const AttributeDimensionTable* dimension =
			AttributeDimensionTableManager::Instance().FindByIdSilent(row.effect_param()).first;
		if (dimension == nullptr) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAttribute id=" << row.id()
					  << " grants primary attribute points,but effect_param=" << row.effect_param()
					  << " is not an AttributeDimension dimension;allocated points will not affect any attribute";
		} else if (!PlayerAttributeSystem::IsPlayerDimension(row.effect_param())) {
			// 维度存在但属于宝宝池(或它的池查不到):规则层照收、点数照记,只是角色的属性重算
			// 只读角色池维度 —— 后果与「维度不存在」完全相同,却不会有任何别的报错。
			// 「是不是角色池维度」问的就是属性重算自己用的那个判定,这里不另抄一份 owner_type 口径。
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAttribute id=" << row.id()
					  << " grants primary attribute points,but effect_param=" << row.effect_param() << " dimension(pool_id="
					  << dimension->pool_id() << ")does not belong to a character pool;allocated points will not affect any attribute";
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
		LOG_ERROR << "[PlayerEquip] Table validation: battle attribute " << raw << " has no display row in EquipAttribute(requires a row with effect="
				  << equiprules::ToRaw(EffectKind::kCombat) << "、effect_param=" << raw
				  << " and nonempty name);this attribute will be missing from the battle attribute panel";
	}

	// EquipAttributeCap:区间必须满足 1 <= min_value <= cap(规则层会夹取,但那已经不是策划填的数了)。
	for (const auto& row : EquipAttributeCapTableManager::Instance().FindAll().data()) {
		if (row.cap() == 0 || row.min_value() == 0 || row.min_value() > row.cap()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAttributeCap id=" << row.id() << " has invalid range: min_value="
					  << row.min_value() << " cap=" << row.cap() << "(must satisfy 1 <= min_value <= cap)";
		}
	}

	// EquipAffixPool:池里的属性必须存在、权重 > 0,并且至少有一档上限(否则任何等级的装备都掷不出它)。
	for (const auto& row : EquipAffixPoolTableManager::Instance().FindAll().data()) {
		const bool attrExists = AttributeRow(row.attr_id()) != nullptr;
		if (!attrExists) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAffixPool id=" << row.id() << " attr_id=" << row.attr_id()
					  << " is absent from EquipAttribute";
		}
		if (row.weight() == 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAffixPool id=" << row.id() << " has zero weight and can never be rolled";
		}
		// 属性行都不存在时,它在上限表里必然也一档都没有(EquipAttributeCap.attr_id 有导表期外键):
		// 那是同一个根因,上面已经报过,不再多计一条。
		if (attrExists && EquipAttributeCapTableManager::Instance().GetByAttrId(row.attr_id()).empty()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAffixPool id=" << row.id() << " attr_id=" << row.attr_id()
					  << " has no cap tier in EquipAttributeCap and can never be rolled";
		}
	}

	// EquipAffixRule:条数区间不能倒挂,概率不能超过万分之一万。
	for (const auto& row : EquipAffixRuleTableManager::Instance().FindAll().data()) {
		if (row.blue_min() > row.blue_max()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAffixRule id=" << row.id() << " has inverted blue attribute count bounds: blue_min="
					  << row.blue_min() << " blue_max=" << row.blue_max() << "(rules will roll using blue_min)";
		}
		if (row.pink_rate() > equiprules::kPermyriadBase || row.yellow_rate() > equiprules::kPermyriadBase) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: EquipAffixRule id=" << row.id() << " has probability exceeding "
					  << equiprules::kPermyriadBase << ": pink_rate=" << row.pink_rate()
					  << " yellow_rate=" << row.yellow_rate() << "(per ten thousand;exceeding this guarantees a roll)";
		}
	}

	// Item:只看装备行。这几条都没有导表期外键(子消息列 / 指向非主键),只能在这里发现。
	for (const auto& row : ItemTableManager::Instance().FindAll().data()) {
		if (row.equip_kind() == 0) {
			continue;
		}
		if (row.max_stack_size() != 1) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: Item id=" << row.id() << " is equipment(equip_kind=" << row.equip_kind()
					  << ")but max_stack_size=" << row.max_stack_size()
					  << ";stacking would merge instances with different attributes;this row will not roll attributes and cannot be equipped";
		}
		if (row.affix_pool() != 0 &&
			EquipAffixPoolTableManager::Instance().GetByPoolId(row.affix_pool()).empty()) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: Item id=" << row.id() << " affix_pool=" << row.affix_pool()
					  << " has no rows in EquipAffixPool;this equipment cannot roll random attributes";
		}
		if (row.affix_rule() != 0 && !EquipAffixRuleTableManager::Instance().Exists(row.affix_rule())) {
			++problems;
			LOG_ERROR << "[PlayerEquip] Table validation: Item id=" << row.id() << " affix_rule=" << row.affix_rule()
					  << " is absent from EquipAffixRule;this equipment cannot roll random attributes";
		}
		for (const auto& base : row.base_attr()) {
			// 属性行自身 effect 是否合法已在上面的 EquipAttribute 段报过,这里只查引用是否存在。
			if (AttributeRow(base.base_attr_id()) == nullptr) {
				++problems;
				LOG_ERROR << "[PlayerEquip] Table validation: Item id=" << row.id() << " base attribute base_attr_id="
						  << base.base_attr_id() << " is absent from EquipAttribute(unused slots must be empty,not 0)";
			}
		}
	}

	// ---- 二期:强化 / 镶嵌的三张表(二期 §4.3「表校验」)。每个 if 对应契约里「恰好计一条」的一个条件 ----
	// 这里只打日志、不拒启,单表热更也不重跑;所以写入口对缺行、对被配成装备的材料 / 宝石行另有 fail-closed
	// (Enhance 的材料检查、GemRowIfGem),两道都要有。

	// EquipEnhance:一行 = 「强化到 +id」。上限是从 1 起连续的最大 id,不在这段连续等级里的行永远用不到。
	const uint32_t maxEnhanceLevel = MaxEnhanceLevel();
	for (const auto& row : EquipEnhanceTableManager::Instance().FindAll().data()) {
		if (row.id() == 0 || row.id() > maxEnhanceLevel) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id()
					  << " 不在从 1 起连续的强化等级里(当前连续到 " << maxEnhanceLevel
					  << "),这一行永远用不到(id 必须从 1 起连续,中间不能缺)";
		} else if (row.id() > 1) {
			// 连续等级内:bonus_permyriad 是「总加成」不是增量,越强化越低就是填错了。
			const EquipEnhanceTable* previous = EnhanceRow(row.id() - 1);
			if (previous != nullptr && row.bonus_permyriad() < previous->bonus_permyriad()) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 bonus_permyriad="
						  << row.bonus_permyriad() << " 小于上一级的 " << previous->bonus_permyriad()
						  << "(它是强化到本级后的总加成,不是相对上一级的增量)";
			}
		}
		if (row.success_rate() == 0 || row.success_rate() > equiprules::kPermyriadBase) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 success_rate="
					  << row.success_rate() << " 不在 1.." << equiprules::kPermyriadBase
					  << "(万分比;0 = 这一级无法强化,写入口会拒绝且不收费)";
		}
		if (row.cost_gold() > kMaxPayableGold) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 cost_gold=" << row.cost_gold()
					  << " 超过 INT64_MAX,没有人付得起(写入口按金币不足拒绝)";
		}
		if (row.material_id() != 0) {
			const ItemTable* materialRow = ItemRow(row.material_id());
			if (materialRow == nullptr) {
				// Item 行都没有,「是不是装备 / 是不是战斗道具」无从谈起:同一个根因,不再查后两项。
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 material_id="
						  << row.material_id() << " 不在 Item 表里,这一级无法强化";
			} else {
				if (materialRow->equip_kind() != 0) {
					++problems;
					LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 material_id="
							  << row.material_id() << " 是装备(equip_kind=" << materialRow->equip_kind()
							  << ");按 config 扣材料会把装备实例当材料销毁,写入口会拒绝这一级";
				}
				if (materialRow->battle_usable() != 0) {
					++problems;
					LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 的 material_id="
							  << row.material_id()
							  << " 是战斗道具(battle_usable != 0);战斗结算按 config 扣物,会与强化扣材料在重登窗口里交错";
				}
			}
			if (row.material_count() == 0) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 配了 material_id="
						  << row.material_id() << " 但 material_count=0,这一级实际不扣材料";
			}
		} else if (row.material_count() != 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipEnhance id=" << row.id() << " 没配 material_id 却填了 material_count="
					  << row.material_count() << ",这一级实际不扣材料";
		}
	}

	// EquipGem:一行一种宝石,id 就是宝石物品的 Item.id。
	for (const auto& row : EquipGemTableManager::Instance().FindAll().data()) {
		const ItemTable* itemRow = ItemRow(row.id());
		if (itemRow == nullptr) {
			// 同上:Item 行不存在时不再查后两项。
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id() << " 不在 Item 表里,这种宝石无法镶嵌、"
					  << "已嵌的也取不下来";
		} else {
			if (itemRow->equip_kind() != 0) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id() << " 对应的物品是装备(equip_kind="
						  << itemRow->equip_kind() << ");按 config 扣发会销毁 / 重铸装备实例,写入口不把它当宝石";
			}
			if (itemRow->battle_usable() != 0) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id()
						  << " 对应的物品是战斗道具(battle_usable != 0);战斗结算按 config 扣物,会与镶嵌扣宝石在重登窗口里交错";
			}
		}
		if (row.gem_type() == 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id()
					  << " 的 gem_type=0(类型必须 >= 1;0 在孔位表里表示「任意」,这种宝石哪个孔都嵌不进)";
		}
		// 属性行存在但 effect 不被规则层接受:EquipAttribute 段已经报过那一行,同一个根因,这里不再计、不再打。
		if (AttributeRow(row.attr_id()) == nullptr) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id() << " 的 attr_id=" << row.attr_id()
					  << " 不在 EquipAttribute 里,这种宝石嵌上也不生效";
		}
		if (row.value() == 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGem id=" << row.id() << " 的 value=0,这种宝石嵌上没有任何效果";
		}
	}

	// EquipGemSocket:一行一个孔。按行的 equip_kind 列分组(不走索引:要查的正是这一列填得对不对)。
	// 同一根因只计一次的两处取舍:
	//   * equip_kind == 0 的行不属于任何部位,只报这一条,不参加下面按部位的两项;
	//   * 越界的 socket 已逐行报过;它原本该占的那个号留下的缺口算同一个根因,不另计(否则把某个孔误填成 9
	//     会被计两条)。所以「不连续」只在缺口数多于越界行数时才报 —— 那才说明还有别的号真的缺了。
	std::map<uint32_t, std::vector<uint32_t>> socketsByKind;
	for (const auto& row : EquipGemSocketTableManager::Instance().FindAll().data()) {
		if (row.equip_kind() == 0) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGemSocket id=" << row.id()
					  << " 的 equip_kind=0(不是任何部位),这个孔不属于任何装备";
		}
		if (row.socket() >= equiprules::kMaxGemSockets) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGemSocket id=" << row.id() << " 的 socket=" << row.socket()
					  << " 超出单件装备的孔位上限(必须 < " << equiprules::kMaxGemSockets << ")";
		}
		if (row.equip_kind() != 0) {
			socketsByKind[row.equip_kind()].push_back(row.socket());
		}
	}
	for (auto& [equipKind, sockets] : socketsByKind) {
		std::sort(sockets.begin(), sockets.end());
		// 重复的孔位号:每多出一行计一条(运行期同号只认排在前面的那一行,后面的行是死数据)。
		for (std::size_t i = 1; i < sockets.size(); ++i) {
			if (sockets[i] == sockets[i - 1]) {
				++problems;
				LOG_ERROR << "[PlayerEquip] 表校验: EquipGemSocket 部位 equip_kind=" << equipKind << " 的 socket="
						  << sockets[i] << " 重复出现,多出来的行不生效";
			}
		}
		// 界内的孔位号去重后必须是 0..n-1(从 0 起连续)。缺口数 = (界内最大号 + 1) − 界内不同号的个数;
		// 每个越界的行「解释」掉一个缺口(它已经逐行报过),缺口比越界的行还多才另计一条。
		std::size_t outOfRangeRows = 0;
		for (const uint32_t socket : sockets) {
			if (socket >= equiprules::kMaxGemSockets) {
				++outOfRangeRows;
			}
		}
		sockets.erase(std::unique(sockets.begin(), sockets.end()), sockets.end());
		std::size_t inRangeSockets = 0;
		uint32_t highestInRange = 0;
		for (const uint32_t socket : sockets) {
			if (socket >= equiprules::kMaxGemSockets) {
				break;  // 已升序:从这里往后全是越界的
			}
			highestInRange = socket;
			++inRangeSockets;
		}
		const std::size_t gaps =
			inRangeSockets == 0 ? 0 : static_cast<std::size_t>(highestInRange) + 1 - inRangeSockets;
		if (gaps > outOfRangeRows) {
			++problems;
			LOG_ERROR << "[PlayerEquip] 表校验: EquipGemSocket 部位 equip_kind=" << equipKind
					  << " 的孔位号不是从 0 起连续的(界内最大孔位号=" << highestInRange << ",缺了 " << gaps
					  << " 个号,其中越界的行只能解释 " << outOfRangeRows << " 个)";
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
		LOG_ERROR << "[PlayerEquip] Required table row for attribute rolls is missing;item has no random attributes: item_guid=" << item.item_id()
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
			LOG_ERROR << "[PlayerEquip] Nonexistent attribute excluded from pool: pool=" << row->affix_pool()
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
			LOG_ERROR << "[PlayerEquip] Target slot occupant cannot be moved;replacement rejected: player_id=" << tlsEcs.GetPlayerId(player)
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
		LOG_ERROR << "[PlayerEquip] Equip failed(removing from bag): player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		return kEquipInternalError;  // TakeInstance 失败零副作用,无需回滚
	}

	ItemComp replaced;
	if (replacing) {
		if (const auto err = equipment.TakeInstance(replacedGuid, replaced); err != kSuccess) {
			LOG_ERROR << "[PlayerEquip] Equip failed(removing previous equipment): player_id=" << tlsEcs.GetPlayerId(player)
					  << " guid=" << itemId << " config=" << configId << " replaced_guid=" << replacedGuid
					  << " slot=" << targetSlot << " err=" << err;
			PutBack(inventory, incoming, inventorySlot, "inventory", player);
			return kEquipInternalError;
		}
	}

	if (const auto err = equipment.PutInstance(incoming, targetSlot); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] Equip failed(inserting into equipment bag): player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
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
			LOG_ERROR << "[PlayerEquip] Equip failed(returning previous equipment to bag): player_id=" << tlsEcs.GetPlayerId(player)
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
	LOG_INFO << "[PlayerEquip] Equipped: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
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
		LOG_ERROR << "[PlayerEquip] Equipped item cannot be moved;unequip rejected: player_id=" << tlsEcs.GetPlayerId(player)
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
		LOG_ERROR << "[PlayerEquip] Character bag has free space but rejects this equipment;unequip rejected: player_id=" << tlsEcs.GetPlayerId(player)
				  << " guid=" << itemId << " config=" << configId << " err=" << err;
		return kEquipInternalError;
	}
	const uint32_t wornSlot = equipment.GetItemPosByGuid(itemId);

	// ---- 搬运 ----
	ItemComp carried;
	if (const auto err = equipment.TakeInstance(itemId, carried); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] Unequip failed(removing from equipment bag): player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		return kEquipInternalError;  // 零副作用,无需回滚
	}
	if (const auto err = inventory.PutInstance(carried); err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] Unequip failed(returning to bag): player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
				  << " config=" << configId << " err=" << err;
		if (!PutBack(equipment, carried, wornSlot, "equipment", player)) {
			// 没能放回装备栏:这件装备已不在身上(丢失由 PutBack 报告),面板必须跟上。
			OnEquipmentChanged(player);
		}
		return kEquipInternalError;
	}

	OnEquipmentChanged(player);
	LOG_INFO << "[PlayerEquip] Unequipped: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
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
	// 走正式发放入口:封禁闸、获得流水(TX_GM_GRANT,审计能一眼分出「GM 发的」)、异常检测都照常过;
	// 新装备的掷属性挂在这条路上,不需要在这里再调 RollNewInstance。
	const auto err = BagService::AddItems(player, bags->bags[kInventory],
										  items, TX_GM_GRANT);
	LOG_INFO << "[PlayerEquip] GM item grant: player_id=" << tlsEcs.GetPlayerId(player) << " config=" << configId
			 << " count=" << count << " err=" << err;
	return err;
}

// ---------------------------------------------------------------------------
// 写入口:强化 / 镶嵌 / 取下(二期 §4.3;步骤号对应设计文档里三张步骤表)
// ---------------------------------------------------------------------------

uint32_t PlayerEquipSystem::Enhance(entt::entity player, Guid itemId, EnhanceOutcome& outcome) {
	outcome = EnhanceOutcome{};
	// 1. 统一前置
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}
	Bag& inventory = bags->bags[kInventory];

	// ---- 纯校验:以下任何一条失败,金币、背包、实例都没有动过 ----
	// 2. 定位;未初始化的实例没有 equip 段可写(绝不替它 mutable_equip()),对玩家而言这件装备不能强化。
	LocatedEquipment located;
	if (const auto err = LocateEquipment(*bags, itemId, located); err != kSuccess) {
		return err;
	}
	const uint32_t configId = located.item->get().config_id();
	if (!located.initialized) {
		LOG_WARN << "[PlayerEquip] 强化被拒,实例未初始化(没有 equip 段): player_id=" << tlsEcs.GetPlayerId(player)
				 << " guid=" << itemId << " config=" << configId;
		return kEquipEnhanceUnavailable;
	}
	// 3. 强化是否开放
	const uint32_t maxLevel = MaxEnhanceLevel();
	if (maxLevel == 0) {
		return kEquipEnhanceUnavailable;
	}
	// 4. 存档等级按表上限夹取(策划下调上限后,超出的那几级不洗存档、也不许再往上强化)
	const uint32_t currentLevel = std::min<uint32_t>(located.item->get().equip().enhance_level(), maxLevel);
	if (currentLevel >= maxLevel) {
		return kEquipEnhanceMaxLevel;
	}
	// 5. 下一级的规则行。currentLevel < maxLevel 时它一定在连续等级里;查不到 = 两次查表之间换了表。
	const uint32_t targetLevel = currentLevel + 1;
	const EquipEnhanceTable* enhanceRow = EnhanceRow(targetLevel);
	if (enhanceRow == nullptr) {
		LOG_ERROR << "[PlayerEquip] 强化失败,EquipEnhance 缺行: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
				  << " config=" << configId << " target_level=" << targetLevel << " max_level=" << maxLevel;
		return kEquipInternalError;
	}
	if (enhanceRow->success_rate() == 0) {
		return kEquipEnhanceUnavailable;  // 必败的行:不为一次不可能成功的强化收费
	}
	// 规则抄进局部量:提交段不再读表行,也让下面每一步用的是同一份数。
	const uint32_t successRate = enhanceRow->success_rate();
	const uint32_t failLevelLoss = enhanceRow->fail_level_loss();
	const uint64_t costGold = enhanceRow->cost_gold();
	const uint32_t materialId = enhanceRow->material_id();
	const uint32_t materialCount = enhanceRow->material_count();
	const bool needsMaterial = materialId != 0 && materialCount > 0;
	if (needsMaterial) {
		// 6. 材料必须是一种真实存在的、不是装备的物品。按 config 扣数量不看 has_equip():材料被配成装备的话,
		//    扣的就是玩家的装备实例(甚至是正在强化的这一件)。表校验只打日志,这里 fail-closed。
		const ItemTable* materialRow = ItemRow(materialId);
		if (materialRow == nullptr || materialRow->equip_kind() != 0) {
			LOG_ERROR << "[PlayerEquip] 强化失败,材料行不可用(不存在或被配成了装备): player_id=" << tlsEcs.GetPlayerId(player)
					  << " guid=" << itemId << " target_level=" << targetLevel << " material_id=" << materialId
					  << " material_row_found=" << (materialRow != nullptr);
			return kEquipInternalError;
		}
		// 7. 静默预检:「材料不够」是玩家随手能触发的常规拒绝,不让它走到底层会打 ERROR + 调用栈的不足分支。
		//    与提交段的扣除同一个判据(Bag::CheckQuantities):必须在下面扣金币之前判完,否则扣了金币才发现材料扣不出。
		//    只有算上带更新版本字段的材料才够 = 版本错位(混版本设计 §4.4 第 6 批):不当普通单位扣,回暂不可用。
		switch (inventory.CheckQuantities({{materialId, materialCount}})) {
		case ItemStore::QuantityVerdict::kEnough:
			break;
		case ItemStore::QuantityVerdict::kVersionSkew:
			LOG_WARN << "[PlayerEquip] metric=persist_version_skew where=equip_enhance player_id="
					 << tlsEcs.GetPlayerId(player) << " material=" << materialId;
			return kFeatureUnavailable;
		case ItemStore::QuantityVerdict::kInsufficient:
			return kEquipEnhanceMaterialNotEnough;
		}
	}
	// 8. 金币。cost == 0 不问(CanAfford 对 <= 0 打 ERROR);超过 int64 上限的表值转过去是负数,直接按付不起拒。
	if (costGold > 0 && (costGold > kMaxPayableGold ||
						 !CurrencySystem::CanAfford(player, kCurrencyGold, static_cast<int64_t>(costGold)))) {
		return kEquipEnhanceGoldNotEnough;
	}
	Bag& holder = located.bag->get();
	const bool worn = located.worn;

	// ---- 提交:scene 单线程、冻结已被第 1 步挡掉、余额与持有量都问过,以下各步正常不会失败;
	//      失败分支只为留下可对账的日志。金币与材料成败都扣,不退(那是设计,不是故障)----
	// 9. 先掷后扣:流水的 extra 要带成败。结果在回包之前不外露,先掷不影响公平。
	const bool success = equiprules::RollEnhanceSuccess(successRate, [](uint64_t n) { return RandBelow(n); });
	const std::string extra = "{\"from\":" + std::to_string(currentLevel) + ",\"success\":" +
							  (success ? "true" : "false") + "}";
	// 10. 扣金币。失败 = 什么都没动。
	if (costGold > 0) {
		if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(costGold),
															TX_EQUIP_ENHANCE, itemId);
			err != kSuccess) {
			LOG_ERROR << "[PlayerEquip] 强化失败(扣金币),什么都没动: player_id=" << tlsEcs.GetPlayerId(player)
					  << " guid=" << itemId << " config=" << configId << " target_level=" << targetLevel
					  << " cost_gold=" << costGold << " err=" << err;
			return kEquipInternalError;
		}
	}
	// 11. 扣材料。走到失败分支时金币已经扣了,而仓库里没有货币退款原语(AddCurrency 会被欠款抵扣与
	//     封禁闸改写,不能当回滚用):只留一条带固定标签的 ERROR 供人工对账。
	if (needsMaterial) {
		ItemCountMap materials;
		materials[materialId] = materialCount;
		if (const auto err =
				BagService::RemoveItemsExact(player, inventory, materials, TX_EQUIP_ENHANCE, itemId, extra);
			err != kSuccess) {
			LOG_ERROR << "[PlayerEquip][ENHANCE_PARTIAL] 强化扣材料失败,已扣的金币(gold_deducted)不退、材料与等级未动,需人工对账: player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId
					  << " from_level=" << currentLevel << " gold_deducted=" << costGold
					  << " material_id=" << materialId << " material_count=" << materialCount << " err=" << err;
			return kEquipInternalError;
		}
	}
	// 12. 写等级。一律重新按 guid 取:located.item 是纯校验段的借用,不带进提交段。
	ItemComp* instance = RefetchInitializedInstance(holder, itemId);
	if (instance == nullptr) {
		LOG_ERROR << "[PlayerEquip][INSTANCE_LOST] 强化已扣费,但写等级时取不到这件装备,需人工补偿: player_id="
				  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId
				  << " from_level=" << currentLevel << " success=" << success << " gold_deducted=" << costGold
				  << " material_id=" << (needsMaterial ? materialId : 0)
				  << " material_count=" << (needsMaterial ? materialCount : 0);
		return kEquipInternalError;
	}
	const uint32_t newLevel =
		success ? targetLevel : equiprules::LevelAfterFailure(currentLevel, failLevelLoss);
	instance->mutable_equip()->set_enhance_level(newLevel);
	// 14. 穿着的那件等级变了 → 加成变了(失败且不掉级时等级没变,不必重算)
	if (worn && newLevel != currentLevel) {
		OnEquipmentChanged(player);
	}
	// 15.
	LOG_INFO << "[PlayerEquip] 强化: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId
			 << " from_level=" << currentLevel << " to_level=" << newLevel << " success=" << success
			 << " worn=" << worn << " cost_gold=" << costGold << " material_id=" << (needsMaterial ? materialId : 0)
			 << " material_count=" << (needsMaterial ? materialCount : 0);
	outcome.success = success;
	outcome.level = newLevel;
	return kSuccess;
}

uint32_t PlayerEquipSystem::InlayGem(entt::entity player, Guid itemId, uint32_t socket, uint32_t gemConfigId) {
	// 1. 统一前置
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}
	Bag& inventory = bags->bags[kInventory];

	// ---- 纯校验:以下任何一条失败,背包与实例都没有动过 ----
	// 2. 定位;未初始化的实例没有 equip 段可写,对玩家而言这件装备此刻没有可用的孔。
	LocatedEquipment located;
	if (const auto err = LocateEquipment(*bags, itemId, located); err != kSuccess) {
		return err;
	}
	const uint32_t configId = located.item->get().config_id();
	if (!located.initialized) {
		LOG_WARN << "[PlayerEquip] 镶嵌被拒,实例未初始化(没有 equip 段): player_id=" << tlsEcs.GetPlayerId(player)
				 << " guid=" << itemId << " config=" << configId << " socket=" << socket
				 << " gem_config=" << gemConfigId;
		return kEquipGemSocketInvalid;
	}
	// 3. 是不是宝石(EquipGem 有这一行,且它的 Item 行存在、不是装备)
	const EquipGemTable* gemRow = GemRowIfGem(gemConfigId);
	if (gemRow == nullptr) {
		return kEquipGemNotGem;
	}
	// 4. 孔位:存在 → 没被占 → 类型相符。「已占」看实例上的全部原始记录,不过滤 —— 一条此刻不生效的记录
	//    照样占着孔(否则会在同一个孔上追加第二条,取下时后一颗被无声清掉)。
	//    (gem_config_id == 0 的记录视同不存在,见 FirstGemRecordOn。)
	const auto& storedGems = located.item->get().equip().gems();
	std::vector<uint32_t> occupied;
	occupied.reserve(static_cast<std::size_t>(storedGems.size()));
	for (const auto& gem : storedGems) {
		if (gem.gem_config_id() != 0) {
			occupied.push_back(gem.socket());
		}
	}
	const equiprules::GemFit fit =
		equiprules::CheckGemFit(SocketRulesFor(located.row->get().equip_kind()), occupied, socket, gemRow->gem_type());
	if (const auto err = TipForGemFit(fit); err != kSuccess) {
		if (fit == equiprules::GemFit::kSocketOccupied && !IsGemEffectiveOn(located.item->get(), located.row->get(), socket)) {
			// 玩家看到的是「已镶但未生效」:宝石行 / 属性行被改掉了,或同孔有重复记录。照常拒绝(先取下),留一条线索。
			const EquipGem* occupant = FirstGemRecordOn(located.item->get(), socket);
			LOG_WARN << "[PlayerEquip] 镶嵌被拒,目标孔被一颗当前不生效的宝石占着: player_id=" << tlsEcs.GetPlayerId(player)
					 << " guid=" << itemId << " config=" << configId << " socket=" << socket
					 << " occupant_gem_config=" << (occupant != nullptr ? occupant->gem_config_id() : 0);
		}
		if (fit == equiprules::GemFit::kCount) {
			LOG_ERROR << "[PlayerEquip] 镶嵌失败,孔位判定返回了枚举之外的结果: player_id=" << tlsEcs.GetPlayerId(player)
					  << " guid=" << itemId << " socket=" << socket << " gem_config=" << gemConfigId;
		}
		return err;
	}
	// 5. 记录条数上限。干净数据走不到这里(孔位数 <= kMaxGemSockets,满了会先被第 4 步判成已占);
	//    走到 = 实例上有孔位定义之外的脏记录。消费侧只看前 kMaxGemSockets 条,再追加的那一条不会生效,所以拒。
	if (storedGems.size() >= static_cast<int>(equiprules::kMaxGemSockets)) {
		LOG_ERROR << "[PlayerEquip] 镶嵌失败,实例上的宝石记录已达上限(脏数据): player_id=" << tlsEcs.GetPlayerId(player)
				  << " guid=" << itemId << " config=" << configId << " socket=" << socket
				  << " gem_records=" << storedGems.size() << " limit=" << equiprules::kMaxGemSockets;
		return kEquipInternalError;
	}
	// 6. 静默预检:背包里至少有 1 颗可按数量扣的宝石,与提交段扣除同一个判据(Bag::CheckQuantities)。
	//    只有带更新版本字段的宝石 = 版本错位(混版本设计 §4.4 第 6 批):不当普通宝石嵌进去,回暂不可用。
	switch (inventory.CheckQuantities({{gemConfigId, 1}})) {
	case ItemStore::QuantityVerdict::kEnough:
		break;
	case ItemStore::QuantityVerdict::kVersionSkew:
		LOG_WARN << "[PlayerEquip] metric=persist_version_skew where=equip_inlay player_id="
				 << tlsEcs.GetPlayerId(player) << " gem=" << gemConfigId;
		return kFeatureUnavailable;
	case ItemStore::QuantityVerdict::kInsufficient:
		return kEquipGemNotOwned;
	}
	Bag& holder = located.bag->get();
	const bool worn = located.worn;

	// ---- 提交 ----
	// 7. 扣 1 颗宝石(按 config,不指定哪一堆)。失败 = 什么都没动。
	const std::string extra = "{\"socket\":" + std::to_string(socket) + "}";
	ItemCountMap gemToConsume;
	gemToConsume[gemConfigId] = 1;
	if (const auto err =
			BagService::RemoveItemsExact(player, inventory, gemToConsume, TX_EQUIP_INLAY, itemId, extra);
		err != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 镶嵌失败(扣宝石),什么都没动: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId
				  << " config=" << configId << " socket=" << socket << " gem_config=" << gemConfigId
				  << " err=" << err;
		return kEquipInternalError;
	}
	// 8. 写实例。一律重新按 guid 取。
	ItemComp* instance = RefetchInitializedInstance(holder, itemId);
	if (instance == nullptr) {
		LOG_ERROR << "[PlayerEquip][GEM_LOST] 宝石已扣,但写孔位时取不到这件装备,需人工补偿: player_id="
				  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId << " socket=" << socket
				  << " gem_config=" << gemConfigId;
		return kEquipInternalError;
	}
	EquipGem* placed = instance->mutable_equip()->add_gems();
	placed->set_socket(socket);
	placed->set_gem_config_id(gemConfigId);
	// 10.
	if (worn) {
		OnEquipmentChanged(player);
	}
	LOG_INFO << "[PlayerEquip] 镶嵌: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId
			 << " socket=" << socket << " gem_config=" << gemConfigId << " worn=" << worn;
	return kSuccess;
}

uint32_t PlayerEquipSystem::RemoveGem(entt::entity player, Guid itemId, uint32_t socket) {
	// 1. 统一前置
	if (const auto err = CheckWritable(player); err != kSuccess) {
		return err;
	}
	auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr) {
		return kEquipInternalError;
	}
	Bag& inventory = bags->bags[kInventory];

	// ---- 纯校验:以下任何一条失败,背包与实例都没有动过 ----
	// 2. 定位
	LocatedEquipment located;
	if (const auto err = LocateEquipment(*bags, itemId, located); err != kSuccess) {
		return err;
	}
	const uint32_t configId = located.item->get().config_id();
	// 3. 该孔的第一条原始记录。不要求孔位仍在 EquipGemSocket 表里、也不要求宝石仍在 EquipGem 表里:
	//    策划删孔 / 删宝石行之后,宝石照样取得下来。未初始化的实例读到的是默认实例(0 条),自然落进「没有」。
	const EquipGem* record = FirstGemRecordOn(located.item->get(), socket);
	if (record == nullptr) {
		if (!located.initialized) {
			LOG_WARN << "[PlayerEquip] 取下被拒,实例未初始化(没有 equip 段): player_id=" << tlsEcs.GetPlayerId(player)
					 << " guid=" << itemId << " config=" << configId << " socket=" << socket;
		}
		return kEquipGemSocketEmpty;
	}
	const uint32_t gemConfigId = record->gem_config_id();
	// 宝石的 Item 行必须还在、且不是装备,才退得回去。两种不满足都 fail-closed:宝石留在装备上,等表修好。
	//   * 行被删:发放必然失败。在这里就拒,不留给第 5 步 —— 否则背包满的玩家会先收到「背包已满」,
	//     腾出格子之后才发现根本退不了。
	//   * 行被配成了装备:退回去的会是一件新铸的装备实例(带 equip 段、掷了属性),表改回来之后它就成了一颗
	//     永远并不了堆的「宝石」。与镶嵌入口同一个取舍。
	const ItemTable* gemItemRow = ItemRow(gemConfigId);
	if (gemItemRow == nullptr || gemItemRow->equip_kind() != 0) {
		LOG_ERROR << "[PlayerEquip] 取下失败,宝石的 Item 行不可用(不存在或被配成了装备),宝石仍在装备上: player_id="
				  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId << " socket=" << socket
				  << " gem_config=" << gemConfigId << " gem_row_found=" << (gemItemRow != nullptr);
		return kEquipInternalError;
	}
	// 4. 静默预检:没空格、也没有哪一堆同种宝石还塞得下 → 一定放不下。「背包满」是玩家随手能触发的常规拒绝,
	//    不让它走到容器层会打 ERROR + 调用栈的预检。
	if (inventory.IsSpaceInsufficient(1) && !HasStackWithRoom(inventory, gemConfigId, gemItemRow->max_stack_size())) {
		return kEquipGemBagFull;
	}
	// 取的这颗此刻是否计入加成(宝石行 / 孔位定义 / 属性行已不在表里,或记录排在遍历上限之外,就是不计入)。
	// 不影响放行 —— 取下正是让玩家把它拿回来的出口;只为提交之后留一条线索。必须在第 5 步之前算:
	// 之后 located.item 失效。
	const bool wasEffective = IsGemEffectiveOn(located.item->get(), located.row->get(), socket);
	Bag& holder = located.bag->get();
	const bool worn = located.worn;

	// ---- 提交:先发物,成功后才抹宝石(照 PlayerMissionSystem::ClaimReward)。任何失败都不会凭空少宝石;
	//      「发回了但装备上还在」也不存在:人物背包满了是整批拒绝、不淘汰,失败即未改动 ----
	// 5. 退 1 颗宝石。走正式发放入口:过发放封禁闸、写获得流水、计入异常获得统计。
	//    这是一次 Insert:located.item / record 从这里起失效,下面只用已经抄出来的值。
	const std::string extra = "{\"socket\":" + std::to_string(socket) + "}";
	ItemCountMap refund;
	refund[gemConfigId] = 1;
	const auto addErr = BagService::AddItems(player, inventory, refund, TX_EQUIP_GEM_REMOVE, itemId, extra);
	if (addErr == kBagItemNotStacked || addErr == kBagAddItemBagFull) {
		return kEquipGemBagFull;  // 同种宝石的堆都满了、又没有空格
	}
	if (addErr != kSuccess) {
		LOG_ERROR << "[PlayerEquip] 取下失败(退回宝石),宝石仍在装备上: player_id=" << tlsEcs.GetPlayerId(player)
				  << " guid=" << itemId << " config=" << configId << " socket=" << socket
				  << " gem_config=" << gemConfigId << " err=" << addErr;
		return kEquipInternalError;
	}
	// 6. 抹宝石。重新按 guid 取(装备在人物背包里时,第 5 步的 Insert 就发生在同一个仓库)。
	ItemComp* instance = RefetchInitializedInstance(holder, itemId);
	if (instance == nullptr) {
		LOG_ERROR << "[PlayerEquip][INSTANCE_LOST] 宝石已退回背包,但抹孔位时取不到这件装备,需人工核对: player_id="
				  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId << " socket=" << socket
				  << " gem_config=" << gemConfigId;
		return kEquipInternalError;
	}
	// 删掉该孔的全部记录。退回的是 FirstGemRecordOn 认的那一条(第一条 gem_config_id != 0 的);同孔的其余记录
	// (重复的,或 config 为 0 的空壳)= 脏数据,它们一起被清掉且不退物,每条留一条 ERROR(否则是一次无痕的
	// 资产销毁)。
	auto* gemsOnInstance = instance->mutable_equip()->mutable_gems();
	bool refundedRecordRemoved = false;
	for (int i = 0; i < gemsOnInstance->size();) {
		if (gemsOnInstance->Get(i).socket() != socket) {
			++i;
			continue;
		}
		const uint32_t recordConfigId = gemsOnInstance->Get(i).gem_config_id();
		if (!refundedRecordRemoved && recordConfigId == gemConfigId) {
			refundedRecordRemoved = true;  // 这一条就是刚退回去的那颗
		} else {
			LOG_ERROR << "[PlayerEquip] 取下时清掉了同孔的多余宝石记录(脏数据,不退物): player_id="
					  << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId << " socket=" << socket
					  << " gem_config_id=" << recordConfigId;
		}
		gemsOnInstance->DeleteSubrange(i, 1);  // 后面的元素前移,下标不动
	}
	// 7.
	if (worn) {
		OnEquipmentChanged(player);
	}
	if (!wasEffective) {
		LOG_WARN << "[PlayerEquip] 取下的是一颗当时不生效的宝石(表里已没有它的宝石行 / 孔位定义 / 属性行): player_id="
				 << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId << " socket=" << socket
				 << " gem_config=" << gemConfigId;
	}
	LOG_INFO << "[PlayerEquip] 取下: player_id=" << tlsEcs.GetPlayerId(player) << " guid=" << itemId << " config=" << configId
			 << " socket=" << socket << " gem_config=" << gemConfigId << " worn=" << worn;
	return kSuccess;
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
	const uint64_t playerId = tlsEcs.GetPlayerId(player);
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
		// 宝石物品:下发类型与它提供的属性(客户端零配表,靠它决定「这颗能嵌进哪些孔」)。
		// 判据与镶嵌入口是同一个(GemRowIfGem);不是宝石的非装备一个新字段都不写,与一期逐位一致。
		if (const EquipGemTable* gemRow = GemRowIfGem(item.config_id()); gemRow != nullptr) {
			info.set_gem_type(gemRow->gem_type());
			// 属性行不可用的宝石嵌上也不计入加成(ResolveGemsNow 的第 ③ 条),所以这里也不显示属性。
			const EquipAttributeTable* attr = AttributeRow(gemRow->attr_id());
			if (attr != nullptr && IsEffectAccepted(*attr)) {
				FillGemAttrLine(*attr, gemRow->value(), *info.mutable_gem_attr());
			}
		}
		return;
	}

	info.set_equip_level(row->equip_level());
	info.set_equip_class(row->equip_class());
	// equip_class_name 留空:Class 表目前没有名称列(data/schema/class_table.proto)。
	// 以后加了名称列,在这里按 row->equip_class() 查表填上即可,协议不用动。

	// 强化(二期 §4.3)。等级是按表上限夹取后的有效等级,与 CollectBonus 用的是同一个取法。
	const uint32_t maxEnhanceLevel = MaxEnhanceLevel();
	const uint32_t enhanceLevel = EffectiveEnhanceLevel(item);
	const uint32_t enhancePermyriad = EnhanceBonusPermyriad(enhanceLevel);
	info.set_enhance_level(enhanceLevel);
	info.set_enhance_max_level(maxEnhanceLevel);
	info.set_enhance_bonus_permyriad(enhancePermyriad);
	// 下一级预览:判据与 Enhance 的第 2–5 步相同(已初始化、未到上限、下一级的行存在且不是必败行),
	// 所以「带了预览」=「强化入口不会因为这几条拒绝」。金币 / 材料够不够是玩家自己的事,不在这里判。
	const EquipEnhanceTable* nextEnhanceRow = nullptr;
	if (item.has_equip() && enhanceLevel < maxEnhanceLevel) {
		const EquipEnhanceTable* candidate = EnhanceRow(enhanceLevel + 1);
		if (candidate != nullptr && candidate->success_rate() > 0) {
			nextEnhanceRow = candidate;
		}
	}
	if (nextEnhanceRow != nullptr) {
		auto* next = info.mutable_enhance_next();
		next->set_next_level(enhanceLevel + 1);
		next->set_success_rate(nextEnhanceRow->success_rate());
		next->set_cost_gold(nextEnhanceRow->cost_gold());
		next->set_bonus_permyriad(nextEnhanceRow->bonus_permyriad());
		next->set_fail_level_loss(nextEnhanceRow->fail_level_loss());
		// 「要材料」与 Enhance 同判据(material_id != 0 且 material_count > 0);不要材料时两个字段都留 0,
		// 客户端不会画出「某材料 ×0」。材料名 / 图标取材料的 Item 行,查不到留空(不编造)。
		if (nextEnhanceRow->material_id() != 0 && nextEnhanceRow->material_count() > 0) {
			next->set_material_config_id(nextEnhanceRow->material_id());
			next->set_material_count(nextEnhanceRow->material_count());
			if (const ItemTable* materialRow = ItemRow(nextEnhanceRow->material_id()); materialRow != nullptr) {
				next->set_material_name(materialRow->name());
				next->set_material_icon_key(materialRow->icon_key());
			}
		}
	}

	uint32_t baseSeq = 0;
	for (const auto& base : row->base_attr()) {
		const EquipAttributeTable* attr = AttributeRow(base.base_attr_id());
		if (attr == nullptr) {
			LOG_ERROR << "[PlayerEquip] Base attribute references missing EquipAttribute;not displayed: config=" << item.config_id()
					  << " attr_id=" << base.base_attr_id();
			continue;
		}
		if (!IsEffectAccepted(*attr)) {
			// CollectBonus 不会把这一条计入加成(ApplyEffect 拒绝),tooltip 上也就不能出现它的数值。
			LOG_ERROR << "[PlayerEquip] Base attribute effect rejected by rules;not displayed: config=" << item.config_id()
					  << " attr_id=" << attr->id() << " effect=" << attr->effect()
					  << " effect_param=" << attr->effect_param();
			continue;
		}
		// 基础属性的三个数都只从 ResolveBaseNow 取(加成、tooltip、下一级预览同口径):
		// value 已含强化增量;enhance_bonus 是其中强化带来的那一部分;0 级时 value == 表值、增量为 0。
		const uint64_t tableValue = base.base_attr_value();
		const uint64_t valueNow = ResolveBaseNow(tableValue, enhancePermyriad);
		auto* line = info.add_base_attrs();
		line->set_attr_id(attr->id());
		line->set_name(attr->name());
		line->set_tier(equiprules::ToRaw(AffixTier::kBase));
		line->set_value(valueNow);
		line->set_cap(0);  // 基础属性没有「/上限」
		line->set_percent(attr->percent() != 0);
		line->set_seq(baseSeq++);
		line->set_enhance_bonus(valueNow - tableValue);
		if (nextEnhanceRow != nullptr) {
			line->set_enhance_next_value(ResolveBaseNow(tableValue, nextEnhanceRow->bonus_permyriad()));
		}
	}

	// 显示顺序 = (tier, seq);repeated 的下标不承载语义,所以这里排好再下发。
	const auto& affixes = item.equip().affixes();
	std::vector<int> order;
	order.reserve(static_cast<std::size_t>(affixes.size()));
	for (int i = 0; i < affixes.size(); ++i) {
		if (!equiprules::IsRandomAffixTier(affixes.Get(i).tier())) {
			LOG_ERROR << "[PlayerEquip] Invalid random attribute color tier;not displayed: item_guid=" << item.item_id()
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
			LOG_ERROR << "[PlayerEquip] Random attribute references missing EquipAttribute;not displayed: item_guid=" << item.item_id()
					  << " attr_id=" << affix.attr_id();
			continue;
		}
		if (!IsEffectAccepted(*attr)) {
			// 同基础属性:不计入加成的行不显示。
			LOG_ERROR << "[PlayerEquip] Random attribute effect rejected by rules;not displayed: item_guid=" << item.item_id()
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

	// 孔位(二期 §4.3):未初始化的实例嵌不了宝石,不下发孔位。
	if (!item.has_equip()) {
		return;
	}
	// 按孔位定义逐孔下发(含空孔),socket 升序。两个口径分开:
	//   「占用」看原始记录 —— 与镶嵌入口判「已占」是同一个取法(FirstGemRecordOn),所以客户端看到的空孔
	//     就是服务器肯收的孔;
	//   「生效」看 ResolveGemsNow —— 计入加成的才带 attr。占着孔却不带 attr = 已镶但当前不生效,仍可取下。
	const std::vector<ResolvedGem> effectiveGems = ResolveGemsNow(item, *row);
	bool hasPreviousSocket = false;
	uint32_t previousSocket = 0;
	for (const EquipGemSocketTable* socketRow : SocketRowsFor(row->equip_kind())) {
		if (hasPreviousSocket && socketRow->socket() == previousSocket) {
			continue;  // 脏表里同一个孔的第二行:只认排在前面的那一行(与 CheckGemFit 取第一条一致)
		}
		hasPreviousSocket = true;
		previousSocket = socketRow->socket();

		auto* socketInfo = info.add_gem_sockets();
		socketInfo->set_socket(socketRow->socket());
		socketInfo->set_gem_type(socketRow->gem_type());
		socketInfo->set_name(socketRow->name());
		const EquipGem* occupant = FirstGemRecordOn(item, socketRow->socket());
		if (occupant == nullptr) {
			continue;  // 空孔:gem_config_id 保持 0
		}
		socketInfo->set_gem_config_id(occupant->gem_config_id());
		if (const ItemTable* gemItemRow = ItemRow(occupant->gem_config_id()); gemItemRow != nullptr) {
			socketInfo->set_gem_name(gemItemRow->name());
			socketInfo->set_gem_icon_key(gemItemRow->icon_key());
		}
		const auto effective =
			std::find_if(effectiveGems.begin(), effectiveGems.end(),
						 [socketRow](const ResolvedGem& gem) { return gem.socket == socketRow->socket(); });
		if (effective != effectiveGems.end()) {
			FillGemAttrLine(effective->attr.get(), effective->value, *socketInfo->mutable_attr());
		}
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

uint32_t PlayerEquipSystem::MaxEnhanceLevel() {
	const auto& table = EquipEnhanceTableManager::Instance();
	// 从 1 起数到第一个缺口为止。连续等级的长度不会超过行数,循环因此有界。
	// 每次现算、不缓存:表只有十几行,而缓存要自己盯热更(绕开加载回调的单表 Load() 不会通知任何人)。
	const std::size_t rowCount = table.Count();
	uint32_t level = 0;
	while (level < rowCount && table.Exists(level + 1)) {
		++level;
	}
	return level;
}

bool PlayerEquipSystem::IsGem(uint32_t configId) {
	return GemRowIfGem(configId) != nullptr;
}

// ---------------------------------------------------------------------------
// 仅供单测
// ---------------------------------------------------------------------------

void PlayerEquipSystem::SetRandomSourceForTest(std::function<uint64_t(uint64_t)> rand) {
	RandomSourceForTestSlot() = std::move(rand);
}
