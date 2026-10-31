#include "redis_cluster_client.h"

#include <cstddef>
#include <random>
#include <string>
#include <unordered_map>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TimerId.h"

#include "redis_cluster_topology.h"
#include "redis_command_route.h"
#include "redis_reply_classifier.h"

using redis_cluster::Clock;
using redis_cluster::CommandRoute;
using redis_cluster::Duration;
using redis_cluster::HeadState;
using redis_cluster::kAnyNodeQueue;
using redis_cluster::NodeEndpoint;
using redis_cluster::NodeEndpointHash;
using redis_cluster::QueueIndex;
using redis_cluster::RedirectPolicy;
using redis_cluster::ReplyDisposition;
using redis_cluster::RetryDecision;
using redis_cluster::RetryMode;
using redis_cluster::RouteKind;
using redis_cluster::SubmitResult;
using redis_cluster::TimePoint;

namespace
{
	// hiredis 的 nextArgument 要求缓冲以 NUL 结尾,字符串字面量满足。
	constexpr std::string_view kClusterSlotsCommand = "*2\r\n$7\r\nCLUSTER\r\n$5\r\nSLOTS\r\n";
	constexpr std::string_view kAskingCommand = "*1\r\n$6\r\nASKING\r\n";

	// 日志里键名、命令名、服务端文本最多打印这么多字节。
	constexpr std::size_t kMaxLoggedBytes = 128;

	std::string Loggable(std::string_view text)
	{
		return std::string(text.substr(0, kMaxLoggedBytes));
	}

	double ToSeconds(Duration duration)
	{
		return std::chrono::duration<double>(duration).count();
	}

	struct PendingCommand
	{
		struct FreeFormatted
		{
			void operator()(char* buffer) const { ::redisFreeCommand(buffer); }
		};

		~PendingCommand()
		{
			// 捕获对象的析构也可能释放别的资源:先释放 target,再登记完成,同 Hiredis 的 PendingMaintenanceCommand。
			callback = nullptr;
			if (maintenance)
			{
				maintenance->Complete(known);
			}
		}

		RedisClusterClient::CommandCallback callback;
		// redisvFormatCommand 产出的缓冲,以 NUL 结尾,保留到拿到最终应答,供重发使用。
		std::unique_ptr<char, FreeFormatted> formatted;
		std::size_t formattedLength = 0;
		uint64_t id = 0;
		QueueIndex queue = 0;
		std::optional<NodeEndpoint> askTarget;                   // 下一次发送走 ASK:先 ASKING,只这一次
		std::optional<redis_cluster::NotExecutedError> lastError; // 最近一次「未执行」错误,诊断用
		// 只在 Shutdown 时填:在途命令之后排队的命令,要排在它之后回调,而它的空应答可能晚于本对象的 Impl 析构。
		std::vector<std::shared_ptr<PendingCommand>> followers;
		// 路由层接受的命令在拿到最终结果之前都算进程未结清:排队与退避中的命令不在任何连接上,维护排空不能漏掉它们。
		std::shared_ptr<hiredis::Hiredis::MaintenanceState> maintenance;
		bool known = false;
	};

	using PendingCommandPtr = std::shared_ptr<PendingCommand>;

	// Impl 已经进入停机(或已销毁)之后才到的应答:不再有队列和预算,未执行类错误也只能当空应答。
	void FinishDetached(const PendingCommandPtr& command, hiredis::Hiredis* connection, redisReply* reply)
	{
		const bool notExecuted = redis_cluster::IsNotExecutedReply(reply);
		command->callback(connection, notExecuted ? nullptr : reply);
		command->known = notExecuted || (reply != nullptr && reply->type != REDIS_REPLY_ERROR);
		const std::vector<PendingCommandPtr> followers = std::move(command->followers);
		command->followers.clear();
		for (const PendingCommandPtr& follower : followers)
		{
			follower->callback(nullptr, nullptr);
			follower->known = true; // 从未发出,确定没有副作用
		}
	}
}

