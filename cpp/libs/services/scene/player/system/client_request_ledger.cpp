#include "client_request_ledger.h"

#include <unordered_set>
#include <utility>

#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>
#include <google/protobuf/message_lite.h>
#include <muduo/base/Logging.h>
#include <openssl/sha.h>

#include "thread_context/ecs_context.h"

static_assert(kClientRequestFingerprintBytes <= SHA256_DIGEST_LENGTH, "the fingerprint is a prefix of a SHA-256 digest");

std::string ClientRequestFingerprintDigest(const google::protobuf::MessageLite &requestWithoutKey)
{
	std::string bytes;
	bool serialized = false;
	{
		google::protobuf::io::StringOutputStream stream(&bytes);
		google::protobuf::io::CodedOutputStream coded(&stream);
		// 确定性序列化(map 字段按键排序):同一份请求两次序列化逐字节相同,重发才认得出是同一个请求。
		coded.SetSerializationDeterministic(true);
		serialized = requestWithoutKey.SerializeToCodedStream(&coded) && !coded.HadError();
	} // CodedOutputStream 析构时才把缓冲刷进 bytes,所以要等这个块结束
	if (!serialized)
	{
		// proto3 没有必填字段、写内存不会失败,正常走不到。真走到了,指纹可能不完整,只打日志不拦请求:
		// 后果最多是把两个不同的请求当成同一个,而这要求它们还用了同一个键。
		// 包一层 std::string:新版 protobuf 的 GetTypeName 返回 string_view 一类,muduo 的 LogStream 没有对应重载(同 codec.cpp)。
		LOG_ERROR << "[ClientRequest] fingerprint serialization failed: type=" << std::string(requestWithoutKey.GetTypeName());
	}
	unsigned char digest[SHA256_DIGEST_LENGTH];
	SHA256(reinterpret_cast<const unsigned char *>(bytes.data()), bytes.size(), digest);
	return std::string(reinterpret_cast<const char *>(digest), kClientRequestFingerprintBytes);
}

namespace client_request_ledger
{
namespace
{
// 调用方保证 key != 0。
const ClientRequestRecord *FindRecord(const ClientRequestLedgerComp &ledger, uint64_t key)
{
	for (const auto &record : ledger.records())
	{
		if (record.key() == key)
		{
			return &record;
		}
	}
	return nullptr;
}

// 旧存档没有 next_seq(为 0):按 1 处理,第一条记录的 seq 就是 1。
uint64_t EffectiveNextSeq(const ClientRequestLedgerComp &ledger)
{
	return ledger.next_seq() == 0 ? 1 : ledger.next_seq();
}

// 淘汰 seq 最小的那条。records 的下标不承载顺序,所以拿末尾那条填坑,不用整体挪动。调用方保证非空。
void EvictOldest(ClientRequestLedgerComp &ledger)
{
	auto &records = *ledger.mutable_records();
	int oldest = 0;
	for (int i = 1; i < records.size(); ++i)
	{
		if (records.Get(i).seq() < records.Get(oldest).seq())
		{
			oldest = i;
		}
	}
	records.SwapElements(oldest, records.size() - 1);
	records.RemoveLast();
}
} // namespace

ClientRequestVerdict Check(const ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind,
						   std::string_view fingerprint, ClientRequestResult &result)
{
	if (key == 0)
	{
		return ClientRequestVerdict::kNew;
	}
	const ClientRequestRecord *record = FindRecord(ledger, key);
	if (record == nullptr)
	{
		return ClientRequestVerdict::kNew;
	}
	if (record->kind() != static_cast<uint32_t>(kind) || record->fingerprint() != fingerprint)
	{
		return ClientRequestVerdict::kMismatch;
	}
	result.enhanceSuccess = record->enhance_success();
	result.enhanceLevel = record->enhance_level();
	result.createdSchemeId = record->created_scheme_id();
	return ClientRequestVerdict::kReplay;
}

bool Remember(ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result)
{
	if (key == 0 || FindRecord(ledger, key) != nullptr)
	{
		return false;
	}
	// 用 while:加载进来的旧存档可能多于 kCapacity 条(容量下调过),一次淘汰到位。
	while (ledger.records_size() >= kCapacity)
	{
		EvictOldest(ledger);
	}
	const uint64_t seq = EffectiveNextSeq(ledger);
	auto *record = ledger.add_records();
	record->set_key(key);
	record->set_kind(static_cast<uint32_t>(kind));
	record->set_fingerprint(std::move(fingerprint));
	record->set_seq(seq);
	record->set_enhance_success(result.enhanceSuccess);
	record->set_enhance_level(result.enhanceLevel);
	record->set_created_scheme_id(result.createdSchemeId);
	ledger.set_next_seq(seq + 1);
	return true;
}

std::string Validate(const ClientRequestLedgerComp &ledger)
{
	if (ledger.records_size() > kMaxRecordsOnLoad)
	{
		return "too many records: " + std::to_string(ledger.records_size());
	}
	const uint64_t nextSeq = EffectiveNextSeq(ledger);
	std::unordered_set<uint64_t> keys;
	std::unordered_set<uint64_t> seqs;
	for (const auto &record : ledger.records())
	{
		if (record.key() == 0)
		{
			return "a record has key 0";
		}
		if (!keys.insert(record.key()).second)
		{
			return "duplicate key " + std::to_string(record.key());
		}
		if (record.fingerprint().size() != kClientRequestFingerprintBytes)
		{
			return "fingerprint of key " + std::to_string(record.key()) + " is " +
				   std::to_string(record.fingerprint().size()) + " bytes";
		}
		if (record.seq() == 0 || record.seq() >= nextSeq)
		{
			return "seq " + std::to_string(record.seq()) + " of key " + std::to_string(record.key()) +
				   " is outside [1, next_seq=" + std::to_string(nextSeq) + ")";
		}
		if (!seqs.insert(record.seq()).second)
		{
			return "duplicate seq " + std::to_string(record.seq());
		}
	}
	return {};
}
} // namespace client_request_ledger

