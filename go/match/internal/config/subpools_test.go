package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 热池拆分的子池配置（docs/design/global-match-hierarchical-pools.md §5.2.1 第 5 条）：越界在加载配置时拒启，不悄悄夹紧。
func TestSubPoolsValidation(t *testing.T) {
	valid := GlobalMatchConf{CrossRegionWaitSeconds: 30, SubPoolMergeSeconds: 5,
		SubPools: []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 4}, {Mode: 3, ConfigId: 1, Count: 1}, {Mode: 1, ConfigId: 1, Count: MaxSubPools}, {Mode: 7, ConfigId: 1, Count: 2}}}
	require.NoError(t, valid.validateSubPools())
	// 没配 Count > 1 时合并时长不生效，不判：缺省配置与只写 Count 1 的配置都照常启动。
	require.NoError(t, GlobalMatchConf{CrossRegionWaitSeconds: 3, SubPoolMergeSeconds: 5}.validateSubPools())
	require.NoError(t, GlobalMatchConf{CrossRegionWaitSeconds: 3, SubPoolMergeSeconds: 0, SubPools: []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 1}}}.validateSubPools())

	for name, mutate := range map[string]func(*GlobalMatchConf){
		"PVE_SOLO 只有每组 1 人的池": func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 4, ConfigId: 1, Count: 2}} },
		"切磋没有池":               func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 6, ConfigId: 1, Count: 2}} },
		"3V3 未开放":             func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 2, ConfigId: 1, Count: 2}} },
		"未知模式":                func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 0, ConfigId: 1, Count: 2}} },
		"Count 为 0":           func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 0}} },
		"Count 超上限":           func(g *GlobalMatchConf) { g.SubPools = []SubPoolConf{{Mode: 5, ConfigId: 1, Count: MaxSubPools + 1}} },
		"同一模板写两行": func(g *GlobalMatchConf) {
			g.SubPools = []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 2}, {Mode: 5, ConfigId: 1, Count: 3}}
		},
		"合并时长为 0":      func(g *GlobalMatchConf) { g.SubPoolMergeSeconds = 0 },
		"合并时长不小于跨机房阈值": func(g *GlobalMatchConf) { g.SubPoolMergeSeconds = g.CrossRegionWaitSeconds },
	} {
		t.Run(name, func(t *testing.T) {
			g := GlobalMatchConf{CrossRegionWaitSeconds: 30, SubPoolMergeSeconds: 5, SubPools: []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 4}}}
			mutate(&g)
			require.Error(t, g.validateSubPools())
		})
	}
}

// 子池数缺省 1（不拆）；合并时长按秒换毫秒。
func TestSubPoolCountDefaultsToOne(t *testing.T) {
	g := GlobalMatchConf{SubPools: []SubPoolConf{{Mode: 5, ConfigId: 1, Count: 4}, {Mode: 7, ConfigId: 1, Count: 0}}}
	require.Equal(t, uint32(4), g.SubPoolCount(5, 1))
	require.Equal(t, uint32(1), g.SubPoolCount(5, 2), "没配的模板不拆")
	require.Equal(t, uint32(1), g.SubPoolCount(7, 1), "Count 0 按 1 算（加载时已被 validateSubPools 拒绝）")
	require.Equal(t, uint64(5000), GlobalMatchConf{SubPoolMergeSeconds: 5}.SubPoolMergeMs())
}
