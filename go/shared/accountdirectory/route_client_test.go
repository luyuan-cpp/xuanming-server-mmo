package accountdirectory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dspb "proto/data_service"
	"shared/serviceauth"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var routeTestSecret = strings.Repeat("d", 32)

type routeRPCFake struct {
	dspb.DataServiceClient
	got         *dspb.GetPlayerRouteRequest
	out         *dspb.GetPlayerRouteResponse
	err         error
	hasDeadline bool
}

func (f *routeRPCFake) GetPlayerRoute(ctx context.Context, in *dspb.GetPlayerRouteRequest, _ ...grpc.CallOption) (*dspb.GetPlayerRouteResponse, error) {
	f.got = in
	_, f.hasDeadline = ctx.Deadline()
	return f.out, f.err
}

func newRouteClientForTest(t *testing.T, fake *routeRPCFake) *RouteClient {
	t.Helper()
	client, err := NewRouteClient(fake, routeTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestRouteClientRequiresRPCAndSecret(t *testing.T) {
	if _, err := NewRouteClient(nil, routeTestSecret); err == nil {
		t.Fatal("没有客户端要拒绝")
	}
	if _, err := NewRouteClient(&routeRPCFake{}, ""); err == nil {
		t.Fatal("没有密钥要拒绝")
	}
	if _, err := NewRouteClient(&routeRPCFake{}, "short"); err == nil {
		t.Fatal("密钥太短要拒绝")
	}
}

// 请求带签名,服务端按 db 这个调用方、这个方法验得过;调用有上限,认领不会被目录拖住。
func TestRouteClientSignsAndBoundsTheCall(t *testing.T) {
	fake := &routeRPCFake{out: &dspb.GetPlayerRouteResponse{Found: true, StorageId: 7, HomeZone: 5}}
	storage, found, err := newRouteClientForTest(t, fake).StorageOf(context.Background(), 42)
	if err != nil || !found || storage != 7 {
		t.Fatalf("StorageOf = %d, %v, %v", storage, found, err)
	}
	if fake.got.GetPlayerId() != 42 {
		t.Fatalf("player = %d", fake.got.GetPlayerId())
	}
	if !fake.hasDeadline {
		t.Fatal("调用要有上限")
	}
	verifier, err := serviceauth.New(map[string]string{RouteCaller: routeTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := verifier.Verify(context.Background(), MethodPrefix+"GetPlayerRoute", fake.got)
	if err != nil || caller != RouteCaller {
		t.Fatalf("Verify = %q, %v", caller, err)
	}
}

// found=false 才是「没有落点」。别的都是读失败,不能当成没有:没有落点要查数据,读失败只要等目录恢复。
func TestRouteClientOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fake      routeRPCFake
		wantFound bool
		wantErr   bool
	}{
		{name: "读到", fake: routeRPCFake{out: &dspb.GetPlayerRouteResponse{Found: true, StorageId: 7}}, wantFound: true},
		{name: "没有落点", fake: routeRPCFake{out: &dspb.GetPlayerRouteResponse{}}},
		{name: "目录这次没读成", fake: routeRPCFake{out: &dspb.GetPlayerRouteResponse{Retry: true}}, wantErr: true},
		{name: "目录暂不可用", fake: routeRPCFake{err: status.Error(codes.Unavailable, "down")}, wantErr: true},
		{name: "鉴权失败", fake: routeRPCFake{err: status.Error(codes.PermissionDenied, "no")}, wantErr: true},
		{name: "超时", fake: routeRPCFake{err: context.DeadlineExceeded}, wantErr: true},
		{name: "别的错误", fake: routeRPCFake{err: errors.New("boom")}, wantErr: true},
		{name: "回了落点0", fake: routeRPCFake{out: &dspb.GetPlayerRouteResponse{Found: true}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			_, found, err := newRouteClientForTest(t, &fake).StorageOf(context.Background(), 42)
			if found != tc.wantFound {
				t.Fatalf("found = %v", found)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want ErrUnavailable", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// 签名带时间戳,每次调用都重新签,不复用过期的证明。
func TestRouteClientSignsEveryCall(t *testing.T) {
	fake := &routeRPCFake{out: &dspb.GetPlayerRouteResponse{Found: true, StorageId: 7}}
	client := newRouteClientForTest(t, fake)
	if _, _, err := client.StorageOf(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	first := fake.got
	time.Sleep(2 * time.Millisecond)
	if _, _, err := client.StorageOf(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if first == fake.got {
		t.Fatal("每次调用一个新请求")
	}
}
