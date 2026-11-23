package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type roleAppearanceUpdaterFake struct {
	calls        int
	playerID     uint64
	appearanceID string
	updated      bool
	err          error
}

func (f *roleAppearanceUpdaterFake) UpdateRoleAppearance(_ context.Context, playerID uint64, appearanceID string) (bool, error) {
	f.calls++
	f.playerID = playerID
	f.appearanceID = appearanceID
	return f.updated, f.err
}

func signUpdateRoleAppearance(t *testing.T, req *dspb.UpdateRoleAppearanceRequest, caller string, stamp uint64) {
	t.Helper()
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"UpdateRoleAppearance", req, caller, directoryTestSecret, stamp); err != nil {
		t.Fatalf("sign UpdateRoleAppearance: %v", err)
	}
}

func TestUpdateRoleAppearanceAuthValidationAndDelegation(t *testing.T) {
	for _, missing := range []string{"store", "auth"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			sc := &svc.ServiceContext{RoleAppearances: &roleAppearanceUpdaterFake{}, RoleAdmissionAuth: roleAdmissionVerifier(t)}
			if missing == "store" {
				sc.RoleAppearances = nil
			} else {
				sc.RoleAdmissionAuth = nil
			}
			s := NewDataServiceServer(sc)
			req := &dspb.UpdateRoleAppearanceRequest{PlayerId: 1001, AppearanceId: "01_ice_sword_girl"}
			signUpdateRoleAppearance(t, req, "scene", uint64(time.Now().UnixMilli()))
			_, err := s.UpdateRoleAppearance(context.Background(), req)
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("expected Unavailable, got %v", err)
			}
		})
	}

	for _, mode := range []string{"unsigned", "login", "body-changed"} {
		t.Run("auth-"+mode, func(t *testing.T) {
			fake := &roleAppearanceUpdaterFake{}
			s := NewDataServiceServer(&svc.ServiceContext{RoleAppearances: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
			req := &dspb.UpdateRoleAppearanceRequest{PlayerId: 1001, AppearanceId: "01_ice_sword_girl"}
			caller := "scene"
			if mode == "login" {
				caller = "login"
			}
			if mode != "unsigned" {
				signUpdateRoleAppearance(t, req, caller, uint64(time.Now().UnixMilli()))
			}
			if mode == "body-changed" {
				req.AppearanceId = "00_reference_topright_boy"
			}
			_, err := s.UpdateRoleAppearance(context.Background(), req)
			if status.Code(err) != codes.PermissionDenied || fake.calls != 0 {
				t.Fatalf("expected PermissionDenied with 0 calls, got err=%v calls=%d", err, fake.calls)
			}
		})
	}

	t.Run("invalid-argument", func(t *testing.T) {
		fake := &roleAppearanceUpdaterFake{}
		s := NewDataServiceServer(&svc.ServiceContext{RoleAppearances: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.UpdateRoleAppearanceRequest{PlayerId: 1001, AppearanceId: "   "}
		signUpdateRoleAppearance(t, req, "scene", uint64(time.Now().UnixMilli()))
		_, err := s.UpdateRoleAppearance(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument || fake.calls != 0 {
			t.Fatalf("expected InvalidArgument, got err=%v calls=%d", err, fake.calls)
		}
	})

	t.Run("ok-and-error-propagation", func(t *testing.T) {
		fake := &roleAppearanceUpdaterFake{updated: true}
		s := NewDataServiceServer(&svc.ServiceContext{RoleAppearances: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.UpdateRoleAppearanceRequest{PlayerId: 2002, AppearanceId: "00_reference_topright_boy"}
		signUpdateRoleAppearance(t, req, "scene", uint64(time.Now().UnixMilli()))
		resp, err := s.UpdateRoleAppearance(context.Background(), req)
		if err != nil || !resp.GetUpdated() || fake.playerID != 2002 || fake.appearanceID != "00_reference_topright_boy" {
			t.Fatalf("unexpected success outcome: resp=%v err=%v fake=%+v", resp, err, fake)
		}

		fake.err = errors.New("db down")
		req2 := &dspb.UpdateRoleAppearanceRequest{PlayerId: 2002, AppearanceId: "09_bamboo_archer_girl"}
		signUpdateRoleAppearance(t, req2, "scene", uint64(time.Now().UnixMilli()))
		_, err = s.UpdateRoleAppearance(context.Background(), req2)
		if status.Code(err) == codes.OK {
			t.Fatalf("expected error propagation, got nil")
		}
	})
}
