#pragma once

// 玩家换场景节点的窗口里,哪些客户端消息留给 gate 重投、哪些照旧在本节点处置、哪些不在新场景里执行的判定
// (docs/design/client-scene-request-handoff-hold.md §4.3)。
//
// 接线在 SceneHandler::ProcessClientPlayerMessage(客户端消息进 scene 的唯一入口):
//   - ShouldAnswerRetryLater 命中:不派发,应答不带 message_content、dispatch_verdict = RETRY_LATER,gate 的场景 lane
//     把这条请求放回会话队头,在有界等待内按收包顺序重投到玩家当时所在的节点;
//   - IsStaleForThisScene 命中:这条消息是 gate 在会话还绑在别的场景路由上时收到的(它在 gate 里排着等过一次换场景或
//     换节点,换完才发到这里),而它的含义绑在当时那个场景上:不派发,请求-响应类不带 message_content(gate 回 1003),
//     单向消息不回包。
//
// 为什么在入口统一判、不靠各业务系统的冻结闸:
//   - 交接期间本节点手里的状态已经落盘且不得再写(player_frozen_comp.h)。写请求派发了也只会被各自的冻结闸拒掉,
//     而且各系统回的码不一样(kEquipFrozen / kInvalidParameter / kSkillUpgradeBusy …),客户端分不清「稍后再试」与「参数错」;
//   - 只读请求在本节点答了也是马上作废的数据;留给 gate,交接放行后由目标节点按最新状态答。
// 各业务系统自己的冻结闸仍然保留,作纵深防御(不在名单里的请求、节点内部触发的写都不经过这里)。
//
// 单独成头文件、只读 ECS 与描述符,是为了在 cross_zone_test 里直接测(player_handoff_gate_test.cpp),与 player_gm_guard.h 同一做法。
// 线程:只在 scene loop 线程调用;不改变实体的组件集合(AGENTS.md §7 不变量 5)。

#include <cstdint>

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
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "rpc/service_metadata/player_skill_service_metadata.h"
#include "thread_context/ecs_context.h"

namespace scene_handoff_gate
{
	// 这个方法的消息能不能换一个场景节点(或换一个场景)再执行。
	//
	// 能:只读写玩家自己的状态,不引用当前场景里的东西。玩家落到哪个节点、哪张图,这些消息的含义都不变。
	//   - 整个服务都是这一类的:背包与装备、属性、宝宝、任务、货币、活跃度、战斗结果;
	//   - 技能服务里的看技能、升技能(同一服务里的放技能绑在场景上,所以这个服务按方法列);
	//   - 场景服务里的取版本摘要(读玩家自己的各域版本)与查场景信息(答的就是此刻所在的场景,落地后答新场景正是客户端要的),
	//     两者都是单向消息,不会被扣住重投,登记在这里只为不被当成旧场景的消息丢掉(IsStaleForThisScene)。
	// 不能:含义绑在当前场景上的消息。换到新场景再执行是错的,必须照旧当场处置:
	//   - 换图 / 跨 zone 传送(SceneSceneClientPlayer):交接中再来一次是重复请求,handler 有自己的在途闸
	//     (kEnterSceneChangingScene);留到落地之后执行,会变成第二次换图;
	//   - 瞬移、移动、跟随(SceneMovementClientPlayer):坐标是旧场景的;
	//   - 放技能(SceneSkillClientPlayer.ReleaseSkill):目标与位置是旧场景的;
	//   - 神秘商人(SceneMysteryMerchantClientPlayer):购买要求玩家与商人在同一场景且在交互距离内
	//     (PlayerMysteryMerchantSystem::Buy 的 kMysteryMerchantNotHere / kMysteryMerchantTooFar);
	//   - ExitGame 等生命周期方法(ScenePlayer):退出优先,不排队。
	//
	// 默认「不能」:新加的服务或方法不登记就保持改造前的行为(当场处置),漏登记只是少一点无感,不会出错。
	// 往整服务名单里的服务加方法之前,先确认它不引用当前场景;引用的话放到别的服务里,或把那个服务改成按方法列。
	// 用生成的服务描述符与方法下标比较,不用名字字符串:服务或方法改名时这里编译不过,不会悄悄失效。
	inline bool IsReplayableOnAnotherSceneNode(const google::protobuf::MethodDescriptor &method)
	{
		const google::protobuf::ServiceDescriptor *const service = method.service();
		if (service == SceneBagClientPlayer::descriptor() ||
			service == SceneAttributeClientPlayer::descriptor() ||
			service == ScenePetClientPlayer::descriptor() ||
			service == SceneMissionClientPlayer::descriptor() ||
			service == SceneCurrencyClientPlayer::descriptor() ||
			service == SceneActivityClientPlayer::descriptor() ||
			service == SceneBattleResultClientPlayer::descriptor())
		{
			return true;
		}
		const auto index = static_cast<uint32_t>(method.index());
		if (service == SceneSkillClientPlayer::descriptor())
		{
			return index == SceneSkillClientPlayerListSkillsIndex || index == SceneSkillClientPlayerUpgradeSkillIndex;
		}
		if (service == SceneSceneClientPlayer::descriptor())
		{
			return index == SceneSceneClientPlayerGetSyncDigestIndex || index == SceneSceneClientPlayerSceneInfoC2SIndex;
		}
		return false;
	}

