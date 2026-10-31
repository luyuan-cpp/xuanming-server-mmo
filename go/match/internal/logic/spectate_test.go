package logic

// 观战展示索引(设计文档 §10、决策 D9)的 miniredis 测试:活跃集合的登记 / 剔除、随机选场懒剔除、
// 列表最新在前与脏数据清理、可见性闸(§10 第 6 条)、取消证明判据。
// 成局原件在 SQL,这里用 fakeSpectateFormations 顶替 loadSpectateBattleFn / spectateTargetFn(都按原件组记录,与生产同形);
// WatchBattle 的意图、原件与重放见 global_spectate_lobby_native_test.go(需要 TiDB)。

import (
	"context"
	"strconv"
	"testing"

	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fakeSpectateFormations 顶替 SQL 成局:battle_id → 已开房的成局原件。一个用例一份,按 t 复用(测试之间不并行,包级测试缝)。
type fakeSpectateFormations struct {
	owner      *testing.T
	formations map[uint64]*matchpb.MatchFormationRecord
}

var currentSpectateFormations *fakeSpectateFormations

func stubSpectateFormations(t *testing.T) *fakeSpectateFormations {
	t.Helper()
	if current := currentSpectateFormations; current != nil && current.owner == t {
		return current
	}
	f := &fakeSpectateFormations{owner: t, formations: map[uint64]*matchpb.MatchFormationRecord{}}
	prevLoad, prevTarget, prevCurrent := loadSpectateBattleFn, spectateTargetFn, currentSpectateFormations
	loadSpectateBattleFn = func(_ *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
		formation := f.formations[battleId]
		if formation == nil {
			return nil, nil
		}
		return spectateRecordFromFormation(formation)
	}
	spectateTargetFn = func(_ context.Context, _ *svc.ServiceContext, _ uint32, battleId uint64) (string, string, error) {
		formation := f.formations[battleId]
		require.NotNil(t, formation, "定位 battle 节点的场必须已登记 battle=%d", battleId)
		return formation.GetTargetEndpoint(), formation.GetTargetInstanceId(), nil
	}
	currentSpectateFormations = f
	t.Cleanup(func() {
		loadSpectateBattleFn, spectateTargetFn, currentSpectateFormations = prevLoad, prevTarget, prevCurrent
	})
	return f
}

// testFormation 组一份已开房(state 4)的成局原件,落点是 newGatherSvcCtx 灌的 battle 节点 7。
func testFormation(t *testing.T, battleId, createdAtMs uint64, mode matchpb.MatchMode, visibility battlepb.ESpectateVisibility, names ...string) *matchpb.MatchFormationRecord {
	t.Helper()
	players := make([]*battlepb.BattlePlayerSnapshot, 0, len(names))
	for _, name := range names {
		players = append(players, &battlepb.BattlePlayerSnapshot{PlayerName: name})
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{
		BattleId: battleId, MatchMode: uint32(mode), SpectateVisibility: visibility, Players: players,
	})
	require.NoError(t, err)
	return &matchpb.MatchFormationRecord{
		BattleId: battleId, State: 4, Mode: uint32(mode), TargetNodeId: 7, TargetInstanceId: testBattle7Instance, TargetEndpoint: "battle-7",
		DeadlineMs: testRelayDeadlineMs, CreatedMs: createdAtMs, CreateRequest: raw,
	}
}

// registerBattle 登记一场可观战战斗:成局原件进桩,活跃集合按生产路径写。模式用公开的 PVE_SOLO:
// 1V1 等计分局按可见性策略是私密局,不进活跃集合。
func registerBattle(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64, createdAtMs uint64, names ...string) {
	t.Helper()
	stubSpectateFormations(t).formations[battleId] = testFormation(t, battleId, createdAtMs, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, names...)
	registerSpectateBattle(svcCtx, battleId, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, createdAtMs)
}

// registerPrivateBattle 登记一场私密局(1V1 计分局):成局原件进桩、不进活跃集合。stale=true 时再把它
// 塞进活跃集合,模拟残留成员(addSpectateActive 拒收私密局,正常路径进不来)。
func registerPrivateBattle(t *testing.T, svcCtx *svc.ServiceContext, battleId uint64, stale bool) {
	t.Helper()
	activeBefore := activeIndexSize(t, svcCtx)
	stubSpectateFormations(t).formations[battleId] = testFormation(t, battleId, nowMs(), matchpb.MatchMode_MATCH_MODE_1V1, visPrivate, "甲", "乙")
	registerSpectateBattle(svcCtx, battleId, matchpb.MatchMode_MATCH_MODE_1V1, nowMs())
	require.Equal(t, activeBefore, activeIndexSize(t, svcCtx), "私密局登记后不进活跃集合")
	if stale {
		_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), strconv.FormatUint(battleId, 10))
		require.NoError(t, err)
	}
}

