package logic

import (
	"context"
	"strings"
	"testing"
	"time"

	matchpb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type cancellationRPCFixture struct {
	mode, secret string
	t            *testing.T
}

func (f cancellationRPCFixture) CancelActivityBattle(ctx context.Context, in *matchpb.CancelActivityBattleRequest, _ ...grpc.CallOption) (*matchpb.CancelActivityBattleResponse, error) {
	verifier, err := serviceauth.New(map[string]string{"guild": f.secret})
	require.NoError(f.t, err)
	method := matchpb.MatchInternal_CancelActivityBattle_FullMethodName
	caller, err := verifier.Verify(ctx, method, in)
	require.NoError(f.t, err)
	require.Equal(f.t, "guild", caller)
	digest, err := serviceauth.Digest(in)
	require.NoError(f.t, err)
	out := &matchpb.CancelActivityBattleResponse{RequestCancelled: true, RequestDigest: digest}
	if f.mode == "wrong-original" {
		out.RequestDigest = strings.Repeat("0", 64)
	}
	if f.mode != "unsigned" {
		replyMethod := method + "/reply"
		if f.mode == "wrong-method" {
			replyMethod = method
		}
		require.NoError(f.t, serviceauth.Sign(replyMethod, out, "match", f.secret, uint64(time.Now().UnixMilli())))
	}
	if f.mode == "tampered" {
		out.BattleId = 9
	}
	return out, nil
}

type startRPCFixture struct {
	got *matchpb.StartActivityBattleRequest
}

func (f *startRPCFixture) StartActivityBattle(_ context.Context, in *matchpb.StartActivityBattleRequest, _ ...grpc.CallOption) (*matchpb.StartActivityBattleResponse, error) {
	f.got = in
	return &matchpb.StartActivityBattleResponse{BattleId: 900}, nil
}

// match 读不到 guild 的库,开局原件由 guild 的签名担保。
func TestSignedTrialStarterSignsTheOriginalRequest(t *testing.T) {
	secret := strings.Repeat("k", 32)
	rpc := &startRPCFixture{}
	starter, err := NewSignedTrialStarter(rpc, secret)
	require.NoError(t, err)
	out, err := starter.StartActivityBattle(context.Background(), &matchpb.StartActivityBattleRequest{RequestId: 51, BattleConfigId: 1, MemberPlayerIds: []uint64{11, 22}})
	require.NoError(t, err)
	require.EqualValues(t, 900, out.BattleId)
	verifier, err := serviceauth.New(map[string]string{"guild": secret})
	require.NoError(t, err)
	caller, err := verifier.Verify(context.Background(), matchpb.MatchInternal_StartActivityBattle_FullMethodName, rpc.got)
	require.NoError(t, err)
	require.Equal(t, "guild", caller)
	_, err = NewSignedTrialStarter(nil, secret)
	require.Error(t, err)
	_, err = NewSignedTrialStarter(rpc, "short")
	require.Error(t, err)
}

func TestTrialCancellationVerifiesReplySignatureAndOriginalRequestDigest(t *testing.T) {
	for _, mode := range []string{"valid", "unsigned", "wrong-original", "wrong-method", "tampered"} {
		t.Run(mode, func(t *testing.T) {
			secret := strings.Repeat("k", 32)
			client, err := NewTrialCancellation(cancellationRPCFixture{mode: mode, secret: secret, t: t}, secret)
			require.NoError(t, err)
			out, err := client.CancelOriginal(context.Background(), &matchpb.StartActivityBattleRequest{RequestId: 51, BattleConfigId: 1, MemberPlayerIds: []uint64{11, 22}})
			if mode == "valid" {
				require.NoError(t, err)
				require.True(t, out.RequestCancelled)
				require.Zero(t, out.BattleId)
			} else {
				require.ErrorIs(t, err, serviceauth.ErrDenied)
			}
		})
	}
}
