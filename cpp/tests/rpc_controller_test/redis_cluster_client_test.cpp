// RedisClusterClient 接到真实连接之后的行为(docs/design/redis-player-store-cluster.md §5.6)。决策逻辑的穷举在
// redis_cluster_logic_test.cpp,这里验证它们确实接到了连接上:线上看到的命令序列、回调顺序、断连与停机的收尾、
// 回调内重入。
//
// 做法:被测客户端与若干假 Redis(muduo TcpServer)跑在同一个 loop 线程上。假 Redis 做最小 RESP 解析,按到达顺序记录命令,
// 并像真 Redis 一样严格按序应答:被压住的应答会挡住同一连接上后面的所有应答。
// 测试线程等 loop 线程一律用带截止时间的 future(单调时钟),不做无限期等待。
//
// 与 rpc_controller_test.cpp 同一工程、同一个 main;假 Redis 只存 weak 连接(AGENTS.md §11.7)。

#include <gtest/gtest.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#pragma comment(lib, "ws2_32.lib")
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <unistd.h>
#endif

#include <charconv>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <deque>
#include <functional>
#include <future>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <system_error>
#include <thread>
#include <tuple>
#include <utility>
#include <vector>

#include "muduo/base/Timestamp.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/Callbacks.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThread.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpConnection.h"
#include "muduo/net/TcpServer.h"
#include "muduo/contrib/hiredis/Hiredis.h"

#include "engine/infra/storage/redis_cluster/redis_cluster_client.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"

using namespace std::chrono_literals;

namespace
{

// 三个固定落点。金样见 redis_slot_test.cpp:HashSlot("42")=8000、HashSlot("18446744073709551615")=5255、HashSlot("1")=9842。
constexpr std::string_view kKeySlot8000 = "player:{42}:x";
constexpr std::string_view kKeySlot5255 = "player:{18446744073709551615}:x";
constexpr std::string_view kKeySlot9842 = "player:{1}:x";

constexpr std::chrono::milliseconds kSettleTimeout = 5s;   // 等「应该发生」的事
constexpr std::chrono::milliseconds kQuietWindow = 200ms;  // 断言「不该发生」的事时观察的窗口
constexpr std::chrono::milliseconds kPollInterval = 2ms;

using RespCommand = std::vector<std::string>;

// 只认 hiredis 发命令用的多条批量格式。数据不完整时返回 false 且不改 out。
bool TryParseCommand(std::string_view data, std::size_t& consumed, RespCommand& out)
{
	const auto readLength = [&data](std::size_t& pos, char prefix, std::size_t& value)
	{
		if (pos >= data.size() || data[pos] != prefix)
		{
			return false;
		}
		const std::size_t crlf = data.find("\r\n", pos + 1);
		if (crlf == std::string_view::npos)
		{
			return false;
		}
		const std::from_chars_result parsed = std::from_chars(data.data() + pos + 1, data.data() + crlf, value);
		if (parsed.ec != std::errc())
		{
			return false;
		}
		pos = crlf + 2;
		return true;
	};
	std::size_t pos = 0;
	std::size_t argc = 0;
	if (!readLength(pos, '*', argc))
	{
		return false;
	}
	RespCommand args;
	for (std::size_t i = 0; i < argc; ++i)
	{
		std::size_t length = 0;
		if (!readLength(pos, '$', length) || data.size() - pos < length + 2)
		{
			return false;
		}
		args.emplace_back(data.substr(pos, length));
		pos += length + 2;
	}
	consumed = pos;
	out = std::move(args);
	return true;
}

std::string Describe(const RespCommand& command)
{
	std::string text;
	for (const std::string& arg : command)
	{
		if (!text.empty())
		{
			text += ' ';
		}
		text += arg;
	}
	return text;
}

std::string ReplyText(const redisReply* reply)
{
	if (reply == nullptr)
	{
		return "null";
	}
	const std::string text = reply->str != nullptr ? std::string(reply->str, reply->len) : std::string();
	return reply->type == REDIS_REPLY_ERROR ? "err:" + text : text;
}

// 同 hiredis_command_lifecycle_test.cpp;各自在本 TU 的匿名命名空间里,不构成 ODR 冲突。
uint16_t FindFreeLoopbackPort()
{
#ifdef _WIN32
	const SOCKET s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	EXPECT_NE(s, INVALID_SOCKET);
	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	addr.sin_port = 0;
	EXPECT_EQ(::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)), 0);
	int len = sizeof(addr);
	EXPECT_EQ(::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len), 0);
	const uint16_t port = ntohs(addr.sin_port);
	::closesocket(s);
	return port;
#else
	const int s = ::socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	EXPECT_NE(s, -1);
	if (s == -1)
	{
		return 0;
	}
	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	addr.sin_port = 0;
	const int bindResult = ::bind(s, reinterpret_cast<sockaddr*>(&addr), sizeof(addr));
	EXPECT_EQ(bindResult, 0);
	if (bindResult != 0)
	{
		::close(s);
		return 0;
	}
	socklen_t len = sizeof(addr);
	const int nameResult = ::getsockname(s, reinterpret_cast<sockaddr*>(&addr), &len);
	EXPECT_EQ(nameResult, 0);
	if (nameResult != 0)
	{
		::close(s);
		return 0;
	}
	const uint16_t port = ntohs(addr.sin_port);
	::close(s);
	return port;
#endif
}

// 业务命令的应答脚本:返回空 = 压住不回,之后由 ReleaseReply 补上。
using BusinessHandler = std::function<std::optional<std::string>(const RespCommand&)>;

const std::string kOkReply = "$2\r\nok\r\n";

std::optional<std::string> HoldEveryReply(const RespCommand&)
{
	return std::nullopt;
}

