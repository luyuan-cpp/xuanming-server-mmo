package logic

// 告警盲区:scene_manager_nodes_by_role 对「有节点的 zone」与「本进程配置的 ZoneId」的四种声明角色
// 补 0(metrics.go SetNodesByRole),整 zone 全灭时 PoolEmpty 的 `sum by (zone_id) (...) == 0` 支才有序列可比。

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodesByRoleSeriesForTest 读默认注册表里 scene_manager_nodes_by_role 的全部序列,键为 "zone/role"。
func nodesByRoleSeriesForTest(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	series := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "scene_manager_nodes_by_role" {
			continue
		}
		for _, metric := range family.GetMetric() {
			var zone, role string
			for _, pair := range metric.GetLabel() {
				switch pair.GetName() {
				case "zone_id":
					zone = pair.GetValue()
				case "role":
					role = pair.GetValue()
				}
			}
			series[zone+"/"+role] = metric.GetGauge().GetValue()
		}
	}
	return series
}

func TestRefreshLoadScores_ZeroFillsConfiguredZoneAndEmptyRoles(t *testing.T) {
	clearKnownNodesForTest()
	t.Cleanup(clearKnownNodesForTest)
	sc, _ := newTestSvcCtx(t, "")
	sc.Config.ZoneId = 7
	// role 0 = main_world。
	registerKnownNodeForTest(t, "SceneNodeService.rpc/role-metrics-10", testZoneId, "10")

	refreshLoadScores(sc)
	series := nodesByRoleSeriesForTest(t)
	for _, role := range []string{"main_world", "instance", "main_world_cross", "instance_cross"} {
		v, ok := series["7/"+role]
		assert.True(t, ok, "本进程配置的 zone 7 没有任何节点,也要发布 %s=0", role)
		assert.Zero(t, v)
	}
	_, ok := series["7/unknown"]
	assert.False(t, ok, "unknown 不补 0:它只在配置出错时出现,由 > 0 告警盯着")
	assert.Equal(t, 1.0, series["1/main_world"])
	v, ok := series["1/instance"]
	assert.True(t, ok, "有节点的 zone 里空着的角色要显式为 0,不能直接没有序列")
	assert.Zero(t, v)

	clearKnownNodesForTest()
	refreshLoadScores(sc)
	series = nodesByRoleSeriesForTest(t)
	_, ok = series["1/main_world"]
	assert.False(t, ok, "zone 不会被 sticky 保留:合服下线的 zone 不能永远误报")
	v, ok = series["7/instance"]
	assert.True(t, ok)
	assert.Zero(t, v)

	sc.Config.ZoneId = 0
	refreshLoadScores(sc)
	series = nodesByRoleSeriesForTest(t)
	for key := range series {
		assert.False(t, strings.HasPrefix(key, "0/"), "zone 0 不是合法 zone,不得补 0: %s", key)
	}
}
