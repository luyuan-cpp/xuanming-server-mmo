package logic

// 观战(设计文档 §10、决策 D9/D11、不变量 6/7/8)的 miniredis 测试:
// 索引登记/读取/剔除与 TTL、随机选场懒剔除、列表最新在前、WatchBattle 全部出口
// (排队/战斗中/离线/无战斗/指定场不存在/battle 拒绝/并发排队双检/换场清退/重看同场)、
// stopWatchingIfAny 三形态、gather 入口清退观众 + 开局登记。
// battle 节点的 AddObserver/RemoveObserver 用 addObserverFn/removeObserverFn 顶替
// (与 gather.go 的 prepareBattleFn 等四个 RPC 缝同一模式)。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"
	plpb "proto/player_locator"

	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeObserverRPCs 记录 match 对 battle 节点发出的观众挂/摘 RPC。
type fakeObserverRPCs struct {
	mu      sync.Mutex
	adds    []*battlepb.AddObserverRequest
	removes []*battlepb.RemoveObserverRequest
	// onAdd 决定 AddObserver 的响应;nil = 成功。
	onAdd func(req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error)
	// onRemove 决定 RemoveObserver 的结果(非 nil 错误 = 没有取消证明);nil = 成功。
	onRemove func(req *battlepb.RemoveObserverRequest) error
}

func stubObserverRPCs(t *testing.T) *fakeObserverRPCs {
	t.Helper()
	f := &fakeObserverRPCs{}
	prevAdd, prevRemove := addObserverFn, removeObserverFn
	addObserverFn = func(_ context.Context, _ string, req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		f.mu.Lock()
		f.adds = append(f.adds, req)
		onAdd := f.onAdd
		f.mu.Unlock()
		if onAdd != nil {
			return onAdd(req)
		}
		return &battlepb.AddObserverResponse{}, nil
	}
	removeObserverFn = func(_ string, req *battlepb.RemoveObserverRequest) error {
		f.mu.Lock()
		f.removes = append(f.removes, req)
		onRemove := f.onRemove
		f.mu.Unlock()
		if onRemove != nil {
			return onRemove(req)
		}
		return nil
	}
	t.Cleanup(func() { addObserverFn, removeObserverFn = prevAdd, prevRemove })
	return f
}

// setPlayerSessionOnline 模拟 player_locator 写 player:session:{id}:在线、挂在 gate 3。
func setPlayerSessionOnline(t *testing.T, mr *miniredis.Miniredis, playerId uint64) {
	t.Helper()
	raw, err := proto.Marshal(&plpb.PlayerSession{
		SessionId:      uint32(playerId % 1000),
		GateId:         "3",
		GateInstanceId: "gate-inst-3",
		State:          plpb.PlayerSessionState_SESSION_STATE_ONLINE,
		Account:        "acc_" + strconv.FormatUint(playerId, 10),
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playerSessionKey(playerId), string(raw)))
}

// registerBattle 按生产路径登记一场可观战战斗(node 7 = newGatherSvcCtx 灌的 battle 节点)。
func registerBattle(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64, createdAtMs uint64, names ...string) {
	t.Helper()
	registerSpectateBattle(svcCtx, battleId, 7, matchpb.MatchMode_MATCH_MODE_1V1, 0, names, createdAtMs)
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.NotNil(t, record, "登记后记录必须可读 battle=%d", battleId)
}

// staleCreatedAtMs 返回一个必然被判定为过期的 created_at(早于 TTL 窗口 1s)。
func staleCreatedAtMs(svcCtx *svc.ServiceContext) uint64 {
	return nowMs() - uint64(spectateTTLSeconds(svcCtx))*1000 - 1000
}

func watchBattle(t *testing.T, svcCtx *svc.ServiceContext, playerId, battleId uint64) *matchpb.WatchBattleResponse {
	t.Helper()
	resp, err := NewWatchBattleLogic(context.Background(), svcCtx).WatchBattle(&matchpb.WatchBattleRequest{
		PlayerId: playerId,
		BattleId: battleId,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

// watchingMark 读观战互斥标记,返回原件里的 battle_id 十进制;不存在返回空串,解析不出原件时原样返回值
// (用例写入的非法 / 他人标记)。
func watchingMark(t *testing.T, mr *miniredis.Miniredis, playerId uint64) string {
	t.Helper()
	if !mr.Exists(spectateWatchingKey(playerId)) {
		return ""
	}
	v, err := mr.Get(spectateWatchingKey(playerId))
	require.NoError(t, err)
	if original, err := decodeWatchingMark(playerId, v); err == nil {
		return strconv.FormatUint(original.GetBattleId(), 10)
	}
	return v
}

// watchingOriginal 读观战标记里固定的 AddObserver 原件(必须存在且合法)。
func watchingOriginal(t *testing.T, mr *miniredis.Miniredis, playerId uint64) *battlepb.AddObserverRequest {
	t.Helper()
	v, err := mr.Get(spectateWatchingKey(playerId))
	require.NoError(t, err)
	original, err := decodeWatchingMark(playerId, v)
	require.NoError(t, err)
	return original
}

// setWatchingMark 按生产格式铺一条「正在观战 battleId」的标记(原件指向 newGatherSvcCtx 的节点 7 实例),
// 返回原件供断言清退时的 RemoveObserver 带的是同一个 op。
func setWatchingMark(t *testing.T, mr *miniredis.Miniredis, playerId, battleId uint64) *battlepb.AddObserverRequest {
	t.Helper()
	original := newLocalObserverAdd(battleId, playerId, "acc_"+strconv.FormatUint(playerId, 10),
		&battlepb.BattleRouting{SessionId: 1, GateNodeId: 3, GateInstanceId: "gate-inst-3"}, testBattle7Instance)
	mark, err := encodeWatchingMark(original)
	require.NoError(t, err)
	require.NoError(t, mr.Set(spectateWatchingKey(playerId), mark))
	return original
}

// requireRemoveOf 断言一次 RemoveObserver 带的是原件的 battle / 观众 / op / 准入截止 / 实例(观战账本只认原 op)。
func requireRemoveOf(t *testing.T, original *battlepb.AddObserverRequest, remove *battlepb.RemoveObserverRequest, reason string) {
	t.Helper()
	require.Equal(t, original.GetBattleId(), remove.GetBattleId())
	require.Equal(t, original.GetObserverPlayerId(), remove.GetObserverPlayerId())
	require.Equal(t, original.GetSpectatorOperationId(), remove.GetSpectatorOperationId(), "Remove 必须带原 Add 的 op")
	require.Equal(t, original.GetAdmitBeforeMs(), remove.GetAdmitBeforeMs(), "Remove 必须带原 Add 的准入截止")
	require.Equal(t, original.GetTargetInstanceId(), remove.GetTargetInstanceId(), "Remove 必须带原 Add 的实例")
	require.Equal(t, reason, remove.GetReason())
}

func activeIndexSize(t *testing.T, svcCtx *svc.ServiceContext) int {
	t.Helper()
	card, err := svcCtx.MatchRedis.Zcard(spectateBattlesActiveKey)
	require.NoError(t, err)
	return card
}

// ---- 索引 ----

// 登记 → 记录字段完整、ZSET 含成员且 score=created_at、TTL=战斗时限+60s;剔除后两处皆空。
func TestSpectateRegisterLoadRemoveRoundTrip(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const battleId = uint64(880001)
	createdAt := nowMs()
	registerSpectateBattle(svcCtx, battleId, 7, matchpb.MatchMode_MATCH_MODE_5V5, 3, []string{"甲", "乙"}, createdAt)

	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Equal(t, battleId, record.GetSummary().GetBattleId())
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_5V5, record.GetSummary().GetMode())
	require.Equal(t, uint32(3), record.GetSummary().GetBattleConfigId())
	require.Equal(t, []string{"甲", "乙"}, record.GetSummary().GetPlayerNames())
	require.Equal(t, createdAt, record.GetSummary().GetCreatedAtMs())

	members, err := mr.ZMembers(spectateBattlesActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{"880001"}, members)
	score, err := mr.ZScore(spectateBattlesActiveKey, "880001")
	require.NoError(t, err)
	require.Equal(t, float64(createdAt), score)

	require.Equal(t, 300+60, spectateTTLSeconds(svcCtx), "TTL = BattleMaxDurationSeconds + 60s 余量")
	require.Equal(t, time.Duration(spectateTTLSeconds(svcCtx))*time.Second, mr.TTL(spectateBattleKey(battleId)))

	removeSpectateBattle(svcCtx, battleId)
	record, err = loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.Nil(t, record)
	require.Zero(t, activeIndexSize(t, svcCtx))
}

// 随机选场:活跃战斗与坏成员(过期 score / 非法 id)混在一起时,只可能返回活跃战斗。
func TestPickRandomBattleNeverReturnsStaleOrGarbage(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	fresh, stale := uint64(880010), uint64(880011)
	registerBattle(t, svcCtx, fresh, nowMs())
	registerBattle(t, svcCtx, stale, staleCreatedAtMs(svcCtx))
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), "not-a-battle-id")
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		picked, err := pickRandomBattle(svcCtx)
		require.NoError(t, err)
		require.Equal(t, fresh, picked, "第 %d 次:坏成员只能被剔除,不能被返回", i)
	}
}

