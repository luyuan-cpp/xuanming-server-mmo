package clientplayerloginlogic

import (
	"context"
	"time"

	"login/internal/logic/pkg/loginsession"
	"login/internal/svc"
	login_proto "proto/login"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RenewSessionLogic 处理 gate 为仍然连着的连接定期代发的续租(docs/design/gate-online-identity.md §10.4)。
//
// 与 Disconnect、RequestGateMigration 一样只有 gate 合成(proto 标 CLIENT_RPC_SERVER_ONLY):连接身份全在 gate 签名、
// callerauth 验过的 SessionDetails 里,请求体为空。登录态、设备集成员与绑定栅栏的续期在 loginsession.Renew,这里只取身份、转交,
// 并把下一次续租的间隔告诉 gate(寿命只在 login 配,gate 不另配)。
type RenewSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenewSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenewSessionLogic {
	return &RenewSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RenewSessionLogic) RenewSession(in *login_proto.RenewSessionRequest) (*login_proto.RenewSessionResponse, error) {
	// 没有经验签的会话身份、或连接身份不完整:这条连接在 login 不可能有记录。回「没有可续的」,gate 停止为它续租。
	sessionDetails, ok := getSessionDetailsForAction(l.ctx, "renew session")
	if !ok {
		return &login_proto.RenewSessionResponse{}, nil
	}
	sessionRef, hasIdentity := loginsession.RefOf(sessionDetails)
	if !hasIdentity {
		logMissingConnectionIdentity("RenewSession", sessionDetails)
		return &login_proto.RenewSessionResponse{}, nil
	}

	ttl := loginsession.TTL()
	kept, err := loginsession.Renew(l.ctx, l.svcCtx.LoginRedis, sessionRef, ttl)
	if err != nil {
		// 回错误而不是「没有可续的」:后者会让 gate 停止续租。续租可以原样重试,gate 退避之后再发;
		// 期间登录态照旧倒计时,不影响在线玩家。连续失败超过寿命时登录态会过期、这条连接暂时不计入设备数;
		// 恢复后的第一次续租会把还在游戏里的连接按绑定记录重建并重新计数(loginsession.Renew)。
		logx.Errorf("RenewSession failed, gate will retry: session_id=%d gate_instance=%q incarnation=%d err=%v",
			sessionDetails.GetSessionId(), sessionDetails.GetGateInstanceId(), sessionDetails.GetSessionIncarnation(), err)
		return nil, status.Error(codes.Unavailable, "login session renewal unavailable, retry later")
	}
	if !kept {
		return &login_proto.RenewSessionResponse{}, nil
	}
	return &login_proto.RenewSessionResponse{Renewed: true, RenewAfterSeconds: renewAfterSeconds(ttl)}, nil
}

// renewAfterSeconds 把续租间隔换成应答里的秒数,至少 1 秒(0 在协议里表示「没有间隔」)。
func renewAfterSeconds(ttl time.Duration) uint32 {
	seconds := int64(loginsession.RenewAfter(ttl) / time.Second)
	if seconds < 1 {
		return 1
	}
	return uint32(seconds)
}
