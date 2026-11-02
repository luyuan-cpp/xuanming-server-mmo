package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/drainproof"
	"shared/rolefence"
	"shared/serverbase"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type roleFenceStatesFake struct {
	calls int
	state *rfpb.RoleFenceState
	err   error
}

func (f *roleFenceStatesFake) RoleFenceState(context.Context, uint64) (*rfpb.RoleFenceState, error) {
	f.calls++
	return f.state, f.err
}

func roleFenceTestServer(t *testing.T, fake *roleFenceStatesFake) *DataServiceServer {
	t.Helper()
	secrets := map[string]string{}
	for _, caller := range rolefence.Callers {
		secrets[caller] = directoryTestSecret
	}
	auth, err := serviceauth.New(secrets)
	require.NoError(t, err)
	return NewDataServiceServer(&svc.ServiceContext{RoleFenceStates: fake, RoleFenceAuth: auth})
}

func signedRoleFenceRequest(t *testing.T, caller string, player uint64) *dspb.GetRoleFenceStateRequest {
	t.Helper()
	in := &dspb.GetRoleFenceStateRequest{PlayerId: player}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"GetRoleFenceState", in, caller, directoryTestSecret, uint64(time.Now().UnixMilli())))
	return in
}

func TestGetRoleFenceStateAcceptsEveryKnownCaller(t *testing.T) {
	want := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: 101, Version: 2, HomeZone: 1}
	for _, caller := range rolefence.Callers {
		fake := &roleFenceStatesFake{state: want}
		out, err := roleFenceTestServer(t, fake).GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, caller, 11))
		require.NoError(t, err, caller)
		require.True(t, out.Found)
		require.False(t, out.Retry)
		require.True(t, proto.Equal(want, out.State))
	}
}

func TestGetRoleFenceStateRejectsBadProof(t *testing.T) {
	fake := &roleFenceStatesFake{state: &rfpb.RoleFenceState{PlayerId: 11}}
	s := roleFenceTestServer(t, fake)
	_, err := s.GetRoleFenceState(context.Background(), &dspb.GetRoleFenceStateRequest{PlayerId: 11})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "没签名")
	_, err = s.GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, "login", 11))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "login 不是调用方")
	changed := signedRoleFenceRequest(t, "guild", 11)
	changed.PlayerId = 12
	_, err = s.GetRoleFenceState(context.Background(), changed)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "改了正文")
	other := &dspb.GetRoleFenceStateRequest{PlayerId: 11}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"LookupAccountRole", other, "guild", directoryTestSecret, uint64(time.Now().UnixMilli())))
	_, err = s.GetRoleFenceState(context.Background(), other)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "别的方法的签名")
	require.Zero(t, fake.calls)
}

// 结论和读失败都带内回:只读,没有结局未知。回 gRPC 错误的话排空证明会被打成 Hold。
func TestGetRoleFenceStateAnswersInBand(t *testing.T) {
	missing, err := roleFenceTestServer(t, &roleFenceStatesFake{err: accountdirectory.ErrMissing}).GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, "guild", 11))
	require.NoError(t, err)
	require.False(t, missing.Found)
	require.False(t, missing.Retry, "目录确定没有这名角色,不是没读成")
	require.Nil(t, missing.State)

	failed, err := roleFenceTestServer(t, &roleFenceStatesFake{err: errors.New("private SQL failure")}).GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, "guild", 11))
	require.NoError(t, err)
	require.True(t, failed.Retry)
	require.False(t, failed.Found, "读不到不能当成有")

	_, err = roleFenceTestServer(t, &roleFenceStatesFake{err: accountdirectory.ErrInvalid}).GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, "guild", 11))
	require.Equal(t, codes.InvalidArgument, status.Code(err), "调用方缺陷")
}

// requireNoDrainHold 经真的发布准入跑一次 handler,核排空证明没有被打成 Hold。三个方法都在 relay 清单里。
func requireNoDrainHold(t *testing.T, method string, req any, handler grpc.UnaryHandler) {
	t.Helper()
	root, relay := drainproof.New(), drainproof.New()
	admission := serverbase.PublicationAdmission(root, relay, dataServiceReadMethodsForHoldTest)
	_, err := admission(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: method}, handler)
	require.NoError(t, err)
	relay.CloseAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, relay.Wait(ctx))
}

var dataServiceReadMethodsForHoldTest = map[string]bool{
	accountdirectory.MethodPrefix + "GetRoleFenceState":   true,
	accountdirectory.MethodPrefix + "VerifyRoleAdmission": true,
	accountdirectory.MethodPrefix + "GetPlayerRoute":      true,
}