// 随机选场:只剩坏成员时全部现场剔除(记录一并删),返回 0 表示无场可看。
func TestPickRandomBattleEvictsAllBadMembersAndReportsNone(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	stale := uint64(880012)
	registerBattle(t, svcCtx, stale, staleCreatedAtMs(svcCtx))
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), "garbage")
	require.NoError(t, err)

	picked, err := pickRandomBattle(svcCtx)
	require.NoError(t, err)
	require.Zero(t, picked)
	require.Zero(t, activeIndexSize(t, svcCtx), "过期与非法成员必须被现场剔除")
	require.False(t, mr.Exists(spectateBattleKey(stale)), "过期战斗的记录一并删除")
}

// matcher 每轮兜底:过期 score 成员清掉,活跃的保留。
func TestCleanupExpiredSpectateIndexKeepsFresh(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	fresh, stale := uint64(880020), uint64(880021)
	registerBattle(t, svcCtx, fresh, nowMs())
	registerBattle(t, svcCtx, stale, staleCreatedAtMs(svcCtx))

	cleanupExpiredSpectateIndex(svcCtx)

	members, err := mr.ZMembers(spectateBattlesActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{"880020"}, members)
}

// 列表:最新开局在前、孤儿成员(ZSET 有、记录已过期)现场剔除、limit 生效。
func TestListWatchableBattlesNewestFirstAndEvictsOrphans(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	base := nowMs()
	registerBattle(t, svcCtx, 880030, base-3000, "老场")
	registerBattle(t, svcCtx, 880031, base-2000, "中场")
	registerBattle(t, svcCtx, 880032, base-1000, "新场")
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(base), "880033") // 孤儿
	require.NoError(t, err)

	resp, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	ids := make([]uint64, 0, len(resp.GetBattles()))
	for _, b := range resp.GetBattles() {
		ids = append(ids, b.GetBattleId())
	}
	require.Equal(t, []uint64{880032, 880031, 880030}, ids, "created_at 降序,孤儿不出现")
	require.Equal(t, 3, activeIndexSize(t, svcCtx), "孤儿成员已被剔除")
	require.False(t, mr.Exists(spectateBattleKey(880033)))

	limited, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{Limit: 2})
	require.NoError(t, err)
	require.Len(t, limited.GetBattles(), 2)
	require.Equal(t, uint64(880032), limited.GetBattles()[0].GetBattleId())
}

// ---- WatchBattle 出口 ----

// D11:持有 match ticket(排队中)不能观战,不打 battle、不留标记。
func TestWatchBattleRejectsQueuedPlayer(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7101)
	setPlayerLocation(t, mr, watcher, 1)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880040, nowMs())
	require.Zero(t, joinQueue1v1(t, svcCtx, watcher).ErrorCode)

	resp := watchBattle(t, svcCtx, watcher, 0)
	require.Equal(t, constants.ErrSpectateWhileQueued, resp.GetErrorMessage().GetId())
	require.Zero(t, resp.GetBattleId())
	require.Empty(t, fake.adds, "被拒不能打到 battle 节点")
	require.Empty(t, watchingMark(t, mr, watcher))
}

