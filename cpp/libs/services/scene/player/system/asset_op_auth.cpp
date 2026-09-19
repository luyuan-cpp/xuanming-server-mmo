#include "player/system/asset_op_auth.h"

#include <cstdlib>
#include <iterator>
#include <limits>
#include <string>

#include "muduo/base/Logging.h"

#include "security/token_security.h"

namespace
{
// ── 判决名表 ────────────────────────────────────────────────────────────────

constexpr const char* kAssetOpAuthVerdictNames[] = {
	"ok", "caller_not_allowed", "secret_missing", "clock_skew", "signature_mismatch",
};
static_assert(std::size(kAssetOpAuthVerdictNames) == static_cast<size_t>(AssetOpAuthVerdict::kCount),
			  "名字表必须和 AssetOpAuthVerdict 逐项对齐");

// ── 调用方白名单(§4.32)────────────────────────────────────────────────────
//
// 每个调用方一把密钥、独占自己的那两条流。分开而不是共用一把:TRADE 的密钥泄了也动不了
// 帮会的流。形状照抄同目录 asset_op_system.cpp 的 AssetOpStreamRule(平铺数组 + 计数),
// 保持两处一眼可比。
//
// **只列允许项,其余一律拒**,这是安全的那一侧:将来 proto 追加第 6 条流,在这张表补一行
// 之前它一个字节也进不来,而不是默默放行。注意这与账本 IsAssetOpStreamValid 的取舍方向
// 相反 —— 那里"拒"会把持有新流账本的玩家永久 fail-closed,这里"拒"只是让没登记的流发不了包。
//
// SYSTEM_CREDIT(邮件 / GM 发物,D1 预留)在 v1 没有合法调用方,故意不出现在表里。
struct AssetOpCallerRule
{
	const char* caller;       // canonical 串第 2 行;也是本表的查找键
	const char* secretEnv;    // scene 读取的环境变量**名**;密钥值不进仓库、不进日志
	AssetOpStream streams[2]; // 该调用方独占的流
	uint8_t streamCount;
};

constexpr AssetOpCallerRule kAssetOpCallerRules[] = {
	{"guild",
	 "MMORPG_ASSET_OP_SECRET_GUILD",
	 {ASSET_OP_STREAM_GUILD_DEBIT, ASSET_OP_STREAM_GUILD_CREDIT},
	 2},
	{"trade",
	 "MMORPG_ASSET_OP_SECRET_TRADE",
	 {ASSET_OP_STREAM_TRADE_DEBIT, ASSET_OP_STREAM_TRADE_CREDIT},
	 2},
};

constexpr size_t kAssetOpCallerRuleCount = std::size(kAssetOpCallerRules);

// 找出"这条流唯一允许的调用方";nullptr = 该流在 v1 没有合法调用方,或流本身不认识。
const AssetOpCallerRule* FindCallerRuleForStream(AssetOpStream stream)
{
	if (stream == ASSET_OP_STREAM_UNSPECIFIED)
	{
		return nullptr;
	}
	for (const auto& rule : kAssetOpCallerRules)
	{
		for (uint8_t index = 0; index < rule.streamCount; ++index)
		{
			if (rule.streams[index] == stream)
			{
				return &rule;
			}
		}
	}
	return nullptr;
}

// ── 默认密钥来源:环境变量 + 缓存 ───────────────────────────────────────────
//
// 运行模式与密钥在进程生命周期内不会变,而这个值每条资产 RPC 都要读一次,不能每次都 getenv。
// 线程:只在 scene 的 loop 线程上被调用(三个入口都已 runInLoop 投递),故不加锁。
struct AssetOpSecretCacheEntry
{
	bool loaded{false};
	std::string secret; // 原样缓存;去空白与长度判定统一在 VerifyAssetOpAuth 里做
};

AssetOpSecretCacheEntry g_secretCache[kAssetOpCallerRuleCount];

// "该调用方密钥未配置"只报一次:它是启动期配置错误,每条请求刷一行 ERROR 只会把日志淹掉。
bool g_secretMissingReported[kAssetOpCallerRuleCount] = {};

std::string DefaultSecretLookup(std::string_view caller)
{
	for (size_t index = 0; index < kAssetOpCallerRuleCount; ++index)
	{
		if (caller != kAssetOpCallerRules[index].caller)
		{
			continue;
		}
		auto& entry = g_secretCache[index];
		if (!entry.loaded)
		{
			const char* raw = std::getenv(kAssetOpCallerRules[index].secretEnv);
			entry.secret = (raw != nullptr) ? raw : "";
			entry.loaded = true;
		}
		return entry.secret;
	}
	// 调用点已先过白名单,正常走不到这里;真走到就是本文件两处表脱节。
	// 返回空串 = 按"未配置"拒绝,仍是 fail-closed。
	return {};
}

// ── canonical 串拼装 ────────────────────────────────────────────────────────

// 逐字节对齐 Go writeBundleCanonical:"c=" 货币按**请求顺序** "<type>:<amount>" 逗号分隔,
// ";i=" 物品同理;两边都空则是 "c=;i="。
// 宽度一律先提到 uint64 再转十进制,和 Go 的 strconv.FormatUint(uint64(x), 10) 同口径。
void AppendBundleCanonical(std::string& out, const ::AssetBundle& bundle)
{
	out.append("c=");
	for (int index = 0; index < bundle.currencies_size(); ++index)
	{
		if (index > 0)
		{
			out.push_back(',');
		}
		const auto& currency = bundle.currencies(index);
		out.append(std::to_string(static_cast<uint64_t>(currency.currency_type())));
		out.push_back(':');
		out.append(std::to_string(currency.amount()));
	}
	out.append(";i=");
	for (int index = 0; index < bundle.items_size(); ++index)
	{
		if (index > 0)
		{
			out.push_back(',');
		}
		const auto& item = bundle.items(index);
		out.append(std::to_string(static_cast<uint64_t>(item.config_id())));
		out.push_back(':');
		out.append(std::to_string(static_cast<uint64_t>(item.count())));
	}
}

// ── 时间窗 ──────────────────────────────────────────────────────────────────

// |nowMs − timestampMs| <= kAssetOpAuthMaxSkewMs。
//
// timestamp_ms 是**不可信的 uint64**:直接 static_cast 成 int64 在 > INT64_MAX 时是实现定义
// 行为,"2^64-1 毫秒"这种荒谬值反而可能算出个很小的偏差而被放行。先在无符号域里挡掉。
// nowMs < 0(墙钟早于 1970)同样荒谬,一并拒 —— 它会让下面的减法有溢出的可能。
bool IsWithinClockSkew(uint64_t timestampMs, int64_t nowMs)
{
	constexpr uint64_t kInt64Max = static_cast<uint64_t>((std::numeric_limits<int64_t>::max)());
	if (nowMs < 0 || timestampMs > kInt64Max)
	{
		return false;
	}
	const auto signedTimestampMs = static_cast<int64_t>(timestampMs);
	const int64_t skewMs =
		(nowMs >= signedTimestampMs) ? (nowMs - signedTimestampMs) : (signedTimestampMs - nowMs);
	return skewMs <= kAssetOpAuthMaxSkewMs;
}

} // namespace