class RedisClusterClient::Impl : public std::enable_shared_from_this<RedisClusterClient::Impl>
{
public:
	Impl(muduo::net::EventLoop& loop, RedisClusterOptions options)
		: loop_(loop)
		, options_(std::move(options))
		, queues_(options_.retry, options_.limits)
		, refreshPolicy_(options_.topologyRefreshPeriod, options_.topologyRefreshMinInterval)
		, maintenance_(hiredis::Hiredis::ProcessMaintenance())
		, rng_(std::random_device{}())
	{
	}

	// 构造之外单独一步:绑回调要 weak_from_this,构造函数里还拿不到。
	void Start()
	{
		std::vector<NodeEndpoint> seeds;
		for (const NodeEndpoint& seed : options_.seeds)
		{
			if (redis_cluster::IsIpLiteral(seed.ip) && seed.port != 0)
			{
				seeds.push_back(seed);
			}
			else
			{
				LOG_ERROR << "Redis cluster seed " << seed.ToString() << " is not an IP literal with a valid port, ignored";
			}
		}
		options_.seeds = std::move(seeds);

		if (options_.mode == RedisClusterMode::kStandalone)
		{
			if (options_.seeds.size() != 1)
			{
				LOG_ERROR << "Redis standalone mode needs exactly one valid seed, got " << options_.seeds.size()
						  << " -- every command will be rejected";
				return;
			}
			table_.AssignAll(options_.seeds.front());
			topologyReady_ = true;
			EnsureNode(options_.seeds.front());
			return;
		}

		if (options_.seeds.empty())
		{
			LOG_ERROR << "Redis cluster mode has no valid seed -- every command will be rejected";
		}
		for (const NodeEndpoint& seed : options_.seeds)
		{
			EnsureNode(seed);
		}
		refreshPolicy_.Request();
		// 同时承担刷新限速:节拍取最小刷新间隔,没到的请求留到下一拍。
		const Duration tick = options_.topologyRefreshMinInterval;
		tickTimerId_ = loop_.get().runEvery(
			tick > Duration::zero() ? ToSeconds(tick) : 1.0,
			[weak = weak_from_this()]
			{
				if (const auto self = weak.lock())
				{
					self->OnTick();
				}
			});
		tickTimerActive_ = true;
	}

	void Shutdown()
	{
		if (closing_)
		{
			return;
		}
		closing_ = true;
		nodeReadyCb_ = nullptr;
		if (tickTimerActive_)
		{
			loop_.get().cancel(tickTimerId_);
			tickTimerActive_ = false;
		}

		// 先把队列清空,此后迟到的应答都走 FinishDetached,新命令因 closing_ 直接 REDIS_ERR。
		std::vector<PendingCommandPtr> neverSent;
		for (const QueueIndex queue : queues_.BusyQueues())
		{
			const bool headInFlight = queues_.State(queue) == HeadState::kInFlight;
			std::vector<PendingCommandPtr> commands = queues_.Abort(queue);
			for (std::size_t i = 0; i < commands.size(); ++i)
			{
				if (!headInFlight)
				{
					neverSent.push_back(std::move(commands[i]));
				}
				else if (i > 0)
				{
					// 队头还在连接上,空应答要等连接释放。后面的命令挂在它身上,保住回调顺序。
					commands.front()->followers.push_back(std::move(commands[i]));
				}
			}
		}
		for (const PendingCommandPtr& command : neverSent)
		{
			command->callback(nullptr, nullptr);
			command->known = true;
		}

		for (auto& entry : nodes_)
		{
			retiredManagers_.push_back(std::move(entry.second.manager));
		}
		nodes_.clear();
		ShutdownRetired();
	}

	void SetReconnectCallback(ReconnectCallback cb)
	{
		nodeReadyCb_ = std::move(cb);
	}

