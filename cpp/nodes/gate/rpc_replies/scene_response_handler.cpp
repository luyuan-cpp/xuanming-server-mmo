
#include "scene_response_handler.h"

#include "rpc/service_metadata/scene_service_metadata.h"
#include "network/codec/message_response_dispatcher.h"

extern MessageResponseDispatcher gRpcResponseDispatcher;

///<<< BEGIN WRITING YOUR CODE
#include "node/system/node/node.h"
#include "session/manager/session_manager.h"
#include "gate_codec.h"
#include "handler/event/scene_entry_dispatch.h"
#include "table/proto/tip/common_error_tip.pb.h"

#include <chrono>

///<<< END WRITING YOUR CODE

void InitSceneReply()
{
    gRpcResponseDispatcher.registerMessageCallback<::Empty>(ScenePlayerEnterGameNodeMessageId,
        std::bind(&OnScenePlayerEnterGameNodeReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::NodeRouteMessageResponse>(SceneSendMessageToPlayerMessageId,
        std::bind(&OnSceneSendMessageToPlayerReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::ProcessClientPlayerMessageResponse>(SceneProcessClientPlayerMessageMessageId,
        std::bind(&OnSceneProcessClientPlayerMessageReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::NodeRouteMessageResponse>(SceneInvokePlayerServiceMessageId,
        std::bind(&OnSceneInvokePlayerServiceReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::RouteMessageResponse>(SceneRouteNodeStringMsgMessageId,
        std::bind(&OnSceneRouteNodeStringMsgReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::RoutePlayerMessageResponse>(SceneRoutePlayerStringMsgMessageId,
        std::bind(&OnSceneRoutePlayerStringMsgReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::Empty>(SceneUpdateSessionDetailMessageId,
        std::bind(&OnSceneUpdateSessionDetailReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::CreateSceneResponse>(SceneCreateSceneMessageId,
        std::bind(&OnSceneCreateSceneReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::Empty>(SceneDestroySceneMessageId,
        std::bind(&OnSceneDestroySceneReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::PrepareBattleResponse>(ScenePrepareBattleMessageId,
        std::bind(&OnScenePrepareBattleReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::Empty>(SceneCancelBattlePrepareMessageId,
        std::bind(&OnSceneCancelBattlePrepareReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
    gRpcResponseDispatcher.registerMessageCallback<::NodeHandshakeResponse>(SceneNodeHandshakeMessageId,
        std::bind(&OnSceneNodeHandshakeReply, std::placeholders::_1, std::placeholders::_2, std::placeholders::_3));
}

void OnScenePlayerEnterGameNodeReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneSendMessageToPlayerReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeRouteMessageResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneProcessClientPlayerMessageReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::ProcessClientPlayerMessageResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
    if (!replied)
    {
        return;
    }

    if (!replied->has_message_content())
    {
        return;
    }

    const auto sessionId = replied->session_id();
    auto sessionIt = tlsSessionManager.sessions().find(sessionId);
    if (sessionIt == tlsSessionManager.sessions().end())
    {
        LOG_WARN << "SceneProcessClientPlayerMessageReply: session not found, session_id="
                 << sessionId << ", message_id=" << replied->message_content().message_id();
        return;
    }

    if (const auto clientConn = sessionIt->second.conn.lock())
    {
        GetGateCodec().send(clientConn, replied->message_content());
    }
///<<< END WRITING YOUR CODE
}

void OnSceneInvokePlayerServiceReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeRouteMessageResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE

///<<< END WRITING YOUR CODE
}

void OnSceneRouteNodeStringMsgReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::RouteMessageResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneRoutePlayerStringMsgReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::RoutePlayerMessageResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneUpdateSessionDetailReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneCreateSceneReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::CreateSceneResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

void OnSceneDestroySceneReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE}
}

void OnScenePrepareBattleReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::PrepareBattleResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE}
}

void OnSceneCancelBattlePrepareReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE}
}

void OnSceneNodeHandshakeReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeHandshakeResponse>& replied, muduo::Timestamp timestamp)
{
///<<< BEGIN WRITING YOUR CODE
	gNode->GetNodeRegistrationManager().OnHandshakeReplied(*replied);
	// CPP-2 链路就绪印章:TCP 连上 0.5s 后才发握手(registration_manager.cpp RunAfter(0.5)),scene 在收到
	// 握手时才为本 gate 挂 RpcSession,之前发出的进场通知会被丢掉。所以"可以向这个 scene 交付进场"要以
	// 这里的成功应答为准。conn 就是应答所走的出站连接,按连接对象盖章,重连后旧章自然失效。
	// 不用 ConnectToNodeEvent:它按 peer_addr 匹配,而且不带 conn,分不出迟到的旧连接应答。
	if (replied && replied->error_message().id() == kCommon_errorOK)
	{
		gate_scene_entry::OnSceneLinkHandshaken(conn, replied->peer_node().node_uuid(),
		                                        std::chrono::steady_clock::now());
	}
///<<< END WRITING YOUR CODE
}
