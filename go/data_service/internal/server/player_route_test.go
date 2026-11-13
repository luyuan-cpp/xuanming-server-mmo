package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/routing"
	"data_service/internal/store"
	"data_service/internal/svc"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type playerRouteFake struct {
	rows map[uint64]store.RouteRow
	err  error
}

func (f playerRouteFake) RegisterHome(context.Context, store.RouteRegisterRequest) (store.RouteRegisterResult, error) {
	return store.RouteRegisterResult{}, errors.New("not used")
}

func (f playerRouteFake) CountHomeZones(context.Context, []uint32) (uint64, error) {
	return 0, errors.New("not used")
}

func (f playerRouteFake) GetRoutes(_ context.Context, ids []uint64) (map[uint64]store.RouteRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[uint64]store.RouteRow{}
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func playerRouteTestServer(t *testing.T, fake routing.RouteStore) *DataServiceServer {
	t.Helper()
	auth, err := serviceauth.New(map[string]string{accountdirectory.RouteCaller: directoryTestSecret})
	require.NoError(t, err)
	return NewDataServiceServer(&svc.ServiceContext{Router: routing.NewRouter(fake), PlayerRouteAuth: auth})
}

func signedPlayerRouteRequest(t *testing.T, method, caller string, player uint64) *dspb.GetPlayerRouteRequest {
	t.Helper()
	in := &dspb.GetPlayerRouteRequest{PlayerId: player}
	require.NoError(t, serviceauth.Sign(accountdirectory.MethodPrefix+method, in, caller, directoryTestSecret, uint64(time.Now().UnixMilli())))
	return in
}

func TestGetPlayerRouteReturnsPlacement(t *testing.T) {
	s := playerRouteTestServer(t, playerRouteFake{rows: map[uint64]store.RouteRow{
		1: {PlayerID: 1, HomeZone: 5, StorageID: 7, PlacementVersion: 1, Rev: 1},
		2: {PlayerID: 2, HomeZone: 5, Rev: 1},
	}})
	out, err := s.GetPlayerRoute(context.Background(), signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, 1))
	require.NoError(t, err)
	require.True(t, out.GetFound())
	require.EqualValues(t, 7, out.GetStorageId())
	require.EqualValues(t, 5, out.GetHomeZone())

	// 确定没有落点回 found=false:go/db 据此拒绝认领。带内回,不走 gRPC 错误。
	for _, player := range []uint64{2, 3} {
		out, err = s.GetPlayerRoute(context.Background(), signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, player))
		require.NoError(t, err, "player %d", player)
		require.False(t, out.GetFound(), "player %d", player)
		require.False(t, out.GetRetry(), "player %d", player)
	}
}

// 库故障回 retry,不能伪装成「没有落点」。
func TestGetPlayerRouteStoreFailureIsRetry(t *testing.T) {
	s := playerRouteTestServer(t, playerRouteFake{err: errors.New("Error 1205")})
	out, err := s.GetPlayerRoute(context.Background(), signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, 1))
	require.NoError(t, err)
	require.True(t, out.GetRetry())
	require.False(t, out.GetFound())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err = s.GetPlayerRoute(ctx, signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, 1))
	require.NoError(t, err)
	require.True(t, out.GetRetry())
}

// 规格 §7:没有落点、读失败都不触发 HOLD。
func TestGetPlayerRouteOutcomesDoNotHoldDrain(t *testing.T) {
	for name, fake := range map[string]playerRouteFake{"没有落点": {}, "库故障": {err: errors.New("Error 1205")}} {
		t.Run(name, func(t *testing.T) {
			s := playerRouteTestServer(t, fake)
			requireNoDrainHold(t, accountdirectory.MethodPrefix+"GetPlayerRoute", signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, 1), func(ctx context.Context, req any) (any, error) {
				return s.GetPlayerRoute(ctx, req.(*dspb.GetPlayerRouteRequest))
			})
		})
	}
}

// 只有 go/db 能读,证明绑定方法和请求内容。
func TestGetPlayerRouteRejectsBadProof(t *testing.T) {
	s := playerRouteTestServer(t, playerRouteFake{rows: map[uint64]store.RouteRow{1: {PlayerID: 1, HomeZone: 5, StorageID: 7, PlacementVersion: 1, Rev: 1}}})
	_, err := s.GetPlayerRoute(context.Background(), &dspb.GetPlayerRouteRequest{PlayerId: 1})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "没签名")
	_, err = s.GetPlayerRoute(context.Background(), signedPlayerRouteRequest(t, "GetPlayerRoute", "login", 1))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "别的调用方")
	_, err = s.GetPlayerRoute(context.Background(), signedPlayerRouteRequest(t, "GetRoleFenceState", accountdirectory.RouteCaller, 1))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "签的是别的方法")
	changed := signedPlayerRouteRequest(t, "GetPlayerRoute", accountdirectory.RouteCaller, 1)
	changed.PlayerId = 2
	_, err = s.GetPlayerRoute(context.Background(), changed)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "签完改了内容")
}

// 没配密钥时回 Unavailable,不放行。
func TestGetPlayerRouteWithoutAuthIsUnavailable(t *testing.T) {
	s := NewDataServiceServer(&svc.ServiceContext{Router: routing.NewRouter(playerRouteFake{})})
	_, err := s.GetPlayerRoute(context.Background(), &dspb.GetPlayerRouteRequest{PlayerId: 1})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
