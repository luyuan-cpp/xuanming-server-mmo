
#include "thread_context/ecs_context.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "player_skill.h"
#include "player_attribute.h"
#include "player_pet.h"
#include "bag_marshal.h"
#include "mission_marshal.h"
#include "player_revive.h"
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
		LOG_ERROR << "[PlayerInit] ClassTable 为空,无法初始化/复活玩家基础属性";
		return PlayerReviveOutcome::kUntouched;
	}
	const auto outcome =
		ApplyClassInitialAttributesOrRevive(attrs, classRows.Get(0), maxHealth, maxMana);
	if (outcome == PlayerReviveOutcome::kUntouched) {
		return outcome;
	}
	LOG_INFO << "[PlayerInit] "
			 << (outcome == PlayerReviveOutcome::kInitialized ? "新号初始化属性" : "阵亡复活回满")
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
	LOG_INFO << "[PlayerInit] 按二级属性上限回满: health=" << attrs.health()
			 << " mana=" << attrs.mana();
}
}  // namespace

void ReviveBaseAttributesIfDead(BaseAttributesComp& attrs, uint64_t maxHealth, uint64_t maxMana) {
	if (attrs.health() != 0) {
		return;
	}
	ApplyClassInitialAttributesOrReviveFromTable(attrs, maxHealth, maxMana);
}

void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message){
	tlsEcs.actorRegistry.emplace<Transform>(player, message.transform());
	tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(player, message.uint64_pb_component());
	tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player, message.skill_list());
	PlayerSkillSystem::SanitizeSkillList(player);
	tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player, message.uint32_pb_component());
	auto& baseAttrs = tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player, message.derived_attributes_component());
	// 上限此刻还没算(DerivedAttributesComp 由下面的 InitializeOnLoad 产出),
	// 先按职业初值落地,拿到 outcome 决定后面要不要顶到真实上限。
	const auto reviveOutcome = ApplyClassInitialAttributesOrReviveFromTable(baseAttrs);
	auto& levelComp = tlsEcs.actorRegistry.emplace<LevelComp>(player, message.level_component());
	// 等级上限下调(2026-09-10:200 → 85)前 GM 设过的超限存档、以及回档到这类快照,都在这里压回上限。
	// 不压的话总点数 / 面板 / 战斗快照 / 怪物参考等级都按超限等级算;压完后下面 InitializeOnLoad 的
	// "已分配 > 总量"收敛会整池返还多出的点,宝宝等级(跟随主人)也随之回到上限内。
	if (const auto clamped = playerlevel::ClampToMaxLevel(levelComp.level()); clamped != levelComp.level()) {
		LOG_WARN << "[PlayerInit] 存档等级超上限,压回上限: player_id=" << message.player_id()
				 << " level=" << levelComp.level() << " -> " << clamped;
		levelComp.set_level(clamped);
	}
	tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(player, message.attribute_component());
	tlsEcs.actorRegistry.emplace<PlayerPetComp>(player, message.pet_component());
    // 同一 player_database 行恢复背包资产与任务领取权；不重触发游戏事件。
    bag_marshal::Unmarshal(player, message.bag_component());
    mission_marshal::Unmarshal(player, message.mission_component());
	tlsEcs.actorRegistry.emplace<CurrencyComp>(player, message.currency());
	// 补缴欠款(debts)是 PlayerCurrencyComp 的运行时结构,持久化载体是
	// CurrencyComp.debts。这一对 LoadFromProto/SaveToProto 之前从未被调用过 ——
	// 于是 GM 挂上的欠款、以及 AddCurrency 已扣到一半的补缴进度(debt.paid),
	// 玩家一下线重登就整体消失,补缴机制被「重登一次」完整绕过。
	tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player).LoadFromProto(message.currency());
	// 属性加点:补默认方案 + 按等级/方案重算二级属性(max_health 等此前无写者,战斗快照靠它)
	PlayerAttributeSystem::InitializeOnLoad(player);
	// 宝宝:同步等级(跟随主人)+ 按上限夹当前 HP/MP;必须在主人等级已 emplace 之后
	PetSystem::InitializeOnLoad(player);
	// 新号/复活的玩家在这里才拿得到真实上限,顶满(活着的玩家不碰,保住残血语义)
	if (reviveOutcome != PlayerReviveOutcome::kUntouched) {
		TopUpToDerivedMax(player, baseAttrs);
	}
}

void PlayerDatabaseMessageFieldsMarshal(entt::entity player, player_database& message){
	message.mutable_transform()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<Transform>(player));
	message.mutable_uint64_pb_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerUint64Comp>(player));
	message.mutable_skill_list()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player));
	message.mutable_uint32_pb_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerUint32Comp>(player));
	message.mutable_derived_attributes_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<BaseAttributesComp>(player));
	message.mutable_level_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<LevelComp>(player));
	message.mutable_attribute_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerAttributeComp>(player));
	message.mutable_pet_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerPetComp>(player));
    bag_marshal::Marshal(player, *message.mutable_bag_component());
    mission_marshal::Marshal(player, *message.mutable_mission_component());
	message.mutable_currency()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<CurrencyComp>(player));
	// 必须在 CopyFrom 之后:CopyFrom 会覆盖整个 currency 子消息(含 debts),
	// 反序会把刚写进去的欠款抹掉。
	tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player).SaveToProto(*message.mutable_currency());
}
