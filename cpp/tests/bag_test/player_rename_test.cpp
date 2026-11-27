// 角色改名(docs/design/rename-and-content-safety.md §4)单测:纯规则 + PlayerRenameSystem 全链路。
// 不连内容安全/data_service/数据库,经 ContentGuardClient::SetTransportForTest 与 PlayerRenameSystem::SetTestHooks 注入。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/item_hold.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_item_hold_comp.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "proto/content_guard/content_guard.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "services/scene/player/comp/player_frozen_comp.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/content_guard_client.h"
#include "services/scene/player/system/player_rename.h"
#include "services/scene/player/system/player_rename_rules.h"
#include "table/code/item_table.h"
#include "table/code/rolenamerule_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
constexpr Guid kPlayerGuid = 960200001;
constexpr uint64_t kNowMs = 1'800'000'000'000ull;

struct FakeContentGuardState
{
	content_guard::CheckTextRequest lastRequest;
	ContentGuardClient::Done pendingDone;
	bool acceptSend{true};
	int calls{0};
};

FakeContentGuardState *gContentGuardState = nullptr;

bool FakeContentGuardSend(const content_guard::CheckTextRequest &req, ContentGuardClient::Done done)
{
	if (gContentGuardState == nullptr || !gContentGuardState->acceptSend)
	{
		return false;
	}
	++gContentGuardState->calls;
	gContentGuardState->lastRequest = req;
	gContentGuardState->pendingDone = std::move(done);
	return true;
}

TEST(PlayerRenameRulesTest, RuleBoundsAndBasicValidation)
{
	player_rename::Rule good{2, 12, 604800, 604800, 3401, 1};
	EXPECT_EQ(player_rename::RuleProblem(good), nullptr);

	auto bad = good;
	bad.renameItemId = 0;
	EXPECT_NE(player_rename::RuleProblem(bad), nullptr);
	bad = good;
	bad.renameItemCount = 0;
	EXPECT_NE(player_rename::RuleProblem(bad), nullptr);
	bad = good;
	bad.maxChars = 33;
	EXPECT_NE(player_rename::RuleProblem(bad), nullptr);

	std::string trimmed;
	EXPECT_TRUE(player_rename::TrimAndValidateBasic("  青云客  ", 2, 12, trimmed));
	EXPECT_EQ(trimmed, "青云客");
	EXPECT_FALSE(player_rename::TrimAndValidateBasic("  a  ", 2, 12, trimmed));
	EXPECT_FALSE(player_rename::TrimAndValidateBasic("bad\x01name", 2, 12, trimmed));
	EXPECT_FALSE(player_rename::TrimAndValidateBasic(std::string("\xFF\xFE"), 2, 12, trimmed));

	EXPECT_TRUE(player_rename::IsInCooldown(kNowMs - 1000, 60, kNowMs));
	EXPECT_FALSE(player_rename::IsInCooldown(kNowMs - 60'000, 60, kNowMs));
	EXPECT_EQ(player_rename::RetryDelayMs(1), 1000u);
	EXPECT_EQ(player_rename::RetryDelayMs(2), 2000u);
	EXPECT_EQ(player_rename::RetryDelayMs(10), 30'000u);
	EXPECT_EQ(player_rename::TipForRenameResult(data_service::RENAME_ROLE_TAKEN), static_cast<uint32_t>(kRoleNameTaken));
	EXPECT_EQ(player_rename::TipForRenameResult(data_service::RENAME_ROLE_COOLDOWN), static_cast<uint32_t>(kRoleRenameCooldown));
}

class PlayerRenameSystemTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		RoleNameRuleTableManager::Instance().Load();
		ASSERT_TRUE(PlayerRenameSystem::ValidateTables())
			<< "RoleNameRule / Item rename card must be exported via rename_content_safety_xlsx_patch.py + table exporter";
		const auto &row = *RoleNameRuleTableManager::Instance().FindAll().data().begin();
		cardItemId = row.rename_item_id();
		cardCount = row.rename_item_count();
		cooldownSeconds = row.rename_cooldown_seconds();

		cgState = FakeContentGuardState{};
		gContentGuardState = &cgState;
		ContentGuardClient::SetTransportForTest(&FakeContentGuardSend);

		PlayerRenameSystem::ResetTestState();
		PlayerRenameSystem::TestHooks hooks;
		hooks.transport = [this](const data_service::RenameRoleRequest &req, PlayerRenameSystem::RenameRoleDone done) {
			if (!acceptDataServiceSend)
			{
				return false;
			}
			sentRequests.push_back(req);
			pendingRpcDone = std::move(done);
			return true;
		};
		hooks.notify = [this](entt::entity /*p*/, const PlayerRenamedNotify &n) {
			notifies.push_back(n);
		};
		hooks.save = [this](entt::entity /*p*/) {
			++saveRequests;
			return true;
		};
		hooks.nextHoldId = [this](Guid &outId) {
			outId = nextHold++;
			return true;
		};
		PlayerRenameSystem::SetTestHooks(std::move(hooks));

		player = CreatePlayer(kPlayerGuid, "旧名道友", cardCount * 2);
	}

	void TearDown() override
	{
		PlayerRenameSystem::ResetTestState();
		ContentGuardClient::SetTransportForTest(nullptr);
		gContentGuardState = nullptr;
		tlsEcs.playerList.erase(kPlayerGuid);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	entt::entity CreatePlayer(Guid guid, const std::string &name, uint32_t cards)
	{
		const auto entity = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, guid);
		auto &epoch = tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(entity);
		epoch.epoch = 1;
		auto &profile = tlsEcs.actorRegistry.emplace<PlayerProfileComp>(entity);
		profile.set_name(name);
		auto &bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		bags.bags[kInventory].SetCapacityForRestore(40);
		if (cards > 0)
		{
			bags.bags[kInventory].InsertItemForRestore((uint64_t{1} << 60) + 7701, cardItemId, cards, 0);
		}
		tlsEcs.actorRegistry.emplace<PlayerItemHoldComp>(entity);
		tlsEcs.playerList.emplace(guid, entity);
		return entity;
	}

	PlayerAllData SnapshotCurrent() const
	{
		PlayerAllData all;
		*all.mutable_player_database_data()->mutable_profile_component() = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
		*all.mutable_player_database_data()->mutable_item_holds() = tlsEcs.actorRegistry.get<PlayerItemHoldComp>(player);
		return all;
	}

	uint64_t CardCountInBag() const
	{
		return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory].GetTotalItemCount(cardItemId);
	}

	void PassContentGuard()
	{
		ASSERT_TRUE(static_cast<bool>(cgState.pendingDone));
		auto done = std::move(cgState.pendingDone);
		ContentGuardClient::Result res;
		res.verdict = ContentGuardClient::Verdict::kPass;
		done(res);
	}

	FakeContentGuardState cgState;
	uint32_t cardItemId{0};
	uint32_t cardCount{0};
	uint32_t cooldownSeconds{0};
	entt::entity player{entt::null};
	Guid nextHold{550001};
	int saveRequests{0};
	bool acceptDataServiceSend{true};
	std::vector<data_service::RenameRoleRequest> sentRequests;
	PlayerRenameSystem::RenameRoleDone pendingRpcDone;
	std::vector<PlayerRenamedNotify> notifies;
};