const char* AssetOpAuthVerdictName(AssetOpAuthVerdict verdict)
{
	const auto index = static_cast<size_t>(verdict);
	return (index < std::size(kAssetOpAuthVerdictNames)) ? kAssetOpAuthVerdictNames[index] : "unknown";
}

std::string AssetOpCanonical(std::string_view rpc, const ::AssetOpRequest& request)
{
	const auto& auth = request.auth();

	std::string canonical;
	canonical.reserve(160);
	canonical.append(kAssetOpCanonicalVersion);
	canonical.push_back('\n');
	canonical.append(auth.caller());
	canonical.push_back('\n');
	canonical.append(rpc);
	canonical.push_back('\n');
	canonical.append(std::to_string(request.player_id()));
	canonical.push_back('\n');
	// 流按**有符号**十进制写,与 Go 的 strconv.FormatInt(int64(req.GetStream()), 10) 同口径:
	// proto3 的开放枚举允许携带未知的负值,两边必须写出同样的字面量才谈得上逐字节一致。
	canonical.append(std::to_string(static_cast<int64_t>(request.stream())));
	canonical.push_back('\n');
	canonical.append(std::to_string(request.stream_epoch()));
	canonical.push_back('\n');
	canonical.append(std::to_string(request.seq()));
	canonical.push_back('\n');
	canonical.append(std::to_string(request.correlation_id()));
	canonical.push_back('\n');
	canonical.append(std::to_string(static_cast<uint64_t>(request.tx_type())));
	canonical.push_back('\n');
	AppendBundleCanonical(canonical, request.bundle());
	canonical.push_back('\n');
	canonical.append(std::to_string(auth.timestamp_ms()));
	return canonical;
}

