package sessionmanager

// SetSession 把 player_locator 的 gRPC ABORTED 认成 ErrSessionChanged,其余错误一律不认。
//
// 这个映射是 EnterGame 提交会话时「重读重判」的唯一依据(clientplayerlogin 的 commitEnterGameSession):
// 认少了,gate 排空迁移途中的玩家会被一次毫秒级交错打回登录界面;认多了,结果未知的写入会被当成没写而重做。

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonbase "proto/common/base"
	plpb "proto/player_locator"
)

// setSessionOnlyLocator 只实现 SetSession;别的方法走内嵌的 nil 接口,调到即 panic。
type setSessionOnlyLocator struct {
	plpb.PlayerLocatorClient
	err error
}

func (f *setSessionOnlyLocator) SetSession(context.Context, *plpb.SetSessionRequest, ...grpc.CallOption) (*commonbase.Empty, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &commonbase.Empty{}, nil
}

func TestSetSession_AbortedIsReportedAsSessionChanged(t *testing.T) {
	locatorErr := status.Error(codes.Aborted, "stale session update for player 1: current version=5 incoming=5")

	err := SetSession(context.Background(), &setSessionOnlyLocator{err: locatorErr}, &PlayerSession{PlayerID: 1})

	if !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("SetSession error = %v, want it to carry ErrSessionChanged for a locator ABORTED", err)
	}
	// 原错误要留着:日志里得看得到 locator 说的版本号。
	if !errors.Is(err, locatorErr) {
		t.Fatalf("SetSession error = %v, want it to keep the locator's original error", err)
	}
}

func TestSetSession_OtherFailuresAreNotSessionChanged(t *testing.T) {
	cases := map[string]error{
		"deadline exceeded (outcome unknown)": status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		"unavailable (outcome unknown)":       status.Error(codes.Unavailable, "connection refused"),
		"stale login generation":              errors.New("stale session update for player 1: current login_generation=9 incoming=9"),
		"cleanup pending":                     status.Error(codes.Unknown, "previous session cleanup is still pending for player 1"),
		"canceled chain":                      context.Canceled,
	}
	for name, locatorErr := range cases {
		t.Run(name, func(t *testing.T) {
			err := SetSession(context.Background(), &setSessionOnlyLocator{err: locatorErr}, &PlayerSession{PlayerID: 1})
			if err == nil {
				t.Fatal("SetSession returned nil for a failed write")
			}
			if errors.Is(err, ErrSessionChanged) {
				t.Fatalf("SetSession error = %v, must not carry ErrSessionChanged: only a definite concurrent change may be redone", err)
			}
		})
	}
}

func TestSetSession_SuccessReturnsNil(t *testing.T) {
	if err := SetSession(context.Background(), &setSessionOnlyLocator{}, &PlayerSession{PlayerID: 1}); err != nil {
		t.Fatalf("SetSession error = %v, want nil", err)
	}
}
