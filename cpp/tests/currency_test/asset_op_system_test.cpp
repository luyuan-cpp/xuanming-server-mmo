// 通用资产通道 scene 侧处理流程的 ECS 级用例(docs/design/guild-phase2/04-asset-channel.md §4.14.2、
// §4.30、§4.33)。纯函数层的窗口/纪元算法在 asset_op_ledger_test.cpp,签名在 asset_op_auth_test.cpp。
//
// 这里验的是"接口的可观察行为":给定一次 AssetOpRequest,响应的 outcome / reason / durable / partial
// 是什么、账本与资产被改成什么样。不碰私有步骤。
//
// 确定性(AGENTS §11.4):时间、存盘、发币、密钥全部经 PlayerAssetOpSystem 的注入点替换,
// 不依赖真实墙钟、不连 Redis、不连 DataService。

#include <gtest/gtest.h>

#include <cstdint>
#include <filesystem>
#include <string>
#include <string_view>
#include <system_error>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "player/comp/last_persisted_snapshot_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"
#include "player/system/asset_op_auth.h"
#include "player/system/asset_op_ledger.h"
#include "player/system/asset_op_system.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/player_comp.pb.h" // UnregisterPlayer
#include "security/token_security.h"
#include "table/code/item_table.h"
#include "table/proto/tip/asset_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"

// 配表目录与格式。声明照 cpp/generated/table/code/item_table.cpp:7-8 的写法前置,
// 不为这两个函数去拉 engine/config 的整份头。
std::string GetConfigDir();
bool UseProtoBinaryTables();

namespace
{
// 夹具密钥:32 字节固定串(§4.32 要求 >= 32 字节,短于此视同未配)。只在测试进程内存在。
constexpr std::string_view kFixtureSecret = "asset-op-test-secret-0123456789ab";
constexpr uint64_t kTestPlayerId = 950100001;
constexpr uint64_t kTestEpoch = 100;

// 注入的假时钟。用例显式推进,重查限频与 300s 时钟偏差窗因此是确定性的。
int64_t g_fakeNowMs = 1700000000000;
int64_t FakeNowMs()
{
	return g_fakeNowMs;
}

// 模拟一次落盘回调到达:把当前账本原样拷进"最后一次成功写入 Redis 的玩家数据"。
void CaptureLedgerSnapshot(entt::entity player)
{
	PlayerAllData persisted;
	persisted.mutable_player_database_data()->mutable_asset_op_ledger()->CopyFrom(
		tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player));
	tlsEcs.actorRegistry.get_or_emplace<PlayerLastPersistedSnapshotComp>(player).Replace(persisted);
}

// 假存盘:只计数,回报"确实压了一次写",**不**更新快照
// (快照要由用例显式 CompleteFakeSave 模拟落盘回调)。
int g_persistCalls = 0;
bool FakePersist(entt::entity /*player*/)
{
	++g_persistCalls;
	return true;
}

// 另一种假存盘:模拟"脏比较判定与上次落盘逐字段相等,本次不写"——盘上**已经**是这份,
// 所以先把快照追平再返回 false。专门用来覆盖 PersistAndProbeDurable 的 !wrote 分支:
// 不写盘也必须回 durable=true,且不该打那条兜底 ERROR。
int g_persistSkippedCalls = 0;
bool FakePersistSkippedButOnDisk(entt::entity player)
{
	++g_persistSkippedCalls;
	CaptureLedgerSnapshot(player);
	return false;
}

// 假密钥查表:任何 caller 都用同一把夹具密钥;"流 ↔ caller" 白名单由 asset_op_auth 判。
std::string FakeSecretLookup(std::string_view /*caller*/)
{
	return std::string(kFixtureSecret);
}

// 假发币:前 g_addCurrencyFailAfter 次透传真实实现,之后一律回 g_addCurrencyFailCode。
int g_addCurrencyCalls = 0;
int g_addCurrencyFailAfter = -1; // -1 = 永不失败
uint32_t g_addCurrencyFailCode = kAssetBlocked;
uint32_t FakeAddCurrency(entt::entity player, CurrencyType type, int64_t amount, TransactionType txType,
						 uint64_t correlationId)
{
	++g_addCurrencyCalls;
	if (g_addCurrencyFailAfter >= 0 && g_addCurrencyCalls > g_addCurrencyFailAfter)
	{
		return g_addCurrencyFailCode;
	}
	return CurrencySystem::AddCurrency(player, type, amount, txType, correlationId);
}

