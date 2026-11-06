#pragma once

// etcd 访问的传输接缝(docs/design/grpc-muduo-transport.md §13.4)。
//
// EtcdHelper 组好请求后只经 EtcdTransport 发出;应答交给 EtcdResponseSink(EtcdService 实现)。
// 实现只有 MuduoEtcdTransport(muduo_grpc 通道 + 双向流,流断按退避重建,§13.5);官方 CQ 实现 GrpcEtcdTransport
// 已随第 3 批删除(§14)。接口仍保留,单测可经 SetFactory 换成替身。
//
// 线程:全部接口只在节点 EventLoop 线程调用，sink 的回调也只在该线程异步执行。

#include <functional>
#include <memory>
#include <optional>
#include <string>

#include "proto/etcd/etcd.pb.h"

// Txn/LeaseGrant 各自拥有一次完成。空值表示提交或传输失败，CAS 拒绝仍有真实 TxnResponse。
// 已发出的命令可以在停机后由通道收尾，调用方先取消本地等待。
template<class Reply>
using EtcdRegistrationCompletion = std::function<void(std::optional<Reply>)>;

namespace muduo
{
namespace net
{
class EventLoop;
} // namespace net
} // namespace muduo

class EtcdResponseSink
{
public:
    virtual ~EtcdResponseSink() = default;

    // request:传输带回原请求(对账要知道是哪个前缀),不为空。
    virtual void OnRange(const etcdserverpb::RangeRequest *request, const etcdserverpb::RangeResponse &reply) = 0;
    virtual void OnPut(const etcdserverpb::PutResponse &reply) = 0;
    virtual void OnDeleteRange(const etcdserverpb::DeleteRangeResponse &reply) = 0;
    virtual void OnWatch(const etcdserverpb::WatchResponse &reply) = 0;
    virtual void OnKeepAlive(const etcdserverpb::LeaseKeepAliveResponse &reply) = 0;

    // 仅新传输:断掉的流已经换成新流(首条流不回调)。在新流上之前登记的 watch 全部失效,
    // 调用方要重新 Range → 对账 → 建 watch;KeepAlive 流要立即补一拍。
    virtual void OnWatchStreamRestarted() {}
    virtual void OnKeepAliveStreamRestarted() {}
};

class EtcdTransport
{
public:
    virtual ~EtcdTransport() = default;

    virtual const char *Name() const = 0;
    virtual void Range(const etcdserverpb::RangeRequest &request) = 0;
    virtual void Put(const etcdserverpb::PutRequest &request) = 0;
    virtual void DeleteRange(const etcdserverpb::DeleteRangeRequest &request) = 0;
    virtual void Txn(const etcdserverpb::TxnRequest &request,
                     EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done) = 0;
    virtual void LeaseGrant(const etcdserverpb::LeaseGrantRequest &request,
                            EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done) = 0;
    virtual void LeaseRevoke(const etcdserverpb::LeaseRevokeRequest &request) = 0;
    // 往 Watch / LeaseKeepAlive 双向流写一条请求。新传输:流断开、尚未重建时直接丢弃 ——
    // 重建后由 sink 的 Restarted 回调重发需要的请求,旧流上的请求没有意义。
    virtual void WatchWrite(const etcdserverpb::WatchRequest &request) = 0;
    virtual void KeepAliveWrite(const etcdserverpb::LeaseKeepAliveRequest &request) = 0;

    // 节点关机(FinalizeShutdownInLoop 阶段 0,loop 仍在运行):停止重建、释放连接;之后不再回调 sink。
    // 调用前已发出的 LeaseRevoke 尽量写出(§13.7),应答不等。
    virtual void Shutdown() = 0;
};

namespace etcd_transport
{
// endpoint 是 Etcd.Hosts 的第一项("host:port")。sink 的寿命必须覆盖返回的传输对象。
using Factory = std::function<std::unique_ptr<EtcdTransport>(muduo::net::EventLoop &loop, const std::string &endpoint,
                                                             EtcdResponseSink &sink)>;

// 进程级:必须在 Node 构造之前调用(EtcdService::Init 在 Node 构造函数里取工厂)。只给单测换替身用;不设 = MuduoEtcdTransport。
void SetFactory(Factory factory);
// 按已装的工厂建传输;没装工厂(或工厂返回空)时建 MuduoEtcdTransport。
std::unique_ptr<EtcdTransport> Create(muduo::net::EventLoop &loop, const std::string &endpoint, EtcdResponseSink &sink);

// 读环境变量 NODE_ETCD_TRANSPORT,只为兼容旧部署:不设或 muduo_h2 正常;grpc(官方实现已删除)与不认识的值打 WARN,
// 一律走 muduo_h2。定义在 muduo_etcd_transport.cpp。
void InstallFromEnv();
} // namespace etcd_transport
