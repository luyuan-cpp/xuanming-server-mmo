package logic

// 观战记录 spectate:battle:{id} 写入 fail-closed(集群外入口 2b 第 9 条 + 复审):
//   - 记录在 CreateBattle **之前**写(writeSpectateRecord,有界重试,次数 / 退避是常量);
//   - 仍写不进去 → 不建房、不发 DestroyBattle,按干净失败补偿(解冻必然有效、回队首或删票),
//     outcome=index_failed;D82 换节点前改写记录失败同理;
//   - 建房失败路径在补偿之后删掉预写记录;DestroyBattle 也失败(房间可能活着)时保留记录;
//   - 开局成功后在推进 ready 之后补写记录 + 入活跃集合(publishSpectateBattle,best-effort),
//     补回建房窗口里被按 battle_id 观战懒剔除的记录。
// Redis 故障用 miniredis 前置钩子按命令 + key 精确注入;退避用 spectateRecordRetrySleepFn 记录器
// 顶替,不真睡。scene 的冻结用 sceneFreezeModel 按真实状态机模拟(FIGHTING 下拒绝取消),
// 断言补偿后到底还有没有人被冻着,而不是只数 CancelBattlePrepare 调用。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
	scenepb "proto/scene"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
)

// registerSpectateBattle 是单测夹具:一次性登记一场"已确定存在"的战斗(写记录 + 入活跃集合),
// 与 gather 开局成功后的最终状态一致。生产路径不用它 —— gather 把两步拆在建房前后
// (writeSpectateRecord / publishSpectateBattle)。spectate_test.go、requestbattleticket_test.go 也用它。
func registerSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64, battleNodeId uint32,
	mode matchpb.MatchMode, battleConfigId uint32, playerNames []string, createdAtMs uint64,
) error {
	record := newSpectateRecord(battleId, battleNodeId, mode, battleConfigId, playerNames, createdAtMs)
	if err := writeSpectateRecord(svcCtx, record); err != nil {
		return err
	}
	addSpectateActive(svcCtx, record)
	return nil
}

// spectateWriteFault 观战索引写入的故障钩子状态。钩子可能在 Lua 的 redis.call 里带锁重入,
// 所以只读写自身的原子量与只读谓词,绝不调 mr.*;单个 svcCtx 注入的失败控制在 5 次以内(go-zero
// Redis 客户端自带 Google SRE 熔断,protection=5)。
type spectateWriteFault struct {
	// failRecordWrite 按 spectate:battle:* 写入的序号(从 1 起)决定是否注入失败;nil = 都不注入。
	failRecordWrite func(n int32) bool
	// recordWrites 观察到的 spectate:battle:* 写入次数(含注入失败的)。
	recordWrites atomic.Int32
	// failZadd 让 spectate:battles:active 的 ZADD 失败。
	failZadd atomic.Bool
}

// failRecordWritesUpTo 前 n 次记录写入失败,之后成功。
func failRecordWritesUpTo(n int32) func(int32) bool {
	return func(i int32) bool { return i <= n }
}

// failRecordWritesFrom 第 n 次起的记录写入全部失败(之前的成功)。
func failRecordWritesFrom(n int32) func(int32) bool {
	return func(i int32) bool { return i >= n }
}

// failAllRecordWrites 记录写入一律失败。
func failAllRecordWrites(int32) bool { return true }

