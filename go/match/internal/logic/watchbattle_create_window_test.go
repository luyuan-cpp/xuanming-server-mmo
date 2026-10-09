package logic

// WatchBattle 建房窗口保护(集群外入口 2c 第 5 条):gather 在 CreateBattle 之前写观战记录,
// D82 换节点时还会改写记录,记录存在 ≠ 房间存在。指定场先查活跃集合、再读记录;AddObserver
// 回"房间不存在"时:
//   - 读记录前这场还没公开(不在活跃集合)且记录 created_at_ms 在 gatherCreateStageWorst 之内
//     → 房间可能还在建,只回"不存在",记录与活跃集合都不动;
//   - 否则(读记录前已公开 / 出了窗口 / created_at_ms 为 0)→ 照旧懒剔除;
//   - 查活跃集合失败按"未公开"处理(窗口内不剔除);
//   - 记录已不在:不 DEL,只在读记录前已公开时摘活跃集合残留成员。
// 已公开场的懒剔除另见 spectate_test.go TestWatchBattleExplicitMissingRoomEvictsIndex;随机模式只挑
// 活跃集合里的场,不做这项判定(TestWatchBattleRandomModeEvictsFinishedOnlyBattle)。

import (
	"strconv"
	"sync/atomic"
	"testing"

	"match/internal/constants"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
)

// stubRoomMissing 让 AddObserver 一律回 battle 的"房间不存在"(kEntityIsNull);beforeReply 非 nil 时
// 在回包之前执行(模拟 RPC 在途期间发生的事)。
func stubRoomMissing(t *testing.T, beforeReply func()) *fakeObserverRPCs {
	t.Helper()
	fake := stubObserverRPCs(t)
	fake.onAdd = func(*battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
		if beforeReply != nil {
			beforeReply()
		}
		return &battlepb.AddObserverResponse{
			ErrorMessage: tipErr(uint32(table.CommonError_kEntityIsNull), "房间不存在"),
		}, nil
	}
	return fake
}

// preWriteRecord 按 gather 第 4.1 步的样子只写记录、不入活跃集合(node 7 = newGatherSvcCtx 灌的 battle 节点)。
func preWriteRecord(t *testing.T, svcCtx *svc.ServiceContext, battleId, createdAtMs uint64) *matchpb.SpectateBattleRecord {
	t.Helper()
	record := newSpectateRecord(battleId, 7, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 0, []string{"甲"}, createdAtMs)
	require.NoError(t, writeSpectateRecord(svcCtx, record))
	return record
}

func TestWatchBattleMissingRoomCreateWindow(t *testing.T) {
	window := uint64(gatherCreateStageWorst.Milliseconds())
	cases := []struct {
		name      string
		createdAt func(now uint64) uint64
		published bool
		wantKept  bool
	}{
		{"未公开且刚写入:可能在建,不剔除", func(now uint64) uint64 { return now }, false, true},
		{"未公开、接近窗口上沿:仍不剔除", func(now uint64) uint64 { return now - window + 2000 }, false, true},
		{"created_at 晚于本机时钟(实例间偏差):按窗口内处理", func(now uint64) uint64 { return now + 5000 }, false, true},
		{"未公开但出了窗口:gather 必已放弃,懒剔除", func(now uint64) uint64 { return now - window - 1000 }, false, false},
		{"created_at_ms 为 0(旧记录):按窗口外处理,懒剔除", func(uint64) uint64 { return 0 }, false, false},
		{"已公开:房间在发请求前已建成,窗口内也懒剔除", func(now uint64) uint64 { return now }, true, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			fake := stubRoomMissing(t, nil)
			watcher := uint64(7301 + i)
			battleId := uint64(882001 + i)
			setPlayerSessionOnline(t, mr, watcher)
			record := preWriteRecord(t, svcCtx, battleId, tc.createdAt(nowMs()))
			if tc.published {
				addSpectateActive(svcCtx, record)
				require.Equal(t, 1, activeIndexSize(t, svcCtx))
			}

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId(), "两种情况对客户端都是未找到")
			require.Len(t, fake.adds, 1)
			require.Empty(t, watchingMark(t, mr, watcher), "AddObserver 失败必须回滚标记")
			require.Equal(t, tc.wantKept, mr.Exists(spectateBattleKey(battleId)))
			require.Zero(t, activeIndexSize(t, svcCtx), "未公开的场本就不在活跃集合;已公开的被懒剔除")
		})
	}
}

// 读记录前未公开、RPC 在途期间 gather 开局成功并公开(补写记录 + 入活跃集合),回包仍是"房间不存在"
// (请求先于建房到达 battle):判定以读记录前为准,不得剔除。回包后再查活跃集合会看到"已公开"并删掉
// 刚补写的记录 —— 这正是本保护要堵的"补写之后才 DEL"。
func TestWatchBattleMissingRoomPublishedDuringRPCKeepsRecord(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	const watcher, battleId = uint64(7320), uint64(882020)
	setPlayerSessionOnline(t, mr, watcher)
	record := preWriteRecord(t, svcCtx, battleId, nowMs())
	fake := stubRoomMissing(t, func() { publishSpectateBattle(svcCtx, record) })

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1)
	require.True(t, mr.Exists(spectateBattleKey(battleId)), "开局补写的记录必须保留(丢票补签靠它定位节点)")
	require.Equal(t, 1, activeIndexSize(t, svcCtx), "活跃集合成员必须保留")
}

