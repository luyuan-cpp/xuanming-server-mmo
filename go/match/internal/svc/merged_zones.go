package svc

import (
	"context"

	"match/internal/authority"
	"match/internal/metrics"

	"shared/mergedzone"
	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
)

// loadMergedZones 从 data_service 读合服映射建缓存,绑给 Authority,并开始定时重读;StopAndReport 里停。
// 全局库只有 data_service 连,和 guild、trade 同一套。读不到返回错误,调用方拒绝启动。
// 启动预算由 mergedzone.Load 自己封顶:data_service 常常和 match 一起启动,不能用装配 SQL 的短预算。
func (sc *ServiceContext) loadMergedZones(ctx context.Context, store *authority.Store) error {
	metrics.PrimeMergedZoneRefresh()
	zones, err := mergedzone.Load(ctx, sc.DataServiceClient, placement.MergedZoneCacheOptions{
		OnRefresh: observeMergedZoneRefresh,
		OnChange: func(zones *placement.MergedZones) {
			logx.Infof("[match] merged zone mapping changed: %d merged zone(s)", len(zones.Merges()))
		},
	})
	if err != nil {
		return err
	}
	store.BindMergedZones(zones)
	sc.MergedZones = zones
	zones.Start()
	return nil
}

// observeMergedZoneRefresh 记一次定时重读的结局。失败时缓存保留旧值继续回答,这里只留痕。
func observeMergedZoneRefresh(err error) {
	if err != nil {
		metrics.ObserveMergedZoneRefresh(metrics.MergedZoneRefreshError)
		logx.Errorf("[match] merged zone refresh failed, keeping the previous mapping: %v", err)
		return
	}
	metrics.ObserveMergedZoneRefresh(metrics.MergedZoneRefreshOK)
}
