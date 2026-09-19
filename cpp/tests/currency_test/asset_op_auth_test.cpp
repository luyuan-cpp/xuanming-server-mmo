// 通用资产通道请求验签的单测,对应 docs/design/guild-phase2/04-asset-channel.md §4.32
// (不变量 I9:资产 RPC 须签名)。ECS 级的处理流程在 asset_op_system_test.cpp,
// 窗口/纪元算法在 asset_op_ledger_test.cpp。
//
// 这里只测纯函数层:不建实体、不碰 ECS、不读表、不读进程环境变量(密钥一律用注入的
// 假查表)、不用真实墙钟(nowMs 是入参)。因此确定、可重复(AGENTS §11.4)。
//
// **本文件最重要的一条用例是 CanonicalGolden**:那个字面量与 Go
// go/shared/assetop/auth_test.go:22 的 canonicalGolden 逐字节相同,两边用同一组输入。
// 任何一侧改了拼串规则而忘了改另一侧,这条用例当场红;否则线上表现是"全部资产 RPC
// 验签失败",帮会捐献与聚宝斋寄售整体卡死,而且从日志上只看得到一片 auth_failed。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <string_view>

#include "player/system/asset_op_auth.h"
#include "security/token_security.h"

namespace
{
// 夹具密钥:33 字节固定串(§4.32 要求去空白后 >= 32 字节)。只在测试进程内存在,
// 与生产密钥无关(AGENTS §11.3:真实密钥不进仓库)。
constexpr std::string_view kFixtureSecret = "asset-op-test-secret-0123456789ab";
static_assert(kFixtureSecret.size() >= kAssetOpAuthMinSecretBytes, "夹具密钥必须满足最小长度");

// 差一个字节就不够的密钥,用来钉住"恰好 31 字节 = 未配置"这条边界。
constexpr std::string_view kTooShortSecret = "asset-op-test-secret-0123456789";
static_assert(kTooShortSecret.size() == kAssetOpAuthMinSecretBytes - 1, "本串必须正好差 1 字节");

// 另一把密钥,用来验"每个调用方一把、互不共用":拿 guild 的密钥签 trade 的包必须失败。
constexpr std::string_view kOtherFixtureSecret = "asset-op-other-secret-0123456789ab";
static_assert(kOtherFixtureSecret.size() >= kAssetOpAuthMinSecretBytes, "另一把夹具密钥也要够长");

// 冻结的"当前时间":与 golden 的 timestamp_ms 同值,于是默认偏差为 0。
constexpr int64_t kFrozenNowMs = 1700000000123;

// ── 假密钥查表(全部是函数指针,签名见 AssetOpSecretLookup)──────────────────

std::string FixtureSecretLookup(std::string_view /*caller*/)
{
	return std::string(kFixtureSecret);
}

std::string MissingSecretLookup(std::string_view /*caller*/)
{
	return {};
}

std::string TooShortSecretLookup(std::string_view /*caller*/)
{
	return std::string(kTooShortSecret);
}

// 纯空白等同未配置:YAML / env 里写成 " " 不该被当成配好了。
std::string BlankSecretLookup(std::string_view /*caller*/)
{
	return "                                              ";
}

// 带首尾空白的密钥。Go NewSigner 是拿 TrimSpace 之后的字节当 HMAC key 的,
// scene 不 trim 就会两边算出不同的签名 —— 这条用例专门钉住这个跨语言细节。
std::string PaddedSecretLookup(std::string_view /*caller*/)
{
	return "  \t" + std::string(kFixtureSecret) + "\n ";
}

// 每个调用方一把不同的密钥。
std::string PerCallerSecretLookup(std::string_view caller)
{
	return (caller == "guild") ? std::string(kFixtureSecret) : std::string(kOtherFixtureSecret);
}

// 记录"被问到了哪个调用方、问了几次"。用来证明:白名单排在取密钥之前,
// 不可信的 caller 永远不会被拿去拼环境变量名。
int g_lookupCalls = 0;
std::string g_lastLookupCaller;

std::string RecordingSecretLookup(std::string_view caller)
{
	++g_lookupCalls;
	g_lastLookupCaller.assign(caller);
	return std::string(kFixtureSecret);
}

void ResetLookupRecorder()
{
	g_lookupCalls = 0;
	g_lastLookupCaller.clear();
}

// ── 请求构造与签名 ──────────────────────────────────────────────────────────

// §4.32 末尾那一组 golden 输入:player 42、stream 1、epoch 1700000000000、seq 7、
// corr 99、tx 24、货币 [(1,30)]、无物品、caller guild、ts 1700000000123。
// Go auth_test.go 的 testRequest 用的是同一组。
::AssetOpRequest MakeGoldenRequest()
{
	::AssetOpRequest request;
	request.set_player_id(42);
	request.set_stream(ASSET_OP_STREAM_GUILD_DEBIT);
	request.set_seq(7);
	request.set_correlation_id(99);
	request.set_tx_type(24);
	request.set_stream_epoch(1700000000000ULL);
	auto* currency = request.mutable_bundle()->add_currencies();
	currency->set_currency_type(1);
	currency->set_amount(30);
	auto* auth = request.mutable_auth();
	auth->set_caller("guild");
	auth->set_timestamp_ms(1700000000123ULL);
	return request;
}

// 签名必须在所有参与 canonical 的字段(含 caller 与 timestamp_ms)填好之后算。
void SignRequest(std::string_view rpc, ::AssetOpRequest& request, std::string_view secret = kFixtureSecret)
{
	request.mutable_auth()->set_signature_hex(
		token_security::HmacSha256Hex(secret, AssetOpCanonical(rpc, request)));
}

// 签好的 golden 请求 + 默认查表 + 零偏差 = 应当通过。多数用例在此基础上改一个字段。
AssetOpAuthVerdict VerifyGolden(const ::AssetOpRequest& request, std::string_view rpc = "debit",
								int64_t nowMs = kFrozenNowMs,
								AssetOpSecretLookup lookup = &FixtureSecretLookup)
{
	return VerifyAssetOpAuth(rpc, request, nowMs, lookup);
}

} // namespace