class RedisClusterClientTest : public ::testing::Test
{
protected:
	struct FakeNode
	{
		uint16_t port = 0;
		std::unique_ptr<muduo::net::TcpServer> server;
		std::weak_ptr<muduo::net::TcpConnection> connection;   // 最近一条连接
		std::vector<RespCommand> commands;                     // 全部收到的命令,含 AUTH / CLUSTER SLOTS
		std::vector<RespCommand> recorded;                     // 去掉 AUTH / CLUSTER SLOTS,断言线上序列用
		std::deque<std::optional<std::string>> replyQueue;     // 按到达顺序的待发应答;空 = 被压住
		int connectionsUp = 0;
		int connectionsDown = 0;
		BusinessHandler businessHandler;                       // 空 = 回 kOkReply
		bool holdAuth = false;                                 // 压住 AUTH 的应答,连接一直在握手;ReleaseReply 放行
	};

	void SetUp() override
	{
		loopThread_ = std::make_unique<muduo::net::EventLoopThread>();
		loop_.emplace(*loopThread_->startLoop());
	}

	void TearDown() override
	{
		RunInLoopAndWait([this] {
			if (client_)
			{
				client_->Shutdown();
			}
		});
		// 客户端释放 socket 后先让服务端处理 EOF:不能在闭包里抢在读事件之前析构 TcpServer。
		const bool allDown = WaitUntil([this] {
			for (const std::unique_ptr<FakeNode>& node : nodes_)
			{
				if (node->connectionsUp != node->connectionsDown)
				{
					return false;
				}
			}
			return true;
		}, kSettleTimeout);
		if (!allDown)
		{
			ADD_FAILURE() << "fake Redis did not observe all client disconnects; aborting before unsafe server destruction";
			std::fflush(stdout);
			std::abort();
		}
		RunInLoopAndWait([this] {
			client_.reset();
			// 最后一刻才完成 accept 的连接,其 DOWN 回调会在 TcpServer 析构时同步跑,不能再碰正在清空的 nodes_。
			serversClosing_ = true;
			nodes_.clear();
		});
		// 再转两圈:Hiredis 工厂与 removeChannel 排队的清理跑完之后才能停 loop(AGENTS.md §11.7 先排空再 quit)。
		RunInLoopAndWait([] {});
		RunInLoopAndWait([] {});
		loopThread_.reset();
		loop_.reset();
	}

	muduo::net::EventLoop& Loop() { return loop_->get(); }

	template <typename F>
	void RunInLoopAndWait(F&& fn)
	{
		std::promise<void> done;
		std::future<void> finished = done.get_future();
		Loop().runInLoop([&] { fn(); done.set_value(); });
		if (finished.wait_for(kSettleTimeout) != std::future_status::ready)
		{
			// 不能直接 return:loop 线程上的闭包按引用捕获了本栈帧。
			ADD_FAILURE() << "the loop thread did not run the task within the deadline; aborting";
			std::fflush(stdout);
			std::abort();
		}
	}

	template <typename Pred>
	bool WaitUntil(Pred pred, std::chrono::milliseconds timeout = kSettleTimeout)
	{
		const auto deadline = std::chrono::steady_clock::now() + timeout;
		for (;;)
		{
			bool satisfied = false;
			RunInLoopAndWait([&] { satisfied = pred(); });
			if (satisfied)
			{
				return true;
			}
			if (std::chrono::steady_clock::now() >= deadline)
			{
				return false;
			}
			std::this_thread::sleep_for(kPollInterval);
		}
	}

	// 整个窗口内 pred 都成立才返回 true。
	template <typename Pred>
	bool HoldsFor(Pred pred, std::chrono::milliseconds window = kQuietWindow)
	{
		const auto deadline = std::chrono::steady_clock::now() + window;
		for (;;)
		{
			bool holds = false;
			RunInLoopAndWait([&] { holds = pred(); });
			if (!holds)
			{
				return false;
			}
			if (std::chrono::steady_clock::now() >= deadline)
			{
				return true;
			}
			std::this_thread::sleep_for(kPollInterval);
		}
	}

	std::size_t AddNode()
	{
		const uint16_t port = FindFreeLoopbackPort();
		EXPECT_NE(port, 0);
		std::size_t index = 0;
		RunInLoopAndWait([&] {
			index = nodes_.size();
			auto node = std::make_unique<FakeNode>();
			node->port = port;
			node->server = std::make_unique<muduo::net::TcpServer>(
				&Loop(), muduo::net::InetAddress(port, /*loopbackOnly=*/true), "fake_redis_" + std::to_string(index));
			// 回调捕获夹具 this:server 在 TearDown 里先于夹具销毁,属于 AGENTS.md §11.7 的同寿命例外。
			node->server->setConnectionCallback([this, index](const muduo::net::TcpConnectionPtr& connection) {
				if (serversClosing_)
				{
					return;
				}
				FakeNode& self = *nodes_[index];
				if (connection->connected())
				{
					++self.connectionsUp;
					self.connection = connection;
					self.replyQueue.clear();
				}
				else
				{
					++self.connectionsDown;
				}
			});
			node->server->setMessageCallback(
				[this, index](const muduo::net::TcpConnectionPtr& connection, muduo::net::Buffer* input, muduo::Timestamp) {
					if (!serversClosing_)
					{
						OnServerMessage(*nodes_[index], connection, input);
					}
				});
			node->server->start();
			nodes_.push_back(std::move(node));
		});
		return index;
	}

