// 待寄邮件(docs/design/battle-overflow-mail.md):纯规则(mail_outbox_rules.h)、BagService 的两包都放不下的去处
// (GrantOverflowRoute),以及 PlayerMailOutboxSystem 的备条目与寄送。
//
// 盯住的契约:
//   * 一批物品按邮件附件上限(16 种、4096 件)分封,同一种装不下就拆到下一封;
//   * 两包都放不下时先问去处能不能接住整批:接住了照常扣消耗、发布,获得一件不进包,commit 恰好一次;接不住就整笔照旧拒绝;
//   * 待寄队列满、请求号取不到、组件缺失时整批不收,存档一字不改;
//   * 只寄「已提交的存档快照里已有」的条目,每名玩家至多一封在途;回了 mail_id 才删条目,其余按退避重试,没有放弃;
//   * 发给邮件的请求由条目确定性重建:battle 身份、收件人是本人、不带过期时刻,同一条目每次逐字节相同。
//
// 铺底走 Bag::InsertItemForRestore(预设 guid,不铸号)。寄送替身只收下回调,由用例决定什么时候、以什么结果回;
// 「是否已落盘」用替身直接回答,落盘判定本身(RoleAdmissionSystem::IsSnapshotCommitted)由收件箱的用例盯。

#include <gtest/gtest.h>

#include <cstdint>
#include <functional>
#include <set>
#include <string>
#include <vector>

#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "proto/common/component/player_mail_outbox_comp.pb.h"
#include "proto/mail/mail_admin.pb.h"
#include "services/scene/player/comp/player_ownership_comp.h"
#include "services/scene/player/system/mail_outbox_rules.h"
#include "services/scene/player/system/player_mail_outbox.h"
#include "table/code/item_table.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mail_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{
using mail_outbox_rules::MailItems;
using mail_outbox_rules::MailOutboxSource;
using mail_outbox_rules::SendVerdict;
using SendStart = PlayerMailOutboxSystem::SendStart;
using SendOutcome = PlayerMailOutboxSystem::SendOutcome;

// generated/tables/item.json 的既有行:10、11 可叠加,3 不可叠加。
constexpr uint32_t kPotion = 10;
constexpr uint32_t kElixir = 11;
constexpr uint32_t kTalisman = 3;

constexpr Guid kPotionStack = (uint64_t{1} << 60) + 9601;
constexpr uint64_t kBattle = 880201;

// 无 I/O 的假传输 + 直接投一段号(与 bag_remove_exact_test.cpp 的 ArmTxLogSegment 同一个做法)。
bool ArmSegment(GuidKind kind, const std::string& name, uint64_t low, uint64_t high)
{
	auto& client = tlsGuidSegmentRegistry.Get(kind);
	client.Reset();
	GuidSegmentClient::Options options;
	options.mixIds = false;
	options.kindName = name;
	options.initialStep = 100000;
	if (!client.Enable(options, [](const std::string&, uint32_t) { return true; },
					   [](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; }))
	{
		return false;
	}
	client.Warm();
	client.OnResponse(0, low, high);
	return client.IsReady();
}

MailOutboxEntry MakeEntry(uint64_t requestId, MailItems items)
{
	PlayerMailOutboxComp holder;
	mail_outbox_rules::Append(holder, requestId, MailOutboxSource::kBattleOverflow, kBattle, "battle reward", "bags were full",
							  items, 1);
	return holder.entries(0);
}

// ── 纯规则 ───────────────────────────────────────────────────────────────────────────

TEST(MailOutboxRules, SplitIntoMailsRespectsTheAttachmentLimits)
{
	MailItems twenty;
	for (uint32_t config = 1; config <= 20; ++config)
	{
		twenty[config] = 1;
	}
	const auto byLines = mail_outbox_rules::SplitIntoMails(twenty);
	ASSERT_EQ(2u, byLines.size());
	EXPECT_EQ(16u, byLines[0].size()) << "一封至多 16 种";
	EXPECT_EQ(4u, byLines[1].size());
	EXPECT_EQ(1u, byLines[0].begin()->first) << "按 config_id 升序装填";

	const auto byCount = mail_outbox_rules::SplitIntoMails({{kPotion, 5000}});
	ASSERT_EQ(2u, byCount.size());
	EXPECT_EQ(4096u, byCount[0].at(kPotion)) << "一封至多 4096 件";
	EXPECT_EQ(904u, byCount[1].at(kPotion)) << "装不下的余量拆到下一封";

	const auto mixed = mail_outbox_rules::SplitIntoMails({{kPotion, 4000}, {kElixir, 200}});
	ASSERT_EQ(2u, mixed.size());
	EXPECT_EQ((MailItems{{kPotion, 4000}, {kElixir, 96}}), mixed[0]);
	EXPECT_EQ((MailItems{{kElixir, 104}}), mixed[1]);

	EXPECT_TRUE(mail_outbox_rules::SplitIntoMails({}).empty());
	EXPECT_TRUE(mail_outbox_rules::SplitIntoMails({{0, 3}, {kPotion, 0}}).empty()) << "配置号或数量为 0 的项跳过";
}

