#pragma once

// RedisClusterClient:C++ 的 Redis 集群路由层(docs/design/redis-player-store-cluster.md §5.6)。
// 每个 loop 线程一个(与 tlsRedis 同样 thread_local),所有方法只能在该 loop 线程调用。
// 决策逻辑都在不碰网络的纯逻辑类里:redis_command_route(解析与守卫)、redis_reply_classifier(错误归类)、
// redis_slot_queues(停等与预算)、redis_cluster_topology(CLUSTER SLOTS、slot 表、刷新节拍)。本类只负责把它们接到连接上。
//
// 模式:
// - kStandalone:一个节点,全部 slot 归它,不跑 CLUSTER SLOTS。除此之外与 cluster 同一条路径(同样的守卫、停等、
//   错误语义),本机单实例开发也能撞见「跨 slot 没有顺序保证」这类上线才会暴露的问题。
// - kCluster:每个主节点一条连接(RedisManager,带 AUTH、存活探测与重连),拓扑来自 CLUSTER SLOTS。
//
// 命令契约(command / commandForKey):
// - 形状与 hiredis::Hiredis::command 相同:返回 REDIS_OK 时回调恰好调用一次;返回 REDIS_ERR 时回调不会被调用。
// - 守卫:解析命令名和键。未登记的命令、无键且没给路由键的命令、键不同 slot 或没带 hash tag 的多键命令,当场 REDIS_ERR 并打 ERROR。
// - 顺序:同 slot 至多一条在途,后续本地排队。同 slot 的执行顺序与回调顺序都等于调用顺序;不同 slot 之间没有顺序保证。
// - 错误:MOVED / ASK / TRYAGAIN / CLUSTERDOWN、NOREPLICAS / LOADING / MASTERDOWN / BUSY / OOM / MISCONF / READONLY
//   都表示「没有执行」,路由层按预算重试,绝不以 REDIS_REPLY_ERROR 交给业务。预算耗尽按断连处理:这条与同 slot 排队的后续全部空应答。
//   其余错误应答是业务结论,原样回调。
// - 断连:在途与该节点 slot 的排队命令都空应答(在途的立刻,退避中的最迟在下一次重试时);新命令直接 REDIS_ERR。
//   空应答 = 结果未知,路由层绝不自动重放结果未知的命令。
// - 背压:每 slot 排队与全进程排队都有上限,超出返回 REDIS_ERR。
// - 回调的 Hiredis* 参数:拿到服务端应答时是那条连接;本地收尾(排队命令空应答、预算耗尽)时为 nullptr。业务不要依赖它。
// - 回调内可以重入 command / commandForKey / Shutdown / Connect。
//
// 生命周期(AGENTS.md §11.7):连接由 RedisManager 持有,本类不存连接;定时器、排队任务与命令回调只绑 weak_ptr<Impl>,
// 对象没了回调自动失效。Shutdown 必须在 EventLoop 析构前调用,幂等。

#include <muduo/contrib/hiredis/Hiredis.h>

#include <chrono>
#include <cstdarg>
#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string_view>
#include <vector>

#include "redis_command_route.h"
#include "redis_node_endpoint.h"
#include "redis_slot_queues.h"
#include "thread_context/redis_manager.h"

enum class RedisClusterMode : uint8_t
{
	kStandalone,
	kCluster,
};

struct RedisClusterOptions
{
	RedisClusterMode mode = RedisClusterMode::kStandalone;
	// 必须是 IP 字面量,调用方启动时解析一次(运行期不做阻塞 DNS)。standalone 恰好一个;cluster 至少一个,
	// 种子只用来拿拓扑,不是主节点的种子在拓扑到达后断开。
	std::vector<redis_cluster::NodeEndpoint> seeds;
	// 每条节点连接的口令、重连间隔与存活探测。重连间隔会再加上 reconnectJitterSec 以内的随机抖动。
	RedisConnectOptions connect;
	// 玩家域句柄取 kAllKeys(§5.1 的接缝规则)。
	redis_cluster::HashTagPolicy hashTagPolicy = redis_cluster::HashTagPolicy::kMultiKeyOnly;
	redis_cluster::RetryPolicy retry;
	redis_cluster::QueueLimits limits;
	redis_cluster::Duration topologyRefreshPeriod = std::chrono::seconds(30);
	redis_cluster::Duration topologyRefreshMinInterval = std::chrono::seconds(1);
	// 全部已知节点连不上持续这么久,就把种子连回来重新拿拓扑。不能一断就连:启动时种子退场之后,
	// 主节点还在握手的那一小段同样是「一个都没连上」,不该把种子又招回来。
	redis_cluster::Duration seedFallbackDelay = std::chrono::seconds(5);
	double reconnectJitterSec = 1.0;
};