	int Submit(std::optional<std::string_view> routingKey, const CommandCallback& cb, const char* fmt, va_list args)
	{
		loop_.get().assertInLoopThread();
		if (closing_)
		{
			return REDIS_ERR;
		}
		char* raw = nullptr;
		const int length = ::redisvFormatCommand(&raw, fmt, args);
		if (length < 0)
		{
			++stats_.rejectedByGuard;
			LOG_ERROR << "Redis cluster client could not format command " << Loggable(fmt) << " (ret=" << length << ")";
			return REDIS_ERR;
		}

		auto command = std::make_shared<PendingCommand>();
		command->formatted.reset(raw);
		command->formattedLength = static_cast<std::size_t>(length);

		const CommandRoute route = redis_cluster::ResolveCommandRoute(
			std::string_view(raw, command->formattedLength), routingKey, options_.hashTagPolicy);
		if (route.kind == RouteKind::kRejected)
		{
			++stats_.rejectedByGuard;
			LOG_ERROR << "Redis cluster guard rejected command " << Loggable(route.commandName) << ": "
					  << std::string(redis_cluster::RouteRejectReasonName(route.reason)) << " key=" << Loggable(route.offendingKey);
			return REDIS_ERR;
		}

		const QueueIndex queue = route.kind == RouteKind::kAnyNode ? kAnyNodeQueue : route.slot;
		if (!CanAccept(queue))
		{
			++stats_.rejectedNotConnected;
			return REDIS_ERR;
		}
		if (!maintenance_ || !maintenance_->Begin())
		{
			return REDIS_ERR;
		}
		command->maintenance = maintenance_;
		command->callback = cb;
		command->queue = queue;
		command->id = nextCommandId_++;

		const TimePoint now = Clock::now();
		switch (queues_.Submit(queue, command, now))
		{
		case SubmitResult::kQueued:
			return REDIS_OK;
		case SubmitResult::kQueueFull:
		case SubmitResult::kTotalQueueFull:
			++stats_.rejectedQueueFull;
			command->known = true; // 没有入队,确定没有副作用
			return REDIS_ERR;
		case SubmitResult::kDispatchNow:
			break;
		}

		switch (SendHead(queue, now))
		{
		case SendOutcome::kSent:
		case SendOutcome::kWaitingRetry:
			return REDIS_OK;
		case SendOutcome::kExhausted:
		case SendOutcome::kUnavailable:
			break;
		}
		// 当场发不出去:队列此前空闲,没有后续命令。同 Hiredis::command:返回 REDIS_ERR 就不调回调。
		queues_.Abort(queue);
		command->known = true;
		return REDIS_ERR;
	}

	bool Connected() const
	{
		if (closing_ || !topologyReady_)
		{
			return false;
		}
		for (const auto& entry : nodes_)
		{
			if (IsConnected(entry.second))
			{
				return true;
			}
		}
		return false;
	}

	const RedisClusterStats& Stats() const
	{
		return stats_;
	}

private:
	struct Node
	{
		std::unique_ptr<RedisManager> manager;
		// 握手完成过。没有完成过的节点是新发现的(MOVED 指过来、拓扑里新出现),队头在预算内等它;
		// 完成过又断开的就是断连,按单连接语义收尾。
		bool everConnected = false;
		bool lastSeenConnected = false;   // 上一拍是否连着,用来发现「没有在途命令时的断连」
	};

	enum class SendOutcome : uint8_t
	{
		kSent,           // 已发出,等应答
		kWaitingRetry,   // 暂时发不出,重试定时器已安排
		kUnavailable,    // 节点断连或发送被拒
		kExhausted,      // 预算耗尽
	};

	static bool IsConnected(const Node& node)
	{
		const RedisManager::HiredisPtr& connection = node.manager->GetZoneRedis();
		return connection && connection->connected();
	}

	Node* FindNode(const NodeEndpoint& endpoint)
	{
		const auto found = nodes_.find(endpoint);
		return found == nodes_.end() ? nullptr : &found->second;
	}