TEST(MailOutboxRules, ValidateNamesTheFirstBadShape)
{
	PlayerMailOutboxComp good;
	mail_outbox_rules::Append(good, 7, MailOutboxSource::kBattleOverflow, kBattle, "t", "b", {{kPotion, 4096}}, 1);
	EXPECT_TRUE(mail_outbox_rules::Validate(good).empty()) << mail_outbox_rules::Validate(good);

	auto expectProblem = [](const PlayerMailOutboxComp& outbox, const std::string& expected) {
		EXPECT_EQ(expected, mail_outbox_rules::Validate(outbox));
	};
	PlayerMailOutboxComp zeroId = good;
	zeroId.mutable_entries(0)->set_request_id(0);
	expectProblem(zeroId, "zero_request_id");
	PlayerMailOutboxComp noSource = good;
	noSource.mutable_entries(0)->set_source(0);
	expectProblem(noSource, "unspecified_source");
	PlayerMailOutboxComp noTitle = good;
	noTitle.mutable_entries(0)->clear_title();
	expectProblem(noTitle, "empty_title");
	PlayerMailOutboxComp noItems = good;
	noItems.mutable_entries(0)->clear_items();
	expectProblem(noItems, "bad_item_line_count");
	PlayerMailOutboxComp tooMany = good;
	tooMany.mutable_entries(0)->mutable_items(0)->set_count(4097);
	expectProblem(tooMany, "too_many_items");
	PlayerMailOutboxComp unsorted;
	mail_outbox_rules::Append(unsorted, 7, MailOutboxSource::kBattleOverflow, kBattle, "t", "b", {{kPotion, 1}, {kElixir, 1}}, 1);
	unsorted.mutable_entries(0)->mutable_items()->SwapElements(0, 1);
	expectProblem(unsorted, "items_not_ascending");
	PlayerMailOutboxComp duplicate = good;
	*duplicate.add_entries() = good.entries(0);
	expectProblem(duplicate, "duplicate_request_id");
}

TEST(MailOutboxRules, AppendFindAndRemove)
{
	PlayerMailOutboxComp outbox;
	mail_outbox_rules::Append(outbox, 7, MailOutboxSource::kBattleOverflow, kBattle, "t", "b", {{kPotion, 2}}, 100);
	mail_outbox_rules::Append(outbox, 8, MailOutboxSource::kBattleOverflow, kBattle, "t", "b", {{kElixir, 1}}, 100);
	const auto* found = mail_outbox_rules::Find(outbox, 8);
	ASSERT_NE(nullptr, found);
	EXPECT_EQ(kBattle, found->source_id());
	EXPECT_EQ(kElixir, found->items(0).config_id());
	EXPECT_EQ(nullptr, mail_outbox_rules::Find(outbox, 9));
	*outbox.add_entries() = outbox.entries(0); // 坏数据里的重复号
	EXPECT_EQ(2u, mail_outbox_rules::Remove(outbox, 7)) << "这个号的全部条目一起摘";
	ASSERT_EQ(1, outbox.entries_size());
	EXPECT_EQ(8u, outbox.entries(0).request_id());
}

