// 通用资产通道 scene 侧处理流程的 ECS 级用例(docs/design/guild-phase2/04-asset-channel.md §4.14.2、
// §4.30、§4.33)。纯函数层的窗口/纪元算法在 asset_op_ledger_test.cpp,签名在 asset_op_auth_test.cpp。
//
// 这里验的是"接口的可观察行为":给定一次 AssetOpRequest,响应的 outcome / reason / durable / partial
// 是什么、账本与资产被改成什么样。不碰私有步骤。
//
// 确定性(AGENTS §11.4):时间、存盘、发币、密钥全部经 PlayerAssetOpSystem 的注入点替换,
// 不依赖真实墙钟、不连 Redis、不连 DataService。

#include <gtest/gtest.h>
#include <google/protobuf/unknown_field_set.h>
#include "engine/core/type_define/player_guid_comp.h"

#include <cstdint>
#include <filesystem>
#include <limits>
#include <string>
#include <string_view>
#include <system_error>
#include <vector>

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
		tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player_, kTestPlayerId);
		tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
		tlsEcs.actorRegistry.emplace<PlayerAssetOpLedgerComp>(player_);
		// 资产通道只在存盘带归属围栏的实体上记账(B4c,08-save-owner-fence.md §8.2.1):
		// 默认给一个已铸造的 epoch,epoch == 0 / 组件缺失的行为由 OwnerEpoch* 用例单独覆盖。
		tlsEcs.actorRegistry.emplace<PlayerOwnerEpochComp>(player_).epoch = 1;
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
		// 正式 outbox 中止重投保留原币包、业务号及 tx，仅改 RPC 与签名。
		return MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, seq, 30, TX_GUILD_DONATE, 77, epoch);
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
	// 本例验证背包已加载的玩家;缺背包的 Debit 与 Credit 行为由后续用例分别覆盖。
	GiveBags();
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	// Debit v1 只收恰好 1 条货币、0 件物品。
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

	// 已记账的结局只证明原始请求;同 seq 改成另一份合法包不能借用其收据。
	::AssetOpResponse itemReplay;
	PlayerAssetOpSystem::Debit(withItem, itemReplay);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, itemReplay.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), itemReplay.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, CallDebit(1, 30).outcome());
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
	::AssetOpResponse twoReplay;
	PlayerAssetOpSystem::Debit(twoCurrencies, twoReplay);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, twoReplay.outcome()) << "原请求已记账";
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), twoReplay.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, CallDebit(2, 30).outcome()) << "变包不能借用原收据";
	EXPECT_EQ(100u, Gold());
}

TEST_F(AssetOpSystemTest, DebitWithItemWithoutBagsRejectedAndRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player_));

	// Debit 不写物品,非法物品包应固定为拒绝,不能因缺背包先返回未记账的 RETRY。
	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);
	SignForTest("debit", request);
	::AssetOpResponse response;
	PlayerAssetOpSystem::Debit(request, response);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
	EXPECT_EQ(1, g_persistCalls);
	::AssetOpResponse replay;
	PlayerAssetOpSystem::Debit(request, replay);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, replay.outcome()) << "原包重投应返回已记录的拒绝";
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), replay.reason().id());
	const auto changedBundle = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, changedBundle.outcome()) << "同 seq 改包不能借用原收据";
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), changedBundle.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player_));
}

// Credit 确实需要写物品,缺持久背包时仍须整体重试,不能为了修复 Debit 而放开这道门禁。
// 请求须先通过配表校验；用真实配表保证本用例的物品载荷合法。
// 背包未加载时仍应在分配物品号或修改资产之前整体返回 RETRY。
TEST_F(AssetOpSystemTest, CreditMissingBagsRetriesBeforeAnyMixedBundleMutation)
{
	ASSERT_TRUE(ItemTableAvailable()) << "真实物品配置是合法载荷的前置，不能以坏包替代背包未加载场景";
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player_));
	const auto ledgerBefore = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString();
	const auto issuedBefore = tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued;
	PlayerAssetOpSystem::SetAddCurrencyFnForTest(&FakeAddCurrency);

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetPlayerNotHere), response.reason().id());
	EXPECT_FALSE(response.durable());
	EXPECT_FALSE(response.partial());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(ledgerBefore, tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString());
	EXPECT_EQ(issuedBefore, tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued);
	EXPECT_EQ(0, g_addCurrencyCalls);
	EXPECT_EQ(0, g_persistCalls);
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerBagsComp>(player_));
}

