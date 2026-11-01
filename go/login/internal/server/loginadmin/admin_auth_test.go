package server

import (
	"context"
	"testing"

	loginpb "proto/login"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testAdminToken = "login-admin-token-for-tests"

func callThrough(t *testing.T, token, method string, incoming metadata.MD) (bool, error) {
	t.Helper()
	ctx := context.Background()
	if incoming != nil {
		ctx = metadata.NewIncomingContext(ctx, incoming)
	}
	called := false
	_, err := AdminAuthInterceptor(token)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	})
	return called, err
}

func TestAdminAuthRefusesEveryAdminMethodWithoutTheToken(t *testing.T) {
	method := loginpb.LoginAdmin_RemovePlayersFromAccounts_FullMethodName
	cases := []struct {
		name     string
		token    string
		incoming metadata.MD
		pass     bool
	}{
		{name: "没配口令就停用,带什么都不行", token: "", incoming: metadata.Pairs(AdminTokenMetadataKey, testAdminToken)},
		{name: "没配口令时空值也不能算通过", token: "", incoming: metadata.Pairs(AdminTokenMetadataKey, "")},
		{name: "没带口令", token: testAdminToken},
		{name: "口令不对", token: testAdminToken, incoming: metadata.Pairs(AdminTokenMetadataKey, "wrong")},
		{name: "口令对", token: testAdminToken, incoming: metadata.Pairs(AdminTokenMetadataKey, testAdminToken), pass: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called, err := callThrough(t, tc.token, method, tc.incoming)
			if tc.pass {
				if err != nil || !called {
					t.Fatalf("应当放行: called=%t err=%v", called, err)
				}
				return
			}
			if called || status.Code(err) != codes.PermissionDenied {
				t.Fatalf("应当拒绝且不进 handler: called=%t err=%v", called, err)
			}
		})
	}
}

// 登录入口不要口令:只拦 LoginAdmin。
func TestAdminAuthLeavesOtherServicesAlone(t *testing.T) {
	called, err := callThrough(t, "", loginpb.ClientPlayerLogin_Login_FullMethodName, nil)
	if err != nil || !called {
		t.Fatalf("不该拦别的服务: called=%t err=%v", called, err)
	}
}
