#include "contrib/hiredis/Hiredis.h"

#include "muduo/base/Logging.h"
#include "muduo/net/Channel.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/SocketsOps.h"

#include <hiredis/async.h>
#include <limits>
#include <utility>

using namespace muduo;
using namespace muduo::net;
using namespace hiredis;


std::shared_ptr<Hiredis::MaintenanceState> Hiredis::ProcessMaintenance()
{
  static const auto state = std::make_shared<MaintenanceState>();
  return state;
}
bool Hiredis::MaintenanceState::Begin()
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (closed_) return false;
  if (accepted_ == std::numeric_limits<std::uint64_t>::max()) { unknown_ = true; return false; }
  ++accepted_;
  return true;
}
void Hiredis::MaintenanceState::Complete(bool known)
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (!known || completed_ == accepted_) unknown_ = true;
  if (completed_ < accepted_) ++completed_;
}
void Hiredis::MaintenanceState::Close()
{
  std::lock_guard<std::mutex> lock(mutex_);
  closed_ = true;
}
bool Hiredis::MaintenanceState::HoldLocal(bool newClient)
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (newClient && closed_) return false;
  if (localHeld_ == std::numeric_limits<std::uint64_t>::max())
  {
    localAccountingUnknown_ = true;
    unknown_ = true;
    return false;
  }
  ++localHeld_;
  return true;
}
void Hiredis::MaintenanceState::ReleaseLocal(bool joined)
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (!joined) unknown_ = true;
  if (localReleased_ == localHeld_) localAccountingUnknown_ = true;
  else ++localReleased_;
}
bool Hiredis::MaintenanceState::AllowConnect() const
{
  std::lock_guard<std::mutex> lock(mutex_);
  return !closed_ && !localAccountingUnknown_;
}
void Hiredis::MaintenanceState::MarkUnknown()
{
  std::lock_guard<std::mutex> lock(mutex_);
  unknown_ = true;
}
bool Hiredis::MaintenanceState::Joined() const
{
  std::lock_guard<std::mutex> lock(mutex_);
  return closed_ && !localAccountingUnknown_ && accepted_ == completed_ && localHeld_ == localReleased_;
}
bool Hiredis::MaintenanceState::Frontier(std::string& value) const
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (!closed_ || unknown_ || localAccountingUnknown_ || accepted_ != completed_ || localHeld_ != localReleased_) return false;
  value = "redis-command-v2:" + std::to_string(accepted_) + ":" + std::to_string(completed_)
      + ":" + std::to_string(localHeld_) + ":" + std::to_string(localReleased_);
  return true;
}

bool Hiredis::MaintenanceState::SettledFrontier(std::string& value) const
{
  std::lock_guard<std::mutex> lock(mutex_);
  if (unknown_ || localAccountingUnknown_ || accepted_ != completed_) return false;
  value = "redis-command-settled-v2:" + std::to_string(accepted_) + ":" + std::to_string(completed_);
  return true;
}

// wrapper 最后一个引用与 removeChannel 的排队引用分别登记。队列被丢弃仅释放本机引用，不能伪造成功前沿。
class Hiredis::LocalReference
{
 public:
  LocalReference(std::shared_ptr<MaintenanceState> state, bool newClient,
                 std::shared_ptr<Channel> channel = {})
    : state_(std::move(state)), channel_(std::move(channel)), held_(state_ && state_->HoldLocal(newClient)) {}
  ~LocalReference()
  {
    channel_.reset();
    if (held_) state_->ReleaseLocal(joined_);
  }
  bool held() const { return held_; }
  void Join() { channel_.reset(); joined_ = true; }
 private:
  std::shared_ptr<MaintenanceState> state_;
  std::shared_ptr<Channel> channel_;
  bool held_;
  bool joined_ = false;
};
namespace {
// native 在用户回调返回后仍要完成旧 context 的 cleanup，不能在同一 wrapper 上递归重连。
struct CallbackPhase {
  explicit CallbackPhase(std::shared_ptr<bool> activeState) : active(std::move(activeState)), previous(*this->active)
  { *this->active = true; }
  ~CallbackPhase() { *active = previous; }
  std::shared_ptr<bool> active;
  bool previous;
};
struct PendingMaintenanceCommand {
  Hiredis::CommandCallback callback;
  std::shared_ptr<Hiredis::MaintenanceState> state;
  bool admitted = false;
  bool known = false;
  ~PendingMaintenanceCommand()
  {
    // 捕获对象的析构也可能交还本机引用；必须先释放 target，再登记 terminal。
    callback = {};
    if (admitted) state->Complete(known);
  }
};
}


