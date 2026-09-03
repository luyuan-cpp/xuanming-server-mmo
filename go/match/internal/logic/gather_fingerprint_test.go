package logic

// 配表指纹比对(设计决策 D14)与 prepare_deadline_ms(D13)的 miniredis 测试:
// gather 真跑到 CreateBattle,scene / battle 的四个 gRPC 用 fakeGatherRPCs 顶替
// (prepareBattleFn / cancelBattlePrepareFn / createBattleFn / destroyBattleFn),
// 节点镜像用 NodeWatcher.Upsert 直接灌,不起 etcd。

import (
	"sync"
	"testing"

	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
	scenepb "proto/scene"

	"shared/snowflake"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// fakeGatherRPCs 记录 gather 发出的四类 RPC;fingerprints 决定各玩家 PrepareBattle
// 响应里的 table_fingerprint(缺项 = 空串,模拟旧版 scene 不回报)。
type fakeGatherRPCs struct {
	mu           sync.Mutex
	fingerprints map[uint64]string
	prepares     []*scenepb.PrepareBattleRequest
	cancelled    []uint64
	created      []*battlepb.CreateBattleRequest
	destroyed    []uint64
}

func stubGatherRPCs(t *testing.T, fingerprints map[uint64]string) *fakeGatherRPCs {
	t.Helper()
	f := &fakeGatherRPCs{fingerprints: fingerprints}
	prevPrepare, prevCancel, prevCreate, prevDestroy := prepareBattleFn, cancelBattlePrepareFn, createBattleFn, destroyBattleFn
	prepareBattleFn = func(endpoint string, req *scenepb.PrepareBattleRequest) (*scenepb.PrepareBattleResponse, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prepares = append(f.prepares, req)
		return &scenepb.PrepareBattleResponse{
			Snapshot: &battlepb.BattlePlayerSnapshot{
				PlayerId:   req.PlayerId,
				PlayerName: "p",
				Routing:    &battlepb.BattleRouting{ZoneId: uint32(req.PlayerId % 2)},
			},
			TableFingerprint: f.fingerprints[req.PlayerId],
		}, nil
	}
	cancelBattlePrepareFn = func(endpoint string, req *scenepb.CancelBattlePrepareRequest) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cancelled = append(f.cancelled, req.PlayerId)
		return nil
	}
	createBattleFn = func(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.created = append(f.created, req)
		return &battlepb.CreateBattleResponse{BattleId: req.BattleId}, nil
	}
	destroyBattleFn = func(endpoint string, req *battlepb.DestroyBattleRequest) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.destroyed = append(f.destroyed, req.BattleId)
		return nil
	}
	t.Cleanup(func() {
		prepareBattleFn, cancelBattlePrepareFn, createBattleFn, destroyBattleFn = prevPrepare, prevCancel, prevCreate, prevDestroy
	})
	return f
}

// newGatherSvcCtx 在 newTestSvcCtx 之上补齐 gather 需要的发号器与节点镜像:
// zone1 / zone2 各一个 node_id=1 的 scene(与 setPlayerLocation 写的 NodeId "1"
// 对齐),battle 池一个节点。
func newGatherSvcCtx(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis) {
	t.Helper()
	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.BattleIDGen = snowflake.NewNode(1)
	svcCtx.SceneNodes = discovery.NewNodeWatcher("scene", svc.SceneNodeRpcPrefix, nil, nil)
	svcCtx.SceneNodes.Upsert(svc.SceneNodeRpcPrefix+"zone/1/1", discovery.NodeEntry{NodeId: 1, ZoneId: 1, Endpoint: "scene-z1"})
	svcCtx.SceneNodes.Upsert(svc.SceneNodeRpcPrefix+"zone/2/1", discovery.NodeEntry{NodeId: 1, ZoneId: 2, Endpoint: "scene-z2"})
	svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)
	svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, Endpoint: "battle-7"})
	return svcCtx, mr
}

