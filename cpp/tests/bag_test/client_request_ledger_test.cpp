// 客户端请求幂等键(docs/design/client-request-idempotency.md)的单测。
//
// 分三组:
//   * ClientRequestLedgerTest —— 落盘账本上的纯函数:查账的三种结论、键为 0、同一个键只记一次、满了淘汰 seq 最小的、
//     加载校验、序列化往返之后照样认得、参数指纹。
//   * ClientRequestDedupTest —— 挂在玩家实体上的接入层:账本是持久数据,读写两侧都不懒建;缺失或判坏一律 fail-closed;按玩家隔离。
//   * ClientRequestAttributeSchemeTest —— 新建加点方案的 handler:同键只建一次、只扣一次;换了名字拒绝且不执行;
//     重放排在方案上限判定之前;被拒的请求不记键;键为 0 保持旧行为;账本判坏时带键请求 fail-closed。
// 神秘商人购买、强化的同类用例分别在 player_mystery_merchant_test.cpp、player_equip_test.cpp(复用那边的夹具)。
// 存盘往返与「缺组件整份拒存」分别在 persist_round_trip_test.cpp、player_feature_persistence_test.cpp。

#include <gtest/gtest.h>

#include <cstdint>
#include <set>
#include <string>

#include "../../nodes/scene/handler/rpc/player/player_attribute_handler.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/type_define/type_define.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/common/component/currency_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "services/scene/player/system/client_request_ledger.h"
#include "services/scene/player/system/player_attribute.h"
#include "table/code/attributeallocratio_table.h"
#include "table/code/attributeautoplan_table.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/equipaffixpool_table.h"
#include "table/code/equipaffixrule_table.h"
#include "table/code/equipattribute_table.h"
#include "table/code/equipattributecap_table.h"
#include "table/code/equipenhance_table.h"
#include "table/code/equipgem_table.h"
#include "table/code/equipgemsocket_table.h"
#include "table/code/equipslot_table.h"
#include "table/code/item_table.h"
#include "table/proto/tip/attribute_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

namespace
{

constexpr uint64_t kKeyA = 0x1234'5678'9abc'def1ULL;
constexpr uint64_t kKeyB = 0x0fed'cba9'8765'4321ULL;
// 账本里的指纹固定 16 字节(kClientRequestFingerprintBytes);纯账本用例直接给定值,不必真去算。
const std::string kFp(kClientRequestFingerprintBytes, '\x11');
const std::string kOtherFp(kClientRequestFingerprintBytes, '\x22');

namespace ledger = client_request_ledger;

// ════════════════════════════════════════════════════════════════════════════
// 落盘账本上的纯函数
// ════════════════════════════════════════════════════════════════════════════

TEST(ClientRequestLedgerTest, UnknownKeyIsNewAndLeavesTheResultAlone)
{
	ClientRequestLedgerComp book;
	ClientRequestResult result;
	result.enhanceLevel = 7;
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_EQ(7u, result.enhanceLevel) << "only kReplay may write the result";
	EXPECT_EQ(0, book.records_size());
}

TEST(ClientRequestLedgerTest, SameKeyKindAndFingerprintReplaysTheFirstResult)
{
	ClientRequestLedgerComp book;
	ClientRequestResult first;
	first.enhanceSuccess = true;
	first.enhanceLevel = 6;
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, first));

	ClientRequestResult replayed;
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, replayed));
	EXPECT_TRUE(replayed.enhanceSuccess);
	EXPECT_EQ(6u, replayed.enhanceLevel);

	ASSERT_EQ(1, book.records_size());
	const auto &record = book.records(0);
	EXPECT_EQ(kKeyA, record.key());
	EXPECT_EQ(static_cast<uint32_t>(ClientRequestKind::kEnhanceEquip), record.kind());
	EXPECT_EQ(kFp, record.fingerprint());
	EXPECT_EQ(1u, record.seq()) << "an empty ledger (next_seq 0) starts at seq 1";
	EXPECT_EQ(2u, book.next_seq());
	EXPECT_TRUE(ledger::Validate(book).empty()) << ledger::Validate(book);
}

