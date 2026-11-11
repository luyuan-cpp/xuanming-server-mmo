package svc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	"data_service/internal/store"

	base "proto/common/base"
	componentpb "proto/common/component"
	dbpb "proto/common/database"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/assetop"
	"shared/drainproof"
	sharedregistry "shared/noderegistry"
	"shared/rolefence"
	"shared/safego"
	"shared/workshard"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubFenceStates struct {
	state *rfpb.RoleFenceState
	err   error
}

func (s stubFenceStates) RoleFenceState(context.Context, uint64) (*rfpb.RoleFenceState, error) {
	return s.state, s.err
}

// 目录里没有 ≠ 目录读不到:前者这一行终结,后者行留着稍后再来。映射错了要么卡死要么误判角色不存在。
func TestRecallFenceReader_DistinguishesMissingFromUnavailable(t *testing.T) {
	want := &rfpb.RoleFenceState{PlayerId: 9}
	state, err := recallFenceReader{states: stubFenceStates{state: want}}.GetRoleFenceState(context.Background(), 9)
	require.NoError(t, err)
	assert.Same(t, want, state)

	_, err = recallFenceReader{states: stubFenceStates{err: accountdirectory.ErrMissing}}.GetRoleFenceState(context.Background(), 9)
	require.ErrorIs(t, err, rolefence.ErrRoleNotFound)

	_, err = recallFenceReader{states: stubFenceStates{err: errors.New("库坏了")}}.GetRoleFenceState(context.Background(), 9)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.NotErrorIs(t, err, rolefence.ErrRoleNotFound)
}

type stubAssetReader struct {
	row *store.PlayerAssetRow
	err error
}

func (s stubAssetReader) ReadPlayerStoreAsset(context.Context, uint64) (*store.PlayerAssetRow, error) {
	return s.row, s.err
}

