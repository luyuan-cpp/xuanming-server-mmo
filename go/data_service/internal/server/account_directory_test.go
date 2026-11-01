package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"data_service/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/drainproof"
	"shared/serverbase"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

const directoryTestSecret = "account-directory-test-only-secret-0001"

// 仅替换目录 I/O，实际 RPC 入口、请求证明和错误转换仍走生产实现。
type serverDirectoryFake struct {
	calls     []string
	ctx       context.Context
	account   string
	create    bool
	player    uint64
	role      *base.AccountSimplePlayer
	revision  uint64
	versions  []*dspb.AccountRoleVersion
	players   *base.AccountSimplePlayerList
	ownership *dspb.AccountRoleOwnership
	failure   error
}

var _ accountdirectory.Directory = (*serverDirectoryFake)(nil)

func (f *serverDirectoryFake) record(ctx context.Context, method string) {
	f.calls = append(f.calls, method)
	f.ctx = ctx
}
func (f *serverDirectoryFake) Read(ctx context.Context, account string, create bool) (*base.AccountSimplePlayerList, error) {
	f.record(ctx, "Read")
	f.account, f.create = account, create
	return f.players, f.failure
}
func (f *serverDirectoryFake) Create(ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	f.record(ctx, "Create")
	f.account, f.role = account, role
	return f.players, f.failure
}
func (f *serverDirectoryFake) Lookup(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	f.record(ctx, "Lookup")
	f.player = player
	return f.ownership, f.failure
}
func (f *serverDirectoryFake) Repair(ctx context.Context, account string, role *base.AccountSimplePlayer, revision uint64) (*base.AccountSimplePlayerList, error) {
	f.record(ctx, "Repair")
	f.account, f.role, f.revision = account, role, revision
	return f.players, f.failure
}
func (f *serverDirectoryFake) Remove(ctx context.Context, account string, versions []*dspb.AccountRoleVersion) error {
	f.record(ctx, "Remove")
	f.account, f.versions = account, versions
	return f.failure
}

type directoryRPCCase struct {
	name    string
	storeOp string
	request func() proto.Message
	mutate  func(proto.Message)
	invoke  func(context.Context, *DataServiceServer, proto.Message) (proto.Message, error)
	check   func(*testing.T, *serverDirectoryFake)
}

