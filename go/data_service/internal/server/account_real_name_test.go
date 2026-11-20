package server

import (
	"context"
	"testing"
	"time"

	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const realNameTestPI = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"

type fakeRealNames struct {
	sets    int
	lookups int
}

func (f *fakeRealNames) SetRealName(_ context.Context, _ string, pi string) (string, error) {
	f.sets++
	return pi, nil
}

func (f *fakeRealNames) RealNameOfPlayer(context.Context, uint64) (string, bool, error) {
	f.lookups++
	return realNameTestPI, true, nil
}

func realNameServer(t *testing.T, names *fakeRealNames) *DataServiceServer {
	t.Helper()
	payment, err := serviceauth.New(map[string]string{accountdirectory.PaymentCaller: directoryTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	return NewDataServiceServer(&svc.ServiceContext{
		AccountDirectory: &serverDirectoryFake{}, AccountDirectoryAuth: directoryVerifier(t),
		AccountRealNames: names, PaymentDirectoryAuth: payment,
	})
}

// 写实名只认 login 的证明;payment、trade 的证明都不行。
func TestSetAccountRealNameRequiresLoginProof(t *testing.T) {
	for _, caller := range []string{"", "trade", accountdirectory.PaymentCaller} {
		names := &fakeRealNames{}
		in := &dspb.SetAccountRealNameRequest{Account: "acc", RealNamePi: realNameTestPI}
		if caller != "" {
			signDirectoryRequest(t, "SetAccountRealName", in, caller, uint64(time.Now().UnixMilli()))
		}
		_, err := realNameServer(t, names).SetAccountRealName(context.Background(), in)
		if status.Code(err) != codes.PermissionDenied || names.sets != 0 {
			t.Fatalf("caller %q: err=%v sets=%d; want PermissionDenied without a write", caller, err, names.sets)
		}
	}
	names := &fakeRealNames{}
	in := &dspb.SetAccountRealNameRequest{Account: "acc", RealNamePi: realNameTestPI}
	signDirectoryRequest(t, "SetAccountRealName", in, "login", uint64(time.Now().UnixMilli()))
	out, err := realNameServer(t, names).SetAccountRealName(context.Background(), in)
	if err != nil || out.GetRealNamePi() != realNameTestPI || names.sets != 1 {
		t.Fatalf("login proof: out=%v err=%v sets=%d", out, err, names.sets)
	}
}

// 按玩家查实名只认 payment 的证明;login 的证明不能拿来查。
func TestGetAccountRealNameRequiresPaymentProof(t *testing.T) {
	for _, caller := range []string{"", "login"} {
		names := &fakeRealNames{}
		in := &dspb.GetAccountRealNameRequest{PlayerId: 42}
		if caller != "" {
			signDirectoryRequest(t, "GetAccountRealName", in, caller, uint64(time.Now().UnixMilli()))
		}
		_, err := realNameServer(t, names).GetAccountRealName(context.Background(), in)
		if status.Code(err) != codes.PermissionDenied || names.lookups != 0 {
			t.Fatalf("caller %q: err=%v lookups=%d; want PermissionDenied without a read", caller, err, names.lookups)
		}
	}
	names := &fakeRealNames{}
	in := &dspb.GetAccountRealNameRequest{PlayerId: 42}
	signDirectoryRequest(t, "GetAccountRealName", in, accountdirectory.PaymentCaller, uint64(time.Now().UnixMilli()))
	out, err := realNameServer(t, names).GetAccountRealName(context.Background(), in)
	if err != nil || !out.GetFound() || out.GetRealNamePi() != realNameTestPI {
		t.Fatalf("payment proof: out=%v err=%v", out, err)
	}
}