	Node& EnsureNode(const NodeEndpoint& endpoint)
	{
		const auto found = nodes_.find(endpoint);
		if (found != nodes_.end())
		{
			return found->second;
		}
		Node& node = nodes_.try_emplace(endpoint).first->second;
		node.manager = std::make_unique<RedisManager>();
		node.manager->SetReconnectCallback(
			[weak = weak_from_this(), endpoint]
			{
				if (const auto self = weak.lock())
				{
					self->OnNodeReady(endpoint);
				}
			});
		RedisConnectOptions connect = options_.connect;
		const double base = connect.reconnectIntervalSec > 0
			? connect.reconnectIntervalSec
			: RedisConnectOptions::kDefaultReconnectIntervalSec;
		std::uniform_real_distribution<double> unit(0.0, 1.0);
		connect.reconnectIntervalSec =
			redis_cluster::JitteredReconnectIntervalSec(base, options_.reconnectJitterSec, unit(rng_));
		node.manager->Connect(
			&loop_.get(), muduo::net::InetAddress(endpoint.ip, endpoint.port, endpoint.IsIpv6()), std::move(connect));
		return node;
	}

	// 轮转着挑一个已连接节点,避免总问同一个可能视图过时的节点。
	Node* PickConnectedNode()
	{
		std::size_t connectedCount = 0;
		for (const auto& entry : nodes_)
		{
			if (IsConnected(entry.second))
			{
				++connectedCount;
			}
		}
		if (connectedCount == 0)
		{
			return nullptr;
		}
		std::size_t skip = pickCursor_++ % connectedCount;
		for (auto& entry : nodes_)
		{
			if (!IsConnected(entry.second))
			{
				continue;
			}
			if (skip == 0)
			{
				return &entry.second;
			}
			--skip;
		}
		return nullptr;
	}

	bool CanAccept(QueueIndex queue)
	{
		if (queue == kAnyNodeQueue)
		{
			return PickConnectedNode() != nullptr;
		}
		const NodeEndpoint* owner = table_.Owner(queue);
		if (owner == nullptr)
		{
			return false;
		}
		const Node* node = FindNode(*owner);
		if (node == nullptr)
		{
			return false;
		}
		// 节点还在首次握手时(新主刚上线、MOVED 刚指过来)照常排队,在预算内退避:
		// 否则一次普通的 slot 迁移也会以 REDIS_ERR 的形式露给业务。单实例只有种子一个节点,连上之前拒绝。
		return IsConnected(*node) || (options_.mode == RedisClusterMode::kCluster && !node->everConnected);
	}

	// 发送队头。不改变「队头是谁」;kExhausted / kUnavailable 由调用方决定如何收尾。
	SendOutcome SendHead(QueueIndex queue, TimePoint now)
	{
		const PendingCommandPtr* head = queues_.Head(queue);
		if (head == nullptr)
		{
			return SendOutcome::kUnavailable;
		}
		const PendingCommandPtr command = *head;
		const std::optional<NodeEndpoint> askTarget = std::exchange(command->askTarget, std::nullopt);

		Node* node = nullptr;
		if (askTarget)
		{
			node = &EnsureNode(*askTarget);
		}
		else if (queue == kAnyNodeQueue)
		{
			node = PickConnectedNode();
		}
		else if (const NodeEndpoint* owner = table_.Owner(queue))
		{
			node = FindNode(*owner);
		}
		if (node == nullptr)
		{
			// slot 暂时没有主(故障转移中的拓扑空洞)或全部节点都断了。命令没发出,在预算内等。
			return BackoffHead(queue, now);
		}
		const RedisManager::HiredisPtr& connection = node->manager->GetZoneRedis();
		if (!connection || !connection->connected())
		{
			if (node->everConnected && !askTarget)
			{
				return SendOutcome::kUnavailable;
			}
			// 还在首次握手,或 ASK 目标暂时连不上(下一次重发会回到 slot 主节点,它若还在迁移会再回 ASK)。
			return BackoffHead(queue, now);
		}
		if (askTarget)
		{
			// 同一调用栈连续写入同一连接,中间不会混进别的命令;ASKING 只对紧随其后的那条命令生效。
			const int asking = connection->formattedCommand(
				[](hiredis::Hiredis*, redisReply*) {}, kAskingCommand.data(), kAskingCommand.size(), &hiredis::Hiredis::hasNoSideEffect);
			if (asking != REDIS_OK)
			{
				return BackoffHead(queue, now);
			}
		}
		const int ret = connection->formattedCommand(
			[weak = weak_from_this(), command](hiredis::Hiredis* replyConnection, redisReply* reply)
			{
				if (const auto self = weak.lock())
				{
					self->OnCommandReply(command, replyConnection, reply);
					return;
				}
				FinishDetached(command, replyConnection, reply);
			},
			command->formatted.get(), command->formattedLength, &redis_cluster::IsNotExecutedReply);
		if (ret != REDIS_OK)
		{
			return SendOutcome::kUnavailable;
		}
		queues_.MarkInFlight(queue);
		return SendOutcome::kSent;
	}