TEST(MailOutboxRules, ClassifySendBackoffAndLogCadence)
{
	EXPECT_EQ(SendVerdict::kUnknown, mail_outbox_rules::ClassifySend(false, 0, 0)) << "没拿到回包";
	EXPECT_EQ(SendVerdict::kDelivered, mail_outbox_rules::ClassifySend(true, 0, 55));
	EXPECT_EQ(SendVerdict::kDelivered, mail_outbox_rules::ClassifySend(true, kSuccess, 55)) << "kSuccess 与 0 同为成功";
	EXPECT_EQ(SendVerdict::kUnknown, mail_outbox_rules::ClassifySend(true, 0, 0)) << "成功却没有 mail_id";
	EXPECT_EQ(SendVerdict::kRejected, mail_outbox_rules::ClassifySend(true, kMailRecipientBoxFull, 0));
	EXPECT_EQ(SendVerdict::kRejected, mail_outbox_rules::ClassifySend(true, kMailRequestConflict, 0));
	EXPECT_EQ(SendVerdict::kUnknown, mail_outbox_rules::ClassifySend(true, kServiceUnavailable, 0)) << "故障码:邮件可能已写";
	EXPECT_EQ(SendVerdict::kUnknown, mail_outbox_rules::ClassifySend(true, 999999, 0)) << "码表外的码按结果未知";
	EXPECT_FALSE(mail_outbox_rules::NeedsManualFollowUp(kMailRecipientBoxFull)) << "邮箱满玩家删信就好";
	EXPECT_TRUE(mail_outbox_rules::NeedsManualFollowUp(kMailAttachmentInvalid));

	EXPECT_EQ(1000u, mail_outbox_rules::RetryDelayMs(false, 1));
	EXPECT_EQ(2000u, mail_outbox_rules::RetryDelayMs(false, 2));
	EXPECT_EQ(60000u, mail_outbox_rules::RetryDelayMs(false, 7)) << "结果未知封顶 60 秒";
	EXPECT_EQ(30000u, mail_outbox_rules::RetryDelayMs(true, 1));
	EXPECT_EQ(300000u, mail_outbox_rules::RetryDelayMs(true, 5)) << "拒收封顶 5 分钟";
	EXPECT_EQ(300000u, mail_outbox_rules::RetryDelayMs(true, 4000000000u)) << "次数再大也不溢出";

	EXPECT_TRUE(mail_outbox_rules::IsFailureLogDue(1));
	EXPECT_FALSE(mail_outbox_rules::IsFailureLogDue(2));
	EXPECT_TRUE(mail_outbox_rules::IsFailureLogDue(8));
	EXPECT_TRUE(mail_outbox_rules::IsFailureLogDue(16));
	EXPECT_FALSE(mail_outbox_rules::IsFailureLogDue(0));
}

TEST(MailOutboxRules, SignatureRejectionIsItsOwnCase)
{
	EXPECT_TRUE(mail_outbox_rules::IsSignatureRejected(16u)) << "16 = UNAUTHENTICATED,与 grpc::StatusCode 同值";
	EXPECT_TRUE(mail_outbox_rules::IsSignatureRejected(mail_outbox_rules::kGrpcStatusUnauthenticated));
	EXPECT_FALSE(mail_outbox_rules::IsSignatureRejected(0u)) << "OK";
	EXPECT_FALSE(mail_outbox_rules::IsSignatureRejected(14u)) << "UNAVAILABLE:传输故障,结果未知";
	EXPECT_FALSE(mail_outbox_rules::IsSignatureRejected(4u)) << "DEADLINE_EXCEEDED:结果未知";
}

TEST(MailOutboxRules, SendRequestIsRebuiltDeterministicallyFromTheEntry)
{
	const auto entry = MakeEntry(4242, {{kPotion, 3}, {kElixir, 1}});
	mailpb::SendMailRequest first;
	mailpb::SendMailRequest second;
	ASSERT_TRUE(PlayerMailOutboxSystem::BuildSendRequest(77, entry, first));
	ASSERT_TRUE(PlayerMailOutboxSystem::BuildSendRequest(77, entry, second));
	EXPECT_EQ(first.SerializeAsString(), second.SerializeAsString()) << "同一条目每次逐字节相同,邮件据此判重";
	EXPECT_EQ(5u, first.sender_kind()) << "battle,与 go/mail SenderKind(\"battle\") 同值";
	EXPECT_EQ(4242u, first.request_id());
	EXPECT_EQ(77u, first.recipient_player_id());
	EXPECT_EQ(0u, first.expire_ms()) << "不带过期时刻:邮件按首次落库那一刻算有效期";
	EXPECT_EQ(0u, first.guild_id());
	EXPECT_EQ("battle reward", first.title());
	ASSERT_EQ(2, first.attachments().items_size());
	EXPECT_EQ(kPotion, first.attachments().items(0).config_id());
	EXPECT_EQ(3u, first.attachments().items(0).count());
	EXPECT_EQ(kElixir, first.attachments().items(1).config_id());
	EXPECT_FALSE(first.has_auth()) << "签名在发送时加";

	mailpb::SendMailRequest untouched;
	auto broken = entry;
	broken.mutable_items(0)->set_count(0);
	EXPECT_FALSE(PlayerMailOutboxSystem::BuildSendRequest(77, broken, untouched)) << "形状不合法不建";
	EXPECT_FALSE(PlayerMailOutboxSystem::BuildSendRequest(0, entry, untouched)) << "没有收件人不建";
	EXPECT_EQ(0u, untouched.request_id());
}

