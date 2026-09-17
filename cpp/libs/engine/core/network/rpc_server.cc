// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)

#include "rpc_server.h"
#include "game_channel.h"

#include "muduo/base/Logging.h"

#include <google/protobuf/descriptor.h>
#include <google/protobuf/service.h>

#include "rpc_connection_event.h"
#include "node/system/node/node_util.h"   // NodeUtils::RemoveRpcSessionsBoundTo

using namespace muduo;
using namespace muduo::net;

RpcServer::RpcServer(EventLoop* loop,
                     const InetAddress& listenAddr)
    : tcpServer(loop, listenAddr, "RpcServer")
{
  tcpServer.setConnectionCallback(
      std::bind(&RpcServer::onConnection, this, _1));
//   tcpServer.setMessageCallback(
//       std::bind(&RpcServer::onMessage, this, _1, _2, _3));
}

void RpcServer::registerService(google::protobuf::Service* service)
{
  const google::protobuf::ServiceDescriptor* desc = service->GetDescriptor();
  services_[desc->full_name().data()] = service;
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
    GameChannelPtr channel(new GameChannel(conn));
    channel->SetServiceMap(&services_);
    conn->setMessageCallback(
        std::bind(&GameChannel::HandleIncomingMessage, get_pointer(channel), _1, _2, _3));
    conn->setContext(channel);
  }
  else
  {
    conn->setContext(GameChannelPtr());
    // 入站节点链路断开:摘掉所有仍引用这条连接的 RpcSession 组件。
    // RpcSession::connection 现已是 weak_ptr(§7.4 / §8.0),不摘也不会把连接对象钉住;
    // 仍要摘,是为了让路由方**立刻**看到"节点不可达"(try_get<RpcSession> 为空),
    // 而不是留一个 lock() 永远失败的空壳,等到对端 etcd 键过期(NodeTTLSeconds,默认
    // 180s)触发 DestroyEntity 才消失。消费方(player_message_utils / node_message_utils /
    // scene_handler ...)本来就按 try_get<RpcSession> 为空处理"节点不可达",与此前
    // IsConnected()==false 的分支等价;重注册时 tryRegister 先 remove 再 emplace,组件已不在也无副作用。
    // 见 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.4。
    NodeUtils::RemoveRpcSessionsBoundTo(conn);
  }
}

// void RpcServer::onMessage(const TcpConnectionPtr& conn,
//                           Buffer* buf,
//                           Timestamp time)
// {
//   RpcChannelPtr& channel = boost::any_cast<RpcChannelPtr&>(conn->getContext());
//   channel->onMessage(conn, buf, time);
// }


