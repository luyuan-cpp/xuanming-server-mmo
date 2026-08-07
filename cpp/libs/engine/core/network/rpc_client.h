#pragma once
#include <memory>

#include "muduo/base/Logging.h"
#include "muduo/base/Timestamp.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/TcpClient.h"
#include "muduo/net/TcpConnection.h"

#include "network/game_channel.h"

#include "rpc_connection_event.h"
#include <thread_context/ecs_context.h>

class RpcClient : muduo::noncopyable
{
public:
    RpcClient(muduo::net::EventLoop* loop,
        const muduo::net::InetAddress& serverAddr)
        : client_(loop, serverAddr, "RpcClient"),
        channel_(std::make_shared<GameChannel>())
    {
        client_.setConnectionCallback(
            std::bind(&RpcClient::onConnection, this, std::placeholders::_1));
        client_.setMessageCallback(
            std::bind(&GameChannel::HandleIncomingMessage, muduo::get_pointer(channel_), std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
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

    inline bool connected()const { return connected_; }

    void registerService(google::protobuf::Service* service)
    {
        const google::protobuf::ServiceDescriptor* desc = service->GetDescriptor();
        services_[std::string(desc->full_name())] = service;
    }

    void connect()
    {
        channel_->SetServiceMap(&services_);
        client_.connect();
    }

    void CallRemoteMethod(uint32_t message_id, const ::google::protobuf::Message& request)
    {
        if (!connected_)
        {
            LOG_ERROR << "Failed to call remote method: Client is not connected to the server.";
            return;
        }

        LOG_DEBUG << "Sending request (Message ID: " << message_id << ") to remote method...";
        channel_->CallRemoteMethod(message_id, request);
    }

    void SendRequest(uint32_t message_id, const ::google::protobuf::Message& message)
    {
        if (!connected_)
        {
            LOG_ERROR << "Failed to send request: Client is not connected to the server.";
            return;
        }

        LOG_DEBUG << "Sending request (Message ID: " << message_id << ") with message size: " << message.ByteSizeLong() << " bytes.";
        channel_->SendRequest(message_id, message);
    }

    void RouteMessageToNode(uint32_t message_id, const ::google::protobuf::Message& request)
    {
        if (!connected_)
        {
            LOG_ERROR << "Failed to route message: Client is not connected to the server.";
            return;
        }

        LOG_DEBUG << "Routing message (Message ID: " << message_id << ") to node...";
        channel_->RouteMessageToNode(message_id, request);
    }

	muduo::net::TcpConnectionPtr GetConnection() const {
if (client_.connection() == nullptr)
		{
			static muduo::net::TcpConnectionPtr c;
			return c;
		}

		return client_.connection();
	}

private:
    // Disconnects within this window after the initial successful connect are
    // treated as benign startup-race noise and logged at INFO instead of WARN.
    // Typical cause on Windows: win_connect() reports the socket writable via
    // select() before the peer's TCP listen backlog is fully ready, producing
    // a "false-positive" connect that the OS reaps within ~1s. muduo's
    // enableRetry() then reconnects successfully on the very next attempt.
    static constexpr int kStartupGraceSeconds = 5;

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
            conn->setTcpNoDelay(true);
            conn->setHighWaterMarkCallback(OnInterNodeHighWaterMark, kInterNodeHighWaterMark);
            channel_->SetConnection(conn);
            connected_ = true;
            if (connectedAt_ == muduo::Timestamp::invalid())
            {
                connectedAt_ = muduo::Timestamp::now();
            }
            LOG_INFO << "Connected to server at " << conn->peerAddress().toIpPort() << ".";
        }
        else
        {
            connected_ = false;
            const double sinceConnect = (connectedAt_ == muduo::Timestamp::invalid())
                                            ? -1.0
                                            : muduo::timeDifference(muduo::Timestamp::now(), connectedAt_);
            if (sinceConnect >= 0.0 && sinceConnect < kStartupGraceSeconds)
            {
                LOG_INFO << "Disconnected from server at " << conn->peerAddress().toIpPort()
                         << " within startup grace (" << sinceConnect << "s); will auto-reconnect.";
            }
            else
            {
                LOG_WARN << "Disconnected from server at " << conn->peerAddress().toIpPort() << ".";
            }
            connectedAt_ = muduo::Timestamp::invalid();
        }

        tlsEcs.dispatcher.trigger<OnConnected2TcpServerEvent>(conn);
    }

    bool connected_{ false };
    muduo::Timestamp connectedAt_{};
    muduo::net::TcpClient client_;
    GameChannelPtr channel_;
    std::map<std::string, ::google::protobuf::Service*> services_;
};

using RpcClientPtr = std::shared_ptr<RpcClient>;