AssetOpAuthVerdict VerifyAssetOpAuth(std::string_view rpc, const ::AssetOpRequest& request, int64_t nowMs,
									 AssetOpSecretLookup lookup)
{
	const auto& auth = request.auth();

	// 1. 流 ↔ 调用方白名单。
	//
	// **必须排在取密钥之前**:caller 来自不可信请求,而默认密钥来源要拿它去拼环境变量名。
	// 先过白名单,再往下走的 caller 就只可能是本文件常量表里的那两个字面量之一。
	// 比较用精确相等(不 trim、不忽略大小写):Go Signer 写进 auth.caller 的已经是去过空白的
	// 名字,这里放宽只会让 canonical 串与白名单键不再是同一个东西。
	const AssetOpCallerRule* rule = FindCallerRuleForStream(request.stream());
	if (rule == nullptr || auth.caller() != rule->caller)
	{
		return AssetOpAuthVerdict::kCallerNotAllowed;
	}
	const size_t ruleIndex = static_cast<size_t>(rule - kAssetOpCallerRules);

	// 2. 密钥。去首尾空白后判长度 —— 与 Go NewSigner 的 strings.TrimSpace 同口径:
	// 对侧是拿**去过空白**的字节当 HMAC key 的,这里不 trim 就会两边算出不同的签名。
	// (Go 按 Unicode 空白 trim,这里按 ASCII;密钥来自环境变量与 K8s Secret,都是 ASCII。)
	const AssetOpSecretLookup resolve = (lookup != nullptr) ? lookup : &DefaultSecretLookup;
	const std::string secret = token_security::TrimAscii(resolve(rule->caller));
	if (secret.size() < kAssetOpAuthMinSecretBytes)
	{
		if (!g_secretMissingReported[ruleIndex])
		{
			g_secretMissingReported[ruleIndex] = true;
			// 只写变量**名**与长度下限,绝不写密钥值、也不写实际长度(AGENTS §11.3)。
			LOG_ERROR << "[AssetOpAuth] 调用方密钥未配置或去空白后不足 " << kAssetOpAuthMinSecretBytes
					  << " 字节,该调用方的资产 RPC 将全部被拒 caller=" << rule->caller
					  << " env=" << rule->secretEnv;
		}
		return AssetOpAuthVerdict::kSecretMissing;
	}

	// 3. 时间窗(§4.32:没有 nonce,时间窗是截获包寿命的唯一上限)。
	if (!IsWithinClockSkew(auth.timestamp_ms(), nowMs))
	{
		return AssetOpAuthVerdict::kClockSkew;
	}

	// 4. 签名。HmacSha256Hex 出错时返回空串,ConstantTimeEquals 对空串恒为 false,
	// 于是 OpenSSL 失败也落在拒绝的那一侧。
	const std::string expected = token_security::HmacSha256Hex(secret, AssetOpCanonical(rpc, request));
	if (!token_security::ConstantTimeEquals(expected, auth.signature_hex()))
	{
		return AssetOpAuthVerdict::kSignatureMismatch;
	}
	return AssetOpAuthVerdict::kOk;
}
