// Package server 是 chat 的 gRPC 入口薄包装(照 match / scene_manager 的 internal/server 模式):
// 每个 RPC 委托给 logic,不写业务。
package server

import (
	"context"

	"chat/internal/logic"
	"chat/internal/svc"

	chatpb "proto/chat"
)

// ChatServer 实现 chatpb.ClientPlayerChatServer。
// 该 service 在 proto 里标了 OptionIsClientProtocolService=true:只接客户端经 gate→路由服
// 转发来的请求,东西向调用不许加到这个 service 上(契约 §3)。
type ChatServer struct {
	chatpb.UnimplementedClientPlayerChatServer
	svcCtx *svc.ServiceContext
}

func NewChatServer(svcCtx *svc.ServiceContext) *ChatServer {
	return &ChatServer{svcCtx: svcCtx}
}

// SendChat 发一条聊天(WORLD / PRIVATE)。
func (s *ChatServer) SendChat(ctx context.Context, in *chatpb.SendChatRequest) (*chatpb.SendChatResponse, error) {
	return logic.NewChatLogic(ctx, s.svcCtx).SendChat(in)
}

// PullChatHistory 拉最近 N 条历史(快照式,新在前)。
func (s *ChatServer) PullChatHistory(ctx context.Context, in *chatpb.PullChatHistoryRequest) (*chatpb.PullChatHistoryResponse, error) {
	return logic.NewChatLogic(ctx, s.svcCtx).PullChatHistory(in)
}
