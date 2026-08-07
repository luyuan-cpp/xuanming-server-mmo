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
    // FIXME:
  }
}

// void RpcServer::onMessage(const TcpConnectionPtr& conn,
//                           Buffer* buf,
//                           Timestamp time)
// {
//   RpcChannelPtr& channel = boost::any_cast<RpcChannelPtr&>(conn->getContext());
//   channel->onMessage(conn, buf, time);
// }