// ── 带玩家实体的夹具 ────────────────────────────────────────────────────────────────────

class MailOutboxPlayerTest : public testing::Test
{
protected:
	void SetUp() override
	{
		ItemTableManager::Instance().Load();
		for (const auto configId : {kPotion, kElixir, kTalisman})
		{
			ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(configId).first) << "fixture Item row " << configId;
		}
		static uint64_t nextPlayer = 9879800;
		playerId = ++nextPlayer;
		player = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, Guid{playerId});
		// ConsumeInventoryAndGrant 与寄送都要可改的实体(归属围栏)与按 player_id 现取实体。
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player).epoch = 1;
		tlsEcs.playerList.erase(playerId);
		tlsEcs.playerList.emplace(playerId, player);
		auto& bags = tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player);
		for (auto& bag : bags.bags)
		{
			bag.SetPlayerGuid(Guid{playerId});
		}
		tlsEcs.actorRegistry.emplace<PlayerMailOutboxComp>(player);
		ASSERT_TRUE(ArmSegment(GuidKind::kTxLog, "txlog", 7700000, 7800000));

		// 换掉全部对外依赖:不读进程环境、不发请求、不碰存盘路径。
		PlayerMailOutboxSystem::Dependencies dependencies;
		dependencies.senderConfigured = [this] { return senderConfigured; };
		dependencies.send = [this](uint64_t to, const MailOutboxEntry& entry, PlayerMailOutboxSystem::SendDone done) {
			sends.push_back({to, entry.request_id(), std::move(done)});
			return nextStart;
		};
		dependencies.isDurable = [this](entt::entity, uint64_t requestId) { return durable.contains(requestId); };
		dependencies.requestSave = [this](entt::entity, const char* reason) { saveRequests.emplace_back(reason); };
		// 寄送回调与 Tick 从这里取「现在」:用例用 Tick(at) 推时钟,回调按同一个时刻记退避。
		dependencies.nowMs = [this] { return now; };
		PlayerMailOutboxSystem::SetDependenciesForTest(std::move(dependencies));
	}

	void TearDown() override
	{
		PlayerMailOutboxSystem::SetDependenciesForTest({});
		// 进程级状态:txlog 号段回到 bag_test.cpp 的 main() 留下的「未启用」。
		tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
		tlsEcs.playerList.erase(playerId);
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	Bag& Inventory() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kInventory]; }
	Bag& Temporary() { return tlsEcs.actorRegistry.get<PlayerBagsComp>(player).bags[kTemporary]; }
	PlayerMailOutboxComp& Outbox() { return tlsEcs.actorRegistry.get<PlayerMailOutboxComp>(player); }

	// 两包都满:人物背包 1 格放着一堆药,临时格 0 格。一件不可叠加的护符哪个包都放不下。
	void FillBothBags()
	{
		Inventory().SetCapacityForRestore(1);
		Inventory().InsertItemForRestore(kPotionStack, kPotion, /*stackSize=*/4, /*pos=*/0);
		Temporary().SetCapacityForRestore(0);
	}

	// 已写进存档的一条(直接写组件,不经 Prepare)。
	uint64_t QueueEntry(MailItems items)
	{
		const uint64_t requestId = nextRequestId++;
		mail_outbox_rules::Append(Outbox(), requestId, MailOutboxSource::kBattleOverflow, kBattle, "t", "b", items, 1);
		return requestId;
	}

	// 时钟走到 at 并跑一拍;之后的寄送回答按这一刻记退避。
	void Tick(uint64_t at)
	{
		now = at;
		PlayerMailOutboxSystem::TickAt(at);
	}

	struct Sent
	{
		uint64_t playerId;
		uint64_t requestId;
		PlayerMailOutboxSystem::SendDone done;
	};

	uint64_t playerId{};
	entt::entity player{entt::null};
	std::vector<Sent> sends;
	SendStart nextStart{SendStart::kSent};
	bool senderConfigured{true};
	std::set<uint64_t> durable;
	std::vector<std::string> saveRequests; // 每次请求存盘的原因
	uint64_t nextRequestId{5100};
	uint64_t now{0};
};

