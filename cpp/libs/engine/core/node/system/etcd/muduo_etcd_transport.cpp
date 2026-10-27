// Windows:必须最先包含,让 winsock2 的声明抢在 <Windows.h> 之前(与 gate/main.cpp 同一理由);整个头文件只在 WIN32 下有内容。
#ifdef WIN32
#include "muduo/base/CrossPlatformAdapterFunction.h"
#include <WinSock2.h>
#include <WS2tcpip.h>
#else
#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <sys/socket.h>
#endif

#include "node/system/etcd/muduo_etcd_transport.h"

#include <chrono>
#include <cstdlib>
#include <random>
#include <thread>
#include <unordered_map>
#include <utility>

#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>
#include <muduo/net/TimerId.h>

#include "muduo_grpc/channel.h"
#include "node/system/etcd/etcd_endpoint.h"
#include "node/system/etcd/etcd_stream_backoff.h"
#include "node/system/grpc_call_deadline.h"
#include "proto/common/base/node.pb.h"

namespace
{
// etcd v3 的 gRPC 路径(proto/etcd/etcd.proto,package etcdserverpb)。
constexpr char kRangeMethod[] = "/etcdserverpb.KV/Range";
constexpr char kPutMethod[] = "/etcdserverpb.KV/Put";
constexpr char kDeleteRangeMethod[] = "/etcdserverpb.KV/DeleteRange";
constexpr char kTxnMethod[] = "/etcdserverpb.KV/Txn";
constexpr char kLeaseGrantMethod[] = "/etcdserverpb.Lease/LeaseGrant";
constexpr char kLeaseRevokeMethod[] = "/etcdserverpb.Lease/LeaseRevoke";
constexpr char kWatchMethod[] = "/etcdserverpb.Watch/Watch";
constexpr char kLeaseKeepAliveMethod[] = "/etcdserverpb.Lease/LeaseKeepAlive";

// §13.3:etcd 通道的周期 keepalive。etcd 服务端 --grpc-keepalive-min-time 默认 5s,30s 一次远高于下限。
constexpr std::chrono::milliseconds kKeepaliveTime{30000};
constexpr std::chrono::milliseconds kKeepaliveTimeout{10000};
// 连接级退避封顶 5s(默认 120s):etcd 恢复后几秒内就要连回去,lease TTL 不等人。与流重建的封顶一致。
constexpr std::chrono::milliseconds kReconnectBackoffMax{5000};
// 接收上限放到 64MiB(默认 4MiB):一个前缀的 Range 应答装着该类全部节点的 NodeInfo,约 4000 个就会超 4MiB;
// 超了每次都失败,该前缀就永远建不了 watch(新传输只在本前缀 Range 成功后才建)。etcd clientv3 默认不设上限。
constexpr size_t kMaxReceiveMessageBytes = 64 * 1024 * 1024;
// 同一方法的 unary 失败日志限频:etcd 不可用期间每 2s 九个前缀的 Range 都会失败。
constexpr std::chrono::seconds kFailureLogInterval{10};
// §13.6:主机名解析的重试预算。
constexpr int kResolveAttempts = 10;
constexpr std::chrono::seconds kResolveRetryInterval{1};

bool ToCanonicalIpv4(const std::string &host, std::string &ip)
{
	in_addr address{};
	if (::inet_pton(AF_INET, host.c_str(), &address) != 1)
	{
		return false;
	}
	char text[INET_ADDRSTRLEN] = {};
	if (::inet_ntop(AF_INET, &address, text, sizeof(text)) == nullptr)
	{
		return false;
	}
	ip = text;
	return true;
}

bool ResolveIpv4Once(const std::string &host, std::string &ip, int &errorCode)
{
	addrinfo hints{};
	hints.ai_family = AF_INET;
	hints.ai_socktype = SOCK_STREAM;
	addrinfo *results = nullptr;
	errorCode = ::getaddrinfo(host.c_str(), nullptr, &hints, &results);
	if (errorCode != 0 || results == nullptr)
	{
		return false;
	}
	bool resolved = false;
	char text[INET_ADDRSTRLEN] = {};
	const auto *address = reinterpret_cast<const sockaddr_in *>(results->ai_addr);
	if (address != nullptr && ::inet_ntop(AF_INET, &address->sin_addr, text, sizeof(text)) != nullptr)
	{
		ip = text;
		resolved = true;
	}
	::freeaddrinfo(results);
	return resolved;
}

// Node 构造期调用(loop 尚未运行,阻塞可接受)。运行中不再做 DNS:K8s Service 的 ClusterIP 在 Service 存续期间不变。
std::string ResolveIpv4OrDie(const std::string &host)
{
	std::string ip;
	if (ToCanonicalIpv4(host, ip))
	{
		return ip;
	}
	for (int attempt = 1; attempt <= kResolveAttempts; ++attempt)
	{
		int errorCode = 0;
		if (ResolveIpv4Once(host, ip, errorCode))
		{
			LOG_INFO << "etcd host " << host << " resolved to " << ip << " (attempt " << attempt << ")";
			return ip;
		}
		LOG_WARN << "Failed to resolve etcd host " << host << " (attempt " << attempt << "/" << kResolveAttempts
				 << ", getaddrinfo error " << errorCode << ")";
		if (attempt < kResolveAttempts)
		{
			std::this_thread::sleep_for(kResolveRetryInterval);
		}
	}
	LOG_FATAL << "Cannot resolve etcd host " << host << " after " << kResolveAttempts
			  << " attempts; refusing to start without etcd";
	return ip;
}
} // namespace

