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

type accountTradableQuotaFake struct {
	calls            int
	playerID         uint64
	desiredCharTotal uint32
	found            bool
	confirmed        uint32
	used             uint32
	err              error
}

func (f *accountTradableQuotaFake) SyncAccountStage1Tradable(_ context.Context, playerID uint64, desiredCharTotal uint32) (bool, uint32, uint32, error) {
	f.calls++
	f.playerID = playerID
	f.desiredCharTotal = desiredCharTotal
	return f.found, f.confirmed, f.used, f.err
}

func signSyncAccountStage1Tradable(t *testing.T, req *dspb.SyncAccountStage1TradableRequest, caller string, stamp uint64) {
	t.Helper()
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"SyncAccountStage1Tradable", req, caller, directoryTestSecret, stamp); err != nil {
		t.Fatalf("sign SyncAccountStage1Tradable: %v", err)
	}
}

func TestSyncAccountStage1TradableAuthValidationAndDelegation(t *testing.T) {
	for _, missing := range []string{"store", "auth"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			sc := &svc.ServiceContext{AccountTradableQuota: &accountTradableQuotaFake{}, RoleAdmissionAuth: roleAdmissionVerifier(t)}
			if missing == "store" {
				sc.AccountTradableQuota = nil
			} else {
				sc.RoleAdmissionAuth = nil
			}
			s := NewDataServiceServer(sc)
			req := &dspb.SyncAccountStage1TradableRequest{PlayerId: 1001, DesiredCharTotal: 5}
			signSyncAccountStage1Tradable(t, req, "scene", uint64(time.Now().UnixMilli()))
			_, err := s.SyncAccountStage1Tradable(context.Background(), req)
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("expected Unavailable, got %v", err)
			}
		})
	}

	for _, mode := range []string{"unsigned", "login", "body-changed"} {
		t.Run("auth-"+mode, func(t *testing.T) {
			fake := &accountTradableQuotaFake{}
			s := NewDataServiceServer(&svc.ServiceContext{AccountTradableQuota: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
			req := &dspb.SyncAccountStage1TradableRequest{PlayerId: 1001, DesiredCharTotal: 5}
			caller := "scene"
			if mode == "login" {
				caller = "login"
			}
			if mode != "unsigned" {
				signSyncAccountStage1Tradable(t, req, caller, uint64(time.Now().UnixMilli()))
			}
			if mode == "body-changed" {
				req.DesiredCharTotal = 6
			}
			_, err := s.SyncAccountStage1Tradable(context.Background(), req)
			if status.Code(err) != codes.PermissionDenied || fake.calls != 0 {
				t.Fatalf("expected PermissionDenied with 0 calls, got err=%v calls=%d", err, fake.calls)
			}
		})
	}

	t.Run("invalid-argument", func(t *testing.T) {
		fake := &accountTradableQuotaFake{}
		s := NewDataServiceServer(&svc.ServiceContext{AccountTradableQuota: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		for _, req := range []*dspb.SyncAccountStage1TradableRequest{
			{PlayerId: 0, DesiredCharTotal: 1},
			{PlayerId: 1001, DesiredCharTotal: 61},
		} {
			signSyncAccountStage1Tradable(t, req, "scene", uint64(time.Now().UnixMilli()))
			_, err := s.SyncAccountStage1Tradable(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument || fake.calls != 0 {
				t.Fatalf("expected InvalidArgument with 0 calls, got err=%v calls=%d", err, fake.calls)
			}
		}
	})

	t.Run("ok-and-error-propagation", func(t *testing.T) {
		fake := &accountTradableQuotaFake{found: true, confirmed: 4, used: 60}
		s := NewDataServiceServer(&svc.ServiceContext{AccountTradableQuota: fake, RoleAdmissionAuth: roleAdmissionVerifier(t)})
		req := &dspb.SyncAccountStage1TradableRequest{PlayerId: 2002, DesiredCharTotal: 10}
		signSyncAccountStage1Tradable(t, req, "scene", uint64(time.Now().UnixMilli()))
		resp, err := s.SyncAccountStage1Tradable(context.Background(), req)
		if err != nil || !resp.GetFound() || resp.GetConfirmedCharTotal() != 4 || resp.GetAccountUsedTotal() != 60 ||
			fake.playerID != 2002 || fake.desiredCharTotal != 10 {
			t.Fatalf("unexpected success outcome: resp=%v err=%v fake=%+v", resp, err, fake)
		}

		fake.err = errors.New("db down")
		req2 := &dspb.SyncAccountStage1TradableRequest{PlayerId: 2002, DesiredCharTotal: 11}
		signSyncAccountStage1Tradable(t, req2, "scene", uint64(time.Now().UnixMilli()))
		_, err = s.SyncAccountStage1Tradable(context.Background(), req2)
		if status.Code(err) == codes.OK {
			t.Fatalf("expected error propagation, got nil")
		}
	})
}
