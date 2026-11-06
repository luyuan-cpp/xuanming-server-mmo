// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)
//
// This is a public header file, it must only include public header files.

#ifndef COMMON_SRC_RPC_MESSAGE_rpc_server_H_
#define COMMON_SRC_RPC_MESSAGE_rpc_server_H_

#include <functional>
#include <map>
#include <optional>
#include <string>
#include <utility>

#include "muduo/net/TcpServer.h"
#include "network/rpc_link_liveness.h"

namespace google
{
  namespace protobuf
  {

    class Service;

  } // namespace protobuf
} // namespace google

namespace muduo
{
  namespace net
  {

    class RpcServer
    {
    public:
      using ConnectionObserver = std::function<void(const TcpConnectionPtr &)>;

      RpcServer(EventLoop *loop,
                const InetAddress &listenAddr);

      void registerService(::google::protobuf::Service *);

      // 链路心跳(docs/design/muduo-rpc-channel.md §21):之后每条入站连接的 channel 都带心跳,写空闲时发 HEARTBEAT,
      // 读空闲满超时就关闭连接(DOWN 照常摘会话)。与 registerService 一样在第一条连接到来之前设置。
      // 只由节点装配(Node::StartRpcServer)开启,默认关;对端(出站 RpcClient)必须同样开启。
      void setLinkHeartbeat(const ::RpcLinkHeartbeat &heartbeat) { heartbeat_ = heartbeat; }

      // 在 loop 线程、本类自己的 UP / DOWN 处理之后同步调用,只读观察(与 RpcClient::setConnectionObserver 同义)。
      // 生产代码不调用:测试靠它拿到入站连接上的 channel,喂合成时刻(AGENTS §11.5 的测试隔离接缝)。
      // DOWN 可能跑在 ~TcpServer 的同步栈上,回调里不得销毁本对象。在 start() 之前设置。
      void setConnectionObserver(ConnectionObserver observer) { connectionObserver_ = std::move(observer); }

      void start();

      [[nodiscard]] TcpServer &GetTcpServer()
      {
        return tcpServer;
      }

    private:
      void onConnection(const TcpConnectionPtr &conn);

      // 以下成员都声明在 tcpServer 之前:~TcpServer 会对每条连接同步执行 connectDestroyed,并以裸 this 回调 onConnection(DOWN);
      // 那时声明在它之后的成员已经析构。DOWN 分支本身不读服务表与心跳设置(每个 RpcChannel 自持一份拷贝),只读观察者。
      std::map<std::string, ::google::protobuf::Service *> services_;
      std::optional<::RpcLinkHeartbeat> heartbeat_;
      ConnectionObserver connectionObserver_;

      TcpServer tcpServer;
    };

  } // namespace net
} // namespace muduo

#endif // COMMON_SRC_RPC_MESSAGE_rpc_server_H_