// staleCreatedAtMs 返回一个必然被判定为过期的 created_at(早于 TTL 窗口 1s)。
func staleCreatedAtMs() uint64 {
	return nowMs() - uint64(spectateTTLSeconds)*1000 - 1000
}

func activeIndexSize(t *testing.T, svcCtx *svc.ServiceContext) int {
	t.Helper()
	card, err := svcCtx.MatchRedis.Zcard(spectateBattlesActiveKey)
	require.NoError(t, err)
	return card
}

// ---- 索引 ----

// 登记 → ZSET 含成员且 score=created_at;剔除后清空。
func TestSpectateRegisterRemoveRoundTrip(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const battleId = uint64(880001)
	createdAt := nowMs()
	registerSpectateBattle(svcCtx, battleId, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, createdAt)

	members, err := mr.ZMembers(spectateBattlesActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{"880001"}, members)
	score, err := mr.ZScore(spectateBattlesActiveKey, "880001")
	require.NoError(t, err)
	require.Equal(t, float64(createdAt), score)

	removeSpectateBattle(svcCtx, battleId)
	require.Zero(t, activeIndexSize(t, svcCtx))
}

// 随机选场:活跃战斗与坏成员(过期 score / 非法 id)混在一起时,只可能返回活跃战斗。
func TestPickRandomBattleNeverReturnsStaleOrGarbage(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	fresh, stale := uint64(880010), uint64(880011)
	registerBattle(t, svcCtx, fresh, nowMs())
	registerBattle(t, svcCtx, stale, staleCreatedAtMs())
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), "not-a-battle-id")
	require.NoError(t, err)

	for i := 0; i < 10; i++ {
		picked, err := pickRandomBattle(svcCtx)
		require.NoError(t, err)
		require.Equal(t, fresh, picked, "第 %d 次:坏成员只能被剔除,不能被返回", i)
	}
}

// 随机选场:只剩坏成员时全部现场剔除,返回 0 表示无场可看。
func TestPickRandomBattleEvictsAllBadMembersAndReportsNone(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	stale := uint64(880012)
	registerBattle(t, svcCtx, stale, staleCreatedAtMs())
	_, err := svcCtx.MatchRedis.Zadd(spectateBattlesActiveKey, int64(nowMs()), "garbage")
	require.NoError(t, err)

	picked, err := pickRandomBattle(svcCtx)
	require.NoError(t, err)
	require.Zero(t, picked)
	require.Zero(t, activeIndexSize(t, svcCtx), "过期与非法成员必须被现场剔除")
}

// 随机选场最多尝试 3 次:全是过期成员时放弃并返回 0(不是死循环、也不是一次性清空整个索引)。
// 5 个过期成员 → 剔掉 3 个后放弃,剩 2 个由定期清理兜底。
func TestPickRandomBattleGivesUpAfterThreeAttempts(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	stale := staleCreatedAtMs()
	for i := 0; i < 5; i++ {
		registerBattle(t, svcCtx, uint64(880310+i), stale)
	}

	picked, err := pickRandomBattle(svcCtx)
	require.NoError(t, err)
	require.Zero(t, picked)
	require.Equal(t, 2, activeIndexSize(t, svcCtx), "每次尝试剔一个,尝试 3 次剔 3 个")
}

