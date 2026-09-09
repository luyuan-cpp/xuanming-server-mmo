package idsegment

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
)

// fakeReq / fakeResp 模拟生成的 AllocateIdSegmentRequest / Response 的形状。
type fakeReq struct {
	bizTag string
	step   uint32
}

type fakeResp struct {
	code   uint32
	lo, hi uint64
}

func (r *fakeResp) GetErrorCode() uint32 { return r.code }
func (r *fakeResp) GetLo() uint64        { return r.lo }
func (r *fakeResp) GetHi() uint64        { return r.hi }

func buildFakeReq(bizTag string, step uint32) *fakeReq { return &fakeReq{bizTag: bizTag, step: step} }

func TestAdapt(t *testing.T) {
	t.Run("success passes lo/hi through and forwards the request fields", func(t *testing.T) {
		var got *fakeReq
		call := func(_ context.Context, in *fakeReq, _ ...grpc.CallOption) (*fakeResp, error) {
			got = in
			return &fakeResp{lo: 101, hi: 201}, nil
		}
		lo, hi, err := Adapt(call, buildFakeReq)(context.Background(), "player", 100)
		if err != nil || lo != 101 || hi != 201 {
			t.Fatalf("lo=%d hi=%d err=%v", lo, hi, err)
		}
		if got == nil || got.bizTag != "player" || got.step != 100 {
			t.Fatalf("request = %+v, want biz_tag=player step=100", got)
		}
	})

	t.Run("error_code != 0 becomes RPCError carrying the code", func(t *testing.T) {
		call := func(_ context.Context, _ *fakeReq, _ ...grpc.CallOption) (*fakeResp, error) {
			return &fakeResp{code: 7, lo: 1, hi: 2}, nil
		}
		_, _, err := Adapt(call, buildFakeReq)(context.Background(), "guild", 100)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("want *RPCError, got %v", err)
		}
		if rpcErr.Code != 7 || rpcErr.BizTag != "guild" {
			t.Fatalf("RPCError = %+v", rpcErr)
		}
	})

	t.Run("transport error is returned as-is", func(t *testing.T) {
		boom := errors.New("unavailable")
		call := func(_ context.Context, _ *fakeReq, _ ...grpc.CallOption) (*fakeResp, error) {
			return nil, boom
		}
		_, _, err := Adapt(call, buildFakeReq)(context.Background(), "guild", 100)
		if !errors.Is(err, boom) {
			t.Fatalf("want the transport error, got %v", err)
		}
	})
}