// v1 不支持的 P2 字段(item_uuids / pet_id)现在是**记账式终局拒绝**(§4.9 第 8 步)。
//
// 2026-09-19 之前这里断言的是相反行为("忽略、不记账"),理由是这两个字段没进签名串,
// 攻击者能在一条合法签名的请求上追加 pet_id 重发、把该 seq 永久钉成 REJECTED。
// 现在 canonical 已扩成 `…;u=…;p=…`,改任一字段签名即失效(见
// AssetOpAuthTest.SignatureCoversGuidAndPetTamper),追加攻击不复存在,
// 于是按规格记账才是对的 —— Go 能当场终结并退回业务,不必让 outbox 行永远 PENDING。
TEST_F(AssetOpSystemTest, UnsupportedP2FieldsRejectedAndRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	auto withPet = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	withPet.mutable_bundle()->set_pet_id(1);
	SignForTest("debit", withPet);
	::AssetOpResponse petResponse;
	PlayerAssetOpSystem::Debit(withPet, petResponse);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, petResponse.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), petResponse.reason().id());
	EXPECT_EQ(100u, Gold()) << "拒绝不改资产";
	EXPECT_NE(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "已记账:结局固定";
	::AssetOpResponse petReplay;
	PlayerAssetOpSystem::Debit(withPet, petReplay);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, petReplay.outcome()) << "原请求重查取回同一结局";
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), petReplay.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, CallDebit(1, 30).outcome()) << "同 seq 变包没有匹配身份的收据";
	EXPECT_EQ(100u, Gold());

	// item_uuids 同理,Credit 方向也一样。
	GiveBags();
	auto withUuids = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	withUuids.mutable_bundle()->add_item_uuids(123456789);
	const auto uuidResponse = CallCredit(withUuids);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, uuidResponse.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), uuidResponse.reason().id());
	EXPECT_NE(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT)) << "已记账";
}

// 篡改仍然挡得住,只是改由**签名**来挡,而不是靠信封档忽略载荷。
TEST_F(AssetOpSystemTest, TamperedP2FieldsFailAuthNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));

	// 攻击者拿到一条合法请求:先按真实载荷签名,再追加字段。
	auto tampered = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	SignForTest("debit", tampered);
	tampered.mutable_bundle()->set_pet_id(1);
	::AssetOpResponse tamperedResponse;
	PlayerAssetOpSystem::Debit(tampered, tamperedResponse);

	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, tamperedResponse.outcome()) << "签名已覆盖 pet_id,验签直接失败";
	EXPECT_EQ(static_cast<uint32_t>(kAssetAuthFailed), tamperedResponse.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "验签失败不记账:篡改包不得钉死结局";
	EXPECT_EQ(0, g_persistCalls);

	// 真实的那笔仍然扣得成 —— 这正是"验签失败不记账"要保住的东西。
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	EXPECT_EQ(70u, Gold());
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
	ASSERT_EQ(static_cast<uint32_t>(kSuccess), CurrencySystem::AttachDebt(player_, kCurrencyGold, 50, "test", "gm"));

	const auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_FALSE(response.partial());
	EXPECT_EQ(0u, Gold()) << "30 全额抵扣欠款,到手 0";
}

// 缺运行时货币状态也属于加载未完成:混合包必须在校验包内容、加物品或发币之前整体重试。
// 不依赖 Item 表、不跳过:有表时旧实现会先加物品再在发币处失败,无表时会误记成终局拒绝;
// 两种旧路径都不满足本例要求的 RETRY / kAssetPlayerNotHere 和账本原样。
TEST_F(AssetOpSystemTest, CreditMissingCurrencyRuntimeRetriesBeforeAnyMixedBundleMutation)
{
	GiveBags();
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory];
	const auto occupiedBefore = inventory.OccupiedGridCount();
	const auto itemCountBefore = inventory.GetTotalItemCount(10);
	const auto currencyBefore = tlsEcs.actorRegistry.get<CurrencyComp>(player_).SerializeAsString();
	const auto ledgerBefore = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString();
	const auto issuedBefore = tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued;
	tlsEcs.actorRegistry.remove<PlayerCurrencyComp>(player_);
	PlayerAssetOpSystem::SetAddCurrencyFnForTest(&FakeAddCurrency);

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetPlayerNotHere), response.reason().id());
	EXPECT_FALSE(response.durable());
	EXPECT_FALSE(response.partial());
	EXPECT_EQ(occupiedBefore, inventory.OccupiedGridCount());
	EXPECT_EQ(itemCountBefore, inventory.GetTotalItemCount(10));
	EXPECT_EQ(currencyBefore, tlsEcs.actorRegistry.get<CurrencyComp>(player_).SerializeAsString());
	EXPECT_EQ(ledgerBefore, tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT));
	EXPECT_EQ(issuedBefore, tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued);
	EXPECT_EQ(0, g_addCurrencyCalls);
	EXPECT_EQ(0, g_persistCalls);
	EXPECT_EQ(0, g_persistSkippedCalls);
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerCurrencyComp>(player_));
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

