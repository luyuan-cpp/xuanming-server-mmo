// LoginAdmin gRPC 服务端转发层，由 go/login/goctl_wrappers_test.go 校验与 proto/login 全量对齐。

package server

import (
	"context"

	"login/internal/logic/loginadmin"
	"login/internal/svc"
	"proto/login"
)

type LoginAdminServer struct {
	svcCtx *svc.ServiceContext
	login.UnimplementedLoginAdminServer
}

func NewLoginAdminServer(svcCtx *svc.ServiceContext) *LoginAdminServer {
	return &LoginAdminServer{
		svcCtx: svcCtx,
	}
}

// 摘孤儿角色:走目录的删角流程(data_service.RemoveAccountRoles,全部参与者通过才写墓碑)。
func (s *LoginAdminServer) RemovePlayersFromAccounts(ctx context.Context, in *login.RemovePlayersFromAccountsRequest) (*login.RemovePlayersFromAccountsResponse, error) {
	l := loginadminlogic.NewRemovePlayersFromAccountsLogic(ctx, s.svcCtx)
	return l.RemovePlayersFromAccounts(in)
}

func (s *LoginAdminServer) ScheduleAccountDeletion(ctx context.Context, in *login.ScheduleAccountDeletionRequest) (*login.AccountDeletionResponse, error) {
	l := loginadminlogic.NewScheduleAccountDeletionLogic(ctx, s.svcCtx)
	return l.ScheduleAccountDeletion(in)
}

func (s *LoginAdminServer) ResetAccountRealName(ctx context.Context, in *login.ResetAccountRealNameRequest) (*login.ResetAccountRealNameResponse, error) {
	l := loginadminlogic.NewResetAccountRealNameLogic(ctx, s.svcCtx)
	return l.ResetAccountRealName(in)
}
