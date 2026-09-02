#pragma once

#include <grpcpp/grpcpp.h>
#include <muduo/net/EventLoop.h>
#include "proto/battle/battle_node.grpc.pb.h"

// BattleNode 内部控制面 gRPC 实现(match 服务调用)。
// 手写文件:生成器暂未给 battle 节点配 handler 骨架输出,形态严格镜像
// cpp/nodes/scene/handler/grpc/scene_node_service.h(runInLoop + promise/future
// 把请求投递到 muduo loop 线程,阻塞 gRPC 池线程直至处理完成)。
//
// 注意:Handle* 在 loop 线程执行 —— 不做阻塞 I/O;错误经 response 字段返回,
// gRPC status 恒为 OK。
class BattleNodeImpl final : public BattleNode::Service
{
public:
    explicit BattleNodeImpl(muduo::net::EventLoop& loop);

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

private:
    // loop 线程执行;必须快速完成(gRPC 线程经 promise/future 等待)。
    static void HandleCreateBattle(const ::CreateBattleRequest* request, ::CreateBattleResponse* response);
    static void HandleDestroyBattle(const ::DestroyBattleRequest* request);
    static void HandleAddObserver(const ::AddObserverRequest* request, ::AddObserverResponse* response);
    static void HandleRemoveObserver(const ::RemoveObserverRequest* request);

    muduo::net::EventLoop& loop_;
};
