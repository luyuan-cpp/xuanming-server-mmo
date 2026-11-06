package logic

// 跨 zone 重定向剔除停写与满载的 gate(2026-11-05,缺口清单第 7 条残余):
//   - 纯函数 signRedirectToGate:停写(marks.stale)与满载(NodeInfo.max_connections 的 90%)的 gate 不被选中,
//     全部不合格时回落;与排空过滤的先后顺序和 login 的 CandidatesForZone + PickGate 一致;
//   - staleGateIDs:同一条记录的 ModRevision 在本进程持续超过 gatepick.StaleAfter 才判停写,判定按 node_id 交给过滤;
//   - 时限契约:健康 gate 的停写时长上界(一个上报周期 + 直读超时)小于 gatepick.StaleAfter。
// 剔除与回落的规则本身(水位整数边界、回落原因)在 shared/gatepick 的单测里。

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	commonpb "proto/common/base"
	"shared/gatepick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

const (
	redirectExcludedMetric = "scene_manager_gate_redirect_excluded_total"
	redirectFallbackMetric = "scene_manager_gate_redirect_fallback_total"
)

// cappedGateNode 造一台自报连接上限的 gate(maxConnections 为 0 = 没声明),客户端地址按 port 区分。
func cappedGateNode(nodeID, playerCount, maxConnections, port uint32) *commonpb.NodeInfo {
	n := testGateNode(nodeID, 100, playerCount,
		testEndpoint(fmt.Sprintf("10.0.0.%d", nodeID), 18000), testEndpoint("127.0.0.1", port))
	n.MaxConnections = maxConnections
	return n
}

func redirectCount(t *testing.T, name, reason string) float64 {
	t.Helper()
	return counterValueForTest(t, name, map[string]string{
		"zone_id": strconv.FormatUint(uint64(testRedirectZoneID), 10), "reason": reason,
	})
}

// 停写的 gate 人再少也不选,并计一次 excluded{reason="stale"}。修复前签给人最少的 2 号。
func TestSignRedirectToGate_SkipsStaleGate(t *testing.T) {
	busy := cappedGateNode(1, 80, 0, 30000)
	hung := cappedGateNode(2, 5, 0, 30001)
	before := redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{busy, hung}, gateMarks{stale: map[uint32]bool{2: true}}, firstDraws)

	assert.Equal(t, uint32(1), got, "停写的 gate 多半卡住了,不接新玩家")
	assert.Equal(t, 1.0, redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)-before)
}

// 人数到自报上限 90% 的 gate 不选;没声明上限(0)的照常按人数挑。修复前签给人少的 2 号。
func TestSignRedirectToGate_SkipsFullGate(t *testing.T) {
	roomy := cappedGateNode(1, 80, 1000, 30000)
	full := cappedGateNode(2, 45, 50, 30001) // 45*100 >= 50*90
	before := redirectCount(t, redirectExcludedMetric, gatepick.ReasonFull)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{roomy, full}, gateMarks{}, firstDraws)

	assert.Equal(t, uint32(1), got, "到水位的 gate 不再接新玩家")
	assert.Equal(t, 1.0, redirectCount(t, redirectExcludedMetric, gatepick.ReasonFull)-before)

	undeclared := cappedGateNode(2, 45, 0, 30001)
	assert.Equal(t, uint32(2), redirectGateIDMarked(t, []*commonpb.NodeInfo{roomy, undeclared}, gateMarks{}, firstDraws),
		"没声明上限的 gate 不按上限判")
}

// 目标 zone 的 gate 全部停写或满载:回落全集照常重定向、按人数挑,并计一次 fallback(与 login 同语义,不让玩家无处可去)。
func TestSignRedirectToGate_AllStaleOrFullFallsBack(t *testing.T) {
	hung := cappedGateNode(1, 80, 0, 30000)
	full := cappedGateNode(2, 50, 50, 30001)
	before := redirectCount(t, redirectFallbackMetric, gatepick.ReasonMixed)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{hung, full}, gateMarks{stale: map[uint32]bool{1: true}}, firstDraws)

	assert.Equal(t, uint32(2), got, "回落之后按人数挑")
	assert.Equal(t, 1.0, redirectCount(t, redirectFallbackMetric, gatepick.ReasonMixed)-before)
}

// 排空过滤先于停写 / 满载过滤,与 login 的 CandidatesForZone(排空)+ PickGate(eligibleGates)同序:
// 不在排空的 gate 全部停写时,停写那层在它们之间回落,不回到排空中的 gate。
func TestSignRedirectToGate_DrainFilterRunsBeforeEligibility(t *testing.T) {
	draining := cappedGateNode(1, 10, 0, 30000)
	hung := cappedGateNode(2, 40, 0, 30001)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{draining, hung},
		gateMarks{draining: map[uint32]bool{1: true}, stale: map[uint32]bool{2: true}}, firstDraws)

	assert.Equal(t, uint32(2), got)
}

// staleGateIDs:第一次读没有历史不判;同一个 ModRevision 持续超过 StaleAfter 才判,判定按 node_id 交出;恢复重写后下一次读就不判。
func TestStaleGateIDs_JudgesByModRevisionAcrossReads(t *testing.T) {
	var tracker gatepick.RevisionTracker
	t0 := time.Unix(1_800_000_000, 0)
	record := func(nodeID uint32, modRev int64) gateRecord {
		kv := testGateKV(nodeID, fmt.Sprintf(`{"nodeId": %d, "zoneId": 2, "endpoint": {"ip": "10.0.0.%d", "port": 18000}}`,
			nodeID, nodeID))
		kv.ModRevision = modRev
		records := decodeGateNodes([]*mvccpb.KeyValue{kv})
		require.Len(t, records, 1)
		return records[0]
	}

	assert.Nil(t, staleGateIDs(&tracker, testRedirectZoneID, []gateRecord{record(1, 100), record(2, 200)}, t0),
		"第一次读没有历史可比,不判")
	// 1 号一直没重写,2 号按周期重写。
	assert.Equal(t, map[uint32]bool{1: true},
		staleGateIDs(&tracker, testRedirectZoneID, []gateRecord{record(1, 100), record(2, 230)}, t0.Add(gatepick.StaleAfter+time.Second)))
	// 1 号恢复上报。
	assert.Nil(t, staleGateIDs(&tracker, testRedirectZoneID, []gateRecord{record(1, 101), record(2, 231)}, t0.Add(gatepick.StaleAfter+2*time.Second)))
}

// 时限契约:健康 gate 被看到的同一个 ModRevision 至多停留「一个上报周期 + 一次直读超时」,必须小于停写阈值,
// 否则重定向读得密时会把健康的 gate 判成停写。
func TestGateFetchTimeout_KeepsHealthyGatesBelowStaleAfter(t *testing.T) {
	assert.Less(t, gatepick.ReportPeriod+gateFetchTimeout, gatepick.StaleAfter)
}