	void OnServerMessage(FakeNode& node, const muduo::net::TcpConnectionPtr& connection, muduo::net::Buffer* input)
	{
		for (;;)
		{
			std::size_t consumed = 0;
			RespCommand command;
			if (!TryParseCommand(std::string_view(input->peek(), input->readableBytes()), consumed, command))
			{
				return;
			}
			input->retrieve(consumed);
			node.commands.push_back(command);
			const std::string verb = command.empty() ? std::string() : command.front();
			if (verb == "AUTH")
			{
				if (node.holdAuth)
				{
					node.replyQueue.emplace_back(std::nullopt);
				}
				else
				{
					node.replyQueue.emplace_back("+OK\r\n");
				}
			}
			else if (verb == "CLUSTER")
			{
				node.replyQueue.emplace_back(slotsReply_);
			}
			else
			{
				node.recorded.push_back(command);
				if (verb == "ASKING")
				{
					node.replyQueue.emplace_back("+OK\r\n");
				}
				else if (verb == "PING")
				{
					node.replyQueue.emplace_back("+PONG\r\n");
				}
				else if (node.businessHandler)
				{
					node.replyQueue.push_back(node.businessHandler(command));
				}
				else
				{
					node.replyQueue.emplace_back(kOkReply);
				}
			}
			FlushReplies(node, connection);
		}
	}

	// 严格按序:队首被压住时后面的应答都等着,与真 Redis 一样。
	static void FlushReplies(FakeNode& node, const muduo::net::TcpConnectionPtr& connection)
	{
		while (!node.replyQueue.empty() && node.replyQueue.front().has_value())
		{
			connection->send(*node.replyQueue.front());
			node.replyQueue.pop_front();
		}
	}

	// 给最早一条被压住的应答填上内容。
	void ReleaseReply(std::size_t index, const std::string& reply)
	{
		RunInLoopAndWait([&] {
			FakeNode& node = *nodes_[index];
			for (std::optional<std::string>& queued : node.replyQueue)
			{
				if (!queued)
				{
					queued = reply;
					break;
				}
			}
			if (const muduo::net::TcpConnectionPtr connection = node.connection.lock())
			{
				FlushReplies(node, connection);
			}
		});
	}

	void CloseServerSide(std::size_t index)
	{
		RunInLoopAndWait([&] {
			if (const muduo::net::TcpConnectionPtr connection = nodes_[index]->connection.lock())
			{
				connection->forceClose();
			}
		});
	}

	void SetBusinessHandler(std::size_t index, BusinessHandler handler)
	{
		RunInLoopAndWait([&] { nodes_[index]->businessHandler = std::move(handler); });
	}

	// ranges:{first, last, 节点下标}。
	void SetSlots(const std::vector<std::tuple<int, int, std::size_t>>& ranges)
	{
		RunInLoopAndWait([&] {
			std::string reply = "*" + std::to_string(ranges.size()) + "\r\n";
			for (const auto& [first, last, index] : ranges)
			{
				reply += "*3\r\n:" + std::to_string(first) + "\r\n:" + std::to_string(last) + "\r\n";
				reply += "*2\r\n$9\r\n127.0.0.1\r\n:" + std::to_string(nodes_[index]->port) + "\r\n";
			}
			slotsReply_ = std::move(reply);
		});
	}

	redis_cluster::NodeEndpoint Endpoint(std::size_t index)
	{
		std::optional<redis_cluster::NodeEndpoint> endpoint;
		RunInLoopAndWait([&] { endpoint = redis_cluster::NodeEndpoint{"127.0.0.1", nodes_[index]->port}; });
		return *endpoint;
	}

	RedisClusterOptions BaseOptions(RedisClusterMode mode, const std::vector<std::size_t>& seeds)
	{
		RedisClusterOptions options;
		options.mode = mode;
		for (const std::size_t index : seeds)
		{
			options.seeds.push_back(Endpoint(index));
		}
		options.connect.reconnectIntervalSec = 30.0;       // 不测重连的用例:整个用例内不重连,结果确定
		options.connect.livenessProbeIntervalSec = 30.0;   // 用例期间不发探测 PING,线上序列只有被测命令
		options.connect.livenessTimeoutSec = 90.0;
		options.reconnectJitterSec = 0.0;
		options.retry.budget = 2s;
		options.retry.initialBackoff = 1ms;
		options.retry.maxBackoff = 20ms;
		options.topologyRefreshPeriod = std::chrono::hours(1);   // 不测周期刷新的用例:只有事件触发的刷新
		options.topologyRefreshMinInterval = 10ms;
		return options;
	}

	void StartClient(RedisClusterOptions options)
	{
		RunInLoopAndWait([&] {
			client_ = std::make_unique<RedisClusterClient>();
			client_->SetReconnectCallback([this](const redis_cluster::NodeEndpoint& endpoint) {
				readyEndpoints_.push_back(endpoint.ToString());
			});
			client_->Connect(&Loop(), std::move(options));
		});
	}

	RedisClusterClient::CommandCallback Recorder(const std::string& label)
	{
		return [this, label](hiredis::Hiredis*, redisReply* reply) {
			events_.push_back(label + ":" + ReplyText(reply));
		};
	}

	int Set(const std::string& label, std::string_view key, const std::string& value)
	{
		int ret = REDIS_ERR;
		const std::string keyText(key);
		RunInLoopAndWait([&] {
			ret = client_->command(Recorder(label), "SET %s %s", keyText.c_str(), value.c_str());
		});
		return ret;
	}

	// 拓扑到了、目标节点也连上之后才会接受。
	bool SetWhenAccepted(const std::string& label, std::string_view key, const std::string& value)
	{
		const std::string keyText(key);
		return WaitUntil([&] {
			return client_->command(Recorder(label), "SET %s %s", keyText.c_str(), value.c_str()) == REDIS_OK;
		});
	}

	std::vector<std::string> Events()
	{
		std::vector<std::string> events;
		RunInLoopAndWait([&] { events = events_; });
		return events;
	}