	// 这条客户端消息在本节点暂时受理不了时,能不能答复 gate「稍后重投」:
	// 必须是请求-响应消息(应答不是 Empty;单向消息没有应答可带处置,gate 也不重投),且换节点执行含义不变。
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

	// 这条消息是不是「在旧场景里发出、换完场景才送到」、而且含义绑在旧场景上(设计 S13)。
	// receivedOnPreviousRoute 是 gate 在转发信封上标的(scene.proto ProcessClientPlayerMessageRequest.received_on_previous_route):
	// gate 收到它时会话绑的场景路由与发出时不同。改造前这类消息在交接窗口里一律被拒(冻结闸、实体不在),
	// 从不在新场景里执行;gate 的场景 lane 让它排着等过换场景之后,这里保持同一结果。换节点执行含义不变的照常执行。
	inline bool IsStaleForThisScene(const google::protobuf::MethodDescriptor &method, bool receivedOnPreviousRoute)
	{
		return receivedOnPreviousRoute && !IsReplayableOnAnotherSceneNode(method);
	}

	// 入口(ProcessClientPlayerMessage)查到的玩家在本节点的状态,按入口的判断顺序排列。
	enum class EntryState : uint8_t
	{
		kSessionUnknown,  // 会话不在本节点的 SessionMap 里:玩家刚被放行去了别的节点(gate 的路由还没切过来),或入场登记还没走完
		kPlayerNotLoaded, // 会话在,玩家实体还没建出来:存档载入中
		kPlayerPresent,   // 玩家实体在本节点
	};

	// 入口对这条客户端消息是否答复「稍后重投」(不派发、应答不带 message_content)。三种状态的规则收在这一处:
	//   - 会话不在 / 实体没载入:本来就派发不了;换节点执行含义不变的请求让 gate 稍后重投,其余照旧空应答(gate 回 1003);
	//   - 实体在:只有归属交接在途时才留给 gate,其余照常往下走(旧场景消息闸、GM 闸、热关停、handler)。
	// 调用方在 kPlayerPresent 时要先过退出闸(退出优先),再问这里;player 只在 kPlayerPresent 时使用。
	inline bool ShouldAnswerRetryLater(EntryState state, entt::entity player, const google::protobuf::MethodDescriptor &method)
	{
		switch (state)
		{
		case EntryState::kSessionUnknown:
		case EntryState::kPlayerNotLoaded:
			return MayBeRetriedLater(method);
		case EntryState::kPlayerPresent:
			return IsHeldForOwnershipHandoff(player, method);
		}
		return false;
	}
} // namespace scene_handoff_gate
