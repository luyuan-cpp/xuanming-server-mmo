package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"match/internal/config"

	matchpb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

// 工具签出来的请求必须能被服务端同样的校验放行:调用方是 ops,签名绑定 SetSpectateFeatured 这个方法与请求正文。
func TestNewSignedRequestVerifiesAsOpsForThisMethodOnly(t *testing.T) {
	secret := strings.Repeat("o", 32)
	verifier, err := serviceauth.New(map[string]string{config.SpectateOpsCaller: secret})
	require.NoError(t, err)

	request, err := newSignedRequest(42, true, secret, time.Now())
	require.NoError(t, err)
	require.Equal(t, uint64(42), request.GetBattleId())
	require.True(t, request.GetFeatured())
	caller, err := verifier.Verify(context.Background(), matchpb.MatchInternal_SetSpectateFeatured_FullMethodName, request)
	require.NoError(t, err)
	require.Equal(t, config.SpectateOpsCaller, caller)

	_, err = verifier.Verify(context.Background(), matchpb.MatchInternal_CancelActivityBattle_FullMethodName, request)
	require.Error(t, err, "签名绑定方法,不能拿去调别的内部方法")
	request.Featured = false
	_, err = verifier.Verify(context.Background(), matchpb.MatchInternal_SetSpectateFeatured_FullMethodName, request)
	require.Error(t, err, "签名绑定正文,改了 featured 不能再通过")
}

// 验签被拒是业务拒绝(服务端不回 gRPC 错误),工具必须打印明确错误并返回非零,不能当成功。
func TestReportReplyExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		reject     matchpb.SpectateFeaturedReject
		wantCode   int
		wantStderr string
	}{
		{name: "成功", reject: matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NONE, wantCode: 0},
		{name: "验签被拒", reject: matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_DENIED, wantCode: 1, wantStderr: opsCallerSecretEnv},
		{name: "局不存在", reject: matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NOT_FOUND, wantCode: 1},
		{name: "私密局", reject: matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_PRIVATE, wantCode: 1},
		{name: "内部错误", reject: matchpb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_INTERNAL, wantCode: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			require.Equal(t, c.wantCode, reportReply(&stdout, &stderr, &matchpb.SetSpectateFeaturedResponse{Reject: c.reject}))
			require.Contains(t, stdout.String(), c.reject.String())
			if c.wantStderr == "" {
				require.Empty(t, stderr.String())
			} else {
				require.Contains(t, stderr.String(), c.wantStderr)
			}
		})
	}
}

func TestNewSignedRequestRejectsBadInput(t *testing.T) {
	_, err := newSignedRequest(42, true, "", time.Now())
	require.Error(t, err, "空密钥")
	_, err = newSignedRequest(42, true, "too-short", time.Now())
	require.Error(t, err, "密钥不足 32 字节")
	_, err = newSignedRequest(0, true, strings.Repeat("o", 32), time.Now())
	require.Error(t, err, "battle_id 为 0")
}