// 没有写权行回 nil,有写权行回非 nil:Loop 靠这个区分「没存档,继续等」和「账本为空」。
func TestRecallLedger_ContractMatchesDataServiceLedger(t *testing.T) {
	ledger, err := recallLedger{assets: stubAssetReader{}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.Nil(t, ledger)

	ledger, err = recallLedger{assets: stubAssetReader{row: &store.PlayerAssetRow{Player: &dbpb.PlayerDatabase{}}}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.NotNil(t, ledger, "有存档、账本为空是空消息,不是 nil")

	stored := &componentpb.PlayerAssetOpLedgerComp{}
	ledger, err = recallLedger{assets: stubAssetReader{row: &store.PlayerAssetRow{Player: &dbpb.PlayerDatabase{AssetOpLedger: stored}}}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.NoError(t, err)
	assert.Same(t, stored, ledger)

	_, err = recallLedger{assets: stubAssetReader{err: errors.New("读失败")}}.ReadSQLCommittedLedger(context.Background(), 9)
	require.Error(t, err)
}

func recallEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// 通道装配失败要在连任何东西之前报出来:Enabled=true 时缺密钥、缺 Redis 地址都拒启。
func TestNewRecallChannel_RefusesIncompleteConfigBeforeConnecting(t *testing.T) {
	secret := map[string]string{RecallAssetSecretEnv: "0123456789abcdef0123456789abcdef"}
	enabled := config.Config{Recall: config.RecallConfig{Enabled: true}, SharedRedis: config.SharedRedisConf{Host: "127.0.0.1:1"}}
	enabled.Etcd.Hosts = []string{"127.0.0.1:1"}

	noHost := enabled
	noHost.SharedRedis.Host = ""
	_, err := newRecallChannel(noHost, nil, nil, nil, recallEnv(secret))
	require.ErrorContains(t, err, "SharedRedis.Host")

	_, err = newRecallChannel(enabled, nil, nil, nil, recallEnv(nil))
	require.ErrorContains(t, err, RecallAssetSecretEnv)

	_, err = newRecallChannel(enabled, nil, nil, nil, recallEnv(map[string]string{RecallAssetSecretEnv: "short"}))
	require.ErrorContains(t, err, RecallAssetSecretEnv)

	noEtcd := enabled
	noEtcd.Etcd.Hosts = nil
	_, err = newRecallChannel(noEtcd, nil, nil, nil, recallEnv(secret))
	require.ErrorContains(t, err, "Etcd.Hosts")
}

// 没有回收存储就不能接参与者:协调者据此不装配,否则漏问一个就是漏冻结。
func TestRecallFenceParticipant_RequiresTheStore(t *testing.T) {
	_, err := (&ServiceContext{}).recallFenceParticipant("0123456789abcdef0123456789abcdef")
	require.Error(t, err)
}

// 没配回收也要能停:关停路径不能因为这一段没起而报错。
func TestStopRecallChannel_NoopWhenNotAssembled(t *testing.T) {
	s := &ServiceContext{}
	s.StartRecallChannel()
	require.NoError(t, s.StopRecallChannel(context.Background()))
}

// 回收重投的成员表只圈本 zone 的 data_service 注册(缺口清单第 25 条第二批,设计 §10.2):前缀与 noderegistry 写的发现键同形,
// 以 "/" 结尾,zone 1 不会圈进 zone 10 的副本;分配占位键不在前缀下。
func TestRecallShardMembersPrefix_IsTheZoneRegistrationPrefix(t *testing.T) {
	prefix := recallShardMembersPrefix(1)
	assert.Equal(t, "DataServiceNodeService.rpc/zone/1/", prefix)
	nodeType := uint32(base.ENodeType_DataServiceNodeService)
	assert.True(t, strings.HasPrefix(sharedregistry.RpcPath("DataServiceNodeService.rpc", 1, nodeType, 3), prefix), "本 zone 的注册键在前缀下")
	assert.False(t, strings.HasPrefix(sharedregistry.RpcPath("DataServiceNodeService.rpc", 10, nodeType, 3), prefix), "别的 zone 不圈进来")
	assert.False(t, strings.HasPrefix(sharedregistry.AllocationKey("DataServiceNodeService.rpc", nodeType, 3), prefix), "占位键不在前缀下")
}

// 回收的 outbox(store.RecallStore)绑得上按片分工:没实现 OwnedDueLister 的话 newRecallChannel 回错误、Recall.Enabled=true 时拒启。
// 类别键与本仓库别的分片工作都不同名。没装配回收通道时补身份是空操作。
func TestRecallLoopBindsWorkOwner(t *testing.T) {
	loop, err := assetop.NewLoop(recallLoopConfig(), &store.RecallStore{}, &assetop.Caller{}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, loop.BindWorkOwner(recallAssetOpWorkKind, func(string) bool { return true }))
	for _, other := range []string{"guild-assetop", "trade-assetop", "mail-assetop", "outcome-result", "outcome-settlement", "trade-commerce"} {
		assert.NotEqual(t, other, recallAssetOpWorkKind)
	}
	(&ServiceContext{}).BindRecallWorkMember("uuid-a")
}

// ── 第四批:派发按片分工与派发循环的活性(缺口清单第 25 条,设计 §14.2、§14.3) ─────────────────────────────

// fakeDispatchStore 是派发用的假存储:手里一张 PLANNED 行表,ListPlannedOwned 按收到的分片条件过滤(只认 workshard.SQLFilter
// 生成的三种形状:1=1、1=0、MOD(player_id,64) IN(…)),Dispatch 一律派成并把行移出 PLANNED。几个「副本」可以共用一个。
type fakeDispatchStore struct {
	mu         sync.Mutex
	planned    []store.RecallPlanned
	filters    []workshard.Filter
	dispatched map[uint64]int // op_id → 被派成的次数
	listPanics bool
}

func newFakeDispatchStore(players ...uint64) *fakeDispatchStore {
	s := &fakeDispatchStore{dispatched: map[uint64]int{}}
	for i, player := range players {
		s.planned = append(s.planned, store.RecallPlanned{OpID: uint64(i + 1), PlayerID: player})
	}
	return s
}

func (s *fakeDispatchStore) ListPlannedOwned(_ context.Context, _ uint64, limit int, owned workshard.Filter) ([]store.RecallPlanned, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listPanics {
		panic("test: list planned")
	}
	s.filters = append(s.filters, owned)
	shards := make(map[int]bool, len(owned.Args()))
	for _, arg := range owned.Args() {
		shards[arg.(int)] = true
	}
	var out []store.RecallPlanned
	for _, row := range s.planned {
		if len(out) == limit {
			break
		}
		if owned.SQL() == "1=0" || (owned.SQL() != "1=1" && !shards[workshard.ShardOf(row.PlayerID)]) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *fakeDispatchStore) Dispatch(_ context.Context, op store.RecallPlanned, _ uint64) (store.RecallDispatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.planned {
		if row.OpID == op.OpID {
			s.planned = append(s.planned[:i], s.planned[i+1:]...)
			s.dispatched[op.OpID]++
			return store.RecallDispatched, nil
		}
	}
	return store.RecallLost, nil
}

func (s *fakeDispatchStore) DeferPlanned(context.Context, uint64, uint64, uint64) error { return nil }

func (s *fakeDispatchStore) lastFilter() workshard.Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filters[len(s.filters)-1]
}

func (s *fakeDispatchStore) remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.planned)
}

func (s *fakeDispatchStore) dispatchedTimes(opID uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatched[opID]
}

// dispatchReplica 造一个只做派发的副本:self 是它的 node_uuid,members 返回此刻的成员表(用 HRW 判归属,与 workshard.Membership 同一个函数)。
func dispatchReplica(s *fakeDispatchStore, self string, members func() []string) *RecallChannel {
	return &RecallChannel{store: s, relay: drainproof.New(), owns: func(key string) bool { return workshard.Owns(self, key, members()) }}
}

// onePlayerPerShard 回 64 个玩家号,第 n 个落在第 n 片。
func onePlayerPerShard() []uint64 {
	players := make([]uint64, workshard.Shards)
	for shard := range players {
		players[shard] = uint64(64*1000 + shard)
	}
	return players
}

// 两个副本各派各的片:每一行恰好被它那一片的负责者派一次,合起来一行不漏;交给存储的条件按 player_id 取模、片号升序走绑定参数。
func TestDispatchRoundDispatchesOnlyOwnedShards(t *testing.T) {
	members := []string{"uuid-a", "uuid-b"}
	fake := newFakeDispatchStore(onePlayerPerShard()...)
	a := dispatchReplica(fake, "uuid-a", func() []string { return members })
	b := dispatchReplica(fake, "uuid-b", func() []string { return members })

	a.dispatchRound(context.Background())
	filterA := fake.lastFilter()
	for shard := 0; shard < workshard.Shards; shard++ {
		want := 0
		if workshard.Owner(workshard.ShardKey(recallDispatchWorkKind, shard), members) == "uuid-a" {
			want = 1
		}
		require.Equal(t, want, fake.dispatchedTimes(uint64(shard+1)), "a 这一轮只派自己负责的片:片 %d", shard)
	}
	b.dispatchRound(context.Background())
	require.Zero(t, fake.remaining(), "两个副本合起来应把 64 片全部派完")
	for op := uint64(1); op <= workshard.Shards; op++ {
		require.Equal(t, 1, fake.dispatchedTimes(op), "op %d 应恰好派一次", op)
	}
	var wantShards []any
	for shard := 0; shard < workshard.Shards; shard++ {
		if workshard.Owns("uuid-a", workshard.ShardKey(recallDispatchWorkKind, shard), members) {
			wantShards = append(wantShards, shard)
		}
	}
	require.NotEmpty(t, wantShards, "夹具前提:两个成员时 a 至少负责一片")
	require.Less(t, len(wantShards), workshard.Shards, "夹具前提:两个成员时 a 不会负责全部片")
	assert.Equal(t, fmt.Sprintf("MOD(player_id,64) IN(%s)", strings.TrimSuffix(strings.Repeat("?,", len(wantShards)), ",")), filterA.SQL())
	assert.Equal(t, wantShards, filterA.Args(), "a 的条件只含它负责的片,片号升序")
	for _, other := range []string{recallAssetOpWorkKind, "guild-assetop", "trade-assetop", "mail-assetop", "mail-expiry-sweep", "trade-commerce"} {
		assert.NotEqual(t, other, recallDispatchWorkKind, "派发的类别键不与别的分片工作同名")
	}
}

// 成员变化时的接手:a 独自时派全部片;b 加入后 a 让出 b 的片;b 消失后 a 下一轮接回全部片。正确性不靠这一步,但片不会长期没人派。
func TestDispatchRoundTakesOverWhenMembersChange(t *testing.T) {
	var mu sync.Mutex
	members := []string{"uuid-a"}
	current := func() []string { mu.Lock(); defer mu.Unlock(); return members }
	setMembers := func(m ...string) { mu.Lock(); defer mu.Unlock(); members = m }

	fake := newFakeDispatchStore()
	a := dispatchReplica(fake, "uuid-a", current)
	a.dispatchRound(context.Background())
	assert.Equal(t, "1=1", fake.lastFilter().SQL(), "成员表里只有自己:派全部片,不加条件")

	setMembers("uuid-a", "uuid-b")
	a.dispatchRound(context.Background())
	shrunk := fake.lastFilter()
	require.NotEqual(t, "1=1", shrunk.SQL(), "b 加入之后 a 只派自己的片")
	require.NotEmpty(t, shrunk.Args(), "夹具前提:两个成员时 a 至少负责一片")
	for _, arg := range shrunk.Args() {
		assert.True(t, workshard.Owns("uuid-a", workshard.ShardKey(recallDispatchWorkKind, arg.(int)), current()), "片 %v 应归 a", arg)
	}

	setMembers("uuid-a")
	fake.mu.Lock()
	for i, player := range onePlayerPerShard() {
		fake.planned = append(fake.planned, store.RecallPlanned{OpID: uint64(100 + i), PlayerID: player})
	}
	fake.mu.Unlock()
	a.dispatchRound(context.Background())
	assert.Equal(t, "1=1", fake.lastFilter().SQL(), "b 消失之后 a 接回全部片")
	assert.Zero(t, fake.remaining(), "接手之后原属 b 的片也派完了")

	setMembers("uuid-b")
	a.dispatchRound(context.Background())
	assert.Equal(t, "1=0", fake.lastFilter().SQL(), "成员表不空、本副本不在其中(还没注册或租约丢了):一片都不派")
}

// recallDispatchLastRun 读 data_service_recall_dispatch_last_run_unixtime;没有序列(或读不出)时 ok=false。
// 不接 *testing.T:require.Eventually 在别的 goroutine 里调条件函数,那里不能 FailNow。
func recallDispatchLastRun() (value float64, ok bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, false
	}
	for _, mf := range families {
		if mf.GetName() == "data_service_recall_dispatch_last_run_unixtime" && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// 派发循环的活性:每跑完一轮写这一轮结束的时刻;这一轮 panic(safego 兜住)时不写,值停在上一轮;循环起来就有序列、退出就删。
func TestDispatchLoopLivenessMarksFinishedRoundsOnly(t *testing.T) {
	_, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err, "夹具前提:默认注册表读得出来")
	metrics.ClearRecallDispatchRun()
	_, ok := recallDispatchLastRun()
	require.False(t, ok, "循环没起来时不该有活性序列")

	fake := newFakeDispatchStore(64*1000 + 1)
	r := dispatchReplica(fake, "uuid-a", func() []string { return []string{"uuid-a"} })
	before := time.Now().Unix()
	r.dispatchOnce(context.Background())
	value, ok := recallDispatchLastRun()
	require.True(t, ok, "跑完一轮之后应有活性序列")
	assert.GreaterOrEqual(t, value, float64(before))
	assert.LessOrEqual(t, value, float64(time.Now().Unix()))

	metrics.MarkRecallDispatchRun(time.Unix(1000, 0))
	fake.mu.Lock()
	fake.listPanics = true
	fake.mu.Unlock()
	require.False(t, safego.Run(recallDispatchPoint, func() { r.dispatchOnce(context.Background()) }), "夹具前提:这一轮 panic")
	value, ok = recallDispatchLastRun()
	require.True(t, ok)
	assert.Equal(t, float64(1000), value, "panic 的那一轮不写活性时刻")
	assert.Equal(t, 1, fake.dispatchedTimes(1), "夹具前提:第一轮派成了那一行,panic 那一轮没有别的副作用")

	metrics.ClearRecallDispatchRun()
	fake.mu.Lock()
	fake.listPanics = false
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.runDispatch(ctx) }()
	require.Eventually(t, func() bool { _, ok := recallDispatchLastRun(); return ok }, 2*time.Second, 10*time.Millisecond,
		"循环一起来就写一次,不等第一轮")
	cancel()
	<-done
	_, ok = recallDispatchLastRun()
	assert.False(t, ok, "循环退出之后删掉序列:停掉的循环不算卡住")
}
