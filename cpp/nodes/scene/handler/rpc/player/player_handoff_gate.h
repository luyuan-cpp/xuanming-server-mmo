#pragma once

// 玩家换场景节点的窗口里,哪些客户端请求留给 gate 重投、哪些照旧在本节点处置的判定
// (docs/design/client-scene-request-handoff-hold.md §4.3)。
//
// 接线在 SceneHandler::ProcessClientPlayerMessage(客户端消息进 scene 的唯一入口)。命中时应答不带 message_content、
// dispatch_verdict = RETRY_LATER,gate 的场景 lane 把这条请求放回会话队头,在有界等待内按收包顺序重投到玩家当时所在的节点。
//
// 为什么在入口统一判、不靠各业务系统的冻结闸:
//   - 交接期间本节点手里的状态已经落盘且不得再写(player_frozen_comp.h)。写请求派发了也只会被各自的冻结闸拒掉,
//     而且各系统回的码不一样(kEquipFrozen / kInvalidParameter / kSkillUpgradeBusy …),客户端分不清「稍后再试」与「参数错」;
//   - 只读请求在本节点答了也是马上作废的数据;留给 gate,交接放行后由目标节点按最新状态答。
// 各业务系统自己的冻结闸仍然保留,作纵深防御(不在名单里的请求、单向消息、节点内部触发的写都不经过这里)。
//
// 单独成头文件、只读 ECS 与描述符,是为了在 cross_zone_test 里直接测(player_handoff_gate_test.cpp),与 player_gm_guard.h 同一做法。
// 线程:只在 scene loop 线程调用;不改变实体的组件集合(AGENTS.md §7 不变量 5)。

#include <google/protobuf/descriptor.h>

#include "entt/src/entt/entity/entity.hpp"
#include "player/comp/player_ownership_comp.h"
#include "proto/common/base/empty.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_battle_result.pb.h"
#include "proto/scene/player_currency.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "thread_context/ecs_context.h"

namespace scene_handoff_gate
{
	// 这个方法的请求能不能换一个场景节点再执行。
	//
	// 能:只读写玩家自己的持久状态(背包与装备、属性、宝宝、任务、货币、活跃度、战斗结果),不引用当前场景里的东西。
	//     玩家落到哪个节点、哪张图,这些请求的含义都不变。
	// 不能:含义绑在当前场景上的请求。换到新场景再执行是错的,必须照旧当场处置:
	//   - 换图 / 跨 zone 传送(SceneSceneClientPlayer):交接中再来一次是重复请求,handler 有自己的在途闸
	//     (kEnterSceneChangingScene);留到落地之后执行,会变成第二次换图;
	//   - 瞬移、移动(SceneMovementClientPlayer):坐标是旧场景的;
	//   - 放技能(SceneSkillClientPlayer):目标与位置是旧场景的。同一服务里的学技能、升技能因此也不在名单里,
	//     它们照旧由 handler 的冻结闸当场拒绝;
	//   - 神秘商人(SceneMysteryMerchantClientPlayer):与场景内的导航点有关,没有逐一核对,先不放进来;
	//   - ExitGame 等生命周期方法(ScenePlayer):退出优先,不排队。
	//
	// 按服务列白名单,默认「不能」:新加的服务不登记就保持改造前的行为(当场处置),漏登记只是少一点无感,不会出错。
	// 往白名单里的服务加方法之前,先确认它不引用当前场景;引用的话放到别的服务里。
	// 用生成的服务描述符比较而不是服务名字符串:服务改名时这里编译不过,不会悄悄失效。
	inline bool IsReplayableOnAnotherSceneNode(const google::protobuf::MethodDescriptor &method)
	{
		const google::protobuf::ServiceDescriptor *const service = method.service();
		return service == SceneBagClientPlayer::descriptor() ||
			   service == SceneAttributeClientPlayer::descriptor() ||
			   service == ScenePetClientPlayer::descriptor() ||
			   service == SceneMissionClientPlayer::descriptor() ||
			   service == SceneCurrencyClientPlayer::descriptor() ||
			   service == SceneActivityClientPlayer::descriptor() ||
			   service == SceneBattleResultClientPlayer::descriptor();
	}

	// 这条客户端消息在本节点暂时受理不了时,能不能答复 gate「稍后重投」:
	// 必须是请求-响应消息(应答不是 Empty;单向消息没有应答可带处置,gate 也不重投),且换节点执行含义不变。
	// 三个使用点(ProcessClientPlayerMessage):会话不在本节点、玩家实体还没载入、归属交接在途。
	inline bool MayBeRetriedLater(const google::protobuf::MethodDescriptor &method)
	{
		return method.output_type() != Empty::descriptor() && IsReplayableOnAnotherSceneNode(method);
	}

	// 归属交接在途(PlayerTravelHandoffComp:跨节点换图 / 跨 zone 传送 / 服务器发起的改派,从冻结到交接出结果)时,
	// 这条客户端消息是否留给 gate 重投(本节点不派发)。player 由调用方确认有效。
	// 只挂 PlayerFrozenComp、没有 PlayerTravelHandoffComp 的冻结(账号角色交易封存)不算:那不是换节点,等不到「目标节点」,
	// 留给 gate 只会白等到超时;照旧派发,由各业务系统的冻结闸当场拒绝。
	inline bool IsHeldForOwnershipHandoff(entt::entity player, const google::protobuf::MethodDescriptor &method)
	{
		return MayBeRetriedLater(method) && tlsEcs.actorRegistry.any_of<PlayerTravelHandoffComp>(player);
	}
} // namespace scene_handoff_gate