// D11:battle:lock 存在(战斗中)不能观战。
func TestWatchBattleRejectsPlayerInBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7102)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880041, nowMs())
	require.NoError(t, mr.Set(battleLockKey(watcher), "880999"))

	resp := watchBattle(t, svcCtx, watcher, 880041)
	require.Equal(t, constants.ErrSpectateWhileInBattle, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 观众离线(无 player:session)没有可路由的会话:拒绝。
func TestWatchBattleRejectsOfflineObserver(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	registerBattle(t, svcCtx, 880042, nowMs())

	resp := watchBattle(t, svcCtx, 7103, 880042)
	require.Equal(t, constants.ErrSpectateOffline, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Empty(t, watchingMark(t, mr, 7103))
}

// 随机模式下索引为空:明确回"没有可观战的战斗"。
func TestWatchBattleRandomModeWithoutBattles(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7104)
	setPlayerSessionOnline(t, mr, watcher)

	resp := watchBattle(t, svcCtx, watcher, 0)
	require.Equal(t, constants.ErrNoWatchableBattle, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 成功路径:观众路由只带 session/gate/zone,scene 字段留 0(不变量 6:观众永远收不到结算);
// 观战标记 = battle_id 且带 TTL;响应回 battle_id。
func TestWatchBattleBindsObserverWithGateOnlyRouting(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7105)
	setPlayerLocation(t, mr, watcher, 2)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880050, nowMs(), "甲", "乙")

	resp := watchBattle(t, svcCtx, watcher, 880050)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, uint64(880050), resp.GetBattleId())

	require.Len(t, fake.adds, 1)
	req := fake.adds[0]
	require.Equal(t, uint64(880050), req.GetBattleId())
	require.Equal(t, watcher, req.GetObserverPlayerId())
	require.Equal(t, "acc_7105", req.GetObserverName())
	routing := req.GetRouting()
	require.NotNil(t, routing)
	require.Equal(t, uint32(7105%1000), routing.GetSessionId())
	require.Equal(t, uint32(3), routing.GetGateNodeId())
	require.Equal(t, "gate-inst-3", routing.GetGateInstanceId())
	require.Equal(t, uint32(2), routing.GetZoneId())
	require.Zero(t, routing.GetSceneNodeId(), "观众路由 scene 字段必须为 0(不变量 6)")
	require.Empty(t, routing.GetSceneInstanceId())

	// 观战账本三字段(battle_room_manager.cpp HandleAddObserver):规范 UUID op、准入截止在 (now, now+30s]、
	// 实例 = 节点 7 登记的 uuid;缺任何一项 battle 都回 kInvalidParameter。
	require.True(t, validSpectatorOperation(req.GetSpectatorOperationId()), "op=%q", req.GetSpectatorOperationId())
	now := nowMs()
	require.Greater(t, req.GetAdmitBeforeMs(), now)
	require.LessOrEqual(t, req.GetAdmitBeforeMs(), now+30000, "准入截止不得超过 now+30s")
	require.Equal(t, testBattle7Instance, req.GetTargetInstanceId())
	require.Equal(t, req.GetSpectatorOperationId(), resp.GetSpectatorOperationId(), "回包带回本次观战的 op")

	require.Equal(t, "880050", watchingMark(t, mr, watcher))
	require.True(t, proto.Equal(req, watchingOriginal(t, mr, watcher)), "观战标记固定的就是发出的 Add 原件")
	require.Equal(t, time.Duration(spectateTTLSeconds(svcCtx))*time.Second, mr.TTL(spectateWatchingKey(watcher)))
	require.Empty(t, fake.removes)
}

// 指定场:battle 回"房间不存在"(kEntityIsNull)→ 懒剔除索引、回滚标记、回"不存在或已结束"。
func TestWatchBattleExplicitMissingRoomEvictsIndex(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		return &battlepb.AddObserverResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在"),
		}, nil
	}
	const watcher = uint64(7106)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880060, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 880060)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1)
	require.Empty(t, watchingMark(t, mr, watcher), "AddObserver 失败必须回滚标记")
	require.False(t, mr.Exists(spectateBattleKey(880060)), "房间不存在 → 懒剔除记录")
	require.Zero(t, activeIndexSize(t, svcCtx))
}

// 随机模式:唯一一场已收尾 → 剔除后重试一次,再挑不到则回"没有可观战的战斗"。
func TestWatchBattleRandomModeEvictsFinishedOnlyBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		return &battlepb.AddObserverResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在"),
		}, nil
	}
	const watcher = uint64(7107)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880070, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 0)
	require.Equal(t, constants.ErrNoWatchableBattle, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1, "只有一场,剔除后第二次选场为空,不再打 battle")
	require.Zero(t, activeIndexSize(t, svcCtx))
	require.Empty(t, watchingMark(t, mr, watcher))
}

// battle RPC 失败/拒绝(非房间不存在):不动索引(战斗仍在打),只回滚标记。
func TestWatchBattleRpcFailureKeepsIndexRollsBackMark(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		return nil, errors.New("battle 节点不可达")
	}
	const watcher = uint64(7110)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880075, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 880075)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Empty(t, watchingMark(t, mr, watcher))
	require.True(t, mr.Exists(spectateBattleKey(880075)), "RPC 失败不能误剔除仍在进行的战斗")
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
}

// 并发排队双检:标记落地后、AddObserver 进行中玩家被凑单(ticket 出现)→ 自我清退:
// RemoveObserver(concurrent_queue)补偿解绑、删标记、回"匹配中无法观战"。
func TestWatchBattleDoubleCheckSelfEvictsOnConcurrentQueue(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7108)
	setPlayerLocation(t, mr, watcher, 1)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880080, nowMs())
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		// 模拟 TOCTOU 窗口:AddObserver 进行中玩家在别处 JoinQueue 成功。
		require.Zero(t, joinQueue1v1(t, svcCtx, watcher).ErrorCode)
		return &battlepb.AddObserverResponse{}, nil
	}

	resp := watchBattle(t, svcCtx, watcher, 880080)
	require.Equal(t, constants.ErrSpectateWhileQueued, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1)
	require.Len(t, fake.removes, 1)
	require.Equal(t, uint64(880080), fake.removes[0].GetBattleId())
	requireRemoveOf(t, fake.adds[0], fake.removes[0], "concurrent_queue")
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 换场:已在观战 A 时请求 B → 先按 A 的原件发 RemoveObserver(rewatch)再挂 B,标记改为 B。
func TestWatchBattleRewatchEvictsPreviousBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7109)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880090, nowMs())
	registerBattle(t, svcCtx, 880091, nowMs())
	require.Zero(t, watchBattle(t, svcCtx, watcher, 880090).GetErrorMessage().GetId())
	require.Equal(t, "880090", watchingMark(t, mr, watcher))

	resp := watchBattle(t, svcCtx, watcher, 880091)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Equal(t, uint64(880091), resp.GetBattleId())
	require.Len(t, fake.removes, 1)
	require.Equal(t, uint64(880090), fake.removes[0].GetBattleId())
	requireRemoveOf(t, fake.adds[0], fake.removes[0], "rewatch")
	require.Len(t, fake.adds, 2)
	require.NotEqual(t, fake.adds[0].GetSpectatorOperationId(), fake.adds[1].GetSpectatorOperationId(), "新观战新 op")
	require.Equal(t, "880091", watchingMark(t, mr, watcher))
}

