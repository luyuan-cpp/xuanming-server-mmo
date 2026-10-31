package logic

// 断线 / 登出上报的 gate 实例号栅栏(docs/design/scene-enter-chain-residuals.md §6,余项 e)。
//
// 反例:gate 以同一 node_id 重启(或冲突收尾、跨 zone 同号),旧实例 I1 上的会话 3:k 与新实例 I2 上玩家 P 的
// 新会话号相同。I1 迟到的 Disconnect / LeaveGame 只比会话号的话,会给 I2 上活着的 P 挂租约(30s 后被踢)
// 或直接删掉 P 的会话。改动后请求与记录的实例号都非空且不等即忽略;任一侧为空不比较。
//
// 本文件不自建 smpb.SceneManagerClient 假实现(全包唯一的一份由 W2 的 scene_manager_client_fake_test.go 提供,
// §7「组间关系」):MarkOffline 若被错误放行,SceneManagerClient 为 nil 会让 LeaveScene 失败并把清理入租约
// ready 队列,于是用「会话逐字节保留 + ready 队列无成员」间接断言没有走到 LeaveScene。
// 辅助标识符统一加 gateInst 前缀,避免与同包别的测试撞名。

import (
	"context"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
)

const (
	gateInstSessionID = uint32(7)
	gateInstRecordI2  = "gate-I2"
	gateInstStaleI1   = "gate-I1"
)

// gateInstSeedOnlineSession 经 SetSession 写下一条 ONLINE 会话:会话号 7,实例号 recordInstance。
// SceneId 置 0,让 MarkOffline 的 LeaveScene 走「回读 SceneManager 权威 location」那条路(与首登现状一致)。
func gateInstSeedOnlineSession(t *testing.T, sc *svc.ServiceContext, playerID uint64, recordInstance string) *pb.PlayerSession {
	t.Helper()
	session := makeTestSession(playerID)
	session.SessionId = gateInstSessionID
	session.GateInstanceId = recordInstance
	session.SceneId = 0
	_, err := NewSetSessionLogic(context.Background(), sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	return session
}

// gateInstSeedAuthorityLocation 种一条 SceneManager 权威 location。MarkOffline 被错误放行时据此解析出 scene_id,
// 又因 SceneManagerClient 为 nil 而 LeaveScene 失败、清理入队 —— 这正是断言要抓的错误路径。
func gateInstSeedAuthorityLocation(t *testing.T, sc *svc.ServiceContext, playerID uint64) {
	t.Helper()
	data, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 50, NodeId: "scene-1"})
	require.NoError(t, err)
	require.NoError(t, sc.PlayerRedis.Set(context.Background(), sceneManagerLocationKey(playerID), data, 0).Err())
}

// gateInstStaleCount 读 player_locator_stale_gate_instance_total{op}。这是同号跨实例上报的唯一观测口
// (login 不在本地预检实例号,§6.2),所以要钉住「忽略即计数」。go-zero 的 metric 只在 prometheus 启用后才记数,
// 先 Enable;计数器是进程级全局量,用例一律比较前后差值。
func gateInstStaleCount(t *testing.T, op string) float64 {
	t.Helper()
	gzprom.Enable()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != "player_locator_stale_gate_instance_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "op" && l.GetValue() == op {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func gateInstReadyMembers(t *testing.T, mr *miniredis.Miniredis) []string {
	t.Helper()
	return leaseMembers(t, mr)
}

func TestSetDisconnecting_StaleGateInstanceIgnored(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const playerID uint64 = 1
	gateInstSeedOnlineSession(t, sc, playerID, gateInstRecordI2)
	before := readSessionBytes(t, sc, playerID)
	staleBefore := gateInstStaleCount(t, staleGateInstanceOpSetDisconnecting)

	// 旧实例 I1 迟到的断线:会话号与记录相同,实例号不同。
	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        playerID,
		SessionId:       gateInstSessionID,
		LeaseTtlSeconds: 30,
		GateInstanceId:  gateInstStaleI1,
	})
	require.NoError(t, err, "陈旧上报按 no-op 成功返回,login 不必重试")

	assert.Equal(t, before, readSessionBytes(t, sc, playerID), "记录必须逐字节不变(状态仍 ONLINE、版本不变)")
	got := readSession(t, sc, playerID)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_ONLINE, got.State)
	assert.NotContains(t, gateInstReadyMembers(t, mr), strconv.FormatUint(playerID, 10),
		"不得给新实例上的活会话挂租约,否则 30s 后活玩家被踢")
	assert.Equal(t, float64(1), gateInstStaleCount(t, staleGateInstanceOpSetDisconnecting)-staleBefore,
		"被忽略的陈旧断线必须计入 stale_gate_instance_total{op=\"set_disconnecting\"}")
}