	bool WaitEvents(std::size_t count)
	{
		return WaitUntil([&] { return events_.size() >= count; });
	}

	// 去掉 AUTH 与 CLUSTER SLOTS 之后的线上命令序列,文本形式。
	std::vector<std::string> Wire(std::size_t index)
	{
		std::vector<std::string> wire;
		RunInLoopAndWait([&] {
			for (const RespCommand& command : nodes_[index]->recorded)
			{
				wire.push_back(Describe(command));
			}
		});
		return wire;
	}

	std::size_t WireCount(std::size_t index)
	{
		return Wire(index).size();
	}

	bool WaitWireCount(std::size_t index, std::size_t count)
	{
		return WaitUntil([&] { return nodes_[index]->recorded.size() >= count; });
	}

	bool WaitClientConnected()
	{
		return WaitUntil([&] { return client_->connected(); });
	}

	std::string SetText(std::string_view key, const std::string& value)
	{
		return "SET " + std::string(key) + " " + value;
	}

	std::unique_ptr<muduo::net::EventLoopThread> loopThread_;
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> loop_;
	std::vector<std::unique_ptr<FakeNode>> nodes_;
	bool serversClosing_ = false;
	std::string slotsReply_ = "*0\r\n";
	std::unique_ptr<RedisClusterClient> client_;
	// 只在 loop 线程读写;测试线程经 RunInLoopAndWait 取。
	std::vector<std::string> events_;
	std::vector<std::string> readyEndpoints_;
};

// ---------------------------------------------------------------------------
// 同 slot 停等
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, SameSlotIsStopAndWaitOtherSlotsProceed)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, HoldEveryReply);
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_EQ(Set("c3", kKeySlot5255, "3"), REDIS_OK);

	// c2 与 c1 同 slot:c1 没有应答,c2 不上线。c3 在别的 slot,不受影响。
	ASSERT_TRUE(WaitWireCount(node, 2));
	EXPECT_TRUE(HoldsFor([&] { return nodes_[node]->recorded.size() == 2; }));
	EXPECT_EQ(Wire(node), (std::vector<std::string>{SetText(kKeySlot8000, "1"), SetText(kKeySlot5255, "3")}));

	ReleaseReply(node, "$4\r\nokc1\r\n");
	ASSERT_TRUE(WaitWireCount(node, 3));
	EXPECT_EQ(Wire(node).back(), SetText(kKeySlot8000, "2"));

	ReleaseReply(node, "$4\r\nokc3\r\n");
	ReleaseReply(node, "$4\r\nokc2\r\n");
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:okc1", "c3:okc3", "c2:okc2"}));
}

TEST_F(RedisClusterClientTest, CallbackCanResubmitOnTheSameSlotAndKeepsOrder)
{
	const std::size_t node = AddNode();
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	const std::string key(kKeySlot8000);
	std::optional<int> nestedRet;
	RunInLoopAndWait([&] {
		ASSERT_EQ(client_->command([&, key](hiredis::Hiredis*, redisReply* reply) {
			events_.push_back("first:" + ReplyText(reply));
			// 排在已排队的 second 后面。
			nestedRet = client_->command(Recorder("third"), "SET %s %s", key.c_str(), "3");
		}, "SET %s %s", key.c_str(), "1"), REDIS_OK);
		ASSERT_EQ(client_->command(Recorder("second"), "SET %s %s", key.c_str(), "2"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Events(), (std::vector<std::string>{"first:ok", "second:ok", "third:ok"}));
	EXPECT_EQ(Wire(node), (std::vector<std::string>{SetText(kKeySlot8000, "1"), SetText(kKeySlot8000, "2"), SetText(kKeySlot8000, "3")}));
	RunInLoopAndWait([&] {
		ASSERT_TRUE(nestedRet.has_value());
		EXPECT_EQ(*nestedRet, REDIS_OK);
	});
}

// ---------------------------------------------------------------------------
// 守卫
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, GuardRejectsInStandaloneModeWithoutCallback)
{
	const std::size_t node = AddNode();
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	int calls = 0;
	const auto callback = [&calls](hiredis::Hiredis*, redisReply*) { ++calls; };
	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->command(callback, "MGET %s %s", "a:{1}", "b:{2}"), REDIS_ERR) << "跨 slot";
		EXPECT_EQ(client_->command(callback, "MGET %s %s", "a", "a"), REDIS_ERR) << "没带 tag";
		EXPECT_EQ(client_->command(callback, "PUBLISH %s %s", "channel", "message"), REDIS_ERR) << "未登记";
		EXPECT_EQ(client_->command(callback, "PING"), REDIS_ERR) << "无键命令要显式路由键";
		EXPECT_EQ(client_->command(callback, "EVAL %s 2 %s %s", "return 1", "a:{1}", "b:{2}"), REDIS_ERR) << "脚本跨 slot";
		EXPECT_EQ(client_->command(callback, "GET k %Z"), REDIS_ERR) << "格式串非法";
		EXPECT_EQ(client_->commandForKey("", callback, "PING"), REDIS_ERR) << "空路由键";
		EXPECT_EQ(client_->commandForKey("a:{1}", callback, "GET %s", "b:{2}"), REDIS_ERR) << "路由键与键不同 slot";
		EXPECT_EQ(client_->command({}, "GET %s", "a"), REDIS_ERR) << "空回调";
		EXPECT_EQ(client_->stats().rejectedByGuard, 8u);
	});
	EXPECT_TRUE(HoldsFor([&] { return nodes_[node]->recorded.empty() && calls == 0; }));
}

