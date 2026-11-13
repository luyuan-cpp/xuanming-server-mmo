// 玩家换场景节点的窗口里,哪些客户端请求留给 gate 重投的判定(nodes/scene/handler/rpc/player/player_handoff_gate.h,
// docs/design/client-scene-request-handoff-hold.md §4.3)。
//
// 回归对象(2026-11-12):换场景节点的窗口里,客户端的请求原先一律被拒(冻结期间各系统各回各的码,其余窗口 1003),
// 由客户端自己重发。现在换节点执行含义不变的请求留给 gate 按序重投。这里钉住两件事:
//   - 白名单:只有读写玩家自身持久状态的服务可以换节点再执行;绑在当前场景上的请求(换图、瞬移、放技能)必须照旧当场处置,
//     否则它们会在落地之后被拿到新场景里执行;
//   - 交接判定:只认归属交接(PlayerTravelHandoffComp),不认别的冻结;
//   - 旧场景消息闸(设计 S13):在 gate 里排着等过换场景、换完才送到的消息,含义绑在旧场景上的不执行。
// 接线在 SceneHandler::ProcessClientPlayerMessage,节点层入口没有单测宿主,所以判定抽成头文件在这里直接测
// (与 player_gm_guard.h 同一做法)。

#include <gtest/gtest.h>

#include <google/protobuf/descriptor.h>

#include "../../nodes/scene/handler/rpc/player/player_handoff_gate.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "proto/scene/player_lifecycle.pb.h"
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
	using scene_handoff_gate::IsHeldForOwnershipHandoff;
	using scene_handoff_gate::IsReplayableOnAnotherSceneNode;
	using scene_handoff_gate::IsStaleForThisScene;
	using scene_handoff_gate::MayBeRetriedLater;

	struct NamedMethod
	{
		const google::protobuf::ServiceDescriptor *service;
		const char *method;
	};

	// 找不到就返回空:方法改名时用例在 ASSERT 上失败,不解引用空指针。
	const google::protobuf::MethodDescriptor *Find(const NamedMethod &named)
	{
		return named.service->FindMethodByName(named.method);
	}

	// 用完即销毁的玩家实体:本工程的用例共用 tlsEcs,不能把实体留给后面的用例。
	struct ScopedPlayer
	{
		entt::entity entity{tlsEcs.actorRegistry.create()};
		~ScopedPlayer() { tlsEcs.actorRegistry.destroy(entity); }
	};
} // namespace

TEST(PlayerHandoffGate, PlayerStateRequestsMayBeRetriedOnAnotherNode)
{
	const NamedMethod kPlayerStateRequests[] = {
		{SceneBagClientPlayer::descriptor(), "GetBag"},
		{SceneBagClientPlayer::descriptor(), "EquipItem"},
		{SceneAttributeClientPlayer::descriptor(), "AllocateAttributePoints"},
		{ScenePetClientPlayer::descriptor(), "SummonPet"},
		{SceneMissionClientPlayer::descriptor(), "ClaimMissionReward"},
		{SceneCurrencyClientPlayer::descriptor(), "GetCurrencyList"},
		{SceneActivityClientPlayer::descriptor(), "ClaimActivityVitalityReward"},
		{SceneBattleResultClientPlayer::descriptor(), "AckBattleResults"},
		// 技能服务按方法列:看技能、升技能读写的是玩家自己的技能表。
		{SceneSkillClientPlayer::descriptor(), "ListSkills"},
		{SceneSkillClientPlayer::descriptor(), "UpgradeSkill"},
	};
	for (const auto &named : kPlayerStateRequests)
	{
		const auto *method = Find(named);
		ASSERT_NE(method, nullptr) << named.method;
		EXPECT_TRUE(MayBeRetriedLater(*method)) << method->full_name();
	}
}

TEST(PlayerHandoffGate, SceneBoundRequestsAreNeverRetriedOnAnotherNode)
{
	// 换到新场景再执行是错的:重复的换图会变成第二次换图,瞬移与放技能带的是旧场景的坐标和目标,
	// 神秘商人的购买要求玩家与商人在同一场景且在交互距离内。退出优先,不排队。
	const NamedMethod kSceneBoundRequests[] = {
		{SceneSceneClientPlayer::descriptor(), "EnterScene"},
		{SceneSceneClientPlayer::descriptor(), "TravelToZone"},
		{SceneMovementClientPlayer::descriptor(), "TeleportRequest"},
		{SceneSkillClientPlayer::descriptor(), "ReleaseSkill"},
		{SceneMysteryMerchantClientPlayer::descriptor(), "GetMysteryMerchantInfo"},
		{SceneMysteryMerchantClientPlayer::descriptor(), "BuyMysteryMerchantGoods"},
		{ScenePlayer::descriptor(), "ExitGame"},
	};
	for (const auto &named : kSceneBoundRequests)
	{
		const auto *method = Find(named);
		ASSERT_NE(method, nullptr) << named.method;
		EXPECT_FALSE(IsReplayableOnAnotherSceneNode(*method)) << method->full_name();
		EXPECT_FALSE(MayBeRetriedLater(*method)) << method->full_name();
	}
}

