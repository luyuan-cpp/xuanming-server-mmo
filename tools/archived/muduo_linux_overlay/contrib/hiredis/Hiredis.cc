#include "contrib/hiredis/Hiredis.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Channel.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/SocketsOps.h"

#include <hiredis/async.h>

using namespace muduo;
using namespace muduo::net;
using namespace hiredis;

static void dummy(const std::shared_ptr<Channel>&)
{
}

Hiredis::Hiredis(EventLoop* loop, const InetAddress& serverAddr)
  : loop_(loop),
    serverAddr_(serverAddr),
    context_(NULL)
{
}

Hiredis::~Hiredis()
{
  LOG_DEBUG << this;
  // redisAsyncFree 先执行挂起 command 的 null-reply 回调,再通过 ev.cleanup
  // 注销 Channel。断言必须放在 free 之后,否则正常停机时活跃连接必然触发断言。
  if (context_)
  {
    ::redisAsyncFree(context_);
    context_ = NULL;
  }
  assert(!channel_ || channel_->isNoneEvent());
}

bool Hiredis::connected() const
{
  return channel_ && context_ && (context_->c.flags & REDIS_CONNECTED);
}

const char* Hiredis::errstr() const
{
  assert(context_ != NULL);
  return context_->errstr;
}

void Hiredis::connect()
{
  assert(!context_);

  context_ = ::redisAsyncConnect(serverAddr_.toIp().c_str(), serverAddr_.port());

  context_->ev.addRead = addRead;
  context_->ev.delRead = delRead;
  context_->ev.addWrite = addWrite;
  context_->ev.delWrite = delWrite;
  context_->ev.cleanup = cleanup;
  context_->ev.data = this;

  setChannel();

  assert(context_->onConnect == NULL);
  assert(context_->onDisconnect == NULL);
  ::redisAsyncSetConnectCallback(context_, connectCallback);
  ::redisAsyncSetDisconnectCallback(context_, disconnectCallback);
}

void Hiredis::disconnect()
{
  if (connected())
  {
    LOG_DEBUG << this;
    ::redisAsyncDisconnect(context_);
  }
}

int Hiredis::fd() const
{
  assert(context_);
  return context_->c.fd;
}

void Hiredis::setChannel()
{
  LOG_DEBUG << this;
  assert(!channel_);
  channel_.reset(new Channel(loop_, fd()));
  channel_->setReadCallback(std::bind(&Hiredis::handleRead, this, _1));
  channel_->setWriteCallback(std::bind(&Hiredis::handleWrite, this));
}

void Hiredis::removeChannel()
{
  LOG_DEBUG << this;
  // cleanup 与 disconnect callback 都可能走到这里;hiredis 明确允许 event
  // cleanup 重复执行,adapter 也必须幂等。
  if (!channel_)
  {
    return;
  }
  channel_->disableAll();
  channel_->remove();
  loop_->queueInLoop(std::bind(dummy, channel_));
  channel_.reset();
}

void Hiredis::handleRead(muduo::Timestamp receiveTime)
{
  LOG_TRACE << "receiveTime = " << receiveTime.toString();
  ::redisAsyncHandleRead(context_);
}

void Hiredis::handleWrite()
{
  if (!(context_->c.flags & REDIS_CONNECTED))
  {
    removeChannel();
  }
  ::redisAsyncHandleWrite(context_);
}

/* static */ Hiredis* Hiredis::getHiredis(const redisAsyncContext* ac)
{
  Hiredis* hiredis = static_cast<Hiredis*>(ac->ev.data);
  assert(hiredis->context_ == ac);
  return hiredis;
}

void Hiredis::logConnection(bool up) const
{
  InetAddress localAddr(sockets::getLocalAddr(fd()));
  InetAddress peerAddr(sockets::getPeerAddr(fd()));

  LOG_INFO << localAddr.toIpPort() << " -> "
           << peerAddr.toIpPort() << " is "
           << (up ? "UP" : "DOWN");
}

/* static */ void Hiredis::connectCallback(const redisAsyncContext* ac, int status)
{
  LOG_TRACE;
  getHiredis(ac)->connectCallback(status);
}

void Hiredis::connectCallback(int status)
{
  if (status != REDIS_OK)
  {
    LOG_ERROR << context_->errstr << " failed to connect to " << serverAddr_.toIpPort();
  }
  else
  {
    logConnection(true);
    setChannel();
  }

  if (connectCb_)
  {
    connectCb_(this, status);
  }

  // 连接失败时 hiredis 会在 connect callback 返回后自动释放 context。
  // 外部 callback 仍可在返回前读取 errstr,随后必须清空 wrapper 中的悬空指针。
  if (status != REDIS_OK)
  {
    context_ = NULL;
  }
}

/* static */ void Hiredis::disconnectCallback(const redisAsyncContext* ac, int status)
{
  LOG_TRACE;
  getHiredis(ac)->disconnectCallback(status);
}

void Hiredis::disconnectCallback(int status)
{
  logConnection(false);
  removeChannel();

  if (disconnectCb_)
  {
    disconnectCb_(this, status);
  }

  // hiredis 在 disconnect callback 返回后立即释放 redisAsyncContext;
  // 及时清空,避免下次重连析构 wrapper 时对悬空指针二次 free。
  context_ = NULL;
}

void Hiredis::addRead(void* privdata)
{
  LOG_TRACE;
  Hiredis* hiredis = static_cast<Hiredis*>(privdata);
  hiredis->channel_->enableReading();
}

void Hiredis::delRead(void* privdata)
{
  LOG_TRACE;
  Hiredis* hiredis = static_cast<Hiredis*>(privdata);
  hiredis->channel_->disableReading();
}

void Hiredis::addWrite(void* privdata)
{
  LOG_TRACE;
  Hiredis* hiredis = static_cast<Hiredis*>(privdata);
  hiredis->channel_->enableWriting();
}

void Hiredis::delWrite(void* privdata)
{
  LOG_TRACE;
  Hiredis* hiredis = static_cast<Hiredis*>(privdata);
  hiredis->channel_->disableWriting();
}

void Hiredis::cleanup(void* privdata)
{
  Hiredis* hiredis = static_cast<Hiredis*>(privdata);
  LOG_DEBUG << hiredis;
  hiredis->removeChannel();
}

int Hiredis::command(const CommandCallback& cb, muduo::StringArg cmd, ...)
{
  if (!connected()) return REDIS_ERR;

  LOG_TRACE;
  CommandCallback* p = new CommandCallback(cb);
  va_list args;
  va_start(args, cmd);
  int ret = ::redisvAsyncCommand(context_, commandCallback, p, cmd.c_str(), args);
  va_end(args);
  return ret;
}

/* static */ void Hiredis::commandCallback(redisAsyncContext* ac, void* r, void* privdata)
{
  redisReply* reply = static_cast<redisReply*>(r);
  CommandCallback* cb = static_cast<CommandCallback*>(privdata);
  getHiredis(ac)->commandCallback(reply, cb);
}

void Hiredis::commandCallback(redisReply* reply, CommandCallback* cb)
{
  (*cb)(this, reply);
  delete cb;
}

int Hiredis::ping()
{
  return command(std::bind(&Hiredis::pingCallback, this, _1, _2), "PING");
}

void Hiredis::pingCallback(Hiredis* me, redisReply* reply)
{
  assert(this == me);
  LOG_DEBUG << reply->str;
}