// 恢复其它背包用例使用的无 I/O item 号段基线(与 bag_test.cpp 的 main() 同一段范围):用例中途 ASSERT 退出也照样还原。
struct RestoreItemSegmentBaseline
{
	~RestoreItemSegmentBaseline() { EXPECT_TRUE(ArmSegment(GuidKind::kItem, "item", 1, uint64_t{1} << 54)); }
};

// ── BagService:两包都放不下时的去处 ──────────────────────────────────────────────────────

TEST_F(MailOutboxPlayerTest, OverflowRouteTakesTheWholeBatchAndConsumptionStillApplies)
{
	FillBothBags();
	std::vector<ItemCountMap> accepted, committed;
	const BagService::GrantOverflowRoute route{
		[&](const ItemCountMap& items) { accepted.push_back(items); return static_cast<uint32_t>(kSuccess); },
		[&](const ItemCountMap& items) { committed.push_back(items); }};
	int credits = 0;
	EXPECT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {{kPotion, 1}}, {{kTalisman, 1}}, kBattle, "{}",
															 [&] { ++credits; return static_cast<uint32_t>(kSuccess); },
															 /*consumeFromHoldId=*/0, &route));
	ASSERT_EQ(1u, accepted.size());
	ASSERT_EQ(1u, committed.size()) << "commit 恰好一次";
	EXPECT_EQ((ItemCountMap{{kTalisman, 1}}), committed[0]);
	EXPECT_EQ(1, credits) << "beforeCommit 照常跑";
	EXPECT_EQ(3u, Inventory().GetTotalItemCount(kPotion)) << "消耗照常扣";
	EXPECT_EQ(0u, Inventory().GetTotalItemCount(kTalisman)) << "改投的那一批不进包";
	EXPECT_EQ(0u, Temporary().OccupiedGridCount());
	EXPECT_EQ(0u, BagService::ConsecutiveGrantOverflowRejections(player)) << "接住了不算一次放不下";
}

TEST_F(MailOutboxPlayerTest, RouteRefusalRejectsTheWholeGrantWithoutSideEffects)
{
	FillBothBags();
	int commits = 0;
	const BagService::GrantOverflowRoute route{
		[](const ItemCountMap&) { return static_cast<uint32_t>(kServiceUnavailable); },
		[&](const ItemCountMap&) { ++commits; }};
	int credits = 0;
	EXPECT_EQ(kServiceUnavailable,
			  BagService::ConsumeInventoryAndGrant(player, {{kPotion, 1}}, {{kTalisman, 1}}, kBattle, "{}",
												   [&] { ++credits; return static_cast<uint32_t>(kSuccess); }, 0, &route))
		<< "回去处给的码";
	EXPECT_EQ(0, commits);
	EXPECT_EQ(0, credits) << "beforeCommit 没跑";
	EXPECT_EQ(4u, Inventory().GetTotalItemCount(kPotion)) << "消耗没动";
	EXPECT_EQ(1u, BagService::ConsecutiveGrantOverflowRejections(player)) << "接不住仍记一次两包都放不下(收件箱据此分类延后原因)";
}

TEST_F(MailOutboxPlayerTest, RouteIsNotAskedWhenTheBatchFitsAndCommitIsSkippedOnLaterFailure)
{
	Inventory().SetCapacityForRestore(5);
	Inventory().InsertItemForRestore(kPotionStack, kPotion, 4, 0);
	RestoreItemSegmentBaseline restoreItemSegment;
	ASSERT_TRUE(ArmSegment(GuidKind::kItem, "item", 7900000, 8000000));
	int asks = 0;
	int commits = 0;
	const BagService::GrantOverflowRoute route{
		[&](const ItemCountMap&) { ++asks; return static_cast<uint32_t>(kSuccess); },
		[&](const ItemCountMap&) { ++commits; }};
	EXPECT_EQ(kSuccess, BagService::ConsumeInventoryAndGrant(player, {}, {{kTalisman, 1}}, kBattle, "{}", {}, 0, &route));
	EXPECT_EQ(0, asks) << "放得下就不问去处";
	EXPECT_EQ(1u, Inventory().GetTotalItemCount(kTalisman));

	// 接住之后 beforeCommit 失败:整笔作废,commit 不调。
	Inventory().SetCapacityForRestore(Inventory().OccupiedGridCount());
	Temporary().SetCapacityForRestore(0);
	EXPECT_EQ(kInvalidParameter, BagService::ConsumeInventoryAndGrant(player, {{kPotion, 1}}, {{kTalisman, 1}}, kBattle, "{}",
																	  [] { return static_cast<uint32_t>(kInvalidParameter); }, 0, &route));
	EXPECT_EQ(1, asks);
	EXPECT_EQ(0, commits);
	EXPECT_EQ(4u, Inventory().GetTotalItemCount(kPotion));
}

