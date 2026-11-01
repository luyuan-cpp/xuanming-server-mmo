package accountdirectory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	base "proto/common/base"
	dspb "proto/data_service"
)

type initialCommitRPC struct {
	dspb.DataServiceClient
	create func(context.Context, *dspb.CreateAccountRoleRequest) (*dspb.AccountDirectoryResponse, error)
}

func (f initialCommitRPC) CreateAccountRole(ctx context.Context, in *dspb.CreateAccountRoleRequest, _ ...grpc.CallOption) (*dspb.AccountDirectoryResponse, error) {
	return f.create(ctx, in)
}

// Aborted 是「提交点之前就失败了」,不能和结局未知混:login 收到它释放名字,收到 Unavailable 不释放。
func TestCreateClientKeepsNotCommittedApartFromUnknown(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 9101, ZoneId: 1}
	for _, tc := range []struct {
		name      string
		code      codes.Code
		want      error
		notWanted []error
	}{
		{name: "确定没提交", code: codes.Aborted, want: ErrNotCommitted, notWanted: []error{ErrConflict, ErrUnavailable}},
		{name: "结局未知", code: codes.Unavailable, want: ErrUnavailable, notWanted: []error{ErrNotCommitted}},
		{name: "超时也是结局未知", code: codes.DeadlineExceeded, want: ErrUnavailable, notWanted: []error{ErrNotCommitted}},
		{name: "冲突", code: codes.FailedPrecondition, want: ErrConflict, notWanted: []error{ErrNotCommitted, ErrUnavailable}},
		{name: "满位", code: codes.ResourceExhausted, want: ErrFull, notWanted: []error{ErrNotCommitted, ErrUnavailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(initialCommitRPC{create: func(context.Context, *dspb.CreateAccountRoleRequest) (*dspb.AccountDirectoryResponse, error) {
				return nil, status.Error(tc.code, "拒绝")
			}}, strings.Repeat("c", 32))
			_, err := client.CreateWithInitialCommit(context.Background(), "owner", role)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, notWanted := range tc.notWanted {
				if errors.Is(err, notWanted) {
					t.Fatalf("err = %v must not be %v", err, notWanted)
				}
			}
		})
	}
}

type replayRPC struct {
	dspb.DataServiceClient
	err error
}

func (f replayRPC) BeginAccountRoleTrade(context.Context, *dspb.BeginAccountRoleTradeRequest, ...grpc.CallOption) (*dspb.AccountRoleTradeReceipt, error) {
	return nil, f.err
}
func (f replayRPC) ConfirmAccountRoleTradeFreeze(context.Context, *dspb.ConfirmAccountRoleTradeFreezeRequest, ...grpc.CallOption) (*dspb.AccountRoleTradeReceipt, error) {
	return nil, f.err
}
func (f replayRPC) CancelAccountRoleTrade(context.Context, *dspb.CancelAccountRoleTradeRequest, ...grpc.CallOption) (*dspb.AccountRoleTradeReceipt, error) {
	return nil, f.err
}
func (f replayRPC) TransferAccountRole(context.Context, *dspb.TransferAccountRoleRequest, ...grpc.CallOption) (*dspb.AccountRoleTransferReceipt, error) {
	return nil, f.err
}
func (f replayRPC) RemoveAccountRoles(context.Context, *dspb.RemoveAccountRolesRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return nil, f.err
}

// 规格 §3.6:只有建角的 Aborted 是「确定没提交」。过户、删角的 Aborted 是「结局已知,同参数重放」,这次调用可能已经提交过东西,不能凭它释放或退款。
// 错误链上有 ErrUnavailable:只认识后者的调用方照旧重试。
func TestReplayCallsNeverReadAbortedAsNotCommitted(t *testing.T) {
	secret := strings.Repeat("c", 32)
	rpc := replayRPC{err: status.Error(codes.Aborted, "unfinished")}
	trade, login := NewTradeClient(rpc, secret), NewClient(rpc, secret)
	ctx := context.Background()
	calls := map[string]func() error{
		"Begin": func() error {
			_, err := trade.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 1})
			return err
		},
		"Confirm": func() error {
			_, err := trade.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 1})
			return err
		},
		"Cancel": func() error {
			_, err := trade.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 1})
			return err
		},
		"Transfer": func() error {
			_, err := trade.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: 1})
			return err
		},
		"Remove": func() error {
			return login.Remove(ctx, "owner", []*dspb.AccountRoleVersion{{PlayerId: 1, OwnerRevision: 1}})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, ErrRetryLater) || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrRetryLater", err)
			}
			if errors.Is(err, ErrNotCommitted) {
				t.Fatalf("err = %v must not be ErrNotCommitted", err)
			}
		})
	}
}

type roleSlotRPC struct {
	dspb.DataServiceClient
	out *dspb.CheckAccountRoleSlotResponse
	err error
}

func (f roleSlotRPC) CheckAccountRoleSlot(context.Context, *dspb.CheckAccountRoleSlotRequest, ...grpc.CallOption) (*dspb.CheckAccountRoleSlotResponse, error) {
	return f.out, f.err
}

// 读不到不能当成有名额。
func TestRoleSlotFullReadFailureIsNotAnAnswer(t *testing.T) {
	secret := strings.Repeat("c", 32)
	for _, tc := range []struct {
		name string
		rpc  roleSlotRPC
		full bool
		want error
	}{
		{name: "有名额", rpc: roleSlotRPC{out: &dspb.CheckAccountRoleSlotResponse{}}},
		{name: "满了", rpc: roleSlotRPC{out: &dspb.CheckAccountRoleSlotResponse{Full: true}}, full: true},
		{name: "读不到", rpc: roleSlotRPC{out: &dspb.CheckAccountRoleSlotResponse{Retry: true}}, want: ErrRetryLater},
		{name: "调用失败", rpc: roleSlotRPC{err: status.Error(codes.Unavailable, "down")}, want: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full, err := NewTradeClient(tc.rpc, secret).RoleSlotFull(context.Background(), "buyer")
			if full != tc.full || !errors.Is(err, tc.want) {
				t.Fatalf("full=%t err=%v", full, err)
			}
		})
	}
}

func TestCreateClientValidatesInitialCommitReceipt(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 9101, ZoneId: 1}
	for _, kind := range []string{"committed", "unknown-commit"} {
		t.Run(kind, func(t *testing.T) {
			client := NewClient(initialCommitRPC{create: func(ctx context.Context, in *dspb.CreateAccountRoleRequest) (*dspb.AccountDirectoryResponse, error) {
				if in.Account != "owner" || !proto.Equal(in.Role, role) {
					t.Fatal("RPC 丢失原建角身份")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("RPC 无原有界截止时间")
				}
				if kind == "unknown-commit" {
					return nil, status.Error(codes.Unavailable, "提交应答未知")
				}
				return &dspb.AccountDirectoryResponse{Players: &base.AccountSimplePlayerList{Players: []*base.AccountSimplePlayer{proto.Clone(role).(*base.AccountSimplePlayer)}}}, nil
			}}, strings.Repeat("c", 32))
			out, err := client.CreateWithInitialCommit(context.Background(), "owner", role)
			if kind == "committed" {
				if err != nil || out == nil {
					t.Fatalf("有效回执被拒: %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) || out != nil {
				t.Fatalf("未知回执被放行: %v/%v", out, err)
			}
		})
	}
}
