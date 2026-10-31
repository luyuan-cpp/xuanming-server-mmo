#ifndef MUDUO_CONTRIB_HIREDIS_HIREDIS_H
#define MUDUO_CONTRIB_HIREDIS_HIREDIS_H

#include "muduo/base/noncopyable.h"
#include "muduo/base/StringPiece.h"
#include "muduo/base/Types.h"
#include "muduo/net/Callbacks.h"
#include "muduo/net/InetAddress.h"

#include <hiredis/hiredis.h>
#include <memory>
#include <mutex>
#include <cstdint>
#include <string>

struct redisAsyncContext;

namespace muduo
{
namespace net
{
class Channel;
class EventLoop;
}
}

namespace hiredis
{

class Hiredis : public std::enable_shared_from_this<Hiredis>,
                muduo::noncopyable
{
  class LocalReference;
 public:
  // 命令关闭与接受共用锁；正常回复且业务回调返回后才计完成。跨连接销毁保留 UNKNOWN。
  class MaintenanceState {
   public:
    bool Begin();
    void Complete(bool known);
    void Close();
    // 只证明原 loop 上命令回调、wrapper 和排队 Channel 引用均已释放；不消除 Redis UNKNOWN。
    bool Joined() const;
    bool Frontier(std::string& value) const;
    // P1 当前已接收命令均有确定结果；保留连接和 relay 准入。
    bool SettledFrontier(std::string& value) const;
   private:
    friend class Hiredis;
    friend class LocalReference;
    bool HoldLocal(bool newClient);
    void ReleaseLocal(bool joined);
    bool AllowConnect() const;
    void MarkUnknown();
    mutable std::mutex mutex_;
    std::uint64_t accepted_ = 0;
    std::uint64_t completed_ = 0;
    std::uint64_t localHeld_ = 0;
    std::uint64_t localReleased_ = 0;
    bool localAccountingUnknown_ = false;
    bool closed_ = false;
    bool unknown_ = false;
  };
  static std::shared_ptr<MaintenanceState> ProcessMaintenance();

  typedef std::function<void(Hiredis*, int)> ConnectCallback;
  typedef std::function<void(Hiredis*, int)> DisconnectCallback;
  typedef std::function<void(Hiredis*, redisReply*)> CommandCallback;

  // 只能由工厂创建。所有操作和最后一个强引用的释放必须在 loop 线程，且 loop 必须仍然存活。
  // I/O 分发期间释放最后一个引用时，整个对象、context 和 fd 统一延至 pending 阶段销毁。
  // 空 loop 只用于从不 connect 的不可用占位对象。
  static std::shared_ptr<Hiredis> Create(muduo::net::EventLoop* loop,
                                       const muduo::net::InetAddress& serverAddr,
                                       std::shared_ptr<MaintenanceState> maintenance = ProcessMaintenance());
  ~Hiredis();

  const muduo::net::InetAddress& serverAddress() const { return serverAddr_; }
  // redisAsyncContext* context() { return context_; }
  bool connected() const;
  const char* errstr() const;

  void setConnectCallback(const ConnectCallback& cb) { connectCb_ = cb; }
  void setDisconnectCallback(const DisconnectCallback& cb) { disconnectCb_ = cb; }

  void connect();
  void disconnect();

  int command(const CommandCallback& cb, muduo::StringArg cmd, ...);

  // 这条应答是否表示「服务端确定没有执行」(MOVED、LOADING 等)。只影响维护前沿的记账:
  // 判定为真的错误应答不算 UNKNOWN,否则一次普通的 slot 迁移就会永久卡住进程的维护排空。
  // 连接断开时 reply 为空,同样会问它:结果未知的命令必须回 false。
  typedef bool (*NotExecutedPredicate)(const redisReply*);

  // 给不改数据的命令用(AUTH、PING、CLUSTER SLOTS、ASKING):任何结局都不留下 UNKNOWN,包括断线时的空应答。
  // 否则节点退场时在途的握手或探测会永久卡住进程的维护排空。
  static bool hasNoSideEffect(const redisReply*) { return true; }

  // 发送已格式化好的 RESP 命令(redisAsyncFormattedCommand)。返回值、回调次数与 privdata 释放约定都同 command()。
  // cmd 必须以 NUL 结尾(hiredis 的 nextArgument 会读到结尾后一字节);内容在返回前已拷进输出缓冲,调用方不必继续保留。
  // notExecuted 为空时错误应答与空应答一律记 UNKNOWN,同 command()。
  int formattedCommand(const CommandCallback& cb, const char* cmd, size_t len,
                       NotExecutedPredicate notExecuted = NULL);

  int ping();

 private:
  Hiredis(muduo::net::EventLoop* loop, const muduo::net::InetAddress& serverAddr,
          std::shared_ptr<MaintenanceState> maintenance);
  void handleRead(muduo::Timestamp receiveTime);
  void handleWrite();

  int fd() const;
  void logConnection(bool up) const;
  void setChannel();
  void removeChannel();

  void connectCallback(int status);
  void disconnectCallback(int status);

  static Hiredis* getHiredis(const redisAsyncContext* ac);

  static void connectCallback(const redisAsyncContext* ac, int status);
  static void disconnectCallback(const redisAsyncContext* ac, int status);
  // command callback
  static void commandCallback(redisAsyncContext* ac, void*, void*);

  static void addRead(void* privdata);
  static void delRead(void* privdata);
  static void addWrite(void* privdata);
  static void delWrite(void* privdata);
  static void cleanup(void* privdata);

 private:
  std::shared_ptr<MaintenanceState> maintenance_;
  std::shared_ptr<LocalReference> lifetime_;
  std::string lastError_;
  // 仅 loop 线程访问；局部副本让回调返回时无需重新读取 wrapper。
  std::shared_ptr<bool> callbackActive_ = std::make_shared<bool>(false);
  muduo::net::EventLoop* loop_;
  const muduo::net::InetAddress serverAddr_;
  redisAsyncContext* context_;
  std::shared_ptr<muduo::net::Channel> channel_;
  ConnectCallback connectCb_;
  DisconnectCallback disconnectCb_;
};

}  // namespace hiredis

#endif  // MUDUO_CONTRIB_HIREDIS_HIREDIS_H
