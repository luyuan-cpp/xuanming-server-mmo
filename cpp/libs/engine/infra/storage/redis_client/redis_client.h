#pragma once

#include <algorithm>
#include <chrono>
#include <string>
#include <memory>
#include <unordered_map>
#include <vector>
#include <limits>

#include "muduo/base/Logging.h"
#include "muduo/contrib/hiredis/Hiredis.h"

#include "deps/hiredis/hiredis.h"

#include "engine/core/type_define/type_define.h"

static constexpr const char* kSaveAndMarkLuaScript = R"(
    redis.call('SET', KEYS[1], ARGV[1])
    redis.call('SADD', 'dirty_keys_set', KEYS[1])
    return 1
)";


namespace google
{
    namespace protobuf
    {
        class Message;
    }//namespace protobuf
}// namespace google


using MessageCachedArray = std::vector<uint8_t>;

// 仅供单测在不连接真实 Redis 的情况下驱动异步回包；生产代码不定义或使用。
template <class MessageKey, class MessageValue>
struct MessageAsyncClientTestPeer;

class SyncRedisContext_Deleter
{
public:
    void operator()(redisContext* res);
};

class MessageSyncRedisClient
{
public:
    using ContextPtr = std::unique_ptr<redisContext, SyncRedisContext_Deleter>;
    void Connect(const std::string& redis_server_addr, int32_t port, int32_t sec, int32_t usec);

    void Save(const google::protobuf::Message& message);
    void Save(const google::protobuf::Message& message, Guid guid);
    void Save(const google::protobuf::Message& message, const std::string& key);

    void Load(google::protobuf::Message& message);
    void Load(google::protobuf::Message& message, Guid guid);
    void Load(google::protobuf::Message& message, const std::string& key);
private:
    void OnDisconnect();

    ContextPtr context_;
};

using PbSyncRedisClientPtr = std::shared_ptr<MessageSyncRedisClient>;

template <class MessageKey, class MessageValue>
class MessageAsyncClient
{
public:
	using MessageValuePtr = std::shared_ptr<MessageValue>;

	// Max NIL retries. Combined with exponential backoff (2,4,8,16,32,60s) this
	// gives ~2 minutes for the DB write-back path (Kafka -> db service -> Redis)
	// to land the row before we permanently fail the load.
	static constexpr int kMaxLoadRetries = 6;

	struct Element
	{
		MessageKey message_key;
		std::string redis_key;
		MessageValuePtr message_value;
		// Pre-serialized payload (Save path only). Populated by Save() so the
		// retry path doesn't have to re-serialize the protobuf message.
		std::vector<uint8_t> serialized_payload;
		int retry_count = 0;
		// Save retries continue after the alert threshold. This flag ensures the
		// owner receives one high-severity notification per pending value instead
		// of one notification every capped-backoff tick.
		bool save_failure_notified = false;
		// Earliest time this element is allowed to be retried (steady_clock).
		// Set when the element is enqueued into pending_retry_queue_ /
		// pending_save_queue_.
		std::chrono::steady_clock::time_point next_retry_at{};
	};

	using ElementPtr = std::shared_ptr<Element>;
	using LoadingQueue = std::unordered_map<std::string, ElementPtr>;
	using EventCallback = std::function<void(MessageKey, MessageValue&)>;

	// Why a load callback failed. Lets the application distinguish a brand-new
	// player (DataNotFound: NIL after exhausting retries -- nothing in Redis nor
	// MySQL within the backoff window) from a real Redis fault (RedisError:
	// connection lost, parse failure, or unexpected reply type).
	enum class LoadFailureReason
	{
		DataNotFound,
		RedisError,
	};
	using FailedCallback = std::function<void(MessageKey, LoadFailureReason)>;

	// 存盘连续失败达到告警阈值的通知。通知后仍保留最新值并继续重试。
	//
	// 为什么必须有:异步存盘的调用方普遍依赖"成功回调迟早会来"来收尾
	// (销毁实体、清 session、解锁)。旧实现在重试耗尽时只打一条 LOG_ERROR
	// 就 return,没有任何人被通知 —— 退出流程于是永久悬挂,而且数据只留在
	// 内存里,盘上还是上一次成功存盘的旧值。加载路径早就有
	// load_failed_callback_,存盘路径缺这一半是接口级的不对称。
	using SaveFailedCallback = std::function<void(MessageKey, const std::string& redisKey, int retryCount)>;