// 定期清理:过期 score 成员清掉,活跃的保留。
func TestCleanupExpiredSpectateIndexKeepsFresh(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	fresh, stale := uint64(880020), uint64(880021)
	registerBattle(t, svcCtx, fresh, nowMs())
	registerBattle(t, svcCtx, stale, staleCreatedAtMs())

	cleanupExpiredSpectateIndex(svcCtx)

	members, err := mr.ZMembers(spectateBattlesActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{"880020"}, members)
}

// ---- 列表 ----

// 列表:最新开局在前、孤儿成员(ZSET 有、成局已不存在)现场剔除、limit 生效。
func TestListWatchableBattlesNewestFirstAndEvictsOrphans(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
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

	limited, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{Limit: 2})
	require.NoError(t, err)
	require.Len(t, limited.GetBattles(), 2)
	require.Equal(t, uint64(880032), limited.GetBattles()[0].GetBattleId())
}

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

// 列表:score 过期的成员现场剔除且不出现在结果里。
func TestListWatchableBattlesEvictsStaleMembers(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	registerBattle(t, svcCtx, 880230, nowMs(), "好场")
	registerBattle(t, svcCtx, 880231, staleCreatedAtMs())

	resp, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetBattles(), 1)
	require.Equal(t, uint64(880230), resp.GetBattles()[0].GetBattleId())

	require.Equal(t, 1, activeIndexSize(t, svcCtx), "过期成员要被剔除")
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

// 观战列表隐藏私密局:残留在活跃集合里的私密成员被摘掉、不出现在结果里。
func TestListWatchableBattlesHidesPrivateMember(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	const public, private = uint64(880295), uint64(880296)
	registerPrivateBattle(t, svcCtx, private, true)
	registerBattle(t, svcCtx, public, nowMs()-1000, "公开场")

	resp, err := NewListWatchableBattlesLogic(context.Background(), svcCtx).
		ListWatchableBattles(&matchpb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetBattles(), 1)
	require.Equal(t, public, resp.GetBattles()[0].GetBattleId())
	members, err := mr.ZMembers(spectateBattlesActiveKey)
	require.NoError(t, err)
	require.Equal(t, []string{strconv.FormatUint(public, 10)}, members, "私密成员被摘掉")
}

// ---- 取消证明 ----

// 取消证明的判据:只有带回原 op 与原实例的 cancelled 才算。battle 对不合账本契约的 Remove 回空包、不摘人。
func TestObserverCancelProven(t *testing.T) {
	const op, otherOp = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	const instance, otherInstance = "instance-a", "instance-b"
	req := &battlepb.RemoveObserverRequest{SpectatorOperationId: op, TargetInstanceId: instance}
	cases := []struct {
		name  string
		reply *battlepb.RemoveObserverResponse
		want  bool
	}{
		{"带回原 op 与实例", &battlepb.RemoveObserverResponse{Cancelled: true, SpectatorOperationId: op, TargetInstanceId: instance}, true},
		{"空回包", &battlepb.RemoveObserverResponse{}, false},
		{"没有回包", nil, false},
		{"未取消", &battlepb.RemoveObserverResponse{SpectatorOperationId: op, TargetInstanceId: instance}, false},
		{"op 不符", &battlepb.RemoveObserverResponse{Cancelled: true, SpectatorOperationId: otherOp, TargetInstanceId: instance}, false},
		{"实例不符", &battlepb.RemoveObserverResponse{Cancelled: true, SpectatorOperationId: op, TargetInstanceId: otherInstance}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, observerCancelProven(req, c.reply))
		})
	}
}
