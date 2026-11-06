package logic

// 跨 zone 重定向挑 gate 的分档(2026-11-05 缺口清单第 7 条残余;11-06 排空并入同一次分档):
//   - 纯函数 signRedirectToGate:完全合格的(没排空、没停写、没到自报上限的 90%)优先;没有时退到连得上的
//     (排空中或到水位,但没停写),全部停写才退到全部;与 login 的 CandidatesForZone + PickGate 同一份规则;
//   - staleGateIDs:同一条记录的 ModRevision 在本进程持续超过 gatepick.StaleAfter 才判停写,判定按 node_id 交给分档;
//   - 时限契约:健康 gate 的停写时长上界(一个上报周期 + 直读超时)小于 gatepick.StaleAfter。
// 分档规则本身(水位整数边界、各档回落原因)在 shared/gatepick 的单测里。

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

// 回归(2026-11-06):没排空的 gate 全部停写、只有排空中的健康时,票签给排空中的那台,不签给卡住的。
// 分层回落时排空那层先剔掉 1 号,停写那层只能在卡住的 2 号里回落:票签出去源实体就销毁了,玩家连不上只能断线重登。
func TestSignRedirectToGate_HealthyDrainingGateBeatsStale(t *testing.T) {
	draining := cappedGateNode(1, 40, 0, 30000)
	hung := cappedGateNode(2, 0, 0, 30001)
	fellBack := redirectCount(t, redirectFallbackMetric, gatepick.ReasonDraining)
	excludedStale := redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{draining, hung},
		gateMarks{draining: map[uint32]bool{1: true}, stale: map[uint32]bool{2: true}}, firstDraws)

	assert.Equal(t, uint32(1), got, "连得上的排空中 gate 胜过卡住的 gate")
	assert.Equal(t, 1.0, redirectCount(t, redirectFallbackMetric, gatepick.ReasonDraining)-fellBack)
	assert.Equal(t, 1.0, redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)-excludedStale)
}

// 没有完全合格的 gate 但有连得上的:在连得上的里按人数挑,停写的照样剔掉,回落原因按连得上的那些算(这里是 full)。
func TestSignRedirectToGate_StaleAndFullUsesReachableGate(t *testing.T) {
	hung := cappedGateNode(1, 0, 0, 30000)
	full := cappedGateNode(2, 50, 50, 30001)
	fellBack := redirectCount(t, redirectFallbackMetric, gatepick.ReasonFull)
	excludedStale := redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{hung, full}, gateMarks{stale: map[uint32]bool{1: true}}, firstDraws)

	assert.Equal(t, uint32(2), got, "到水位的 gate 还连得上,卡住的连不上")
	assert.Equal(t, 1.0, redirectCount(t, redirectFallbackMetric, gatepick.ReasonFull)-fellBack)
	assert.Equal(t, 1.0, redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)-excludedStale)
}

// 目标 zone 的 gate 全部停写:退到全部照常重定向、按人数挑(与 login 同语义,不让玩家无处可去),
// 计一次 fallback{reason="stale"};被用上的 gate 不计 excluded。
func TestSignRedirectToGate_AllStaleFallsBackToAll(t *testing.T) {
	busy := cappedGateNode(1, 80, 0, 30000)
	idle := cappedGateNode(2, 5, 0, 30001)
	fellBack := redirectCount(t, redirectFallbackMetric, gatepick.ReasonStale)
	excludedStale := redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)

	got := redirectGateIDMarked(t, []*commonpb.NodeInfo{busy, idle},
		gateMarks{stale: map[uint32]bool{1: true, 2: true}}, firstDraws)

	assert.Equal(t, uint32(2), got, "退到全部之后按人数挑")
	assert.Equal(t, 1.0, redirectCount(t, redirectFallbackMetric, gatepick.ReasonStale)-fellBack)
	assert.Equal(t, 0.0, redirectCount(t, redirectExcludedMetric, gatepick.ReasonStale)-excludedStale,
		"退到全部时一台都没剔")
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
