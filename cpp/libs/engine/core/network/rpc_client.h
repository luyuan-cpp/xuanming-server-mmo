#pragma once
#include <functional>
#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/base/Timestamp.h"
#include "muduo/net/Buffer.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"

#include "network/rpc_channel.h"
#include "network/rpc_channel_stats.h"

// 出站节点链路:一个 muduo TcpClient(enableRetry 自动重连)+ 当前连接上的 RpcChannel。
//
// 连接生命周期约定(docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §8、AGENTS §11.7):
//  1. 连接的所有权只在 muduo(这里是 client_)手里;本类和 RpcChannel 只存 weak_ptr。
//  2. 绑进 muduo 回调的不是裸 this,而是 weak_ptr<自己>:对象没了,回调 lock() 失败即返回。
// 于是本类**不需要析构函数**去摘回调,也不需要任何"延迟关闭"兜底 —— 直接销毁就是安全的:
// ~TcpClient 见 use_count()==1 会 forceClose,之后到达的 handleClose 回调发现对象已亡就什么都不做;
// 在途调用由 ~RpcChannel 以 kChannelClosed 失败。
//
// 每次连上新建一个 RpcChannel(与入站 RpcServer 对称),断开时让在途调用以 kDisconnected 失败并丢掉它
// (docs/design/muduo-rpc-channel.md §6.1)。以前一个 channel 跨重连复用:旧连接的在途调用要靠手动清,
// 本端作为服务端延后回包时还会用新连接回旧 id,被对端新 channel 里同号的调用误认领。
class RpcClient : muduo::noncopyable, public std::enable_shared_from_this<RpcClient>
{
public:
    using ConnectionObserver = std::function<void(const muduo::net::TcpConnectionPtr&)>;
    enum class Readiness : uint8_t { kOnConnect, kAfterHandshake };

    RpcClient(muduo::net::EventLoop* loop,
        const muduo::net::InetAddress& serverAddr,
        Readiness readiness = Readiness::kOnConnect,
        std::shared_ptr<RpcMaintenanceState> maintenance = RpcChannel::ProcessMaintenance())
        : client_(loop, serverAddr, "RpcClient"),
        serverAddr_(serverAddr), readiness_(readiness), maintenance_(std::move(maintenance))
    {
        // 回调在 connect() 里绑:构造函数里还拿不到 weak_from_this()。
        client_.enableRetry();
    }

    const muduo::net::InetAddress& local_addr()const
    {
        if (client_.connection() == nullptr)
        {
            static muduo::net::InetAddress s;
            return s;
        }

        return client_.connection()->localAddress();
    }

    const muduo::net::InetAddress& peer_addr()const
    {
        if (client_.connection() == nullptr)
        {
            static muduo::net::InetAddress s;
            return s;
        }

        return client_.connection()->peerAddress();
    }

    // 只在所属 loop 线程调用；connected 表示通信连接存在，业务发送统一使用 IsReady。
    bool connected() const
    {
        const auto conn = client_.connection();
        return conn && conn->connected() && channel_ && !channel_->IsClosed() && channel_->IsBoundTo(conn);
    }

    bool IsReady() const { return ready_ && !channelsClosed_ && connected(); }

    // 来源校验必须匹配传入的那条连接，不能把替代连接上的确认借给旧连接。
    bool IsReadyOn(const muduo::net::TcpConnectionPtr& conn) const
    {
        return conn && IsReady() && channel_->IsBoundTo(conn);
    }

    // 仅通信握手模块在验证对端身份后调用。旧连接回包与关机后的晚回包不能恢复 Ready。
    bool MarkReady(const muduo::net::TcpConnectionPtr& conn)
    {
        if (channelsClosed_ || !conn || !connected() || !channel_->IsBoundTo(conn)) return false;
        ready_ = true;
        consecutiveHandshakeFailures_ = 0;
        return true;
    }

    // 仅通信握手模块在一条连接握手失败时调用,返回含本次在内的连续失败次数(握手成功时清零)。
    // 握手模块据此拉长关闭失败连接前的等待;计数跨重连保留,只描述这条链路,不承载业务状态。
    uint32_t RecordHandshakeFailure() { return ++consecutiveHandshakeFailures_; }

    // 必须在 connect() 之前调用:每次连上时把服务表拷进新的 RpcChannel。
    void registerService(google::protobuf::Service* service)
    {
        const google::protobuf::ServiceDescriptor* desc = service->GetDescriptor();
        services_[std::string(desc->full_name())] = service;
    }

    // 与 registerService 一样，必须在 connect() 之前设置；在 loop 线程同步观察 UP/DOWN。
    void setConnectionObserver(ConnectionObserver observer)
    {
        connectionObserver_ = std::move(observer);
    }

