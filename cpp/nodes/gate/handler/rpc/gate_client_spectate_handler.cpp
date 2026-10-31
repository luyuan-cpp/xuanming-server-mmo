
#include "gate_client_spectate_handler.h"

///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE

void GateClientSpectateHandler::SubscribeSpectate(::google::protobuf::RpcController* controller, const ::SubscribeSpectateRequest* request,
	::SubscribeSpectateResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	// 这是生成器产出的节点间 RPC 入口,没有调用方,不做任何事。客户端的这条消息走不到这里:client_session_handler.cpp 按
	// targetNodeType == GateNodeService 在分发处本地处理(spectate/gate_spectate_client.h)。逻辑不要放这里。
///<<< END WRITING YOUR CODE
}

void GateClientSpectateHandler::UnsubscribeSpectate(::google::protobuf::RpcController* controller, const ::UnsubscribeSpectateRequest* request,
	::UnsubscribeSpectateResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	// 同 SubscribeSpectate:没有调用方,不做任何事,客户端的这条消息在 client_session_handler.cpp 本地处理。
///<<< END WRITING YOUR CODE
}
