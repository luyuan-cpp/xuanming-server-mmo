
#include <string>
#include "engine/core/type_define/player_guid_comp.h"

#include "thread_context/ecs_context.h"
#include "utils/registry/describe_entity.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/common/component/player_comp.pb.h"  // PlayerProfileComp(角色名只读副本)
#include "modules/bag/comp/player_bags_comp.h"       // 存盘前确认背包组件在场(bag_marshal 对缺组件输出空记录)
#include "modules/mission/comp/mission_comp.h"       // 同上,任务容器
#include "modules/currency/comp/player_currency_comp.h"
#include "engine/core/utils/proto/unknown_field_carry.h"
#include "services/scene/player/comp/player_persist_carry_comp.h"
#include "player_skill.h"
#include "player_attribute.h"
#include "player_level_rules.h"
#include "player_pet.h"
#include "bag_marshal.h"
#include "mission_marshal.h"
#include "player_revive.h"
// 通用资产通道:账本组件(持久化)+ 纯函数校验 + 运行时"账本损坏"标记。
// 账本必须与 currency / bag_component 走同一次 Marshal、同一次 Redis SET(不变量 I3)。
#include "proto/common/component/asset_op_ledger_comp.pb.h"
// 回合制战斗结算幂等账本(turn-battle-gap-closure.md §9.3):与 asset_op_ledger 同一条不变量 I3,
// 必须与 currency / bag_component 同记录同一次落盘,否则「资产落了标记没落」=重复发奖,反之=白丢奖励。
#include "proto/common/component/battle_settlement_ledger_comp.pb.h"
// 玩家收件箱消费端账本(player-inbox-service.md §4.4):与资产账本同一条不变量(IB-3 ≡ I3),
// 处置效果(CurrencyComp.blocked_types / debts)与账本必须同记录、同一次落盘。
#include "proto/common/component/player_inbox_ledger_comp.pb.h"
// 神秘商人的个人限购记录(docs/design/mystery-merchant.md §5.3):与上面几本账本同一条不变量 I3,
// 必须与 currency / bag_component 同记录同一次落盘。
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "asset_op_ledger.h"
#include "asset_op_system.h"
#include "player/comp/player_inbox_runtime_comp.h" // PlayerInboxLedgerInvalidComp
#include "player_inbox_rules.h"
#include "table/code/class_table.h"
#include "muduo/base/Logging.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"  // DerivedAttributesComp
#include "actor/attribute/system/actor_attribute_calculator.h"
#include "actor/attribute/constants/actor_state_attribute_calculator_constants.h"

namespace {
// 取 ClassTable 行喂给纯规则(player_revive.h),再把实际发生的分支打进日志。
// 规则本身与 ECS/表解耦,单测在 turn_battle_engine_test/player_revive_rule_test.cpp。
//
// maxHealth/maxMana 传 0 表示上限暂不可知(登录时 DerivedAttributesComp 还没算),
// 先按职业初值落地,由调用方在 PlayerAttributeSystem::InitializeOnLoad 之后再回满真实上限。
//
// 触发面:每次从 DB 加载玩家都会跑(首登、重登、跨 zone 迁移落地都走这条),
// 但只对 health==0 的玩家生效 —— 残血玩家(health>0)一律不动,
// 所以「残血带出战斗」(设计 D4)不受影响;战斗中的残血另由 InBattleComp 隔离。
PlayerReviveOutcome ApplyClassInitialAttributesOrReviveFromTable(
	BaseAttributesComp& attrs, uint64_t maxHealth = 0, uint64_t maxMana = 0) {
	const auto& classRows = ClassTableManager::Instance().FindAll().data();
	if (classRows.size() == 0) {
		LOG_ERROR << "[PlayerInit] ClassTable is empty;cannot initialize/revive player base attributes";
		return PlayerReviveOutcome::kUntouched;
	}
	const auto outcome =
		ApplyClassInitialAttributesOrRevive(attrs, classRows.Get(0), maxHealth, maxMana);
	if (outcome == PlayerReviveOutcome::kUntouched) {
		return outcome;
	}
	LOG_INFO << "[PlayerInit] "
			 << (outcome == PlayerReviveOutcome::kInitialized ? "New player attribute initialization" : "Full recovery after revival")
			 << ": health=" << attrs.health() << " strength=" << attrs.strength()
			 << " speed=" << attrs.speed();
	return outcome;
}

// 二级属性算完之后把 HP/MP 顶到真实上限(登录路径专用)。
// 不这么做的话阵亡玩家永远只回到 1 级初值:Recalculate 的补增量分支要求 oldMaxHealth>0,
// 而 DerivedAttributesComp 不落库、每次登录都是新的(oldMaxHealth==0),那条分支进不去;
// 随后的 min(health, max_health) 只向下夹,不向上补。
void TopUpToDerivedMax(entt::entity player, BaseAttributesComp& attrs) {
	const auto* derived = tlsEcs.actorRegistry.try_get<DerivedAttributesComp>(player);
	if (derived == nullptr) {
		return;
	}
	if (derived->max_health() > 0) {
		attrs.set_health(derived->max_health());
	}
	if (derived->max_mana() > 0) {
		attrs.set_mana(derived->max_mana());
	}
	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kHealth);
	ActorAttributeCalculatorSystem::MarkAttributeForUpdate(player, kEnergy);
	LOG_INFO << "[PlayerInit] Restored to secondary attribute limits: health=" << attrs.health()
			 << " mana=" << attrs.mana();
}