// 查活跃集合失败按"未公开"处理,窗口内即不剔除。误留只是晚些随 TTL 自清(补签拿到的仍是
// "房间不存在"),误删可能让进行中的战斗丢票后回不去。
func TestWatchBattleMissingRoomActiveLookupFailureKeepsRecord(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	const watcher, battleId = uint64(7321), uint64(882021)
	setPlayerSessionOnline(t, mr, watcher)
	preWriteRecord(t, svcCtx, battleId, nowMs())
	fake := stubRoomMissing(t, nil)
	// 钩子只读写自身原子量,不调 mr.*(见 installSpectateWriteFault 的说明);只注入 1 次,远低于熔断阈值。
	var zscores atomic.Int32
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if cmd == "ZSCORE" && len(args) > 0 && args[0] == spectateBattlesActiveKey {
			zscores.Add(1)
			c.WriteError("ERR injected active lookup failure")
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1)
	require.Equal(t, int32(1), zscores.Load(), "指定场在读记录前查一次活跃集合")
	require.True(t, mr.Exists(spectateBattleKey(battleId)))
	require.Empty(t, watchingMark(t, mr, watcher))
}

// D82 换节点:读到的是指向首选节点的预写记录,随后 gather 把记录改写到重试节点、在那里建房并公开;
// AddObserver 仍按旧记录打到首选节点,回"房间不存在"。读记录前未公开 → 不得剔除:记录保留且指向
// 重试节点,活跃集合成员也保留。先读记录、后查活跃集合的写法会在这里看到"已公开",把重试节点上
// 正在进行的战斗的记录与成员一并删掉。
func TestWatchBattleMissingRoomRecordRewrittenToRetryNodeKeepsRecord(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	const watcher, battleId = uint64(7322), uint64(882022)
	const retryNodeId = uint32(8)
	setPlayerSessionOnline(t, mr, watcher)
	createdAtMs := nowMs()
	preWriteRecord(t, svcCtx, battleId, createdAtMs)
	fake := stubRoomMissing(t, nil)

	// 钩子在读记录之后、发 AddObserver 之前执行:模拟 gather 第 4.2 步改写记录、重试节点建房成功、第 6 步公开。
	prev := beforeAcquireWatchingHook
	beforeAcquireWatchingHook = func(uint64) {
		rewritten := newSpectateRecord(battleId, retryNodeId, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, 0,
			[]string{"甲"}, createdAtMs)
		require.NoError(t, writeSpectateRecord(svcCtx, rewritten))
		publishSpectateBattle(svcCtx, rewritten)
	}
	t.Cleanup(func() { beforeAcquireWatchingHook = prev })

	resp := watchBattle(t, svcCtx, watcher, battleId)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Len(t, fake.adds, 1, "按读到的旧记录打到首选节点")
	record, err := loadSpectateBattle(svcCtx, battleId)
	require.NoError(t, err)
	require.NotNil(t, record, "重试节点上进行中战斗的记录必须保留(丢票补签靠它定位节点)")
	require.Equal(t, retryNodeId, record.GetBattleNodeId())
	require.Equal(t, 1, activeIndexSize(t, svcCtx), "活跃集合成员必须保留")
	require.Empty(t, watchingMark(t, mr, watcher))
}

// 记录已不在:不打 battle、不 DEL 记录键 —— GET 已读到空,此刻的 DEL 只可能删掉 GET 之后才预写的
// 记录。读记录前已公开(活跃集合残留成员)才 ZREM;未公开什么都不写,免得误摘随后才公开的成员。
// 用 prehook 数命令(只读写自身原子量,不调 mr.*,不改变命令结果)。
func TestWatchBattleExplicitMissingRecordNeverDeletesRecordKey(t *testing.T) {
	cases := []struct {
		name      string
		published bool
		wantZrem  int32
	}{
		{"未公开:什么都不写", false, 0},
		{"已公开(残留成员):只摘成员", true, 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newGatherSvcCtx(t)
			fake := stubObserverRPCs(t)
			watcher := uint64(7330 + i)
			battleId := uint64(882030 + i)
			member := strconv.FormatUint(battleId, 10)
			setPlayerSessionOnline(t, mr, watcher)
			if tc.published {
				_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), member)
				require.NoError(t, err)
			}
			recordKey := spectateBattleKey(battleId)
			var dels, zrems atomic.Int32
			mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
				switch {
				case cmd == "DEL" && len(args) > 0 && args[0] == recordKey:
					dels.Add(1)
				case cmd == "ZREM" && len(args) > 1 && args[0] == spectateBattlesActiveKey && args[1] == member:
					zrems.Add(1)
				}
				return false
			})
			t.Cleanup(func() { mr.Server().SetPreHook(nil) })

			resp := watchBattle(t, svcCtx, watcher, battleId)
			require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
			require.Empty(t, fake.adds, "记录都没有就不该打 battle 节点")
			require.Zero(t, dels.Load(), "记录已不在时不得 DEL 记录键")
			require.Equal(t, tc.wantZrem, zrems.Load())
			require.Zero(t, activeIndexSize(t, svcCtx))
			require.Empty(t, watchingMark(t, mr, watcher))
		})
	}
}