// Redis快照已含结局仍不能证明SQL提交；重查必须有机会补发强制保存来重建SQL证据。
TEST_F(AssetOpSystemTest, RedisDurableCanRebuildSQLCommitProof)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	CompleteFakeSave();

	g_fakeNowMs += kAssetOpResaveMinIntervalMs;
	const int before = g_persistCalls;

	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	EXPECT_TRUE(response.durable());
	EXPECT_EQ(before + 1, g_persistCalls) << "Redis durable不应阻止SQL证据修复";
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

// ── 归属围栏(B4c,08-save-owner-fence.md §8.2.1)──────────────────────────────
//
// owner_epoch == 0 时 SavePlayerToRedis 走无守卫的旧 Save:记下的结局可能被旧节点晚到的退出存盘
// 抹掉,而 Go 已据 durable 终结 —— 帮会记了资金,玩家侧的扣款却没了(04 K1)。所以改动类 RPC
// 在无围栏的实体上一律 RETRY、不记账、不触发存盘。

// E1:epoch == 0 时 Debit / Credit / AbortDebit 都不记账。
TEST_F(AssetOpSystemTest, OwnerEpochZeroRetryNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = 0;

	const auto debit = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, debit.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), debit.reason().id())
		<< "对玩家是\"角色迁移中,稍后自动重试\";kAssetBlocked 是货币封禁的文案";
	EXPECT_FALSE(debit.durable());

	const auto credit = CallCredit(MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP));
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, credit.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), credit.reason().id());

	const auto aborted = CallAbort(2);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, aborted.outcome()) << "中止占位也是一笔账本改动,同样会被抹掉";
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), aborted.reason().id());

	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "RETRY 不记账";
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_CREDIT));
	EXPECT_EQ(0, g_persistCalls) << "资产通道不在无围栏的实体上引发存盘";
}

// E2:组件缺失与 epoch == 0 同样处理(fail-closed)。
TEST_F(AssetOpSystemTest, OwnerEpochMissingRetryNotRecorded)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	tlsEcs.actorRegistry.remove<PlayerOwnerEpochComp>(player_);

	const auto debit = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, debit.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetFrozen), debit.reason().id());
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT));
	EXPECT_EQ(0, g_persistCalls);
}

// E3:已见 seq 的只读答复不受闸影响,但 epoch == 0 时不补存 —— 补存同样是一份无守卫的写。
TEST_F(AssetOpSystemTest, OwnerEpochZeroSeenSeqAnswersWithoutResave)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome()) << "epoch == 1 时照常记账";
	const int persistAfterApply = g_persistCalls;

	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = 0;
	g_fakeNowMs += kAssetOpResaveMinIntervalMs; // 限频窗口已过,若不设闸本该补存

	const auto seen = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, seen.outcome()) << "已见 seq 回原结局,不经改动闸";
	EXPECT_FALSE(seen.durable()) << "快照里还没有这笔,如实报未落盘";
	EXPECT_EQ(persistAfterApply, g_persistCalls) << "无围栏时不补存";
	EXPECT_EQ(70u, Gold()) << "绝不重扣";

	CompleteFakeSave();
	const auto afterSave = CallDebit(1, 30);
	EXPECT_TRUE(afterSave.durable()) << "durable 只看快照,与 epoch 无关";
}