// Item 表随 currency_test 部署与否不确定(该工程原本不碰配表)。没有就跳过物品用例,
// 而不是让它们以"表为空"的假象通过。
//
// **必须先判文件存在再 Load()**:`ItemTableManager::Load` 无条件走 `File2String`,
// 而 `file2string.cpp:19` 在打不开文件时是 `LOG_FATAL` —— muduo 的 FATAL 直接
// `abort()`(`Logging.cc:205`),会把整个 currency_test 进程连同 CurrencyTest* /
// ClientGmGateTest* 一起打死,try/catch 也接不住。currency_test 的 main 不像
// bag_test 那样调 `test_config::FindAndLoadTestConfig`,`GetConfigDir()` 因此是空串,
// 路径退化成 CWD 下的 item.json,而产物目录是 build/cpp/tests/ —— 默认就是"打不开"。
bool ItemTableAvailable()
{
	static const bool available = []
	{
		const std::string path = GetConfigDir() + (UseProtoBinaryTables() ? "item.pb" : "item.json");
		std::error_code ec;
		if (!std::filesystem::exists(path, ec))
		{
			return false; // 配表没随本工程部署:跳过物品用例,不是崩
		}
		ItemTableManager::Instance().Load();
		return ItemTableManager::Instance().FindByIdSilent(10).first != nullptr;
	}();
	return available;
}

class AssetOpSystemTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		g_fakeNowMs = 1700000000000;
		g_persistCalls = 0;
		g_persistSkippedCalls = 0;
		g_addCurrencyCalls = 0;
		g_addCurrencyFailAfter = -1;
		g_addCurrencyFailCode = kAssetBlocked;

		PlayerAssetOpSystem::SetNowFnForTest(&FakeNowMs);
		PlayerAssetOpSystem::SetPersistFnForTest(&FakePersist);
		PlayerAssetOpSystem::SetSecretLookupForTest(&FakeSecretLookup);

		player_ = tlsEcs.actorRegistry.create();
		tlsEcs.actorRegistry.emplace<Guid>(player_, kTestPlayerId);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player_);
		tlsEcs.playerList[kTestPlayerId] = player_;

		ArmItemSegment();
	}

	void TearDown() override
	{
		tlsEcs.playerList.erase(kTestPlayerId);
		if (tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_);
		}
		tlsGuidSegmentRegistry.Get(GuidKind::kItem).Reset();

		PlayerAssetOpSystem::SetNowFnForTest(nullptr);
		PlayerAssetOpSystem::SetPersistFnForTest(nullptr);
		PlayerAssetOpSystem::SetSecretLookupForTest(nullptr);
		PlayerAssetOpSystem::SetAddCurrencyFnForTest(nullptr);
	}

	// 给 item 号段塞一段够用的号:Credit 的 guid 余量预检要求 Available() >= Σcount。
	static void ArmItemSegment()
	{
		auto& segment = tlsGuidSegmentRegistry.Get(GuidKind::kItem);
		segment.Reset();
		GuidSegmentClient::Options options;
		options.kindName = "item";
		options.initialStep = 100000;
		const bool enabled = segment.Enable(
			std::move(options), [](const std::string&, uint32_t) { return true; },
			[](GuidSegmentClient::TimerKind, double, std::function<void()>) {}, [] { return 0.0; });
		ASSERT_TRUE(enabled);
		segment.Warm();
		segment.OnResponse(0, 1, 100001);
	}

	void GiveBags()
	{
		tlsEcs.actorRegistry.emplace<PlayerBagsComp>(player_);
	}

	// 模拟一次落盘回调到达:把当前账本原样拷进"最后一次成功写入 Redis 的玩家数据"。
	void CompleteFakeSave() { CaptureLedgerSnapshot(player_); }

	const AssetOpStreamLedger* Ledger(AssetOpStream stream) const
	{
		return FindAssetOpStream(tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_), stream);
	}

	uint64_t Gold() const
	{
		return CurrencySystem::GetBalance(player_, kCurrencyGold);
	}

	// caller 留空 = 用夹具默认 "guild"。签名必须在所有参与 canonical 的字段填好之后算。
	static void SignForTest(std::string_view rpc, ::AssetOpRequest& request,
							std::string_view caller = "guild", std::string_view secret = kFixtureSecret)
	{
		auto* auth = request.mutable_auth();
		auth->set_caller(std::string(caller));
		auth->set_timestamp_ms(static_cast<uint64_t>(g_fakeNowMs));
		auth->set_signature_hex(token_security::HmacSha256Hex(secret, AssetOpCanonical(rpc, request)));
	}

	static ::AssetOpRequest MakeCurrencyRequest(AssetOpStream stream, uint64_t seq, uint64_t amount,
											   TransactionType txType, uint64_t correlationId = 77,
											   uint64_t epoch = kTestEpoch)
	{
		::AssetOpRequest request;
		request.set_player_id(kTestPlayerId);
		request.set_stream(stream);
		request.set_seq(seq);
		request.set_correlation_id(correlationId);
		request.set_tx_type(static_cast<uint32_t>(txType));
		request.set_stream_epoch(epoch);
		auto* currency = request.mutable_bundle()->add_currencies();
		currency->set_currency_type(kCurrencyGold);
		currency->set_amount(amount);
		return request;
	}

	static ::AssetOpRequest MakeAbortRequest(uint64_t seq, uint64_t epoch = kTestEpoch)
	{
		::AssetOpRequest request;
		request.set_player_id(kTestPlayerId);
		request.set_stream(ASSET_OP_STREAM_GUILD_DEBIT);
		request.set_seq(seq);
		request.set_stream_epoch(epoch);
		return request;
	}

	// 一次完整的扣款调用(默认 GUILD_DEBIT / TX_GUILD_DONATE)。
	::AssetOpResponse CallDebit(uint64_t seq, uint64_t amount, uint64_t epoch = kTestEpoch)
	{
		auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, seq, amount, TX_GUILD_DONATE, 77, epoch);
		SignForTest("debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Debit(request, response);
		return response;
	}

	::AssetOpResponse CallAbort(uint64_t seq, uint64_t epoch = kTestEpoch)
	{
		auto request = MakeAbortRequest(seq, epoch);
		SignForTest("abort_debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::AbortDebit(request, response);
		return response;
	}

	::AssetOpResponse CallCredit(const ::AssetOpRequest& prepared)
	{
		::AssetOpRequest request = prepared;
		SignForTest("credit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Credit(request, response);
		return response;
	}

	entt::entity player_{entt::null};
};

// ── 扣款:成功 / durable / 幂等 ─────────────────────────────────────────────

TEST_F(AssetOpSystemTest, DebitAppliedThenDurable)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	const auto first = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, first.outcome());
	EXPECT_FALSE(first.durable()) << "落盘回调还没到,不许报 durable";
	EXPECT_FALSE(first.partial());
	EXPECT_EQ(70u, Gold());
	EXPECT_EQ(1, g_persistCalls);

	// 同 seq 重投:只读答复,绝不重扣。
	const auto again = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, again.outcome());
	EXPECT_EQ(70u, Gold());

	CompleteFakeSave();
	const auto afterSave = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, afterSave.outcome());
	EXPECT_TRUE(afterSave.durable());
	EXPECT_EQ(70u, Gold());
}

TEST_F(AssetOpSystemTest, DebitInsufficientRejectedFixed)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 10));

	const auto rejected = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, rejected.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetCurrencyInsufficient), rejected.reason().id());
	EXPECT_EQ(10u, Gold());

	// 结局固定(I2):后来钱够了,同 seq 仍然是拒绝。
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 90));
	const auto still = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, still.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetCurrencyInsufficient), still.reason().id());
	EXPECT_EQ(100u, Gold());
}

// ── 找人 / 闸门 ─────────────────────────────────────────────────────────────

