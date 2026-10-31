package logic

import (
	"context"
	"strings"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	base "proto/common/base"
	pb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func cancellationContext(t *testing.T) *svc.ServiceContext {
	t.Helper()
	secret := strings.Repeat("s", 32)
	verifier, err := serviceauth.New(map[string]string{"guild": secret})
	require.NoError(t, err)
	return &svc.ServiceContext{Authority: &authority.Store{}, ActivityCancelAuth: verifier, ActivityCancelSecret: secret}
}
func TestActivityCancellationRejectsProofFailureBeforeAnySQL(t *testing.T) {
	for _, mode := range []string{"unsigned", "wrong-caller", "wrong-method", "changed-body", "expired", "missing-auth", "client"} {
		t.Run(mode, func(t *testing.T) {
			sc := cancellationContext(t)
			original := activityRequest(11, 22)
			original.RequestId = 51
			original.ActivityContext.LaunchRequestId = 51
			in := &pb.CancelActivityBattleRequest{Original: original}
			ctx := context.Background()
			expected := codes.PermissionDenied
			if mode != "unsigned" {
				caller, method, stamp := "guild", pb.MatchInternal_CancelActivityBattle_FullMethodName, uint64(time.Now().UnixMilli())
				if mode == "wrong-caller" {
					caller = "match"
				}
				if mode == "wrong-method" {
					method += "/reply"
				}
				if mode == "expired" {
					stamp = 1
				}
				require.NoError(t, serviceauth.Sign(method, in, caller, sc.ActivityCancelSecret, stamp))
			}
			if mode == "changed-body" {
				in.Original.MemberPlayerIds[1] = 23
			}
			if mode == "missing-auth" {
				sc.ActivityCancelAuth = nil
				expected = codes.Unavailable
			}
			if mode == "client" {
				ctx = ctxkeys.WithSessionDetails(ctx, &base.SessionDetails{SessionId: 1, PlayerId: 11})
			}
			_, err := NewCancelActivityBattleLogic(ctx, sc).CancelActivityBattle(in)
			require.Equal(t, expected, status.Code(err))
		})
	}
}
func TestActivityCancellationRequiresPersistedLaunchIdentityEvenWithValidProof(t *testing.T) {
	sc := cancellationContext(t)
	original := activityRequest(11, 22)
	original.RequestId = 51
	in := &pb.CancelActivityBattleRequest{Original: original}
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CancelActivityBattle_FullMethodName, in, "guild", sc.ActivityCancelSecret, uint64(time.Now().UnixMilli())))
	_, err := NewCancelActivityBattleLogic(context.Background(), sc).CancelActivityBattle(in)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
