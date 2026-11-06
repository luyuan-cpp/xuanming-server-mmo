#pragma once

#include <cstdint>
#include <memory>
#include <string>

#include "node/system/etcd/etcd_transport.h"

// muduo_grpc 通道上的 etcd 传输(docs/design/grpc-muduo-transport.md §13)。
//
//   * unary(Range / Put / DeleteRange / Txn / LeaseGrant / LeaseRevoke):每次调用带截止时间
//     (grpc_call_deadline::Get(EtcdNodeService),与生成的 etcd 客户端同一配置)。注册请求按每次调用完成，
//     其它 unary 的失败按方法限频记录，发现侧继续按既有策略对账。
//   * Watch / LeaseKeepAlive 两条双向流常驻;任何原因结束都按退避重建:0.2s 起、×2、封顶 5s、±20% 抖动,
//     流活过 10s 再断则从 0.2s 重新开始,**不设次数上限**(§10.2 第 1 条的有意偏离)。
//     重建后回调 sink 的 OnWatchStreamRestarted / OnKeepAliveStreamRestarted,由 EtcdService 重新 Range 对账 / 补一拍。
//   * 通道开周期 keepalive(30s / 10s):靠 HTTP/2 PING 发现静默断链,etcd 服务端 keepalive 下限 5s,不会被 too_many_pings。
//   * 地址在构造时解析一次(Node 构造期,loop 尚未运行):IPv4 字面量直接用,主机名 getaddrinfo,
//     失败每秒重试、最多 10 次,仍失败 LOG_FATAL(§13.6)。运行中不再做 DNS。
//
// 生命周期(AGENTS.md §11.7):内部状态由 shared_ptr 持有,通道 / 流 / 定时器的回调只绑 weak_ptr;
// 本对象析构或 Shutdown 后不再回调 sink。必须在 loop 线程上、loop 仍在运行时析构(EtcdService::Shutdown 负责)。
class MuduoEtcdTransport final : public EtcdTransport
{
public:
    // endpoint:"host:port",可带 "http://" 前缀;不支持 TLS。解析失败 LOG_FATAL。
    MuduoEtcdTransport(muduo::net::EventLoop &loop, const std::string &endpoint, EtcdResponseSink &sink);
    ~MuduoEtcdTransport() override;
    MuduoEtcdTransport(const MuduoEtcdTransport &) = delete;
    MuduoEtcdTransport &operator=(const MuduoEtcdTransport &) = delete;

    const char *Name() const override { return "muduo_h2"; }

    void Range(const etcdserverpb::RangeRequest &request) override;
    void Put(const etcdserverpb::PutRequest &request) override;
    void DeleteRange(const etcdserverpb::DeleteRangeRequest &request) override;
    void Txn(const etcdserverpb::TxnRequest &request,
             EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done) override;
    void LeaseGrant(const etcdserverpb::LeaseGrantRequest &request,
                    EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done) override;
    void LeaseRevoke(const etcdserverpb::LeaseRevokeRequest &request) override;
    void WatchWrite(const etcdserverpb::WatchRequest &request) override;
    void KeepAliveWrite(const etcdserverpb::LeaseKeepAliveRequest &request) override;
    void Shutdown() override;

private:
    class Core;
    std::shared_ptr<Core> core_;
};
