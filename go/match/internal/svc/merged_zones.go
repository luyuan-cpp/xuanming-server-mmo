package svc

import (
	"context"

	"match/internal/authority"
	"match/internal/metrics"

	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
)

// loadMergedZones 从全局库读合服映射建缓存,绑给 Authority,并开始定时重读;StopAndReport 里停。
// match 有全局库连接,直接读 zone_merged_into,不走 data_service(guild、trade 没有这条连接才走 RPC)。
// 读不到返回错误,调用方拒绝启动。
func (sc *ServiceContext) loadMergedZones(ctx context.Context, store *authority.Store) error {
	metrics.PrimeMergedZoneRefresh()
	zones, err := store.LoadMergedZones(ctx, placement.MergedZoneCacheOptions{
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