TEST_F(PlayerRenameSystemTest, SyncRejectsMissingCardSameNameCooldownAndFrozen)
{
	EXPECT_EQ(PlayerRenameSystem::RequestRename(player, "旧名道友", kNowMs), static_cast<uint32_t>(kRoleRenameSameName));

	auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	profile.set_renamed_ms(kNowMs - 1000);
	EXPECT_EQ(PlayerRenameSystem::RequestRename(player, "新名道友", kNowMs), static_cast<uint32_t>(kRoleRenameCooldown));
	profile.set_renamed_ms(0);

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	EXPECT_EQ(PlayerRenameSystem::RequestRename(player, "新名道友", kNowMs), static_cast<uint32_t>(kRoleRenameFrozen));
	tlsEcs.actorRegistry.erase<PlayerFrozenComp>(player);

	EXPECT_EQ(cgState.calls, 0);
}

TEST_F(PlayerRenameSystemTest, FullCommitFlowPersistsHoldBeforeRpcAndConsumesCardOnOk)
{
	const uint64_t cardsBefore = CardCountInBag();
	ASSERT_EQ(PlayerRenameSystem::RequestRename(player, "青冥剑仙", kNowMs), kSuccess);
	ASSERT_EQ(cgState.calls, 1);
	EXPECT_EQ(cgState.lastRequest.scene(), content_guard::TEXT_SCENE_ROLE_NAME);

	// 送检通过:挂预留 + 记 rename_op + 请求存盘,但落盘前绝不发 RenameRole。
	PassContentGuard();
	EXPECT_EQ(saveRequests, 1);
	EXPECT_TRUE(sentRequests.empty());
	const auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	ASSERT_TRUE(profile.has_rename_op());
	EXPECT_EQ(profile.rename_op().seq(), 1u);
	EXPECT_EQ(BagService::HeldCount(player, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory], cardItemId),
			  cardCount);

	// 存盘提交后发出 DataService.RenameRole。
	PlayerRenameSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 50);
	ASSERT_EQ(sentRequests.size(), 1u);
	EXPECT_EQ(sentRequests[0].player_id(), kPlayerGuid);
	EXPECT_EQ(sentRequests[0].seq(), 1u);
	EXPECT_EQ(sentRequests[0].name(), "青冥剑仙");

	// data_service 确认提交:扣卡、释放预留、更新档案名字、推送成功。
	data_service::RenameRoleResponse resp;
	resp.set_result(data_service::RENAME_ROLE_OK);
	resp.set_current_seq(1);
	resp.set_name("青冥剑仙");
	resp.set_renamed_ms(kNowMs + 100);
	ASSERT_TRUE(static_cast<bool>(pendingRpcDone));
	pendingRpcDone(true, &resp);

	EXPECT_EQ(CardCountInBag(), cardsBefore - cardCount);
	EXPECT_EQ(BagService::HeldCount(player, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory], cardItemId), 0u);
	EXPECT_FALSE(profile.has_rename_op());
	EXPECT_EQ(profile.name(), "青冥剑仙");
	EXPECT_EQ(profile.renamed_ms(), kNowMs + 100);
	ASSERT_EQ(notifies.size(), 1u);
	EXPECT_EQ(notifies[0].error_message().id(), 0u);
	EXPECT_EQ(notifies[0].name(), "青冥剑仙");
	EXPECT_EQ(notifies[0].next_rename_time(), (kNowMs + 100 + static_cast<uint64_t>(cooldownSeconds) * 1000ull) / 1000ull);
}

