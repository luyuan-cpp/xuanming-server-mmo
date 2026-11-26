#pragma once
#include "proto/scene/scene.pb.h"

// done 由框架传入且非空:不碰它,handler 返回后框架立即回包;要等下游再回复,在返回前 RpcDoneCallback::Adopt(done)(network/rpc_done_callback.h)。
class SceneHandler : public ::Scene
{
public:

	void PlayerEnterGameNode(::google::protobuf::RpcController* controller,  const ::PlayerEnterGameNodeRequest* request, ::Empty* response, ::google::protobuf::Closure* done) override;

	void ProcessClientPlayerMessage(::google::protobuf::RpcController* controller,  const ::ProcessClientPlayerMessageRequest* request, ::ProcessClientPlayerMessageResponse* response, ::google::protobuf::Closure* done) override;

	void UpdateSessionDetail(::google::protobuf::RpcController* controller,  const ::RegisterPlayerSessionRequest* request, ::Empty* response, ::google::protobuf::Closure* done) override;

	void CreateScene(::google::protobuf::RpcController* controller,  const ::CreateSceneRequest* request, ::CreateSceneResponse* response, ::google::protobuf::Closure* done) override;

	void DestroyScene(::google::protobuf::RpcController* controller,  const ::DestroySceneRequest* request, ::Empty* response, ::google::protobuf::Closure* done) override;

	void PrepareBattle(::google::protobuf::RpcController* controller,  const ::PrepareBattleRequest* request, ::PrepareBattleResponse* response, ::google::protobuf::Closure* done) override;

	void CancelBattlePrepare(::google::protobuf::RpcController* controller,  const ::CancelBattlePrepareRequest* request, ::Empty* response, ::google::protobuf::Closure* done) override;

	void NodeHandshake(::google::protobuf::RpcController* controller,  const ::NodeHandshakeRequest* request, ::NodeHandshakeResponse* response, ::google::protobuf::Closure* done) override;

	void CheckJianghuDuelEligibility(::google::protobuf::RpcController* controller,  const ::CheckJianghuDuelEligibilityRequest* request, ::CheckJianghuDuelEligibilityResponse* response, ::google::protobuf::Closure* done) override;

};