TEST(PlayerHandoffGate, OneWayMethodIsNeverRetriedEvenInAReplayableService)
{
	// 应答为 Empty 的方法没有应答可带处置,gate 也不重投。属性服务在白名单里,但它的推送方法应答是 Empty。
	const auto *push = Find({SceneAttributeClientPlayer::descriptor(), "NotifyAttributePanelChanged"});
	ASSERT_NE(push, nullptr);
	EXPECT_TRUE(IsReplayableOnAnotherSceneNode(*push));
	EXPECT_FALSE(MayBeRetriedLater(*push));

	const auto *move = Find({SceneMovementClientPlayer::descriptor(), "MoveStart"});
	ASSERT_NE(move, nullptr);
	EXPECT_FALSE(MayBeRetriedLater(*move));

	// 取版本摘要的结果走推送(NotifySyncDigest),方法本身的应答是 Empty:gate 当它是单向消息,不扣住重投;
	// 没等到推送由客户端按 landing-recovery-sync-digest.md §4.3 自己重发。它换节点含义不变(见 SceneServiceIsListedPerMethod)。
	const auto *digest = Find({SceneSceneClientPlayer::descriptor(), "GetSyncDigest"});
	ASSERT_NE(digest, nullptr);
	EXPECT_FALSE(MayBeRetriedLater(*digest));
}

// 技能服务里只有看技能、升技能可以换节点再执行;以后往这个服务加方法,默认不带,要带就在 player_handoff_gate.h 里登记。
TEST(PlayerHandoffGate, SkillServiceIsListedPerMethod)
{
	const google::protobuf::ServiceDescriptor *const skill = SceneSkillClientPlayer::descriptor();
	for (int i = 0; i < skill->method_count(); ++i)
	{
		const google::protobuf::MethodDescriptor *const method = skill->method(i);
		const bool expected = method->name() == "ListSkills" || method->name() == "UpgradeSkill";
		EXPECT_EQ(IsReplayableOnAnotherSceneNode(*method), expected) << method->full_name();
	}
}

TEST(PlayerHandoffGate, PlayerStateRequestDuringOwnershipHandoffIsLeftToTheGate)
{
	const auto *equipItem = Find({SceneBagClientPlayer::descriptor(), "EquipItem"});
	ASSERT_NE(equipItem, nullptr);

	ScopedPlayer player;
	EXPECT_FALSE(IsHeldForOwnershipHandoff(player.entity, *equipItem)) << "没有交接在途时照常派发";

	// StartTravelHandoff 成对挂上两个组件;判定只看交接意图。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player.entity);
	tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player.entity);
	EXPECT_TRUE(IsHeldForOwnershipHandoff(player.entity, *equipItem));

	// 交接未成(AbortTravelHandoff 解冻):两个组件摘掉之后,gate 重投过来的请求照常派发。
	tlsEcs.actorRegistry.remove<PlayerTravelHandoffComp>(player.entity);
	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player.entity);
	EXPECT_FALSE(IsHeldForOwnershipHandoff(player.entity, *equipItem));
}

TEST(PlayerHandoffGate, SceneBoundRequestDuringOwnershipHandoffIsStillDispatched)
{
	ScopedPlayer player;
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player.entity);
	tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player.entity);

	// 交接中再来一次换图:照旧派发,由 handler 的在途闸当场回 kEnterSceneChangingScene。
	const auto *enterScene = Find({SceneSceneClientPlayer::descriptor(), "EnterScene"});
	ASSERT_NE(enterScene, nullptr);
	EXPECT_FALSE(IsHeldForOwnershipHandoff(player.entity, *enterScene));

	// 退出优先:交接途中的 ExitGame 必须派发,由 PlayerLifecycleSystem 的退出分支收口。
	const auto *exitGame = Find({ScenePlayer::descriptor(), "ExitGame"});
	ASSERT_NE(exitGame, nullptr);
	EXPECT_FALSE(IsHeldForOwnershipHandoff(player.entity, *exitGame));
}

TEST(PlayerHandoffGate, FreezeWithoutHandoffIsNotHeld)
{
	const auto *equipItem = Find({SceneBagClientPlayer::descriptor(), "EquipItem"});
	ASSERT_NE(equipItem, nullptr);

	ScopedPlayer player;
	// 账号角色交易封存只挂 PlayerFrozenComp:那不是换节点,等不到「目标节点」,留给 gate 重投只会白等到超时。
	// 照旧派发,写请求由各业务系统的冻结闸当场拒绝。
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player.entity);
	EXPECT_FALSE(IsHeldForOwnershipHandoff(player.entity, *equipItem));
}

