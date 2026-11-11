// Copyright 2010, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.

// Author: Shuo Chen (chenshuo at chenshuo dot com)

#include "muduo/base/Logging.h"
#include "muduo/net/Channel.h"
#include "muduo/net/EventLoop.h"

#include <sstream>

#include <poll.h>

using namespace muduo;
using namespace muduo::net;

const int Channel::kNoneEvent = 0;
const int Channel::kReadEvent = POLLIN | POLLPRI;
const int Channel::kWriteEvent = POLLOUT;

Channel::Channel(EventLoop* loop, int fd__)
  : loop_(loop),
    fd_(fd__),
    events_(0),
    revents_(0),
    index_(-1),
    logHup_(true),
    tied_(false),
    eventHandling_(false),
    addedToLoop_(false)
{
#ifdef WIN32
    logHup_ = false;
#endif // WIN32
}

Channel::~Channel()
{
  assert(!eventHandling_);
  assert(!addedToLoop_);
  if (loop_->isInLoopThread())
  {
    assert(!loop_->hasChannel(this));
  }
}

void Channel::tie(const std::shared_ptr<void>& obj)
{
  tie_ = obj;
  tied_ = true;
}

void Channel::update()
{
  addedToLoop_ = true;
  loop_->updateChannel(this);
}

void Channel::remove()
{
  assert(isNoneEvent());
  addedToLoop_ = false;
  loop_->removeChannel(this);
}

void Channel::handleEvent(Timestamp receiveTime)
{
  std::shared_ptr<void> guard;
  if (tied_)
  {
    guard = tie_.lock();
    if (guard)
    {
      handleEventWithGuard(receiveTime);
    }
  }
  else
  {
    handleEventWithGuard(receiveTime);
  }
}

void Channel::handleEventWithGuard(Timestamp receiveTime)
{
  eventHandling_ = true;
  LOG_TRACE << reventsToString();
  bool hangupAsError = false;
  if ((revents_ & POLLHUP) && !(revents_ & POLLIN))
  {
    if (logHup_)
    {
      LOG_WARN << "fd = " << fd_ << " Channel::handle_event() POLLHUP";
    }
#ifdef WIN32
    // LOCAL DELTA:Windows 上挂断后只交给 closeCallback 并直接返回,是为了不在已关闭的
    // TcpConnection 上再跑 error / write 回调。这只对设了 closeCallback 的 channel 成立。
    // Connector 的 channel 只设了 write / error 回调:WSAPoll 对被拒的非阻塞 connect
    // 报 POLLHUP(通常连同 POLLERR),直接返回会让 Connector 永远等不到 handleError、
    // 停在 kConnecting,水平触发的 WSAPoll 还会每轮立即返回而空转。
    // 所以没设 closeCallback、设了 errorCallback 的 channel(本仓只有 Connector)不提前返回,
    // 挂断按错误投递(没带 POLLERR 也投递,只投一次),由 handleError 读 SO_ERROR 走 retry;
    // 只设 read / write 回调、没设 errorCallback 的 channel(例如 HupChannelContract 用例;Hiredis 早期也是):
    // WSAPoll 对纯 FIN 可能只报 POLLHUP,必须投递一次 read 让消费者读取 EOF 并清理连接。该读回调可能已释放
    // 消费者的状态,因此随后直接返回,不能再落到 write 回调。Hiredis 现在另设了 closeCallback(内部同样走读处理),走 closeCallback 那一支。
    if (closeCallback_ || !errorCallback_)
    {
      if (closeCallback_) closeCallback_();
      else if (readCallback_)
      {
        readCallback_(receiveTime);
      }
      eventHandling_ = false;
      return;
    }
    hangupAsError = true;
#else
    if (closeCallback_) closeCallback_();
#endif // WIN32
  }

  if (revents_ & POLLNVAL)
  {
    LOG_WARN << "fd = " << fd_ << " Channel::handle_event() POLLNVAL";
  }

  if ((revents_ & (POLLERR | POLLNVAL)) || hangupAsError)
  {
    if (errorCallback_) errorCallback_();
  }
  if (revents_ & (POLLIN | POLLPRI | POLLRDHUP))
  {
    if (readCallback_) readCallback_(receiveTime);
  }
  if (revents_ & POLLOUT)
  {
    if (writeCallback_) writeCallback_();
  }
  eventHandling_ = false;
}

string Channel::reventsToString() const
{
  return eventsToString(fd_, revents_);
}

string Channel::eventsToString() const
{
  return eventsToString(fd_, events_);
}

string Channel::eventsToString(int fd, int ev)
{
  std::ostringstream oss;
  oss << fd << ": ";
  if (ev & POLLIN)
    oss << "IN ";
  if (ev & POLLPRI)
    oss << "PRI ";
  if (ev & POLLOUT)
    oss << "OUT ";
  if (ev & POLLHUP)
    oss << "HUP ";
  if (ev & POLLRDHUP)
    oss << "RDHUP ";
  if (ev & POLLERR)
    oss << "ERR ";
  if (ev & POLLNVAL)
    oss << "NVAL ";

  return oss.str();
}