	SendOutcome BackoffHead(QueueIndex queue, TimePoint now)
	{
		const RetryDecision decision = queues_.OnNotExecuted(queue, RetryMode::kBackoff, now);
		if (decision.kind == RetryDecision::Kind::kExhausted)
		{
			return SendOutcome::kExhausted;
		}
		ScheduleRetry(queue, decision.delay);
		return SendOutcome::kWaitingRetry;
	}

	// 发送队头,发不出去就把这个队列收尾。
	void DriveHead(QueueIndex queue, TimePoint now)
	{
		switch (SendHead(queue, now))
		{
		case SendOutcome::kSent:
		case SendOutcome::kWaitingRetry:
			return;
		case SendOutcome::kExhausted:
			NoteBudgetExhausted(queue);
			break;
		case SendOutcome::kUnavailable:
			refreshPolicy_.Request();
			break;
		}
		// 走到这里队头没有发出(或上一次已被判定未执行),确定没有副作用。
		AbortQueue(queue, /*headKnown=*/true);
	}

	void ScheduleRetry(QueueIndex queue, Duration delay)
	{
		const PendingCommandPtr* head = queues_.Head(queue);
		if (head == nullptr)
		{
			return;
		}
		// 不取消:闭包只绑 weak,对象没了或队头换了都是空操作,最迟 maxBackoff 后自行消失。
		loop_.get().runAfter(
			ToSeconds(delay),
			[weak = weak_from_this(), queue, id = (*head)->id]
			{
				if (const auto self = weak.lock())
				{
					self->OnRetryTimer(queue, id);
				}
			});
	}

	void OnRetryTimer(QueueIndex queue, uint64_t id)
	{
		if (closing_)
		{
			return;
		}
		const PendingCommandPtr* head = queues_.Head(queue);
		if (head == nullptr || (*head)->id != id || queues_.State(queue) != HeadState::kBackoff)
		{
			return;
		}
		DriveHead(queue, Clock::now());
	}

	void NoteBudgetExhausted(QueueIndex queue)
	{
		++stats_.budgetExhausted;
		const PendingCommandPtr* head = queues_.Head(queue);
		const std::string lastError = head != nullptr && (*head)->lastError
			? std::string(redis_cluster::NotExecutedErrorToken(*(*head)->lastError))
			: std::string("no reachable node");
		LOG_ERROR << "Redis cluster retry budget exhausted on queue " << queue << " (last error " << lastError
				  << "), completing the head and " << queues_.QueuedIn(queue)
				  << " queued commands with null replies (metric=redis_cluster_budget_exhausted)";
	}

	// 收尾:队头与全部排队命令按调用顺序空应答。先把队列腾空再回调,回调里重入的新命令看到的是空闲队列。
	// 与单连接断开的语义一致:后一条执行了,前一条一定已执行或已明确失败。
	void AbortQueue(QueueIndex queue, bool headKnown)
	{
		const std::vector<PendingCommandPtr> aborted = queues_.Abort(queue);
		stats_.abortedCommands += aborted.size();
		for (std::size_t i = 0; i < aborted.size(); ++i)
		{
			aborted[i]->callback(nullptr, nullptr);
			aborted[i]->known = i == 0 ? headKnown : true;
		}
	}

