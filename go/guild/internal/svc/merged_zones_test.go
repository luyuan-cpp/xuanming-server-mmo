package svc

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 没配 DataServiceRpc 时 ServiceContext.DataServiceClient 是 nil 接口:拒绝启动,不能当成没有合服。
func TestLoadMergedZones_NoClientRefusesToStart(t *testing.T) {
	cache, err := LoadMergedZones(context.Background(), nil)
	require.Error(t, err)
	require.Nil(t, cache)
}

// 刷新失败只留痕:缓存自己保留旧值,这里不 panic(指标在 Prometheus 关着时是空操作)。
func TestObserveMergedZoneRefresh(t *testing.T) {
	observeMergedZoneRefresh(nil)
	observeMergedZoneRefresh(errors.New("data_service unavailable"))
	PrimeMergedZoneMetrics()
}