// E4:闸只看当前 epoch,不留粘性状态 —— 铸造之后同一 seq 重投照常应用。
TEST_F(AssetOpSystemTest, OwnerEpochMintedLaterAppliesSameSeq)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = 0;
	ASSERT_EQ(ASSET_OP_OUTCOME_RETRY, CallDebit(1, 30).outcome());

	tlsEcs.actorRegistry.get<PlayerOwnerEpochComp>(player_).epoch = 7;
	const auto applied = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, applied.outcome());
	EXPECT_EQ(70u, Gold());
	EXPECT_EQ(1, g_persistCalls);
}
TEST_F(AssetOpSystemTest, ReceiptAckRequiresMatchingCorrelationAndKeepsOriginalOutcome)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto ack = MakeCurrencyRequest(ASSET_OP_STREAM_TRADE_DEBIT, 1, 30, TX_TRADE);
	SignForTest("debit", ack, "trade");
	::AssetOpResponse response;
	PlayerAssetOpSystem::Debit(ack, response);
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	auto& comp = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_);
	auto& stream = MutableAssetOpStream(comp, ASSET_OP_STREAM_TRADE_DEBIT);
	ASSERT_EQ(1, stream.receipts_size());
	auto* receipt = stream.mutable_receipts(0);
	receipt->mutable_snapshot()->add_items()->set_item_uuid(123);
	const auto digest = receipt->request_digest();
	const int beforeAck = g_persistCalls;
	ack.set_correlation_id(78);
	SignForTest("ack_receipt", ack, "trade");
	PlayerAssetOpSystem::AckReceipt(ack, response);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
	EXPECT_EQ(1, stream.receipts_size());
	EXPECT_EQ(beforeAck, g_persistCalls);
	ack.set_correlation_id(77); ack.mutable_bundle()->mutable_currencies(0)->set_amount(31);
	SignForTest("ack_receipt", ack, "trade");
	PlayerAssetOpSystem::AckReceipt(ack, response);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome()) << "同业务号的异载荷也不能释放原件";
	EXPECT_TRUE(stream.receipts(0).has_snapshot());
	ack.mutable_bundle()->mutable_currencies(0)->set_amount(30);
	ack.set_correlation_id(77); SignForTest("ack_receipt", ack, "trade");
	PlayerAssetOpSystem::AckReceipt(ack, response);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	ASSERT_EQ(1, stream.receipts_size());
	EXPECT_FALSE(stream.receipts(0).has_snapshot());
	EXPECT_EQ(digest, stream.receipts(0).request_digest());
	EXPECT_EQ(AssetOpSeqState::kApplied, ClassifyAssetOpSeq(&stream, kTestEpoch, 1));
	EXPECT_EQ(beforeAck + 1, g_persistCalls);
	EXPECT_FALSE(response.durable());
	CompleteFakeSave();
	PlayerAssetOpSystem::AckReceipt(ack, response);
	EXPECT_TRUE(response.durable());
	EXPECT_EQ(70u, Gold());
}

TEST_F(AssetOpSystemTest, CoinAndRejectedReplaysRequireOriginalIdentity)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, CallDebit(1, 30).outcome());
	CompleteFakeSave();
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, CallDebit(1, 31).outcome());
	EXPECT_EQ(70u, Gold());
	ASSERT_EQ(ASSET_OP_OUTCOME_REJECTED, CallDebit(2, 100).outcome());
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, CallDebit(2, 1).outcome());
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, CallDebit(2, 100).outcome());
	EXPECT_EQ(70u, Gold());
	ASSERT_EQ(2, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)->receipts_size());
	EXPECT_TRUE(ValidateAssetOpLedger(tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_)).empty());
}

TEST_F(AssetOpSystemTest, MailCreditReplayRequiresOriginalIdentity)
{
	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_SYSTEM_CREDIT, 1, 17, TX_MAIL_ATTACHMENT);
	SignForTest("credit", request, "mail");
	::AssetOpResponse response;
	PlayerAssetOpSystem::Credit(request, response);
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, response.outcome());
	CompleteFakeSave();
	request.mutable_bundle()->mutable_currencies(0)->set_amount(18);
	SignForTest("credit", request, "mail");
	PlayerAssetOpSystem::Credit(request, response);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
	EXPECT_EQ(17u, Gold());
	EXPECT_FALSE(response.durable());
}

TEST_F(AssetOpSystemTest, LegacySeenBitmapWithoutIdentityNeverProvesNewRequest)
{
	auto& comp = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_);
	auto& stream = MutableAssetOpStream(comp, ASSET_OP_STREAM_GUILD_DEBIT);
	ASSERT_TRUE(RecordAssetOpOutcome(stream, kTestEpoch, 1, AssetOpRecordKind::kApplied, 0));
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 70));
	CompleteFakeSave();
	const auto response = CallDebit(1, 30);
	EXPECT_EQ(ASSET_OP_OUTCOME_UNKNOWN, response.outcome());
	EXPECT_FALSE(response.durable());
	EXPECT_EQ(70u, Gold());
	EXPECT_EQ(0, g_persistCalls);
	EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << "兼容旧档，不清账本、不重复应用";
}