class MuduoEtcdTransport::Core : public std::enable_shared_from_this<Core>
{
public:
	enum class StreamKind : uint8_t
	{
		kWatch,
		kKeepAlive,
	};

	Core(muduo::net::EventLoop &loop, EtcdResponseSink &sink)
		: loop_(loop), sink_(sink), rng_(std::random_device{}())
	{
		watch_.name = "Watch";
		watch_.method = kWatchMethod;
		keepAlive_.name = "LeaseKeepAlive";
		keepAlive_.method = kLeaseKeepAliveMethod;
	}

	EtcdResponseSink &sink() { return sink_; }

	void Start(const std::string &endpoint)
	{
		etcd_transport::Endpoint parsed;
		std::string error;
		if (!etcd_transport::ParseEndpoint(endpoint, parsed, error))
		{
			LOG_FATAL << "Invalid etcd endpoint '" << endpoint << "': " << error;
		}
		const std::string ip = ResolveIpv4OrDie(parsed.host);

		muduo_grpc::ChannelOptions options;
		// 本 owner 仅承载 etcd 注册/发现控制面：Watch 只读，KeepAlive、Lease、KV 只更新 etcd。
		// 不把长控制流或 best-effort LeaseRevoke 的 UNKNOWN 当成业务 SQL/Kafka/Redis 发布；
		// 未知仍留在独立账本，Shutdown 的 shutdown_ 门禁、原 channel 析构与全部回调退出必须参加 P3 join。
		// 此分类由真实 owner 显式注入，通用 channel 不按方法名或 CANCELLED 状态猜测。
		options.maintenance = muduo_grpc::Channel::ProcessControlMaintenance();
		options.authority = parsed.host + ":" + std::to_string(parsed.port);
		options.keepaliveTime = kKeepaliveTime;
		options.keepaliveTimeout = kKeepaliveTimeout;
		options.backoff.max = kReconnectBackoffMax;
		options.maxReceiveMessageBytes = kMaxReceiveMessageBytes;
		channel_ = muduo_grpc::Channel::Create(loop_, ip, parsed.port, options);
		if (!channel_)
		{
			LOG_FATAL << "Cannot create the etcd channel to " << ip << ":" << parsed.port;
		}
		callDeadline_ = grpc_call_deadline::Get(static_cast<uint32_t>(common::base::EtcdNodeService));
		LOG_INFO << "etcd muduo_h2 transport: endpoint=" << endpoint << " target=" << ip << ":" << parsed.port
				 << " call_deadline_ms=" << callDeadline_.count() << " keepalive_ms=" << kKeepaliveTime.count() << "/"
				 << kKeepaliveTimeout.count();

		channel_->GetState(true);
		OpenStream(StreamKind::kWatch);
		OpenStream(StreamKind::kKeepAlive);
	}

