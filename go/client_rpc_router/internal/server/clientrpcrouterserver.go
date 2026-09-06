// Package server 是 gRPC 入口薄包装(照 match 的 internal/server 模式):
// 每个 RPC 委托对应 logic,不写业务。
package server

import (
	"context"

	"client_rpc_router/internal/logic"
	"client_rpc_router/internal/svc"

	pb "proto/client_rpc_router"
	base "proto/common/base"
)

// ClientRpcRouterServer 实现 client_rpc_router.ClientRpcRouter。
type ClientRpcRouterServer struct {
	svcCtx *svc.ServiceContext
	pb.UnimplementedClientRpcRouterServer
}

// NewClientRpcRouterServer 构造入口包装。
func NewClientRpcRouterServer(svcCtx *svc.ServiceContext) *ClientRpcRouterServer {
	return &ClientRpcRouterServer{svcCtx: svcCtx}
}

// Forward:gate 把客户端 gRPC 类消息原包交给路由服,按生成路由表原始字节转发
// (设计文档 §3)。
func (s *ClientRpcRouterServer) Forward(ctx context.Context, in *pb.ForwardRequest) (*base.MessageContent, error) {
	return logic.NewForwardLogic(ctx, s.svcCtx).Forward(in)
}
