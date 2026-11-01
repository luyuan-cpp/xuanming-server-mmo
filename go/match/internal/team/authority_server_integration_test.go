package team

import (
	"context"
	"testing"
	"time"

	"match/internal/svc"

	base "proto/common/base"
	teampb "proto/team"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/serviceauth"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 目录替身:只认一份登记,关闭之后不再有效。原件逐字段比较的真库口径在 data_service 的 TestTiDBRoleAdmissionVerifyFollowsGenerationAndClose。
type fixtureAdmissions struct {
	registered *base.RoleAdmissionIdentity
	closed     bool
	err        error
}

func (a *fixtureAdmissions) VerifyRoleAdmission(_ context.Context, admission *base.RoleAdmissionIdentity) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	return !a.closed && proto.Equal(a.registered, admission), nil
}

func TestTeamTiDBSceneMembershipRequiresRegisteredAdmissionAndSignedReply(t *testing.T) {
	f := newNativeTeamFixture(t)
	f.create(t, 1, 101)
	admission := &base.RoleAdmissionIdentity{PlayerId: 1, SceneInstanceId: uuid.NewString(), Generation: 5, OwnerRevision: 7, OwnerEpoch: 9, SceneNodeId: "scene-original", SceneId: 1001, ZoneId: 1, SessionId: 2, LoginGeneration: 3}
	admissions := &fixtureAdmissions{registered: admission}
	require.NoError(t, f.owner.ConfigureRoleFence(context.Background(), fencetest.ReplicaReader{DB: f.owner.DB}, admissions))
	secret := "fixture-only-scene-membership-secret-00000000"
	verifier, err := serviceauth.New(map[string]string{"scene": secret})
	require.NoError(t, err)
	server := &AuthorityServer{svc: &svc.ServiceContext{Authority: f.owner}, verifier: verifier, secret: secret}
	request := &teampb.ReadTeamMembershipRequest{Admission: admission}
	require.NoError(t, serviceauth.Sign(teamMembershipMethod, request, "scene", secret, uint64(time.Now().UnixMilli())))
	response, err := server.ReadMembership(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, uint64(101), response.TeamId)
	require.Equal(t, uint64(1), response.Version)
	digest, err := serviceauth.Digest(request)
	require.NoError(t, err)
	require.Equal(t, digest, response.RequestDigest)
	replyVerifier, err := serviceauth.New(map[string]string{"team": secret})
	require.NoError(t, err)
	caller, err := replyVerifier.Verify(context.Background(), teamMembershipMethod+"/reply", response)
	require.NoError(t, err)
	require.Equal(t, "team", caller)
	clientCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-session-detail-bin", "invalid-old-session"))
	_, err = server.ReadMembership(clientCtx, request)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// 读不成都带内回 retry:只读校验没有结局未知,回 gRPC 错误会把排空证明打成 Hold。回包照样签名,队伍字段是空的。
	requireRetry := func(in *teampb.ReadTeamMembershipRequest, message string) {
		t.Helper()
		refused, err := server.ReadMembership(context.Background(), in)
		require.NoError(t, err, message)
		require.True(t, refused.Retry, message)
		require.Zero(t, refused.TeamId, message)
		require.Zero(t, refused.Version, message)
		require.Nil(t, refused.Record, message)
		refusedDigest, err := serviceauth.Digest(in)
		require.NoError(t, err, message)
		require.Equal(t, refusedDigest, refused.RequestDigest, message)
		caller, err := replyVerifier.Verify(context.Background(), teamMembershipMethod+"/reply", refused)
		require.NoError(t, err, message)
		require.Equal(t, "team", caller, message)
	}
	// 即使是真scene重签，旧E/旧generation也不能通过已登记原件的完整比较。
	old := proto.Clone(request).(*teampb.ReadTeamMembershipRequest)
	old.Admission.OwnerEpoch = 8
	require.NoError(t, serviceauth.Sign(teamMembershipMethod, old, "scene", secret, uint64(time.Now().UnixMilli())))
	requireRetry(old, "旧世代")
	admissions.closed = true
	requireRetry(request, "读之后才问目录:关闭之后读到的队伍不能回给 scene")
	// 目录这一次问不到不是登记失效,同样不回队伍。
	admissions.closed, admissions.err = false, rolefence.ErrUnavailable
	requireRetry(request, "目录读不到")
	admissions.err = nil
	// 冻结和过户在本库副本上就拒绝,不问目录。
	allowed, err := server.ReadMembership(context.Background(), request)
	require.NoError(t, err)
	require.False(t, allowed.Retry)
	fencetest.Update(t, context.Background(), f.owner.DB, 1, "team-original-1", 7, 901)
	requireRetry(request, "冻结")
	fencetest.Update(t, context.Background(), f.owner.DB, 1, "buyer", 8, 0)
	requireRetry(request, "过户")
}