func directoryRPCCases() []directoryRPCCase {
	role := func() *base.AccountSimplePlayer {
		return &base.AccountSimplePlayer{PlayerId: ^uint64(0), AppearanceId: "06_thunder_caster_boy"}
	}
	return []directoryRPCCase{
		{
			name: "ReadAccountDirectory", storeOp: "Read",
			request: func() proto.Message {
				return &dspb.ReadAccountDirectoryRequest{Account: "账号甲", CreateIfMissing: true}
			},
			mutate: func(in proto.Message) { in.(*dspb.ReadAccountDirectoryRequest).Account = "账号乙" },
			invoke: func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
				return s.ReadAccountDirectory(ctx, in.(*dspb.ReadAccountDirectoryRequest))
			},
			check: func(t *testing.T, f *serverDirectoryFake) {
				if f.account != "账号甲" || !f.create {
					t.Fatalf("读目录参数丢失: account=%q create=%v", f.account, f.create)
				}
			},
		},
		{
			name: "CreateAccountRole", storeOp: "Create",
			request: func() proto.Message { return &dspb.CreateAccountRoleRequest{Account: "账号甲", Role: role()} },
			mutate:  func(in proto.Message) { in.(*dspb.CreateAccountRoleRequest).Role.AppearanceId = "被改外观" },
			invoke: func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
				return s.CreateAccountRole(ctx, in.(*dspb.CreateAccountRoleRequest))
			},
			check: func(t *testing.T, f *serverDirectoryFake) {
				if f.account != "账号甲" || !proto.Equal(f.role, role()) {
					t.Fatalf("建角原身份未透传: account=%q role=%v", f.account, f.role)
				}
			},
		},
		{
			name: "LookupAccountRole", storeOp: "Lookup",
			request: func() proto.Message { return &dspb.LookupAccountRoleRequest{PlayerId: ^uint64(0)} },
			mutate:  func(in proto.Message) { in.(*dspb.LookupAccountRoleRequest).PlayerId-- },
			invoke: func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
				return s.LookupAccountRole(ctx, in.(*dspb.LookupAccountRoleRequest))
			},
			check: func(t *testing.T, f *serverDirectoryFake) {
				if f.player != ^uint64(0) {
					t.Fatalf("全uint64角色号被改变: %d", f.player)
				}
			},
		},
		{
			name: "RepairAccountRole", storeOp: "Repair",
			request: func() proto.Message {
				return &dspb.RepairAccountRoleRequest{Account: "账号甲", Role: role(), ExpectedOwnerRevision: 17}
			},
			mutate: func(in proto.Message) { in.(*dspb.RepairAccountRoleRequest).ExpectedOwnerRevision++ },
			invoke: func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
				return s.RepairAccountRole(ctx, in.(*dspb.RepairAccountRoleRequest))
			},
			check: func(t *testing.T, f *serverDirectoryFake) {
				if f.account != "账号甲" || f.revision != 17 || !proto.Equal(f.role, role()) {
					t.Fatalf("修复未保留归属修订或身份: %+v", f)
				}
			},
		},
		{
			name: "RemoveAccountRoles", storeOp: "Remove",
			request: func() proto.Message {
				return &dspb.RemoveAccountRolesRequest{Account: "账号甲", Roles: []*dspb.AccountRoleVersion{{PlayerId: ^uint64(0), OwnerRevision: 17}, {PlayerId: 2, OwnerRevision: 9}}}
			},
			mutate: func(in proto.Message) { in.(*dspb.RemoveAccountRolesRequest).Roles[0].OwnerRevision++ },
			invoke: func(ctx context.Context, s *DataServiceServer, in proto.Message) (proto.Message, error) {
				return s.RemoveAccountRoles(ctx, in.(*dspb.RemoveAccountRolesRequest))
			},
			check: func(t *testing.T, f *serverDirectoryFake) {
				want := []*dspb.AccountRoleVersion{{PlayerId: ^uint64(0), OwnerRevision: 17}, {PlayerId: 2, OwnerRevision: 9}}
				if f.account != "账号甲" || len(f.versions) != len(want) {
					t.Fatalf("批量移除参数丢失: %+v", f)
				}
				for i := range want {
					if !proto.Equal(f.versions[i], want[i]) {
						t.Fatalf("角色归属修订被改变: got=%v want=%v", f.versions, want)
					}
				}
			},
		},
	}
}