	using HiredisPtr = std::unique_ptr<hiredis::Hiredis>;

	explicit MessageAsyncClient(HiredisPtr& hiredis)
		: hiredis_(hiredis)
	{
	}

	inline const std::string full_name() const { return std::string(MessageValue::GetDescriptor()->full_name()); }

	void SetSaveCallback(const EventCallback& cb) { save_callback_ = cb; }
	void SetLoadCallback(const EventCallback& cb) { load_callback_ = cb; }
	void SetLoadFailedCallback(const FailedCallback& cb) { load_failed_callback_ = cb; }
	void SetSaveFailedCallback(const SaveFailedCallback& cb) { save_failed_callback_ = cb; }


	void Save(const MessageValuePtr& message, const MessageKey& key)
	{
		ElementPtr element = std::make_shared<Element>();
		element->message_key = key;
		element->redis_key = full_name() + ":" + std::to_string(key);
		element->message_value = message;

		// Serialize once; retry path re-uses this buffer.
		const size_t size = message->ByteSizeLong();
		element->serialized_payload.resize(size);
		if (size > 0 && !message->SerializeToArray(element->serialized_payload.data(), static_cast<int>(size)))
		{
			LOG_ERROR << "SerializeToArray failed for key " << key;
			return;
		}

		// A key may have only one write in flight. If a periodic save is still
		// waiting for its reply when logout produces a newer full snapshot, keep
		// only that newer value and do not let the older completion finish logout.
		// This also prevents an old failed retry from overwriting a later success.
		if (saving_queue_.find(element->redis_key) != saving_queue_.end())
		{
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		if (!hiredis_ || !hiredis_->connected())
		{
			LOG_WARN << "Redis not connected, queueing Save for retry: " << element->redis_key;
			// next_retry_at default-constructed (epoch); periodic timer will pick it up
			// as soon as Redis reconnects.
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		IssueSave(element);
	}

	void AsyncLoad(const MessageKey& key, int retry_count = 0)
	{
		std::string redis_key = full_name() + ":" + std::to_string(key);

		LOG_DEBUG << "AsyncLoad: key=" << redis_key
				  << " connected=" << (hiredis_ ? hiredis_->connected() : false)
				  << " retry=" << retry_count
				  << " loading_queue=" << loading_queue_.size()
				  << " pending_retry=" << pending_retry_queue_.size();

		if (!hiredis_ || !hiredis_->connected())
		{
			LOG_WARN << "Redis not connected, queueing AsyncLoad for retry: " << redis_key;
			auto element = std::make_shared<Element>();
			element->message_key = key;
			element->redis_key = redis_key;
			element->retry_count = retry_count;
			// next_retry_at default-constructed (epoch); periodic timer will pick it up
			// as soon as Redis reconnects.
			pending_retry_queue_[redis_key] = element;
			return;
		}

		// Already in-flight
		if (loading_queue_.find(redis_key) != loading_queue_.end())
		{
			return;
		}

		// If the same key is already waiting in pending_retry_queue_, preserve its
		// retry_count so an external re-trigger does not reset NIL backoff.
		if (auto pendingIt = pending_retry_queue_.find(redis_key); pendingIt != pending_retry_queue_.end())
		{
			if (pendingIt->second->retry_count > retry_count)
			{
				retry_count = pendingIt->second->retry_count;
			}
			pending_retry_queue_.erase(pendingIt);
		}

		ElementPtr element = std::make_shared<Element>();
		element->message_key = key;
		element->redis_key = redis_key;
		element->retry_count = retry_count;

		loading_queue_.emplace(redis_key, element);

		const std::string format = "GET " + redis_key;
		int ret = hiredis_->command(std::bind(&MessageAsyncClient::OnLoaded, this, std::placeholders::_1, std::placeholders::_2, element),
			format.c_str());
		if (ret != REDIS_OK)
		{
			LOG_ERROR << "Redis command failed (ret=" << ret << ") for key: " << redis_key
					  << ", connected=" << hiredis_->connected();
			loading_queue_.erase(redis_key);
			pending_retry_queue_[redis_key] = element;
		}
	}

	// Call ONLY from the Redis reconnect callback. Migrates in-flight loads
	// (whose callbacks will never fire post-disconnect) into the retry queue,
	// re-issues every pending load immediately, and re-flushes pending saves.
	void OnReconnected()
	{
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}

		// A disconnected async command will not reliably deliver its callback.
		// Move each in-flight save back to pending, unless Save() already placed a
		// newer full snapshot for the same key there. Full snapshots are
		// superseding, so retaining the newest pending value is sufficient.
		for (auto &[k, v] : saving_queue_)
		{
			if (pending_save_queue_.find(k) == pending_save_queue_.end())
			{
				v->next_retry_at = std::chrono::steady_clock::now();
				pending_save_queue_[k] = v;
			}
		}
		saving_queue_.clear();

		// Cached EVALSHA hash is bound to the previous server connection;
		// drop it so we re-SCRIPT LOAD on the new connection. A SCRIPT LOAD that
		// was in flight on the dead connection will never clear this flag.
		script_sha1_.clear();
		script_load_in_flight_ = false;
		EnsureScriptLoaded();

		const auto now = std::chrono::steady_clock::now();
		for (auto &[k, v] : loading_queue_)
		{
			v->next_retry_at = now;
			pending_retry_queue_[k] = v;
		}
		loading_queue_.clear();

		const size_t pendingLoads = pending_retry_queue_.size();
		const size_t pendingSaves = pending_save_queue_.size();
		if (pendingLoads + pendingSaves == 0)
		{
			return;
		}

		LOG_INFO << "OnReconnected: re-issuing " << pendingLoads << " loads, "
				 << pendingSaves << " saves";
		FlushDuePending(now, /*force=*/true);
	}

	// Periodic timer entry point. ONLY drains entries in pending_retry_queue_
	// and pending_save_queue_ whose backoff has elapsed. MUST NOT touch
	// loading_queue_, otherwise in-flight GETs get re-issued and load_callback_
	// fires twice for the same key.
	void RetryDuePending()
	{
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}
		if (pending_retry_queue_.empty() && pending_save_queue_.empty())
		{
			return;
		}
		FlushDuePending(std::chrono::steady_clock::now(), /*force=*/false);
	}