std::shared_ptr<Hiredis> Hiredis::Create(EventLoop* loop, const InetAddress& serverAddr, std::shared_ptr<MaintenanceState> maintenance)
{
  if (loop) loop->assertInLoopThread();
  return std::shared_ptr<Hiredis>(new Hiredis(loop, serverAddr, std::move(maintenance)), [loop](Hiredis* client)
  {
    if (loop) loop->assertInLoopThread();
    if (loop && loop->eventHandling())
    {
      // 此处裸指针是删除器接管的待销毁对象，不是回调借用的业务 this。
      // 必须保留整个对象和 socket：只延迟 Channel 注销会让 fd 被新连接复用、撞上旧 Poller 注册。
      loop->queueInLoop([client] { delete client; });
    }
    else
    {
      delete client;
    }
  });
}

Hiredis::Hiredis(EventLoop* loop, const InetAddress& serverAddr, std::shared_ptr<MaintenanceState> maintenance)
  : maintenance_(std::move(maintenance)),
    lifetime_(std::make_shared<LocalReference>(maintenance_, true)),
    loop_(loop),
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
  // 成员 callback target 随后先析构，lifetime_ 最后才释放本机登记。
  lifetime_->Join();
}

bool Hiredis::connected() const
{
  return channel_ && context_ && (context_->c.flags & REDIS_CONNECTED);
}

const char* Hiredis::errstr() const
{
  return context_ ? context_->errstr : lastError_.c_str();
}

void Hiredis::connect()
{
  assert(loop_);
  loop_->assertInLoopThread();
  // Close 与此准入同锁线性化。已关闭的新占位对象不会登记任何 native/loop 工作。
  const auto self = weak_from_this().lock();
  if (!self || *callbackActive_ || !maintenance_ || !lifetime_->held() || !maintenance_->AllowConnect()) return;
  assert(!context_);

  context_ = ::redisAsyncConnect(serverAddr_.toIp().c_str(), serverAddr_.toPort());
  if (!context_ || context_->err)
  {
    lastError_ = context_ && context_->errstr ? context_->errstr : "hiredis context allocation failed";
    if (context_) ::redisAsyncFree(context_);
    context_ = NULL;
    const auto state = maintenance_;
    CallbackPhase phase(callbackActive_);
    const ConnectCallback callback = connectCb_;
    state->MarkUnknown();
    if (callback) callback(this, REDIS_ERR);
    return;
  }

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
    loop_->assertInLoopThread();
    // 无在途命令时 disconnect 可同步触发业务回调；即使调用方在其中 reset，也须等 C 调用完全返回。
    // 析构中的 null-reply 回调可能再请求断线，此时强引用已归零、context 已在 FREEING，直接忽略。
    const auto self = weak_from_this().lock();
    if (!self) return;
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
  channel_->tie(shared_from_this());
  const std::weak_ptr<Hiredis> weakSelf = shared_from_this();
  channel_->setReadCallback([weakSelf](Timestamp time)
  {
    if (const auto self = weakSelf.lock()) self->handleRead(time);
  });
  channel_->setWriteCallback([weakSelf]
  {
    if (const auto self = weakSelf.lock()) self->handleWrite();
  });
  // Windows 的纯 HUP 不走读回调,仍须让 hiredis 读取 EOF、释放挂起命令并通知断线。
  // tie 保住一次完整事件分发；weak 回调让最后一个外部引用被释放后的同批事件失效。
  channel_->setCloseCallback([weakSelf]
  {
    if (const auto self = weakSelf.lock()) self->handleRead(Timestamp::now());
  });
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
  auto retired = std::make_shared<LocalReference>(maintenance_, false, std::move(channel_));
  loop_->queueInLoop([retired] { retired->Join(); });
}

