// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)
//
// This is a public header file, it must only include public header files.

// ---------------------------------------------------------------------------
// LOCAL OVERLAY -- 由 tools/archived/setup_dependencies.sh 在配置 muduo 之前
// 拷到 third_party/muduo-linux/muduo/net/TcpServer.h。必须与同目录 TcpServer.cc、Acceptor.h、Acceptor.cc 覆盖件一起生效。
//
// 底稿是上游 v2.0.3-1(f1fc77e)。b55b0ac(v2.0.2)的这个文件只差一处注释里的拼写(listenning),整文件覆盖两版都能编。
// 除标 LOCAL DELTA 的一处外与上游逐字一致:加 setAdmissionCallback,把准入回调转给 Acceptor
// (docs/design/gate-connection-admission-control.md P1-1)。只多一个成员函数,类的数据成员不变。
// Windows 侧同一改动在 cpp/libs/engine/muduo_windows/src/muduo/net/TcpServer.h。
//
// 升级 muduo 子模块时:用新的上游文件覆盖本文件,再逐处补回 LOCAL DELTA。
// ---------------------------------------------------------------------------

#ifndef MUDUO_NET_TCPSERVER_H
#define MUDUO_NET_TCPSERVER_H

#include "muduo/base/Atomic.h"
#include "muduo/base/Types.h"
#include "muduo/net/TcpConnection.h"

#include <map>

namespace muduo
{
namespace net
{

class Acceptor;
class EventLoop;
class EventLoopThreadPool;

///
/// TCP server, supports single-threaded and thread-pool models.
///
/// This is an interface class, so don't expose too much details.
class TcpServer : noncopyable
{
 public:
  typedef std::function<void(EventLoop*)> ThreadInitCallback;
  enum Option
  {
    kNoReusePort,
    kReusePort,
  };

  //TcpServer(EventLoop* loop, const InetAddress& listenAddr);
  TcpServer(EventLoop* loop,
            const InetAddress& listenAddr,
            const string& nameArg,
            Option option = kNoReusePort);
  ~TcpServer();  // force out-line dtor, for std::unique_ptr members.

  const string& ipPort() const { return ipPort_; }
  const string& name() const { return name_; }
  EventLoop* getLoop() const { return loop_; }

  /// Set the number of threads for handling input.
  ///
  /// Always accepts new connection in loop's thread.
  /// Must be called before @c start
  /// @param numThreads
  /// - 0 means all I/O in loop's thread, no thread will created.
  ///   this is the default value.
  /// - 1 means all I/O in another thread.
  /// - N means a thread pool with N threads, new connections
  ///   are assigned on a round-robin basis.
  void setThreadNum(int numThreads);
  void setThreadInitCallback(const ThreadInitCallback& cb)
  { threadInitCallback_ = cb; }
  /// valid after calling start()
  std::shared_ptr<EventLoopThreadPool> threadPool()
  { return threadPool_; }

  /// Starts the server if it's not listening.
  ///
  /// It's harmless to call it multiple times.
  /// Thread safe.
  void start();

  /// Set connection callback.
  /// Not thread safe.
  void setConnectionCallback(const ConnectionCallback& cb)
  { connectionCallback_ = cb; }

  /// Set message callback.
  /// Not thread safe.
  void setMessageCallback(const MessageCallback& cb)
  { messageCallback_ = cb; }

  /// Set write complete callback.
  /// Not thread safe.
  void setWriteCompleteCallback(const WriteCompleteCallback& cb)
  { writeCompleteCallback_ = cb; }

  /// LOCAL DELTA: Set admission callback (docs/design/gate-connection-admission-control.md P1-1).
  /// Asked once per accepted socket in the acceptor loop, before any per-connection
  /// object is built. Returning false closes the socket at once with a RST: no
  /// TcpConnection, no buffers, no connection map entry, no connection callback.
  /// Only for criteria that do not depend on who the peer is (total count, global
  /// rate, shutting down). Not thread safe: call it in the loop thread.
  typedef std::function<bool (const InetAddress& peerAddr)> AdmissionCallback;
  void setAdmissionCallback(const AdmissionCallback& cb);

 private:
  /// Not thread safe, but in loop
  void newConnection(int sockfd, const InetAddress& peerAddr);
  /// Thread safe.
  void removeConnection(const TcpConnectionPtr& conn);
  /// Not thread safe, but in loop
  void removeConnectionInLoop(const TcpConnectionPtr& conn);

  typedef std::map<string, TcpConnectionPtr> ConnectionMap;

  EventLoop* loop_;  // the acceptor loop
  const string ipPort_;
  const string name_;
  std::unique_ptr<Acceptor> acceptor_; // avoid revealing Acceptor
  std::shared_ptr<EventLoopThreadPool> threadPool_;
  ConnectionCallback connectionCallback_;
  MessageCallback messageCallback_;
  WriteCompleteCallback writeCompleteCallback_;
  ThreadInitCallback threadInitCallback_;
  AtomicInt32 started_;
  // always in loop thread
  int nextConnId_;
  ConnectionMap connections_;
};

}  // namespace net
}  // namespace muduo

#endif  // MUDUO_NET_TCPSERVER_H
