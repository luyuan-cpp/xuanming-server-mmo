package sessionmanager

import (
	"context"
	"errors"
	"testing"

	commonbase "proto/common/base"
	plpb "proto/player_locator"

	"google.golang.org/grpc"
)

// fakePlayerLocatorClient is the smallest implementation of
// plpb.PlayerLocatorClient that lets us steer SetDisconnecting outcomes
// in tests without standing up a real gRPC server.
//
// It records the last call so each test can assert on the request
// payload (player_id, session_id, lease_ttl) and choose what to return.
// All other methods are deliberately stubbed with panic — we only
// exercise SetDisconnecting here, and a panic surfaces a future test
// that strays from the contract under test.
type fakePlayerLocatorClient struct {
	lastSetDisconnecting *plpb.SetDisconnectingRequest
	setDisconnectErr     error
	setDisconnectCalls   int
	getSessionResponse   *plpb.GetSessionResponse
	getSessionErr        error
	getSessionCalls      int
	lastMarkOffline      *plpb.MarkOfflineRequest
	markOfflineErr       error
	markOfflineCalls     int
}

func (f *fakePlayerLocatorClient) SetLocation(context.Context, *plpb.PlayerLocation, ...grpc.CallOption) (*commonbase.Empty, error) {
	panic("SetLocation not used in this test")
}
func (f *fakePlayerLocatorClient) GetLocation(context.Context, *plpb.PlayerId, ...grpc.CallOption) (*plpb.PlayerLocation, error) {
	panic("GetLocation not used in this test")
}
func (f *fakePlayerLocatorClient) MarkOffline(_ context.Context, in *plpb.MarkOfflineRequest, _ ...grpc.CallOption) (*commonbase.Empty, error) {
	f.markOfflineCalls++
	f.lastMarkOffline = in
	return &commonbase.Empty{}, f.markOfflineErr
}
func (f *fakePlayerLocatorClient) SetSession(context.Context, *plpb.SetSessionRequest, ...grpc.CallOption) (*commonbase.Empty, error) {
	panic("SetSession not used in this test")
}
func (f *fakePlayerLocatorClient) GetSession(context.Context, *plpb.GetSessionRequest, ...grpc.CallOption) (*plpb.GetSessionResponse, error) {
	f.getSessionCalls++
	if f.getSessionResponse == nil {
		return &plpb.GetSessionResponse{}, f.getSessionErr
	}
	return f.getSessionResponse, f.getSessionErr
}
func (f *fakePlayerLocatorClient) SetDisconnecting(_ context.Context, in *plpb.SetDisconnectingRequest, _ ...grpc.CallOption) (*commonbase.Empty, error) {
	f.setDisconnectCalls++
	f.lastSetDisconnecting = in
	return &commonbase.Empty{}, f.setDisconnectErr
}
func (f *fakePlayerLocatorClient) Reconnect(context.Context, *plpb.ReconnectRequest, ...grpc.CallOption) (*plpb.ReconnectResponse, error) {
	panic("Reconnect not used in this test")
}

// TestSetSessionDisconnecting_PassesPlayerIdAndSessionId is the regression
// guard for the cpp-gate fix in this commit series: gate's
// HandleConnectionDisconnection now copies SessionInfo.playerId into
// SessionDetails, so login's markPlayerSessionDisconnecting() actually
// reaches this RPC (instead of early-returning on playerID == 0). If
// anyone later changes the wire shape so player_id stops travelling,
// this test will fail and the deprecation/lease leak resurfaces.
//
// We pin the lease at 30s because the doc and the disconnect-cleanup
// timing both depend on that exact value (see
// docs/design/player_login_flow.md §2 Disconnect: "30s reconnect window").
func TestSetSessionDisconnecting_PassesPlayerIdAndSessionId(t *testing.T) {
	mock := &fakePlayerLocatorClient{}
	if err := SetSessionDisconnecting(context.Background(), mock, 12345, 678, "gate-I2", 9); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if mock.setDisconnectCalls != 1 {
		t.Fatalf("expected exactly one SetDisconnecting call, got %d", mock.setDisconnectCalls)
	}
	got := mock.lastSetDisconnecting
	if got.PlayerId != 12345 {
		t.Errorf("PlayerId mismatch: got %d, want 12345", got.PlayerId)
	}
	if got.SessionId != 678 {
		t.Errorf("SessionId mismatch: got %d, want 678", got.SessionId)
	}
	// 实例号必须随请求到 player_locator,它据此忽略旧 gate 实例迟到的同号断线(scene-enter-chain-residuals.md §6)。
	if got.GateInstanceId != "gate-I2" {
		t.Errorf("GateInstanceId mismatch: got %q, want gate-I2", got.GateInstanceId)
	}
	// 会话代次同理:player_locator 据此忽略同一 gate 进程里同号旧连接迟到的断线(gate-online-identity.md §10.4)。
	if got.SessionIncarnation != 9 {
		t.Errorf("SessionIncarnation mismatch: got %d, want 9", got.SessionIncarnation)
	}
	if got.LeaseTtlSeconds != 30 {
		t.Errorf("LeaseTtlSeconds: got %d, want 30 (matches disconnect-window doc)", got.LeaseTtlSeconds)
	}
}

