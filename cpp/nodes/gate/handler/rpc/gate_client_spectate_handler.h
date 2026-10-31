#pragma once
#include "proto/gate/gate_client_spectate.pb.h"

// done 由框架传入且非空:不碰它,handler 返回后框架立即回包;要等下游再回复,在返回前 RpcDoneCallback::Adopt(done)(network/rpc_done_callback.h)。
class GateClientSpectateHandler : public ::GateClientSpectate
{
public:

	void SubscribeSpectate(::google::protobuf::RpcController* controller,  const ::SubscribeSpectateRequest* request, ::SubscribeSpectateResponse* response, ::google::protobuf::Closure* done) override;

	void UnsubscribeSpectate(::google::protobuf::RpcController* controller,  const ::UnsubscribeSpectateRequest* request, ::UnsubscribeSpectateResponse* response, ::google::protobuf::Closure* done) override;

};