// ── 备条目 ─────────────────────────────────────────────────────────────────────────────

TEST_F(MailOutboxPlayerTest, PrepareSplitsTakesRequestIdsAndCommitWritesOneEntryPerMail)
{
	PlayerMailOutboxSystem::PreparedMails prepared;
	ASSERT_EQ(kSuccess, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 5000}, {kElixir, 3}}, prepared));
	ASSERT_EQ(2u, prepared.size());
	EXPECT_EQ(0, Outbox().entries_size()) << "只取号,不写存档";
	EXPECT_NE(prepared[0].requestId, prepared[1].requestId);
	EXPECT_NE(0u, prepared[0].requestId);

	PlayerMailOutboxSystem::CommitPrepared(player, kBattle, prepared, 1234);
	ASSERT_EQ(2, Outbox().entries_size());
	const auto& first = Outbox().entries(0);
	EXPECT_EQ(prepared[0].requestId, first.request_id());
	EXPECT_EQ(static_cast<uint32_t>(MailOutboxSource::kBattleOverflow), first.source());
	EXPECT_EQ(kBattle, first.source_id());
	EXPECT_EQ(PlayerMailOutboxSystem::kBattleOverflowTitle, first.title());
	EXPECT_EQ(PlayerMailOutboxSystem::kBattleOverflowBody, first.body());
	EXPECT_EQ(1234u, first.created_at_ms());
	ASSERT_EQ(1, first.items_size());
	EXPECT_EQ(4096u, first.items(0).count());
	const auto& second = Outbox().entries(1);
	ASSERT_EQ(2, second.items_size());
	EXPECT_EQ(904u, second.items(0).count());
	EXPECT_EQ(kElixir, second.items(1).config_id());
	EXPECT_TRUE(mail_outbox_rules::Validate(Outbox()).empty()) << mail_outbox_rules::Validate(Outbox());
}

TEST_F(MailOutboxPlayerTest, PrepareRefusesWhenTheOutboxIsFullIdsRunOutOrTheComponentIsMissing)
{
	PlayerMailOutboxSystem::PreparedMails prepared;
	EXPECT_EQ(kInvalidParameter, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {}, prepared));
	EXPECT_EQ(kInvalidParameter, PlayerMailOutboxSystem::PrepareBattleOverflow(player, 0, {{kPotion, 1}}, prepared));

	for (std::size_t i = 0; i + 1 < mail_outbox_rules::kMaxEntries; ++i)
	{
		QueueEntry({{kPotion, 1}});
	}
	EXPECT_EQ(kSuccess, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 1}}, prepared)) << "还剩一个位置";
	EXPECT_EQ(kBagItemNotStacked, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 5000}}, prepared))
		<< "两封放不进只剩一个位置的队列";
	EXPECT_TRUE(prepared.empty());

	Outbox().clear_entries();
	tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Reset();
	EXPECT_EQ(kServiceUnavailable, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 1}}, prepared))
		<< "请求号取不到";
	EXPECT_TRUE(prepared.empty());

	ASSERT_TRUE(ArmSegment(GuidKind::kTxLog, "txlog", 7700000, 7800000));
	tlsEcs.actorRegistry.remove<PlayerMailOutboxComp>(player);
	EXPECT_EQ(kServiceUnavailable, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 1}}, prepared))
		<< "持久组件缺失不补建";
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerMailOutboxComp>(player));
}

