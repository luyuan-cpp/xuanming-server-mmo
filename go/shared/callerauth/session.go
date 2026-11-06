package callerauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	base "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SecretEnv 与既有部署的内部身份密钥同源；不复用客户端 gate 票据密钥。
const SecretEnv = "MMORPG_INTERNAL_AUTH_SECRET"

// 在线域验签闸门的指标。label 只放 Reason 的有限枚举，绝不放 caller 或玩家标识（低基数纪律）。
// 稳态下 result!="ok" 应恒 0：非 0 说明 gate 与服务密钥不一致、时钟漂移超窗、签名格式被改写或有伪造调用。
var (
	sessionVerifyTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "caller_auth",
		Name:      "session_verify_total",
		Help:      "带会话身份的内部请求验签结果；result 取值见 callerauth.Reason。",
		Labels:    []string{"result"},
	})
	sessionNonceOverflowTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "caller_auth",
		Name:      "nonce_overflow_total",
		Help:      "nonce 表超过上限被迫提前翻代的次数；非 0 表示本进程的重放窗口被临时缩短。",
		Labels:    []string{},
	})
)

type authenticatedSessionKey struct{}

// AuthenticateContext 仅在验签成功后保存不可变身份副本；普通 metadata 解码不能制造这个标记。
// 失败仍返回可解码的声明，供 login 既有 dev/test 兼容适配器诊断；它不具有已认证标记。
func (v *Verifier) AuthenticateContext(ctx context.Context, method string) (context.Context, *base.SessionDetails, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(MetaSessionDetail)) != 1 {
		return ctx, nil, ErrBadPayload
	}
	raw, err := base64.StdEncoding.DecodeString(md.Get(MetaSessionDetail)[0])
	if err != nil || len(raw) > 4096 {
		return ctx, nil, ErrBadPayload
	}
	detail := &base.SessionDetails{}
	if proto.Unmarshal(raw, detail) != nil {
		return ctx, nil, ErrBadPayload
	}
	for _, key := range []string{MetaCaller, MetaTimestamp, MetaNonce, MetaSignature} {
		if len(md.Get(key)) > 1 {
			return ctx, detail, ErrBadPayload
		}
	}
	_, verified := v.Verify(md, method, raw)
	if verified != nil {
		return ctx, detail, verified
	}
	if detail.SessionId == 0 || detail.GateInstanceId == "" || detail.SessionIncarnation == 0 {
		return ctx, nil, ErrBadPayload
	}
	return context.WithValue(ctx, authenticatedSessionKey{}, proto.Clone(detail)), detail, nil
}

// AuthenticatedSession 返回验签原件副本，防止业务的局部修改改变已认证事实。
func AuthenticatedSession(ctx context.Context) (*base.SessionDetails, bool) {
	detail, ok := ctx.Value(authenticatedSessionKey{}).(*base.SessionDetails)
	if !ok {
		return nil, false
	}
	return proto.Clone(detail).(*base.SessionDetails), true
}

// SessionInterceptor 只核带会话的内部请求；无会话的管理/后台方法继续走各自原准入。
// 原始 metadata 留给各服务已有的方法白名单和解码适配器，不将未验声明写成客户端 actor。
func SessionInterceptor(v *Verifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get(MetaSessionDetail)) == 0 {
			return next(ctx, request)
		}
		verified, _, err := v.AuthenticateContext(ctx, info.FullMethod)
		sessionVerifyTotal.Inc(Reason(err))
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "内部会话身份验证失败")
		}
		return next(verified, request)
	}
}

type gateSecrets struct{ keys [][]byte }

func (s gateSecrets) Verify(mac, data []byte) bool {
	for _, key := range s.keys {
		m := hmac.New(sha256.New, key)
		_, _ = m.Write(data)
		if hmac.Equal(mac, m.Sum(nil)) {
			return true
		}
	}
	return false
}

// GateInterceptor 从装配层注入的环境读取固定用途密钥，启动时缺失即拒绝。
// Additional 只验不签，逗号分隔环境变量名，支持先分发验钥再切签钥的轮换。
func GateInterceptor(getenv func(string) string) (grpc.UnaryServerInterceptor, error) {
	if getenv == nil {
		return nil, errors.New("内部会话验签缺少环境读取器")
	}
	names := []string{SecretEnv}
	if additional := strings.TrimSpace(getenv(SecretEnv + "_ADDITIONAL_ENVS")); additional != "" {
		names = append(names, strings.Split(additional, ",")...)
	}
	secrets := gateSecrets{}
	for _, name := range names {
		key := getenv(strings.TrimSpace(name))
		if len([]byte(key)) < 32 || key != strings.TrimSpace(key) {
			return nil, errors.New("内部会话验签密钥未注入或不足32字节")
		}
		secrets.keys = append(secrets.keys, []byte(key))
	}
	return SessionInterceptor(NewVerifier(Options{Secrets: secrets, Enforce: true, AllowedCallers: []string{"gate"},
		// 超限提前翻代只会缩短重放窗口、不会放行验签失败的请求；必须看得见，才知道该调上限还是有异常流量。
		OnNonceOverflow: func() {
			sessionNonceOverflowTotal.Inc()
			logx.Error("callerauth: nonce 表超过上限被迫提前翻代，本进程重放窗口临时缩短")
		},
	})), nil
}