// 重看同一场:先按旧原件 RemoveObserver(rewatch)再用新 op 挂观众。battle 名单里挂着旧 op 时,
// 新 op 的 Add 会被观战账本拒绝,旧原件的准入截止又多半已过、不能原样重放(与全局路径同口径)。
func TestWatchBattleSameBattleRewatchRemovesOriginalFirst(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7111)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880095, nowMs())

	require.Zero(t, watchBattle(t, svcCtx, watcher, 880095).GetErrorMessage().GetId())
	require.Zero(t, watchBattle(t, svcCtx, watcher, 880095).GetErrorMessage().GetId())
	require.Len(t, fake.adds, 2)
	require.Len(t, fake.removes, 1, "重看同场先摘旧 op")
	requireRemoveOf(t, fake.adds[0], fake.removes[0], "rewatch")
	require.NotEqual(t, fake.adds[0].GetSpectatorOperationId(), fake.adds[1].GetSpectatorOperationId())
	require.Equal(t, "880095", watchingMark(t, mr, watcher))
	require.True(t, proto.Equal(fake.adds[1], watchingOriginal(t, mr, watcher)), "标记换成新原件")
}

// ---- 清退 ----

// stopWatchingIfAny 各形态:记录在 → 按原件摘观众 + 删标记;记录已清 → 只删标记;标记非法 / 缺原 op 的
// 十进制旧标记 / 他人原件 → 只删标记;节点已换实例 → 不发 Remove、只删标记。
func TestStopWatchingIfAnyPaths(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)

	// 1) 战斗仍在:按原件 RemoveObserver(同 op / 截止 / 实例)+ 删标记
	registerBattle(t, svcCtx, 880110, nowMs())
	original := setWatchingMark(t, mr, 7301, 880110)
	stopWatchingIfAny(svcCtx, 7301, "unit")
	require.Len(t, fake.removes, 1)
	requireRemoveOf(t, original, fake.removes[0], "unit")
	require.Empty(t, watchingMark(t, mr, 7301))

	// 2) 战斗已收尾(记录 TTL 自清):只删标记,不打 battle
	setWatchingMark(t, mr, 7302, 880111)
	stopWatchingIfAny(svcCtx, 7302, "unit")
	require.Len(t, fake.removes, 1)
	require.Empty(t, watchingMark(t, mr, 7302))

	// 3) 标记值非法:只删标记
	require.NoError(t, mr.Set(spectateWatchingKey(7303), "oops"))
	stopWatchingIfAny(svcCtx, 7303, "unit")
	require.Len(t, fake.removes, 1)
	require.Empty(t, watchingMark(t, mr, 7303))

	// 4) 没在观战:无副作用
	stopWatchingIfAny(svcCtx, 7304, "unit")
	require.Len(t, fake.removes, 1)

	// 5) 账本上线前的十进制 battle_id 旧标记:拿不出原 op,battle 不会据它摘人,只删标记
	require.NoError(t, mr.Set(spectateWatchingKey(7305), "880110"))
	stopWatchingIfAny(svcCtx, 7305, "unit")
	require.Len(t, fake.removes, 1)
	require.Empty(t, watchingMark(t, mr, 7305))

	// 6) 标记里的原件属于别的玩家:不拿它去摘人,只删标记
	foreign := setWatchingMark(t, mr, 7306, 880110)
	mark, err := encodeWatchingMark(foreign)
	require.NoError(t, err)
	require.NoError(t, mr.Set(spectateWatchingKey(7307), mark))
	stopWatchingIfAny(svcCtx, 7307, "unit")
	require.Len(t, fake.removes, 1)
	require.Empty(t, watchingMark(t, mr, 7307))

	// 7) 节点 7 已换实例(进程重启):原实例的房间与观众已随进程消失,不发 Remove,只删标记
	svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7",
		discovery.NodeEntry{NodeId: 7, NodeUuid: "0d7c2f55-3b9e-4a61-8c4f-6e1a2b3c4d5e", Endpoint: "battle-7"})
	stopWatchingIfAny(svcCtx, 7306, "unit")
	require.Len(t, fake.removes, 1)
	require.Empty(t, watchingMark(t, mr, 7306))
}

// D11 排队侧 + D9:进 gather 的成员若在观战,入口清退(RemoveObserver enter_gather + 删标记);
// 开局成功后新战斗登记进观战索引。gather 真跑,scene/battle RPC 全部打桩。
func TestGatherEntryEvictsSpectatorAndRegistersBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rpcs := stubGatherRPCs(t, map[uint64]string{7201: "fp", 7202: "fp"})
	observers := stubObserverRPCs(t)
	registerBattle(t, svcCtx, 880100, nowMs(), "旧甲", "旧乙")
	original := setWatchingMark(t, mr, 7201, 880100) // 7201 正在观战旧场
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 7201, 7202)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, observers.removes, 1)
	requireRemoveOf(t, original, observers.removes[0], "enter_gather")
	require.Empty(t, watchingMark(t, mr, 7201))
	require.Empty(t, observers.adds)

	require.Len(t, rpcs.created, 1)
	newBattle := rpcs.created[0].GetBattleId()
	record, err := loadSpectateBattle(svcCtx, newBattle)
	require.NoError(t, err)
	require.NotNil(t, record, "开局成功的战斗必须登记可观战(D9)")
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Equal(t, matchpb.MatchMode_MATCH_MODE_1V1, record.GetSummary().GetMode())
	require.Len(t, record.GetSummary().GetPlayerNames(), 2)
	require.Equal(t, 2, activeIndexSize(t, svcCtx), "旧场 + 新场")
}

// ---- WatchBattle 剩余出口(按 watchbattlelogic.go 逐个 return 点数出来的)----

// 无身份(session 无 player、请求体也没带)→ ErrInternal,不产生任何副作用
func TestWatchBattleWithoutIdentity(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	registerBattle(t, svcCtx, 880120, nowMs())

	resp, err := NewWatchBattleLogic(context.Background(), svcCtx).
		WatchBattle(&matchpb.WatchBattleRequest{BattleId: 880120})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
	require.False(t, mr.Exists(spectateWatchingKey(0)))
}

// 观战标记值非法(不是十进制 battle_id)→ 清掉残留后照常接入新场,不误发 RemoveObserver
func TestWatchBattleGarbageWatchingMarkIsHealed(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7112)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880130, nowMs())
	require.NoError(t, mr.Set(spectateWatchingKey(watcher), "not-a-number"))

	resp := watchBattle(t, svcCtx, watcher, 880130)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, uint64(880130), resp.GetBattleId())
	require.Empty(t, fake.removes, "非法标记只能删,不能拿它去 battle 摘观众")
	require.Equal(t, "880130", watchingMark(t, mr, watcher))
}