	// 发一个 unary 调用;成功且应答能解析时 onReply(*this, response)。失败只记日志(恢复由 EtcdService 的看门狗负责)。
	template <typename Response, typename OnReply>
	void Unary(const char *method, const google::protobuf::Message &request, OnReply onReply)
	{
		if (shutdown_ || !channel_)
		{
			return;
		}
		muduo_grpc::UnaryCallRequest call;
		call.method = method;
		if (!request.SerializeToString(&call.message))
		{
			LOG_ERROR << "etcd " << method << ": failed to serialize the request";
			return;
		}
		call.deadline = std::chrono::steady_clock::now() + callDeadline_;
		std::weak_ptr<Core> weakSelf = weak_from_this();
		channel_->StartUnaryCall(std::move(call),
								 [weakSelf, methodName = std::string(method), onReply = std::move(onReply)](
									 muduo_grpc::UnaryCallResult &&result)
								 {
			const auto self = weakSelf.lock();
			if (!self || self->shutdown_)
			{
				return;
			}
			if (!result.status.ok())
			{
				self->LogCallFailure(methodName, result.status);
				return;
			}
			Response response;
			if (!response.ParseFromString(result.response))
			{
				self->LogCallFailure(methodName, muduo_grpc::Status(muduo_grpc::StatusCode::kInternal,
																	 "failed to parse the response"));
				return;
			}
			onReply(*self, response); });
	}

	void WriteStream(StreamKind kind, const google::protobuf::Message &request)
	{
		StreamSlot &slot = Slot(kind);
		if (shutdown_)
		{
			return;
		}
		if (!slot.stream || slot.stream->closed())
		{
			// 流正在重建:旧流上的请求没有意义,重建后由 sink 的 Restarted 回调重发需要的请求。
			LOG_DEBUG << "etcd " << slot.name << " write dropped: the stream is being rebuilt";
			return;
		}
		std::string bytes;
		if (!request.SerializeToString(&bytes))
		{
			LOG_ERROR << "etcd " << slot.name << ": failed to serialize the request";
			return;
		}
		if (!slot.stream->Write(std::move(bytes)))
		{
			LOG_WARN << "etcd " << slot.name << " write rejected; the stream is closing and will be rebuilt";
		}
	}

	void Shutdown()
	{
		if (shutdown_)
		{
			return;
		}
		shutdown_ = true;
		// 正常路径(EtcdService::Shutdown)在 loop 线程上、loop 仍在运行;漏了清理时只能放弃取消定时器
		// (闭包只绑 weak_ptr,本对象没了它们自动失效)。
		const bool inLoop = muduo::net::EventLoop::getEventLoopOfCurrentThread() == &loop_;
		for (StreamSlot *slot : {&watch_, &keepAlive_})
		{
			if (slot->rebuildArmed && inLoop)
			{
				loop_.cancel(slot->rebuildTimer);
			}
			slot->rebuildArmed = false;
		}
		// 先释放通道再释放流句柄:~Channel 把在途调用(含刚发出的 LeaseRevoke)以失败投递 —— 那些回调只绑
		// weak_ptr<Core> 且看到 shutdown_,不再回调 sink;句柄随后析构只是给流记录置取消标记。
		// LeaseRevoke 只是「尽量送达」(§13.7):通道 READY 且 muduo 输出缓冲为空时,发出那一刻字节已写进 socket;
		// 否则还留在用户态缓冲里,随 forceClose 丢弃,lease 等 TTL 到期才被 etcd 回收(与旧实现同样不等应答)。
		channel_.reset();
		watch_.stream.reset();
		keepAlive_.stream.reset();
	}

private:
	struct StreamSlot
	{
		std::string name;
		std::string method;
		std::shared_ptr<muduo_grpc::ClientStream> stream;
		uint64_t generation{0};
		std::chrono::steady_clock::time_point openedAt{};
		etcd_transport::StreamRebuildBackoff backoff;
		muduo::net::TimerId rebuildTimer;
		bool rebuildArmed{false};
	};

	struct FailureLog
	{
		std::chrono::steady_clock::time_point lastLoggedAt{};
		uint64_t suppressed{0};
		bool everLogged{false};
	};

	StreamSlot &Slot(StreamKind kind) { return kind == StreamKind::kWatch ? watch_ : keepAlive_; }