// 规格 §3.6:不存在的角色、读失败都不触发 HOLD。
func TestGetRoleFenceStateOutcomesDoNotHoldDrain(t *testing.T) {
	for name, storeErr := range map[string]error{"不存在": accountdirectory.ErrMissing, "库故障": errors.New("private SQL failure")} {
		t.Run(name, func(t *testing.T) {
			s := roleFenceTestServer(t, &roleFenceStatesFake{err: storeErr})
			requireNoDrainHold(t, accountdirectory.MethodPrefix+"GetRoleFenceState", signedRoleFenceRequest(t, "guild", 11), func(ctx context.Context, req any) (any, error) {
				return s.GetRoleFenceState(ctx, req.(*dspb.GetRoleFenceStateRequest))
			})
		})
	}
}

type roleAdmissionChecksFake struct {
	valid bool
	err   error
	seen  *base.RoleAdmissionIdentity
}

func (f *roleAdmissionChecksFake) VerifyRoleAdmission(_ context.Context, admission *base.RoleAdmissionIdentity) (bool, error) {
	f.seen = admission
	return f.valid, f.err
}

func signedAdmissionCheck(t *testing.T, caller string, admission *base.RoleAdmissionIdentity) *dspb.VerifyRoleAdmissionRequest {
	t.Helper()
	in := &dspb.VerifyRoleAdmissionRequest{Admission: admission}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+"VerifyRoleAdmission", in, caller, directoryTestSecret, uint64(time.Now().UnixMilli())))
	return in
}

func TestVerifyRoleAdmissionAnswersKnownCallersAndRejectsBadProof(t *testing.T) {
	secrets := map[string]string{}
	for _, caller := range rolefence.Callers {
		secrets[caller] = directoryTestSecret
	}
	auth, err := serviceauth.New(secrets)
	require.NoError(t, err)
	admission := &base.RoleAdmissionIdentity{PlayerId: 11, Generation: 3}
	fake := &roleAdmissionChecksFake{valid: true}
	s := NewDataServiceServer(&svc.ServiceContext{RoleAdmissionChecks: fake, RoleFenceAuth: auth})
	out, err := s.VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "match", admission))
	require.NoError(t, err)
	require.True(t, out.Valid)
	require.True(t, proto.Equal(admission, fake.seen))
	fake.valid = false
	out, err = s.VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "match", admission))
	require.NoError(t, err)
	require.False(t, out.Valid, "登记失效是业务答复,不是错误")
	_, err = s.VerifyRoleAdmission(context.Background(), &dspb.VerifyRoleAdmissionRequest{Admission: admission})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "login", admission))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	fake.err = accountdirectory.ErrInvalid
	_, err = s.VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "match", admission))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	fake.err = errors.New("private SQL failure")
	out, err = s.VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "match", admission))
	require.NoError(t, err)
	require.True(t, out.Retry, "读不到带内回,不能当成登记失效")
	require.False(t, out.Valid)
	requireNoDrainHold(t, accountdirectory.MethodPrefix+"VerifyRoleAdmission", signedAdmissionCheck(t, "match", admission), func(ctx context.Context, req any) (any, error) {
		return s.VerifyRoleAdmission(ctx, req.(*dspb.VerifyRoleAdmissionRequest))
	})
	_, err = NewDataServiceServer(&svc.ServiceContext{}).VerifyRoleAdmission(context.Background(), signedAdmissionCheck(t, "match", admission))
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// 调用方已放弃不等于证明无效:和读失败一样带内回 retry,不报成鉴权失败。这时什么都没读。
func TestRoleFenceReadsCanceledAnswerRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	states := &roleFenceStatesFake{state: &rfpb.RoleFenceState{PlayerId: 11}}
	state, err := roleFenceTestServer(t, states).GetRoleFenceState(ctx, signedRoleFenceRequest(t, "guild", 11))
	require.NoError(t, err)
	require.True(t, state.Retry)
	require.False(t, state.Found)
	require.Zero(t, states.calls)

	auth, err := serviceauth.New(map[string]string{"match": directoryTestSecret})
	require.NoError(t, err)
	checks := &roleAdmissionChecksFake{valid: true}
	s := NewDataServiceServer(&svc.ServiceContext{RoleAdmissionChecks: checks, RoleFenceAuth: auth})
	admission, err := s.VerifyRoleAdmission(ctx, signedAdmissionCheck(t, "match", &base.RoleAdmissionIdentity{PlayerId: 11, Generation: 3}))
	require.NoError(t, err)
	require.True(t, admission.Retry)
	require.False(t, admission.Valid)
	require.Nil(t, checks.seen)
}

func TestGetRoleFenceStateUnavailableUntilAssembled(t *testing.T) {
	s := NewDataServiceServer(&svc.ServiceContext{})
	_, err := s.GetRoleFenceState(context.Background(), signedRoleFenceRequest(t, "guild", 11))
	require.Equal(t, codes.Unavailable, status.Code(err))
}
