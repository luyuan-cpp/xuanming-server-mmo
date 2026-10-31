package logic

import (
	"context"
	"errors"
	"fmt"

	"data_service/internal/constants"
	"data_service/internal/idissuer"
	"data_service/internal/metrics"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ── AllocateIdSegment(号段发号,docs/design/player-data-tidb-scaling-plan.md §6)───
// 调用方(login 建角色 / guild 建公会 / C++ scene 铸物品 guid)一次领走 [lo, hi),
// 本地双 buffer 用完再来。本层只做错误定性与指标;唯一性由 internal/idissuer 保证。

type AllocateIdSegmentReq struct {
	BizTag string
	Step   uint32 // 0 = 用发号器的默认长度
}

type AllocateIdSegmentResp struct {
	ErrorCode uint32
	Lo        uint64 // inclusive
	Hi        uint64 // exclusive
}

func AllocateIdSegment(ctx context.Context, svcCtx *svc.ServiceContext, req *AllocateIdSegmentReq) (*AllocateIdSegmentResp, error) {
	if req == nil || req.BizTag == "" {
		metrics.ObserveIdSegmentAllocate("invalid", "invalid")
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	// 指标 label 只收合法 tag,其余一律记成 "invalid"(label 基数必须有界)。
	tagLabel := "invalid"
	if idissuer.IsValidBizTag(req.BizTag) {
		tagLabel = req.BizTag
	}
	if svcCtx == nil || svcCtx.IdSegmentStore == nil {
		metrics.ObserveIdSegmentAllocate(tagLabel, "source_error")
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentSourceError},
			fmt.Errorf("id issuer is unavailable")
	}

	lo, hi, err := svcCtx.IdSegmentStore.Allocate(ctx, req.BizTag, req.Step)
	switch {
	case err == nil:
		metrics.ObserveIdSegmentAllocate(tagLabel, "ok")
		return &AllocateIdSegmentResp{Lo: lo, Hi: hi}, nil

	case errors.Is(err, idissuer.ErrInvalidTag), errors.Is(err, idissuer.ErrInvalidStep):
		// 调用方参数问题,不算故障(与 ErrCodeInvalidRequest 的既定口径一致)。
		metrics.ObserveIdSegmentAllocate(tagLabel, "invalid")
		logx.Infof("[IdSegment] rejected biz_tag=%q step=%d: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil

	case errors.Is(err, idissuer.ErrExhausted):
		// fail-closed:值域见底是事故级事件,调用方必须停止发号。
		metrics.ObserveIdSegmentAllocate(tagLabel, "exhausted")
		logx.Errorf("[IdSegment] EXHAUSTED biz_tag=%s step=%d: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentExhausted}, nil

	case errors.Is(err, idissuer.ErrUnknownTag):
		metrics.ObserveIdSegmentAllocate(tagLabel, "unknown_tag")
		logx.Errorf("[IdSegment] UNKNOWN TAG biz_tag=%s: add it to IdSegment.Tags in the data_service config", req.BizTag)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentUnknownTag}, nil

	case errors.Is(err, idissuer.ErrCounterMissing):
		// 不写任何记录。不是首次部署的话,这就是两套 etcd 同时丢了数据,从头发号会静默串档。
		metrics.ObserveIdSegmentAllocate(tagLabel, "records_missing")
		logx.Errorf("[IdSegment] REFUSED biz_tag=%s: %v", req.BizTag, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentUnknownTag}, nil

	default:
		metrics.ObserveIdSegmentAllocate(tagLabel, "source_error")
		logx.Errorf("[IdSegment] allocate biz_tag=%s step=%d failed: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentSourceError}, err
	}
}