	void OnCommandReply(const PendingCommandPtr& command, hiredis::Hiredis* connection, redisReply* reply)
	{
		const QueueIndex queue = command->queue;
		const PendingCommandPtr* head = queues_.Head(queue);
		const bool isLiveHead = !closing_ && head != nullptr && head->get() == command.get()
			&& queues_.State(queue) == HeadState::kInFlight;
		if (!isLiveHead)
		{
			FinishDetached(command, connection, reply);
			return;
		}

		const TimePoint now = Clock::now();
		if (reply == nullptr)
		{
			// 连接断开或被释放:结果未知,绝不重放。
			++stats_.nullReplies;
			RequestRefresh(now);
			AbortQueue(queue, /*headKnown=*/false);
			return;
		}

		const RedirectPolicy redirects = options_.mode == RedisClusterMode::kCluster && queue != kAnyNodeQueue
			? RedirectPolicy::kFollow
			: RedirectPolicy::kNoFollow;
		const ReplyDisposition disposition = redis_cluster::ClassifyReply(*reply, redirects);
		if (!disposition.notExecuted)
		{
			// 先让下一条成为队头再回调:回调里重入的同 slot 命令排在它后面。回调返回后才发它,回调顺序才等于调用顺序。
			const bool promoted = queues_.CompleteHead(queue, now);
			command->callback(connection, reply);
			command->known = reply->type != REDIS_REPLY_ERROR;
			if (promoted && !closing_ && queues_.State(queue) == HeadState::kPending)
			{
				DriveHead(queue, Clock::now());
			}
			return;
		}

		command->lastError = disposition.notExecuted;
		if (disposition.refreshTopology)
		{
			RequestRefresh(now);
		}
		if (disposition.movedTo)
		{
			++stats_.movedRedirects;
			table_.ApplyMoved(queue, *disposition.movedTo);
			EnsureNode(*disposition.movedTo);
		}
		else if (disposition.askTarget)
		{
			++stats_.askRedirects;
			command->askTarget = disposition.askTarget;
		}
		else
		{
			++stats_.backoffRetries;
		}

		const RetryDecision decision = queues_.OnNotExecuted(queue, disposition.retryMode, now);
		switch (decision.kind)
		{
		case RetryDecision::Kind::kResendNow:
			DriveHead(queue, now);
			return;
		case RetryDecision::Kind::kRetryAfter:
			ScheduleRetry(queue, decision.delay);
			return;
		case RetryDecision::Kind::kExhausted:
			NoteBudgetExhausted(queue);
			AbortQueue(queue, /*headKnown=*/true);
			return;
		}
	}

	void RequestRefresh(TimePoint now)
	{
		refreshPolicy_.Request();
		StartRefreshIfDue(now);
	}

	void StartRefreshIfDue(TimePoint now)
	{
		if (options_.mode != RedisClusterMode::kCluster || closing_ || !refreshPolicy_.ShouldStart(now))
		{
			return;
		}
		Node* node = PickConnectedNode();
		if (node == nullptr)
		{
			return; // 请求留着,下一拍再试
		}
		const int ret = node->manager->GetZoneRedis()->formattedCommand(
			[weak = weak_from_this()](hiredis::Hiredis*, redisReply* reply)
			{
				if (const auto self = weak.lock())
				{
					self->OnClusterSlotsReply(reply);
				}
			},
			kClusterSlotsCommand.data(), kClusterSlotsCommand.size(), &hiredis::Hiredis::hasNoSideEffect);
		if (ret != REDIS_OK)
		{
			LOG_WARN << "Redis cluster could not send CLUSTER SLOTS (ret=" << ret << "), will retry";
			return;
		}
		refreshPolicy_.OnStarted(now);
	}