// installSpectateWriteFault 给 miniredis 装故障钩子(与 SetError 共用槽位,装了就别再用 SetError)。
func installSpectateWriteFault(t *testing.T, mr *miniredis.Miniredis, failRecordWrite func(int32) bool) *spectateWriteFault {
	t.Helper()
	f := &spectateWriteFault{failRecordWrite: failRecordWrite}
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if len(args) == 0 {
			return false
		}
		key := args[0]
		switch {
		case (cmd == "SET" || cmd == "SETEX") && strings.HasPrefix(key, "spectate:battle:"):
			n := f.recordWrites.Add(1)
			if f.failRecordWrite == nil || !f.failRecordWrite(n) {
				return false
			}
			c.WriteError("ERR injected spectate record write failure")
			return true
		case cmd == "ZADD" && key == spectateBattlesActiveKey && f.failZadd.Load():
			c.WriteError("ERR injected spectate active zadd failure")
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return f
}

// 冻结状态(sceneFreezeModel.state 的取值)。
const (
	freezePreparing = "preparing"
	freezeFighting  = "fighting"
)

// sceneFreezeModel 按 scene 的真实状态机模拟冻结:PrepareBattle 成功 → PREPARING;CreateBattle 成功 →
// 名单里的 PREPARING 升 FIGHTING(battle 在回包之前就发确认事件并周期补发,确认先于随后的取消到达是
// 常态);CancelBattlePrepare 只解冻 PREPARING,FIGHTING 下拒绝 —— scene 照常回 OK 但不解冻
// (player_battle.cpp metric=battle_cancel_rejected_fighting)。
type sceneFreezeModel struct {
	mu    sync.Mutex
	state map[uint64]string
}

// stubSceneFreezeModel 必须在 stubGatherRPCs(及其上的 createBattleFn 覆盖)之后调用:包住当前的
// prepare / cancel / create 桩,记录照旧由内层桩完成;Cleanup 按 LIFO 还原成内层桩。
func stubSceneFreezeModel(t *testing.T) *sceneFreezeModel {
	t.Helper()
	m := &sceneFreezeModel{state: map[uint64]string{}}
	prevPrepare, prevCancel, prevCreate := prepareBattleFn, cancelBattlePrepareFn, createBattleFn
	prepareBattleFn = func(endpoint string, req *scenepb.PrepareBattleRequest) (*scenepb.PrepareBattleResponse, error) {
		resp, err := prevPrepare(endpoint, req)
		if err == nil && resp.GetErrorMessage().GetId() == 0 {
			m.mu.Lock()
			m.state[req.GetPlayerId()] = freezePreparing
			m.mu.Unlock()
		}
		return resp, err
	}
	createBattleFn = func(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		resp, err := prevCreate(endpoint, req)
		if err == nil && resp.GetErrorMessage().GetId() == 0 {
			m.mu.Lock()
			for _, p := range req.GetPlayers() {
				if m.state[p.GetPlayerId()] == freezePreparing {
					m.state[p.GetPlayerId()] = freezeFighting
				}
			}
			m.mu.Unlock()
		}
		return resp, err
	}
	cancelBattlePrepareFn = func(endpoint string, req *scenepb.CancelBattlePrepareRequest) error {
		err := prevCancel(endpoint, req)
		if err == nil {
			m.mu.Lock()
			if m.state[req.GetPlayerId()] == freezePreparing {
				delete(m.state, req.GetPlayerId())
			}
			m.mu.Unlock()
		}
		return err
	}
	t.Cleanup(func() {
		prepareBattleFn, cancelBattlePrepareFn, createBattleFn = prevPrepare, prevCancel, prevCreate
	})
	return m
}

// frozen 返回仍被冻结(PREPARING 或 FIGHTING)的玩家。
func (m *sceneFreezeModel) frozen() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uint64, 0, len(m.state))
	for pid := range m.state {
		out = append(out, pid)
	}
	return out
}

// stubSpectateRetrySleep 把退避等待换成记录器,返回读取已记录等待时长的函数。
func stubSpectateRetrySleep(t *testing.T) func() []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var sleeps []time.Duration
	prev := spectateRecordRetrySleepFn
	spectateRecordRetrySleepFn = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
	}
	t.Cleanup(func() { spectateRecordRetrySleepFn = prev })
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), sleeps...)
	}
}

// ---- 观战记录写入单元 ----

// 瞬时失败被有界重试吸收:第一次写失败、第二次成功 → 返回 nil,记录可读;退避一次 100ms。
// writeSpectateRecord 只写记录,不入活跃集合(那是开局成功之后 publishSpectateBattle 的事)。
func TestWriteSpectateRecordRetriesTransientFailure(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	faults := installSpectateWriteFault(t, mr, failRecordWritesUpTo(1))
	sleeps := stubSpectateRetrySleep(t)
	const battleId = uint64(881001)

	err := writeSpectateRecord(svcCtx, newSpectateRecord(battleId, 7, matchpb.MatchMode_MATCH_MODE_1V1, 0, []string{"甲"}, nowMs()))

	require.NoError(t, err)
	require.Equal(t, int32(2), faults.recordWrites.Load())
	require.Equal(t, []time.Duration{spectateRecordWriteBackoff}, sleeps())
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Zero(t, activeIndexSize(t, svcCtx), "建房之前不得进观战列表")
}

