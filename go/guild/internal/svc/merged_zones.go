package svc

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"

	"shared/mergedzone"
	"shared/placement"
)

// merged_zone 刷新结果的 result 取值。
const (
	mergedZoneRefreshOK    = "ok"
	mergedZoneRefreshError = "error"
)

// guildMergedZoneRefreshTotal:定时重读合服映射的结果。error 持续上涨 = 这个副本在用旧映射,
// 合服之后它会把两个区当成不同的区,回一次可重试的拒绝;同区判定的结果等刷新恢复后自己回到正确。
var guildMergedZoneRefreshTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild",
	Subsystem: "merged_zone",
	Name:      "refresh_total",
	Help:      "定时重读合服映射(data_service.GetMergedZones)的结果。result: ok / error;error 时继续用旧映射。",
	Labels:    []string{"result"},
})

// PrimeMergedZoneMetrics 把 result 的取值预置为 0,让「出现即告警」的 increase(...) 不漏第一次。
// 必须在 zrpc.MustNewServer 之后调:go-zero 的指标开关由它打开,之前写的样本会被丢弃(同 server.PrimeGuildInternalMetrics)。
func PrimeMergedZoneMetrics() {
	guildMergedZoneRefreshTotal.Add(0, mergedZoneRefreshOK)
	guildMergedZoneRefreshTotal.Add(0, mergedZoneRefreshError)
}

// LoadMergedZones 从 data_service 取合服映射建缓存,不起定时重读(guild.go 负责 Start、Stop)。
// 读不到返回错误,guild.go 拒绝启动:分不清「没有合服」和「读不到」的实例不能对外服务。
func LoadMergedZones(ctx context.Context, client mergedzone.Client) (*placement.MergedZoneCache, error) {
	return mergedzone.Load(ctx, client, placement.MergedZoneCacheOptions{
		OnRefresh: observeMergedZoneRefresh,
		OnChange: func(zones *placement.MergedZones) {
			logx.Infof("[guild] merged zone mapping changed: %d merged zone(s)", len(zones.Merges()))
		},
	})
}

// observeMergedZoneRefresh 记一次定时重读的结局。失败时缓存保留旧值继续回答,这里只留痕。
func observeMergedZoneRefresh(err error) {
	if err != nil {
		guildMergedZoneRefreshTotal.Inc(mergedZoneRefreshError)
		logx.Errorf("[guild] merged zone refresh failed, keeping the previous mapping: %v", err)
		return
	}
	guildMergedZoneRefreshTotal.Inc(mergedZoneRefreshOK)
}
