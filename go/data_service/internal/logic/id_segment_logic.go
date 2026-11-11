package logic

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"data_service/internal/config"
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
	tagLabel := bizTagLabel(svcCtx, req.BizTag)
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

	case errors.Is(err, idissuer.ErrCounterMissing), errors.Is(err, idissuer.ErrRegistryMissing):
		// 不写任何记录。新地域还没初始化,或者本地两份记录 / 登记处这个码的记录丢了;从头发号会静默串档。
		metrics.ObserveIdSegmentAllocate(tagLabel, "records_missing")
		logx.Errorf("[IdSegment] REFUSED biz_tag=%s: %v", req.BizTag, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentUnknownTag}, nil

	case errors.Is(err, idissuer.ErrBindingMismatch), errors.Is(err, idissuer.ErrRegionRetired),
		errors.Is(err, idissuer.ErrSameCluster), errors.Is(err, idissuer.ErrRegionMismatch):
		// 不写任何记录。这个部署环境或这两套 etcd 不是登记处给这个地域码绑的那一份(重复部署、克隆、换了 etcd 没有 rebind),
		// 这个码已退役,或者配置错了(两份记录落在同一套 etcd、记录里的地域和配置不一致)。重试不会变好,要人处理。
		metrics.ObserveIdSegmentAllocate(tagLabel, "refused")
		logx.Errorf("[IdSegment] REFUSED biz_tag=%s: %v", req.BizTag, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentUnknownTag}, nil

	default:
		metrics.ObserveIdSegmentAllocate(tagLabel, "source_error")
		logx.Errorf("[IdSegment] allocate biz_tag=%s step=%d failed: %v", req.BizTag, req.Step, err)
		return &AllocateIdSegmentResp{ErrorCode: constants.ErrCodeIdSegmentSourceError}, err
	}
}

// bizTagLabel 是指标的 biz_tag label。label 基数必须有界:只有登记进 IdSegment.Tags 的 tag 原样做 label;
// 格式合法但没登记的记成 "unregistered"(调用方可以随便编 tag,原样记就是无界的);格式都不对的记成 "invalid"。
func bizTagLabel(svcCtx *svc.ServiceContext, tag string) string {
	if !idissuer.IsValidBizTag(tag) {
		return "invalid"
	}
	var cfg config.IdSegmentConfig
	if svcCtx != nil {
		cfg = svcCtx.Config.IdSegment
	}
	if slices.Contains(cfg.EffectiveTags(), tag) {
		return tag
	}
	return "unregistered"
}