TEST(ClientRequestLedgerTest, SameKeyWithOtherParametersOrAnotherKindIsMismatch)
{
	ClientRequestLedgerComp book;
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kBuyMysteryMerchantGoods, kFp, ClientRequestResult{}));

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kMismatch,
			  ledger::Check(book, kKeyA, ClientRequestKind::kBuyMysteryMerchantGoods, kOtherFp, result))
		<< "old key reused for an operation with other parameters";
	EXPECT_EQ(ClientRequestVerdict::kMismatch, ledger::Check(book, kKeyA, ClientRequestKind::kCreateAttributeScheme, kFp, result))
		<< "old key reused across methods";
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, kKeyB, ClientRequestKind::kBuyMysteryMerchantGoods, kFp, result))
		<< "another key is another operation even with identical parameters";
}

TEST(ClientRequestLedgerTest, ZeroKeyIsNeverRememberedNorMatched)
{
	ClientRequestLedgerComp book;
	EXPECT_FALSE(ledger::Remember(book, 0, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	EXPECT_EQ(0, book.records_size());

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, 0, ClientRequestKind::kEnhanceEquip, kFp, result));
}

TEST(ClientRequestLedgerTest, RememberingAKeyTwiceKeepsTheFirstEntry)
{
	ClientRequestLedgerComp book;
	ClientRequestResult first;
	first.createdSchemeId = 2;
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kCreateAttributeScheme, kFp, first));

	ClientRequestResult second;
	second.createdSchemeId = 3;
	EXPECT_FALSE(ledger::Remember(book, kKeyA, ClientRequestKind::kCreateAttributeScheme, kFp, second));
	EXPECT_EQ(1, book.records_size());

	ClientRequestResult replayed;
	ASSERT_EQ(ClientRequestVerdict::kReplay,
			  ledger::Check(book, kKeyA, ClientRequestKind::kCreateAttributeScheme, kFp, replayed));
	EXPECT_EQ(2u, replayed.createdSchemeId);
}

TEST(ClientRequestLedgerTest, FullLedgerEvictsTheSmallestSeqFirst)
{
	constexpr auto kCapacity = static_cast<uint64_t>(ledger::kCapacity);
	ClientRequestLedgerComp book;
	for (uint64_t key = 1; key <= kCapacity; ++key)
	{
		ASSERT_TRUE(ledger::Remember(book, key, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	}
	EXPECT_EQ(ledger::kCapacity, book.records_size());

	ASSERT_TRUE(ledger::Remember(book, kCapacity + 1, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	EXPECT_EQ(ledger::kCapacity, book.records_size()) << "the ledger never grows past its capacity";

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, 1, ClientRequestKind::kEnhanceEquip, kFp, result))
		<< "the record with the smallest seq is the one evicted";
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, 2, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, kCapacity + 1, ClientRequestKind::kEnhanceEquip, kFp, result));

	// 再记一条,挤掉的是第二旧的那条:按 seq 淘汰,与键的大小、在 records 里的位置都无关。
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, 2, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, 3, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_TRUE(ledger::Validate(book).empty()) << ledger::Validate(book);
}

TEST(ClientRequestLedgerTest, LedgerLoadedAboveCapacityIsTrimmedOnTheNextRemember)
{
	// 容量下调过的旧存档:条数超过 kCapacity,但没超过加载上限,不算坏;下一次记账一次淘汰到位。
	ClientRequestLedgerComp book;
	const int loaded = ledger::kCapacity + 5;
	for (int i = 0; i < loaded; ++i)
	{
		auto *record = book.add_records();
		record->set_key(1000 + static_cast<uint64_t>(i));
		record->set_kind(static_cast<uint32_t>(ClientRequestKind::kEnhanceEquip));
		record->set_fingerprint(kFp);
		record->set_seq(static_cast<uint64_t>(i) + 1);
	}
	book.set_next_seq(static_cast<uint64_t>(loaded) + 1);
	ASSERT_TRUE(ledger::Validate(book).empty()) << ledger::Validate(book);

	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	EXPECT_EQ(ledger::kCapacity, book.records_size());
	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kNew, ledger::Check(book, 1000, ClientRequestKind::kEnhanceEquip, kFp, result));
	EXPECT_EQ(ClientRequestVerdict::kReplay, ledger::Check(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result));
}