TEST_F(AssetOpSystemTest, FiveFullIdentityWindowsDoNotConsumeSnapshotBudgetOrBlockPruning)
{
	auto& comp = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_);
	const auto max = (std::numeric_limits<uint64_t>::max)();
	const auto watermark = max - 2048;
	for (const auto kind : {ASSET_OP_STREAM_GUILD_DEBIT, ASSET_OP_STREAM_GUILD_CREDIT,
		ASSET_OP_STREAM_TRADE_DEBIT, ASSET_OP_STREAM_TRADE_CREDIT, ASSET_OP_STREAM_SYSTEM_CREDIT})
	{
		auto& ledger = MutableAssetOpStream(comp, kind);
		ledger.set_stream_epoch(kTestEpoch);
		ledger.set_watermark(watermark);
		ledger.set_max_seq(watermark + kAssetOpWindowBits);
		for (int word = 0; word < kAssetOpWindowWords; ++word) ledger.set_seen_bits(word, max);
		for (uint64_t offset = 1; offset <= kAssetOpWindowBits; ++offset)
		{
			auto* receipt = ledger.add_receipts();
			receipt->set_seq(watermark + offset);
			receipt->set_correlation_id(max);
			receipt->set_request_digest(std::string(32, 'd'));
		}
	}
	ASSERT_GT(comp.ByteSizeLong(), kAssetOpMaxPlayerReceiptBytes);
	ASSERT_TRUE(ValidateAssetOpLedger(comp).empty());
	const auto response = CallAbort(watermark + kAssetOpWindowBits + 1);
	ASSERT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	const auto* ledger = Ledger(ASSET_OP_STREAM_GUILD_DEBIT);
	ASSERT_NE(nullptr, ledger);
	EXPECT_EQ(watermark + 1, ledger->watermark());
	EXPECT_EQ(kAssetOpWindowBits, static_cast<uint32_t>(ledger->receipts_size()));
	EXPECT_TRUE(ValidateAssetOpLedger(comp).empty());
}

TEST_F(AssetOpSystemTest, CompleteItemEscrowReplayAndCreditKeepIdentityGrowthAndUnknownFields)
{
	ASSERT_TRUE(ItemTableAvailable()) << "真实物品配置是本用例的前置，不能跳过托管验证";
	ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(1).first);
	GiveBags();
	auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory];
	ItemComp original;
	original.set_item_id(950199999); original.set_config_id(1); original.set_size(1);
	original.mutable_equip()->set_enhance_level(7);
	auto* affix = original.mutable_equip()->add_affixes(); affix->set_attr_id(3); affix->set_value(123); affix->set_tier(2); affix->set_seq(1);
	original.GetReflection()->MutableUnknownFields(&original)->AddVarint(100, 456);
	ASSERT_EQ(kSuccess, inventory.PutInstance(original));
	::AssetOpRequest debit = MakeCurrencyRequest(ASSET_OP_STREAM_TRADE_DEBIT, 1, 1, TX_TRADE);
	debit.clear_bundle(); debit.mutable_bundle()->add_item_uuids(original.item_id()); SignForTest("debit", debit, "trade");
	::AssetOpResponse removed;
	PlayerAssetOpSystem::Debit(debit, removed);
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, removed.outcome());
	ASSERT_EQ(1, removed.snapshot().items_size());
	EXPECT_EQ(nullptr, inventory.GetItemCompByGuid(original.item_id()));
	::AssetOpResponse replay;
	PlayerAssetOpSystem::Debit(debit, replay);
	EXPECT_EQ(removed.snapshot().SerializeAsString(), replay.snapshot().SerializeAsString()) << "重查必须返回同一份托管原件";
	::AssetOpRequest credit = MakeCurrencyRequest(ASSET_OP_STREAM_TRADE_CREDIT, 1, 1, TX_TRADE);
	credit.clear_bundle(); credit.mutable_bundle()->add_item_instances()->CopyFrom(removed.snapshot().items(0)); SignForTest("credit", credit, "trade");
	::AssetOpResponse delivered;
	PlayerAssetOpSystem::Credit(credit, delivered);
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, delivered.outcome());
	const auto* restored = inventory.GetItemCompByGuid(original.item_id());
	ASSERT_NE(nullptr, restored);
	auto compared = *restored; compared.clear_acquire_seq();
	EXPECT_EQ(original.SerializeAsString(), compared.SerializeAsString());
	PlayerAssetOpSystem::Credit(credit, delivered);
	ASSERT_EQ(ASSET_OP_OUTCOME_APPLIED, delivered.outcome());
	const auto* replayed = inventory.GetItemCompByGuid(original.item_id());
	ASSERT_NE(nullptr, replayed);
	auto replayedOriginal = *replayed; replayedOriginal.clear_acquire_seq();
	EXPECT_EQ(original.SerializeAsString(), replayedOriginal.SerializeAsString());
	std::vector<uint64_t> replayedGuids;
	inventory.ForEachItem([&replayedGuids](uint64_t guid, const ItemComp&) { replayedGuids.push_back(guid); });
	EXPECT_EQ((std::vector<uint64_t>{original.item_id()}), replayedGuids) << "同一入账请求不能复制实例";
	EXPECT_EQ(1u, inventory.OccupiedGridCount()) << "实例层只保留原件";
	EXPECT_EQ(1u, inventory.GridSlotCount()) << "原件与布局保持一一对应";
	EXPECT_EQ(0u, inventory.GetTotalItemCount(1)) << "带未知字段的原件不能降成可按配置消耗的普通单位";
}