TEST_F(RedisClusterClientTest, AllKeysPolicyKeepsUntaggedKeysOffTheWire)
{
	const std::size_t node = AddNode();
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kStandalone, {node});
	options.hashTagPolicy = redis_cluster::HashTagPolicy::kAllKeys;
	StartClient(std::move(options));
	ASSERT_TRUE(WaitClientConnected());

	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->command(Recorder("control"), "GET %s", "world_channel_directory:zone:1:2"), REDIS_ERR);
		EXPECT_EQ(client_->stats().rejectedByGuard, 1u);
		EXPECT_EQ(client_->command(Recorder("player"), "GET %s", "player:{42}:location"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(1));
	EXPECT_EQ(WireCount(node), 1u);
	EXPECT_EQ(Wire(node).back(), "GET player:{42}:location");
}

TEST_F(RedisClusterClientTest, RoutingKeyAndScriptLoadReachTheRightNodes)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	StartClient(BaseOptions(RedisClusterMode::kCluster, {a}));
	ASSERT_TRUE(SetWhenAccepted("ready", kKeySlot9842, "ready"));
	ASSERT_TRUE(WaitEvents(1));

	// 无键的 PING 跟着路由键走:slot 9842 在 b。
	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->commandForKey(kKeySlot9842, Recorder("ping"), "PING"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Wire(b).back(), "PING");
	EXPECT_EQ(WireCount(a), 0u);

	// SCRIPT LOAD 发往任一已连接节点,恰好一次。
	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->command(Recorder("load"), "SCRIPT LOAD %s", "return 1"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(WireCount(a) + WireCount(b), 3u) << "ready + ping + load";
}

// ---------------------------------------------------------------------------
// 拓扑
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, BootstrapsFromSeedRoutesBySlotAndDropsNonMasterSeed)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	const std::size_t seed = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});

	// 同一闭包里启动并断言:闭包返回之前 loop 处理不了任何连接事件,拓扑一定还没到。
	RunInLoopAndWait([&] {
		StartClient(BaseOptions(RedisClusterMode::kCluster, {seed}));
		EXPECT_FALSE(client_->connected());
		EXPECT_EQ(client_->command(Recorder("early"), "SET %s %s", "player:{42}:x", "0"), REDIS_ERR);
	});

	ASSERT_TRUE(SetWhenAccepted("onA", kKeySlot8000, "1"));
	ASSERT_TRUE(SetWhenAccepted("onB", kKeySlot9842, "2"));
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Wire(a), (std::vector<std::string>{SetText(kKeySlot8000, "1")}));
	EXPECT_EQ(Wire(b), (std::vector<std::string>{SetText(kKeySlot9842, "2")}));
	EXPECT_TRUE(Wire(seed).empty());

	// 种子不是主节点,拓扑到达之后断开,且不再回连。
	ASSERT_TRUE(WaitUntil([&] { return nodes_[seed]->connectionsDown == 1; }));
	EXPECT_TRUE(HoldsFor([&] { return nodes_[seed]->connectionsUp == 1; }));
}

TEST_F(RedisClusterClientTest, NodeRemovedFromTopologyIsShutDown)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kCluster, {a});
	options.topologyRefreshPeriod = 20ms;
	StartClient(std::move(options));
	ASSERT_TRUE(SetWhenAccepted("onB", kKeySlot9842, "1"));
	ASSERT_TRUE(WaitEvents(1));

	SetSlots({{0, 16383, a}});
	ASSERT_TRUE(WaitUntil([&] { return nodes_[b]->connectionsDown == 1; }));
	EXPECT_TRUE(HoldsFor([&] { return nodes_[b]->connectionsUp == 1; })) << "移出拓扑的节点不再回连";

	// slot 9842 现在归 a。
	ASSERT_TRUE(SetWhenAccepted("onA", kKeySlot9842, "2"));
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Wire(a), (std::vector<std::string>{SetText(kKeySlot9842, "2")}));
}

TEST_F(RedisClusterClientTest, FallsBackToSeedsWhenEveryKnownNodeIsDown)
{
	const std::size_t seed = AddNode();
	const std::size_t a = AddNode();
	SetSlots({{0, 16383, a}});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kCluster, {seed});
	options.seedFallbackDelay = 30ms;
	StartClient(std::move(options));
	ASSERT_TRUE(SetWhenAccepted("onA", kKeySlot8000, "1"));
	ASSERT_TRUE(WaitEvents(1));
	ASSERT_TRUE(WaitUntil([&] { return nodes_[seed]->connectionsDown == 1; })) << "种子不是主节点,先退场";

	// 整个集群换了主节点。a 的重连间隔是 30s,用例内不会回来。
	SetSlots({{0, 16383, seed}});
	CloseServerSide(a);
	ASSERT_TRUE(WaitUntil([&] { return nodes_[seed]->connectionsUp == 2; }));

	ASSERT_TRUE(SetWhenAccepted("onSeed", kKeySlot8000, "2"));
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Wire(seed), (std::vector<std::string>{SetText(kKeySlot8000, "2")}));
}

TEST_F(RedisClusterClientTest, InvalidTopologyIsIgnoredAndCountedAsFailure)
{
	const std::size_t a = AddNode();
	// 端点是主机名:整个应答无效,路由表保持为空,不会去做阻塞 DNS。
	RunInLoopAndWait([&] {
		slotsReply_ = "*1\r\n*3\r\n:0\r\n:16383\r\n*2\r\n$7\r\nredis-0\r\n:6379\r\n";
	});
	StartClient(BaseOptions(RedisClusterMode::kCluster, {a}));
	ASSERT_TRUE(WaitUntil([&] { return client_->stats().topologyRefreshFailures >= 1; }));
	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->stats().topologyApplied, 0u);
		EXPECT_FALSE(client_->connected());
		EXPECT_EQ(client_->command(Recorder("x"), "SET %s %s", "player:{42}:x", "1"), REDIS_ERR);
	});

	// 失败之后按限速继续重试,服务端恢复正常就收敛。
	SetSlots({{0, 16383, a}});
	ASSERT_TRUE(SetWhenAccepted("ok", kKeySlot8000, "1"));
}