TEST(ClientRequestLedgerTest, LedgerSurvivesSerializationAndStillReplays)
{
	// 存档里的样子:序列化成字节再读回来(加载就是这么拿到它的),首次结果与参数指纹都要原样认得。
	ClientRequestLedgerComp book;
	ClientRequestResult first;
	first.enhanceSuccess = false;
	first.enhanceLevel = 3;
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, first));

	ClientRequestLedgerComp reloaded;
	ASSERT_TRUE(reloaded.ParseFromString(book.SerializeAsString()));
	EXPECT_TRUE(ledger::Validate(reloaded).empty()) << ledger::Validate(reloaded);
	ClientRequestResult replayed;
	replayed.enhanceSuccess = true;
	ASSERT_EQ(ClientRequestVerdict::kReplay, ledger::Check(reloaded, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, replayed));
	EXPECT_FALSE(replayed.enhanceSuccess);
	EXPECT_EQ(3u, replayed.enhanceLevel);

	// 读回来之后继续记账,seq 接着往上走,不会和旧记录撞号。
	ASSERT_TRUE(ledger::Remember(reloaded, kKeyB, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	EXPECT_TRUE(ledger::Validate(reloaded).empty()) << ledger::Validate(reloaded);
}

TEST(ClientRequestLedgerTest, ValidateRejectsEveryInconsistentShape)
{
	auto valid = [] {
		ClientRequestLedgerComp book;
		EXPECT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
		EXPECT_TRUE(ledger::Remember(book, kKeyB, ClientRequestKind::kCreateAttributeScheme, kOtherFp, ClientRequestResult{}));
		return book;
	};
	EXPECT_TRUE(ledger::Validate(ClientRequestLedgerComp{}).empty()) << "an old save without the field is an empty ledger";
	ASSERT_TRUE(ledger::Validate(valid()).empty()) << ledger::Validate(valid());

	auto zeroKey = valid();
	zeroKey.mutable_records(0)->set_key(0);
	EXPECT_FALSE(ledger::Validate(zeroKey).empty());

	auto duplicateKey = valid();
	duplicateKey.mutable_records(1)->set_key(kKeyA);
	EXPECT_FALSE(ledger::Validate(duplicateKey).empty());

	auto shortFingerprint = valid();
	shortFingerprint.mutable_records(0)->set_fingerprint("short");
	EXPECT_FALSE(ledger::Validate(shortFingerprint).empty());

	auto zeroSeq = valid();
	zeroSeq.mutable_records(0)->set_seq(0);
	EXPECT_FALSE(ledger::Validate(zeroSeq).empty());

	auto duplicateSeq = valid();
	duplicateSeq.mutable_records(1)->set_seq(duplicateSeq.records(0).seq());
	EXPECT_FALSE(ledger::Validate(duplicateSeq).empty());

	auto seqNotBelowNext = valid();
	seqNotBelowNext.set_next_seq(seqNotBelowNext.records(1).seq());
	EXPECT_FALSE(ledger::Validate(seqNotBelowNext).empty()) << "next_seq must stay above every recorded seq";

	ClientRequestLedgerComp tooMany;
	for (int i = 0; i <= ledger::kMaxRecordsOnLoad; ++i)
	{
		auto *record = tooMany.add_records();
		record->set_key(static_cast<uint64_t>(i) + 1);
		record->set_fingerprint(kFp);
		record->set_seq(static_cast<uint64_t>(i) + 1);
	}
	tooMany.set_next_seq(static_cast<uint64_t>(ledger::kMaxRecordsOnLoad) + 2);
	EXPECT_FALSE(ledger::Validate(tooMany).empty());

	auto unknownKind = valid();
	unknownKind.mutable_records(0)->set_kind(200);
	EXPECT_TRUE(ledger::Validate(unknownKind).empty()) << "a kind written by a newer version is carried, not rejected";
}

::BuyMysteryMerchantGoodsRequest MerchantRequest(uint64_t key)
{
	::BuyMysteryMerchantGoodsRequest request;
	request.set_round_id(1'762'300'800);
	request.set_goods_id(3);
	request.set_count(2);
	request.set_expected_price(240);
	request.set_expected_currency_type(1);
	request.set_expected_item_config_id(1001);
	request.set_expected_item_count(5);
	request.set_client_request_id(key);
	return request;
}

TEST(ClientRequestLedgerTest, FingerprintIgnoresTheKeyButCatchesEveryOtherField)
{
	const auto original = MerchantRequest(kKeyA);
	const std::string fingerprint = ClientRequestFingerprint(original);
	EXPECT_EQ(kClientRequestFingerprintBytes, fingerprint.size());
	EXPECT_EQ(fingerprint, ClientRequestFingerprint(MerchantRequest(kKeyB))) << "the key itself is not a parameter";
	EXPECT_EQ(fingerprint, ClientRequestFingerprint(original)) << "deterministic across calls";
	EXPECT_EQ(kKeyA, original.client_request_id()) << "the key is cleared on a copy, not on the caller's request";

	auto otherRound = original;
	otherRound.set_round_id(original.round_id() + 1);
	auto otherCount = original;
	otherCount.set_count(3);
	auto otherPrice = original;
	otherPrice.set_expected_price(241);
	auto otherItem = original;
	otherItem.set_expected_item_config_id(1002);
	std::set<std::string> seen{fingerprint};
	for (const auto *changed : {&otherRound, &otherCount, &otherPrice, &otherItem})
	{
		EXPECT_TRUE(seen.insert(ClientRequestFingerprint(*changed)).second) << "every field takes part in the fingerprint";
	}

	::CreateAttributeSchemeRequest named;
	named.set_name("A");
	::CreateAttributeSchemeRequest renamed;
	renamed.set_name("B");
	EXPECT_NE(ClientRequestFingerprint(named), ClientRequestFingerprint(renamed));
}

TEST(ClientRequestLedgerTest, FingerprintOfAMapFieldDoesNotDependOnInsertionOrder)
{
	// 以后接入的请求可能带 map(比如加点):指纹走确定性序列化,同样的内容按不同顺序插入也相同。
	::AllocateAttributePointsRequest forward;
	forward.set_pool_id(1);
	(*forward.mutable_allocated())[1] = 10;
	(*forward.mutable_allocated())[2] = 20;
	(*forward.mutable_allocated())[3] = 30;
	::AllocateAttributePointsRequest backward;
	backward.set_pool_id(1);
	(*backward.mutable_allocated())[3] = 30;
	(*backward.mutable_allocated())[2] = 20;
	(*backward.mutable_allocated())[1] = 10;
	EXPECT_EQ(ClientRequestFingerprintDigest(forward), ClientRequestFingerprintDigest(backward));
}

TEST(ClientRequestLedgerTest, KindValuesAndNameTablesAreStable)
{
	// 种类值随存档落盘:改了它们,老存档里的记录就对不上号了。
	EXPECT_EQ(1u, static_cast<uint32_t>(ClientRequestKind::kEnhanceEquip));
	EXPECT_EQ(2u, static_cast<uint32_t>(ClientRequestKind::kBuyMysteryMerchantGoods));
	EXPECT_EQ(3u, static_cast<uint32_t>(ClientRequestKind::kCreateAttributeScheme));
	EXPECT_STREQ("enhance_equip", ClientRequestKindName(ClientRequestKind::kEnhanceEquip));
	EXPECT_STREQ("buy_mystery_merchant_goods", ClientRequestKindName(ClientRequestKind::kBuyMysteryMerchantGoods));
	EXPECT_STREQ("create_attribute_scheme", ClientRequestKindName(ClientRequestKind::kCreateAttributeScheme));
	EXPECT_STREQ("?", ClientRequestKindName(ClientRequestKind::kCount));
	EXPECT_STREQ("unavailable", ClientRequestVerdictName(ClientRequestVerdict::kUnavailable));
	EXPECT_STREQ("?", ClientRequestVerdictName(ClientRequestVerdict::kCount));
}

// ════════════════════════════════════════════════════════════════════════════
// 挂在玩家实体上的接入层
// ════════════════════════════════════════════════════════════════════════════

TEST(ClientRequestDedupTest, RecordsGoIntoThePersistedComponent)
{
	const auto player = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player); // 持久数据,生产里由加载创建

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kNew,
			  client_request_dedup::Check(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result));
	client_request_dedup::Remember(player, 0, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{});
	EXPECT_EQ(0, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).records_size()) << "key 0 records nothing";

	ClientRequestResult first;
	first.enhanceLevel = 4;
	client_request_dedup::Remember(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, first);
	EXPECT_EQ(1, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).records_size());

	ClientRequestResult replayed;
	EXPECT_EQ(ClientRequestVerdict::kReplay,
			  client_request_dedup::Check(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, replayed));
	EXPECT_EQ(4u, replayed.enhanceLevel);
	EXPECT_EQ(ClientRequestVerdict::kMismatch,
			  client_request_dedup::Check(player, kKeyA, ClientRequestKind::kEnhanceEquip, kOtherFp, replayed));
	tlsEcs.actorRegistry.destroy(player);
}

