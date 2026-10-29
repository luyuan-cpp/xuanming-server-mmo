package server

// GetPlayerHomeZone / BatchGetPlayerHomeZone 对「调用方已放弃」的口径(M1-2,2026-10-21):ctx 已取消 / 截止时间已过时
// 回 Canceled / DeadlineExceeded(status.FromContextError),不回 Unavailable + 码 1 / 30 —— 后者是本服务 / 全局库的故障口径,
// 会触发「缓存不可信、全局库也不可用」的 critical 告警。读方门禁开与关两种形态都钉住。ctx 一律用「已取消」或「截止时间是 1970 年」的,
// 不依赖墙钟;取消时 go-redis 取连接前就回 ctx 错误,结论与调度无关。

import (
	"context"
	"testing"
	"time"

	"proto/data_service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assertCallerGone 断言 err 是 want(Canceled / DeadlineExceeded)的 gRPC status,且文案里没有任何 error_code(不是故障口径)。
func assertCallerGone(t *testing.T, err error, want codes.Code, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	st, ok := status.FromError(err)
	require.True(t, ok, "not a gRPC status: %v", err)
	assert.Equal(t, want, st.Code(), msgAndArgs...)
	assert.NotContains(t, st.Message(), "error_code=", msgAndArgs...)
}

func TestHomeZoneRPCs_CallerGoneIsNotAFault(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"canceled", canceled, codes.Canceled},
		{"deadline exceeded", expired, codes.DeadlineExceeded},
	} {
		t.Run("read gate on/"+tc.name, func(t *testing.T) {
			reader := &readGateFakeReader{}
			s, _ := newReadGateTestServer(t, true, reader)
			resp, err := s.GetPlayerHomeZone(tc.ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 6301})
			assert.Nil(t, resp)
			assertCallerGone(t, err, tc.want, "GetPlayerHomeZone")
			batch, err := s.BatchGetPlayerHomeZone(tc.ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{6301, 6302}})
			assert.Nil(t, batch)
			assertCallerGone(t, err, tc.want, "BatchGetPlayerHomeZone")
			assert.Zero(t, reader.calls, "no player_route fallback with a finished ctx")
		})
		t.Run("read gate off/"+tc.name, func(t *testing.T) {
			s, _ := newHomeZoneTestServer(t)
			resp, err := s.GetPlayerHomeZone(tc.ctx, &data_service.GetPlayerHomeZoneRequest{PlayerId: 6303})
			assert.Nil(t, resp)
			assertCallerGone(t, err, tc.want, "GetPlayerHomeZone")
			batch, err := s.BatchGetPlayerHomeZone(tc.ctx, &data_service.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{6303}})
			assert.Nil(t, batch)
			assertCallerGone(t, err, tc.want, "BatchGetPlayerHomeZone")
		})
	}
}