func directoryVerifier(t *testing.T) *serviceauth.Verifier {
	t.Helper()
	verifier, err := serviceauth.New(map[string]string{"login": directoryTestSecret, "trade": directoryTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func signDirectoryRequest(t *testing.T, method string, request proto.Message, caller string, stamp uint64) {
	t.Helper()
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+method, request, caller, directoryTestSecret, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestAccountDirectoryRPCRejectsUnready(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		for _, missing := range []string{"auth", "store", "both"} {
			t.Run(rpc.name+"/"+missing, func(t *testing.T) {
				fake := &serverDirectoryFake{}
				sc := &svc.ServiceContext{AccountDirectory: fake, AccountDirectoryAuth: directoryVerifier(t)}
				if missing != "store" {
					sc.AccountDirectoryAuth = nil
				}
				if missing != "auth" {
					sc.AccountDirectory = nil
				}
				_, err := rpc.invoke(context.Background(), NewDataServiceServer(sc), rpc.request())
				if status.Code(err) != codes.Unavailable || len(fake.calls) != 0 {
					t.Fatalf("未就绪不应访问目录: err=%v calls=%v", err, fake.calls)
				}
			})
		}
	}
}

func TestAccountDirectoryRPCRequiresLoginProof(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		for _, rejected := range []string{"unsigned", "other-caller", "wrong-method", "changed-body", "expired"} {
			t.Run(rpc.name+"/"+rejected, func(t *testing.T) {
				fake := &serverDirectoryFake{}
				s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: fake, AccountDirectoryAuth: directoryVerifier(t)})
				request := rpc.request()
				if rejected != "unsigned" {
					method, caller, stamp := rpc.name, "login", uint64(time.Now().UnixMilli())
					if rejected == "other-caller" {
						caller = "trade"
					}
					if rejected == "wrong-method" {
						method += "Other"
					}
					if rejected == "expired" {
						stamp = 1
					}
					signDirectoryRequest(t, method, request, caller, stamp)
				}
				if rejected == "changed-body" {
					rpc.mutate(request)
				}
				_, err := rpc.invoke(context.Background(), s, request)
				if status.Code(err) != codes.PermissionDenied || len(fake.calls) != 0 {
					t.Fatalf("无效或越权证明不能访问目录: err=%v calls=%v", err, fake.calls)
				}
			})
		}
	}
}

func TestAccountDirectoryRPCPreservesInputsAndResults(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		t.Run(rpc.name, func(t *testing.T) {
			players := &base.AccountSimplePlayerList{Players: []*base.AccountSimplePlayer{{PlayerId: ^uint64(0), AppearanceId: "06_thunder_caster_boy"}}}
			owned := &dspb.AccountRoleOwnership{Account: "账号甲", Role: players.Players[0], OwnerRevision: 17}
			fake := &serverDirectoryFake{players: players, ownership: owned}
			s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: fake, AccountDirectoryAuth: directoryVerifier(t)})
			request := rpc.request()
			signDirectoryRequest(t, rpc.name, request, "login", uint64(time.Now().UnixMilli()))
			ctx := context.WithValue(context.Background(), struct{}{}, "目录请求上下文")
			out, err := rpc.invoke(ctx, s, request)
			var want proto.Message = &dspb.AccountDirectoryResponse{Players: players}
			if rpc.storeOp == "Lookup" {
				want = owned
			}
			if rpc.storeOp == "Remove" {
				want = &emptypb.Empty{}
			}
			if err != nil || !proto.Equal(out, want) || !reflect.DeepEqual(fake.calls, []string{rpc.storeOp}) || fake.ctx != ctx {
				t.Fatalf("成功应答或调用上下文改变: out=%v err=%v calls=%v", out, err, fake.calls)
			}
			rpc.check(t, fake)
		})
	}
}

// 规格 §3.6:经真的发布准入跑一遍。确定的拒绝不打成 Hold,结局未知照旧。Hold 不会自己清零,打错了排空证明就过不去。
func TestAccountDirectoryRejectionsHoldDrainOnlyWhenOutcomeUnknown(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		for _, failure := range []struct {
			name string
			err  error
			hold bool
		}{
			{"invalid", accountdirectory.ErrInvalid, false},
			{"missing", accountdirectory.ErrMissing, false},
			{"changed-owner", accountdirectory.ErrConflict, false},
			{"not-committed", fmt.Errorf("%w: %w", accountdirectory.ErrNotCommitted, errors.New("存档库故障")), false},
			// 投票问不齐、过户已提交但收尾没做完:结局已知,等重放,不能打成 Hold。
			{"retry-later", accountdirectory.ErrRetryLater, false},
			{"retry-later-with-cause", fmt.Errorf("%w: %w", accountdirectory.ErrRetryLater, errors.New("读失败")), false},
			{"unavailable", accountdirectory.ErrUnavailable, true},
			{"sql-failure", errors.New("提交结局未知"), true},
			// 满位是确定的拒绝,规格 §3.6。
			{"full", accountdirectory.ErrFull, false},
		} {
			t.Run(rpc.name+"/"+failure.name, func(t *testing.T) {
				s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: &serverDirectoryFake{failure: failure.err}, AccountDirectoryAuth: directoryVerifier(t)})
				request := rpc.request()
				signDirectoryRequest(t, rpc.name, request, "login", uint64(time.Now().UnixMilli()))
				requireDrainHold(t, failure.hold, func(ctx context.Context) error {
					_, err := rpc.invoke(ctx, s, request)
					return err
				})
			})
		}
	}
}