// 持续失败:恰好尝试 spectateRecordWriteAttempts 次(最后一次失败后不再等),返回错误,不留记录。
func TestWriteSpectateRecordGivesUpAfterBoundedAttempts(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	faults := installSpectateWriteFault(t, mr, failAllRecordWrites)
	sleeps := stubSpectateRetrySleep(t)
	const battleId = uint64(881002)

	err := writeSpectateRecord(svcCtx, newSpectateRecord(battleId, 7, matchpb.MatchMode_MATCH_MODE_1V1, 0, []string{"甲"}, nowMs()))

	require.Error(t, err)
	require.Equal(t, int32(spectateRecordWriteAttempts), faults.recordWrites.Load(), "重试次数有上限")
	require.Len(t, sleeps(), spectateRecordWriteAttempts-1, "最后一次失败后直接返回,不再退避")
	require.False(t, mr.Exists(spectateBattleKey(battleId)))
}

// 预算回归:常规路径(不触发 D82 换节点)上,建房前写观战记录新增的最坏耗时必须落在 matched 票据
// TTL 里 —— 从弹组到推进 ready / 进入补偿(补偿前会先续期)的最坏链路:每人观战清退 + PrepareBattle,
// 写记录,CreateBattle,失败时 DestroyBattle(删记录、补写、ZADD 都在关键路径之外)。
// 单条 Redis 命令按 spectateRedisOpWorst(go-redis ReadTimeout)计,不按 ctx 截止计。
func TestSpectateRecordWriteWorstFitsMatchedTicketTTL(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.Config.MatchedTicketTTLSeconds = 1 // 配置下限压到最低,只看公式
	for _, required := range []uint32{1, 2, kMaxBattleTeamSize, required5v5Players} {
		budget := time.Duration(matchedTicketTTLFor(svcCtx, required)) * time.Second
		worst := time.Duration(required)*(removeObserverTimeout+prepareBattleTimeout) +
			spectateRecordWriteWorst + createBattleTimeout + rollbackTimeout
		require.Less(t, worst, budget, "required=%d", required)
	}
}

// 活跃集合 ZADD 失败仍 best-effort:返回 nil(补签与指定 battle_id 观战只读记录),记录可读。
func TestRegisterSpectateBattleZaddFailureIsBestEffort(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	faults := installSpectateWriteFault(t, mr, nil)
	faults.failZadd.Store(true)
	stubSpectateRetrySleep(t)
	const battleId = uint64(881003)

	err := registerSpectateBattle(svcCtx, battleId, 7, matchpb.MatchMode_MATCH_MODE_1V1, 0, []string{"甲"}, nowMs())

	require.NoError(t, err, "ZADD 只服务观战列表,不能让它拖垮开局")
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.NotNil(t, record, "补签定位靠的记录必须在")
	require.Zero(t, activeIndexSize(t, svcCtx))
}

// ---- gather 集成 ----

// 队列凑单:记录持续写不进去 → 不建房、不发 DestroyBattle(干净失败)→ 两人真的解冻
// (scene 状态机模型:没有确认事件,取消必然生效)、原票回队首、不推进 ready,不留记录,记 index_failed。
func TestGatherSpectateIndexFailureFailsCleanBeforeCreate(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9801: "fp", 9802: "fp"})
	scene := stubSceneFreezeModel(t)
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9801, 9802)
	faults := installSpectateWriteFault(t, mr, failAllRecordWrites)
	sleeps := stubSpectateRetrySleep(t)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	indexFailedBase := metrics.GatherValue(mode, "index_failed")
	successBase := metrics.GatherValue(mode, "success")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Empty(t, fake.created, "记录写不进去就不建房")
	require.Empty(t, fake.destroyed, "没建房就没有 DestroyBattle")
	require.Len(t, fake.prepares, 2)
	battleId := fake.prepares[0].GetBattleId()
	require.ElementsMatch(t, members, fake.cancelled, "已冻结的两人都要解冻")
	require.Empty(t, scene.frozen(), "没有确认事件,取消必然生效:补偿后不得有人仍被冻结")
	require.Equal(t, int32(spectateRecordWriteAttempts), faults.recordWrites.Load())
	require.Len(t, sleeps(), spectateRecordWriteAttempts-1)
	require.False(t, mr.Exists(spectateBattleKey(battleId)), "不得留下指向未建成战斗的记录")
	require.Zero(t, activeIndexSize(t, svcCtx))

	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket, "Redis 故障不是玩家的错,不得删票 player=%d", pid)
		require.Equal(t, tickets[pid], ticket.Ticket, "回队首的是原票据 player=%d", pid)
		require.Equal(t, ticketStateQueued, ticket.State, "失败时票据不得停在 ready player=%d", pid)
	}
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"9801", "9802"}, list, "两人按弹出序回队首")
	require.Equal(t, indexFailedBase+1, metrics.GatherValue(mode, "index_failed"))
	require.Equal(t, successBase, metrics.GatherValue(mode, "success"), "失败的开局不得记成 success")
}

