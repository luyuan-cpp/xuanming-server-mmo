package logic

import (
	"crypto/subtle"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 运维口令。写法照 login 的 LoginAdmin(go/login/internal/server/loginadmin/admin_auth.go)。
//
// RetractWorldChat 是 CLIENT_RPC_SERVER_ONLY:入口准入(shared/roleidentity.EntryInterceptor)只挡带会话的客户端请求,
// 不带会话的内部调用一律放行,由方法自己核调用方。撤回是处置动作,不核的话集群里任何连得到本端口的进程都能撤任意一条世界消息。
const (
	// AdminTokenEnv 是运维口令所在的环境变量。口令只经 Secret 注入,不进配置文件。
	AdminTokenEnv = "MMORPG_CHAT_ADMIN_TOKEN"
	// AdminTokenMetadataKey 是口令所在的 gRPC metadata 键,必须全小写。
	AdminTokenMetadataKey = "x-admin-token"
	// MinAdminTokenBytes 是非空口令的最小长度,和 login、player_inbox 同值。
	MinAdminTokenBytes = 16
)

// ResolveAdminToken 读运维口令。没设 = 运维面停用(每次调用都回 PermissionDenied,不是免鉴权);设了但太短 = 拒启。
func ResolveAdminToken(getenv func(string) string) (string, error) {
	token := getenv(AdminTokenEnv)
	if token != "" && len(token) < MinAdminTokenBytes {
		return "", fmt.Errorf("%s is shorter than %d bytes", AdminTokenEnv, MinAdminTokenBytes)
	}
	return token, nil
}

// authorizeOps 核运维口令。比较用常数时间,口令值绝不进日志。
func (l *ChatLogic) authorizeOps(method string) error {
	want := l.svcCtx.AdminToken
	if want == "" {
		l.Errorf("[chat] %s refused: %s is not set, the ops surface is disabled", method, AdminTokenEnv)
		return status.Errorf(codes.PermissionDenied, "%s is disabled because %s is not set", method, AdminTokenEnv)
	}
	var got string
	if md, ok := metadata.FromIncomingContext(l.ctx); ok {
		if vals := md.Get(AdminTokenMetadataKey); len(vals) > 0 {
			got = vals[0]
		}
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		// 只记有没有带,绝不记口令本身。
		l.Errorf("[chat] %s refused: bad or missing %s (present=%t)", method, AdminTokenMetadataKey, got != "")
		return status.Errorf(codes.PermissionDenied, "%s requires a valid %s", method, AdminTokenMetadataKey)
	}
	return nil
}
