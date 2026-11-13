// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)
//

// ---------------------------------------------------------------------------
// LOCAL OVERLAY -- 由 tools/archived/setup_dependencies.sh 在配置 muduo 之前
// 拷到 third_party/muduo-linux/muduo/net/TcpServer.cc。
//
// 除标 LOCAL DELTA 的四处外与上游逐字一致:
//   1. newConnection 与 2. removeConnectionInLoop 的连接生命周期日志由 LOG_INFO 降为 LOG_TRACE,
//      与 Windows 两份副本(cpp/libs/engine/muduo_windows、third_party/muduo)的 52ebf9e5d 一致。
//      生产 LogLevel 是 2(INFO),每条客户端连接建立 / 断开各一行 INFO 会让连接风暴变成日志风暴,
//      抵消 gate 自己 1/1024 的采样(docs/design/gate-connection-admission-control.md G5、P0-3)。
//   3. start() 去掉 assert(!acceptor_->listenning())。gitlink 在 b55b0ac(v2.0.2,listenning)
//      与 f1fc77e(v2.0.3-1,listening)之间翻转过,两版的 TcpServer.cc 只差这个拼写;整文件覆盖
//      必须两版都能编。这条 assert 没有额外保护:started_.getAndSet(1) 已保证 listen 只排一次,
//      acceptor_ 又是私有成员,外部无法先让它进入监听。
//   4. 加 setAdmissionCallback,把准入回调转给 Acceptor(docs/design/gate-connection-admission-control.md P1-1)。
//      必须与同目录的 TcpServer.h、Acceptor.h、Acceptor.cc 覆盖件一起生效。
//
// 升级 muduo 子模块时:用新的上游文件覆盖本文件,再逐处补回 LOCAL DELTA。
// ---------------------------------------------------------------------------

#include "muduo/net/TcpServer.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Acceptor.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/EventLoopThreadPool.h"
#include "muduo/net/SocketsOps.h"

#include <stdio.h>  // snprintf

using namespace muduo;
using namespace muduo::net;

TcpServer::TcpServer(EventLoop* loop,
                     const InetAddress& listenAddr,
                     const string& nameArg,
                     Option option)
  : loop_(CHECK_NOTNULL(loop)),
    ipPort_(listenAddr.toIpPort()),
    name_(nameArg),
    acceptor_(new Acceptor(loop, listenAddr, option == kReusePort)),
    threadPool_(new EventLoopThreadPool(loop, name_)),
    connectionCallback_(defaultConnectionCallback),
    messageCallback_(defaultMessageCallback),
    nextConnId_(1)
{
  acceptor_->setNewConnectionCallback(
      std::bind(&TcpServer::newConnection, this, _1, _2));
}

TcpServer::~TcpServer()
{
  loop_->assertInLoopThread();
  LOG_TRACE << "TcpServer::~TcpServer [" << name_ << "] destructing";

  for (auto& item : connections_)
  {
    TcpConnectionPtr conn(item.second);
    item.second.reset();
    conn->getLoop()->runInLoop(
      std::bind(&TcpConnection::connectDestroyed, conn));
  }
}

void TcpServer::setThreadNum(int numThreads)
{
  assert(0 <= numThreads);
  threadPool_->setThreadNum(numThreads);
}

// LOCAL DELTA 4: 转给 Acceptor(见 Acceptor::setAdmissionCallback)。Acceptor 是内部类,不经 TcpServer 应用层够不着它。
void TcpServer::setAdmissionCallback(const AdmissionCallback& cb)
{
  loop_->assertInLoopThread();
  acceptor_->setAdmissionCallback(cb);
}

void TcpServer::start()
{
  if (started_.getAndSet(1) == 0)
  {
    threadPool_->start(threadInitCallback_);

    // LOCAL DELTA 3: 上游此处有 assert(!acceptor_->listenning()),两个 gitlink 版本拼写不同,见文件头。
    loop_->runInLoop(
        std::bind(&Acceptor::listen, get_pointer(acceptor_)));
  }
}

void TcpServer::newConnection(int sockfd, const InetAddress& peerAddr)
{
  loop_->assertInLoopThread();
  EventLoop* ioLoop = threadPool_->getNextLoop();
  char buf[64];
  snprintf(buf, sizeof buf, "-%s#%d", ipPort_.c_str(), nextConnId_);
  ++nextConnId_;
  string connName = name_ + buf;

  // LOCAL DELTA 1: 上游是 LOG_INFO,见文件头。
  LOG_TRACE << "TcpServer::newConnection [" << name_
            << "] - new connection [" << connName
            << "] from " << peerAddr.toIpPort();
  InetAddress localAddr(sockets::getLocalAddr(sockfd));
  // FIXME poll with zero timeout to double confirm the new connection
  // FIXME use make_shared if necessary
  TcpConnectionPtr conn(new TcpConnection(ioLoop,
                                          connName,
                                          sockfd,
                                          localAddr,
                                          peerAddr));
  connections_[connName] = conn;
  conn->setConnectionCallback(connectionCallback_);
  conn->setMessageCallback(messageCallback_);
  conn->setWriteCompleteCallback(writeCompleteCallback_);
  conn->setCloseCallback(
      std::bind(&TcpServer::removeConnection, this, _1)); // FIXME: unsafe
  ioLoop->runInLoop(std::bind(&TcpConnection::connectEstablished, conn));
}

void TcpServer::removeConnection(const TcpConnectionPtr& conn)
{
  // FIXME: unsafe
  loop_->runInLoop(std::bind(&TcpServer::removeConnectionInLoop, this, conn));
}

void TcpServer::removeConnectionInLoop(const TcpConnectionPtr& conn)
{
  loop_->assertInLoopThread();
  // LOCAL DELTA 2: 上游是 LOG_INFO,见文件头。
  LOG_TRACE << "TcpServer::removeConnectionInLoop [" << name_
            << "] - connection " << conn->name();
  size_t n = connections_.erase(conn->name());
  (void)n;
  assert(n == 1);
  EventLoop* ioLoop = conn->getLoop();
  ioLoop->queueInLoop(
      std::bind(&TcpConnection::connectDestroyed, conn));
}