// 整队开战:记录写不进去 → 返回 false,不建房、不发 DestroyBattle,全员真的解冻 + 全员删票,不回队列。
func TestRunTeamGatherSpectateIndexFailureDeletesAllTickets(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, nil)
	scene := stubSceneFreezeModel(t)
	roster := []uint64{9811, 9812, 9813}
	for _, pid := range roster {
		setPlayerLocation(t, mr, pid, 1)
	}
	tickets, failed := NewTeamBattleStarter(svcCtx).CreateMatchedTickets(context.Background(), 1, testTeamId, roster, nil)
	require.Zero(t, failed)
	installSpectateWriteFault(t, mr, failAllRecordWrites)
	stubSpectateRetrySleep(t)
	mode := matchpb.MatchMode_MATCH_MODE_PVE_TEAM.String()
	indexFailedBase := metrics.GatherValue(mode, "index_failed")

	require.False(t, RunTeamGather(svcCtx, 1, roster, tickets), "记录写不进去 = 开局失败")

	require.Empty(t, fake.created)
	require.Empty(t, fake.destroyed)
	require.ElementsMatch(t, roster, fake.cancelled)
	require.Empty(t, scene.frozen(), "全员真的解冻")
	for _, pid := range roster {
		require.False(t, mr.Exists(matchTicketKey(pid)), "整队出局:全员删票 player=%d", pid)
	}
	require.False(t, mr.Exists(matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), 1)), "不回队列")
	require.Equal(t, indexFailedBase+1, metrics.GatherValue(mode, "index_failed"))
}

// D82 换节点前改写记录失败:首选节点的准入拒绝无副作用,改写又写不进去 → 不再建房、不发 DestroyBattle,
// 干净失败(记 index_failed,不记 not_allocatable),两人真的解冻、原票回队首,不留记录。
func TestGatherNotAllocatableRecordRewriteFailureFailsClean(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	addBattleNode(svcCtx, 8)
	fake := stubGatherRPCs(t, map[uint64]string{9841: "fp", 9842: "fp"})
	battles := stubBattleCreateDestroy(t, failFirstN(1, errNotAllocatable))
	scene := stubSceneFreezeModel(t)
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9841, 9842)
	faults := installSpectateWriteFault(t, mr, failRecordWritesFrom(2)) // 建房前的首写成功,改写全失败
	stubSpectateRetrySleep(t)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	indexFailedBase := metrics.GatherValue(mode, "index_failed")
	notAllocBase := metrics.GatherValue(mode, "not_allocatable")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	creates := battles.createCalls()
	require.Len(t, creates, 1, "记录没改指到重试节点就不得向它建房")
	require.Empty(t, battles.destroyCalls())
	require.Equal(t, int32(1+spectateRecordWriteAttempts), faults.recordWrites.Load())
	require.ElementsMatch(t, members, fake.cancelled)
	require.Empty(t, scene.frozen())
	require.False(t, mr.Exists(spectateBattleKey(creates[0].battleId)), "补偿之后删掉预写记录")
	requireRequeuedInOrder(t, svcCtx, func() []string {
		list, err := mr.List(queueKey)
		require.NoError(t, err)
		return list
	}, members, tickets)
	require.Equal(t, indexFailedBase+1, metrics.GatherValue(mode, "index_failed"))
	require.Equal(t, notAllocBase, metrics.GatherValue(mode, "not_allocatable"))
}