	void OpenStream(StreamKind kind)
	{
		StreamSlot &slot = Slot(kind);
		if (shutdown_ || !channel_)
		{
			return;
		}
		const uint64_t generation = ++slot.generation;
		slot.openedAt = std::chrono::steady_clock::now();
		std::weak_ptr<Core> weakSelf = weak_from_this();
		muduo_grpc::StreamCallbacks callbacks;
		callbacks.onMessage = [weakSelf, kind, generation](std::string &&message)
		{
			if (const auto self = weakSelf.lock())
			{
				self->OnStreamMessage(kind, generation, std::move(message));
			}
		};
		callbacks.onClose = [weakSelf, kind, generation](const muduo_grpc::Status &status, const muduo_grpc::Metadata &)
		{
			if (const auto self = weakSelf.lock())
			{
				self->OnStreamClosed(kind, generation, status);
			}
		};
		// 流在通道 IDLE / CONNECTING 时排队、连上再开;此前写入的消息在本端缓冲,开流后按序发出。
		slot.stream = channel_->StartStream(slot.method, muduo_grpc::Metadata(), std::move(callbacks));
		if (generation == 1)
		{
			return; // 首条流:注册链从 EtcdService::Init 之后的全量 Range 开始,不需要 Restarted
		}
		LOG_INFO << "etcd " << slot.name << " stream reopened (generation " << generation << ")";
		if (kind == StreamKind::kWatch)
		{
			sink_.OnWatchStreamRestarted();
		}
		else
		{
			sink_.OnKeepAliveStreamRestarted();
		}
	}

	void OnStreamMessage(StreamKind kind, uint64_t generation, std::string message)
	{
		StreamSlot &slot = Slot(kind);
		if (shutdown_ || generation != slot.generation)
		{
			return;
		}
		if (kind == StreamKind::kWatch)
		{
			etcdserverpb::WatchResponse response;
			if (!response.ParseFromString(message))
			{
				AbandonStream(kind, generation, "failed to parse a WatchResponse");
				return;
			}
			sink_.OnWatch(response);
			return;
		}
		etcdserverpb::LeaseKeepAliveResponse response;
		if (!response.ParseFromString(message))
		{
			AbandonStream(kind, generation, "failed to parse a LeaseKeepAliveResponse");
			return;
		}
		sink_.OnKeepAlive(response);
	}

	// 本端放弃一条流(收到无法解析的消息):取消它(之后不再有它的回调),按普通的流结束走退避重建。
	void AbandonStream(StreamKind kind, uint64_t generation, const char *reason)
	{
		StreamSlot &slot = Slot(kind);
		if (slot.stream)
		{
			slot.stream->Cancel();
		}
		OnStreamClosed(kind, generation, muduo_grpc::Status(muduo_grpc::StatusCode::kInternal, reason));
	}

	void OnStreamClosed(StreamKind kind, uint64_t generation, const muduo_grpc::Status &status)
	{
		StreamSlot &slot = Slot(kind);
		if (shutdown_ || generation != slot.generation)
		{
			return;
		}
		slot.stream.reset();
		const auto lived = std::chrono::steady_clock::now() - slot.openedAt;
		const auto base = slot.backoff.OnStreamEnded(lived);
		std::uniform_real_distribution<double> spread(-etcd_transport::StreamRebuildBackoff::kJitter,
													  etcd_transport::StreamRebuildBackoff::kJitter);
		const double delaySeconds = static_cast<double>(base.count()) / 1000.0 * (1.0 + spread(rng_));
		LOG_WARN << "etcd " << slot.name << " stream ended after "
				 << std::chrono::duration_cast<std::chrono::milliseconds>(lived).count() << "ms: " << status.ToString()
				 << "; reopening in " << delaySeconds << "s";
		std::weak_ptr<Core> weakSelf = weak_from_this();
		// 墙钟定时器只当延时用,早晚几毫秒无所谓;没有次数上限(§10.2 第 1 条的有意偏离)。
		slot.rebuildTimer = loop_.runAfter(delaySeconds, [weakSelf, kind]
										   {
			if (const auto self = weakSelf.lock())
			{
				self->OnRebuildTimer(kind);
			} });
		slot.rebuildArmed = true;
	}

	void OnRebuildTimer(StreamKind kind)
	{
		Slot(kind).rebuildArmed = false;
		OpenStream(kind);
	}

	void LogCallFailure(const std::string &method, const muduo_grpc::Status &status)
	{
		auto &log = failureLogs_[method];
		const auto now = std::chrono::steady_clock::now();
		if (log.everLogged && now - log.lastLoggedAt < kFailureLogInterval)
		{
			++log.suppressed;
			return;
		}
		LOG_WARN << "etcd " << method << " failed: " << status.ToString() << " (" << log.suppressed
				 << " similar failures suppressed since the last report)";
		log.everLogged = true;
		log.lastLoggedAt = now;
		log.suppressed = 0;
	}