TEST_F(AssetOpSystemTest, NotOnNode)
{
	tlsEcs.playerList.erase(kTestPlayerId);

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_NOT_HERE, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetPlayerNotHere), response.reason().id());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "未记账";
}

TEST_F(AssetOpSystemTest, FrozenRetryNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);

	const auto frozen = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, frozen.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), frozen.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "RETRY 不记账";
	EXPECT_EQ(0, g_persistCalls);

	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player_);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	EXPECT_EQ(70u, Gold());
}

// 传送/换节点交接在途:PlayerFrozenComp 与 PlayerTravelHandoffComp 成对挂,但只挂了 handoff
// (或先后顺序错开)时也必须拒。复用 IsCrossZoneFrozen 会漏掉这一格 —— 那时 SavePlayerToRedis
// 会跳过写盘并返回 false,结局会被误报成 durable。
TEST_F(AssetOpSystemTest, TravelHandoffRetryNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto& travel = tlsEcs.actorRegistry.emplace<PlayerTravelHandoffComp>(player_);
	travel.targetZoneId = 2;
	travel.requestedAtMs = 1;

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), response.reason().id());
	EXPECT_FALSE(response.durable());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT));
	EXPECT_EQ(0, g_persistCalls) << "交接在途绝不触发存盘";
}

TEST_F(AssetOpSystemTest, InBattleRetryAbortAllowed)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto& inBattle = tlsEcs.actorRegistry.emplace<InBattleComp>(player_);
	inBattle.set_battle_id(4242);

	const auto blocked = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, blocked.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInBattle), blocked.reason().id());
	EXPECT_EQ(100u, Gold());

	// 中止不改资产,战斗中也允许 —— 它只是把这条 seq 的门关上。
	const auto aborted = CallAbort(1);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, aborted.outcome());
	EXPECT_EQ(0u, aborted.reason().id());

	tlsEcs.actorRegistry.remove<InBattleComp>(player_);
	const auto afterBattle = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, afterBattle.outcome()) << "中止占位已固定结局";
	EXPECT_EQ(100u, Gold());
}

TEST_F(AssetOpSystemTest, AbortSeenAppliedReturnsApplied)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());

	const auto aborted = CallAbort(1);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, aborted.outcome()) << "已应用的 seq 中止不了";
	EXPECT_EQ(70u, Gold());
}

TEST_F(AssetOpSystemTest, UnregisterRetryButReadOnlyAnswers)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	const int persistAfterApply = g_persistCalls;

	tlsEcs.actorRegistry.emplace<UnregisterPlayer>(player_);

	const auto newSeq = CallDebit(2, 10);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, newSeq.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetPlayerNotHere), newSeq.reason().id());
	EXPECT_EQ(70u, Gold());

	g_fakeNowMs += kAssetOpResaveMinIntervalMs; // 限频窗口已过,仍不该触发存盘
	const auto seen = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, seen.outcome()) << "已见 seq 不经闸门";
	EXPECT_EQ(persistAfterApply, g_persistCalls) << "退出存盘在途,不再触发存盘";
}

TEST_F(AssetOpSystemTest, LedgerInvalidTagRetry)
{
	tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerInvalidComp>(player_);

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), response.reason().id());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT));
}

// ── 信封 / 验签 / seq 不可采信 ──────────────────────────────────────────────

TEST_F(AssetOpSystemTest, EnvelopeInvalid)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	// 方向不符:GUILD_CREDIT 调 AssetDebit
	{
		auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
		SignForTest("debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Debit(request, response);
		EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	}
	// tx 不在该流的白名单里
	{
		auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_SHOP);
		SignForTest("debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Debit(request, response);
		EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	}
	// seq = 0
	{
		auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 0, 30, TX_GUILD_DONATE);
		SignForTest("debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Debit(request, response);
		EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	}
	// 纪元 = 0
	{
		auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE, 77, 0);
		SignForTest("debit", request);
		::AssetOpResponse response;
		PlayerAssetOpSystem::Debit(request, response);
		EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
		EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	}

	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT));
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT));
}

