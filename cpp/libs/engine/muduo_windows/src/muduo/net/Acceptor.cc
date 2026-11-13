// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)

// LOCAL DELTA(与 Linux 覆盖件 tools/archived/muduo_linux_overlay/muduo/net/Acceptor.cc 同一改动,两边要一起改)。
// 改的是 accept 失败之后的处理(docs/design/gate-connection-admission-control.md 的 G6、G7、G8):
//   - 原来:ENFILE / ENOBUFS / ENOMEM 在 sockets::accept 里直接 LOG_FATAL,整个进程 abort,gate 上的全部在线连接一起断;
//     EMFILE 每失败一次打两行 ERROR,监听 fd 水平触发、一直可读,于是循环空转加日志洪水;
//     让位用的空闲 fd 重新占位失败时不检查,之后每次 EMFILE 都排不掉连接,空转到进程结束。
//   - 现在:资源耗尽一律降级,不崩溃。fd 用尽时用空闲 fd 让位排掉一条(对端立刻看到断开,可以换一台重试);
//     排不掉、或者是内核缓冲 / 内存不足时,暂停监听 kAcceptPauseSeconds 再恢复,循环不空转;
//     失败日志最多每秒一行,带这一秒里攒下的次数;累计次数经 Acceptor::exhaustionCounts() 给指标出口。
//   - 编程错误(EBADF / EFAULT / EINVAL / ENOTSOCK 与不认识的 errno)仍在 sockets::accept 里 LOG_FATAL。
// 另加准入回调与拒绝时的 RST(同一设计稿的 P1-1、P1-2):
//   - 原来:TcpServer 对每条 accept 成功的连接无条件建 TcpConnection(两个 1KB 缓冲、连接表节点、poller 登记),
//     应用层要拒绝只能在连接回调里 forceClose,拒绝一条的开销与接纳一条相同,关闭走 FIN,留 60s TIME_WAIT。
//   - 现在:accept 成功之后先问准入回调,不要的连接当场设 SO_LINGER(on, 0) 再 close(发 RST),什么都不建;
//     fd 用尽时让位排掉的那一条同样以 RST 关闭。
// Windows 只是本地开发形态:空闲 socket 让位一次之后没有办法重新占位,之后一律走暂停监听。

#include "muduo/net/Acceptor.h"

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/InetAddress.h"
#include "muduo/net/SocketsOps.h"

#include <atomic>

#include <errno.h>
#include <fcntl.h>
//#include <sys/types.h>
//#include <sys/stat.h>
#include <unistd.h>

using namespace muduo;
using namespace muduo::net;

// LOCAL DELTA begin
namespace
{

// 接不进来又排不掉时,暂停监听这么久再恢复。期间新连接留在内核的全连接队列里,队列满了由内核丢 SYN、客户端重传。
// 取 100ms:比一次 TCP 重传(1s 起)短得多,恢复后不会让排队的客户端多等一轮;又足够长,耗尽持续期间每秒最多醒 10 次。
const double kAcceptPauseSeconds = 0.1;

// 失败日志的最小间隔。
const std::chrono::seconds kFailureReportInterval(1);

// 写在 loop 线程,读在任意线程(指标出口);只做累计,relaxed 即可。
std::atomic<uint64_t> g_descriptorExhaustions(0);
std::atomic<uint64_t> g_memoryExhaustions(0);
std::atomic<uint64_t> g_acceptPauses(0);

// 拒绝一条已经接进来的连接(准入回调不要它,或 fd 用尽时让位排掉的那一条):先设 SO_LINGER(on, 0) 再 close,
// 内核直接发 RST 并立刻释放这个 socket,不进 TIME_WAIT。服务端主动以 FIN 关闭要留 60s 的 TIME_WAIT,
// 稳态数量 = 60 × 拒绝速率,而被拒的量正是攻击流量的量级(nginx 的 reset_timedout_connection 是同一个理由)。
// 对端看到的是连接被重置,与断线一样走重连。只用在拒绝路径:正常关闭(TcpConnection)仍然走 FIN。
// SO_LINGER 设不上也照样关,只是退回 FIN 关闭。
void abortiveClose(int connfd)
{
  struct linger abortive;
  abortive.l_onoff = 1;
  abortive.l_linger = 0;
#ifdef __linux__
  ::setsockopt(connfd, SOL_SOCKET, SO_LINGER, &abortive, static_cast<socklen_t>(sizeof abortive));
#endif // __linux__
#ifdef WIN32
  ::setsockopt(static_cast<SOCKET>(connfd), SOL_SOCKET, SO_LINGER,
               reinterpret_cast<const char*>(&abortive), static_cast<int>(sizeof abortive));
#endif // WIN32
  sockets::close(connfd);
}

}  // namespace

