package loginsession

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	base "proto/common/base"
	"shared/callerauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type identityTestKey struct{}

func (identityTestKey) Sign(data []byte) []byte {
	m := hmac.New(sha256.New, []byte("仅供确定性测试的内部签名密钥"))
	m.Write(data)
	return m.Sum(nil)
}
func (s identityTestKey) Verify(mac, data []byte) bool { return hmac.Equal(mac, s.Sign(data)) }

func TestVerifiedOnlineIdentitySurvivesLoginRedisRemoval(t *testing.T) {
	in := &base.SessionDetails{SessionId: 7, GateInstanceId: "gate-A", SessionIncarnation: 19,
		PlayerId: 11, OwnerRevision: 3, AuthenticatedAccount: " 原账号 "}
	md, err := callerauth.SignedMetadataForDetails(identityTestKey{}, "gate", "/test/Online", in)
	require.NoError(t, err)
	v := callerauth.NewVerifier(callerauth.Options{Secrets: identityTestKey{}, Enforce: true})
	_, err = callerauth.SessionInterceptor(v)(metadata.NewIncomingContext(context.Background(), md), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Online"}, func(ctx context.Context, _ any) (any, error) {
			account, err := Account(ctx, nil, in)
			require.NoError(t, err, "已经认证的在线身份不能再依赖已删除的登录 Redis 键")
			require.Equal(t, in.AuthenticatedAccount, account)
			changed := proto.Clone(in).(*base.SessionDetails)
			changed.OwnerRevision++
			_, err = Account(ctx, nil, changed)
			require.Error(t, err, "业务层不能把原身份升级成新归属")
			changed = proto.Clone(in).(*base.SessionDetails)
			changed.GateInstanceId = "gate-after-restart"
			_, err = Account(ctx, nil, changed)
			require.Error(t, err, "重启后的连接不能继承旧认证")
			return nil, nil
		})
	require.NoError(t, err)
}

func TestUnverifiedOrTamperedOnlineAccountIsRejected(t *testing.T) {
	in := &base.SessionDetails{SessionId: 7, GateInstanceId: "gate-A", SessionIncarnation: 19,
		PlayerId: 11, OwnerRevision: 3, AuthenticatedAccount: "attacker"}
	r := &loginReader{value: "victim"}
	_, err := Account(context.Background(), r, in)
	require.Error(t, err)
	require.Zero(t, r.calls, "不能回查 Redis 把不可信声明洗成可信身份")
	md, err := callerauth.SignedMetadataForDetails(identityTestKey{}, "gate", "/test/Online", in)
	require.NoError(t, err)
	in.AuthenticatedAccount = "victim"
	raw, err := proto.Marshal(in)
	require.NoError(t, err)
	md.Set(callerauth.MetaSessionDetail, base64.StdEncoding.EncodeToString(raw))
	v := callerauth.NewVerifier(callerauth.Options{Secrets: identityTestKey{}, Enforce: true})
	_, err = callerauth.SessionInterceptor(v)(metadata.NewIncomingContext(context.Background(), md), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Online"}, func(context.Context, any) (any, error) {
			t.Fatal("篡改账号的请求不应进入业务")
			return nil, nil
		})
	require.Error(t, err)
}
