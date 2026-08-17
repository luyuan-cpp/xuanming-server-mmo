#pragma once

#include <grpcpp/grpcpp.h>
#include <muduo/net/EventLoop.h>
#include "proto/scene_manager/scene_node_service.grpc.pb.h"

// gRPC service implementation for SceneNodeGrpc.
// All RPC methods dispatch to the muduo event loop via runInLoop + promise/future
// (blocking the gRPC thread pool thread until the event loop processes the request).
//
// IMPORTANT: Handle* methods run on the event loop thread.
//   - Do NOT perform blocking I/O or long-running operations.
//   - Always return grpc::Status::OK; communicate errors via response fields.
class SceneNodeGrpcImpl final : public scene_node::SceneNodeGrpc::Service
{
public:
    explicit SceneNodeGrpcImpl(muduo::net::EventLoop& loop);

    grpc::Status CreateScene(grpc::ServerContext* context,
        const ::CreateSceneRequest* request,
        ::CreateSceneResponse* response) override;

    grpc::Status DestroyScene(grpc::ServerContext* context,
        const ::DestroySceneRequest* request,
        ::Empty* response) override;

    grpc::Status ReleasePlayer(grpc::ServerContext* context,
        const ::scene_node::ReleasePlayerRequest* request,
        ::Empty* response) override;

    grpc::Status PrepareBattle(grpc::ServerContext* context,
        const ::PrepareBattleRequest* request,
        ::PrepareBattleResponse* response) override;

    grpc::Status CancelBattlePrepare(grpc::ServerContext* context,
        const ::CancelBattlePrepareRequest* request,
        ::Empty* response) override;

private:
    // Handler functions -- run on the muduo event loop thread.
    // WARNING: Must complete quickly. The gRPC thread is blocked waiting via promise/future.
    static void HandleCreateScene(const ::CreateSceneRequest* request, ::CreateSceneResponse* response);
    static void HandleDestroyScene(const ::DestroySceneRequest* request);
    static void HandleReleasePlayer(const ::scene_node::ReleasePlayerRequest* request);
    static void HandlePrepareBattle(const ::PrepareBattleRequest* request, ::PrepareBattleResponse* response);
    static void HandleCancelBattlePrepare(const ::CancelBattlePrepareRequest* request);

    muduo::net::EventLoop& loop_;
};