// ── canonical 串:跨语言契约 ────────────────────────────────────────────────

// 与 Go go/shared/assetop/auth_test.go:22 的 canonicalGolden 逐字节相同。**不要就地"修正"
// 这个字面量**:它对不上说明拼串规则漂了,该改的是代码,或者两边同批改再一起更新 golden。
TEST(AssetOpAuthTest, CanonicalGolden)
{
	const ::AssetOpRequest request = MakeGoldenRequest();
	const std::string expected =
		"mmorpg-asset-op/v1\nguild\ndebit\n42\n1\n1700000000000\n7\n99\n24\nc=1:30;i=;u=;p=0\n1700000000123";

	EXPECT_EQ(expected, AssetOpCanonical("debit", request));
}

// 空 bundle 的写法也是契约的一部分,单独钉住(对应 Go TestCanonicalEmptyBundle)。
TEST(AssetOpAuthTest, CanonicalEmptyBundle)
{
	::AssetOpRequest request;
	request.set_player_id(7);
	request.set_stream(ASSET_OP_STREAM_GUILD_DEBIT);
	request.set_seq(1);
	request.set_stream_epoch(2);
	auto* auth = request.mutable_auth();
	auth->set_caller("guild");
	auth->set_timestamp_ms(5);

	EXPECT_EQ("mmorpg-asset-op/v1\nguild\nabort_debit\n7\n1\n2\n1\n0\n0\nc=;i=;u=;p=0\n5",
			  AssetOpCanonical("abort_debit", request));
}