func TestSetDisconnecting_EmptyGateInstanceProceeds(t *testing.T) {
	cases := map[string]struct {
		recordInstance  string
		requestInstance string
	}{
		"legacy login sends no instance":    {recordInstance: gateInstRecordI2, requestInstance: ""},
		"legacy record carries no instance": {recordInstance: "", requestInstance: gateInstStaleI1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sc, mr := newTestSvcCtx(t)
			const playerID uint64 = 2
			seeded := gateInstSeedOnlineSession(t, sc, playerID, tc.recordInstance)

			_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
				PlayerId:        playerID,
				SessionId:       gateInstSessionID,
				LeaseTtlSeconds: 30,
				GateInstanceId:  tc.requestInstance,
			})
			require.NoError(t, err)

			got := readSession(t, sc, playerID)
			assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, got.State, "任一侧实例号为空时不比较,照旧进入 DISCONNECTING")
			assert.Equal(t, seeded.SessionVersion+1, got.SessionVersion)
			assert.Contains(t, gateInstReadyMembers(t, mr), strconv.FormatUint(playerID, 10))
		})
	}
}

func TestSetDisconnecting_SameGateInstanceProceeds(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const playerID uint64 = 3
	seeded := gateInstSeedOnlineSession(t, sc, playerID, gateInstRecordI2)

	_, err := NewSetDisconnectingLogic(context.Background(), sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId:        playerID,
		SessionId:       gateInstSessionID,
		LeaseTtlSeconds: 30,
		GateInstanceId:  gateInstRecordI2,
	})
	require.NoError(t, err)

	got := readSession(t, sc, playerID)
	assert.Equal(t, pb.PlayerSessionState_SESSION_STATE_DISCONNECTING, got.State)
	assert.Equal(t, seeded.SessionVersion+1, got.SessionVersion)
	assert.Contains(t, gateInstReadyMembers(t, mr), strconv.FormatUint(playerID, 10))
}

func TestMarkOffline_StaleGateInstanceIgnored(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const playerID uint64 = 4
	seeded := gateInstSeedOnlineSession(t, sc, playerID, gateInstRecordI2)
	gateInstSeedAuthorityLocation(t, sc, playerID)
	before := readSessionBytes(t, sc, playerID)
	staleBefore := gateInstStaleCount(t, staleGateInstanceOpMarkOffline)

	// 旧实例 I1 迟到的登出:会话号与版本都对得上,实例号不同。
	_, err := NewMarkOfflineLogic(context.Background(), sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      gateInstSessionID,
		ExpectedSessionVersion: seeded.SessionVersion,
		ExpectedGateInstanceId: gateInstStaleI1,
	})
	require.NoError(t, err)

	assert.Equal(t, before, readSessionBytes(t, sc, playerID), "新实例上的会话必须逐字节保留")
	assert.Empty(t, gateInstReadyMembers(t, mr), "被忽略的登出不得走到 LeaveScene(错误放行时 nil client 会让清理入队)")
	assert.True(t, mr.Exists(sceneManagerLocationKey(playerID)), "SceneManager 的 location 不归 MarkOffline 直接删,这里只是确认没被动过")
	// login 不在本地预检实例号(§6.2),LeaveGame 这一面的同号跨实例只能在这里被看到。
	assert.Equal(t, float64(1), gateInstStaleCount(t, staleGateInstanceOpMarkOffline)-staleBefore,
		"被忽略的陈旧登出必须计入 stale_gate_instance_total{op=\"mark_offline\"}")
}

func TestMarkOffline_EmptyExpectedInstanceProceeds(t *testing.T) {
	sc, mr := newTestSvcCtx(t)
	const playerID uint64 = 5
	seeded := gateInstSeedOnlineSession(t, sc, playerID, gateInstRecordI2)

	// 旧版 login 不带实例号:不比较,照常删除。没有权威 location,LeaveScene 无需 client 即成功。
	_, err := NewMarkOfflineLogic(context.Background(), sc).MarkOffline(&pb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      gateInstSessionID,
		ExpectedSessionVersion: seeded.SessionVersion,
	})
	require.NoError(t, err)

	assert.False(t, mr.Exists(sessionKey(playerID)), "实例号为空时退回旧行为:会话照常删除")
	assert.Empty(t, gateInstReadyMembers(t, mr))
}

func TestIsStaleGateInstanceReport(t *testing.T) {
	assert.True(t, isStaleGateInstanceReport("gate-I2", "gate-I1"))
	assert.False(t, isStaleGateInstanceReport("gate-I2", "gate-I2"))
	assert.False(t, isStaleGateInstanceReport("", "gate-I1"), "记录无实例号:不比较")
	assert.False(t, isStaleGateInstanceReport("gate-I2", ""), "请求无实例号(旧版 login):不比较")
	assert.False(t, isStaleGateInstanceReport("", ""))
}