	muduo::net::EventLoop &loop_;
	EtcdResponseSink &sink_;
	std::shared_ptr<muduo_grpc::Channel> channel_;
	std::chrono::milliseconds callDeadline_{0};
	StreamSlot watch_;
	StreamSlot keepAlive_;
	bool shutdown_{false};
	std::mt19937 rng_;
	std::unordered_map<std::string, FailureLog> failureLogs_;
};

MuduoEtcdTransport::MuduoEtcdTransport(muduo::net::EventLoop &loop, const std::string &endpoint,
									   EtcdResponseSink &sink)
	: core_(std::make_shared<Core>(loop, sink))
{
	core_->Start(endpoint);
}

MuduoEtcdTransport::~MuduoEtcdTransport()
{
	core_->Shutdown();
}

void MuduoEtcdTransport::Range(const etcdserverpb::RangeRequest &request)
{
	// 带回原请求:EtcdService 按 request.key()(前缀)做对账。
	core_->Unary<etcdserverpb::RangeResponse>(kRangeMethod, request,
											  [request](Core &core, const etcdserverpb::RangeResponse &reply)
											  { core.sink().OnRange(&request, reply); });
}

void MuduoEtcdTransport::Put(const etcdserverpb::PutRequest &request)
{
	core_->Unary<etcdserverpb::PutResponse>(kPutMethod, request,
											[](Core &core, const etcdserverpb::PutResponse &reply)
											{ core.sink().OnPut(reply); });
}

void MuduoEtcdTransport::DeleteRange(const etcdserverpb::DeleteRangeRequest &request)
{
	core_->Unary<etcdserverpb::DeleteRangeResponse>(kDeleteRangeMethod, request,
													[](Core &core, const etcdserverpb::DeleteRangeResponse &reply)
													{ core.sink().OnDeleteRange(reply); });
}

void MuduoEtcdTransport::Txn(const etcdserverpb::TxnRequest &request)
{
	core_->Unary<etcdserverpb::TxnResponse>(kTxnMethod, request,
											[](Core &core, const etcdserverpb::TxnResponse &reply)
											{ core.sink().OnTxn(reply); });
}

void MuduoEtcdTransport::LeaseGrant(const etcdserverpb::LeaseGrantRequest &request)
{
	core_->Unary<etcdserverpb::LeaseGrantResponse>(kLeaseGrantMethod, request,
												   [](Core &core, const etcdserverpb::LeaseGrantResponse &reply)
												   { core.sink().OnLeaseGrant(reply); });
}

void MuduoEtcdTransport::LeaseRevoke(const etcdserverpb::LeaseRevokeRequest &request)
{
	// 应答没有消费方(旧实现同样不处理);只为失败时留一行日志。
	core_->Unary<etcdserverpb::LeaseRevokeResponse>(kLeaseRevokeMethod, request,
													[](Core &, const etcdserverpb::LeaseRevokeResponse &) {});
}

void MuduoEtcdTransport::WatchWrite(const etcdserverpb::WatchRequest &request)
{
	core_->WriteStream(Core::StreamKind::kWatch, request);
}

void MuduoEtcdTransport::KeepAliveWrite(const etcdserverpb::LeaseKeepAliveRequest &request)
{
	core_->WriteStream(Core::StreamKind::kKeepAlive, request);
}

void MuduoEtcdTransport::Shutdown()
{
	core_->Shutdown();
}

namespace etcd_transport
{
void InstallFromEnv()
{
	// 只为兼容旧部署的环境变量:传输只剩 muduo_h2 一种(官方实现随第 3 批删除,§14),这里不装任何工厂。
	const char *raw = std::getenv("NODE_ETCD_TRANSPORT");
	const std::string value = raw != nullptr ? raw : "";
	if (value.empty() || value == "muduo_h2")
	{
		return;
	}
	if (value == "grpc")
	{
		LOG_WARN << "NODE_ETCD_TRANSPORT=grpc is obsolete: the official gRPC etcd client was removed; using muduo_h2";
		return;
	}
	LOG_WARN << "Unknown NODE_ETCD_TRANSPORT='" << value << "'; using muduo_h2";
}
} // namespace etcd_transport