// 会话里的 gate_id 不是数字(battle 出站 topic 是 gate-{数字},解析不了就没法路由)→ ErrInternal
func TestWatchBattleRejectsNonNumericGateId(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7113)
	raw, err := proto.Marshal(&plpb.PlayerSession{
		SessionId:      1,
		GateId:         "gate-a",
		GateInstanceId: "inst",
		State:          plpb.PlayerSessionState_SESSION_STATE_ONLINE,
		Account:        "acc",
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playerSessionKey(watcher), string(raw)))
	registerBattle(t, svcCtx, 880140, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 880140)
	require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 指定场的记录已被 TTL 清掉(ZSET 成员还残留)→ 懒剔除索引后回「不存在或已结束」,不打 battle
func TestWatchBattleExplicitRecordExpiredEvictsIndexWithoutRpc(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7114)
	setPlayerSessionOnline(t, mr, watcher)
	// 只有 ZSET 成员、没有记录:TTL 先到期的真实形态
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), "880150")
	require.NoError(t, err)

	resp := watchBattle(t, svcCtx, watcher, 880150)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds, "记录都没有就不该打 battle 节点")
	require.Zero(t, activeIndexSize(t, svcCtx), "残留成员必须懒剔除")
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 列表 limit:0 走默认、超过上限被夹到上限(防客户端一次拉爆索引)
func TestListWatchableBattlesClampsLimit(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	base := nowMs()
	for i := 0; i < maxWatchableListLimit+5; i++ {
		registerBattle(t, svcCtx, uint64(881000+i), base-uint64(i))
	}

	capped, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{Limit: maxWatchableListLimit + 100})
	require.NoError(t, err)
	require.Len(t, capped.GetBattles(), maxWatchableListLimit, "超上限的 limit 必须被夹住")

	defaulted, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	require.Len(t, defaulted.GetBattles(), defaultWatchableListLimit, "limit=0 走默认条数")
}

// ---- 补齐评审点出的剩余出口(逐条对着 watchbattlelogic.go / spectate.go 的 return 数的)----

// 随机模式真正的「换一场重试」:第一次挂观众被告知房间不存在 → 剔除后换一场并成功接入。
// 不依赖 pickRandomBattle 的选序:第一次调用一律失败、第二次一律成功,
// 因此断言「打了两次、最终成功、失败那场被剔除、成功那场留存」在任何选序下都成立。
func TestWatchBattleRandomModeSwitchesToAnotherBattleAfterMissingRoom(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7115)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880160, nowMs())
	registerBattle(t, svcCtx, 880161, nowMs()-1000)

	var attempts int
	var firstTried uint64
	fake.onAdd = func(req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		attempts++
		if attempts == 1 {
			firstTried = req.GetBattleId()
			return &battlepb.AddObserverResponse{
				ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在"),
			}, nil
		}
		return &battlepb.AddObserverResponse{}, nil
	}

	resp := watchBattle(t, svcCtx, watcher, 0)
	require.Zero(t, resp.GetErrorMessage().GetId(), "换一场应当成功 tip=%v", resp.GetErrorMessage())
	require.Len(t, fake.adds, 2, "必须换一场重试(maxAttempts=2),而不是一次就放弃")
	require.NotEqual(t, firstTried, resp.GetBattleId(), "重试必须换到另一场")
	require.Equal(t, fake.adds[1].GetBattleId(), resp.GetBattleId())

	require.False(t, mr.Exists(spectateBattleKey(firstTried)), "已收尾的那场必须被懒剔除")
	require.True(t, mr.Exists(spectateBattleKey(resp.GetBattleId())), "成功接入的那场必须留存")
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
	require.Equal(t, strconv.FormatUint(resp.GetBattleId(), 10), watchingMark(t, mr, watcher))
}

// 索引里混着「只有 ZSET 成员、记录已被 TTL 清掉」的孤儿:随机模式不能因此失败,
// 最终必须接入那场有记录的战斗(选序随机,但结果确定)。
func TestWatchBattleRandomModeSkipsRecordlessMember(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7116)
	const liveBattle = uint64(880171)
	setPlayerSessionOnline(t, mr, watcher)
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()-2000), "880170") // 孤儿
	require.NoError(t, err)
	registerBattle(t, svcCtx, liveBattle, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 0)
	require.Zero(t, resp.GetErrorMessage().GetId(), "孤儿成员不该让随机观战失败 tip=%v", resp.GetErrorMessage())
	require.Equal(t, liveBattle, resp.GetBattleId())
	for _, add := range fake.adds {
		require.Equal(t, liveBattle, add.GetBattleId(), "没有记录的战斗不该被打到 battle 节点")
	}
	require.Equal(t, strconv.FormatUint(liveBattle, 10), watchingMark(t, mr, watcher))
}

// battle 用「非房间不存在」的 tip 拒绝(观众席位满 / 墓碑表满 / 观众是参战者):索引不许剔除(战斗还在打),
// 标记要回滚,且随机模式下不换场重试 —— 这三条正是 spectate.go 顶部注释声明的契约。
// 席位满回专用的「观战人数已满」,其余拒绝回「该战斗当前无法观战」(映射见 spectateRejectVerdict)。
func TestWatchBattleNonMissingTipKeepsIndexAndDoesNotRetry(t *testing.T) {
	cases := []struct {
		name        string
		tip         uint32
		wantTip     uint32
		wantOutcome string
	}{
		{"观众已满", uint32(table.MatchError_kMatchSpectateFull), constants.ErrSpectateFull, "full"},
		{"墓碑表满", uint32(table.CommonError_kRateLimitExceeded), constants.ErrBattleNotWatchable, "rejected"},
		{"观众是参战者", uint32(table.CommonError_kInvalidParameter), constants.ErrBattleNotWatchable, "rejected"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			fake := stubObserverRPCs(t)
			watcher := uint64(7120 + i)
			battleId := uint64(880180 + i)
			setPlayerSessionOnline(t, mr, watcher)
			registerBattle(t, svcCtx, battleId, nowMs())
			fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
				return &battlepb.AddObserverResponse{ErrorMessage: tipErr(c.tip, c.name)}, nil
			}
			before := metrics.WatchBattleValue(c.wantOutcome)

			resp := watchBattle(t, svcCtx, watcher, 0) // 随机模式
			require.Equal(t, c.wantTip, resp.GetErrorMessage().GetId())
			require.Equal(t, before+1, metrics.WatchBattleValue(c.wantOutcome), "出口必须按结论计指标")
			require.Len(t, fake.adds, 1, "非『房间不存在』不换场重试")
			require.True(t, mr.Exists(spectateBattleKey(battleId)), "战斗仍在打,索引不许剔除")
			require.Equal(t, 1, activeIndexSize(t, svcCtx))
			require.Empty(t, watchingMark(t, mr, watcher), "失败必须回滚标记")
		})
	}
}

