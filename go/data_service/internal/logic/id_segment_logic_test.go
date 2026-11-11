package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/idissuer"
	"data_service/internal/svc"
)

type fakeIdSource struct {
	lo, hi uint64
	err    error
}

func (f fakeIdSource) Close() error { return nil }

func (f fakeIdSource) Allocate(context.Context, string, uint32) (uint64, uint64, error) {
	return f.lo, f.hi, f.err
}

// 发号器的每种错误都要落到确定的错误码上;只有非确定性的故障才同时返回 gRPC 错误。
func TestAllocateIdSegment_ErrorMapping(t *testing.T) {
	storeDown := errors.New("etcd unavailable")
	cases := []struct {
		name     string
		err      error
		wantCode uint32
		wantErr  bool
	}{
		{"ok", nil, constants.ErrCodeOK, false},
		{"invalid tag", idissuer.ErrInvalidTag, constants.ErrCodeInvalidRequest, false},
		{"invalid step", idissuer.ErrInvalidStep, constants.ErrCodeInvalidRequest, false},
		{"exhausted", idissuer.ErrExhausted, constants.ErrCodeIdSegmentExhausted, false},
		{"wrapped exhausted", fmt.Errorf("reserve: %w", idissuer.ErrExhausted), constants.ErrCodeIdSegmentExhausted, false},
		{"unknown tag", idissuer.ErrUnknownTag, constants.ErrCodeIdSegmentUnknownTag, false},
		{"records missing", idissuer.ErrCounterMissing, constants.ErrCodeIdSegmentUnknownTag, false},
		{"region not registered", idissuer.ErrRegistryMissing, constants.ErrCodeIdSegmentUnknownTag, false},
		{"binding mismatch", fmt.Errorf("reserve: %w", idissuer.ErrBindingMismatch), constants.ErrCodeIdSegmentUnknownTag, false},
		{"region retired", idissuer.ErrRegionRetired, constants.ErrCodeIdSegmentUnknownTag, false},
		// 配置错误:重试不会变好,与绑定不符同一口径(refused,确定性拒绝,不回 gRPC 错误)。
		{"region mismatch", fmt.Errorf("reserve: %w", idissuer.ErrRegionMismatch), constants.ErrCodeIdSegmentUnknownTag, false},
		{"same cluster", fmt.Errorf("reserve: %w", idissuer.ErrSameCluster), constants.ErrCodeIdSegmentUnknownTag, false},
		{"contended", idissuer.ErrContended, constants.ErrCodeIdSegmentSourceError, true},
		{"store down", storeDown, constants.ErrCodeIdSegmentSourceError, true},
		{"caller deadline", context.DeadlineExceeded, constants.ErrCodeIdSegmentSourceError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx := &svc.ServiceContext{IdSegmentStore: fakeIdSource{lo: 10, hi: 20, err: tc.err}}
			resp, err := AllocateIdSegment(context.Background(), svcCtx, &AllocateIdSegmentReq{BizTag: "player", Step: 10})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if resp.ErrorCode != tc.wantCode {
				t.Fatalf("code = %d, want %d", resp.ErrorCode, tc.wantCode)
			}
			if tc.err == nil && (resp.Lo != 10 || resp.Hi != 20) {
				t.Fatalf("range = [%d, %d)", resp.Lo, resp.Hi)
			}
			if tc.err != nil && (resp.Lo != 0 || resp.Hi != 0) {
				t.Fatalf("a failed call must not return a range, got [%d, %d)", resp.Lo, resp.Hi)
			}
		})
	}
}

func TestAllocateIdSegment_NoIssuerFailsClosed(t *testing.T) {
	resp, err := AllocateIdSegment(context.Background(), &svc.ServiceContext{}, &AllocateIdSegmentReq{BizTag: "player"})
	if err == nil || resp.ErrorCode != constants.ErrCodeIdSegmentSourceError {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

// 指标的 biz_tag label 基数有界:只有登记过的 tag 原样记,调用方编出来的格式合法的 tag 记成 unregistered。
func TestBizTagLabel_OnlyRegisteredTagsBecomeLabels(t *testing.T) {
	restricted := &svc.ServiceContext{}
	restricted.Config.IdSegment.Tags = []string{"item"}
	cases := []struct {
		name   string
		svcCtx *svc.ServiceContext
		tag    string
		want   string
	}{
		{"default list", &svc.ServiceContext{}, "player", "player"},
		{"no service context uses the default list", nil, "player", "player"},
		{"well-formed but unregistered", &svc.ServiceContext{}, "made_up_tag_123", "unregistered"},
		{"malformed", &svc.ServiceContext{}, "Bad-Tag", "invalid"},
		{"configured list wins", restricted, "player", "unregistered"},
		{"configured tag", restricted, "item", "item"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bizTagLabel(tc.svcCtx, tc.tag); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllocateIdSegment_EmptyRequest(t *testing.T) {
	for _, req := range []*AllocateIdSegmentReq{nil, {}} {
		resp, err := AllocateIdSegment(context.Background(), &svc.ServiceContext{}, req)
		if err != nil || resp.ErrorCode != constants.ErrCodeInvalidRequest {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	}
}