// 多条货币 / 物品按**请求顺序**拼,不排序(两侧都按同一份请求重算,排序只会多一个出错机会)。
TEST(AssetOpAuthTest, CanonicalKeepsRequestOrder)
{
	::AssetOpRequest request;
	request.set_player_id(1);
	request.set_stream(ASSET_OP_STREAM_TRADE_CREDIT);
	request.set_seq(2);
	request.set_stream_epoch(3);
	auto* bundle = request.mutable_bundle();
	// 故意逆序:货币 9 在 2 之前,物品 300 在 100 之前。
	auto* firstCurrency = bundle->add_currencies();
	firstCurrency->set_currency_type(9);
	firstCurrency->set_amount(5);
	auto* secondCurrency = bundle->add_currencies();
	secondCurrency->set_currency_type(2);
	secondCurrency->set_amount(7);
	auto* firstItem = bundle->add_items();
	firstItem->set_config_id(300);
	firstItem->set_count(2);
	auto* secondItem = bundle->add_items();
	secondItem->set_config_id(100);
	secondItem->set_count(1);
	auto* auth = request.mutable_auth();
	auth->set_caller("trade");
	auth->set_timestamp_ms(11);

	EXPECT_EQ("mmorpg-asset-op/v1\ntrade\ncredit\n1\n4\n3\n2\n0\n0\nc=9:5,2:7;i=300:2,100:1;u=;p=0\n11",
			  AssetOpCanonical("credit", request));
}

// 按 guid 扣的物品与宝宝**必须**进 canonical 串(2026-09-19 起;本用例取代了原先断言
// "这两个字段不改变 canonical" 的 CanonicalIgnoresUnsignedP2Fields)。
// 与 Go TestCanonicalCoversGuidAndPet 同一组输入。
TEST(AssetOpAuthTest, CanonicalCoversGuidAndPet)
{
	::AssetOpRequest request;
	request.set_player_id(7);
	request.set_stream(ASSET_OP_STREAM_TRADE_DEBIT);
	request.set_seq(1);
	request.set_stream_epoch(2);
	request.set_tx_type(24);
	auto* bundle = request.mutable_bundle();
	bundle->add_item_uuids(900000000000000001ULL);
	bundle->add_item_uuids(900000000000000002ULL);
	bundle->set_pet_id(700000000000000003ULL);
	auto* auth = request.mutable_auth();
	auth->set_caller("trade");
	auth->set_timestamp_ms(5);

	EXPECT_EQ("mmorpg-asset-op/v1\ntrade\ndebit\n7\n3\n2\n1\n0\n24\n"
			  "c=;i=;u=900000000000000001,900000000000000002;p=700000000000000003\n5",
			  AssetOpCanonical("debit", request));
}

// 上一条的**意义**所在:改掉 item_uuids / pet_id 之后,原签名必须失效。
//
// 这才是 §4.32 真正要挡的攻击:scene 的 gRPC 是 InsecureServerCredentials(),集群内
// 任意进程可连可嗅;攻击者截下一条合法的 TRADE_DEBIT,只把 item_uuids 换成该玩家的
// 其它装备再发出去。这个 seq scene 没见过,幂等挡不住;时间戳没动,300s 时间窗也挡不住。
// 唯一挡得住的就是签名覆盖到这两个字段。
TEST(AssetOpAuthTest, SignatureCoversGuidAndPetTamper)
{
	::AssetOpRequest signed_request;
	signed_request.set_player_id(7);
	signed_request.set_stream(ASSET_OP_STREAM_TRADE_DEBIT);
	signed_request.set_seq(1);
	signed_request.set_stream_epoch(2);
	signed_request.set_tx_type(24);
	signed_request.mutable_bundle()->add_item_uuids(900000000000000001ULL);
	signed_request.mutable_bundle()->set_pet_id(700000000000000003ULL);
	signed_request.mutable_auth()->set_caller("trade");
	SignRequest("debit", signed_request);
	ASSERT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(signed_request));

	// 攻击者能做的正是这一步:载荷换掉,签名与时间戳原样带上。
	{
		::AssetOpRequest tampered = signed_request;
		tampered.mutable_bundle()->set_item_uuids(0, 900000000000000099ULL);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered))
			<< "换掉 item_uuid 后签名仍然通过 —— u 段没有进 canonical 串";
	}
	{
		::AssetOpRequest tampered = signed_request;
		tampered.mutable_bundle()->add_item_uuids(900000000000000099ULL);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered))
			<< "多加一个 item_uuid 后签名仍然通过";
	}
	{
		::AssetOpRequest tampered = signed_request;
		tampered.mutable_bundle()->clear_item_uuids();
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered))
			<< "清空 item_uuids 后签名仍然通过";
	}
	{
		::AssetOpRequest tampered = signed_request;
		tampered.mutable_bundle()->set_pet_id(700000000000000099ULL);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered))
			<< "换掉 pet_id 后签名仍然通过 —— p 段没有进 canonical 串";
	}
	{
		::AssetOpRequest tampered = signed_request;
		tampered.mutable_bundle()->set_pet_id(0);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered))
			<< "清空 pet_id 后签名仍然通过";
	}
}