	// Backwards-compatible alias.
	void RetryDuePendingLoads() { RetryDuePending(); }

	MessageValuePtr CreateMessage() { return std::make_shared<MessageValue>(); }

	// --- Observability ---
	// Number of GETs whose reply has not yet been received.
	size_t in_flight_load_count() const { return loading_queue_.size(); }
	// Number of loads waiting in the NIL/disconnect retry queue.
	size_t pending_load_count() const { return pending_retry_queue_.size(); }
	// Number of saves waiting in the failure/disconnect retry queue.
	size_t pending_save_count() const { return pending_save_queue_.size(); }
	// Number of SETs whose reply has not yet been received.
	size_t in_flight_save_count() const { return saving_queue_.size(); }

	// Convenience: log a one-shot snapshot of all three queue lengths. Useful
	// when bolting MessageAsyncClient onto a periodic reporter.
	void LogQueueSnapshot(const char *tag) const
	{
		if (in_flight_load_count() == 0 && in_flight_save_count() == 0 &&
			pending_load_count() == 0 && pending_save_count() == 0)
		{
			return;
		}
		LOG_INFO << "[" << tag << "] " << full_name()
				 << " in_flight_loads=" << in_flight_load_count()
				 << " in_flight_saves=" << in_flight_save_count()
				 << " pending_loads=" << pending_load_count()
				 << " pending_saves=" << pending_save_count();
	}

private:
	static std::chrono::milliseconds BackoffForRetry(int next_retry_count)
	{
		// 1->500ms, 2->1s, 3->2s, 4->4s, 5->8s, 6+->16s  (cumulative ~31.5s)
		// Tuned for the typical Login -> Kafka -> DB -> Redis warm path P99 (~1-3s).
		// First retry hits fast so a healthy preload barely impacts player-perceived
		// login latency; later retries widen to shed load if the backend is stuck.
		switch (next_retry_count)
		{
		case 1:
			return std::chrono::milliseconds(500);
		case 2:
			return std::chrono::milliseconds(1000);
		case 3:
			return std::chrono::milliseconds(2000);
		case 4:
			return std::chrono::milliseconds(4000);
		case 5:
			return std::chrono::milliseconds(8000);
		default:
			return std::chrono::milliseconds(16000);
		}
	}