TEST_F(AssetOpSystemTest, AuthFailureUnknownNoRecord)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	// 另一把 32 字节密钥签名:信封合法,签名不符。
	SignForTest("debit", request, "guild", "another-32-byte-secret-000000000");
	::AssetOpResponse response;
	PlayerAssetOpSystem::Debit(request, response);

	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetAuthFailed), response.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT));
	EXPECT_EQ(0, g_persistCalls);
}

TEST_F(AssetOpSystemTest, BehindWindowUnknown)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 1000));

	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 1).outcome());
	// 每步都在跳号上限(max_seq + 1024)之内,一步步把窗口推上去:
	// 1024 仍在窗内;2048 触发整窗右移,watermark 变成 1024。
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1024, 1).outcome());
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(2048, 1).outcome());

	const auto slidOut = CallDebit(1, 1);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, slidOut.outcome()) << "已滑出窗口,结局不可知";
	EXPECT_EQ(0u, slidOut.reason().id());
}

TEST_F(AssetOpSystemTest, JumpTooFarUnknown)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 1000));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());

	const auto tooFar = CallDebit(2000, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, tooFar.outcome());
	EXPECT_EQ(0u, tooFar.reason().id());
	EXPECT_EQ(970u, Gold()) << "跳号不记账、不扣钱";
}

// ── 流纪元(§4.30)──────────────────────────────────────────────────────────

TEST_F(AssetOpSystemTest, EpochResetAppliesFresh)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30, 100).outcome());
	EXPECT_EQ(70u, Gold());

	// 新纪元 = 另一本流水簿,同号不是同一笔业务。
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30, 200).outcome());
	EXPECT_EQ(40u, Gold());

	const auto* ledger = Ledger(ASSET_OP_STREAM_GUILD_DEBIT);
	ASSERT_NE(nullptr, ledger);
	EXPECT_EQ(200u, ledger->stream_epoch());
}

TEST_F(AssetOpSystemTest, StaleEpochUnknownNoRecord)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30, 200).outcome());

	const auto stale = CallDebit(1, 30, 100);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, stale.outcome());
	EXPECT_EQ(0u, stale.reason().id());
	EXPECT_EQ(70u, Gold());

	const auto* ledger = Ledger(ASSET_OP_STREAM_GUILD_DEBIT);
	ASSERT_NE(nullptr, ledger);
	EXPECT_EQ(200u, ledger->stream_epoch()) << "旧纪元的请求不得改动账本";
}

// 闸门回 RETRY 时**不得**重置纪元,否则已见结局会凭空消失(违反 I2)。
TEST_F(AssetOpSystemTest, RetryDoesNotResetEpoch)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30, 100).outcome());

	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(player_);
	const auto frozen = CallDebit(1, 30, 200);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, frozen.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), frozen.reason().id());

	const auto* ledger = Ledger(ASSET_OP_STREAM_GUILD_DEBIT);
	ASSERT_NE(nullptr, ledger);
	EXPECT_EQ(100u, ledger->stream_epoch());

	tlsEcs.actorRegistry.remove<PlayerFrozenComp>(player_);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30, 100).outcome()) << "旧纪元结局仍在";
	EXPECT_EQ(70u, Gold());
}

// ── 包内容 ──────────────────────────────────────────────────────────────────

TEST_F(AssetOpSystemTest, DebitBundleInvalidRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	// Debit v1 只收恰好 1 条货币、0 件物品
	auto withItem = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	auto* item = withItem.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);
	SignForTest("debit", withItem);
	::AssetOpResponse response;
	PlayerAssetOpSystem::Debit(withItem, response);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	EXPECT_EQ(100u, Gold());

	// 已记账:换成合法包重投同 seq,仍然是拒绝。
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, CallDebit(1, 30).outcome());
	EXPECT_EQ(100u, Gold());

	// 2 条货币同样违反 Debit v1 的"恰好 1 条货币、0 件物品"(§4.14.2 点名的第二个子例);
	// 换 seq 2,免得撞上已记账的 seq 1。
	auto twoCurrencies = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 2, 30, TX_GUILD_DONATE);
	auto* extra = twoCurrencies.mutable_bundle()->add_currencies();
	extra->set_currency_type(kCurrencyDiamond);
	extra->set_amount(5);
	SignForTest("debit", twoCurrencies);
	::AssetOpResponse twoResponse;
	PlayerAssetOpSystem::Debit(twoCurrencies, twoResponse);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, twoResponse.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), twoResponse.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, CallDebit(2, 30).outcome()) << "已记账";
	EXPECT_EQ(100u, Gold());
}

