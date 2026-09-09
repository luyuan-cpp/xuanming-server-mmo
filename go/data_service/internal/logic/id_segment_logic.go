package logic

import (
	"context"
	"errors"
	"fmt"

	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ── AllocateIdSegment(Leaf-segment 号段,设计 §6.2)─────────────
// 调用方(login 建角色 / guild 建公会 / C++ scene 铸物品 guid)一次领走 [lo, hi),
// 本地双 buffer 用完再来。本层只做参数校验、错误定性与指标;并发与值域由 store 保证。

type AllocateIdSegmentReq struct {
	BizTag string
	Step   uint32 // 0 = 沿用表里配置的 step
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
	if store.IsValidBizTag(req.BizTag) {
		tagLabel = req.BizTag
	}
	if svcCtx == nil || svcCtx.IdSegmentStore == nil {
		metrics.ObserveIdSegmentAllocate(tagLabel, "db_error")
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentDBError},
			fmt.Errorf("id segment store is unavailable")
	}

	lo, hi, err := svcCtx.IdSegmentStore.Allocate(ctx, req.BizTag, req.Step)
	switch {
	case err == nil:
		metrics.ObserveIdSegmentAllocate(tagLabel, "ok")
		return &AllocateIdSegmentResp{Lo: lo, Hi: hi}, nil

	case errors.Is(err, store.ErrIdSegmentInvalidTag), errors.Is(err, store.ErrIdSegmentInvalidStep):
		// 调用方参数问题,不算故障(与 ErrCodeInvalidRequest 的既定口径一致)。
		metrics.ObserveIdSegmentAllocate(tagLabel, "invalid")
		logx.Infof("[IdSegment] rejected biz_tag=%q step=%d: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil

	case errors.Is(err, store.ErrIdSegmentExhausted):
		// fail-closed:值域见底是事故级事件,表状态零变更,调用方必须停止发号。
		metrics.ObserveIdSegmentAllocate(tagLabel, "exhausted")
		logx.Errorf("[IdSegment] EXHAUSTED biz_tag=%s step=%d: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentExhausted}, nil

	case errors.Is(err, store.ErrIdSegmentUnknownTag):
		// 生产形态(IdSegment.AllowAutoSeed=false)下表里没有这一行:要么漏配 BootstrapTags,
		// 要么全局库被重置 / 从备份恢复 —— 后者正是"从 1 重发、覆盖别人角色行"的前夜。
		// 表零变更;与 Exhausted 同样是确定性状态,走 in-band 码(拦截器按 FaultCodeSet 记故障)。
		metrics.ObserveIdSegmentAllocate(tagLabel, "unknown_tag")
		logx.Errorf("[IdSegment] UNKNOWN TAG biz_tag=%s: no id_segment row and runtime auto-seed is disabled (production). "+
			"Bootstrap it: add %q to IdSegment.BootstrapTags and run `data_service -f <yaml> -migrate` (idempotent; also raises max_id above the consuming table). "+
			"If the global DB was just restored from a backup, treat this as an id-safety event and follow the runbook in docs/design/data_service_role_and_scope.md before minting: %v",
			req.BizTag, req.BizTag, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentUnknownTag}, nil

	default:
		metrics.ObserveIdSegmentAllocate(tagLabel, "db_error")
		logx.Errorf("[IdSegment] allocate biz_tag=%s step=%d failed: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentDBError}, err
	}
}
