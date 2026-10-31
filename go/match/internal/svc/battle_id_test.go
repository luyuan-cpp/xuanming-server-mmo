package svc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	dspb "proto/data_service"
	"shared/idsegment"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type battleSegmentService struct {
	dspb.DataServiceClient
	next   atomic.Uint64
	badTag atomic.Bool
	err    error
}

func (f *battleSegmentService) AllocateIdSegment(_ context.Context, in *dspb.AllocateIdSegmentRequest, _ ...grpc.CallOption) (*dspb.AllocateIdSegmentResponse, error) {
	if in.GetBizTag() != BattleIDBizTag {
		f.badTag.Store(true)
	}
	if f.err != nil {
		return nil, f.err
	}
	hi := f.next.Add(uint64(in.GetStep())) + 1
	return &dspb.AllocateIdSegmentResponse{Lo: hi - uint64(in.GetStep()), Hi: hi}, nil
}

var enabledBattleSegment = idsegment.Conf{Enabled: true, Step: 100, MinStep: 10, MaxStep: 1000}

// 配置错了要在起服时发现:发不出 id 的 match 什么都做不了。
func TestNewBattleIDSegmentRejectsBadConfig(t *testing.T) {
	_, err := NewBattleIDSegment(idsegment.Conf{}, &battleSegmentService{})
	require.Error(t, err, "没写 Enabled: true")

	_, err = NewBattleIDSegment(enabledBattleSegment, nil)
	require.Error(t, err, "没配 DataServiceRpc")

	_, err = NewBattleIDSegment(idsegment.Conf{Enabled: true, Step: 100, MinStep: 200, MaxStep: 300}, &battleSegmentService{})
	require.ErrorIs(t, err, idsegment.ErrInvalidOptions, "MinStep > Step")
}

// 三种 id 共用一个生成器,发出来的号不重复也不为 0,都走 biz_tag = battle。
func TestSegmentBattleIDsGenerate(t *testing.T) {
	service := &battleSegmentService{}
	segment, err := NewBattleIDSegment(enabledBattleSegment, service)
	require.NoError(t, err)
	t.Cleanup(segment.Close)

	var generator BattleIDGenerator = segmentBattleIDs{segment: segment}
	seen := make(map[uint64]struct{})
	for range 500 {
		id, err := generator.Generate()
		require.NoError(t, err)
		require.NotZero(t, id)
		require.Less(t, id, idsegment.DefaultMaxIDExclusive)
		seen[id] = struct{}{}
	}
	assert.Len(t, seen, 500)
	assert.False(t, service.badTag.Load())
}

// 发号器不通时 Generate 必须报错,调用方才会整体失败。不能用 0 顶替。
func TestSegmentBattleIDsSurfacesFailure(t *testing.T) {
	down := errors.New("data_service down")
	segment, err := NewBattleIDSegment(enabledBattleSegment, &battleSegmentService{err: down})
	require.NoError(t, err)
	t.Cleanup(segment.Close)

	id, err := segmentBattleIDs{segment: segment}.Generate()
	require.ErrorIs(t, err, idsegment.ErrSegmentUnavailable)
	assert.Zero(t, id)
}
