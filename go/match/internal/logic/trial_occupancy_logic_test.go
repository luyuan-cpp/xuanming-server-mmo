package logic

import (
	"context"
	"testing"
	"time"

	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	base "proto/common/base"
	pb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

// 占用的三个 RPC 验签都在碰 SQL 之前。这里的 ServiceContext 没有 Authority,放行的请求走到 SQL 就 panic。
func TestTrialOccupancyRPCsRejectBadProofInBand(t *testing.T) {
	const secret = "fixture-only-guild-call-secret-00000000"
	verifier, err := serviceauth.New(map[string]string{"guild": secret})
	require.NoError(t, err)
	denied := pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED
	now := uint64(time.Now().UnixMilli())

	for name, setup := range map[string]func() (context.Context, *svc.ServiceContext){
		"没配密钥": func() (context.Context, *svc.ServiceContext) { return context.Background(), &svc.ServiceContext{} },
		"验不过": func() (context.Context, *svc.ServiceContext) {
			return context.Background(), &svc.ServiceContext{GuildCallAuth: verifier}
		},
		"客户端来源": func() (context.Context, *svc.ServiceContext) {
			return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 9601}), &svc.ServiceContext{GuildCallAuth: verifier}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sc := setup()
			acquire := &pb.AcquireTrialOccupancyRequest{OwnerId: 51, Roster: []*pb.TrialOccupancyRole{{PlayerId: 9601}}, TtlMs: 1000}
			release := &pb.ReleaseTrialOccupancyRequest{OwnerId: 51, PlayerIds: []uint64{9601}}
			completion := &pb.GetBattleCompletionRequest{BattleId: 900}
			if name == "客户端来源" {
				require.NoError(t, serviceauth.Sign(pb.MatchInternal_AcquireTrialOccupancy_FullMethodName, acquire, "guild", secret, now))
				require.NoError(t, serviceauth.Sign(pb.MatchInternal_ReleaseTrialOccupancy_FullMethodName, release, "guild", secret, now))
				require.NoError(t, serviceauth.Sign(pb.MatchInternal_GetBattleCompletion_FullMethodName, completion, "guild", secret, now))
			}
			acquired, err := AcquireTrialOccupancy(ctx, sc, acquire)
			require.NoError(t, err, "业务拒绝走 reject,不回 gRPC 错误")
			require.Equal(t, denied, acquired.GetReject())
			released, err := ReleaseTrialOccupancy(ctx, sc, release)
			require.NoError(t, err)
			require.Equal(t, denied, released.GetReject())
			read, err := GetBattleCompletion(ctx, sc, completion)
			require.NoError(t, err)
			require.Equal(t, denied, read.GetReject())
		})
	}
}
