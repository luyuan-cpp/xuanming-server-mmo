// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)
//
// This is an internal header file, you should not include this.

// LOCAL DELTA(与 Linux 覆盖件 tools/archived/muduo_linux_overlay/muduo/net/Acceptor.h 同一改动,两边要一起改):
// accept 失败的分类、进程级计数与降级处理的声明。资源耗尽时不再崩溃,原因见 Acceptor.cc 文件头。

#ifndef MUDUO_NET_ACCEPTOR_H
#define MUDUO_NET_ACCEPTOR_H

#include <chrono>
#include <functional>
#include <memory>

#include <stdint.h>

#include "muduo/net/Channel.h"
#include "muduo/net/Socket.h"

namespace muduo
{
namespace net
{

class EventLoop;
class InetAddress;

///
/// Acceptor of incoming TCP connections.
///
class Acceptor : noncopyable
{
 public:
  typedef std::function<void (int sockfd, const InetAddress&)> NewConnectionCallback;

  // LOCAL DELTA: accept 返回 -1 之后该怎么办,只由 errno 决定(纯函数,单测见
  // cpp/tests/rpc_controller_test/acceptor_failure_policy_test.cpp)。
  enum class AcceptFailure : uint8_t
  {
    kRetryLater,            // 没有就绪连接或被信号打断:等下一次可读事件
    kConnectionGone,        // 排在最前的那条连接已经没了,内核已把它移出队列:接着接下一条
    kDescriptorsExhausted,  // fd 用尽(进程级 EMFILE、全机级 ENFILE):让位排掉一条,排不掉就暂停监听
    kMemoryExhausted,       // 内核缓冲或内存不足(ENOBUFS、ENOMEM):暂停监听
    kCount,
  };
  static AcceptFailure classifyAcceptFailure(int savedErrno);

  // LOCAL DELTA: 进程内全部 Acceptor 的累计次数,供 /metrics 出口读;任意线程可调。
  struct ExhaustionCounts
  {
    uint64_t descriptors;  // accept 因 fd 用尽失败的次数
    uint64_t memory;       // accept 因内核缓冲或内存不足失败的次数
    uint64_t pauses;       // 因此暂停监听的次数
  };
  static ExhaustionCounts exhaustionCounts();

  Acceptor(EventLoop* loop, const InetAddress& listenAddr, bool reuseport);
  ~Acceptor();

  void setNewConnectionCallback(const NewConnectionCallback& cb)
  { newConnectionCallback_ = cb; }

  void listen();

  bool listening() const { return listening_; }

  // LOCAL DELTA: 与 Linux 覆盖件保持同一接口(那边要兼容上游 v2.0.2 的旧拼写)。
  bool listenning() const { return listening(); }

 private:
  void handleRead();

  // LOCAL DELTA: 三个都只在 loop 线程上调用。
  bool makeRoomForAccept();
  void pauseAccepting();
  void reportAcceptFailure(int savedErrno, bool resourceExhausted, const char* action);

  EventLoop* loop_;
  Socket acceptSocket_;
  Channel acceptChannel_;
  NewConnectionCallback newConnectionCallback_;
  bool listening_;
  int idleFd_;

  // LOCAL DELTA
  bool paused_;
  // 恢复监听的定时器只持它的 weak:Acceptor 先析构时回调什么也不做,不需要记 TimerId 去取消。
  std::shared_ptr<bool> lifeToken_;
  // 失败日志限频:下一行最早什么时候可以打,以及这期间攒下没打的失败次数。
  std::chrono::steady_clock::time_point nextFailureReport_;
  uint64_t unreportedFailures_;
};

}  // namespace net
}  // namespace muduo

#endif  // MUDUO_NET_ACCEPTOR_H
