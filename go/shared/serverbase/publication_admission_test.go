package serverbase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"shared/authoritysql"
	"shared/drainproof"
	"shared/generated/pb/table"
)

// holdsDrain 经真的发布准入跑一次 handler,回排空证明有没有被打成 Hold。根入口和派生入口是同一个判定,各跑一遍。
func holdsDrain(t *testing.T, handler grpc.UnaryHandler) bool {
	t.Helper()
	const relayMethod = "/test.Service/Relay"
	check := func(fullMethod string, gate func(root, relay *drainproof.Session) *drainproof.Session) bool {
		root, relay := drainproof.New(), drainproof.New()
		admission := PublicationAdmission(root, relay, map[string]bool{relayMethod: true})
		_, _ = admission(context.Background(), nil, newUnaryInfo(fullMethod), handler)
		target := gate(root, relay)
		target.CloseAdmission()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := target.Wait(ctx)
		if err != nil && !errors.Is(err, drainproof.ErrHold) {
			t.Fatalf("Wait: %v", err)
		}
		return err != nil
	}
	viaRoot := check("/test.Service/Root", func(root, _ *drainproof.Session) *drainproof.Session { return root })
	viaRelay := check(relayMethod, func(_, relay *drainproof.Session) *drainproof.Session { return relay })
	if viaRoot != viaRelay {
		t.Fatalf("根入口 %t,派生入口 %t,判定应该相同", viaRoot, viaRelay)
	}
	return viaRoot
}

// 状态码表,规格 docs/design/horizontal-scale-logical-db.md §3.6。全部 gRPC 状态码都要在这张表里,加了新码要表态。
func TestPublicationAdmissionHoldsByStatusCode(t *testing.T) {
	want := map[codes.Code]bool{
		// 服务端明确没有写入。
		codes.NotFound:           false,
		codes.InvalidArgument:    false,
		codes.FailedPrecondition: false,
		codes.AlreadyExists:      false,
		codes.PermissionDenied:   false,
		codes.Unauthenticated:    false,
		codes.OutOfRange:         false,
		// 这次请求没有结局未知的写:建角的 ErrNotCommitted、目录的 ErrRetryLater、friend 的身份检查。
		codes.Aborted: false,
		// 全仓只有目录的「账号角色数已满」回它:建角和过户都在提交点之前拒绝。
		codes.ResourceExhausted: false,
		// 提交可能已经成了。
		codes.Unavailable:      true,
		codes.DeadlineExceeded: true,
		codes.Canceled:         true,
		codes.Internal:         true,
		codes.Unknown:          true,
		// 没有逐个证明过的,拿不准归未知。
		codes.Unimplemented: true,
		codes.DataLoss:      true,
	}
	// codes.OK 不是错误,下面单独测。这里核对表没有漏码。
	for code := codes.Canceled; code <= codes.Unauthenticated; code++ {
		if _, listed := want[code]; !listed {
			t.Fatalf("状态码 %s 不在表里", code)
		}
	}
	for code, hold := range want {
		t.Run(code.String(), func(t *testing.T) {
			got := holdsDrain(t, func(context.Context, any) (any, error) { return nil, status.Error(code, "拒绝") })
			if got != hold {
				t.Fatalf("Hold = %t,应为 %t", got, hold)
			}
		})
	}
}

func TestPublicationAdmissionHoldsWhenHandlerReachedNoVerdict(t *testing.T) {
	for name, err := range map[string]error{
		"非 gRPC 错误":     errors.New("sql: connection lost"),
		"context 取消":    context.Canceled,
		"context 超时":    context.DeadlineExceeded,
		"包了一层的确定性拒绝":    fmt.Errorf("get home zone: %w", status.Error(codes.NotFound, "拒绝")),
		"Join 进去的确定性拒绝": errors.Join(errors.New("目录不可用"), status.Error(codes.PermissionDenied, "拒绝")),
	} {
		t.Run(name, func(t *testing.T) {
			if !holdsDrain(t, func(context.Context, any) (any, error) { return nil, err }) {
				t.Fatalf("没有下结论的错误必须 Hold: %v", err)
			}
		})
	}
}

// 闸门冻结确定没写,搬库窗口一过重试就成功,不能把这个 boot 的排空证明打成 Hold。
// 同一次请求里有过结局未知,或者错误没经过内层拦截器下结论,都算未知。
func TestPublicationAdmissionWriterFrozen(t *testing.T) {
	through := func(inner grpc.UnaryHandler) grpc.UnaryHandler {
		return func(ctx context.Context, req any) (any, error) {
			return WriterFrozenInterceptor()(ctx, req, newUnaryInfo("/test.Service/Method"), inner)
		}
	}
	cases := []struct {
		name    string
		handler grpc.UnaryHandler
		hold    bool
	}{
		{
			name: "冻结错误一路包着回来",
			handler: through(func(ctx context.Context, _ any) (any, error) {
				hitFrozenGate(ctx)
				return nil, fmt.Errorf("save: %w", authoritysql.ErrWriterFrozen)
			}),
		},
		{
			name: "handler 把冻结翻成了带内故障码",
			handler: through(func(ctx context.Context, _ any) (any, error) {
				hitFrozenGate(ctx)
				return &frozenTestResponse{ErrorMessage: &frozenTestTip{Id: 7}}, nil
			}),
		},
		{
			name: "之前有过结局未知",
			handler: through(func(ctx context.Context, _ any) (any, error) {
				authoritysql.NoteWriterOutcomeUnknown(ctx, authoritysql.ErrOutcomeUnknown)
				hitFrozenGate(ctx)
				return nil, authoritysql.ErrWriterFrozen
			}),
			hold: true,
		},
		{
			name: "没经过内层拦截器",
			handler: func(context.Context, any) (any, error) {
				return nil, authoritysql.ErrWriterFrozen
			},
			hold: true,
		},
		{
			name: "普通的 Unavailable",
			handler: through(func(context.Context, any) (any, error) {
				return nil, status.Error(codes.Unavailable, "storage failed")
			}),
			hold: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdsDrain(t, tc.handler); got != tc.hold {
				t.Fatalf("Hold = %t,应为 %t", got, tc.hold)
			}
		})
	}
}

