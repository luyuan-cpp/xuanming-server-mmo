#pragma once

#include <grpcpp/grpcpp.h>
#include <muduo/net/EventLoop.h>
#include "battle_admission_gate.h"
#include "proto/battle/battle_node.grpc.pb.h"

// BattleNode 内部控制面 gRPC 实现(match 服务调用)。
// 手写文件:生成器暂未给 battle 节点配 handler 骨架输出,形态严格镜像
// cpp/nodes/scene/handler/grpc/scene_node_service.h(runInLoop + promise/future
// 把请求投递到 muduo loop 线程,阻塞 gRPC 池线程直至处理完成)。
//
// 注意:Handle* 在 loop 线程执行 —— 不做阻塞 I/O;业务错误经 response 字段(tip)返回,
// gRPC status 为 OK。唯一例外是 CreateBattle 的节点级准入拒绝(集群外入口 D82):
// 建房准入闸未开 / 已关(battle_admission_gate.h),或拿不到 Agones 分配许可,即回
// grpc::Status(UNAVAILABLE, "battle_not_allocatable"),此时没有任何副作用。
// 这是节点级"换一台试"信号,发生在业务处理之前,客户端看不到,故不走 tip。
// 消息字面量是跨语言字符串契约,常量与说明在 battle_node.cpp。

class BattleNodeImpl final : public BattleNode::Service
{
public:
    explicit BattleNodeImpl(muduo::net::EventLoop& loop);

    // ---- 建房准入闸(battle_admission_gate.h;只管 CreateBattle,其余 RPC 只作用于已有房间) ----
    // 两者都只在 loop 线程调用:loop 内复核与 Close 同在 loop 上,才能保证排在关闸之后的建房必被拒。

    // SetAfterStart 里 Agones lifecycle 启动之后调用。返回 false = 没有打开:停机已先开始
    // (闸已关,不会再开),或重复调用(已经是打开的)。
    bool OpenAdmission();

    // SetBeforeShutdown 第一句调用(先于 AbortAllRooms)。终态,幂等。
    void CloseAdmission();

    grpc::Status CreateBattle(grpc::ServerContext* context,
        const ::CreateBattleRequest* request,
        ::CreateBattleResponse* response) override;

    grpc::Status DestroyBattle(grpc::ServerContext* context,
        const ::DestroyBattleRequest* request,
        ::Empty* response) override;

    // ---- 二期:观战接入(match → battle,设计文档 §10) ----

    grpc::Status AddObserver(grpc::ServerContext* context,
        const ::AddObserverRequest* request,
        ::AddObserverResponse* response) override;

    grpc::Status RemoveObserver(grpc::ServerContext* context,
        const ::RemoveObserverRequest* request,
        ::Empty* response) override;

    // ---- 客户端直连:丢票补签(match → battle,turn-based §18 D25 / client-rpc-router.md D33) ----
    // 会话身份由 match 从大厅会话取得并放进 request.player_id,本节点只核对名单并自签。

    grpc::Status IssueBattleTicket(grpc::ServerContext* context,
        const ::IssueBattleTicketRequest* request,
        ::IssueBattleTicketResponse* response) override;

private:
    // loop 线程执行;必须快速完成(gRPC 线程经 promise/future 等待)。
    static void HandleCreateBattle(const ::CreateBattleRequest* request, ::CreateBattleResponse* response);
    static void HandleDestroyBattle(const ::DestroyBattleRequest* request);
    static void HandleAddObserver(const ::AddObserverRequest* request, ::AddObserverResponse* response);
    static void HandleRemoveObserver(const ::RemoveObserverRequest* request);
    static void HandleIssueBattleTicket(const ::IssueBattleTicketRequest* request, ::IssueBattleTicketResponse* response);

    muduo::net::EventLoop& loop_;
    battle_admission::AdmissionGate admission_;
};
