// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)

#include "rpc_server.h"
#include "rpc_channel.h"

#include "muduo/base/Logging.h"

#include <boost/any.hpp>
#include <google/protobuf/descriptor.h>
#include <google/protobuf/service.h>

#include "node/system/node/node_util.h"   // NodeUtils::RemoveRpcSessionsBoundTo

using namespace muduo;
using namespace muduo::net;

// 本文件在 namespace muduo::net 的 using 之下:本仓的 RpcChannel 一律写全限定的 ::RpcChannel,
// 不要被 muduo 原版 protorpc 的 muduo::net::RpcChannel(未编译,但头文件在 include 路径上)劫持。

RpcServer::RpcServer(EventLoop* loop,
                     const InetAddress& listenAddr)
    : tcpServer(loop, listenAddr, "RpcServer")
{
  tcpServer.setConnectionCallback(
      std::bind(&RpcServer::onConnection, this, _1));
}

void RpcServer::registerService(google::protobuf::Service* service)
{
  const google::protobuf::ServiceDescriptor* desc = service->GetDescriptor();
  services_[std::string(desc->full_name())] = service;
}

void RpcServer::start()
{
  tcpServer.start();
}

void RpcServer::onConnection(const TcpConnectionPtr& conn)
{
    LOG_DEBUG << "RpcServer - " << conn->peerAddress().toIpPort() << " -> "
        << conn->localAddress().toIpPort() << " is "
        << (conn->connected() ? "UP" : "DOWN");
  if (conn->connected())
  {
    // 与 RpcClient 对称的节点间高水位保护(那边的注释有完整论证):
    // 对端不消费时输出缓冲无界,64MB 说明对端已追不上,断开让它走重连,
    // 好过本进程 OOM。服务端侧断开后由对端的 enableRetry 负责恢复。
    constexpr size_t kInterNodeHighWaterMark = 64 * 1024 * 1024;
    conn->setHighWaterMarkCallback(
        [](const TcpConnectionPtr& c, size_t queuedBytes)
        {
          LOG_ERROR << "Inter-node output buffer exceeded high water mark (queued="
                    << queuedBytes << ") to peer " << c->peerAddress().toIpPort()
                    << "; peer is not draining — forcing close.";
          c->forceClose();
        },
        kInterNodeHighWaterMark);
    // 每条入站连接一个 channel,由连接的 context 持有;服务表拷进 channel(不借用本对象的成员)。
    // 节点链路开了心跳(setLinkHeartbeat)时,channel 自己按节拍检查:读空闲满超时就 forceClose,走下面的 DOWN 分支。
    ::RpcChannelPtr channel = ::RpcChannel::Create(*conn->getLoop(), conn, services_,
                                                   ::RpcChannel::ProcessMaintenance(), heartbeat_);
    // 消息回调只绑 weak(§11.7):以前绑的是裸 get_pointer(channel),安全性全靠 context 恰好还持着它。
    std::weak_ptr<::RpcChannel> weakChannel = channel;
    conn->setMessageCallback(
        [weakChannel](const TcpConnectionPtr& c, Buffer* buf, Timestamp receiveTime)
        {
          if (const auto locked = weakChannel.lock())
          {
            locked->OnMessage(c, buf, receiveTime);
            return;
          }
          buf->retrieveAll();
        });
    conn->setContext(channel);
  }
  else
  {
    // 在途调用(本端经 RpcSession 发起的)以 kDisconnected 失败;回调 queueInLoop,不在本回调栈上执行。
    // 这个分支可能跑在 ~TcpServer → connectDestroyed 的同步栈上:只碰连接自己的 context,不读本对象的成员
    // (末尾的观察者除外,它声明在 tcpServer 之前)。
    if (const auto* channel = boost::any_cast<::RpcChannelPtr>(&conn->getContext()); channel != nullptr && *channel)
    {
      (*channel)->OnDisconnect();
    }
    conn->setContext(::RpcChannelPtr());
    // 入站节点链路断开:摘掉所有仍引用这条连接的 RpcSession 组件。
    // RpcSession 只存 weak,不摘也不会把连接或 channel 钉住;仍要摘,是为了让路由方**立刻**看到"节点不可达"
    // (try_get<RpcSession> 为空),而不是留一个 lock() 永远失败的空壳,等到对端 etcd 键过期(NodeTTLSeconds,
    // 默认 180s)触发 DestroyEntity 才消失。消费方(client_message_sender / scene_handler ...)本来就按
    // try_get<RpcSession> 为空处理"节点不可达";重注册时 tryRegister 先 remove 再 emplace,组件已不在也无副作用。
    // 见 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.4。
    NodeUtils::RemoveRpcSessionsBoundTo(conn);
  }

  // 观察者声明在 tcpServer 之前,~TcpServer 同步回调 DOWN 时它仍然有效。
  if (connectionObserver_)
  {
    connectionObserver_(conn);
  }
}