// ── 正常通过 ────────────────────────────────────────────────────────────────

TEST(AssetOpAuthTest, VerifyOk)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request));
}

// 四条有主的流各走一遍,确认白名单表与流的对应没写反。
TEST(AssetOpAuthTest, VerifyOkOnEveryOwnedStream)
{
	struct Case
	{
		AssetOpStream stream;
		const char* caller;
		const char* rpc;
	};
	const Case cases[] = {
		{ASSET_OP_STREAM_GUILD_DEBIT, "guild", "debit"},
		{ASSET_OP_STREAM_GUILD_CREDIT, "guild", "credit"},
		{ASSET_OP_STREAM_TRADE_DEBIT, "trade", "debit"},
		{ASSET_OP_STREAM_TRADE_CREDIT, "trade", "credit"},
	};
	for (const auto& testCase : cases)
	{
		::AssetOpRequest request = MakeGoldenRequest();
		request.set_stream(testCase.stream);
		request.mutable_auth()->set_caller(testCase.caller);
		SignRequest(testCase.rpc, request);

		EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request, testCase.rpc))
			<< "stream=" << static_cast<int>(testCase.stream) << " caller=" << testCase.caller;
	}
}

// AssetAbortDebit 收全部有主的流,签名口径与另外两个入口完全一致。
TEST(AssetOpAuthTest, VerifyOkForAbortDebit)
{
	::AssetOpRequest request = MakeGoldenRequest();
	request.clear_bundle();
	SignRequest("abort_debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request, "abort_debit"));
}

// 密钥去空白后再做 HMAC,与 Go NewSigner 的 TrimSpace 同口径。
TEST(AssetOpAuthTest, SecretTrimmedBeforeHmac)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request, kFixtureSecret); // 用**去过空白**的密钥签

	// 查表返回带首尾空白的同一把密钥,仍须通过。
	EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request, "debit", kFrozenNowMs, &PaddedSecretLookup));
}

// ── 白名单 ──────────────────────────────────────────────────────────────────

TEST(AssetOpAuthTest, CallerNotAllowed)
{
	struct Case
	{
		const char* name;
		AssetOpStream stream;
		const char* caller;
	};
	const Case cases[] = {
		{"guild 的调用方去动 trade 的流", ASSET_OP_STREAM_TRADE_DEBIT, "guild"},
		{"trade 的调用方去动 guild 的流", ASSET_OP_STREAM_GUILD_CREDIT, "trade"},
		{"调用方为空", ASSET_OP_STREAM_GUILD_DEBIT, ""},
		{"不认识的调用方", ASSET_OP_STREAM_GUILD_DEBIT, "gm"},
		{"大小写不同即不同", ASSET_OP_STREAM_GUILD_DEBIT, "Guild"},
		{"带空白的调用方名不做宽容匹配", ASSET_OP_STREAM_GUILD_DEBIT, " guild"},
		{"流未指定", ASSET_OP_STREAM_UNSPECIFIED, "guild"},
	};
	for (const auto& testCase : cases)
	{
		::AssetOpRequest request = MakeGoldenRequest();
		request.set_stream(testCase.stream);
		request.mutable_auth()->set_caller(testCase.caller);
		SignRequest("debit", request); // 签名本身是对的,拒绝只因为白名单

		EXPECT_EQ(AssetOpAuthVerdict::kCallerNotAllowed, VerifyGolden(request)) << testCase.name;
	}
}

