package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// /api/assign-gate 回 code=401(令牌无效,或 login 已关匿名分配)不是排队状态:状态机不等待、不轮询,
// 直接以 errAssignGateUnauthenticated 返回,交 resolveGateViaHTTPLocal 决定丢掉令牌重来还是停止。
func TestAssignGateWithQueue_UnauthenticatedReturnsTypedError(t *testing.T) {
	gw := &scriptedGateway{t: t, steps: []queueCall{
		{endpoint: "assign-gate", rsp: &httpAssignGateResponse{Code: 401, Error: "access token invalid"}},
	}}
	noSleep := func(context.Context, time.Duration) error {
		t.Fatal("401 不应等待")
		return nil
	}

	rsp, err := AssignGateWithQueue(context.Background(), gw, &httpAssignGateRequest{ZoneID: 1, AccessToken: "expired"}, nil, noSleep, zeroJitter)
	if rsp != nil {
		t.Fatalf("401 不应返回放行结果: %+v", rsp)
	}
	if !errors.Is(err, errAssignGateUnauthenticated) {
		t.Fatalf("err = %v, want errAssignGateUnauthenticated", err)
	}
	if len(gw.steps) != 0 {
		t.Fatalf("脚本还剩 %d 步没走完", len(gw.steps))
	}
}

func TestReusableAccessToken(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name   string
		token  string
		expire int64
		want   string
	}{
		{name: "empty", token: "", expire: now + 3600, want: ""},
		{name: "unknown expiry", token: "tok", expire: 0, want: "tok"},
		{name: "plenty of ttl", token: "tok", expire: now + 3600, want: "tok"},
		{name: "about to expire", token: "tok", expire: now + 30, want: ""},
		{name: "expired", token: "tok", expire: now - 1, want: ""},
	}
	for _, tc := range cases {
		if got := reusableAccessToken(tc.token, tc.expire); got != tc.want {
			t.Errorf("%s: reusableAccessToken = %q, want %q", tc.name, got, tc.want)
		}
	}
}
