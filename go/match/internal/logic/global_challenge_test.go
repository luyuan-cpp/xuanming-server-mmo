package logic

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"match/internal/authority"
	"match/internal/constants"
	"match/internal/svc"
	dspb "proto/data_service"
	pb "proto/match"
	"shared/rolefence"
	"shared/serverbase"
)

func TestGlobalChallengeNoticeDoesNotExposePrivateOriginalAccount(t *testing.T) {
	invitation := &pb.MatchChallengeInvitationRecord{ChallengeId: 9, ChallengerPlayerId: 1, TargetPlayerId: 2, ConfigId: 7, ChallengerAccount: "private-original-login", ChallengerRevision: 3, TargetAccount: "private-target-login", TargetRevision: 4, ExpiresMs: 123456}
	shown := globalChallengeInvitationNotice(invitation, "公开展示名")
	require.Equal(t, "公开展示名", shown.ChallengerName)
	require.Equal(t, invitation.ChallengeId, shown.ChallengeId)
	require.Equal(t, invitation.ConfigId, shown.BattleConfigId)
	require.Empty(t, globalChallengeInvitationNotice(invitation, "").ChallengerName, "公开来源缺失也不能回退到私有原账号")
}

// 规格 §3.6:自愈读不到目录时事务已回滚,回非故障 tip,排空证明不能 Hold。走到提交的失败才是「尚未确认」,照旧是故障。
func TestGlobalChallengeCreateFailureKeepsUnknownApart(t *testing.T) {
	l := &ChallengeLogic{Logger: logx.WithContext(context.Background())}
	for name, tc := range map[string]struct {
		err  error
		want uint32
	}{
		"已有邀请":   {authority.ErrChallengePending, constants.ErrChallengePending},
		"归属变了":   {authority.ErrOriginalPlayer, constants.ErrChallengeExpired},
		"目录读不到":  {errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")), constants.ErrChallengeExpired},
		"提交结局未知": {errors.New("commit failed"), constants.ErrInternal},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, l.globalChallengeCreateFailure(tc.err).GetErrorMessage().GetId())
		})
	}
	require.Equal(t, serverbase.VerdictBizReject, serverbase.TipVerdict(constants.ErrChallengeExpired))
	require.Equal(t, serverbase.VerdictFault, serverbase.TipVerdict(constants.ErrInternal))
}

type challengeNameClient struct {
	dspb.DataServiceClient
	response    *dspb.BatchGetPlayerNameResponse
	err         error
	requested   []uint64
	hasDeadline bool
}

func (c *challengeNameClient) BatchGetPlayerName(ctx context.Context, in *dspb.BatchGetPlayerNameRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error) {
	c.requested = append([]uint64(nil), in.PlayerIds...)
	_, c.hasDeadline = ctx.Deadline()
	return c.response, c.err
}

func TestChallengePublicNameUsesOnlyBoundedRoleNameLookup(t *testing.T) {
	client := &challengeNameClient{response: &dspb.BatchGetPlayerNameResponse{Names: map[uint64]string{1: "公开角色名", 2: "其他角色"}}}
	s := &svc.ServiceContext{DataServiceClient: client}
	require.Equal(t, "公开角色名", challengePublicName(context.Background(), s, 1))
	require.Equal(t, []uint64{1}, client.requested)
	require.True(t, client.hasDeadline)
	client.err = errors.New("fixture lookup unavailable")
	require.Empty(t, challengePublicName(context.Background(), s, 1))
	require.Empty(t, challengePublicName(context.Background(), &svc.ServiceContext{}, 1))
}