// CreateBattle 通用失败且 DestroyBattle 也失败:房间可能仍活着,走保守分支 —— 不解冻、不回队,
// 票据留 matched 由 TTL 自愈,记 create_failed_room_alive;预写的观战记录保留(房间若活着,
// 丢票补签靠它回到本局),但不进观战列表。
func TestGatherCreateFailureWithDestroyFailureKeepsRecord(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9821: "fp", 9822: "fp"})
	prevCreate, prevDestroy := createBattleFn, destroyBattleFn
	var createdIds []uint64
	createBattleFn = func(_ string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		createdIds = append(createdIds, req.GetBattleId())
		return nil, errors.New("context deadline exceeded")
	}
	var destroyCalls atomic.Int32
	destroyBattleFn = func(string, *battlepb.DestroyBattleRequest) error {
		destroyCalls.Add(1)
		return errors.New("battle 节点不可达")
	}
	t.Cleanup(func() { createBattleFn, destroyBattleFn = prevCreate, prevDestroy })
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9821, 9822)
	// 与生产同序:matcher 弹组后先把票据推进 matched 再跑 gather(matchQueueOnce);popMatchedPair 只弹组,
	// 票据还是 queued。保守分支不碰票据,不先推进的话下面"票据留 matched"的断言永远不成立
	// (这条用例自 09-29 入库起一直是红的,2026-10-10 首次实跑时发现)。
	for _, pid := range members {
		written, err := setTicketMatched(svcCtx, pid, tickets[pid], matchedTicketTTLFor(svcCtx, 2))
		require.NoError(t, err)
		require.True(t, written)
	}
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	roomAliveBase := metrics.GatherValue(mode, "create_failed_room_alive")
	createFailedBase := metrics.GatherValue(mode, "create_failed")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, createdIds, 1)
	require.Equal(t, int32(1), destroyCalls.Load())
	require.Empty(t, fake.cancelled, "房间可能仍活着,不得解冻")
	record, err := loadSpectateBattle(svcCtx, createdIds[0])
	require.NoError(t, err)
	require.NotNil(t, record, "房间可能仍活着,补签定位靠的记录不得删")
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Zero(t, activeIndexSize(t, svcCtx), "没确认开局就不进观战列表")
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateMatched, ticket.State, "票据留 matched 由 TTL 自愈 player=%d", pid)
	}
	require.False(t, mr.Exists(queueKey), "保守分支不回队")
	require.Equal(t, roomAliveBase+1, metrics.GatherValue(mode, "create_failed_room_alive"))
	require.Equal(t, createFailedBase, metrics.GatherValue(mode, "create_failed"))
}

// 只失败一次:重试吸收,照常开局 —— 不销毁、不解冻、票据 ready;开局后补写一次记录并入活跃集合
// (共 3 次写:失败 1 + 成功 1 + 补写 1)。模型核对:建房成功后两人确实是 FIGHTING。
func TestGatherSpectateIndexTransientFailureStillStartsBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9831: "fp", 9832: "fp"})
	scene := stubSceneFreezeModel(t)
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9831, 9832)
	faults := installSpectateWriteFault(t, mr, failRecordWritesUpTo(1))
	sleeps := stubSpectateRetrySleep(t)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1)
	require.Empty(t, fake.destroyed)
	require.Empty(t, fake.cancelled)
	require.ElementsMatch(t, members, scene.frozen(), "开局成功:两人按确认事件升 FIGHTING")
	require.Equal(t, int32(3), faults.recordWrites.Load())
	require.Equal(t, []time.Duration{spectateRecordWriteBackoff}, sleeps())
	record, err := loadSpectateBattle(svcCtx, fake.created[0].GetBattleId())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateReady, ticket.State)
	}
}

// 建房窗口里记录被懒剔除(活动开局的 battle_id 在 gather 之前已回给 guild,有人此刻按 battle_id 观战,
// battle 回房间不存在,WatchBattle 删记录):开局成功后的补写把记录找回来,并照常入活跃集合。
func TestGatherPublishRestoresRecordEvictedDuringCreate(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9851: "fp", 9852: "fp"})
	prevCreate := createBattleFn
	createBattleFn = func(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		require.True(t, mr.Exists(spectateBattleKey(req.GetBattleId())), "记录必须先于建房写入")
		removeSpectateBattle(svcCtx, req.GetBattleId()) // 模拟窗口内的懒剔除
		return prevCreate(endpoint, req)
	}
	t.Cleanup(func() { createBattleFn = prevCreate })
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9851, 9852)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1)
	record, err := loadSpectateBattle(svcCtx, fake.created[0].GetBattleId())
	require.NoError(t, err)
	require.NotNil(t, record, "补签定位靠的记录必须被补回")
	require.Equal(t, uint32(7), record.GetBattleNodeId())
	require.Equal(t, 1, activeIndexSize(t, svcCtx))
}

// 开局成功后入活跃集合失败:仍是成功开局(success、票据 ready),记录在,只是不进观战列表。
func TestGatherSucceedsWhenActiveZaddFails(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9861: "fp", 9862: "fp"})
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9861, 9862)
	faults := installSpectateWriteFault(t, mr, nil)
	faults.failZadd.Store(true)
	mode := matchpb.MatchMode_MATCH_MODE_1V1.String()
	successBase := metrics.GatherValue(mode, "success")

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1)
	require.Empty(t, fake.destroyed)
	require.Empty(t, fake.cancelled)
	record, err := loadSpectateBattle(svcCtx, fake.created[0].GetBattleId())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Zero(t, activeIndexSize(t, svcCtx))
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateReady, ticket.State)
	}
	require.Equal(t, successBase+1, metrics.GatherValue(mode, "success"))
}