// 存盘只读:取一个持久化组件,不在就把名字记进 missing(逗号分隔),**不补建**。
// 持久化组件的唯一创建点是加载(PlayerDatabaseMessageFieldsUnmarshal),之后没有任何路径摘它们;
// 存盘时缺了说明加载不变量已被破坏。此刻补一个零值组件再落盘,等于拿空货币 / 空账本覆盖盘上的真实数据。
template <typename Comp>
const Comp* FindPersistedComp(entt::entity player, const char* name, std::string& missing)
{
	const auto* comp = tlsEcs.actorRegistry.try_get<Comp>(player);
	if (comp == nullptr)
	{
		if (!missing.empty())
		{
			missing += ',';
		}
		missing += name;
	}
	return comp;
}
}  // namespace

void ReviveBaseAttributesIfDead(BaseAttributesComp& attrs, uint64_t maxHealth, uint64_t maxMana) {
	if (attrs.health() != 0) {
		return;
	}
	ApplyClassInitialAttributesOrReviveFromTable(attrs, maxHealth, maxMana);
}

void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message){
	// 本表顶层的未知字段(新版本加的整列)与加载世代,存盘时由 PlayerDatabaseMessageFieldsMarshal 原样带回
	// (docs/design/mixed-version-persist-field-protection.md §4.2 L1)。持久数据类,唯一创建点。
	tlsEcs.actorRegistry.emplace<PlayerDatabaseCarryComp>(
		player, PlayerDatabaseCarryComp{unknown_field_carry::Capture(message), message.persist_schema_gen()});
	tlsEcs.actorRegistry.emplace<Transform>(player, message.transform());
	tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(player, message.uint64_pb_component());
	// 技能列表的清理与补初始技能要看职业与等级,放在下面两者挂上之后(PlayerSkillSystem::InitializeOnLoad)
	tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player, message.skill_list());
	tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player, message.uint32_pb_component());
	auto& baseAttrs = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player, message.derived_attributes_component());
	// 上限此刻还没算(DerivedAttributesComp 由下面的 InitializeOnLoad 产出),
	// 先按职业初值落地,拿到 outcome 决定后面要不要顶到真实上限。
	const auto reviveOutcome = ApplyClassInitialAttributesOrReviveFromTable(baseAttrs);
	const auto& levelComp = tlsEcs.actorRegistry.emplace<LevelComp>(player, message.level_component());
	// 存储等级原样携带,不按本版本上限压回(docs/design/mixed-version-persist-field-protection.md §12.1):
	// 新版本提高上限后回滚 scene、混版本跨 zone 迁移,以及上限下调(2026-09-10:200 → 85)前 GM 设过的老存档,
	// 都会带着超上限的等级进来;压回会随下一次存盘把等级永久降下来,升回新版本也恢复不了。
	// 总点数 / 面板 / 战斗快照 / 怪物参考等级等玩法一律按 playerlevel::CappedLevel 生效(player_level_rules.h),
	// 这里只告警、不改组件。
	if (levelComp.level() > playerlevel::kMaxLevel) {
		LOG_WARN << "[PlayerInit] metric=level_above_cap player_id=" << message.player_id()
				 << " stored_level=" << levelComp.level() << " cap=" << playerlevel::kMaxLevel
				 << "; stored level carried as-is, gameplay uses the cap";
	}
	tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player, message.attribute_component());
	tlsEcs.actorRegistry.emplace<PlayerPetComp>(player, message.pet_component());
	// 角色名副本:真源是 data_service 全局库 player_name,由 login 首次入场补齐进这条记录。
	// scene 只读、只原样存回,任何路径都不写 name。字段缺失时得到空名组件(战斗快照留空名,照常结算)。
	tlsEcs.actorRegistry.emplace<PlayerProfileComp>(player, message.profile_component());
    // 同一 player_database 行恢复背包资产与任务领取权；不重触发游戏事件。
    bag_marshal::Unmarshal(player, message.bag_component());
    mission_marshal::Unmarshal(player, message.mission_component());
	// 通用资产通道账本:跟着同一条 player_database 行回来。校验不过只关掉这名玩家的
	// 资产通道(挂 PlayerAssetOpLedgerInvalidComp,后续资产 RPC 一律 fail-closed),
	// 不阻断登录 —— 账本坏掉不该让玩家进不去游戏,但绝不能拿坏账本继续记账。
	auto& ledger = tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player, message.asset_op_ledger());
	if (auto err = ValidateAssetOpLedger(ledger); !err.empty()) {
		LOG_ERROR << "[AssetOp] Ledger corrupted;asset operations disabled for player player_id=" << message.player_id()
				  << " reason=" << err;
		tlsEcs.actorRegistry.emplace_or_replace<PlayerAssetOpLedgerInvalidComp>(player);
	}
	// 结算幂等账本跟着同一条 player_database 行回来。不做校验、不阻断登录:
	// 空账本的后果只是「上一局可能被重投补发一次」,而补发本身被待结算记录的条件删兜住;
	// 拿不准时宁可多判一次「已应用」也不要让玩家进不去游戏。
	tlsEcs.actorRegistry.emplace<BattleSettlementLedgerComp>(player, message.settlement_ledger());
	// 玩家收件箱消费端账本(字段 20)跟着同一条 player_database 行回来;旧存档没有这个字段 = 空账本,合法。
	// 校验不过只停用这名玩家的收件箱(挂 PlayerInboxLedgerInvalidComp,前置闸据此不再拉取),不阻断登录、
	// 不改写原数据 —— 拿坏账本继续记账会让不幂等的挂欠款被重复执行(设计 §8 #19)。
	auto& inboxLedger = tlsEcs.actorRegistry.emplace<PlayerInboxLedgerComp>(player, message.inbox_ledger());
	if (auto err = player_inbox_rules::ValidateLedger(inboxLedger); !err.empty()) {
		// metric= 标记供日志告警(评审 L13):这名玩家的处置会一直不执行,而表面上与离线无异。这一行每次加载只打一次,
		// 告警靠前置闸按同一标记周期性补打的 WARN(source=apply_gate)持续亮着,节拍对齐 [10m] 窗口
		// (player_inbox_rules::kStuckWarnEveryFailures,评审 F3)。修复时不要清空账本重建:那会让已执行过的处置再执行一次。
		LOG_ERROR << "[PlayerInbox] metric=inbox_ledger_invalid source=loader player_id=" << message.player_id()
				  << " reason=" << err << "; ledger corrupted, inbox consumption disabled for this player";
		tlsEcs.actorRegistry.emplace_or_replace<PlayerInboxLedgerInvalidComp>(player);
	}
	// 神秘商人限购记录跟着同一条 player_database 行回来。不做校验、不阻断登录:记录只跨一轮,
	// 轮次号对不上时读写两侧都按「空」处理(mystery_merchant_rules.h),坏数据最多影响当前这一轮。
	// 必须在这里 emplace:购买路径用 try_get 且缺组件即拒绝,不会现造一份空记录来放行。
	tlsEcs.actorRegistry.emplace<PlayerMysteryMerchantComp>(player, message.mystery_merchant());
	tlsEcs.actorRegistry.emplace<CurrencyComp>(player, message.currency());
	// 补缴欠款(debts)是 PlayerCurrencyComp 的运行时结构,持久化载体是
	// CurrencyComp.debts。这一对 LoadFromProto/SaveToProto 之前从未被调用过 ——
	// 于是 GM 挂上的欠款、以及 AddCurrency 已扣到一半的补缴进度(debt.paid),
	// 玩家一下线重登就整体消失,补缴机制被「重登一次」完整绕过。
	// 这里就是 PlayerCurrencyComp 的唯一创建点(实体由 InitPlayerFromAllData 新建,此前没有路径会挂它),
	// 用 emplace 表达「创建」而不是「有就复用」:若将来有人在加载前提前挂了它,断言会立刻指出重复创建。
	tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player).LoadFromProto(message.currency());
	// 技能:只剔除 id 为 0 / 重复的坏条目(其余条目原样携带,按本版本技能表生效)+ 按当前职业补初始技能。
	// 必须在属性重算之前 —— 被动技能的加成是重算的输入之一(docs/design/class-skills.md §3.2 / §7)
	PlayerSkillSystem::InitializeOnLoad(player);
	// 属性加点:补默认方案 + 按生效等级 / 方案重算二级属性(max_health 等此前无写者,战斗快照靠它)。
	// 加载与重算不改写、不返还 allocated:已知维度超过本版本总量(或单项上限)的池整池按 0 生效,
	// 本版本不认识的维度键原样携带、不生效(attribute_allocation_rules.h)
	PlayerAttributeSystem::InitializeOnLoad(player);
	// 宝宝:等级跟随主人生效等级,只升不降 + 按上限夹当前 HP/MP;加点与角色同一规则,超额的池不生效、
	// 不清空。必须在主人等级已 emplace 之后
	PetSystem::InitializeOnLoad(player);
	// 新号/复活的玩家在这里才拿得到真实上限,顶满(活着的玩家不碰,保住残血语义)
	if (reviveOutcome != PlayerReviveOutcome::kUntouched) {
		TopUpToDerivedMax(player, baseAttrs);
	}
}