// SYSTEM_CREDIT(邮件 / GM 发物)在 v1 没有合法调用方,任何签名都过不去。
TEST(AssetOpAuthTest, SystemCreditAlwaysRejected)
{
	for (const char* caller : {"guild", "trade", "system", ""})
	{
		::AssetOpRequest request = MakeGoldenRequest();
		request.set_stream(ASSET_OP_STREAM_SYSTEM_CREDIT);
		request.mutable_auth()->set_caller(caller);
		SignRequest("credit", request);

		EXPECT_EQ(AssetOpAuthVerdict::kCallerNotAllowed, VerifyGolden(request, "credit"))
			<< "caller=" << caller;
	}
}

// 白名单必须排在取密钥之前:caller 来自不可信请求,而默认查表要拿它去拼环境变量名。
// 白名单没过就问密钥,等于让外部输入决定读哪个环境变量。
TEST(AssetOpAuthTest, SecretLookupOnlySeesWhitelistedCaller)
{
	::AssetOpRequest rejected = MakeGoldenRequest();
	rejected.mutable_auth()->set_caller("../../etc/passwd");
	SignRequest("debit", rejected);

	ResetLookupRecorder();
	EXPECT_EQ(AssetOpAuthVerdict::kCallerNotAllowed,
			  VerifyGolden(rejected, "debit", kFrozenNowMs, &RecordingSecretLookup));
	EXPECT_EQ(0, g_lookupCalls) << "白名单未过就不该去查密钥";

	::AssetOpRequest accepted = MakeGoldenRequest();
	SignRequest("debit", accepted);

	ResetLookupRecorder();
	EXPECT_EQ(AssetOpAuthVerdict::kOk,
			  VerifyGolden(accepted, "debit", kFrozenNowMs, &RecordingSecretLookup));
	EXPECT_EQ(1, g_lookupCalls);
	EXPECT_EQ("guild", g_lastLookupCaller);
}

// ── 密钥 ────────────────────────────────────────────────────────────────────

TEST(AssetOpAuthTest, SecretMissing)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kSecretMissing,
			  VerifyGolden(request, "debit", kFrozenNowMs, &MissingSecretLookup));
}

// 31 字节:差一个字节也算未配置,不做 dev 放行(资产路径一律 fail-closed)。
TEST(AssetOpAuthTest, SecretTooShort)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request, kTooShortSecret);

	EXPECT_EQ(AssetOpAuthVerdict::kSecretMissing,
			  VerifyGolden(request, "debit", kFrozenNowMs, &TooShortSecretLookup));
}

TEST(AssetOpAuthTest, SecretBlankCountsAsMissing)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kSecretMissing,
			  VerifyGolden(request, "debit", kFrozenNowMs, &BlankSecretLookup));
}

// 每个调用方一把密钥:拿 guild 的那把去签 trade 的包必须失败(密钥隔离,§4.32)。
TEST(AssetOpAuthTest, PerCallerSecretsAreNotInterchangeable)
{
	::AssetOpRequest request = MakeGoldenRequest();
	request.set_stream(ASSET_OP_STREAM_TRADE_DEBIT);
	request.mutable_auth()->set_caller("trade");
	SignRequest("debit", request, kFixtureSecret); // guild 的那把

	EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch,
			  VerifyGolden(request, "debit", kFrozenNowMs, &PerCallerSecretLookup));

	// 换回 trade 自己的密钥就通过,证明上面失败的原因确实是密钥不同。
	SignRequest("debit", request, kOtherFixtureSecret);
	EXPECT_EQ(AssetOpAuthVerdict::kOk,
			  VerifyGolden(request, "debit", kFrozenNowMs, &PerCallerSecretLookup));
}

// ── 时间窗 ──────────────────────────────────────────────────────────────────