	void FlushDuePending(std::chrono::steady_clock::time_point now, bool force)
	{
		// Drain due loads
		std::vector<ElementPtr> dueLoads;
		dueLoads.reserve(pending_retry_queue_.size());
		for (auto it = pending_retry_queue_.begin(); it != pending_retry_queue_.end();)
		{
			if (force || it->second->next_retry_at <= now)
			{
				dueLoads.push_back(it->second);
				it = pending_retry_queue_.erase(it);
			}
			else
			{
				++it;
			}
		}

		// Drain due saves
		std::vector<ElementPtr> dueSaves;
		dueSaves.reserve(pending_save_queue_.size());
		for (auto it = pending_save_queue_.begin(); it != pending_save_queue_.end();)
		{
			if (force || it->second->next_retry_at <= now)
			{
				dueSaves.push_back(it->second);
				it = pending_save_queue_.erase(it);
			}
			else
			{
				++it;
			}
		}

		if (dueLoads.empty() && dueSaves.empty())
		{
			return;
		}
		LOG_INFO << "RetryDuePending: re-issuing " << dueLoads.size() << " loads, "
				 << dueSaves.size() << " saves "
				 << "(deferred loads=" << pending_retry_queue_.size()
				 << " saves=" << pending_save_queue_.size() << ")";
		for (auto &element : dueLoads)
		{
			AsyncLoad(element->message_key, element->retry_count);
		}
		for (auto &element : dueSaves)
		{
			IssueSave(element);
		}
	}

	// Lazily load the SET+SADD Lua script and cache its SHA1 for EVALSHA.
	// Called eagerly from OnReconnected and lazily from IssueSave when sha is empty.
	void EnsureScriptLoaded()
	{
		if (!script_sha1_.empty() || script_load_in_flight_)
		{
			return;
		}
		if (!hiredis_ || !hiredis_->connected())
		{
			return;
		}
		script_load_in_flight_ = true;
		hiredis_->command(std::bind(&MessageAsyncClient::OnSaveScriptLoaded, this, std::placeholders::_1, std::placeholders::_2),
						  "SCRIPT LOAD %s", kSaveAndMarkLuaScript);
	}

	void OnSaveScriptLoaded(hiredis::Hiredis * /*c*/, redisReply *reply)
	{
		script_load_in_flight_ = false;
		if (!reply || reply->type != REDIS_REPLY_STRING)
		{
			LOG_ERROR << "SCRIPT LOAD failed for " << full_name()
					  << " (reply " << (reply ? std::to_string(reply->type) : std::string("null")) << "); will retry on next save";
			return;
		}
		script_sha1_.assign(reply->str, reply->len);
		LOG_INFO << "SCRIPT LOAD ok for " << full_name() << " sha1=" << script_sha1_;
	}

