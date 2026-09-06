#pragma once

#include <grpcpp/grpcpp.h>
#include <muduo/net/EventLoop.h>

// 生成产物:proto/battle/player_battle.proto 重生成后落到
// cpp/generated/proto/battle/player_battle.grpc.pb.h(路径按 scene_manager 等
// 既有 gRPC 域的生成规律推断),定义 BattleClientPlayer::Service。
#include "proto/battle/player_battle.grpc.pb.h"

// gate → battle 的客户端消息 gRPC 服务
// (SubmitBattleAction / GetBattleState / StopWatchBattle / SetAutoBattle)。
//
// 手写而非生成骨架,原因:生成的 grpc handler 骨架(见 grpc_handler_gen.go 模板)
// 会丢弃 ServerContext,而本服务必须:
//   1. 从请求 metadata x-session-detail-bin 解出 gate 注入的权威会话身份
//      (player_id 只认它,不信请求体 —— 模仿 go/login SessionInterceptor);
//   2. 把同一份 session detail 回写进响应 initial metadata —— gate 的
//      SetIfEmptyHandler 靠它把 gRPC 应答路由回对应客户端 TCP 会话
//      (见 cpp/nodes/gate/main.cpp 的响应桥接)。
//
// 线程投递模式与 scene 的 SceneNodeGrpcImpl 一致:runInLoop + promise/future,
// Handle 逻辑(BattleRoomManager)全部在 muduo loop 线程执行。
//
// Notify* 方法是 S2C 推送方向(battle 经 Kafka 推给 gate),永远不会被 gRPC
// 调用,不覆写,走生成基类默认的 UNIMPLEMENTED。
class BattleClientPlayerGrpcImpl final : public BattleClientPlayer::Service
{
public:
    explicit BattleClientPlayerGrpcImpl(muduo::net::EventLoop &loop);

    grpc::Status SubmitBattleAction(grpc::ServerContext *context,
        const ::SubmitBattleActionRequest *request,
        ::SubmitBattleActionResponse *response) override;

    grpc::Status GetBattleState(grpc::ServerContext *context,
        const ::GetBattleStateRequest *request,
        ::BattleStateS2C *response) override;

    // ---- 二期:观战退出 + 自动战斗(设计文档 §10/§11) ----

    grpc::Status StopWatchBattle(grpc::ServerContext *context,
        const ::StopWatchBattleRequest *request,
        ::StopWatchBattleResponse *response) override;

    grpc::Status SetAutoBattle(grpc::ServerContext *context,
        const ::SetAutoBattleRequest *request,
        ::SetAutoBattleResponse *response) override;

    // 丢票补签不在本服务下:gate 收成只连路由服后不再直连 battle(client-rpc-router.md D33),
    // 客户端走 MatchService.RequestBattleTicket → BattleNode.IssueBattleTicket(见 battle_node.h)。

private:
    muduo::net::EventLoop &loop_;
};