	void OnClusterSlotsReply(const redisReply* reply)
	{
		if (closing_)
		{
			return;
		}
		std::string error;
		std::optional<redis_cluster::ClusterTopology> topology;
		if (reply == nullptr)
		{
			error = "connection lost";
		}
		else if (reply->type == REDIS_REPLY_ERROR && reply->str != nullptr)
		{
			error = std::string(reply->str, reply->len);
		}
		else
		{
			topology = redis_cluster::ParseClusterSlots(*reply, error);
		}
		if (!topology)
		{
			refreshPolicy_.OnFinished(/*success=*/false);
			++stats_.topologyRefreshFailures;
			LOG_ERROR << "Redis cluster topology refresh failed: " << Loggable(error);
			return;
		}
		refreshPolicy_.OnFinished(/*success=*/true);
		ApplyTopology(*topology);
	}

	void ApplyTopology(const redis_cluster::ClusterTopology& topology)
	{
		table_.ApplyTopology(topology);
		topologyReady_ = true;
		++stats_.topologyApplied;
		for (const NodeEndpoint& master : table_.Masters())
		{
			EnsureNode(master);
		}

		// 不在表里的节点(被摘掉的主、不是主的种子、拓扑还不知道的 ASK / MOVED 目标)退场。
		// 在途命令会在 Shutdown 时空应答,不在这里同步拆:当前可能在这条连接自己的回调栈上。
		bool retired = false;
		for (auto it = nodes_.begin(); it != nodes_.end();)
		{
			if (table_.IsMaster(it->first))
			{
				++it;
				continue;
			}
			LOG_INFO << "Redis cluster node " << it->first.ToString() << " left the topology, shutting it down";
			retiredManagers_.push_back(std::move(it->second.manager));
			it = nodes_.erase(it);
			retired = true;
		}
		if (retired)
		{
			loop_.get().queueInLoop(
				[weak = weak_from_this()]
				{
					if (const auto self = weak.lock())
					{
						self->ShutdownRetired();
					}
				});
		}
	}

	void ShutdownRetired()
	{
		// 先换到局部变量:Shutdown 时在途命令的空应答回调可能重入本类。
		std::vector<std::unique_ptr<RedisManager>> retired;
		retired.swap(retiredManagers_);
		for (const std::unique_ptr<RedisManager>& manager : retired)
		{
			manager->Shutdown();
		}
	}

	void OnNodeReady(const NodeEndpoint& endpoint)
	{
		if (closing_)
		{
			return;
		}
		Node* node = FindNode(endpoint);
		if (node == nullptr)
		{
			return; // 已退场、等排队的 Shutdown
		}
		node->everConnected = true;
		node->lastSeenConnected = true;
		// 首次握手要拿拓扑;重连说明之前断过,角色可能已变。两种情况都该刷新,有限速兜着。
		RequestRefresh(Clock::now());
		const ReconnectCallback callback = nodeReadyCb_;
		if (callback)
		{
			callback(endpoint);
		}
	}

	void OnTick()
	{
		if (closing_)
		{
			return;
		}
		const TimePoint now = Clock::now();
		bool anyConnected = false;
		for (auto& entry : nodes_)
		{
			const bool nowConnected = IsConnected(entry.second);
			if (entry.second.lastSeenConnected && !nowConnected)
			{
				// 没有在途命令时的断连只有这里看得到。RedisManager 已打过断线日志,这里只负责要一次刷新。
				refreshPolicy_.Request();
			}
			entry.second.lastSeenConnected = nowConnected;
			anyConnected = anyConnected || nowConnected;
		}
		if (anyConnected)
		{
			noConnectionSince_.reset();
		}
		else if (!noConnectionSince_)
		{
			noConnectionSince_ = now;
		}
		else if (now - *noConnectionSince_ >= options_.seedFallbackDelay)
		{
			// 已知节点全连不上(比如整个集群换了主节点):把退场过的种子连回来,至少还有一条拿拓扑的路。
			for (const NodeEndpoint& seed : options_.seeds)
			{
				EnsureNode(seed);
			}
		}
		StartRefreshIfDue(now);
	}