// 进程启动以来的累计计数,只增不减。没有玩家维度,可直接进日志或指标。
struct RedisClusterStats
{
	uint64_t rejectedByGuard = 0;        // 守卫拒绝(含命令格式化失败)
	uint64_t rejectedNotConnected = 0;   // slot 没有主或节点未连接
	uint64_t rejectedQueueFull = 0;      // 排队上限
	uint64_t movedRedirects = 0;
	uint64_t askRedirects = 0;
	uint64_t backoffRetries = 0;         // 未执行类错误触发的退避
	uint64_t budgetExhausted = 0;        // 预算耗尽次数,每次收尾一个队列
	uint64_t nullReplies = 0;            // 在途命令收到空应答的次数
	uint64_t abortedCommands = 0;        // 本地以空应答收尾的命令总数(含队头)
	uint64_t topologyApplied = 0;
	uint64_t topologyRefreshFailures = 0;

	bool operator==(const RedisClusterStats&) const = default;
};

class RedisClusterClient
{
public:
	using CommandCallback = hiredis::Hiredis::CommandCallback;
	using ReconnectCallback = std::function<void(const redis_cluster::NodeEndpoint&)>;

	RedisClusterClient();
	~RedisClusterClient();

	RedisClusterClient(const RedisClusterClient&) = delete;
	RedisClusterClient& operator=(const RedisClusterClient&) = delete;
	RedisClusterClient(RedisClusterClient&&) = delete;
	RedisClusterClient& operator=(RedisClusterClient&&) = delete;

	// 必须在 loop 线程调用。可重复调用:新周期先建好,旧周期再收尾(旧命令按 Shutdown 的语义空应答)。
	void Connect(muduo::net::EventLoop* loop, RedisClusterOptions options);

	// 未发出的命令立刻空应答;在途命令随连接释放空应答,其后排队的命令排在它之后回调。清掉重连回调。
	void Shutdown();

	// 任一节点完成握手(首次与每次重连)时在同一调用栈同步调用,带节点身份。配了口令时此刻 AUTH 已成功。
	void SetReconnectCallback(ReconnectCallback cb);

	// 按命令自身的键路由。
	int command(const CommandCallback& cb, muduo::StringArg fmt, ...);

	// 显式指定路由键,给无键命令用(例如要与某玩家的前一条命令同 slot 的 PING)。带键的命令也可以用,
	// 但路由键必须与键同 slot。
	int commandForKey(std::string_view routingKey, const CommandCallback& cb, muduo::StringArg fmt, ...);

	// 拓扑已就绪且至少一个节点已连接。这不保证某个具体 slot 可用,那要看 command 的返回值。
	bool connected() const;

	RedisClusterStats stats() const;

private:
	class Impl;

	int Submit(std::optional<std::string_view> routingKey, const CommandCallback& cb, const char* fmt, va_list args);

	std::shared_ptr<Impl> impl_;
	ReconnectCallback reconnectCb_;
};

// 玩家域句柄(设计 §5.1、§5.6),与 tlsRedis 同样 thread_local,只在 scene 上由 RedisSystem 建连;gate、battle 不访问玩家域,
// 这里恒未连接(connected() 为假,command() 返回 REDIS_ERR)。控制域仍走 tlsRedis。
// 不放进 thread_context:本头 include 了 redis_manager.h,放过去就成了头文件与静态库的循环依赖。
extern thread_local RedisClusterClient tlsPlayerRedis;