Acceptor::AcceptFailure Acceptor::classifyAcceptFailure(int savedErrno)
{
  switch (savedErrno)
  {
    case EAGAIN:
#if EWOULDBLOCK != EAGAIN
    case EWOULDBLOCK:
#endif
    case EINTR:
      return AcceptFailure::kRetryLater;
    case EMFILE:
    case ENFILE:
      return AcceptFailure::kDescriptorsExhausted;
    case ENOBUFS:
    case ENOMEM:
      return AcceptFailure::kMemoryExhausted;
    default:
      // sockets::accept 只把「这一条连接没了」的 errno 放回来(ECONNABORTED、EPROTO、EPERM 与 accept(2)
      // 列出的已排队连接上的网络错误),其余 errno 在那里已经 LOG_FATAL。
      return AcceptFailure::kConnectionGone;
  }
}

Acceptor::ExhaustionCounts Acceptor::exhaustionCounts()
{
  ExhaustionCounts counts;
  counts.descriptors = g_descriptorExhaustions.load(std::memory_order_relaxed);
  counts.memory = g_memoryExhaustions.load(std::memory_order_relaxed);
  counts.pauses = g_acceptPauses.load(std::memory_order_relaxed);
  return counts;
}
// LOCAL DELTA end

Acceptor::Acceptor(EventLoop* loop, const InetAddress& listenAddr, bool reuseport)
  : loop_(loop),
    acceptSocket_(sockets::createNonblockingOrDie(listenAddr.family())),
    acceptChannel_(loop, acceptSocket_.fd()),
    listening_(false),
#ifdef __linux__
    idleFd_(::open("/dev/null", O_RDONLY | O_CLOEXEC)),
#endif // __linux__
#ifdef WIN32
	idleFd_(sockets::createNonblockingOrDie(listenAddr.family())),
#endif // WIN32
    // LOCAL DELTA: 以下四个成员
    paused_(false),
    lifeToken_(std::make_shared<bool>(true)),
    nextFailureReport_(),
    unreportedFailures_(0)
{
  assert(idleFd_ >= 0);
  acceptSocket_.setReuseAddr(true);
  acceptSocket_.setReusePort(reuseport);
  acceptSocket_.bindAddress(listenAddr);
  acceptChannel_.setReadCallback(
      std::bind(&Acceptor::handleRead, this));
}

Acceptor::~Acceptor()
{
  acceptChannel_.disableAll();
  acceptChannel_.remove();
  // LOCAL DELTA: 让位之后没能重新占位时 idleFd_ 是 -1,不去关一个无效的 fd。
  if (idleFd_ >= 0)
  {
#ifdef __linux__
    ::close(idleFd_);
#endif // __linux__
#ifdef WIN32
    ::winclosesock(idleFd_);
#endif // WIN32
  }
}

void Acceptor::listen()
{
  loop_->assertInLoopThread();
  listening_ = true;
  acceptSocket_.listen();
  acceptChannel_.enableReading();
}

void Acceptor::handleRead()
{
  loop_->assertInLoopThread();
  InetAddress peerAddr;
  //FIXME loop until no more
  // LOCAL DELTA: 上面这条 FIXME 不要照做。每次可读事件只接一条,accept 风暴就不会在一次事件里饿死同一个 loop 上
  // 已有连接的读写(等价于 Envoy 的 max_connections_to_accept_per_socket_event=1)。
  int connfd = acceptSocket_.accept(&peerAddr);
  if (connfd >= 0)
  {
    // string hostport = peerAddr.toIpPort();
    // LOG_TRACE << "Accepts of " << hostport;
    // LOCAL DELTA begin: 准入回调不要的连接当场以 RST 关掉,不交给 newConnectionCallback_(不建 TcpConnection)。
    if (admissionCallback_ && !admissionCallback_(peerAddr))
    {
      abortiveClose(connfd);
      return;
    }
    // LOCAL DELTA end
    if (newConnectionCallback_)
    {
      newConnectionCallback_(connfd, peerAddr);
    }
    else
    {
      sockets::close(connfd);
    }
  }
  else
  {
    // LOCAL DELTA begin: 原来在这里无条件 LOG_SYSERR,只对 EMFILE 让位且不检查结果。
    const int savedErrno = errno;
    switch (classifyAcceptFailure(savedErrno))
    {
      case AcceptFailure::kRetryLater:
        break;
      case AcceptFailure::kConnectionGone:
        reportAcceptFailure(savedErrno, false, "skipped the dead connection");
        break;
      case AcceptFailure::kDescriptorsExhausted:
        g_descriptorExhaustions.fetch_add(1, std::memory_order_relaxed);
        if (makeRoomForAccept())
        {
          reportAcceptFailure(savedErrno, true, "shed one pending connection");
        }
        else
        {
          pauseAccepting();
          reportAcceptFailure(savedErrno, true, "paused accepting");
        }
        break;
      case AcceptFailure::kMemoryExhausted:
        g_memoryExhaustions.fetch_add(1, std::memory_order_relaxed);
        pauseAccepting();
        reportAcceptFailure(savedErrno, true, "paused accepting");
        break;
      case AcceptFailure::kCount:
        break;
    }
    // LOCAL DELTA end
  }
}

