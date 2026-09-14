package logic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dspb "proto/data_service"
)

// fakeDataService 只实现 GetPlayerHomeZone;其余方法经内嵌的 nil 接口调用会 panic,
// 正好证明 HomeZone 没有碰别的 RPC。
type fakeDataService struct {
	dspb.DataServiceClient
	resp        *dspb.GetPlayerHomeZoneResponse
	err         error
	hadDeadline bool
}

func (f *fakeDataService) GetPlayerHomeZone(ctx context.Context, _ *dspb.GetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	_, f.hadDeadline = ctx.Deadline()
	return f.resp, f.err
}

func TestDataServiceHomeZone(t *testing.T) {
	cases := []struct {
		name     string
		resp     *dspb.GetPlayerHomeZoneResponse
		err      error
		wantZone uint32
		wantErr  bool
	}{
		{name: "mapped", resp: &dspb.GetPlayerHomeZoneResponse{HomeZoneId: 2}, wantZone: 2},
		{name: "unmapped (NotFound)", err: status.Error(codes.NotFound, "error_code=5: no home zone mapping for player 9")},
		{name: "unmapped (legacy Unknown)", err: status.Error(codes.Unknown, "no home zone mapping for player 9")},
		{name: "mapping redis down", err: status.Error(codes.Unavailable, "error_code=6: dial tcp"), wantErr: true},
		{name: "other Unknown error", err: status.Error(codes.Unknown, "boom"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeDataService{resp: tc.resp, err: tc.err}

			zone, err := NewDataServiceHomeZone(client, 0).HomeZone(context.Background(), 9)

			assert.True(t, client.hadDeadline, "每次查询都必须带有界超时")
			if tc.wantErr {
				require.Error(t, err)
				assert.Zero(t, zone)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantZone, zone)
		})
	}
}