// popMatchedPair 让 a(zone1)/ b(zone2)入 1V1 队列并按生产路径弹出
// (票据进 matched),返回 gather 入参。
func popMatchedPair(t *testing.T, svcCtx *svc.ServiceContext, mr *miniredis.Miniredis, a, b uint64) ([]uint64, map[uint64]string, string) {
	t.Helper()
	setPlayerLocation(t, mr, a, 1)
	setPlayerLocation(t, mr, b, 2)
	require.Zero(t, joinQueue1v1(t, svcCtx, a).ErrorCode)
	require.Zero(t, joinQueue1v1(t, svcCtx, b).ErrorCode)
	queueKey := matchQueueKey(int32(matchpb.MatchMode_MATCH_MODE_1V1), 0)
	members, tickets, ok := popGroup(svcCtx, queueKey, 2)
	require.True(t, ok)
	require.Equal(t, []uint64{a, b}, members, "同评分按等待先后弹出")
	return members, tickets, queueKey
}

// 全员指纹一致:透传给 CreateBattle;PrepareBattleRequest 带 prepare_deadline_ms
// = now + matched TTL(按组大小),且 deadline_ms 仍是战斗时限。
func TestGatherPassesConsistentTableFingerprintAndPrepareDeadline(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	fake := stubGatherRPCs(t, map[uint64]string{9101: "fp-A", 9102: "fp-A"})
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9101, 9102)

	before := nowMs()
	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)
	after := nowMs()

	require.Len(t, fake.created, 1)
	require.Equal(t, "fp-A", fake.created[0].TableFingerprint, "全员一致的指纹必须透传给 battle")
	require.Empty(t, fake.cancelled)
	require.Empty(t, fake.destroyed)

	matchedTTLMs := uint64(matchedTicketTTLFor(svcCtx, 2)) * 1000
	battleTTLMs := uint64(svcCtx.Config.BattleMaxDurationSeconds) * 1000
	require.Len(t, fake.prepares, 2)
	for _, req := range fake.prepares {
		require.GreaterOrEqual(t, req.PrepareDeadlineMs, before+matchedTTLMs)
		require.LessOrEqual(t, req.PrepareDeadlineMs, after+matchedTTLMs)
		require.GreaterOrEqual(t, req.DeadlineMs, before+battleTTLMs)
		require.LessOrEqual(t, req.DeadlineMs, after+battleTTLMs)
		require.Less(t, req.PrepareDeadlineMs, req.DeadlineMs, "备战期限必须短于战斗期限")
	}
	for _, pid := range members {
		ticket, err := loadTicket(svcCtx, pid)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.Equal(t, ticketStateReady, ticket.State)
	}
}

// warn(默认):指纹不一致仍开局,不透传指纹,计数器 +1。
func TestGatherWarnModeMismatchStillCreatesBattle(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	require.Empty(t, svcCtx.Config.TableFingerprintMode, "空值必须按默认 warn 处理")
	fake := stubGatherRPCs(t, map[uint64]string{9201: "fp-A", 9202: "fp-B"})
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9201, 9202)
	baseline := metrics.TableFingerprintMismatchValue(tableFingerprintWarn)
	enforceBaseline := metrics.TableFingerprintMismatchValue(tableFingerprintEnforce)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1, "warn 下照常开局")
	require.Empty(t, fake.created[0].TableFingerprint, "不一致时不能透传任何一方的指纹")
	require.Empty(t, fake.cancelled)
	require.Equal(t, baseline+1, metrics.TableFingerprintMismatchValue(tableFingerprintWarn))
	require.Equal(t, enforceBaseline, metrics.TableFingerprintMismatchValue(tableFingerprintEnforce), "warn 不能记到 enforce 上")
}

// warn:部分为空(旧版 scene 不回报)同样算不一致 —— 不透传、计数。
func TestGatherWarnModePartialEmptyCountsAsMismatch(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	svcCtx.Config.TableFingerprintMode = tableFingerprintWarn
	fake := stubGatherRPCs(t, map[uint64]string{9301: "fp-A"}) // 9302 缺项 = 空
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9301, 9302)
	baseline := metrics.TableFingerprintMismatchValue(tableFingerprintWarn)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1)
	require.Empty(t, fake.created[0].TableFingerprint)
	require.Equal(t, baseline+1, metrics.TableFingerprintMismatchValue(tableFingerprintWarn))
}