// 新主还在握手时,没有任何在途命令的 slot 也照常排队:一次普通的拓扑变化不以 REDIS_ERR 露给业务。
TEST_F(RedisClusterClientTest, IdleSlotOnHandshakingMasterQueuesInsteadOfRejecting)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	RunInLoopAndWait([&] { nodes_[b]->holdAuth = true; });
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kCluster, {a});
	options.connect.password = "cluster-test-pass";
	StartClient(std::move(options));
	ASSERT_TRUE(WaitUntil([&] { return !nodes_[b]->commands.empty(); })) << "b 的 AUTH 已到,应答被压住";

	ASSERT_EQ(Set("onB", kKeySlot9842, "1"), REDIS_OK);
	EXPECT_TRUE(HoldsFor([&] { return nodes_[b]->recorded.empty(); })) << "握手完成之前命令不上线";
	EXPECT_TRUE(Events().empty());

	ReleaseReply(b, "+OK\r\n");
	ASSERT_TRUE(WaitEvents(1));
	EXPECT_EQ(Events(), (std::vector<std::string>{"onB:ok"}));
	EXPECT_EQ(Wire(b), (std::vector<std::string>{SetText(kKeySlot9842, "1")}));
}

// ---------------------------------------------------------------------------
// 重定向
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, MovedToUnknownNodeIsFollowedAndNeverReachesBusiness)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 16383, a}});
	StartClient(BaseOptions(RedisClusterMode::kCluster, {a}));
	ASSERT_TRUE(WaitClientConnected());

	// 后面的刷新也要说 slot 8000 归 b,不然周期内表又被改回去。
	const uint16_t bPort = Endpoint(b).port;
	SetBusinessHandler(a, [bPort](const RespCommand&) -> std::optional<std::string> {
		return "-MOVED 8000 127.0.0.1:" + std::to_string(bPort) + "\r\n";
	});
	SetSlots({{0, 7999, a}, {8000, 8000, b}, {8001, 16383, a}});

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	// b 还在握手时,同 slot 的后续命令照常排队,不把这次迁移露给业务。
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);

	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:ok", "c2:ok"}));
	EXPECT_EQ(Wire(a), (std::vector<std::string>{SetText(kKeySlot8000, "1")})) << "c2 不再绕回 a";
	EXPECT_EQ(Wire(b), (std::vector<std::string>{SetText(kKeySlot8000, "1"), SetText(kKeySlot8000, "2")}));

	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->stats().movedRedirects, 1u);
	});
}

TEST_F(RedisClusterClientTest, AskSendsAskingFirstAndDoesNotUpdateTheTable)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	StartClient(BaseOptions(RedisClusterMode::kCluster, {a}));
	ASSERT_TRUE(SetWhenAccepted("ready", kKeySlot9842, "ready"));
	ASSERT_TRUE(WaitEvents(1));

	const uint16_t bPort = Endpoint(b).port;
	auto asked = std::make_shared<bool>(false);
	SetBusinessHandler(a, [bPort, asked](const RespCommand&) -> std::optional<std::string> {
		if (*asked)
		{
			return kOkReply;
		}
		*asked = true;
		return "-ASK 8000 127.0.0.1:" + std::to_string(bPort) + "\r\n";
	});

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events().back(), "c1:ok");
	EXPECT_EQ(Wire(b), (std::vector<std::string>{SetText(kKeySlot9842, "ready"), "ASKING", SetText(kKeySlot8000, "1")}));

	// ASK 只管那一次:下一条仍然先发给 a。
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Wire(a).back(), SetText(kKeySlot8000, "2"));
	EXPECT_EQ(WireCount(b), 3u);
}

// ---------------------------------------------------------------------------
// 错误语义
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, NotExecutedErrorsAreRetriedAndNeverReachBusiness)
{
	const std::size_t node = AddNode();
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	const std::vector<std::string> errors = {
		"-TRYAGAIN Multiple keys request during rehashing of slot\r\n",
		"-CLUSTERDOWN The cluster is down\r\n",
		"-NOREPLICAS Not enough good replicas to write.\r\n",
		"-LOADING Redis is loading the dataset in memory\r\n",
		"-MASTERDOWN Link with MASTER is down\r\n",
		"-BUSY Redis is busy running a script.\r\n",
		"-OOM command not allowed when used memory > 'maxmemory'.\r\n",
		"-MISCONF Redis is configured to save RDB snapshots\r\n",
		"-READONLY You can't write against a read only replica.\r\n",
		// standalone 跟随不了重定向,同样按未执行退避。
		"-MOVED 8000 127.0.0.1:1\r\n",
		"-ASK 8000 127.0.0.1:1\r\n",
	};
	std::size_t expectedEvents = 0;
	for (const std::string& error : errors)
	{
		auto failuresLeft = std::make_shared<int>(2);
		SetBusinessHandler(node, [error, failuresLeft](const RespCommand&) -> std::optional<std::string> {
			if (*failuresLeft > 0)
			{
				--*failuresLeft;
				return error;
			}
			return kOkReply;
		});
		const std::size_t wireBefore = WireCount(node);
		ASSERT_EQ(Set("c", kKeySlot8000, "1"), REDIS_OK) << error;
		++expectedEvents;
		ASSERT_TRUE(WaitEvents(expectedEvents)) << error;
		EXPECT_EQ(Events().back(), "c:ok") << error;
		EXPECT_EQ(WireCount(node) - wireBefore, 3u) << "两次未执行 + 一次成功: " << error;
	}
}