// 场景服务按方法列:只有取版本摘要与查场景信息换节点含义不变;换图、跨 zone 传送与各推送都不算。
TEST(PlayerHandoffGate, SceneServiceIsListedPerMethod)
{
	const google::protobuf::ServiceDescriptor *const sceneService = SceneSceneClientPlayer::descriptor();
	for (int i = 0; i < sceneService->method_count(); ++i)
	{
		const google::protobuf::MethodDescriptor *const method = sceneService->method(i);
		const bool expected = method->name() == "GetSyncDigest" || method->name() == "SceneInfoC2S";
		EXPECT_EQ(IsReplayableOnAnotherSceneNode(*method), expected) << method->full_name();
	}
}

// 设计 S13:gate 标了「换过场景路由才送到」的消息,含义绑在旧场景上的不在新场景执行;换节点含义不变的照常执行。
// 没标的消息一律不受这道闸影响。
TEST(PlayerHandoffGate, MessageReceivedUnderAPreviousRouteIsStaleOnlyIfSceneBound)
{
	const NamedMethod kSceneBound[] = {
		{SceneSceneClientPlayer::descriptor(), "EnterScene"},
		{SceneSceneClientPlayer::descriptor(), "TravelToZone"},
		{SceneMovementClientPlayer::descriptor(), "MoveStart"},
		{SceneMovementClientPlayer::descriptor(), "TeleportRequest"},
		{SceneSkillClientPlayer::descriptor(), "ReleaseSkill"},
		{SceneMysteryMerchantClientPlayer::descriptor(), "BuyMysteryMerchantGoods"},
	};
	for (const auto &named : kSceneBound)
	{
		const auto *method = Find(named);
		ASSERT_NE(method, nullptr) << named.method;
		EXPECT_TRUE(IsStaleForThisScene(*method, /*receivedOnPreviousRoute=*/true)) << method->full_name();
		EXPECT_FALSE(IsStaleForThisScene(*method, /*receivedOnPreviousRoute=*/false)) << method->full_name();
	}

	const NamedMethod kStillValid[] = {
		{SceneBagClientPlayer::descriptor(), "EquipItem"},
		{SceneSkillClientPlayer::descriptor(), "UpgradeSkill"},
		{SceneSceneClientPlayer::descriptor(), "GetSyncDigest"},
		{SceneSceneClientPlayer::descriptor(), "SceneInfoC2S"},
	};
	for (const auto &named : kStillValid)
	{
		const auto *method = Find(named);
		ASSERT_NE(method, nullptr) << named.method;
		EXPECT_FALSE(IsStaleForThisScene(*method, /*receivedOnPreviousRoute=*/true)) << method->full_name();
	}
}

// 入口三种状态的处置规则(ProcessClientPlayerMessage 的三个接线点调的就是这一个函数)。
TEST(PlayerHandoffGate, EntryAnswersRetryLaterByStateAndMethod)
{
	using scene_handoff_gate::EntryState;
	using scene_handoff_gate::ShouldAnswerRetryLater;
	const auto *equipItem = Find({SceneBagClientPlayer::descriptor(), "EquipItem"});
	const auto *enterScene = Find({SceneSceneClientPlayer::descriptor(), "EnterScene"});
	ASSERT_NE(equipItem, nullptr);
	ASSERT_NE(enterScene, nullptr);

	// 会话不在本节点 / 玩家实体还没载入:换节点执行含义不变的请求留给 gate,绑在场景上的请求照旧空应答。
	EXPECT_TRUE(ShouldAnswerRetryLater(EntryState::kSessionUnknown, entt::null, *equipItem));
	EXPECT_TRUE(ShouldAnswerRetryLater(EntryState::kPlayerNotLoaded, entt::null, *equipItem));
	EXPECT_FALSE(ShouldAnswerRetryLater(EntryState::kSessionUnknown, entt::null, *enterScene));
	EXPECT_FALSE(ShouldAnswerRetryLater(EntryState::kPlayerNotLoaded, entt::null, *enterScene));

	// 实体在本节点:平时照常派发,只有归属交接在途时才留给 gate;绑在场景上的请求任何时候都照常派发。
	ScopedPlayer player;
	EXPECT_FALSE(ShouldAnswerRetryLater(EntryState::kPlayerPresent, player.entity, *equipItem));
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player.entity);
	tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player.entity);
	EXPECT_TRUE(ShouldAnswerRetryLater(EntryState::kPlayerPresent, player.entity, *equipItem));
	EXPECT_FALSE(ShouldAnswerRetryLater(EntryState::kPlayerPresent, player.entity, *enterScene));
}
