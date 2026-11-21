package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type roleRenamerFake struct {
	calls int
	req   store.RenameRequest
	out   store.RenameOutcome
	err   error
}

func (f *roleRenamerFake) RenameRole(_ context.Context, req store.RenameRequest) (store.RenameOutcome, error) {
	f.calls++
	f.req = req
	return f.out, f.err
}

func (f *roleRenamerFake) ResumeRenames(context.Context, time.Duration, time.Duration, int) (store.RenameResumeResult, error) {
	return store.RenameResumeResult{}, nil
}

func signRenameRole(t *testing.T, req *dspb.RenameRoleRequest, caller string, stamp uint64) {
	t.Helper()
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"RenameRole", req, caller, directoryTestSecret, stamp); err != nil {
		t.Fatalf("sign RenameRole: %v", err)
	}
}

func TestRenameRoleRejectsBadProofAndUnready(t *testing.T) {
	for _, missing := range []string{"store", "auth"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			sc := &svc.ServiceContext{RoleRenames: &roleRenamerFake{}, RoleAdmissionAuth: roleAdmissionVerifier(t)}
			if missing == "store" {
				sc.RoleRenames = nil
			} else {
				sc.RoleAdmissionAuth = nil
			}
			s := NewDataServiceServer(sc)
			req := &dspb.RenameRoleRequest{PlayerId: 1001, Seq: 1, Name: "青云剑客", MinChars: 2, MaxChars: 12}
			signRenameRole(t, req, "scene", uint64(time.Now().UnixMilli()))
			_, err := s.RenameRole(context.Background(), req)
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("expected Unavailable, got %v", err)
			}
		})
	}

	for _, mode := range []string{"unsigned", "login", "trade", "body-changed", "expired"} {
		t.Run("auth-"+mode, func(t *testing.T) {
			fake := &roleRenamerFake{}
			s := NewDataServiceServer(&svc.ServiceContext{RoleRenames: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
			req := &dspb.RenameRoleRequest{PlayerId: 1001, Seq: 1, Name: "青云剑客", MinChars: 2, MaxChars: 12}
			caller, stamp := "scene", uint64(time.Now().UnixMilli())
			if mode == "login" || mode == "trade" {
				caller = mode
			}
			if mode == "expired" {
				stamp = 1
			}
			if mode != "unsigned" {
				signRenameRole(t, req, caller, stamp)
			}
			if mode == "body-changed" {
				req.Name = "被篡改的名字"
			}
			_, err := s.RenameRole(context.Background(), req)
			if status.Code(err) != codes.PermissionDenied || fake.calls != 0 {
				t.Fatalf("expected PermissionDenied with 0 calls, got err=%v calls=%d", err, fake.calls)
			}
		})
	}
}

func TestRenameRoleNormalizesAndForwardsOutcomes(t *testing.T) {
	now := uint64(time.Now().UnixMilli())
	t.Run("valid-name", func(t *testing.T) {
		fake := &roleRenamerFake{out: store.RenameOutcome{
			Result:     dspb.RenameRoleResult_RENAME_ROLE_OK,
			CurrentSeq: 3,
			Name:       "青云剑客",
			RenamedMs:  1700000000000,
		}}
		s := NewDataServiceServer(&svc.ServiceContext{RoleRenames: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.RenameRoleRequest{
			PlayerId:        2002,
			Seq:             3,
			Name:            "  青云剑客  ",
			CooldownSeconds: 604800,
			HoldSeconds:     604800,
			MinChars:        2,
			MaxChars:        12,
		}
		signRenameRole(t, req, "scene", now)
		resp, err := s.RenameRole(context.Background(), req)
		if err != nil {
			t.Fatalf("RenameRole: %v", err)
		}
		if fake.calls != 1 || fake.req.Invalid || fake.req.Display != "青云剑客" || fake.req.Norm == "" {
			t.Fatalf("unexpected store req: calls=%d req=%+v", fake.calls, fake.req)
		}
		if fake.req.Cooldown != 604800*time.Second || fake.req.Hold != 604800*time.Second {
			t.Fatalf("unexpected durations: %+v", fake.req)
		}
		if resp.Result != dspb.RenameRoleResult_RENAME_ROLE_OK || resp.CurrentSeq != 3 || resp.Name != "青云剑客" || resp.RenamedMs != 1700000000000 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("invalid-name-still-records-seq", func(t *testing.T) {
		fake := &roleRenamerFake{out: store.RenameOutcome{
			Result:     dspb.RenameRoleResult_RENAME_ROLE_INVALID,
			CurrentSeq: 4,
		}}
		s := NewDataServiceServer(&svc.ServiceContext{RoleRenames: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.RenameRoleRequest{PlayerId: 2002, Seq: 4, Name: "a", MinChars: 2, MaxChars: 12}
		signRenameRole(t, req, "scene", now)
		resp, err := s.RenameRole(context.Background(), req)
		if err != nil {
			t.Fatalf("RenameRole invalid name: %v", err)
		}
		if fake.calls != 1 || !fake.req.Invalid || fake.req.Display != "" || fake.req.Norm != "" {
			t.Fatalf("expected Invalid=true with empty display/norm, got %+v", fake.req)
		}
		if resp.Result != dspb.RenameRoleResult_RENAME_ROLE_INVALID || resp.CurrentSeq != 4 {
			t.Fatalf("unexpected resp: %+v", resp)
		}
	})

	t.Run("bad-rules-rejected-before-store", func(t *testing.T) {
		fake := &roleRenamerFake{}
		s := NewDataServiceServer(&svc.ServiceContext{RoleRenames: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.RenameRoleRequest{PlayerId: 2002, Seq: 5, Name: "青云剑客", MinChars: 10, MaxChars: 2}
		signRenameRole(t, req, "scene", now)
		_, err := s.RenameRole(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument || fake.calls != 0 {
			t.Fatalf("expected InvalidArgument with 0 calls, got err=%v calls=%d", err, fake.calls)
		}
	})

	t.Run("store-error-maps-to-grpc-unavailable", func(t *testing.T) {
		fake := &roleRenamerFake{err: errors.New("db down")}
		s := NewDataServiceServer(&svc.ServiceContext{RoleRenames: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.RenameRoleRequest{PlayerId: 2002, Seq: 6, Name: "青云剑客", MinChars: 2, MaxChars: 12}
		signRenameRole(t, req, "scene", now)
		_, err := s.RenameRole(context.Background(), req)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("expected Unavailable, got %v", err)
		}
	})
}