// SETNX 抢观战标记失败 → ErrAlreadyWatching。这条出口只在「入口检查之后、标记落地之前」
// 有并发 WatchBattle 时可达,用 beforeAcquireWatchingHook 把那个并发时刻做成确定性用例。
// 三条断言分别钉住:出口存在、抢占失败不许打 battle、不许覆盖或误删别人的标记。
func TestWatchBattleAlreadyWatchingWhenMarkStolenBeforeAcquire(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7130)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880190, nowMs())

	prev := beforeAcquireWatchingHook
	beforeAcquireWatchingHook = func(playerId uint64) {
		// 模拟并发的另一次 WatchBattle 抢先落标记
		require.NoError(t, mr.Set(spectateWatchingKey(playerId), "999"))
	}
	t.Cleanup(func() { beforeAcquireWatchingHook = prev })

	resp := watchBattle(t, svcCtx, watcher, 880190)
	require.Equal(t, constants.ErrAlreadyWatching, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds, "抢占失败绝不能打 battle 节点")
	require.Equal(t, "999", watchingMark(t, mr, watcher), "不得覆盖也不得回滚删掉他人的标记")
}

// 会话存在但状态不是 ONLINE(重连中 / 已下线未清):同样按离线拒绝。
// 只测「会话键缺失」不够 —— 生产里更常见的是键还在、状态已变。
func TestWatchBattleRejectsNonOnlineSessionState(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7131)
	raw, err := proto.Marshal(&plpb.PlayerSession{
		SessionId:      7,
		GateId:         "3",
		GateInstanceId: "inst",
		State:          plpb.PlayerSessionState_SESSION_STATE_OFFLINE,
		Account:        "acc",
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(playerSessionKey(watcher), string(raw)))
	registerBattle(t, svcCtx, 880200, nowMs())

	resp := watchBattle(t, svcCtx, watcher, 880200)
	require.Equal(t, constants.ErrSpectateOffline, resp.GetErrorMessage().GetId())
	require.Empty(t, fake.adds)
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 权威身份:session 里的 player_id 压过请求体伪造的 player_id
// (客户端直达协议里请求体可被篡改;观众登记挂的是会话路由,认错人就会以别人的身份签票挂观众)。
func TestWatchBattleUsesSessionPlayerIdOverRequestBody(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const realPlayer = uint64(7140)
	const forgedPlayer = uint64(7141)
	setPlayerSessionOnline(t, mr, realPlayer)
	registerBattle(t, svcCtx, 880210, nowMs())

	ctx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: realPlayer})
	resp, err := NewWatchBattleLogic(ctx, svcCtx).WatchBattle(&matchpb.WatchBattleRequest{
		PlayerId: forgedPlayer,
		BattleId: 880210,
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())

	require.Len(t, fake.adds, 1)
	require.Equal(t, realPlayer, fake.adds[0].GetObserverPlayerId(), "必须以 session 身份挂观众")
	require.Equal(t, "880210", watchingMark(t, mr, realPlayer))
	require.Empty(t, watchingMark(t, mr, forgedPlayer), "伪造的 player_id 不得留下任何痕迹")
}

// 战斗锁在「标记落地之后」才出现(并发 gather 拿到锁):双检的另一半,同样自我清退。
// 与 TestWatchBattleDoubleCheckSelfEvictsOnConcurrentQueue 成对(那条走 ticket,这条走 battle:lock)。
func TestWatchBattleDoubleCheckSelfEvictsOnConcurrentBattleLock(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7150)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, 880220, nowMs())
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		require.NoError(t, mr.Set(battleLockKey(watcher), "880999")) // 并发 gather 拿到战斗锁
		return &battlepb.AddObserverResponse{}, nil
	}

	resp := watchBattle(t, svcCtx, watcher, 880220)
	require.Equal(t, constants.ErrSpectateWhileQueued, resp.GetErrorMessage().GetId())
	require.Len(t, fake.removes, 1)
	require.Equal(t, "concurrent_queue", fake.removes[0].GetReason())
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 列表:score 过期、记录只剩空壳(Summary 为 nil)两类脏数据都要现场剔除且不出现在结果里。
func TestListWatchableBattlesEvictsStaleAndSummarylessRecords(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	base := nowMs()
	registerBattle(t, svcCtx, 880230, base, "好场")

	// 过期 score(记录还在,但已过 TTL 窗口)
	registerBattle(t, svcCtx, 880231, staleCreatedAtMs(svcCtx))
	// 空壳记录:有 SpectateBattleRecord 但 Summary 为 nil
	shell, err := proto.Marshal(&matchpb.SpectateBattleRecord{BattleNodeId: 7})
	require.NoError(t, err)
	require.NoError(t, mr.Set(spectateBattleKey(880232), string(shell)))
	_, err = svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(base), "880232")
	require.NoError(t, err)

	resp, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetBattles(), 1)
	require.Equal(t, uint64(880230), resp.GetBattles()[0].GetBattleId())

	require.Equal(t, 1, activeIndexSize(t, svcCtx), "过期与空壳成员都要被剔除")
	require.False(t, mr.Exists(spectateBattleKey(880231)))
	require.False(t, mr.Exists(spectateBattleKey(880232)))
}

// TTL 公式:观战索引 TTL = BattleMaxDurationSeconds + 60s;配置缺省(<=0)时退回 300+60。
// 分成两个不同的配置值来断言,否则「读到了配置」和「走了兜底默认值」在 300 这个点上分不开。
func TestSpectateTTLFollowsConfigAndFallsBack(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	require.Equal(t, 300+60, spectateTTLSeconds(svcCtx))

	svcCtx.Config.BattleMaxDurationSeconds = 900
	require.Equal(t, 900+60, spectateTTLSeconds(svcCtx), "必须跟随配置,而不是写死 360")

	svcCtx.Config.BattleMaxDurationSeconds = 0
	require.Equal(t, 300+60, spectateTTLSeconds(svcCtx), "配置缺省时退回 300s 基线")
}

// ---- 剩余缺口:清退的错误分支 / 选场放弃 / Redis 故障 / gather 回滚 ----

// 记录读坏(pb 损坏)时的清退:不能拿脏数据去摘观众,但标记必须清掉 ——
// 不清就把玩家卡满整个 TTL 窗口(既不能观战也不能被 gather 正确清退),这是 D11 互斥最弱的一环。
func TestStopWatchingIfAnyWithCorruptRecordStillClearsMark(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	const watcher = uint64(7310)
	setWatchingMark(t, mr, watcher, 880300)
	require.NoError(t, mr.Set(spectateBattleKey(880300), "\x08")) // 截断的 varint,必定反序列化失败

	stopWatchingIfAny(svcCtx, watcher, "unit")

	require.Empty(t, fake.removes, "记录读不出来就不该拿脏数据去 battle 摘观众")
	require.Empty(t, watchingMark(t, mr, watcher), "标记必须清掉,否则玩家被卡满 TTL 窗口")
}