// 规格 §3.6:自愈读不到目录时事务已回滚。handler 没映射、照旧报故障的,内层拦截器凭观察位改回可重试的状态,不 Hold。
// 映射成非故障 tip 的不动,客户端要的是 tip。这次请求有过结局未知就不改,照旧 Hold。
func TestNoWriteRetryFailure(t *testing.T) {
	faultTip := &respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginRedisError)}}
	businessTip := &respTip{tip: &fakeTip{id: uint32(table.BagError_kBagAddItemBagFull)}}
	denied := status.Error(codes.PermissionDenied, "denied")
	interceptors := map[string]grpc.UnaryServerInterceptor{
		"UnaryInterceptor":        UnaryInterceptor(Options{}),
		"WriterFrozenInterceptor": WriterFrozenInterceptor(),
	}
	cases := []struct {
		name     string
		note     bool
		unknown  bool
		resp     any
		err      error
		wantResp any
		wantErr  error
		hold     bool
	}{
		{name: "handler 回了故障 tip", note: true, resp: faultTip, wantErr: errNoWriteRetry},
		{name: "handler 回了 Unavailable", note: true, err: status.Error(codes.Unavailable, "storage failed"), wantErr: errNoWriteRetry},
		{name: "映射成非故障 tip 不动", note: true, resp: businessTip, wantResp: businessTip},
		{name: "确定性的拒绝不动", note: true, err: denied, wantErr: denied},
		{name: "handler 回了成功不动", note: true, resp: &respTip{}, wantResp: nil},
		{name: "有过结局未知照旧 Hold", note: true, unknown: true, resp: faultTip, wantResp: faultTip, hold: true},
		{name: "没有观察位照旧 Hold", resp: faultTip, wantResp: faultTip, hold: true},
	}
	for interceptorName, interceptor := range interceptors {
		for _, tc := range cases {
			t.Run(interceptorName+"/"+tc.name, func(t *testing.T) {
				handler := func(ctx context.Context, _ any) (any, error) {
					if tc.note {
						authoritysql.NoteNoWriteRetry(ctx)
					}
					if tc.unknown {
						authoritysql.NoteWriterOutcomeUnknown(ctx, authoritysql.ErrOutcomeUnknown)
					}
					return tc.resp, tc.err
				}
				resp, err := interceptor(context.Background(), nil, newUnaryInfo("/test.Service/Method"), handler)
				if err != tc.wantErr {
					t.Fatalf("err = %v,应为 %v", err, tc.wantErr)
				}
				if tc.wantResp != nil && resp != tc.wantResp {
					t.Fatalf("resp = %v,应为 %v", resp, tc.wantResp)
				}
				through := func(ctx context.Context, req any) (any, error) {
					return interceptor(ctx, req, newUnaryInfo("/test.Service/Method"), handler)
				}
				if got := holdsDrain(t, through); got != tc.hold {
					t.Fatalf("Hold = %t,应为 %t", got, tc.hold)
				}
			})
		}
	}
}

// 带内结论的判定不变:业务拒绝不 Hold,故障和码表漂移 Hold。
func TestPublicationAdmissionInBandVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp any
		hold bool
	}{
		{"成功", &respTip{}, false},
		{"业务拒绝", &respTip{tip: &fakeTip{id: uint32(table.BagError_kBagAddItemBagFull)}}, false},
		{"故障", &respTip{tip: &fakeTip{id: uint32(table.LoginError_kLoginRedisError)}}, true},
		{"码表漂移", &respTip{tip: &fakeTip{id: uint32(100000)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdsDrain(t, func(context.Context, any) (any, error) { return tc.resp, nil }); got != tc.hold {
				t.Fatalf("Hold = %t,应为 %t", got, tc.hold)
			}
		})
	}
}

// panic 没有结论,照旧 Hold,panic 继续往外抛。
func TestPublicationAdmissionHoldsOnPanic(t *testing.T) {
	gate := drainproof.New()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic 被吞了")
			}
		}()
		_, _ = RootAdmission(gate)(context.Background(), nil, newUnaryInfo("/test.Service/Root"), func(context.Context, any) (any, error) {
			panic("boom")
		})
	}()
	gate.CloseAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gate.Wait(ctx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("Wait = %v", err)
	}
}

// 判定不改错误本身:调用方看到的还是 handler 回的码。
func TestPublicationAdmissionKeepsTheError(t *testing.T) {
	want := status.Error(codes.FailedPrecondition, "拒绝")
	_, err := RootAdmission(drainproof.New())(context.Background(), nil, newUnaryInfo("/test.Service/Root"), func(context.Context, any) (any, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}
