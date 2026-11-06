package clientplayerloginlogic

import (
	"context"

	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
)

// RequestGateMigrationLogic 处理 gate 排空时为本连接要迁移票据的请求(docs/design/gate-drain-session-migration.md §4.4)。
//
// 与 Disconnect 一样只有 gate 合成(proto 标 CLIENT_RPC_SERVER_ONLY):身份全在 gate 签名、callerauth 验过的 SessionDetails 里,
// 请求体为空。选目标与签票在 ServiceContext.SignGateMigration,这里只取身份、转交。
type RequestGateMigrationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRequestGateMigrationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RequestGateMigrationLogic {
	return &RequestGateMigrationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RequestGateMigrationLogic) RequestGateMigration(in *login_proto.GateMigrationRequest) (*login_proto.GateMigrationResponse, error) {
	// 没有经验签的会话身份就不签:这条请求只能来自 gate,客户端或别的服务发来的没有可信身份。
	// 回 UNSPECIFIED 而不是 gRPC 错误,gate 统一按「这个连接稍后再试」处理。
	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "request gate migration")
	if !ok {
		return &login_proto.GateMigrationResponse{Result: login_proto.GateMigrationResult_GATE_MIGRATION_RESULT_UNSPECIFIED}, nil
	}
	return l.svcCtx.SignGateMigration(l.ctx, sessionDetails), nil
}