// 没有证明的调用什么都没做。
func TestAccountDirectoryBadProofDoesNotHoldDrain(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		t.Run(rpc.name, func(t *testing.T) {
			s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: &serverDirectoryFake{}, AccountDirectoryAuth: directoryVerifier(t)})
			request := rpc.request()
			requireDrainHold(t, false, func(ctx context.Context) error {
				_, err := rpc.invoke(ctx, s, request)
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("没有证明应该拒绝: %v", err)
				}
				return err
			})
		})
	}
}

func requireDrainHold(t *testing.T, want bool, call func(context.Context) error) {
	t.Helper()
	gate := drainproof.New()
	_, _ = serverbase.RootAdmission(gate)(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/data_service.DataService/Test"}, func(ctx context.Context, _ any) (any, error) {
		return nil, call(ctx)
	})
	gate.CloseAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := gate.Wait(ctx)
	if want && !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("结局未知必须 Hold: %v", err)
	}
	if !want && err != nil {
		t.Fatalf("确定的拒绝不该 Hold: %v", err)
	}
}

func TestAccountDirectoryRPCErrorsRemainDistinct(t *testing.T) {
	for _, rpc := range directoryRPCCases() {
		for _, failure := range []struct {
			name string
			err  error
			code codes.Code
		}{
			{"invalid", accountdirectory.ErrInvalid, codes.InvalidArgument},
			{"missing", accountdirectory.ErrMissing, codes.NotFound},
			{"full", accountdirectory.ErrFull, codes.ResourceExhausted},
			{"changed-owner", accountdirectory.ErrConflict, codes.FailedPrecondition},
			{"unavailable", accountdirectory.ErrUnavailable, codes.Unavailable},
			{"sql-failure", errors.New("不应回给调用方的SQL依赖细节"), codes.Unavailable},
			{"not-committed", accountdirectory.ErrNotCommitted, codes.Aborted},
			// 包着的原因不能把码带偏:login 只有收到 Aborted 才释放名字。
			{"not-committed-with-cause", fmt.Errorf("%w: %w", accountdirectory.ErrNotCommitted, accountdirectory.ErrUnavailable), codes.Aborted},
			// 错误链上有 ErrUnavailable,不能落到 default 回 Unavailable。
			{"retry-later", accountdirectory.ErrRetryLater, codes.Aborted},
		} {
			t.Run(rpc.name+"/"+failure.name, func(t *testing.T) {
				fake := &serverDirectoryFake{failure: fmt.Errorf("目录依赖: %w", failure.err)}
				s := NewDataServiceServer(&svc.ServiceContext{AccountDirectory: fake, AccountDirectoryAuth: directoryVerifier(t)})
				request := rpc.request()
				signDirectoryRequest(t, rpc.name, request, "login", uint64(time.Now().UnixMilli()))
				_, err := rpc.invoke(context.Background(), s, request)
				if status.Code(err) != failure.code || !reflect.DeepEqual(fake.calls, []string{rpc.storeOp}) {
					t.Fatalf("目录错误被混淆: err=%v want=%v calls=%v", err, failure.code, fake.calls)
				}
				if strings.Contains(status.Convert(err).Message(), "SQL依赖细节") {
					t.Fatalf("依赖细节不应出现在RPC应答: %v", err)
				}
			})
		}
	}
}