    // 链路心跳(docs/design/muduo-rpc-channel.md §21):之后每次连上新建的 channel 都带心跳,写空闲时发 HEARTBEAT,
    // 读空闲满超时就关闭连接,交给 enableRetry 重连、再走握手。与 registerService 一样必须在 connect() 之前调用。
    // 只由节点装配(NodeConnector)开启,默认关;对端(入站 RpcServer)必须同样开启。
    void setLinkHeartbeat(const RpcLinkHeartbeat& heartbeat)
    {
        heartbeat_ = heartbeat;
    }

    void connect()
    {
        // 必须由 shared_ptr 持有(make_shared)才能 weak_from_this();栈上构造直接 fail-fast,
        // 否则回调永远 lock 不到自己、悄悄变哑。
        if (weak_from_this().expired())
        {
            LOG_FATAL << "RpcClient must be owned by shared_ptr before connect()";
        }
        // muduo 会把这两个回调拷到 TcpConnection 上,并可能在本对象析构之后才调用
        // (forceClose 一律 queueInLoop)。绑 weak_ptr:对象已亡则回调什么都不做。
        std::weak_ptr<RpcClient> weakSelf = weak_from_this();
        client_.setConnectionCallback([weakSelf](const muduo::net::TcpConnectionPtr& conn)
        {
            if (const auto self = weakSelf.lock())
            {
                self->onConnection(conn);
            }
        });
        client_.setMessageCallback([weakSelf](const muduo::net::TcpConnectionPtr& conn,
                                              muduo::net::Buffer* buf, muduo::Timestamp ts)
        {
            // 只在取 channel 的这一行持有 RpcClient,派发期间不强持它 —— 这只是为了不延长 RpcClient 的寿命。
            // 在任何连接回调 / 消息回调栈上同步销毁 RpcClient 仍然禁止:muduo 自己在回调期间就持着这条连接的强引用
            // (handleRead 的 shared_from_this 临时量 + Channel 的 tie guard),~TcpClient 必然看到 use_count>1 而跳过
            // forceClose。摘节点一律 queueInLoop(rpc_channel.h ClientDoneCallback 契约 (c))。channel 由局部强引用保活到派发结束。
            RpcChannelPtr channel;
            if (const auto self = weakSelf.lock())
            {
                channel = self->channel_;
            }
            if (channel && channel->IsBoundTo(conn))
            {
                channel->OnMessage(conn, buf, ts);
                return;
            }
            buf->retrieveAll();   // 对象已亡或连接已被替换:丢弃,不让缓冲无界增长
        });
        client_.connect();
    }

    // 未就绪时异步投递 kNotSent；不缓存请求，连接恢复也不自动重放业务。
    void CallMethod(uint32_t messageId, const ::google::protobuf::Message& request, ClientDoneCallback done,
                    const RpcCallOptions& options = {})
    {
        if (!IsReady())
        {
            RpcChannel::FailNotSent(*client_.getLoop(), messageId, std::move(done), "link not ready",
                                    serverAddr_.toIpPort(), maintenance_);
            return;
        }
        channel_->CallMethod(messageId, request, std::move(done), options);
    }

    template <typename Method, typename Done>
    void CallMethod(const typename Method::Request& request, Done&& done, const RpcCallOptions& options = {})
    {
        if (!IsReady())
        {
            RpcChannel::FailNotSent(
                *client_.getLoop(), Method::kMessageId,
                [typedDone = std::forward<Done>(done)](const RpcStatus& status, const MessagePtr&) mutable
                {
                    typedDone(status, std::shared_ptr<typename Method::Response>{});
                },
                "link not ready", serverAddr_.toIpPort(), maintenance_);
            return;
        }
        channel_->CallMethod<Method>(request, std::forward<Done>(done), options);
    }

    // 单向(调用方不要应答)。未就绪时返回 false 并计数。
    bool SendRequest(uint32_t messageId, const ::google::protobuf::Message& message)
    {
        if (!IsReady())
        {
            const auto n = GetRpcChannelStats().onewayRejected.fetch_add(1, std::memory_order_relaxed) + 1;
            if (ShouldLogRpcEvent(n))
            {
                LOG_WARN << "[RpcChannel] one-way request dropped: reason=link not ready, message_id=" << messageId
                         << ", peer=" << serverAddr_.toIpPort() << ", occurrences=" << n;
            }
            return false;
        }
        return channel_->SendRequest(messageId, message);
    }

    template <typename Method>
    bool SendRequest(const typename Method::Request& request)
    {
        return SendRequest(Method::kMessageId, request);
    }

    // 节点关机阶段 0(NodeUtils::CloseAllRpcChannels):在途调用以 kChannelClosed 失败,之后拒收新调用。
    // 之后 enableRetry 重连建出的 channel 一出生就是关闭态:阶段 0 到拆除之间 loop 仍在转(至少一轮;gRPC 本地失败完成未排空时拆除还会再让,见 Node::FinalizeTeardownInLoop),
    // 不能让新连接上再登记出在途调用 —— 它们会拖到拆除之后才由 ~RpcChannel 失败。
    void CloseChannel()
    {
        ready_ = false;
        channelsClosed_ = true;
        if (channel_)
        {
            channel_->Close();
        }
    }