	void IssueSave(const ElementPtr &element)
	{
		// Save() and the completion path serialize writes per key. Treat a second
		// issue defensively as a coalesced successor instead of allowing two SETs
		// whose retry order could roll Redis backwards.
		if (saving_queue_.find(element->redis_key) != saving_queue_.end())
		{
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		if (!hiredis_ || !hiredis_->connected())
		{
			pending_save_queue_[element->redis_key] = element;
			return;
		}

		EnsureScriptLoaded();

		saving_queue_[element->redis_key] = element;
		int ret = REDIS_OK;
		if (!script_sha1_.empty())
		{
			ret = hiredis_->command(std::bind(&MessageAsyncClient::OnSaved, this, std::placeholders::_1, std::placeholders::_2, element),
									"EVALSHA %b 1 %b %b",
									script_sha1_.data(), script_sha1_.size(),
									element->redis_key.c_str(), element->redis_key.length(),
									element->serialized_payload.data(), element->serialized_payload.size());
		}
		else
		{
			// Fall back to EVAL while SCRIPT LOAD is in flight or after a failure.
			ret = hiredis_->command(std::bind(&MessageAsyncClient::OnSaved, this, std::placeholders::_1, std::placeholders::_2, element),
									"EVAL %s 1 %b %b",
									kSaveAndMarkLuaScript,
									element->redis_key.c_str(), element->redis_key.length(),
									element->serialized_payload.data(), element->serialized_payload.size());
		}

		if (ret != REDIS_OK)
		{
			LOG_ERROR << "Redis Save command failed (ret=" << ret << ") for key: " << element->redis_key;
			saving_queue_.erase(element->redis_key);
			QueueSaveForRetry(element);
		}
	}

	static constexpr int kMaxSaveRetries = 6;

	void QueueSaveForRetry(const ElementPtr &element)
	{
		// A later full snapshot supersedes this failed value. Issue the newer one
		// immediately; retrying the older payload first would both waste work and
		// create a stale-overwrite window.
		if (auto newer = pending_save_queue_.find(element->redis_key);
			newer != pending_save_queue_.end() && newer->second != element)
		{
			auto next = newer->second;
			pending_save_queue_.erase(newer);
			IssueSave(next);
			return;
		}

		if (element->retry_count >= kMaxSaveRetries && !element->save_failure_notified)
		{
			// 注意:这里**不能**指望 db 服务把缓存修回来。
			// scene 存的是 "<PlayerAllData full_name>:{id}",而 db 服务写回的是
			// login 读的分表 key("player_database:{id}" 等),两组 key 互不覆盖。
			// 所以不能在阈值处丢掉最新 payload 或让调用方销毁唯一内存态。
			// 这里只通知一次,随后以封顶退避继续重试,直到 Redis 恢复或进程
			// 进入有界停机流程。
			LOG_ERROR << "Save exhausted " << kMaxSaveRetries << " retries for key: " << element->redis_key
					  << " -- retaining the latest payload and retrying indefinitely with capped backoff. "
					  << "This key is NOT repaired by dbservice (different key space).";
			element->save_failure_notified = true;
			if (save_failed_callback_)
			{
				save_failed_callback_(element->message_key, element->redis_key, element->retry_count);
			}
		}
		const int nextRetry = std::min(element->retry_count + 1, kMaxSaveRetries);
		const auto backoff = BackoffForRetry(nextRetry);
		element->retry_count = nextRetry;
		element->next_retry_at = std::chrono::steady_clock::now() + backoff;
		// Latest pending value per key wins (Save with same key replaces older retry).
		pending_save_queue_[element->redis_key] = element;
		LOG_WARN << "Queued Save retry " << nextRetry << "/" << kMaxSaveRetries
				 << " for key: " << element->redis_key << " in " << backoff.count() << "ms";
	}

	void OnSaved(hiredis::Hiredis * /*c*/, redisReply *reply, ElementPtr element)
	{
		// Ignore a callback from a connection that was superseded during
		// reconnect. The current in-flight/pending value owns this key now.
		auto inFlight = saving_queue_.find(element->redis_key);
		if (inFlight == saving_queue_.end() || inFlight->second != element)
		{
			LOG_WARN << "Ignoring stale Redis Save callback for key: " << element->redis_key;
			return;
		}

		if (!reply)
		{
			LOG_ERROR << "Redis Save: null reply for key: " << element->redis_key;
			saving_queue_.erase(inFlight);
			QueueSaveForRetry(element);
			return;
		}
		if (reply->type == REDIS_REPLY_ERROR)
		{
			const std::string err = reply->str ? reply->str : "";
			// NOSCRIPT: server flushed scripts (FLUSHALL/SCRIPT FLUSH/restart) -> reload and retry as EVAL once.
			if (err.compare(0, 8, "NOSCRIPT") == 0)
			{
				LOG_WARN << "EVALSHA NOSCRIPT for key: " << element->redis_key << " -- reloading script and retrying";
				script_sha1_.clear();
				EnsureScriptLoaded();
				// Immediate retry as EVAL (do not increment retry_count for this case).
				const int ret = hiredis_->command(std::bind(&MessageAsyncClient::OnSaved, this, std::placeholders::_1, std::placeholders::_2, element),
												  "EVAL %s 1 %b %b",
												  kSaveAndMarkLuaScript,
												  element->redis_key.c_str(), element->redis_key.length(),
												  element->serialized_payload.data(), element->serialized_payload.size());
				if (ret != REDIS_OK)
				{
					saving_queue_.erase(element->redis_key);
					QueueSaveForRetry(element);
				}
				return;
			}
			LOG_ERROR << "Redis Save error for key: " << element->redis_key << " err=" << err;
			saving_queue_.erase(inFlight);
			QueueSaveForRetry(element);
			return;
		}

		saving_queue_.erase(inFlight);
		// Do not publish completion for an older full snapshot when a newer one
		// is waiting. In the Scene lifecycle that callback may destroy an exiting
		// entity, so only the newest successfully persisted snapshot may complete
		// the chain.
		if (auto newer = pending_save_queue_.find(element->redis_key);
			newer != pending_save_queue_.end())
		{
			auto next = newer->second;
			pending_save_queue_.erase(newer);
			IssueSave(next);
			return;
		}
		if (save_callback_)
		{
			save_callback_(element->message_key, *element->message_value);
		}
	}

	void OnLoaded(hiredis::Hiredis* /*c*/, redisReply* reply, ElementPtr element)
	{
		// 重连可能淘汰旧 GET，并为同一 key 发出新请求；旧连接的迟到回包
		// 不能删除或完成这个替代请求。
		auto inFlight = loading_queue_.find(element->redis_key);
		if (inFlight == loading_queue_.end() || inFlight->second != element)
		{
			LOG_WARN << "Ignoring stale Redis Load callback for key: " << element->redis_key;
			return;
		}
		loading_queue_.erase(inFlight);

		if (!reply || reply->type == REDIS_REPLY_ERROR)
		{
			const std::string errorDetail = reply == nullptr
				? " (null reply)"
				: std::string(" err=") + (reply->str != nullptr
					? std::string(reply->str, reply->len)
					: std::string("<missing error text>"));
			LOG_ERROR << "Redis GET error for key: " << element->redis_key
					  << errorDetail;
			FailLoad(element, LoadFailureReason::RedisError);
			return;
		}

		if (reply->type == REDIS_REPLY_NIL)
		{
			if (element->retry_count < kMaxLoadRetries)
			{
				const int nextRetry = element->retry_count + 1;
				const auto backoff = BackoffForRetry(nextRetry);
				LOG_WARN << "Redis GET returned NIL for key: " << element->redis_key
						 << ", retry " << nextRetry << "/" << kMaxLoadRetries
						 << " in " << backoff.count() << "ms";
				element->retry_count = nextRetry;
				element->next_retry_at = std::chrono::steady_clock::now() + backoff;
				pending_retry_queue_[element->redis_key] = element;
				return;
			}

			LOG_ERROR << "Redis GET returned NIL for key: " << element->redis_key
					  << ", exhausted all " << kMaxLoadRetries << " retries";
			FailLoad(element, LoadFailureReason::DataNotFound);
			return;
		}

		if (reply->type == REDIS_REPLY_STRING)
		{
			if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
			{
				LOG_ERROR << "Redis payload too large for ParseFromArray, key: " << element->redis_key
						  << ", len=" << reply->len;
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}
			if (reply->len == 0 || reply->str == nullptr)
			{
				// Scene 保存的 PlayerAllData 必含 player_id；已存在的零字节值是损坏，
				// 不是“新玩家”信号。只有有界重试后的 REDIS_REPLY_NIL 才表示不存在。
				LOG_ERROR << "Redis payload is empty/invalid for key: " << element->redis_key;
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}

			element->message_value = CreateMessage();
			if (!element->message_value->ParseFromArray(reply->str, static_cast<int>(reply->len)))
			{
				LOG_ERROR << "ParseFromArray failed for key: " << element->redis_key;
				// 默认或部分解析的 proto 绝不能作为成功结果发布。Scene 会把
				// player_id=0 当作新角色，随后可能用默认值覆盖权威玩家状态。
				FailLoad(element, LoadFailureReason::RedisError);
				return;
			}
		}
		else
		{
			LOG_ERROR << "Redis GET unexpected reply type=" << reply->type << " for key: " << element->redis_key;
			FailLoad(element, LoadFailureReason::RedisError);
			return;
		}

		if (load_callback_)
		{
			load_callback_(element->message_key, *element->message_value);
		}
	}

	void FailLoad(const ElementPtr &element, LoadFailureReason reason)
	{
		// 终态回包不应再持有重试项；回调前防御性清理，保证回调若重入重试，
		// 也是从干净状态开始。
		pending_retry_queue_.erase(element->redis_key);
		element->message_value.reset();
		if (load_failed_callback_)
		{
			load_failed_callback_(element->message_key, reason);
		}
	}

private:
	template <class, class>
	friend struct MessageAsyncClientTestPeer;

	HiredisPtr& hiredis_;
	LoadingQueue loading_queue_;
	LoadingQueue pending_retry_queue_;
	LoadingQueue saving_queue_;
	LoadingQueue pending_save_queue_;
	std::string script_sha1_;
	bool script_load_in_flight_ = false;
	EventCallback save_callback_;
	SaveFailedCallback save_failed_callback_;
	EventCallback load_callback_;
	FailedCallback load_failed_callback_;
};
