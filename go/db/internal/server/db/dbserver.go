// Db gRPC 服务端转发层，由 go/db/server_wrappers_test.go 校验与 proto/db.Db 全量对齐。

package server

import (
	"context"

	"db/internal/logic/db"
	"db/internal/svc"
	"proto/db"
)

type DbServer struct {
	svcCtx *svc.ServiceContext
	db.UnimplementedDbServer
}

func NewDbServer(svcCtx *svc.ServiceContext) *DbServer {
	return &DbServer{
		svcCtx: svcCtx,
	}
}

func (s *DbServer) Test(ctx context.Context, in *db.TestRequest) (*db.TestResponse, error) {
	l := dblogic.NewTestLogic(ctx, s.svcCtx)
	return l.Test(in)
}