TEST_F(PlayerRenameSystemTest, StaleSeqAdvancesSequenceAndReleasesHoldWithoutConsumingCard)
{
	const uint64_t cardsBefore = CardCountInBag();
	ASSERT_EQ(PlayerRenameSystem::RequestRename(player, "苍穹行者", kNowMs), kSuccess);
	PassContentGuard();
	PlayerRenameSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 20);
	ASSERT_EQ(sentRequests.size(), 1u);

	data_service::RenameRoleResponse resp;
	resp.set_result(data_service::RENAME_ROLE_STALE_SEQ);
	resp.set_current_seq(9);
	pendingRpcDone(true, &resp);

	const auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	EXPECT_EQ(profile.rename_seq(), 9u);
	EXPECT_FALSE(profile.has_rename_op());
	EXPECT_EQ(CardCountInBag(), cardsBefore);
	EXPECT_EQ(BagService::HeldCount(player, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory], cardItemId), 0u);
	ASSERT_EQ(notifies.size(), 1u);
	EXPECT_EQ(notifies[0].error_message().id(), static_cast<uint32_t>(kServiceUnavailable));
}

// 结局回来时玩家正在交接(存档不可改):一个字都不动,记录和占用都留着;解冻后用同一个 seq 重发,拿到结局再扣卡、改名。
TEST_F(PlayerRenameSystemTest, OutcomeArrivingWhileFrozenIsKeptAndAppliedAfterRetry)
{
	const uint64_t cardsBefore = CardCountInBag();
	ASSERT_EQ(PlayerRenameSystem::RequestRename(player, "云深不知", kNowMs), kSuccess);
	PassContentGuard();
	PlayerRenameSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 10);
	ASSERT_EQ(sentRequests.size(), 1u);
	const int savesBefore = saveRequests;

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player);
	data_service::RenameRoleResponse ok;
	ok.set_result(data_service::RENAME_ROLE_OK);
	ok.set_name("云深不知");
	ok.set_renamed_ms(kNowMs + 30);
	pendingRpcDone(true, &ok);

	const auto &profile = tlsEcs.actorRegistry.get<PlayerProfileComp>(player);
	EXPECT_EQ(profile.name(), "旧名道友");
	EXPECT_TRUE(profile.has_rename_op());
	EXPECT_EQ(CardCountInBag(), cardsBefore);
	EXPECT_EQ(BagService::HeldCount(player, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory], cardItemId), cardCount);
	EXPECT_EQ(saveRequests, savesBefore);
	EXPECT_TRUE(notifies.empty());

	// 还冻着:到点也不发。
	PlayerRenameSystem::Tick(kNowMs + 5'000);
	EXPECT_EQ(sentRequests.size(), 1u);

	tlsEcs.actorRegistry.erase<PlayerFrozenComp>(player);
	PlayerRenameSystem::Tick(kNowMs + 60'000);
	ASSERT_EQ(sentRequests.size(), 2u);
	EXPECT_EQ(sentRequests[1].seq(), sentRequests[0].seq());
	pendingRpcDone(true, &ok);
	EXPECT_EQ(profile.name(), "云深不知");
	EXPECT_FALSE(profile.has_rename_op());
	EXPECT_EQ(CardCountInBag(), cardsBefore - cardCount);
	ASSERT_EQ(notifies.size(), 1u);
	EXPECT_EQ(notifies[0].error_message().id(), 0u);
}

// 本版本不认识的结局不能当成失败:占用和记录留着重发,不把卡还给玩家。
TEST_F(PlayerRenameSystemTest, UnknownResultIsRetriedNotTreatedAsRejection)
{
	EXPECT_TRUE(player_rename::IsTransientResult(static_cast<data_service::RenameRoleResult>(99)));
	EXPECT_TRUE(player_rename::IsTransientResult(data_service::RENAME_ROLE_BUSY));
	EXPECT_FALSE(player_rename::IsTransientResult(data_service::RENAME_ROLE_OK));
	EXPECT_FALSE(player_rename::IsTransientResult(data_service::RENAME_ROLE_EXPIRED));

	ASSERT_EQ(PlayerRenameSystem::RequestRename(player, "一剑霜寒", kNowMs), kSuccess);
	PassContentGuard();
	PlayerRenameSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 10);
	ASSERT_EQ(sentRequests.size(), 1u);
	data_service::RenameRoleResponse unknown;
	unknown.set_result(static_cast<data_service::RenameRoleResult>(99));
	pendingRpcDone(true, &unknown);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<PlayerProfileComp>(player).has_rename_op());
	EXPECT_EQ(BagService::HeldCount(player, tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory], cardItemId), cardCount);
	EXPECT_TRUE(notifies.empty());
}

