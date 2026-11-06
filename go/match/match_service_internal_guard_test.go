package main

// match 入口准入(docs/design/gate-online-identity.md §9.2):match 进程里的服务按生成的客户端方法集合分两类。
// 客户端协议(MatchService、ClientPlayerTeam)必须带 gate 签名的完整原身份;内部服务(MatchInternal、TeamAuthority、
// RoleFenceParticipant)带会话一律 PermissionDenied、不进 handler,不带会话交给各自的调用方准入(serviceauth)。
// 直接调 match_service.go 装配的同一个入口拦截器,不起 gRPC server。

import (
	"context"
	"testing"

	"match/internal/pkg/ctxkeys"

	base "proto/common/base"
	matchpb "proto/match"
	rfpb "proto/role_fence"
	teampb "proto/team"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/roleidentity"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// recordingHandler 记录是否被调用,以及下游看到的会话与原角色归属。
type recordingHandler struct {
	called   bool
	session  *base.SessionDetails
	original bool
}

func (h *recordingHandler) handle(ctx context.Context, _ any) (any, error) {
	h.called = true
	h.session, _ = ctxkeys.GetSessionDetails(ctx)
	_, h.original = roleidentity.Original(ctx)
	return nil, nil
}

func runMatchEntry(t *testing.T, ctx context.Context, fullMethod string) (*recordingHandler, codes.Code) {
	t.Helper()
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
	require.NoError(t, err)
	h := &recordingHandler{}
	_, err = entry(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, h.handle)
	return h, status.Code(err)
}

// 生成的客户端方法集合必须把 match 进程里的服务分对:推送与内部服务都不是客户端可发起方法。
func TestMatchServicesClassifiedByGeneratedClientSet(t *testing.T) {
	for _, method := range []string{
		matchpb.MatchService_JoinQueue_FullMethodName,
		teampb.ClientPlayerTeam_CreateTeam_FullMethodName,
	} {
		require.True(t, clientrpc.Callable(method), "客户端协议方法: %s", method)
	}
	for _, method := range []string{
		matchpb.MatchService_NotifyChallengeInvite_FullMethodName,
		teampb.ClientPlayerTeam_NotifyTeamSnapshot_FullMethodName,
		matchpb.MatchInternal_StartActivityBattle_FullMethodName,
		matchpb.MatchInternal_CancelActivityBattle_FullMethodName,
		matchpb.MatchInternal_ListStrandedFormations_FullMethodName,
		matchpb.MatchInternal_ResolveStrandedFormation_FullMethodName,
		teampb.TeamAuthority_ReadMembership_FullMethodName,
		rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName,
	} {
		require.False(t, clientrpc.Callable(method), "推送或内部方法: %s", method)
	}
}

// 客户端调 MatchService 带完整原身份:放行,验签原件进 ctx,原角色归属已捕获。
func TestMatchEntryAdmitsSignedClientSession(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 42, 9)
	h, code := runMatchEntry(t, callerauthtest.Incoming(t, matchpb.MatchService_JoinQueue_FullMethodName, in),
		matchpb.MatchService_JoinQueue_FullMethodName)
	require.Equal(t, codes.OK, code)
	require.True(t, h.called)
	require.NotNil(t, h.session)
	require.True(t, proto.Equal(in, h.session), "原会话字段完整保留")
	require.True(t, h.original)
}

// 离场后的会话(player_id 还在、账号与修订已清空)、以及不带会话调客户端方法:一律 PermissionDenied。
func TestMatchEntryRejectsClientMethodWithoutCompleteIdentity(t *testing.T) {
	left := &base.SessionDetails{SessionId: 7, PlayerId: 42, GateInstanceId: "gate-a", SessionIncarnation: 19}
	for name, ctx := range map[string]context.Context{
		"离场后的会话": callerauthtest.Incoming(t, teampb.ClientPlayerTeam_CreateTeam_FullMethodName, left),
		"不带会话":   context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			h, code := runMatchEntry(t, ctx, teampb.ClientPlayerTeam_CreateTeam_FullMethodName)
			require.Equal(t, codes.PermissionDenied, code)
			require.False(t, h.called)
		})
	}
}

// 带会话调 MatchInternal / TeamAuthority / RoleFenceParticipant:即使签名有效也 PermissionDenied,handler 未调用。
// 成局人工处置的两个运维方法(2026-11-05)在入口这一层同样拦下,logic 入口的会话检查只是第二道。
func TestMatchEntryRejectsClientCallToInternalServices(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 42, 9)
	for _, method := range []string{
		matchpb.MatchInternal_StartActivityBattle_FullMethodName,
		matchpb.MatchInternal_ListStrandedFormations_FullMethodName,
		matchpb.MatchInternal_ResolveStrandedFormation_FullMethodName,
		teampb.TeamAuthority_ReadMembership_FullMethodName,
		rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName,
	} {
		t.Run(method, func(t *testing.T) {
			h, code := runMatchEntry(t, callerauthtest.Incoming(t, method, in), method)
			require.Equal(t, codes.PermissionDenied, code)
			require.False(t, h.called)
		})
	}
}

// 会话头没有验签原件(GateInterceptor 没装在前面才会出现)也不能退化成「当内部调用放行」。
func TestMatchEntryRejectsUnverifiedSessionOnMatchInternal(t *testing.T) {
	in := callerauthtest.OnlineSession("原账号", 42, 9)
	h, code := runMatchEntry(t, callerauthtest.Unverified(t, in), matchpb.MatchInternal_StartActivityBattle_FullMethodName)
	require.Equal(t, codes.Unauthenticated, code)
	require.False(t, h.called)
}

// 不带会话(guild 直连的内部调用):放行,handler 眼里没有会话,也造不出客户端 actor。
func TestMatchEntryAllowsInternalCallWithoutSession(t *testing.T) {
	h, code := runMatchEntry(t, context.Background(), matchpb.MatchInternal_CancelActivityBattle_FullMethodName)
	require.Equal(t, codes.OK, code)
	require.True(t, h.called)
	require.Nil(t, h.session)
	require.False(t, h.original)
}