bool PlayerDatabaseMessageFieldsMarshal(entt::entity player, player_database& message){
	// 存盘只读当前状态,不改变实体的组件集合。校验前置:先确认全部持久化组件都在,再写 message;
	// 缺任何一个都整份放弃、message 原样不动并返回 false,由调用方放弃这次写入(fail-closed)。
	// 旧写法用 get_or_emplace 取组件:组件一旦缺失就当场造出零值组件,把零货币、空账本当真实数据
	// 写进 Redis 与 DB —— 账本被清空还会让已应用的资产指令 / 战斗结算被重复应用。
	std::string missing;
	const auto* transform = FindPersistedComp<Transform>(player, "Transform", missing);
	const auto* uint64Comp = FindPersistedComp<PlayerUint64Comp>(player, "PlayerUint64Comp", missing);
	const auto* skillList = FindPersistedComp<PlayerSkillListComp>(player, "PlayerSkillListComp", missing);
	const auto* uint32Comp = FindPersistedComp<PlayerUint32Comp>(player, "PlayerUint32Comp", missing);
	const auto* baseAttributes = FindPersistedComp<BaseAttributesComp>(player, "BaseAttributesComp", missing);
	const auto* level = FindPersistedComp<LevelComp>(player, "LevelComp", missing);
	const auto* attribute = FindPersistedComp<PlayerAttributeComp>(player, "PlayerAttributeComp", missing);
	const auto* pet = FindPersistedComp<PlayerPetComp>(player, "PlayerPetComp", missing);
	const auto* profile = FindPersistedComp<PlayerProfileComp>(player, "PlayerProfileComp", missing);
	// 背包 / 任务由各自的 marshal 读。它们对缺组件输出的是「权威的空记录」(任务还会标 scoped_state_present),
	// 落盘就等于清空玩家的物品与任务,所以存盘侧必须在这里先拦住。
	FindPersistedComp<PlayerBagsComp>(player, "PlayerBagsComp", missing);
	FindPersistedComp<MissionsContainerComp>(player, "MissionsContainerComp", missing);
	const auto* assetOpLedger = FindPersistedComp<PlayerAssetOpLedgerComp>(player, "PlayerAssetOpLedgerComp", missing);
	const auto* settlementLedger =
		FindPersistedComp<BattleSettlementLedgerComp>(player, "BattleSettlementLedgerComp", missing);
	// 收件箱账本缺失同样整份拒存:补一本空账本落盘,会让已执行过的挂欠款在下次投递时被判成"没见过"再挂一次。
	const auto* inboxLedger = FindPersistedComp<PlayerInboxLedgerComp>(player, "PlayerInboxLedgerComp", missing);
	// 限购记录缺失同样整份拒存:补一份空记录落盘,等于把这一轮已经买过的名额清零。
	const auto* mysteryMerchant =
		FindPersistedComp<PlayerMysteryMerchantComp>(player, "PlayerMysteryMerchantComp", missing);
	const auto* currency = FindPersistedComp<CurrencyComp>(player, "CurrencyComp", missing);
	const auto* currencyRuntime = FindPersistedComp<PlayerCurrencyComp>(player, "PlayerCurrencyComp", missing);
	// 顶层未知字段缺了载体同样整份拒存:补空等于把新版本写进来的整列抹掉(混版本设计 §4.2 L1)。
	const auto* carry = FindPersistedComp<PlayerDatabaseCarryComp>(player, "PlayerDatabaseCarryComp", missing);
	if (!missing.empty())
	{
		const auto* guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		LOG_ERROR << "[PlayerSave] Persisted component(s) missing, refusing to marshal player_database"
				  << " (load invariant broken; nothing is written): entity=" << entt::to_integral(player)
				  << " player_id=" << (guid != nullptr ? guid->value : kInvalidGuid) << " missing=" << missing
				  << " " << DescribeEntity(tlsEcs.actorRegistry, player);
		return false;
	}
	// 校验前置:携带的字节先解析好,解析失败时 message 一个字段都还没动。
	google::protobuf::UnknownFieldSet carriedUnknown;
	if (!unknown_field_carry::Parse(carry->topLevelUnknown, carriedUnknown))
	{
		LOG_ERROR << "[PlayerSave] carried top-level unknown fields of player_database are corrupt; refusing to marshal: entity="
				  << entt::to_integral(player);
		return false;
	}

	message.mutable_transform()->CopyFrom(*transform);
	message.mutable_uint64_pb_component()->CopyFrom(*uint64Comp);
	message.mutable_skill_list()->CopyFrom(*skillList);
	message.mutable_uint32_pb_component()->CopyFrom(*uint32Comp);
	message.mutable_derived_attributes_component()->CopyFrom(*baseAttributes);
	message.mutable_level_component()->CopyFrom(*level);
	message.mutable_attribute_component()->CopyFrom(*attribute);
	message.mutable_pet_component()->CopyFrom(*pet);
	// 名字副本原样存回(scene 不改它)。
	message.mutable_profile_component()->CopyFrom(*profile);
    bag_marshal::Marshal(player, *message.mutable_bag_component());
    mission_marshal::Marshal(player, *message.mutable_mission_component());
	// 资产通道账本与 currency / bag_component 同记录同 DBTask(不变量 I3)。
	// 位置必须在下面那对 currency CopyFrom / SaveToProto **之外** —— 夹在中间会把
	// 刚写进去的补缴欠款抹掉(见下条注释)。
	message.mutable_asset_op_ledger()->CopyFrom(*assetOpLedger);
	// 结算幂等账本:与上一行同理由(不变量 I3)。位置同样必须在下面那对 currency
	// CopyFrom / SaveToProto **之外**,夹在中间会把刚写进去的补缴欠款抹掉。
	message.mutable_settlement_ledger()->CopyFrom(*settlementLedger);
	// 玩家收件箱消费端账本(字段 20):与上两条同理由(IB-3,与 I3 同构)—— GM 处置的效果
	// (封锁在 CurrencyComp.blocked_types、欠款经 SaveToProto 写进 CurrencyComp.debts)与账本同记录、
	// 同一次 Marshal。位置同样必须在下面那对 currency CopyFrom / SaveToProto **之外**:那一对的先后决定
	// 欠款会不会被抹掉,任何新写入都不夹进它们中间。
	message.mutable_inbox_ledger()->CopyFrom(*inboxLedger);
	// 神秘商人限购记录:与上面几本账本同理由(不变量 I3),位置同样在下面那对 currency CopyFrom / SaveToProto 之外。
	message.mutable_mystery_merchant()->CopyFrom(*mysteryMerchant);
	message.mutable_currency()->CopyFrom(*currency);
	// 必须在 CopyFrom 之后:CopyFrom 会覆盖整个 currency 子消息(含 debts),
	// 反序会把刚写进去的欠款抹掉。
	if (!currencyRuntime->SaveToProto(*message.mutable_currency()))
	{
		// 欠款携带的未知字段坏了(只可能是内存被破坏)。唯一调用方 PlayerAllDataMessageFieldsMarshal 在副本上写,
		// 返回 false 时原输出不动。
		LOG_ERROR << "[PlayerSave] carried unknown fields of a currency debt are corrupt; refusing to marshal: entity="
				  << entt::to_integral(player);
		return false;
	}
	unknown_field_carry::Replace(carriedUnknown, message);
	return true;
}
