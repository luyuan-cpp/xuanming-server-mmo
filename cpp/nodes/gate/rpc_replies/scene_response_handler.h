#pragma once
#include "muduo/net/TcpConnection.h"
#include "proto/scene/scene.pb.h"

void OnScenePlayerEnterGameNodeReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp);
void OnSceneSendMessageToPlayerReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeRouteMessageResponse>& replied, muduo::Timestamp timestamp);
void OnSceneProcessClientPlayerMessageReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::ProcessClientPlayerMessageResponse>& replied, muduo::Timestamp timestamp);
void OnSceneInvokePlayerServiceReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeRouteMessageResponse>& replied, muduo::Timestamp timestamp);
void OnSceneRouteNodeStringMsgReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::RouteMessageResponse>& replied, muduo::Timestamp timestamp);
void OnSceneRoutePlayerStringMsgReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::RoutePlayerMessageResponse>& replied, muduo::Timestamp timestamp);
void OnSceneUpdateSessionDetailReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp);
void OnSceneCreateSceneReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::CreateSceneResponse>& replied, muduo::Timestamp timestamp);
void OnSceneDestroySceneReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp);
void OnScenePrepareBattleReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::PrepareBattleResponse>& replied, muduo::Timestamp timestamp);
void OnSceneCancelBattlePrepareReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::Empty>& replied, muduo::Timestamp timestamp);
void OnSceneNodeHandshakeReply(const muduo::net::TcpConnectionPtr& conn, const std::shared_ptr<::NodeHandshakeResponse>& replied, muduo::Timestamp timestamp);
