package svc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"login/internal/logic/pkg/sessionmanager"
	basepb "proto/common/base"
	plpb "proto/player_locator"
)

type fakeDeletionLocator struct {
	plpb.PlayerLocatorClient
	session     *plpb.PlayerSession
	getErr      error
	markErr     error
	markOffline []*plpb.MarkOfflineRequest
}

func (f *fakeDeletionLocator) GetSession(_ context.Context, _ *plpb.GetSessionRequest, _ ...grpc.CallOption) (*plpb.GetSessionResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.session == nil {
		return &plpb.GetSessionResponse{Found: false}, nil
	}
	return &plpb.GetSessionResponse{Found: true, Session: f.session}, nil
}

func (f *fakeDeletionLocator) MarkOffline(_ context.Context, req *plpb.MarkOfflineRequest, _ ...grpc.CallOption) (*basepb.Empty, error) {
	f.markOffline = append(f.markOffline, req)
	if f.markErr != nil {
		return nil, f.markErr
	}
	return &basepb.Empty{}, nil
}

func TestKickPlayerSession_SkipsWhenMissingOrOffline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session *plpb.PlayerSession
	}{
		{name: "no session", session: nil},
		{name: "already offline", session: &plpb.PlayerSession{
			PlayerId: 101, SessionId: 7, GateId: "1", GateInstanceId: "gate-uuid-1",
			State: plpb.PlayerSessionState(sessionmanager.StateOffline),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locator := &fakeDeletionLocator{session: tc.session}
			kicked := false
			err := kickPlayerSession(context.Background(), locator, func(context.Context, string, string, uint32, uint64, uint32) error {
				kicked = true
				return nil
			}, 101, 2041)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kicked || len(locator.markOffline) != 0 {
				t.Fatalf("expected no kick and no MarkOffline, got kicked=%v markOffline=%d", kicked, len(locator.markOffline))
			}
		})
	}
}

func TestKickPlayerSession_KicksGateBeforeMarkOffline(t *testing.T) {
	locator := &fakeDeletionLocator{
		session: &plpb.PlayerSession{
			PlayerId:       202,
			SessionId:      9,
			SessionVersion: 4,
			GateId:         "12",
			GateInstanceId: "gate-uuid-12",
			State:          plpb.PlayerSessionState(sessionmanager.StateOnline),
		},
	}
	var order []string
	var gotGateID, gotGateInst string
	var gotSessionID uint32
	var gotPlayerID uint64
	var gotReason uint32
	err := kickPlayerSession(context.Background(), locator, func(_ context.Context, gateID, gateInstanceID string, sessionID uint32, playerID uint64, reasonTip uint32) error {
		order = append(order, "kick")
		gotGateID, gotGateInst, gotSessionID, gotPlayerID, gotReason = gateID, gateInstanceID, sessionID, playerID, reasonTip
		if len(locator.markOffline) != 0 {
			t.Fatal("MarkOffline must not run before KickSessionWithReason completes")
		}
		return nil
	}, 202, 2041)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 1 || gotGateID != "12" || gotGateInst != "gate-uuid-12" || gotSessionID != 9 || gotPlayerID != 202 || gotReason != 2041 {
		t.Fatalf("unexpected kick call: order=%v gate=%s inst=%s sess=%d player=%d reason=%d",
			order, gotGateID, gotGateInst, gotSessionID, gotPlayerID, gotReason)
	}
	if len(locator.markOffline) != 1 {
		t.Fatalf("expected 1 MarkOffline call, got %d", len(locator.markOffline))
	}
	req := locator.markOffline[0]
	if req.PlayerId != 202 || req.ExpectedSessionId != 9 || req.ExpectedSessionVersion != 4 || req.ExpectedGateInstanceId != "gate-uuid-12" {
		t.Fatalf("unexpected MarkOffline request: %+v", req)
	}
}

func TestKickPlayerSession_AbortsMarkOfflineWhenGateKickFails(t *testing.T) {
	locator := &fakeDeletionLocator{
		session: &plpb.PlayerSession{
			PlayerId:       303,
			SessionId:      11,
			SessionVersion: 2,
			GateId:         "5",
			GateInstanceId: "gate-uuid-5",
			State:          plpb.PlayerSessionState(sessionmanager.StateOnline),
		},
	}
	wantErr := errors.New("kafka unavailable")
	err := kickPlayerSession(context.Background(), locator, func(context.Context, string, string, uint32, uint64, uint32) error {
		return wantErr
	}, 303, 2041)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if len(locator.markOffline) != 0 {
		t.Fatalf("MarkOffline must not run when gate kick fails, got %d calls", len(locator.markOffline))
	}
}

func TestKickPlayerSession_MarksOfflineDirectlyWhenGateIdentityEmpty(t *testing.T) {
	locator := &fakeDeletionLocator{
		session: &plpb.PlayerSession{
			PlayerId:       404,
			SessionId:      13,
			SessionVersion: 3,
			State:          plpb.PlayerSessionState(sessionmanager.StateDisconnecting),
		},
	}
	kicked := false
	err := kickPlayerSession(context.Background(), locator, func(context.Context, string, string, uint32, uint64, uint32) error {
		kicked = true
		return nil
	}, 404, 2041)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kicked {
		t.Fatal("must not call kickGate when GateId / GateInstanceId is empty")
	}
	if len(locator.markOffline) != 1 || locator.markOffline[0].ExpectedSessionId != 13 {
		t.Fatalf("expected MarkOffline for session 13, got %+v", locator.markOffline)
	}
}