// 未进签名串的 P2 字段(item_uuids / pet_id)必须被**忽略**,而不是记成终局拒绝:
// 记了的话,同网段任意进程在一条合法签名的请求上追加 pet_id 重发,就能把这条 seq
// 永久钉成 REJECTED,玩家那笔真实捐献再也扣不成。详见 HasUnsupportedP2Fields 的注释。
TEST_F(AssetOpSystemTest, UnsignedP2FieldsIgnoredNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	// 攻击者拿到的是一条合法请求:先按真实载荷签名,再追加不进签名串的字段。
	auto tampered = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	SignForTest("debit", tampered);
	tampered.mutable_bundle()->set_pet_id(1);
	::AssetOpResponse tamperedResponse;
	PlayerAssetOpSystem::Debit(tampered, tamperedResponse);

	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, tamperedResponse.outcome()) << "签名仍然通过,但载荷不认";
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), tamperedResponse.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "未记账:结局不得被篡改包钉死";
	EXPECT_EQ(0, g_persistCalls);

	// 真实的那笔仍然扣得成 —— 这正是"不记账"要保住的东西。
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	EXPECT_EQ(70u, Gold());

	// item_uuids 同理(Credit 方向也一样判在信封档)。
	GiveBags();
	auto withUuids = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	withUuids.mutable_bundle()->add_item_uuids(123456789);
	const auto uuidResponse = CallCredit(withUuids);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, uuidResponse.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), uuidResponse.reason().id());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT));
}

TEST_F(AssetOpSystemTest, CreditUnknownItemRejected)
{
	GiveBags();
	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	request.mutable_bundle()->clear_currencies();
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(999999999); // Item 表里不存在
	item->set_count(1);

	const auto response = CallCredit(request);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
}

// ── 发放 ────────────────────────────────────────────────────────────────────

TEST_F(AssetOpSystemTest, CreditWithDebtClawback)
{
	// 补缴:欠款先扣属于正常应用,记 APPLIED。
	CurrencySystem::AttachDebt(player_, kCurrencyGold, 50, "test", "gm");

	const auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_FALSE(response.partial());
	EXPECT_EQ(0u, Gold()) << "30 全额抵扣欠款,到手 0";
}

TEST_F(AssetOpSystemTest, CreditBlockedRejected)
{
	ASSERT_EQ(kSuccess, CurrencySystem::BlockCurrency(player_, kCurrencyGold));

	const auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetBlocked), response.reason().id());
	EXPECT_EQ(0u, Gold());

	// 已记账:解封后同 seq 仍然是拒绝。
	ASSERT_EQ(kSuccess, CurrencySystem::UnblockCurrency(player_, kCurrencyGold));
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, CallCredit(request).outcome());
	EXPECT_EQ(0u, Gold());
}

TEST_F(AssetOpSystemTest, CreditFirstCurrencyFailsRetry)
{
	// 只有 1 条货币且失败 → 一点没改,回 RETRY 且不记账(可安全重投)。
	g_addCurrencyFailAfter = 0;
	g_addCurrencyFailCode = kAssetFrozen;
	PlayerAssetOpSystem::SetAddCurrencyFnForTest(&FakeAddCurrency);

	const auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), response.reason().id());
	EXPECT_EQ(0u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT));
}

TEST_F(AssetOpSystemTest, CreditPartialFlagged)
{
	// 第 1 条货币到账、第 2 条失败:已有改动就不能回 RETRY(重投会重复发放),
	// 记 kAppliedPartial 并如实报 partial。
	g_addCurrencyFailAfter = 1;
	g_addCurrencyFailCode = kAssetBlocked;
	PlayerAssetOpSystem::SetAddCurrencyFnForTest(&FakeAddCurrency);

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	auto* second = request.mutable_bundle()->add_currencies();
	second->set_currency_type(kCurrencyDiamond);
	second->set_amount(5);

	const auto response = CallCredit(request);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_TRUE(response.partial());
	EXPECT_EQ(static_cast<uint32_t>(kAssetPartialApplied), response.reason().id());
	EXPECT_EQ(30u, Gold());
	EXPECT_EQ(0u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));

	const auto* ledger = Ledger(ASSET_OP_STREAM_GUILD_CREDIT);
	ASSERT_NE(nullptr, ledger);
	EXPECT_TRUE(IsAssetOpPartial(*ledger, 1));

	// 重查仍然 partial(粘性),Go 据此转人工补偿而不是对侧入账。
	const auto again = CallCredit(request);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, again.outcome());
	EXPECT_TRUE(again.partial());
	EXPECT_EQ(30u, Gold());
}