namespace client_request_dedup
{
ClientRequestVerdict Check(entt::entity player, uint64_t key, ClientRequestKind kind, std::string_view fingerprint,
						   ClientRequestResult &result)
{
	if (key == 0)
	{
		return ClientRequestVerdict::kNew;
	}
	const auto &registry = tlsEcs.actorRegistry;
	if (registry.any_of<ClientRequestLedgerInvalidComp>(player))
	{
		LOG_WARN << "[ClientRequest] metric=client_request_unavailable reason=ledger_invalid kind="
				 << ClientRequestKindName(kind) << " player_id=" << tlsEcs.GetPlayerId(player)
				 << " client_request_id=" << key << " (keyed request refused without executing)";
		return ClientRequestVerdict::kUnavailable;
	}
	// 只读:try_get,不创建组件。持久数据只由加载创建,缺了 = 加载不变量被破坏,不现造一本空账本来放行。
	const auto *ledger = registry.try_get<ClientRequestLedgerComp>(player);
	if (ledger == nullptr)
	{
		LOG_ERROR << "[ClientRequest] metric=client_request_unavailable reason=ledger_missing kind="
				  << ClientRequestKindName(kind) << " player_id=" << tlsEcs.GetPlayerId(player)
				  << " client_request_id=" << key << " (load invariant broken; keyed request refused without executing)";
		return ClientRequestVerdict::kUnavailable;
	}
	const auto verdict = client_request_ledger::Check(*ledger, key, kind, fingerprint, result);
	if (verdict == ClientRequestVerdict::kReplay)
	{
		LOG_INFO << "[ClientRequest] metric=client_request_replay kind=" << ClientRequestKindName(kind)
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
	}
	else if (verdict == ClientRequestVerdict::kMismatch)
	{
		LOG_WARN << "[ClientRequest] metric=client_request_mismatch kind=" << ClientRequestKindName(kind)
				 << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key
				 << " (key was used for a different operation; refused without executing)";
	}
	return verdict;
}

void Remember(entt::entity player, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result)
{
	if (key == 0)
	{
		return;
	}
	auto &registry = tlsEcs.actorRegistry;
	auto *ledger = registry.try_get<ClientRequestLedgerComp>(player);
	if (ledger == nullptr || registry.any_of<ClientRequestLedgerInvalidComp>(player))
	{
		// 接入点第 2 步已经按 kUnavailable 返回,正常走不到:走到了说明那个接入点的顺序写错了。
		LOG_ERROR << "[ClientRequest] Remember skipped, ledger missing or invalid: kind=" << ClientRequestKindName(kind)
				  << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
		return;
	}
	if (!client_request_ledger::Remember(*ledger, key, kind, std::move(fingerprint), result))
	{
		// 只有接入点没先 Check 就执行才会走到这里:说明那个接入点的顺序写错了。
		LOG_ERROR << "[ClientRequest] Remember refused, key is already in the ledger: kind=" << ClientRequestKindName(kind)
				  << " player_id=" << tlsEcs.GetPlayerId(player) << " client_request_id=" << key;
	}
}
} // namespace client_request_dedup
