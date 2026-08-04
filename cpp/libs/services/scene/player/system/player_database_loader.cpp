
#include "thread_context/ecs_context.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "player_skill.h"

void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message){
	tlsEcs.actorRegistry.emplace<Transform>(player, message.transform());
	tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(player, message.uint64_pb_component());
	tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player, message.skill_list());
	PlayerSkillSystem::SanitizeSkillList(player);
	tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player, message.uint32_pb_component());
	tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player, message.derived_attributes_component());
	tlsEcs.actorRegistry.emplace<LevelComp>(player, message.level_component());
	tlsEcs.actorRegistry.emplace<CurrencyComp>(player, message.currency());
	// 补缴欠款(debts)是 PlayerCurrencyComp 的运行时结构,持久化载体是
	// CurrencyComp.debts。这一对 LoadFromProto/SaveToProto 之前从未被调用过 ——
	// 于是 GM 挂上的欠款、以及 AddCurrency 已扣到一半的补缴进度(debt.paid),
	// 玩家一下线重登就整体消失,补缴机制被「重登一次」完整绕过。
	tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player).LoadFromProto(message.currency());
}

void PlayerDatabaseMessageFieldsMarshal(entt::entity player, player_database& message){
	message.mutable_transform()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<Transform>(player));
	message.mutable_uint64_pb_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerUint64Comp>(player));
	message.mutable_skill_list()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player));
	message.mutable_uint32_pb_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<PlayerUint32Comp>(player));
	message.mutable_derived_attributes_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<BaseAttributesComp>(player));
	message.mutable_level_component()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<LevelComp>(player));
	message.mutable_currency()->CopyFrom(tlsEcs.actorRegistry.get_or_emplace<CurrencyComp>(player));
	// 必须在 CopyFrom 之后:CopyFrom 会覆盖整个 currency 子消息(含 debts),
	// 反序会把刚写进去的欠款抹掉。
	tlsEcs.actorRegistry.get_or_emplace<PlayerCurrencyComp>(player).SaveToProto(*message.mutable_currency());
}