// 没配发件密钥(没有部署 mail 的环境)不接改投:欠下一笔永远寄不出去的物品不如不欠,结算照旧整笔延后。
TEST_F(MailOutboxPlayerTest, WithoutAConfiguredSenderTheOverflowIsNotTaken)
{
	senderConfigured = false;
	EXPECT_FALSE(PlayerMailOutboxSystem::IsSenderConfigured());
	const auto idsBefore = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available();
	PlayerMailOutboxSystem::PreparedMails prepared;
	EXPECT_EQ(kServiceUnavailable, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 1}}, prepared));
	EXPECT_TRUE(prepared.empty());
	EXPECT_EQ(0, Outbox().entries_size());
	EXPECT_EQ(idsBefore, tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).Available()) << "一个请求号都没取";

	senderConfigured = true;
	EXPECT_TRUE(PlayerMailOutboxSystem::IsSenderConfigured());
	EXPECT_EQ(kSuccess, PlayerMailOutboxSystem::PrepareBattleOverflow(player, kBattle, {{kPotion, 1}}, prepared));
}

// ── 寄送 ─────────────────────────────────────────────────────────────────────────────

// 有条目而没有一条在已提交的存档快照里(刚加载,或结算那一笔还没落定):隔 5 秒请求一次存盘,不每拍都压;
// 落盘之后就寄,寄到删掉条目后再请求一次存盘。
TEST_F(MailOutboxPlayerTest, EntriesNotYetInACommittedSnapshotAskForASaveEveryFiveSeconds)
{
	const auto requestId = QueueEntry({{kPotion, 1}});
	Tick(10'000);
	EXPECT_TRUE(sends.empty());
	ASSERT_EQ(1u, saveRequests.size());
	EXPECT_EQ("mail_outbox_pending", saveRequests[0]);
	Tick(11'000);
	Tick(14'999);
	EXPECT_EQ(1u, saveRequests.size()) << "5 秒之内不重复请求";
	Tick(15'000);
	EXPECT_EQ(2u, saveRequests.size());

	durable.insert(requestId);
	Tick(15'500);
	ASSERT_EQ(1u, sends.size()) << "落盘之后就寄";
	EXPECT_EQ(2u, saveRequests.size()) << "可寄了就不再为它请求存盘";
	sends[0].done(SendOutcome{true, 0, 61});
	EXPECT_EQ(0, Outbox().entries_size());
	ASSERT_EQ(3u, saveRequests.size()) << "删条目之后请求存盘";
	EXPECT_EQ("mail_outbox_delivered", saveRequests[2]);
}

TEST_F(MailOutboxPlayerTest, OnlyDurableEntriesAreSentOneAtATimeAndDeliveryRemovesThem)
{
	const auto older = QueueEntry({{kPotion, 2}});
	const auto newer = QueueEntry({{kElixir, 1}});
	Tick(1'000);
	EXPECT_TRUE(sends.empty()) << "还没落盘的条目不寄:进程一崩结算会重做,邮件却已发出";

	durable.insert(newer);
	Tick(1'000);
	ASSERT_EQ(1u, sends.size());
	EXPECT_EQ(newer, sends[0].requestId);
	EXPECT_EQ(playerId, sends[0].playerId);
	durable.insert(older);
	Tick(1'500);
	EXPECT_EQ(1u, sends.size()) << "每名玩家至多一封在途";

	sends[0].done(SendOutcome{true, 0, 9001});
	ASSERT_EQ(1, Outbox().entries_size()) << "回了 mail_id 才删";
	EXPECT_EQ(older, Outbox().entries(0).request_id());
	Tick(1'500);
	ASSERT_EQ(2u, sends.size()) << "寄到一封,下一拍就寄下一封";
	EXPECT_EQ(older, sends[1].requestId);
	sends[1].done(SendOutcome{true, kSuccess, 9002});
	EXPECT_EQ(0, Outbox().entries_size());
	Tick(2'000);
	EXPECT_EQ(2u, sends.size());
}

// 验签被拒(两边密钥不同,或时钟相差超过 5 分钟):邮件没写,重试不会自己变好。留着条目,按拒收的 30 秒起退避,不当成结果未知的 1 秒。
TEST_F(MailOutboxPlayerTest, SignatureRejectionKeepsTheEntryOnTheRejectedSchedule)
{
	const auto requestId = QueueEntry({{kPotion, 1}});
	durable.insert(requestId);

	Tick(10'000);
	ASSERT_EQ(1u, sends.size());
	SendOutcome rejected{false, 0, 0};
	rejected.signatureRejected = true;
	sends[0].done(rejected);
	EXPECT_EQ(1, Outbox().entries_size()) << "没寄到就留着";
	Tick(10'000 + 1'000);
	EXPECT_EQ(1u, sends.size()) << "不走结果未知的 1 秒";
	Tick(10'000 + 30'000);
	ASSERT_EQ(2u, sends.size()) << "拒收的第一档是 30 秒";
}

TEST_F(MailOutboxPlayerTest, FailuresKeepTheEntryAndBackOff)
{
	const auto requestId = QueueEntry({{kPotion, 1}});
	durable.insert(requestId);

	Tick(10'000);
	ASSERT_EQ(1u, sends.size());
	sends[0].done(SendOutcome{false, 0, 0}); // 结果未知(第 1 次失败):1 秒后再试
	EXPECT_EQ(1, Outbox().entries_size()) << "没寄到就留着";
	Tick(10'500);
	EXPECT_EQ(1u, sends.size()) << "退避未到";
	Tick(11'000);
	ASSERT_EQ(2u, sends.size());

	sends[1].done(SendOutcome{true, kMailRecipientBoxFull, 0}); // 邮箱满(第 2 次失败):30 秒翻一倍,60 秒
	Tick(11'000 + 59'999);
	EXPECT_EQ(2u, sends.size());
	Tick(11'000 + 60'000);
	ASSERT_EQ(3u, sends.size());
	sends[2].done(SendOutcome{true, 0, 777}); // 删信之后寄到
	EXPECT_EQ(0, Outbox().entries_size());

	// 没发出去(没有就绪的 mail 节点)同样按结果未知退避,不挂在途;寄到一封之后连续失败从头数。
	const auto another = QueueEntry({{kElixir, 1}});
	durable.insert(another);
	nextStart = SendStart::kNoReadyNode;
	Tick(100'000);
	ASSERT_EQ(4u, sends.size());
	nextStart = SendStart::kSent;
	Tick(100'500);
	EXPECT_EQ(4u, sends.size()) << "没发出去也要等退避";
	Tick(101'000);
	EXPECT_EQ(5u, sends.size());
}

TEST_F(MailOutboxPlayerTest, LostAnswersAreRetriedAndALateDeliveryStillRemovesTheEntry)
{
	const auto requestId = QueueEntry({{kPotion, 1}});
	durable.insert(requestId);
	Tick(1'000);
	ASSERT_EQ(1u, sends.size());

	Tick(1'000 + 5'000);
	EXPECT_EQ(1u, sends.size()) << "调用期限之内不当丢失";
	Tick(1'000 + 60'000); // 远超「调用期限 + 2 秒」:按丢失记一次失败
	EXPECT_EQ(1u, sends.size()) << "判丢失的那一拍只记失败,不立刻重寄";
	Tick(1'000 + 61'000);
	ASSERT_EQ(2u, sends.size()) << "退避到点后重寄同一条";
	EXPECT_EQ(requestId, sends[1].requestId);

	sends[0].done(SendOutcome{false, 0, 0}); // 旧的那一次迟到的失败回答:不是当前在途的那一次,不再记
	Tick(1'000 + 61'000);
	EXPECT_EQ(2u, sends.size()) << "当前那一次仍在途,不因旧回答再寄一封";
	sends[0].done(SendOutcome{true, 0, 31}); // 旧的那一次迟到的「寄到了」:照样删条目
	EXPECT_EQ(0, Outbox().entries_size());
	sends[1].done(SendOutcome{true, 0, 31}); // 重寄的那一次拿回同一封:条目已经没了,什么都不做
	EXPECT_EQ(0, Outbox().entries_size());
}

TEST_F(MailOutboxPlayerTest, PlayersThatCannotBeModifiedAreNotSentAndKeepDeliveredEntries)
{
	const auto requestId = QueueEntry({{kPotion, 1}});
	durable.insert(requestId);
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0; // 没有归属围栏
	Tick(1'000);
	EXPECT_TRUE(sends.empty()) << "删条目是存档改动:此刻不可改就不寄,由下一个持有者寄";

	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 1;
	Tick(1'000);
	ASSERT_EQ(1u, sends.size());
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 0;
	sends[0].done(SendOutcome{true, 0, 41});
	EXPECT_EQ(1, Outbox().entries_size()) << "寄到了但此刻不可改:留着,下一个持有者重寄拿回同一封再删";

	// 实体不再登记在本节点:回答什么都不做。
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player).epoch = 1;
	Tick(2'000);
	ASSERT_EQ(2u, sends.size());
	tlsEcs.playerList.erase(playerId);
	sends[1].done(SendOutcome{true, 0, 41});
	EXPECT_EQ(1, Outbox().entries_size());
}
} // namespace