// LOCAL DELTA begin
// fd 用尽时的让位(libev 文档 "The special problem of accept()ing when you can't"):关掉预留的空闲 fd 腾出一个名额,
// 接进排在最前的连接再立刻以 RST 关掉(abortiveClose),然后把空闲 fd 重新占上。全机级的 ENFILE 同样适用:关掉空闲 fd 也腾出一个系统级表项。
// 返回 true 表示监听 fd 不会原地空转:排掉了一条,或者上次没占回来的空闲 fd 这次占上了(名额已经松动,
// 这一条留给下一次可读事件正常接入)。返回 false 表示什么也没做成,调用方要暂停监听。
bool Acceptor::makeRoomForAccept()
{
#ifdef __linux__
  if (idleFd_ < 0)
  {
    idleFd_ = ::open("/dev/null", O_RDONLY | O_CLOEXEC);
    return idleFd_ >= 0;
  }
  ::close(idleFd_);
  const int shedFd = ::accept(acceptSocket_.fd(), NULL, NULL);
  if (shedFd >= 0)
  {
    abortiveClose(shedFd);
  }
  // 别的线程(Kafka、日志滚动)可能在这两步之间抢走刚腾出的名额,这里就会失败:记成 -1,下次进来先补占。
  idleFd_ = ::open("/dev/null", O_RDONLY | O_CLOEXEC);
  return shedFd >= 0;
#endif // __linux__
#ifdef WIN32
  // 预留的是一个空闲 socket,让位一次之后没有重新占位的办法:记成 -1,之后由调用方暂停监听。
  if (idleFd_ < 0)
  {
    return false;
  }
  ::winclosesock(idleFd_);
  idleFd_ = -1;
  const int shedFd = static_cast<int>(::accept(acceptSocket_.fd(), NULL, NULL));
  if (shedFd >= 0)
  {
    abortiveClose(shedFd);
  }
  return shedFd >= 0;
#endif // WIN32
}

// 暂停监听一小段时间再恢复。监听 fd 是水平触发的:接不进来又排不掉时它一直可读,不暂停就是循环空转。
void Acceptor::pauseAccepting()
{
  if (paused_)
  {
    return;
  }
  paused_ = true;
  g_acceptPauses.fetch_add(1, std::memory_order_relaxed);
  acceptChannel_.disableReading();
  // 回调与析构都在 loop 线程上,先查令牌再碰 this 没有竞态。
  std::weak_ptr<bool> life(lifeToken_);
  loop_->runAfter(kAcceptPauseSeconds, [this, life]()
  {
    if (life.expired())
    {
      return;
    }
    paused_ = false;
    if (listening_)
    {
      acceptChannel_.enableReading();
    }
  });
}

// 失败日志最多每 kFailureReportInterval 一行,带上这期间没打出来的次数。用单调时钟:墙钟回拨不会把日志压住。
// 连接自己没了(resourceExhausted=false)是公网上的常态,打 WARN;资源耗尽打 ERROR。
void Acceptor::reportAcceptFailure(int savedErrno, bool resourceExhausted, const char* action)
{
  ++unreportedFailures_;
  const std::chrono::steady_clock::time_point now = std::chrono::steady_clock::now();
  if (now < nextFailureReport_)
  {
    return;
  }
  if (resourceExhausted)
  {
    LOG_ERROR << "Acceptor: accept failed, errno=" << savedErrno << " (" << strerror_tl(savedErrno)
              << "), action=" << action << ", failures_since_last_report=" << unreportedFailures_;
  }
  else
  {
    LOG_WARN << "Acceptor: accept failed, errno=" << savedErrno << " (" << strerror_tl(savedErrno)
             << "), action=" << action << ", failures_since_last_report=" << unreportedFailures_;
  }
  nextFailureReport_ = now + kFailureReportInterval;
  unreportedFailures_ = 0;
}
// LOCAL DELTA end