TEST_F(AssetOpSystemTest, CreditBagFullRetry)
{
	if (!ItemTableAvailable())
	{
		GTEST_SKIP() << "Item 表未随 currency_test 部署,跳过物品发放用例";
	}
	GiveBags();
	auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_);
	bags.bags[kInventory].SetCapacityForRestore(0); // 塞满 = 一格都没有

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	request.mutable_bundle()->clear_currencies();
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);

	const auto full = CallCredit(request);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, full.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetBagFull), full.reason().id());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT)) << "背包满不记账";

	bags.bags[kInventory].SetCapacityForRestore(10);
	const auto afterSpace = CallCredit(request);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, afterSpace.outcome());
	EXPECT_FALSE(afterSpace.partial());
}

// ── durable / 存盘限频 ──────────────────────────────────────────────────────

TEST_F(AssetOpSystemTest, ResaveThrottle)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	ASSERT_EQ(1, g_persistCalls);

	// 时间不前进:连查 3 次也不再触发存盘。
	for (int i = 0; i < 3; ++i)
	{
		const auto response = CallDebit(1, 30);
		EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
		EXPECT_FALSE(response.durable());
	}
	EXPECT_EQ(1, g_persistCalls);

	g_fakeNowMs += kAssetOpResaveMinIntervalMs;
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	EXPECT_EQ(2, g_persistCalls) << "过了限频窗口才允许再请求一次存盘";
}

// 快照已含该结局时,重查直接从快照答 durable,**不**再请求存盘。
TEST_F(AssetOpSystemTest, AlreadyDurableSkipsPersist)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	CompleteFakeSave();

	g_fakeNowMs += kAssetOpResaveMinIntervalMs; // 限频窗口已过,仍不该触发存盘
	const int before = g_persistCalls;

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_TRUE(response.durable());
	EXPECT_EQ(before, g_persistCalls) << "已 durable 就不该再请求存盘";
}

// 脏比较判定"与上次落盘逐字段相等"→ SavePlayerToRedis 返回 false。
// 那不是"没落盘",而是"盘上已经是这份",durable 必须为真(§4.6)。
// 用 FakePersistSkippedButOnDisk 才能真正走到这条分支:快照若已含结局,
// PersistAndProbeDurable 根本不会被调用。
TEST_F(AssetOpSystemTest, SkippedSaveMeansDurable)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	ASSERT_FALSE(CallDebit(1, 30).durable()) << "落盘回调还没到";

	// 换成"不写盘,但盘上已经是这份"的假存盘,并放开限频窗口。
	PlayerAssetOpSystem::SetPersistFnForTest(&FakePersistSkippedButOnDisk);
	g_fakeNowMs += kAssetOpResaveMinIntervalMs;

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_TRUE(response.durable()) << "不写盘 ≠ 没落盘";
	EXPECT_EQ(1, g_persistSkippedCalls) << "确实走到了 PersistAndProbeDurable";
}

TEST_F(AssetOpSystemTest, AbortDurableAfterSave)
{
	const auto aborted = CallAbort(5);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, aborted.outcome());
	EXPECT_EQ(0u, aborted.reason().id());
	EXPECT_FALSE(aborted.durable());

	CompleteFakeSave();
	g_fakeNowMs += kAssetOpResaveMinIntervalMs;

	const auto again = CallAbort(5);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, again.outcome());
	EXPECT_TRUE(again.durable()) << "REJECTED 也必须 durable 才允许 Go 终结(I4 / C9)";
}
} // namespace