TEST_F(RedisClusterClientTest, BusinessErrorsAreDeliveredWithoutRetry)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, [](const RespCommand& command) -> std::optional<std::string> {
		return command.size() > 2 && command[2] == "1" ? "-ERR boom\r\n" : "-HANDOFF register shape\r\n";
	});
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:err:ERR boom", "c2:err:HANDOFF register shape"}));
	EXPECT_EQ(WireCount(node), 2u);
}

TEST_F(RedisClusterClientTest, BudgetExhaustionCompletesHeadAndQueuedWithNullInOrder)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, [](const RespCommand&) -> std::optional<std::string> {
		return "-LOADING Redis is loading the dataset in memory\r\n";
	});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kStandalone, {node});
	options.retry.budget = 100ms;
	StartClient(std::move(options));
	ASSERT_TRUE(WaitClientConnected());

	// 同一闭包里发完:预算很短,测试线程若卡住,第一条可能在后两条排上之前就收尾了。
	RunInLoopAndWait([&] {
		ASSERT_EQ(Set("c1", kKeySlot8000, "head"), REDIS_OK);
		ASSERT_EQ(Set("c2", kKeySlot8000, "queued"), REDIS_OK);
		ASSERT_EQ(Set("c3", kKeySlot8000, "queued"), REDIS_OK);
	});

	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:null", "c2:null", "c3:null"}));
	for (const std::string& command : Wire(node))
	{
		EXPECT_EQ(command, SetText(kKeySlot8000, "head")) << "排队的命令从未发出";
	}
	EXPECT_GE(WireCount(node), 2u) << "预算内至少重试过";
	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->stats().budgetExhausted, 1u);
		EXPECT_EQ(client_->stats().abortedCommands, 3u);
	});

	// 连接还在:队列回到空闲,新命令照常工作。
	SetBusinessHandler(node, {});
	ASSERT_EQ(Set("c4", kKeySlot8000, "after"), REDIS_OK);
	ASSERT_TRUE(WaitEvents(4));
	EXPECT_EQ(Events().back(), "c4:ok");
}

// ---------------------------------------------------------------------------
// 断连
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, DisconnectCompletesInFlightAndQueuedAndRejectsNewCommands)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, HoldEveryReply);
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_EQ(Set("c3", kKeySlot8000, "3"), REDIS_OK);
	ASSERT_TRUE(WaitWireCount(node, 1));

	CloseServerSide(node);
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:null", "c2:null", "c3:null"}));
	EXPECT_EQ(WireCount(node), 1u) << "空应答的命令绝不重放,排队的也不发";

	// 重连间隔 30s,此刻连接一定还没回来。
	EXPECT_EQ(Set("c4", kKeySlot8000, "4"), REDIS_ERR);
	RunInLoopAndWait([&] {
		EXPECT_FALSE(client_->connected());
		EXPECT_EQ(client_->stats().nullReplies, 1u);
		EXPECT_EQ(client_->stats().rejectedNotConnected, 1u);
	});
}

TEST_F(RedisClusterClientTest, DisconnectDuringBackoffCompletesTheQueue)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, [](const RespCommand&) -> std::optional<std::string> {
		return "-LOADING Redis is loading the dataset in memory\r\n";
	});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kStandalone, {node});
	options.retry.budget = 30s;
	options.retry.initialBackoff = 50ms;
	options.retry.maxBackoff = 50ms;
	StartClient(std::move(options));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_TRUE(WaitWireCount(node, 1));
	CloseServerSide(node);

	// 没有预算兜底(30s),靠重试时发现节点已断开来收尾。
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:null", "c2:null"}));
}

TEST_F(RedisClusterClientTest, ReconnectCallbackCarriesNodeIdentityAndServiceResumes)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kCluster, {a});
	options.connect.reconnectIntervalSec = 0.05;
	StartClient(std::move(options));
	ASSERT_TRUE(SetWhenAccepted("first", kKeySlot9842, "1"));
	ASSERT_TRUE(WaitEvents(1));

	const std::string aText = Endpoint(a).ToString();
	const std::string bText = Endpoint(b).ToString();
	RunInLoopAndWait([&] {
		const std::set<std::string> ready(readyEndpoints_.begin(), readyEndpoints_.end());
		EXPECT_EQ(ready, (std::set<std::string>{aText, bText}));
		readyEndpoints_.clear();
	});

	CloseServerSide(b);
	ASSERT_TRUE(WaitUntil([&] {
		return !readyEndpoints_.empty() && nodes_[b]->connectionsUp == 2;
	}));
	RunInLoopAndWait([&] {
		EXPECT_EQ(readyEndpoints_.front(), bText) << "只有重连的节点触发回调";
	});

	ASSERT_TRUE(SetWhenAccepted("second", kKeySlot9842, "2"));
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events().back(), "second:ok");
}

TEST_F(RedisClusterClientTest, PasswordIsAppliedToEveryNodeConnection)
{
	const std::size_t a = AddNode();
	const std::size_t b = AddNode();
	SetSlots({{0, 8191, a}, {8192, 16383, b}});
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kCluster, {a});
	options.connect.password = "cluster-test-pass";
	StartClient(std::move(options));
	ASSERT_TRUE(SetWhenAccepted("onB", kKeySlot9842, "1"));
	ASSERT_TRUE(WaitEvents(1));
	RunInLoopAndWait([&] {
		ASSERT_FALSE(nodes_[a]->commands.empty());
		ASSERT_FALSE(nodes_[b]->commands.empty());
		EXPECT_EQ(nodes_[a]->commands.front().front(), "AUTH");
		EXPECT_EQ(nodes_[b]->commands.front().front(), "AUTH");
	});
}