TEST(ClientRequestDedupTest, MissingLedgerFailsClosedAndIsNeverCreated)
{
	// 持久数据缺失 = 加载不变量被破坏:带键的请求不执行,读写两侧都不现造一本空账本来放行。
	const auto player = tlsEcs.actorRegistry.create();
	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kUnavailable,
			  client_request_dedup::Check(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result));
	client_request_dedup::Remember(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{});
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ClientRequestLedgerComp>(player));
	EXPECT_EQ(ClientRequestVerdict::kNew,
			  client_request_dedup::Check(player, 0, ClientRequestKind::kEnhanceEquip, kFp, result))
		<< "keyless requests never look at the ledger";
	tlsEcs.actorRegistry.destroy(player);
}

TEST(ClientRequestDedupTest, InvalidLedgerFailsClosedAndIsNotWritten)
{
	const auto player = tlsEcs.actorRegistry.create();
	auto &book = tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(player);
	ASSERT_TRUE(ledger::Remember(book, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{}));
	tlsEcs.actorRegistry.emplace<ClientRequestLedgerInvalidComp>(player); // 加载校验不过时挂上
	const std::string before = book.SerializeAsString();

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kUnavailable,
			  client_request_dedup::Check(player, kKeyA, ClientRequestKind::kEnhanceEquip, kFp, result))
		<< "not even a replay: a corrupt ledger cannot be trusted either way";
	client_request_dedup::Remember(player, kKeyB, ClientRequestKind::kEnhanceEquip, kFp, ClientRequestResult{});
	EXPECT_EQ(before, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).SerializeAsString())
		<< "the original data is never rewritten";
	tlsEcs.actorRegistry.destroy(player);
}