// 加载时没有进行中的改名、却留着改名的占用:放掉,改名卡重新可用。
TEST_F(PlayerRenameSystemTest, OrphanRenameHoldIsReleasedOnLoad)
{
	const item_hold::HoldLines lines{{cardItemId, cardCount}};
	ASSERT_EQ(BagService::PlaceHold(player, 770001, item_hold::ItemHoldKind::kRename, lines, kNowMs, kNowMs + 1), kSuccess);
	const auto &bag = tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory];
	ASSERT_EQ(BagService::HeldCount(player, bag, cardItemId), cardCount);

	PlayerRenameSystem::OnPlayerLoaded(player, kNowMs);
	EXPECT_EQ(BagService::HeldCount(player, bag, cardItemId), 0u);
	EXPECT_TRUE(sentRequests.empty());
}

TEST_F(PlayerRenameSystemTest, TransientBusyRetriesOnTickAndLoginResumesPersistedOp)
{
	ASSERT_EQ(PlayerRenameSystem::RequestRename(player, "紫电青霜", kNowMs), kSuccess);
	PassContentGuard();
	PlayerRenameSystem::OnSaveCommitted(kPlayerGuid, SnapshotCurrent(), kNowMs + 10);
	ASSERT_EQ(sentRequests.size(), 1u);

	data_service::RenameRoleResponse busy;
	busy.set_result(data_service::RENAME_ROLE_BUSY);
	pendingRpcDone(true, &busy);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<PlayerProfileComp>(player).has_rename_op());
	EXPECT_TRUE(notifies.empty());

	// 退避到期后 Tick 用同一个 seq 重发。
	PlayerRenameSystem::Tick(kNowMs + 5'000);
	ASSERT_EQ(sentRequests.size(), 2u);
	EXPECT_EQ(sentRequests[1].seq(), sentRequests[0].seq());

	// 模拟崩溃重登:销毁内存态后,OnPlayerLoaded 直接续发同一个 seq。
	PlayerRenameSystem::OnPlayerDestroyed(kPlayerGuid);
	PlayerRenameSystem::OnPlayerLoaded(player, kNowMs + 6'000);
	ASSERT_EQ(sentRequests.size(), 3u);
	EXPECT_EQ(sentRequests[2].seq(), sentRequests[0].seq());
}

} // namespace