// 携带着的本版本不认识的包类型记录(PlayerBagsComp::unsupportedBagTypes,混版本设计 §12 第 5 批)同样参与入账的
// guid 唯一性检查。漏查会让同一实例在新包与背包里各落一份,存盘原样写回,升回新版本后两份都还原(物品复制)。
// 携带记录的两种形状各验一次:只有 item_uuid 的旧档条目、只有整份实例的条目。
TEST_F(AssetOpSystemTest, CreditRejectsAnInstanceAlreadyCarriedInAnUnsupportedBagType)
{
	ASSERT_TRUE(ItemTableAvailable()) << "真实物品配置是本用例的前置";
	ASSERT_NE(nullptr, ItemTableManager::Instance().FindByIdSilent(1).first);
	GiveBags();
	auto& bags = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_);
	ItemComp carried;
	carried.set_item_id(950199997); carried.set_config_id(1); carried.set_size(1);

	for (uint64_t shape = 0; shape < 2; ++shape)
	{
		bags.unsupportedBagTypes = {};
		ItemEntry entry;
		entry.set_bag_type(static_cast<uint32_t>(kBagTypeCount));
		if (shape == 0)
		{
			entry.set_item_uuid(carried.item_id()); entry.set_config_id(1); entry.set_stack_size(1);
		}
		else
		{
			*entry.mutable_instance() = carried;
		}
		bags.unsupportedBagTypes.items.push_back(entry);
		ASSERT_TRUE(bags.ContainsGuid(carried.item_id())) << "shape=" << shape;

		::AssetOpRequest credit = MakeCurrencyRequest(ASSET_OP_STREAM_TRADE_CREDIT, 1 + shape, 1, TX_TRADE);
		credit.clear_bundle();
		auto* transfer = credit.mutable_bundle()->add_item_instances();
		transfer->set_item_uuid(carried.item_id()); transfer->set_config_id(1); transfer->set_stack_size(1);
		transfer->set_instance_proto(carried.SerializeAsString());
		SignForTest("credit", credit, "trade");
		::AssetOpResponse response;
		PlayerAssetOpSystem::Credit(credit, response);
		EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome()) << "shape=" << shape;
		EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id()) << "shape=" << shape;
		EXPECT_EQ(nullptr, bags.bags[kInventory].GetItemCompByGuid(carried.item_id())) << "背包零改动 shape=" << shape;
		EXPECT_EQ(1u, bags.unsupportedBagTypes.items.size()) << "携带记录不动 shape=" << shape;
	}
}

