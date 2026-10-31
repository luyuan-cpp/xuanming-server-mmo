package svc

import (
	"context"
	"errors"
	"testing"

	"match/internal/authority"
	"match/internal/metrics"

	"github.com/stretchr/testify/require"
)

func TestObserveMergedZoneRefreshCountsEachResult(t *testing.T) {
	ok, failed := metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshOK), metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshError)

	observeMergedZoneRefresh(nil)
	require.Equal(t, ok+1, metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshOK))
	require.Equal(t, failed, metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshError))

	observeMergedZoneRefresh(errors.New("db down"))
	require.Equal(t, ok+1, metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshOK))
	require.Equal(t, failed+1, metrics.MergedZoneRefreshValue(metrics.MergedZoneRefreshError))
}

// 读不到映射:回错误给启动流程拒启,不留半套装配;关停路径面对没装配的缓存也不出错。
func TestLoadMergedZonesFailureLeavesNothingBound(t *testing.T) {
	sc := &ServiceContext{}
	store := &authority.Store{}
	require.Error(t, sc.loadMergedZones(context.Background(), store), "没配全局库 schema")
	require.Nil(t, sc.MergedZones)
	require.Nil(t, store.MergedZones())
	sc.MergedZones.Stop()
}