	std::reference_wrapper<muduo::net::EventLoop> loop_;
	RedisClusterOptions options_;
	redis_cluster::SlotOwnerTable table_;
	redis_cluster::SlotCommandQueues<PendingCommandPtr> queues_;
	redis_cluster::TopologyRefreshPolicy refreshPolicy_;
	std::unordered_map<NodeEndpoint, Node, NodeEndpointHash> nodes_;
	std::vector<std::unique_ptr<RedisManager>> retiredManagers_;   // 已摘下、等排队任务 Shutdown 的节点连接
	ReconnectCallback nodeReadyCb_;
	std::shared_ptr<hiredis::Hiredis::MaintenanceState> maintenance_;
	muduo::net::TimerId tickTimerId_;
	std::optional<TimePoint> noConnectionSince_;   // 全部节点连不上是从什么时候开始的;有节点连着时为空
	bool tickTimerActive_ = false;
	bool closing_ = false;
	bool topologyReady_ = false;
	uint64_t nextCommandId_ = 1;
	std::size_t pickCursor_ = 0;
	std::mt19937_64 rng_;   // 重连抖动。只影响连接时机,不进任何业务判定
	RedisClusterStats stats_;
};

thread_local RedisClusterClient tlsPlayerRedis;

RedisClusterClient::RedisClusterClient() = default;

RedisClusterClient::~RedisClusterClient()
{
	Shutdown();
}

void RedisClusterClient::Connect(muduo::net::EventLoop* loop, RedisClusterOptions options)
{
	if (loop == nullptr)
	{
		LOG_ERROR << "RedisClusterClient::Connect called with a null loop";
		return;
	}
	loop->assertInLoopThread();
	// 旧周期放到最后收尾:它的空应答回调可能重入 Connect / Shutdown 或销毁本对象,之后不能再碰任何成员。
	const std::shared_ptr<Impl> previous = std::move(impl_);
	impl_ = std::make_shared<Impl>(*loop, std::move(options));
	impl_->SetReconnectCallback(reconnectCb_);
	impl_->Start();
	if (previous)
	{
		previous->Shutdown();
	}
}

void RedisClusterClient::Shutdown()
{
	reconnectCb_ = nullptr;
	const std::shared_ptr<Impl> current = std::move(impl_);
	impl_ = nullptr;
	if (current)
	{
		current->Shutdown();
	}
}

void RedisClusterClient::SetReconnectCallback(ReconnectCallback cb)
{
	reconnectCb_ = std::move(cb);
	if (impl_)
	{
		impl_->SetReconnectCallback(reconnectCb_);
	}
}

int RedisClusterClient::Submit(std::optional<std::string_view> routingKey, const CommandCallback& cb, const char* fmt, va_list args)
{
	if (!impl_ || !cb)
	{
		return REDIS_ERR;
	}
	// 局部引用保住 Impl:同步失败路径的日志与收尾不依赖 impl_ 成员。
	const std::shared_ptr<Impl> impl = impl_;
	return impl->Submit(routingKey, cb, fmt, args);
}

int RedisClusterClient::command(const CommandCallback& cb, muduo::StringArg fmt, ...)
{
	va_list args;
	va_start(args, fmt);
	const int ret = Submit(std::nullopt, cb, fmt.c_str(), args);
	va_end(args);
	return ret;
}

int RedisClusterClient::commandForKey(std::string_view routingKey, const CommandCallback& cb, muduo::StringArg fmt, ...)
{
	va_list args;
	va_start(args, fmt);
	const int ret = Submit(routingKey, cb, fmt.c_str(), args);
	va_end(args);
	return ret;
}

bool RedisClusterClient::connected() const
{
	return impl_ && impl_->Connected();
}

RedisClusterStats RedisClusterClient::stats() const
{
	return impl_ ? impl_->Stats() : RedisClusterStats{};
}