TEST(ClientRequestDedupTest, KeysArePerPlayer)
{
	const auto alice = tlsEcs.actorRegistry.create();
	const auto bob = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(alice);
	tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(bob);
	client_request_dedup::Remember(alice, kKeyA, ClientRequestKind::kBuyMysteryMerchantGoods, kFp, ClientRequestResult{});

	ClientRequestResult result;
	EXPECT_EQ(ClientRequestVerdict::kNew,
			  client_request_dedup::Check(bob, kKeyA, ClientRequestKind::kBuyMysteryMerchantGoods, kFp, result))
		<< "the same key from another player is another operation";
	EXPECT_EQ(0, tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(bob).records_size());
	tlsEcs.actorRegistry.destroy(alice);
	tlsEcs.actorRegistry.destroy(bob);
}

// ════════════════════════════════════════════════════════════════════════════
// 新建加点方案(handler 级)
// ════════════════════════════════════════════════════════════════════════════

constexpr uint64_t kSchemePlayerGuid = 9'100'000'001ULL;
constexpr uint32_t kSchemePlayerLevel = 80;
constexpr uint64_t kPlentyOfGold = 100'000'000ULL;

class ClientRequestAttributeSchemeTest : public testing::Test
{
protected:
	void SetUp() override
	{
		// 与 PlayerEquipAttributeTest 同一组表:面板要算装备加成,装备相关的表一并加载。
		ClassTableManager::Instance().Load();
		AttributePoolTableManager::Instance().Load();
		AttributeDimensionTableManager::Instance().Load();
		AttributeRuleTableManager::Instance().Load();
		AttributeAllocRatioTableManager::Instance().Load();
		AttributeAutoPlanTableManager::Instance().Load();
		ItemTableManager::Instance().Load();
		EquipSlotTableManager::Instance().Load();
		EquipAttributeTableManager::Instance().Load();
		EquipAttributeCapTableManager::Instance().Load();
		EquipAffixPoolTableManager::Instance().Load();
		EquipAffixRuleTableManager::Instance().Load();
		EquipEnhanceTableManager::Instance().Load();
		EquipGemTableManager::Instance().Load();
		EquipGemSocketTableManager::Instance().Load();
		player = NewPlayer();
		ASSERT_NO_FATAL_FAILURE(RequireRoomForTwoMoreSchemes());
	}