// 按数量扣出只认可互换的普通单位(混版本设计 §4.4 第 6 批)。只有算上带更新版本字段的单位才够 = 版本错位:
// 回 RETRY、不记账、背包不动(不能当普通单位交易出去,也不能记成终态让对侧白白退款);普通单位够时照常扣,
// 且只扣普通堆。确定不够(连算上都不够)照旧记终态拒绝。
TEST_F(AssetOpSystemTest, QuantityDebitOfStacksCarryingNewerVersionFieldsRetriesWithoutRecording)
{
	constexpr uint32_t kStackableConfig = 10;
	ASSERT_TRUE(ItemTableAvailable()) << "真实物品配置是本用例的前置";
	const auto* row = ItemTableManager::Instance().FindByIdSilent(kStackableConfig).first;
	ASSERT_NE(nullptr, row);
	ASSERT_GT(row->max_stack_size(), 7u) << "数量扣出只收可叠加配置";
	GiveBags();
	auto& inventory = tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory];
	ItemComp plain;
	plain.set_item_id(950199991); plain.set_config_id(kStackableConfig); plain.set_size(2);
	ItemComp fromNewerVersion;
	fromNewerVersion.set_item_id(950199992); fromNewerVersion.set_config_id(kStackableConfig); fromNewerVersion.set_size(5);
	fromNewerVersion.mutable_unknown_fields()->AddVarint(412345, 1); // 新版本给物品加的属性(例如绑定)
	inventory.RestoreItems({{plain, 0}, {fromNewerVersion, 1}});
	const auto ledgerBefore = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString();

	const auto quantityDebit = [](uint64_t seq, uint32_t count)
	{
		::AssetOpRequest request = MakeCurrencyRequest(ASSET_OP_STREAM_TRADE_DEBIT, seq, 1, TX_TRADE);
		request.clear_bundle();
		auto* item = request.mutable_bundle()->add_items();
		item->set_config_id(kStackableConfig);
		item->set_count(count);
		SignForTest("debit", request, "trade");
		return request;
	};

	::AssetOpResponse skewed;
	PlayerAssetOpSystem::Debit(quantityDebit(1, 4), skewed);
	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, skewed.outcome()) << "版本错位不得记终态";
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), skewed.reason().id());
	EXPECT_EQ(ledgerBefore, tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString()) << "不记账";
	EXPECT_EQ(2u, inventory.GetTotalItemCount(kStackableConfig));
	ASSERT_NE(nullptr, inventory.GetItemCompByGuid(fromNewerVersion.item_id()));
	EXPECT_EQ(5u, inventory.GetItemCompByGuid(fromNewerVersion.item_id())->size());

	// 同一 seq 没记过账:换成普通单位够的数量照常应用,只扣普通堆,带新字段的堆原样保留。
	::AssetOpResponse applied;
	PlayerAssetOpSystem::Debit(quantityDebit(1, 2), applied);
	EXPECT_EQ(ASSET_OP_OUTCOME_APPLIED, applied.outcome());
	EXPECT_EQ(nullptr, inventory.GetItemCompByGuid(plain.item_id()));
	ASSERT_NE(nullptr, inventory.GetItemCompByGuid(fromNewerVersion.item_id()));
	EXPECT_EQ(5u, inventory.GetItemCompByGuid(fromNewerVersion.item_id())->size());

	// 连算上带新字段的单位都不够:确定不足,照旧记终态拒绝。
	::AssetOpResponse insufficient;
	PlayerAssetOpSystem::Debit(quantityDebit(2, 6), insufficient);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, insufficient.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), insufficient.reason().id());
}

// ── 本二进制不认识的币种(混版本设计 §12、04-asset-channel.md §4.9 第 8b 步)────────────
// Go 各入口已按同源取值表(proto/common/base/currency_type.proto)拒绝写错的值,scene 遇到不认识的币种
// 只可能是更新版本加的:回 RETRY + kFeatureUnavailable,不记账、不存盘、任何资产都不动,
// 等玩家落到认识它的节点再应用;与版本无关的坏包、中止与已见 seq 的回放仍按原口径。

constexpr uint32_t kCurrencyFromNewerVersion = static_cast<uint32_t>(kCurrencyMax);

