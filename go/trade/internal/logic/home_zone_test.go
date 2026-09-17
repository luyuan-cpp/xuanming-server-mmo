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

// fakeBatchDataService 只实现 BatchGetPlayerHomeZone;其余方法经内嵌的 nil 接口调用会 panic,
// 正好证明 HomeZone 没有碰别的 RPC(尤其没有退回单查接口)。
type fakeBatchDataService struct {
	dspb.DataServiceClient
	resp        *dspb.BatchGetPlayerHomeZoneResponse
	err         error
	calls       int
	lastIDs     []uint64
	hadDeadline bool
}

func (f *fakeBatchDataService) BatchGetPlayerHomeZone(ctx context.Context, in *dspb.BatchGetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerHomeZoneResponse, error) {
	f.calls++
	f.lastIDs = in.GetPlayerIds()
	_, f.hadDeadline = ctx.Deadline()
	return f.resp, f.err
}

func TestDataServiceHomeZone(t *testing.T) {
	cases := []struct {
		name     string
		resp     *dspb.BatchGetPlayerHomeZoneResponse
		err      error
		wantZone uint32
		wantErr  bool
	}{
		{name: "mapped", resp: &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: map[uint64]uint32{9: 2}}, wantZone: 2},
		{name: "unmapped: key 缺席", resp: &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: map[uint64]uint32{8: 3}}},
		{name: "unmapped: 值为 0", resp: &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: map[uint64]uint32{9: 0}}},
		{name: "unmapped: 空 map", resp: &dspb.BatchGetPlayerHomeZoneResponse{}},
		{name: "mapping redis down", err: status.Error(codes.Unavailable, "error_code=6: dial tcp"), wantErr: true},
		{name: "NotFound 在批量接口里也是故障", err: status.Error(codes.NotFound, "no home zone mapping"), wantErr: true},
		{name: "deadline exceeded", err: status.Error(codes.DeadlineExceeded, "timeout"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeBatchDataService{resp: tc.resp, err: tc.err}

			zone, err := NewDataServiceHomeZone(client, 0).HomeZone(context.Background(), 9)

			assert.Equal(t, 1, client.calls)
			assert.Equal(t, []uint64{9}, client.lastIDs)
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

// player_id=0 过滤后 id 列表为空:不发请求,按未映射返回。
func TestDataServiceHomeZoneSkipsEmptyRequest(t *testing.T) {
	client := &fakeBatchDataService{}

	zone, err := NewDataServiceHomeZone(client, 0).HomeZone(context.Background(), 0)

	require.NoError(t, err)
	assert.Zero(t, zone)
	assert.Zero(t, client.calls, "空 id 列表不许发 BatchGetPlayerHomeZone")
}

// 没接线(nil 客户端)按故障返回,绝不能当成"未映射"。
func TestDataServiceHomeZoneWithoutClientFailsClosed(t *testing.T) {
	zone, err := NewDataServiceHomeZone(nil, 0).HomeZone(context.Background(), 9)
	require.Error(t, err)
	assert.Zero(t, zone)

	var nilLookup *DataServiceHomeZone
	_, err = nilLookup.HomeZone(context.Background(), 9)
	require.Error(t, err)
}