// 边界含在窗口内:|偏差| == 300000 仍通过,300001 才拒。
TEST(AssetOpAuthTest, ClockSkewBoundaryAccepted)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request, "debit", kFrozenNowMs + kAssetOpAuthMaxSkewMs));
	EXPECT_EQ(AssetOpAuthVerdict::kOk, VerifyGolden(request, "debit", kFrozenNowMs - kAssetOpAuthMaxSkewMs));
}

TEST(AssetOpAuthTest, ClockSkew)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	// 包太旧(截获后重放)。
	EXPECT_EQ(AssetOpAuthVerdict::kClockSkew,
			  VerifyGolden(request, "debit", kFrozenNowMs + kAssetOpAuthMaxSkewMs + 1));
	// 包来自"未来"(调用方时钟跑偏)。
	EXPECT_EQ(AssetOpAuthVerdict::kClockSkew,
			  VerifyGolden(request, "debit", kFrozenNowMs - kAssetOpAuthMaxSkewMs - 1));
}

// timestamp_ms 是不可信的 uint64:超过 INT64_MAX 的荒谬值不得因为整数回绕而算出很小的偏差。
TEST(AssetOpAuthTest, ClockSkewRejectsOutOfRangeTimestamp)
{
	::AssetOpRequest request = MakeGoldenRequest();
	request.mutable_auth()->set_timestamp_ms(UINT64_MAX);
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kClockSkew, VerifyGolden(request));
}

// 时间窗排在签名之前:偏差超限时不该再去比签名,也不该因为签名对了就放行。
TEST(AssetOpAuthTest, ClockSkewCheckedEvenWithValidSignature)
{
	::AssetOpRequest request = MakeGoldenRequest();
	request.mutable_auth()->set_timestamp_ms(1);
	SignRequest("debit", request); // 对这个 timestamp 而言签名是正确的

	EXPECT_EQ(AssetOpAuthVerdict::kClockSkew, VerifyGolden(request));
}

// ── 篡改 ────────────────────────────────────────────────────────────────────

// 进 canonical 串的每一个字段都逐个改一次:任一字段动了,签名就必须对不上。
// 这正是"签名保护了什么"的可执行说明 —— 少保护一个字段,这里就会绿得很可疑。
TEST(AssetOpAuthTest, TamperedEnvelopeFields)
{
	const ::AssetOpRequest signed_ = [] {
		::AssetOpRequest request = MakeGoldenRequest();
		SignRequest("debit", request);
		return request;
	}();

	{
		::AssetOpRequest tampered = signed_;
		tampered.set_player_id(43);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "player_id";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.set_seq(8);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "seq";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.set_stream_epoch(1700000000001ULL);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "stream_epoch";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.set_correlation_id(100);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "correlation_id";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.set_tx_type(25);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "tx_type";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.mutable_auth()->set_timestamp_ms(kFrozenNowMs + 1);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "timestamp_ms";
	}
	{
		// stream 改成同一调用方的另一条流:白名单仍然过,必须由签名挡住。
		::AssetOpRequest tampered = signed_;
		tampered.set_stream(ASSET_OP_STREAM_GUILD_CREDIT);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "stream";
	}
}

// bundle 进串的目的就是"防止中途改金额"(§4.32),逐项钉住。
TEST(AssetOpAuthTest, TamperedBundle)
{
	const ::AssetOpRequest signed_ = [] {
		::AssetOpRequest request = MakeGoldenRequest();
		SignRequest("debit", request);
		return request;
	}();

	{
		::AssetOpRequest tampered = signed_;
		tampered.mutable_bundle()->mutable_currencies(0)->set_amount(3000);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "金额";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.mutable_bundle()->mutable_currencies(0)->set_currency_type(2);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "币种";
	}
	{
		::AssetOpRequest tampered = signed_;
		auto* extra = tampered.mutable_bundle()->add_currencies();
		extra->set_currency_type(2);
		extra->set_amount(1);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "追加一条货币";
	}
	{
		::AssetOpRequest tampered = signed_;
		auto* item = tampered.mutable_bundle()->add_items();
		item->set_config_id(100);
		item->set_count(1);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "追加一件物品";
	}
	{
		::AssetOpRequest tampered = signed_;
		tampered.clear_bundle();
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "清空 bundle";
	}
}