// Credit 先发物品再加币:8b 必须在任何物品副作用之前判出,否则这笔入账会半应用。
TEST_F(AssetOpSystemTest, CreditWithCurrencyUnknownToThisVersionRetriesWithoutAnyMutation)
{
	ASSERT_TRUE(ItemTableAvailable()) << "真实物品配置是合法载荷的前置";
	ASSERT_FALSE(IsKnownCurrencyType(kCurrencyFromNewerVersion));
	GiveBags();
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	PlayerAssetOpSystem::SetAddCurrencyFnForTest(&FakeAddCurrency);
	const auto ledgerBefore = tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString();
	const auto issuedBefore = tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued;

	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	auto* newer = request.mutable_bundle()->add_currencies();
	newer->set_currency_type(kCurrencyFromNewerVersion);
	newer->set_amount(5);
	auto* item = request.mutable_bundle()->add_items();
	item->set_config_id(10);
	item->set_count(1);
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome()) << "版本错位不得记终态";
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), response.reason().id());
	EXPECT_FALSE(response.durable());
	EXPECT_FALSE(response.partial());
	EXPECT_EQ(ledgerBefore, tlsEcs.actorRegistry.get<PlayerAssetOpLedgerComp>(player_).SerializeAsString()) << "不记账";
	EXPECT_EQ(0, g_persistCalls) << "不存盘";
	EXPECT_EQ(0, g_addCurrencyCalls) << "已知币种也不先加";
	EXPECT_EQ(100u, Gold());
	EXPECT_EQ(issuedBefore, tlsGuidSegmentRegistry.Get(GuidKind::kItem).GetStats().issued) << "不分配物品号";
	EXPECT_EQ(0u, tlsEcs.actorRegistry.get<PlayerBagsComp>(player_).bags[kInventory].GetTotalItemCount(10)) << "不发物品";
}

TEST_F(AssetOpSystemTest, DebitWithCurrencyUnknownToThisVersionRetriesWithoutRecording)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_DEBIT, 1, 30, TX_GUILD_DONATE);
	request.mutable_bundle()->mutable_currencies(0)->set_currency_type(kCurrencyFromNewerVersion);
	SignForTest("debit", request);
	::AssetOpResponse response;
	PlayerAssetOpSystem::Debit(request, response);

	EXPECT_EQ(ASSET_OP_OUTCOME_RETRY, response.outcome()) << "帮会单币种形状合法,只差在币种不认识:版本错位";
	EXPECT_EQ(static_cast<uint32_t>(kFeatureUnavailable), response.reason().id());
	EXPECT_EQ(nullptr, Ledger(ASSET_OP_STREAM_GUILD_DEBIT)) << "RETRY 不记账";
	EXPECT_EQ(0, g_persistCalls);
	EXPECT_EQ(100u, Gold());
}

// 重复币种与版本无关:在任何版本上都是坏包,照旧记终态拒绝,不能因为币种是新的就无限重投。
TEST_F(AssetOpSystemTest, DuplicateUnknownCurrencyIsStillRejected)
{
	auto request = MakeCurrencyRequest(ASSET_OP_STREAM_GUILD_CREDIT, 1, 30, TX_GUILD_SHOP);
	request.mutable_bundle()->clear_currencies();
	for (uint64_t amount : {5u, 6u})
	{
		auto* newer = request.mutable_bundle()->add_currencies();
		newer->set_currency_type(kCurrencyFromNewerVersion);
		newer->set_amount(amount);
	}
	const auto response = CallCredit(request);

	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, response.outcome());
	EXPECT_EQ(static_cast<uint32_t>(kAssetInvalidBundle), response.reason().id());
}

// 中止排在 8b 之前:旧节点仍能在截止时刻给带新币种的 seq 关门;此后同 seq 的扣款回放已定的拒绝。
TEST_F(AssetOpSystemTest, AbortStillClosesASeqWhoseBundleCarriesAnUnknownCurrency)
{
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	auto abortRequest = MakeAbortRequest(1);
	abortRequest.mutable_bundle()->mutable_currencies(0)->set_currency_type(kCurrencyFromNewerVersion);
	SignForTest("abort_debit", abortRequest);
	::AssetOpResponse aborted;
	PlayerAssetOpSystem::AbortDebit(abortRequest, aborted);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, aborted.outcome());
	EXPECT_EQ(0u, aborted.reason().id());

	auto debit = MakeAbortRequest(1);
	debit.mutable_bundle()->mutable_currencies(0)->set_currency_type(kCurrencyFromNewerVersion);
	SignForTest("debit", debit);
	::AssetOpResponse replay;
	PlayerAssetOpSystem::Debit(debit, replay);
	EXPECT_EQ(ASSET_OP_OUTCOME_REJECTED, replay.outcome()) << "已见 seq 先回放原结局,不走到 8b";
	EXPECT_EQ(100u, Gold());
}
} // namespace