	void TearDown() override
	{
		if (tlsEcs.actorRegistry.valid(player))
		{
			tlsEcs.actorRegistry.destroy(player);
		}
	}

	// 照 PlayerEquipAttributeTest::NewPlayer:存档组件按加载链路挂上,InitializeOnLoad 补默认方案与二级属性缓存。
	static entt::entity NewPlayer()
	{
		const auto entity = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(entity, Guid{kSchemePlayerGuid});
		tlsEcs.actorRegistry.emplace<BaseAttributesComp>(entity);
		tlsEcs.actorRegistry.emplace<LevelComp>(entity).set_level(kSchemePlayerLevel);
		tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(entity);
		tlsEcs.actorRegistry.emplace<PlayerBagsComp>(entity);
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(entity);
		while (currency.values_size() < static_cast<int>(kCurrencyMax))
		{
			currency.add_values(0);
		}
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(entity);
		tlsEcs.actorRegistry.emplace<ClientRequestLedgerComp>(entity); // 持久数据,生产里由加载创建
		tlsEcs.actorRegistry.emplace<PlayerAttributeComp>(entity);
		PlayerAttributeSystem::InitializeOnLoad(entity);
		return entity;
	}

	void Fund(uint64_t gold) const
	{
		tlsEcs.actorRegistry.get<CurrencyComp>(player).set_values(static_cast<int>(kCurrencyGold), gold);
	}

	uint64_t Gold() const { return CurrencySystem::GetBalance(player, kCurrencyGold); }

	int Schemes() const { return tlsEcs.actorRegistry.get<PlayerAttributeComp>(player).schemes_size(); }

	int Records() const { return tlsEcs.actorRegistry.get<ClientRequestLedgerComp>(player).records_size(); }

	::AttributePanelInfo Panel() const
	{
		::AttributePanelInfo panel;
		EXPECT_EQ(kSuccess, PlayerAttributeSystem::BuildPanel(player, panel));
		return panel;
	}

	// 现读配表而不是写死数值;只把用例依赖的形状当前置条件核对。
	void RequireRoomForTwoMoreSchemes() const
	{
		ASSERT_GE(Panel().max_schemes(), static_cast<uint32_t>(Schemes()) + 2)
			<< "AttributeRule.max_schemes must leave room for two more schemes after the default one";
	}

	::CreateAttributeSchemeResponse Create(const std::string &name, uint64_t key) const
	{
		::CreateAttributeSchemeRequest request;
		request.set_name(name);
		request.set_client_request_id(key);
		::CreateAttributeSchemeResponse response;
		SceneAttributeClientPlayerHandler::CreateAttributeScheme(player, &request, &response);
		return response;
	}

	entt::entity player{entt::null};
};

TEST_F(ClientRequestAttributeSchemeTest, SameKeyCreatesOneSchemeAndChargesOnce)
{
	Fund(kPlentyOfGold);
	const int schemesBefore = Schemes();

	const auto first = Create("A", kKeyA);
	ASSERT_EQ(0u, first.error_message().id());
	ASSERT_NE(0u, first.scheme_id());
	EXPECT_FALSE(first.replayed());
	ASSERT_EQ(schemesBefore + 1, Schemes());
	EXPECT_EQ(1, Records());
	const uint64_t goldAfterFirst = Gold();

	// 回包没到玩家又点了一次,或客户端结果未知之后重发:同一个键,同一个名字。
	const auto second = Create("A", kKeyA);
	EXPECT_EQ(0u, second.error_message().id());
	EXPECT_TRUE(second.replayed());
	EXPECT_EQ(first.scheme_id(), second.scheme_id()) << "the replay answers with the scheme the first request created";
	EXPECT_TRUE(second.has_panel());
	EXPECT_EQ(schemesBefore + 1, Schemes()) << "no second scheme";
	EXPECT_EQ(goldAfterFirst, Gold()) << "no second charge";
	EXPECT_EQ(1, Records());
}