TEST_F(RedisClusterClientTest, UnreachableSeedRejectsCommandsAndShutsDownCleanly)
{
	const uint16_t unusedPort = FindFreeLoopbackPort();
	ASSERT_NE(unusedPort, 0);
	RedisClusterOptions options;
	options.mode = RedisClusterMode::kCluster;
	options.seeds.push_back(redis_cluster::NodeEndpoint{"127.0.0.1", unusedPort});
	options.seeds.push_back(redis_cluster::NodeEndpoint{"redis-0.infra.svc", 6379});   // 不是 IP 字面量,忽略
	options.connect.reconnectIntervalSec = 0.05;
	options.reconnectJitterSec = 0.0;
	options.topologyRefreshMinInterval = 10ms;
	StartClient(std::move(options));
	EXPECT_TRUE(HoldsFor([&] {
		return !client_->connected()
			&& client_->command(Recorder("x"), "SET %s %s", "player:{42}:x", "1") == REDIS_ERR;
	}, 100ms));
	EXPECT_TRUE(Events().empty());
}

// ---------------------------------------------------------------------------
// 资源上限
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, QueueLimitsReturnErrWithoutCallback)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, HoldEveryReply);
	RedisClusterOptions options = BaseOptions(RedisClusterMode::kStandalone, {node});
	options.limits.perQueue = 1;
	options.limits.total = 2;
	StartClient(std::move(options));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("inflight", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("queued", kKeySlot8000, "2"), REDIS_OK);
	EXPECT_EQ(Set("overPerQueue", kKeySlot8000, "3"), REDIS_ERR);

	ASSERT_EQ(Set("otherInflight", kKeySlot5255, "4"), REDIS_OK) << "队头不占排队额度";
	ASSERT_EQ(Set("otherQueued", kKeySlot5255, "5"), REDIS_OK);
	ASSERT_EQ(Set("thirdInflight", kKeySlot9842, "6"), REDIS_OK);
	EXPECT_EQ(Set("overTotal", kKeySlot9842, "7"), REDIS_ERR);

	RunInLoopAndWait([&] {
		EXPECT_EQ(client_->stats().rejectedQueueFull, 2u);
	});
	EXPECT_TRUE(Events().empty());
}

// ---------------------------------------------------------------------------
// 停机与回调内重入
// ---------------------------------------------------------------------------

TEST_F(RedisClusterClientTest, ShutdownCompletesInFlightAndQueuedInOrderAndRejectsNewCommands)
{
	const std::size_t node = AddNode();
	SetBusinessHandler(node, HoldEveryReply);
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	ASSERT_EQ(Set("c1", kKeySlot8000, "1"), REDIS_OK);
	ASSERT_EQ(Set("c2", kKeySlot8000, "2"), REDIS_OK);
	ASSERT_TRUE(WaitWireCount(node, 1));

	RunInLoopAndWait([&] {
		client_->Shutdown();
		EXPECT_FALSE(client_->connected());
		EXPECT_EQ(client_->command(Recorder("late"), "SET %s %s", "player:{42}:x", "3"), REDIS_ERR);
		client_->Shutdown();   // 幂等
	});
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events(), (std::vector<std::string>{"c1:null", "c2:null"}));
}

TEST_F(RedisClusterClientTest, ShutdownInsideReplyCallbackCompletesQueuedCommand)
{
	const std::size_t node = AddNode();
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {node}));
	ASSERT_TRUE(WaitClientConnected());

	const std::string key(kKeySlot8000);
	RunInLoopAndWait([&] {
		ASSERT_EQ(client_->command([this](hiredis::Hiredis*, redisReply* reply) {
			events_.push_back("first:" + ReplyText(reply));
			// 正处在连接的事件分发里,连接要延后到 pending 阶段才真正释放。
			client_->Shutdown();
		}, "SET %s %s", key.c_str(), "1"), REDIS_OK);
		ASSERT_EQ(client_->command(Recorder("second"), "SET %s %s", key.c_str(), "2"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(2));
	EXPECT_EQ(Events(), (std::vector<std::string>{"first:ok", "second:null"}));
	EXPECT_EQ(WireCount(node), 1u);
}

TEST_F(RedisClusterClientTest, DestroyingClientInsideReplyCallbackDeliversDeferredNullReplies)
{
	const std::size_t a = AddNode();
	SetBusinessHandler(a, [](const RespCommand& command) -> std::optional<std::string> {
		// 第一条(slot 8000)立即回,第二条(slot 5255)压住,让它在对象析构之后才以空应答结束。
		return command.size() > 2 && command[2] == "1" ? std::optional<std::string>(kOkReply) : std::nullopt;
	});
	StartClient(BaseOptions(RedisClusterMode::kStandalone, {a}));
	ASSERT_TRUE(WaitClientConnected());

	const std::string first(kKeySlot8000);
	const std::string second(kKeySlot5255);
	RunInLoopAndWait([&] {
		// 会回的那条先发:假 Redis 严格按序应答,被压住的应答不能挡在它前面。
		ASSERT_EQ(client_->command([this](hiredis::Hiredis*, redisReply* reply) {
			events_.push_back("first:" + ReplyText(reply));
			client_->Shutdown();
			client_.reset();
		}, "SET %s %s", first.c_str(), "1"), REDIS_OK);
		ASSERT_EQ(client_->command(Recorder("inflight"), "SET %s %s", second.c_str(), "2"), REDIS_OK);
		ASSERT_EQ(client_->command(Recorder("queued"), "SET %s %s", second.c_str(), "3"), REDIS_OK);
	});
	ASSERT_TRUE(WaitEvents(3));
	EXPECT_EQ(Events(), (std::vector<std::string>{"first:ok", "inflight:null", "queued:null"}));
}

} // namespace
