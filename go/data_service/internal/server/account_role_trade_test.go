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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type roleTradesFake struct {
	in       proto.Message
	ctx      context.Context
	calls    int
	err      error
	trade    *dspb.AccountRoleTradeReceipt
	transfer *dspb.AccountRoleTransferReceipt
	slotFull bool
	slotErr  error
}

func (f *roleTradesFake) record(ctx context.Context, in proto.Message) {
	f.ctx = ctx
	f.in = in
	f.calls++
}
func (f *roleTradesFake) Begin(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	f.record(ctx, in)
	return f.trade, f.err
}
func (f *roleTradesFake) Confirm(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	f.record(ctx, in)
	return f.trade, f.err
}
func (f *roleTradesFake) Cancel(ctx context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	f.record(ctx, in)
	return f.trade, f.err
}
func (f *roleTradesFake) Transfer(ctx context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	f.record(ctx, in)
	return f.transfer, f.err
}
func (f *roleTradesFake) RoleSlotFull(context.Context, string) (bool, error) {
	return f.slotFull, f.slotErr
}

type roleTradeRPC struct {
	method  string
	request func() proto.Message
	call    func(context.Context, *DataServiceServer, proto.Message) (proto.Message, error)
}

func roleTradeRPCs() []roleTradeRPC {
	return []roleTradeRPC{
		{"BeginAccountRoleTrade", func() proto.Message {
			return &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: ^uint64(0), SellerAccount: " seller ", ExpectedOwnerRevision: 17}
		}, func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
			return s.BeginAccountRoleTrade(ctx, in.(*dspb.BeginAccountRoleTradeRequest))
		}},
		{"ConfirmAccountRoleTradeFreeze", func() proto.Message {
			return &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: ^uint64(0), SellerAccount: " seller ", ExpectedOwnerRevision: 17}
		}, func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
			return s.ConfirmAccountRoleTradeFreeze(ctx, in.(*dspb.ConfirmAccountRoleTradeFreezeRequest))
		}},
		{"CancelAccountRoleTrade", func() proto.Message {
			return &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: ^uint64(0), SellerAccount: " seller ", ExpectedOwnerRevision: 17}
		}, func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
			return s.CancelAccountRoleTrade(ctx, in.(*dspb.CancelAccountRoleTradeRequest))
		}},
		{"TransferAccountRole", func() proto.Message {
			return &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: ^uint64(0), SellerAccount: " seller ", BuyerAccount: "Buyer", ExpectedOwnerRevision: 17}
		}, func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
			return s.TransferAccountRole(ctx, in.(*dspb.TransferAccountRoleRequest))
		}},
	}
}
func roleTradeVerifier(t *testing.T) *serviceauth.Verifier {
	t.Helper()
	v, err := serviceauth.New(map[string]string{"trade": directoryTestSecret, "login": directoryTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 规格 §5.4:名额检查只读,读失败带内回,不打成 Hold。没有 trade 证明的调用不读。
func TestCheckAccountRoleSlot(t *testing.T) {
	call := func(fake *roleTradesFake, account string, sign bool) (*dspb.CheckAccountRoleSlotResponse, error) {
		s := NewDataServiceServer(&svc.ServiceContext{AccountRoleTrades: fake, AccountRoleTradeAuth: roleTradeVerifier(t)})
		in := &dspb.CheckAccountRoleSlotRequest{Account: account}
		if sign {
			if err := serviceauth.Sign(accountdirectory.MethodPrefix+"CheckAccountRoleSlot", in, "trade", directoryTestSecret, uint64(time.Now().UnixMilli())); err != nil {
				t.Fatal(err)
			}
		}
		return s.CheckAccountRoleSlot(context.Background(), in)
	}
	out, err := call(&roleTradesFake{slotFull: true}, "buyer", true)
	if err != nil || !out.GetFull() || out.GetRetry() {
		t.Fatalf("满了: out=%v err=%v", out, err)
	}
	out, err = call(&roleTradesFake{}, "buyer", true)
	if err != nil || out.GetFull() || out.GetRetry() {
		t.Fatalf("有名额: out=%v err=%v", out, err)
	}
	out, err = call(&roleTradesFake{slotErr: errors.New("private SQL failure")}, "buyer", true)
	if err != nil || out.GetFull() || !out.GetRetry() {
		t.Fatalf("读失败要带内回: out=%v err=%v", out, err)
	}
	if _, err = call(&roleTradesFake{slotErr: accountdirectory.ErrInvalid}, "", true); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("调用方缺陷: %v", err)
	}
	if _, err = call(&roleTradesFake{slotFull: true}, "buyer", false); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("没有证明: %v", err)
	}
}

func TestRoleTradeRPCRequiresIndependentTradeBodyProof(t *testing.T) {
	for _, rpc := range roleTradeRPCs() {
		for _, failure := range []string{"unsigned", "login", "other-method", "changed-body", "expired"} {
			t.Run(rpc.method+"/"+failure, func(t *testing.T) {
				fake := &roleTradesFake{}
				s := NewDataServiceServer(&svc.ServiceContext{AccountRoleTrades: fake, AccountRoleTradeAuth: roleTradeVerifier(t)})
				in := rpc.request()
				if failure != "unsigned" {
					caller, method, stamp := "trade", rpc.method, uint64(time.Now().UnixMilli())
					if failure == "login" {
						caller = "login"
					}
					if failure == "other-method" {
						method += "Other"
					}
					if failure == "expired" {
						stamp = 1
					}
					if err := serviceauth.Sign(accountdirectory.MethodPrefix+method, in, caller, directoryTestSecret, stamp); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "changed-body" {
					m := in.ProtoReflect()
					f := m.Descriptor().Fields().ByName("seller_account")
					m.Set(f, protoreflect.ValueOfString("other-seller"))
				}
				_, err := rpc.call(context.Background(), s, in)
				if status.Code(err) != codes.PermissionDenied || fake.calls != 0 {
					t.Fatalf("无效证明触发目录写: err=%v calls=%d", err, fake.calls)
				}
			})
		}
	}
}
func TestRoleTradeRPCPreservesUnreadyErrorsAndOriginalFields(t *testing.T) {
	for _, rpc := range roleTradeRPCs() {
		for _, missing := range []string{"auth", "store"} {
			t.Run(rpc.method+"/missing-"+missing, func(t *testing.T) {
				sc := &svc.ServiceContext{AccountRoleTrades: &roleTradesFake{}, AccountRoleTradeAuth: roleTradeVerifier(t)}
				if missing == "auth" {
					sc.AccountRoleTradeAuth = nil
				} else {
					sc.AccountRoleTrades = nil
				}
				_, err := rpc.call(context.Background(), NewDataServiceServer(sc), rpc.request())
				if status.Code(err) != codes.Unavailable {
					t.Fatalf("未就绪必须拒绝: %v", err)
				}
			})
		}
		for _, tc := range []struct {
			name string
			err  error
			code codes.Code
		}{{"success", nil, codes.OK}, {"full", accountdirectory.ErrFull, codes.ResourceExhausted}, {"conflict", accountdirectory.ErrConflict, codes.FailedPrecondition}, {"invalid", accountdirectory.ErrInvalid, codes.InvalidArgument}, {"missing", accountdirectory.ErrMissing, codes.NotFound}, {"retry-later", accountdirectory.ErrRetryLater, codes.Aborted}, {"proof-unavailable", accountdirectory.ErrUnavailable, codes.Unavailable}, {"sql-failure", errors.New("private SQL failure"), codes.Unavailable}} {
			t.Run(rpc.method+"/"+tc.name, func(t *testing.T) {
				fake := &roleTradesFake{err: tc.err, trade: &dspb.AccountRoleTradeReceipt{ListingId: 101}, transfer: &dspb.AccountRoleTransferReceipt{TxId: 1001}}
				s := NewDataServiceServer(&svc.ServiceContext{AccountRoleTrades: fake, AccountRoleTradeAuth: roleTradeVerifier(t)})
				in := rpc.request()
				if err := serviceauth.Sign(accountdirectory.MethodPrefix+rpc.method, in, "trade", directoryTestSecret, uint64(time.Now().UnixMilli())); err != nil {
					t.Fatal(err)
				}
				ctx := context.WithValue(context.Background(), struct{}{}, "role-trade")
				out, err := rpc.call(ctx, s, in)
				if status.Code(err) != tc.code || fake.calls != 1 || fake.in != in || fake.ctx != ctx {
					t.Fatalf("RPC字段/上下文/分类改变: out=%v err=%v calls=%d", out, err, fake.calls)
				}
				if err == nil {
					var expected proto.Message = fake.trade
					if rpc.method == "TransferAccountRole" {
						expected = fake.transfer
					}
					if !proto.Equal(out, expected) {
						t.Fatalf("应答改变: %v", out)
					}
				}
			})
		}
	}
}
