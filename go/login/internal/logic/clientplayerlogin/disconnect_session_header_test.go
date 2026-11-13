package clientplayerloginlogic

// Disconnect 的会话号以会话头为准(docs/design/client-request-ordering.md §6.2,2026-11-12)。
// 会话头(SessionDetails)由 gate 签名、callerauth 验过才进 ctx;请求体的 session_id 不在签名范围内。
// 改之前断线租约按请求体的 session_id 挂,改之后只用会话头的值,请求体填了且不一致就整条拒绝。
// 夹具沿用 session_fence_wiring_test.go 的 newFenceWiring(miniredis + 假 player_locator)。

import (
	"testing"

	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/sessionfence"
	pbbase "proto/common/base"
	login_proto "proto/login"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDisconnect_SettlesWithTheSessionHeaderSessionID(t *testing.T) {
	for name, body := range map[string]uint32{
		"body left empty":        0,
		"body equals the header": fenceWiringSessionID,
	} {
		t.Run(name, func(t *testing.T) {
			svcCtx, _, locator := newFenceWiring(t)
			ctx := fenceWiringCtx(fenceWiringPlayerID)

			_, err := NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: body})
			require.NoError(t, err)
			require.Len(t, locator.disconnecting, 1)
			require.Equal(t, fenceWiringPlayerID, locator.disconnecting[0].GetPlayerId())
			require.Equal(t, fenceWiringSessionID, locator.disconnecting[0].GetSessionId(),
				"断线租约必须按会话头的 session_id 挂(player_locator 据它做会话 CAS)")
			require.Equal(t, fenceWiringGate, locator.disconnecting[0].GetGateInstanceId())
		})
	}
}

func TestDisconnect_RejectsABodySessionIDThatDiffersFromTheHeader(t *testing.T) {
	svcCtx, rdb, locator := newFenceWiring(t)
	ctx := fenceWiringCtx(fenceWiringPlayerID)

	// 这条连接已登录、已绑定角色:被拒的 Disconnect 若照常收尾,会关栅栏、删登录态、替角色挂租约。
	_, gen, err := sessionfence.CheckEnterGame(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, sessionFenceTTL)
	require.NoError(t, err)
	outcome, err := sessionfence.BindPlayer(ctx, rdb, fenceWiringKey, fenceWiringPlayerID, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome)
	details, ok := ctxkeys.GetSessionDetails(ctx)
	require.True(t, ok)
	ref, ok := loginsession.RefOf(details)
	require.True(t, ok)
	require.NoError(t, rdb.Set(ctx, ref.Key(), "fence-wiring-account", 0).Err())

	_, err = NewDisconnectLogic(ctx, svcCtx).Disconnect(&login_proto.LoginNodeDisconnectRequest{SessionId: fenceWiringSessionID + 1})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "请求体与会话头不一致必须拒绝: %v", err)

	require.Empty(t, locator.disconnectingPlayers(), "被拒的请求不得替任何玩家挂断线租约")
	exists, err := rdb.Exists(ctx, ref.Key()).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, exists, "被拒的请求不得删登录态")
	outcome, err = sessionfence.Recheck(ctx, rdb, fenceWiringKey, gen)
	require.NoError(t, err)
	require.Equal(t, sessionfence.OutcomeOK, outcome, "被拒的请求不得关栅栏")
}

func TestDisconnectSessionID(t *testing.T) {
	header := &pbbase.SessionDetails{SessionId: 5, GateInstanceId: "gate-a", SessionIncarnation: 1}
	for name, tc := range map[string]struct {
		body   uint32
		want   uint32
		wantOK bool
	}{
		"body empty uses the header": {body: 0, want: 5, wantOK: true},
		"body equals the header":     {body: 5, want: 5, wantOK: true},
		"body differs is rejected":   {body: 6, want: 0, wantOK: false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := disconnectSessionID(header, &login_proto.LoginNodeDisconnectRequest{SessionId: tc.body})
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