    // 没连接时 TcpClient::connection() 本来就返回空指针,不需要再造一个 static 哨兵。
    muduo::net::TcpConnectionPtr GetConnection() const { return client_.connection(); }

    // 当前连接上的 channel(未连接时为空)。仅通信握手、测试与关机使用；业务走上面的 Ready 门禁。
    const RpcChannelPtr& channel() const { return channel_; }

private:
    // 节点间发送缓冲的高水位保护。客户端-facing 连接早就有(gate 2MB forceClose),
    // 节点间之前**完全没有**:对端节点消费慢(卡在日志 IO / 停机半途 / 网络退化)时,
    // muduo 输出缓冲无界增长 —— scene 向 gate 的 AOI 推送是全系统最大流量路径,
    // 一个卡死的 gate 能把 scene 的内存拖到 OOM,砸掉的是整台 scene 上所有玩家。
    // 阈值取 64MB:远高于任何合法突发(enter-scene AOI 批也就 KB~MB 级),
    // 堆到这个量只能说明对端已经追不上了。断开是安全的:RpcClient::enableRetry
    // 会自动重连,节点注册状态机(health monitor + txn 超时重查)已审计过能收敛。
    static constexpr size_t kInterNodeHighWaterMark = 64 * 1024 * 1024;

    static void OnInterNodeHighWaterMark(const muduo::net::TcpConnectionPtr& conn, size_t queuedBytes)
    {
        LOG_ERROR << "Inter-node output buffer exceeded " << kInterNodeHighWaterMark
                  << " bytes (queued=" << queuedBytes << ") to peer "
                  << conn->peerAddress().toIpPort()
                  << "; peer is not draining — forcing close, auto-reconnect will follow.";
        conn->forceClose();
    }

    void onConnection(const muduo::net::TcpConnectionPtr& conn)
    {
        if (conn->connected())
        {
            ready_ = false;
            conn->setTcpNoDelay(true);
            conn->setHighWaterMarkCallback(OnInterNodeHighWaterMark, kInterNodeHighWaterMark);
            if (channel_)
            {
                // 没等到旧连接的 DOWN 就来了新连接(不应发生):旧 channel 上的在途调用先按断连失败。
                channel_->OnDisconnect();
            }
            channel_ = RpcChannel::Create(*conn->getLoop(), conn, services_, maintenance_, heartbeat_);
            if (channelsClosed_)
            {
                channel_->Close();   // 节点已进入关机阶段 0(见 CloseChannel)
            }
            else
            {
                ready_ = readiness_ == Readiness::kOnConnect;
            }
            LOG_INFO << "Connected to server at " << conn->peerAddress().toIpPort() << ".";
        }
        else
        {
            if (channel_ && channel_->IsBoundTo(conn))
            {
                ready_ = false;
                // 在途调用以 kDisconnected 失败(回调 queueInLoop,不在本回调栈上执行);channel 随之丢弃 ——
                // 本端在它上面接管(Adopt)的延后回复此后一律 lock 失败而丢弃,不会落到下一条连接上。
                channel_->OnDisconnect();
                channel_.reset();
            }
            // 断开一律 WARN,不按连接存活时长降级。这里曾有 5s「启动宽限」(kStartupGraceSeconds),把连上后 5s 内的
            // 断开降为 INFO,依据是旧 win_connect 在 loop 里阻塞 select、无条件返回 0 造成的假阳性连接。
            // d27176e04(2026-05-05)起 win_connect 非阻塞,Connector::handleWrite 以 SO_ERROR 确认连接完成后才回调 UP,
            // UP 就是真实的连接建立(握手是否完成另由节点注册流程判定),这条依据已不存在。宽限又是每条连接重新计时的:
            // 对端反复「接受后立刻关」的抖动会永远停在 INFO、告警看不到。回归测试:rpc_client_lifecycle_test 第 9 节。
            LOG_WARN << "Disconnected from server at " << conn->peerAddress().toIpPort() << ".";
        }

        if (connectionObserver_)
        {
            connectionObserver_(conn);
        }
    }

    muduo::net::TcpClient client_;
    muduo::net::InetAddress serverAddr_;
    Readiness readiness_;
    // 与 RpcChannel 使用同一维护域；生产默认进程域，独立域允许测试隔离节点关机的永久终态。
    std::shared_ptr<RpcMaintenanceState> maintenance_;
    bool ready_{false};
    uint32_t consecutiveHandshakeFailures_{0};
    RpcServiceMap services_;
    // 有值时每次连上的 channel 都带链路心跳(setLinkHeartbeat)。
    std::optional<RpcLinkHeartbeat> heartbeat_;
    bool channelsClosed_{false};
    // 声明在 client_ 之后:析构时先于 client_ 释放(~RpcChannel 在 loop 还活着时把在途调用排进队列)。
    RpcChannelPtr channel_;
    ConnectionObserver connectionObserver_;
};

using RpcClientPtr = std::shared_ptr<RpcClient>;