// enforce:指纹不一致视为 prepare 失败 —— 不建局、已冻结者全部解冻、
// 肇事者(与多数派不一致者)删票出局、幸存者回队首恢复 queued,计数器 +1。
func TestGatherEnforceModeMismatchCompensates(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	svcCtx.Config.TableFingerprintMode = tableFingerprintEnforce
	fake := stubGatherRPCs(t, map[uint64]string{9401: "fp-A", 9402: "fp-B"})
	members, tickets, queueKey := popMatchedPair(t, svcCtx, mr, 9401, 9402)
	baseline := metrics.TableFingerprintMismatchValue(tableFingerprintEnforce)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Empty(t, fake.created, "enforce 下不得建局")
	require.Empty(t, fake.destroyed, "没建局就没有 DestroyBattle")
	require.ElementsMatch(t, []uint64{9401, 9402}, fake.cancelled, "已冻结的两人都要解冻")
	require.Equal(t, baseline+1, metrics.TableFingerprintMismatchValue(tableFingerprintEnforce))

	// 肇事者 = 9402(两人平票,多数派取弹出序靠前的 9401):删票出局。
	require.False(t, mr.Exists(matchTicketKey(9402)), "肇事者票据必须删除")
	// 幸存者回队首:票据 queued + 长 TTL,人在队列里。
	ticket, err := loadTicket(svcCtx, 9401)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, ticketStateQueued, ticket.State)
	ttl, err := svcCtx.MatchRedis.Ttl(matchTicketKey(9401))
	require.NoError(t, err)
	require.Greater(t, ttl, int(svcCtx.Config.MatchedTicketTTLSeconds))
	list, err := mr.List(queueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"9401"}, list, "幸存者回队首,肇事者不在队列")
}

// off:不比对(不一致也不计数)、不透传(一致也不透传)。
func TestGatherOffModeSkipsFingerprint(t *testing.T) {
	svcCtx, mr := newGatherSvcCtx(t)
	svcCtx.Config.TableFingerprintMode = tableFingerprintOff
	fake := stubGatherRPCs(t, map[uint64]string{9501: "fp-A", 9502: "fp-A"})
	members, tickets, _ := popMatchedPair(t, svcCtx, mr, 9501, 9502)
	warnBaseline := metrics.TableFingerprintMismatchValue(tableFingerprintWarn)
	enforceBaseline := metrics.TableFingerprintMismatchValue(tableFingerprintEnforce)

	RunGather(svcCtx, matchpb.MatchMode_MATCH_MODE_1V1, 0, members, true, tickets)

	require.Len(t, fake.created, 1)
	require.Empty(t, fake.created[0].TableFingerprint, "off 下即使一致也不透传")
	require.Equal(t, warnBaseline, metrics.TableFingerprintMismatchValue(tableFingerprintWarn))
	require.Equal(t, enforceBaseline, metrics.TableFingerprintMismatchValue(tableFingerprintEnforce))
}

// checkTableFingerprints 的多数派 / 肇事者选择(纯函数,不走 gather):
// 三人组两同一异 → 异者是肇事者;空指纹不参与多数派;全员皆空记第一位。
func TestCheckTableFingerprintsPicksMinorityAsOffender(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	svcCtx.Config.TableFingerprintMode = tableFingerprintEnforce
	group := func(fps ...string) []preparedMember {
		out := make([]preparedMember, 0, len(fps))
		for i, fp := range fps {
			out = append(out, preparedMember{playerId: uint64(100 + i), tableFingerprint: fp})
		}
		return out
	}

	fp, offender, ok := checkTableFingerprints(svcCtx, "m", 1, group("A", "A", "A"))
	require.True(t, ok)
	require.Equal(t, "A", fp)
	require.Zero(t, offender)

	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "B", "A"))
	require.False(t, ok)
	require.Empty(t, fp)
	require.Equal(t, uint64(101), offender, "与多数派 A 不一致的第二人是肇事者")

	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("", "B", "B"))
	require.False(t, ok)
	require.Empty(t, fp)
	require.Equal(t, uint64(100), offender, "空指纹不能凑多数派,不回报者是肇事者")

	_, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("", ""))
	require.False(t, ok)
	require.Equal(t, uint64(100), offender, "全员皆空没有多数派,记第一位")

	svcCtx.Config.TableFingerprintMode = tableFingerprintWarn
	fp, offender, ok = checkTableFingerprints(svcCtx, "m", 1, group("A", "B"))
	require.True(t, ok, "warn 放行")
	require.Empty(t, fp)
	require.Zero(t, offender)
}