// 随机选场最多尝试 3 次:全是过期成员时放弃并返回 0(不是死循环、也不是一次性清空整个索引)。
// 5 个过期成员 → 剔掉 3 个后放弃,剩 2 个由 matcher 每轮的 cleanupExpiredSpectateIndex 兜底。
func TestPickRandomBattleGivesUpAfterThreeAttempts(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	stale := staleCreatedAtMs(svcCtx)
	for i := 0; i < 5; i++ {
		registerBattle(t, svcCtx, uint64(880310+i), stale)
	}

	picked, err := pickRandomBattle(svcCtx)
	require.NoError(t, err)
	require.Zero(t, picked)
	require.Equal(t, 2, activeIndexSize(t, svcCtx), "每次尝试剔一个,尝试 3 次剔 3 个")
}

// Redis 掉线时列表必须回错误,不能吞成空列表 —— 空列表在客户端等同于「当前没有可观战的战斗」,
// 把基础设施故障伪装成正常业务结果。
func TestListWatchableBattlesPropagatesRedisError(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	registerBattle(t, svcCtx, 880320, nowMs())
	mr.Close()

	_, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.Error(t, err, "读索引失败必须回错误,不能返回空列表冒充『没有可观战的战斗』")
}

// gather 开局失败并回滚:① 不得登记新的可观战战斗;② 入口已清退的观众不因回滚而恢复
// (入口 RemoveObserver 已让 battle 推 SpectateEnd 并关掉观战直连,客户端已收起观战 UI;
// 回滚时再挂回去只会在名单里留一个没有直连的观众)。
func TestGatherFailureLeavesNoSpectateIndexAndKeepsObserverEvicted(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	rpcs := stubGatherRPCs(t, map[uint64]string{7210: "fp", 7211: "fp"})
	observers := stubObserverRPCs(t)
	registerBattle(t, svcCtx, 880330, nowMs(), "旧甲", "旧乙")
	setWatchingMark(t, mr, 7210, 880330)

	prevCreate := createBattleFn
	createBattleFn = func(string, *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		return nil, errors.New("battle 节点开局失败")
	}
	t.Cleanup(func() { createBattleFn = prevCreate })

	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 7210, 7211)
	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Empty(t, rpcs.created, "CreateBattle 被顶替,不应有成功记录")
	require.Len(t, rpcs.cancelled, 2, "回滚必须解冻两名参与者")
	require.Equal(t, 1, activeIndexSize(t, svcCtx), "开局失败不得新登记可观战战斗(索引里只剩旧场)")
	require.False(t, mr.Exists(spectateBattleKey(members[0])))
	require.Empty(t, watchingMark(t, mr, 7210), "已清退的观众不因回滚而恢复")
	require.Len(t, observers.removes, 1)
	require.Equal(t, "enter_gather", observers.removes[0].GetReason())
}

// ---- turn-based §22 D70:battle 签不出票(kServiceUnavailable)时的观战回滚 ----

// battle 侧 AddObserver 预签票失败回 kServiceUnavailable:新观众不登记(同场重看已先按旧原件摘掉旧 op,
// 新 op 也走新观众分支)。match 必须:回「该战斗当前无法观战」、回滚观战标记、
// 不剔除索引(战斗仍在打)、随机模式不换场、不为被拒的新 op 补发 RemoveObserver(battle 侧已不留该观众);
// 标记回滚彻底,battle 恢复后同一玩家能正常再接入。
func TestWatchBattleServiceUnavailableRollsBackMark(t *testing.T) {
	unavailable := func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		return &battlepb.AddObserverResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kServiceUnavailable), "战斗服务暂不可用"),
		}, nil
	}

	t.Run("新观众", func(t *testing.T) {
		svcCtx, mr := newGatherSvcCtx(t)
		fake := stubObserverRPCs(t)
		const watcher = uint64(7160)
		const battleId = uint64(880240)
		setPlayerSessionOnline(t, mr, watcher)
		registerBattle(t, svcCtx, battleId, nowMs())
		fake.onAdd = unavailable

		resp := watchBattle(t, svcCtx, watcher, 0) // 随机模式:唯一一场被拒也不换场重试
		require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
		require.Zero(t, resp.GetBattleId())
		require.Len(t, fake.adds, 1, "签不出票不是房间不存在,不换场重试")
		require.Empty(t, fake.removes, "battle 侧未登记该观众,match 不补发 RemoveObserver")
		require.Empty(t, watchingMark(t, mr, watcher), "失败必须回滚观战标记")
		require.True(t, mr.Exists(spectateBattleKey(battleId)), "战斗仍在打,索引不许剔除")
		require.Equal(t, 1, activeIndexSize(t, svcCtx))

		// 回滚彻底:battle 恢复后同一玩家再看同一场能正常接入,不被残留标记卡住。
		fake.onAdd = nil
		retry := watchBattle(t, svcCtx, watcher, battleId)
		require.Zero(t, retry.GetErrorMessage().GetId(), "tip=%v", retry.GetErrorMessage())
		require.Equal(t, battleId, retry.GetBattleId())
		require.Len(t, fake.adds, 2)
		require.Equal(t, strconv.FormatUint(battleId, 10), watchingMark(t, mr, watcher))
	})

	t.Run("同场重看", func(t *testing.T) {
		svcCtx, mr := newGatherSvcCtx(t)
		fake := stubObserverRPCs(t)
		const watcher = uint64(7161)
		const battleId = uint64(880241)
		setPlayerSessionOnline(t, mr, watcher)
		registerBattle(t, svcCtx, battleId, nowMs())
		require.Zero(t, watchBattle(t, svcCtx, watcher, battleId).GetErrorMessage().GetId())
		require.Equal(t, strconv.FormatUint(battleId, 10), watchingMark(t, mr, watcher))

		fake.onAdd = unavailable
		resp := watchBattle(t, svcCtx, watcher, battleId)
		require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
		require.Len(t, fake.adds, 2)
		// 唯一一次 Remove 是重看前按旧原件摘旧 op;新 op 被拒后 battle 侧不留该观众,不再补发。
		require.Len(t, fake.removes, 1)
		requireRemoveOf(t, fake.adds[0], fake.removes[0], "rewatch")
		require.Empty(t, watchingMark(t, mr, watcher), "与 battle 不留该观众一致:标记必须回滚")
		require.True(t, mr.Exists(spectateBattleKey(battleId)), "战斗仍在打,索引不许剔除")
		require.Equal(t, 1, activeIndexSize(t, svcCtx))
	})
}

