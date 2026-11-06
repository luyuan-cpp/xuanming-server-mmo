// Package callerauthtest 给各服务的单测造「经 gate 签名、已过验签拦截器」的入站上下文。
//
// 在线域的原身份只认 gate 签名的 SessionDetails(shared/roleidentity 的入口准入只读验签原件)。测试要么走真实的
// 签名 + callerauth.SessionInterceptor 得到带验签原件的 ctx(Incoming),要么只放会话 metadata、
// 不带原件(Unverified),用来证明未签名的声明进不了业务。密钥只在测试进程内使用。
package callerauthtest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	base "proto/common/base"
	"shared/callerauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Key 是测试专用的 HMAC 密钥,同时满足 callerauth.Signer 与 callerauth.SecretVerifier。
type Key struct{}

var testSecret = []byte("callerauthtest-only-deterministic-secret-0123456789")

func (Key) Sign(data []byte) []byte {
	m := hmac.New(sha256.New, testSecret)
	_, _ = m.Write(data)
	return m.Sum(nil)
}

func (k Key) Verify(mac, data []byte) bool { return hmac.Equal(mac, k.Sign(data)) }

// Incoming 按 gate 的签名契约给 detail 签 fullMethod,并让它通过 callerauth.SessionInterceptor。
// 返回的 ctx 同时带原始入站 metadata 与验签原件;在线域入口准入(roleidentity.EntryInterceptor)只认后者。
// detail 必须带 session_id、gate_instance_id、session_incarnation,否则验签拦截器拒绝,测试失败。
func Incoming(t testing.TB, fullMethod string, detail *base.SessionDetails) context.Context {
	t.Helper()
	md, err := callerauth.SignedMetadataForDetails(Key{}, "gate", fullMethod, detail)
	if err != nil {
		t.Fatalf("callerauthtest: 签名失败: %v", err)
	}
	verifier := callerauth.NewVerifier(callerauth.Options{Secrets: Key{}, Enforce: true, AllowedCallers: []string{"gate"}})
	var verified context.Context
	_, err = callerauth.SessionInterceptor(verifier)(metadata.NewIncomingContext(context.Background(), md), nil,
		&grpc.UnaryServerInfo{FullMethod: fullMethod}, func(ctx context.Context, _ any) (any, error) {
			verified = ctx
			return nil, nil
		})
	if err != nil || verified == nil {
		t.Fatalf("callerauthtest: 验签拦截器拒绝了测试身份: %v", err)
	}
	return verified
}

// Unverified 只带会话 metadata、不带签名与验签原件:模拟绕过 gate 签名直接自报身份的调用。
func Unverified(t testing.TB, detail *base.SessionDetails) context.Context {
	t.Helper()
	raw, err := proto.Marshal(detail)
	if err != nil {
		t.Fatalf("callerauthtest: 序列化会话失败: %v", err)
	}
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(callerauth.MetaSessionDetail, base64.StdEncoding.EncodeToString(raw)))
}

// OnlineSession 返回一份字段齐全的在线会话:已绑定角色、带原账号与原归属修订。
func OnlineSession(account string, playerID, ownerRevision uint64) *base.SessionDetails {
	return &base.SessionDetails{
		SessionId:            7,
		PlayerId:             playerID,
		GateInstanceId:       "callerauthtest-gate-uuid",
		SessionIncarnation:   19,
		OwnerRevision:        ownerRevision,
		AuthenticatedAccount: account,
	}
}