// TestSetSessionDisconnecting_WrapsRpcError ensures upstream gRPC errors
// surface to the caller — login's markPlayerSessionDisconnecting() logs
// these but does not panic; the helper at this layer must propagate
// faithfully so the upper layer can log AND continue cleanup of other
// state (loginsession.Cleanup happens before this call, by design).
func TestSetSessionDisconnecting_WrapsRpcError(t *testing.T) {
	mock := &fakePlayerLocatorClient{
		setDisconnectErr: errors.New("locator unavailable"),
	}
	err := SetSessionDisconnecting(context.Background(), mock, 1, 2, "", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Wrap message comes from session_manager.go — check the prefix so
	// future formatting tweaks don't break the test brittlely.
	if got := err.Error(); got == "" || !contains(got, "player_locator SetDisconnecting") {
		t.Errorf("error message did not include wrap prefix; got %q", got)
	}
}

func TestDeleteSession_DelayedOldLeaveGameDoesNotDeleteReplacement(t *testing.T) {
	mock := &fakePlayerLocatorClient{
		getSessionResponse: &plpb.GetSessionResponse{
			Found: true,
			Session: &plpb.PlayerSession{
				PlayerId:       12345,
				SessionId:      200,
				SessionVersion: 7,
			},
		},
	}

	// session=100 的 LeaveGame 到达时，权威会话已经被 session=200 替换。
	if err := DeleteSession(context.Background(), mock, 12345, 100, ""); err != nil {
		t.Fatalf("expected stale LeaveGame to be a successful no-op, got %v", err)
	}
	if mock.getSessionCalls != 1 {
		t.Fatalf("expected one GetSession call, got %d", mock.getSessionCalls)
	}
	if mock.markOfflineCalls != 0 {
		t.Fatalf("stale LeaveGame must not call MarkOffline, got %d calls", mock.markOfflineCalls)
	}
}

func TestDeleteSession_PassesObservedSessionVersion(t *testing.T) {
	mock := &fakePlayerLocatorClient{
		getSessionResponse: &plpb.GetSessionResponse{
			Found: true,
			Session: &plpb.PlayerSession{
				PlayerId:       12345,
				SessionId:      200,
				SessionVersion: 7,
			},
		},
	}

	if err := DeleteSession(context.Background(), mock, 12345, 200, "gate-I2"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if mock.markOfflineCalls != 1 {
		t.Fatalf("expected one MarkOffline call, got %d", mock.markOfflineCalls)
	}
	got := mock.lastMarkOffline
	if got.PlayerId != 12345 || got.ExpectedSessionId != 200 || got.ExpectedSessionVersion != 7 {
		t.Fatalf("unexpected MarkOffline request: %+v", got)
	}
	// 实例号原样随请求到 player_locator,由它判定(login 本地不比实例号)。
	if got.ExpectedGateInstanceId != "gate-I2" {
		t.Fatalf("ExpectedGateInstanceId = %q, want gate-I2", got.ExpectedGateInstanceId)
	}
}

// TestDeleteSession_LeavesGateInstanceCheckToLocator:会话号相同时,无论实例号与记录相同、不同(gate 以同一
// node_id 重启,旧实例 I1 上的同号连接迟到的 LeaveGame)还是为空,login 都发 MarkOffline,并原样带上发起登出的
// 实例号(不是记录里的)。同号跨实例由 player_locator 在同一份字节上判定、打 Info 并计
// stale_gate_instance_total{op="mark_offline"};login 本地若先短路,这一面的观测就丢了
// (docs/design/scene-enter-chain-residuals.md §6.2)。
func TestDeleteSession_LeavesGateInstanceCheckToLocator(t *testing.T) {
	record := &plpb.GetSessionResponse{
		Found: true,
		Session: &plpb.PlayerSession{
			PlayerId:       12345,
			SessionId:      200,
			SessionVersion: 7,
			GateInstanceId: "gate-I2",
		},
	}

	cases := map[string]string{
		"cross instance (stale I1)":  "gate-I1",
		"legacy request without one": "",
		"same instance":              "gate-I2",
	}
	for name, requestInstance := range cases {
		t.Run(name, func(t *testing.T) {
			mock := &fakePlayerLocatorClient{getSessionResponse: record}
			if err := DeleteSession(context.Background(), mock, 12345, 200, requestInstance); err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			if mock.markOfflineCalls != 1 {
				t.Fatalf("expected exactly one MarkOffline call (player_locator decides), got %d", mock.markOfflineCalls)
			}
			got := mock.lastMarkOffline
			if got.ExpectedSessionId != 200 || got.ExpectedSessionVersion != 7 {
				t.Fatalf("unexpected MarkOffline request: %+v", got)
			}
			if got.ExpectedGateInstanceId != requestInstance {
				t.Fatalf("ExpectedGateInstanceId = %q, want the reporting instance %q (not the record's)", got.ExpectedGateInstanceId, requestInstance)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