void Hiredis::handleRead(muduo::Timestamp receiveTime)
{
  // 同批 HUP / READ / WRITE 事件可能在上一回调里已释放 context,后续事件直接失效。
  if (!context_) return;
  LOG_TRACE << "receiveTime = " << receiveTime.toString();
  // HUP 也可能是建连阶段的首个事件;与写路径一致,让连接回调重新建立 Channel。
  if (!(context_->c.flags & REDIS_CONNECTED))
  {
    removeChannel();
  }
  ::redisAsyncHandleRead(context_);
}

void Hiredis::handleWrite()
{
  // Linux 的 HUP 分支之后仍会派发同批 OUT;EOF 回调可能已清空 context。
  if (!context_) return;
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

  const auto state = maintenance_;
  CallbackPhase phase(callbackActive_);
  const ConnectCallback callback = connectCb_;
  if (status != REDIS_OK)
  {
    // native context 随回调返回自动释放；先存错误并清指针，回调后不再访问 wrapper。
    lastError_ = context_->errstr ? context_->errstr : "";
    context_ = NULL;
    state->MarkUnknown();
  }
  try { if (callback) callback(this, status); }
  catch (...) { state->MarkUnknown(); throw; }
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

  const auto state = maintenance_;
  CallbackPhase phase(callbackActive_);
  const DisconnectCallback callback = disconnectCb_;
  lastError_ = context_->errstr ? context_->errstr : "";
  // 先清旧 context；同一 wrapper 的回调内重连由 CallbackPhase 拒绝，调用方可在下一轮显式重连。
  context_ = NULL;
  if (status != REDIS_OK) state->MarkUnknown();
  try { if (callback) callback(this, status); }
  catch (...) { state->MarkUnknown(); throw; }
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
  if (!connected() || !cb) return REDIS_ERR;
  loop_->assertInLoopThread();

  // target 拷贝分配失败发生在接纳之前；失败不能留下永远不返回的 accepted 计数。
  auto pending = std::make_unique<PendingMaintenanceCommand>();
  pending->callback = cb;
  pending->state = maintenance_;
  if (!maintenance_ || !maintenance_->Begin()) return REDIS_ERR;
  pending->admitted = true;
  LOG_TRACE;
  auto* p = pending.get();
  va_list args;
  va_start(args, cmd);
  int ret = ::redisvAsyncCommand(context_, commandCallback, p, cmd.c_str(), args);
  va_end(args);
  if (ret != REDIS_OK)
  {
    // hiredis 1.2 只在返回 REDIS_OK 的路径上登记回调(deps/hiredis/async.c __redisAsyncCommand):
    // DISCONNECTING / FREEING 拒收、命令格式化失败、登记回调时 OOM,都在登记之前就返回,此后不会再拿
    // 这个 privdata 回调,释放责任回到这里,不删就泄漏。典型触发:~Hiredis 里 redisAsyncFree 先用空应答
    // 回调挂起命令、后注销 Channel,那段时间 connected() 仍为真,回调里再发的命令会撞 FREEING。
    // 唯一例外是多频道 (P)SUBSCRIBE 中途 OOM(已写进订阅字典的频道仍持有 p),但本封装不支持订阅
    // (commandCallback 每次回调后都 delete)。升级 hiredis 时必须重核这条前提。
    // Windows 与 Linux 覆盖件必须保持同样的释放语义；端口访问器按各平台的 muduo 接口适配。
    p->known = true; // 同步拒绝发生在 hiredis 接受之前，确定零副作用。
  }
  else
  {
    pending.release(); // 只有真实 native 接受后才把唯一完成责任交给应答队列。
  }
  return ret;
}

/* static */ void Hiredis::commandCallback(redisAsyncContext* ac, void* r, void* privdata)
{
  redisReply* reply = static_cast<redisReply*>(r);
  std::unique_ptr<PendingMaintenanceCommand> cb(static_cast<PendingMaintenanceCommand*>(privdata));
  // 正常分发由 Channel tie 保活，析构中的 null-reply 由析构体同步 free 保活。
  // 回调返回后只清理本地回调副本，不再通过业务指针访问 wrapper。
  const bool known = reply != nullptr && reply->type != REDIS_REPLY_ERROR;
  cb->callback(getHiredis(ac), reply);
  cb->known = known;
  // 析构在业务回调返回后计完成；空回包、服务端错误或异常永久 HOLD。
}

int Hiredis::ping()
{
  return command([](Hiredis*, redisReply* reply)
  {
    if (reply && reply->str) LOG_DEBUG << reply->str;
  }, "PING");
}