// rpc 进串,防止拿 AbortDebit 的签名去调 Credit(§4.32)。
TEST(AssetOpAuthTest, TamperedRpc)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);

	EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(request, "credit"));
	EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(request, "abort_debit"));
}

// 签名字段本身畸形:空、截断、大小写变形,都必须落在拒绝那一侧
// (ConstantTimeEquals 长度不等直接拒,两边都空也拒)。
TEST(AssetOpAuthTest, MalformedSignature)
{
	::AssetOpRequest request = MakeGoldenRequest();
	SignRequest("debit", request);
	const std::string good = request.auth().signature_hex();
	ASSERT_EQ(64u, good.size()) << "HMAC-SHA256 的十六进制应当是 64 个字符";

	{
		::AssetOpRequest tampered = request;
		tampered.mutable_auth()->clear_signature_hex();
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "空签名";
	}
	{
		::AssetOpRequest tampered = request;
		tampered.mutable_auth()->set_signature_hex(good.substr(0, good.size() - 1));
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "截断";
	}
	{
		// 契约写的是 lowercase-hex;大写不是"等价写法",一律拒。
		::AssetOpRequest tampered = request;
		std::string upper = good;
		for (char& ch : upper)
		{
			if (ch >= 'a' && ch <= 'f')
			{
				ch = static_cast<char>(ch - 'a' + 'A');
			}
		}
		tampered.mutable_auth()->set_signature_hex(upper);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "大写 hex";
	}
	{
		::AssetOpRequest tampered = request;
		std::string flipped = good;
		flipped[0] = (flipped[0] == '0') ? '1' : '0';
		tampered.mutable_auth()->set_signature_hex(flipped);
		EXPECT_EQ(AssetOpAuthVerdict::kSignatureMismatch, VerifyGolden(tampered)) << "改一个字符";
	}
}

// 整个 auth 字段缺失(未签名的裸包):caller 为空 → 白名单先拒。
TEST(AssetOpAuthTest, MissingAuthRejected)
{
	::AssetOpRequest request = MakeGoldenRequest();
	request.clear_auth();

	EXPECT_EQ(AssetOpAuthVerdict::kCallerNotAllowed, VerifyGolden(request));
}

// ── 判决名 ──────────────────────────────────────────────────────────────────

// 名字只进日志,但它是运维定位"为什么全拒"的唯一线索,不能错位、不能越界崩。
TEST(AssetOpAuthTest, VerdictNames)
{
	EXPECT_STREQ("ok", AssetOpAuthVerdictName(AssetOpAuthVerdict::kOk));
	EXPECT_STREQ("caller_not_allowed", AssetOpAuthVerdictName(AssetOpAuthVerdict::kCallerNotAllowed));
	EXPECT_STREQ("secret_missing", AssetOpAuthVerdictName(AssetOpAuthVerdict::kSecretMissing));
	EXPECT_STREQ("clock_skew", AssetOpAuthVerdictName(AssetOpAuthVerdict::kClockSkew));
	EXPECT_STREQ("signature_mismatch", AssetOpAuthVerdictName(AssetOpAuthVerdict::kSignatureMismatch));
	EXPECT_STREQ("unknown", AssetOpAuthVerdictName(AssetOpAuthVerdict::kCount));
	EXPECT_STREQ("unknown", AssetOpAuthVerdictName(static_cast<AssetOpAuthVerdict>(200)));
}

// 两个跨语言常量与 Go 侧同值(Go: assetop.MaxClockSkewMs / MinSecretLen)。
// 改单边会让"本地测试全绿、线上全拒"这种最难查的故障成为可能。
TEST(AssetOpAuthTest, CrossLanguageConstants)
{
	EXPECT_EQ(300000, kAssetOpAuthMaxSkewMs);
	EXPECT_EQ(32u, kAssetOpAuthMinSecretBytes);
	EXPECT_EQ(std::string_view("mmorpg-asset-op/v1"), kAssetOpCanonicalVersion);
}