// ---- 观战账本(battle_room_manager.cpp HandleAddObserver / HandleRemoveObserver)----

// observerLedgerBattle 是按 battle 观战账本契约校验的桩 battle 节点(房间一直在):
//   - Add:实例 = 节点 7 的 uuid、op 是规范 UUID、准入截止在 (now, now+30s],否则 kInvalidParameter;
//     同 op 只接受未取消、与原件相同的重放;名单里挂着别的 op 时新 op 被拒,并封闭该新 op;
//   - Remove:实例 / op 合规、截止与原件相同才给取消证明;取消后同 op 迟到的 Add 永久被拒,
//     名单里挂着的正是原 op 才摘人。
type observerLedgerBattle struct {
	mu        sync.Mutex
	originals map[string]*battlepb.AddObserverRequest // op → 原 Add
	admits    map[string]uint64                       // op → 原准入截止(Remove 先到时也登记)
	cancelled map[string]bool
	roster    map[uint64]string // observer → 当前登记的 op
}

func newObserverLedgerBattle(fake *fakeObserverRPCs) *observerLedgerBattle {
	b := &observerLedgerBattle{
		originals: map[string]*battlepb.AddObserverRequest{},
		admits:    map[string]uint64{},
		cancelled: map[string]bool{},
		roster:    map[uint64]string{},
	}
	fake.mu.Lock()
	fake.onAdd, fake.onRemove = b.add, b.remove
	fake.mu.Unlock()
	return b
}

func (b *observerLedgerBattle) add(req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	invalid := &battlepb.AddObserverResponse{
		ErrorMessage: tipErr(uint32(table.CommonError_kInvalidParameter), "观战账本拒绝"),
	}
	now, op := nowMs(), req.GetSpectatorOperationId()
	if req.GetTargetInstanceId() != testBattle7Instance || !validSpectatorOperation(op) ||
		req.GetAdmitBeforeMs() <= now || req.GetAdmitBeforeMs()-now > 30000 {
		return invalid, nil
	}
	if admit, known := b.admits[op]; known {
		if b.cancelled[op] || admit != req.GetAdmitBeforeMs() || !proto.Equal(b.originals[op], req) {
			return invalid, nil
		}
	} else {
		b.admits[op] = req.GetAdmitBeforeMs()
		b.originals[op] = proto.Clone(req).(*battlepb.AddObserverRequest)
	}
	if active, ok := b.roster[req.GetObserverPlayerId()]; ok && active != op {
		b.cancelled[op] = true
		return invalid, nil
	}
	b.roster[req.GetObserverPlayerId()] = op
	return &battlepb.AddObserverResponse{}, nil
}

func (b *observerLedgerBattle) remove(req *battlepb.RemoveObserverRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	noProof := errors.New("battle 未确认取消原观战操作")
	now, op, admit := nowMs(), req.GetSpectatorOperationId(), req.GetAdmitBeforeMs()
	if req.GetTargetInstanceId() != testBattle7Instance || !validSpectatorOperation(op) || admit == 0 ||
		(admit > now && admit-now > 30000) {
		return noProof
	}
	if known, ok := b.admits[op]; ok && known != admit {
		return noProof
	}
	b.admits[op], b.cancelled[op] = admit, true
	if b.roster[req.GetObserverPlayerId()] == op {
		delete(b.roster, req.GetObserverPlayerId())
	}
	return nil
}

// activeOp 返回观众当前登记的 op;不在名单返回空串。
func (b *observerLedgerBattle) activeOp(observer uint64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roster[observer]
}

// 回归(本地模式观战被 battle 一律拒绝):本地路径发出的 Add 带齐观战账本三字段,按账本准入;
// 重看同一场先按旧原件摘旧 op 再以新 op 准入;清退 Remove 带当前原件的 op 才能摘人,
// 并让同 op 迟到的 Add 永久失效。修复前 Add 不带 op / 截止 / 实例,第一步就被账本拒绝。
func TestWatchBattleLocalPathHonorsBattleObserverLedger(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	ledger := newObserverLedgerBattle(fake)
	const watcher = uint64(7170)
	const battleId = uint64(880250)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Zero(t, resp.GetErrorMessage().GetId(), "账本必须准入本地观战 tip=%v", resp.GetErrorMessage())
	require.Len(t, fake.adds, 1)
	first := fake.adds[0]
	require.Equal(t, first.GetSpectatorOperationId(), ledger.activeOp(watcher))
	require.Equal(t, first.GetSpectatorOperationId(), resp.GetSpectatorOperationId())

	// 重看同一场:不先摘旧 op,新 op 会被账本拒绝。
	resp = watchBattle(t, svcCtx, watcher, battleId)
	require.Zero(t, resp.GetErrorMessage().GetId(), "重看同场必须成功 tip=%v", resp.GetErrorMessage())
	require.Len(t, fake.adds, 2)
	second := fake.adds[1]
	require.Len(t, fake.removes, 1)
	requireRemoveOf(t, first, fake.removes[0], "rewatch")
	require.Equal(t, second.GetSpectatorOperationId(), ledger.activeOp(watcher))

	// gather 入口清退:Remove 带当前原件的 op,账本摘人、删标记;同 op 迟到的 Add 永久被拒。
	stopWatchingIfAny(svcCtx, watcher, "enter_gather")
	require.Len(t, fake.removes, 2)
	requireRemoveOf(t, second, fake.removes[1], "enter_gather")
	require.Empty(t, ledger.activeOp(watcher), "同 op 的 Remove 必须把观众摘掉")
	require.Empty(t, watchingMark(t, mr, watcher))
	late, err := ledger.add(second)
	require.NoError(t, err)
	require.Equal(t, uint32(table.CommonError_kInvalidParameter), late.GetErrorMessage().GetId(),
		"取消后的同 op Add 不得再准入")
}

// battle 节点没登记实例 uuid:组不出合规原件,fail-closed —— 回「该战斗当前无法观战」,
// 不写标记、不打 battle、不动索引(战斗仍在打)。
func TestWatchBattleRejectsBattleNodeWithoutInstance(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubObserverRPCs(t)
	svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, Endpoint: "battle-7"})
	const watcher = uint64(7171)
	const battleId = uint64(880251)
	setPlayerSessionOnline(t, mr, watcher)
	registerBattle(t, svcCtx, battleId, nowMs())
	before := metrics.WatchBattleValue("rejected")

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Equal(t, before+1, metrics.WatchBattleValue("rejected"))
	require.Empty(t, fake.adds, "没有实例就组不出合规 Add,不得打 battle")
	require.Empty(t, watchingMark(t, mr, watcher))
	require.True(t, mr.Exists(spectateBattleKey(battleId)), "节点定位失败不剔除索引")
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
}