TEST_F(ClientRequestAttributeSchemeTest, SameKeyWithAnotherNameIsRefusedWithoutExecuting)
{
	Fund(kPlentyOfGold);
	ASSERT_EQ(0u, Create("A", kKeyA).error_message().id());
	const int schemes = Schemes();
	const uint64_t gold = Gold();

	// 客户端缺陷:把上一次的键用在了另一个名字上。
	const auto reused = Create("B", kKeyA);
	EXPECT_EQ(kInvalidParameter, reused.error_message().id());
	EXPECT_FALSE(reused.replayed());
	EXPECT_EQ(0u, reused.scheme_id());
	EXPECT_EQ(schemes, Schemes());
	EXPECT_EQ(gold, Gold());
}

TEST_F(ClientRequestAttributeSchemeTest, ReplayIsAnsweredBeforeTheSchemeLimitIsChecked)
{
	Fund(kPlentyOfGold);
	const uint32_t maxSchemes = Panel().max_schemes();
	uint64_t lastKey = 0;
	uint32_t lastSchemeId = 0;
	for (uint64_t key = 100; static_cast<uint32_t>(Schemes()) < maxSchemes; ++key)
	{
		const auto response = Create("S" + std::to_string(key), key);
		ASSERT_EQ(0u, response.error_message().id()) << "key=" << key;
		lastKey = key;
		lastSchemeId = response.scheme_id();
	}
	ASSERT_NE(0u, lastKey);
	ASSERT_EQ(kAttributeSchemeLimitReached, Create("New", kKeyB).error_message().id())
		<< "fixture: the limit is reached, so a new key is refused";

	// 首次建成之后上限就满了;重发要是先判上限,会被拒掉,客户端就以为没建成。
	const auto replay = Create("S" + std::to_string(lastKey), lastKey);
	EXPECT_EQ(0u, replay.error_message().id());
	EXPECT_TRUE(replay.replayed());
	EXPECT_EQ(lastSchemeId, replay.scheme_id());
	EXPECT_EQ(maxSchemes, static_cast<uint32_t>(Schemes()));
}

TEST_F(ClientRequestAttributeSchemeTest, RejectedRequestIsNotRememberedSoTheSameKeyIsJudgedAgain)
{
	ASSERT_GT(Panel().create_scheme_cost_gold(), 0u)
		<< "AttributeRule must charge for the scheme after the free ones for this case to mean anything";
	Fund(0);
	const int schemes = Schemes();
	EXPECT_EQ(kAttributeGoldNotEnough, Create("A", kKeyA).error_message().id());
	EXPECT_EQ(schemes, Schemes());
	EXPECT_EQ(0, Records()) << "a rejection leaves no record";

	// 补够钱之后带同一个键重发:上一次没有副作用,这一次才是首次执行。
	Fund(kPlentyOfGold);
	const auto retried = Create("A", kKeyA);
	EXPECT_EQ(0u, retried.error_message().id());
	EXPECT_FALSE(retried.replayed());
	EXPECT_EQ(schemes + 1, Schemes());
}

TEST_F(ClientRequestAttributeSchemeTest, ZeroKeyKeepsTheOldBehavior)
{
	Fund(kPlentyOfGold);
	const int schemes = Schemes();
	ASSERT_EQ(0u, Create("A", 0).error_message().id());
	const auto again = Create("A", 0);
	EXPECT_EQ(0u, again.error_message().id());
	EXPECT_FALSE(again.replayed());
	EXPECT_EQ(schemes + 2, Schemes()) << "without a key every request is a new operation";
	EXPECT_EQ(0, Records());
}

TEST_F(ClientRequestAttributeSchemeTest, InvalidLedgerRefusesKeyedRequestsButNotKeylessOnes)
{
	Fund(kPlentyOfGold);
	tlsEcs.actorRegistry.emplace<ClientRequestLedgerInvalidComp>(player); // 加载校验不过时挂上
	const int schemes = Schemes();
	const uint64_t gold = Gold();

	EXPECT_EQ(kServiceUnavailable, Create("A", kKeyA).error_message().id()) << "a keyed request cannot be checked, so it is not run";
	EXPECT_EQ(schemes, Schemes());
	EXPECT_EQ(gold, Gold());

	EXPECT_EQ(0u, Create("A", 0).error_message().id()) << "keyless requests never look at the ledger";
	EXPECT_EQ(schemes + 1, Schemes());
}

} // namespace
