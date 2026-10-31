package loginsession

// 登录态按 gate 连接建键的回归测试(docs/design/data-overwrite-rollback-audit-20261018.md §2.3、
// client-request-ordering.md §7.2 第 2 条)。核心场景:gate 被 kill -9 后以同一 node_id 重启、session 序号从 0 开始,
// 新连接与旧进程的连接同号;旧连接的登录态在 TTL 内仍在 Redis 里。新连接不先 Login 就不能读到它。

import (
	"context"
	"errors"
	"testing"
	"time"

	"login/internal/config"
	pbbase "proto/common/base"
	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const (
	collidingSessionID = uint32(3<<17 | 5) // node_id = 3、序号 5:重启前后两个 gate 进程都会发出这个号
	victimAccount      = "victim_account"
)

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	previous := config.AppConfig.Node.SessionExpireMin
	config.AppConfig.Node.SessionExpireMin = 30
	t.Cleanup(func() { config.AppConfig.Node.SessionExpireMin = previous })
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

// 重启前的 gate 进程上的连接(已登录 victim_account)。
func oldGateDetails() *pbbase.SessionDetails {
	return &pbbase.SessionDetails{SessionId: collidingSessionID, GateInstanceId: "gate-before-restart", SessionIncarnation: 9}
}

// 重启后同一 node_id 的新 gate 进程上、与旧连接同号的新连接。
func restartedGateDetails() *pbbase.SessionDetails {
	return &pbbase.SessionDetails{SessionId: collidingSessionID, GateInstanceId: "gate-after-restart", SessionIncarnation: 1}
}

func mustRef(t *testing.T, details *pbbase.SessionDetails) Ref {
	t.Helper()
	ref, ok := RefOf(details)
	require.True(t, ok, "details=%v", details)
	return ref
}

func TestRefOfUsesGateInstanceAndIncarnation(t *testing.T) {
	ref := mustRef(t, oldGateDetails())
	require.Equal(t, "login_session:{gate-before-restart:9}", ref.Key())
	require.Equal(t, "gate-before-restart:9", ref.DeviceMember())
	require.Equal(t, "gate-before-restart", ref.Fence().GateInstanceID)
	require.Equal(t, uint64(9), ref.Fence().Incarnation)
	require.NotEqual(t, ref.Key(), mustRef(t, restartedGateDetails()).Key(), "撞号的两条连接必须落在不同的键上")
}

// 没有会话代次或实例号的连接没有全局唯一身份,不能建键:按 session_id 建键就是撞号继承登录态本身。
func TestRefOfRejectsConnectionWithoutIdentity(t *testing.T) {
	for _, details := range []*pbbase.SessionDetails{
		nil,
		{SessionId: collidingSessionID},
		{SessionId: collidingSessionID, GateInstanceId: "gate-x"},
		{SessionId: collidingSessionID, SessionIncarnation: 3},
	} {
		_, ok := RefOf(details)
		require.False(t, ok, "details=%v", details)
	}
}

func TestGetAccountAfterGateRestartDoesNotInheritCollidingSession(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	// 重启前:旧连接留下了 victim 的登录态,TTL 30min 内仍有效。
	victim := mustRef(t, oldGateDetails())
	require.NoError(t, rdb.Set(ctx, victim.Key(), victimAccount, TTL()).Err())

	// 重启后同号的新连接跳过 Login:读不到任何账号。
	_, err := GetAccount(ctx, rdb, mustRef(t, restartedGateDetails()))
	require.True(t, errors.Is(err, redis.Nil), "撞号的新连接没有登录态,err=%v", err)

	// 旧连接自己的登录态不受影响。
	account, err := GetAccount(ctx, rdb, victim)
	require.NoError(t, err)
	require.Equal(t, victimAccount, account)
}

func TestGetAccountAfterLoginOnRestartedGateReadsOwnAccount(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	require.NoError(t, rdb.Set(ctx, mustRef(t, oldGateDetails()).Key(), victimAccount, TTL()).Err())
	own := mustRef(t, restartedGateDetails())
	require.NoError(t, rdb.Set(ctx, own.Key(), "own_account", TTL()).Err())

	account, err := GetAccount(ctx, rdb, own)
	require.NoError(t, err)
	require.Equal(t, "own_account", account, "正常 Login 之后读到的是本连接自己的账号")
}

func TestRegisterDeviceAddsMemberAndKeepsTheSetAlive(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)

	require.NoError(t, RegisterDevice(ctx, rdb, victimAccount, ref, TTL()))

	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.Equal(t, []string{ref.DeviceMember()}, members)
	require.Equal(t, 30*time.Minute, mr.TTL(deviceSet))
}

func TestRegisterDeviceReportsRedisFailure(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	mr.SetError("ERR injected redis failure")
	t.Cleanup(func() { mr.SetError("") })
	require.Error(t, RegisterDevice(ctx, rdb, victimAccount, mustRef(t, oldGateDetails()), TTL()),
		"第 2 步失败必须报给调用方,这次登录按 Redis 故障回失败")
}

func TestCleanupRemovesSessionAndDeviceMember(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	require.NoError(t, rdb.Set(ctx, ref.Key(), victimAccount, TTL()).Err())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	require.NoError(t, rdb.SAdd(ctx, deviceSet, ref.DeviceMember(), "another-device:1").Err())

	Cleanup(ctx, rdb, ref, "test")

	require.False(t, mr.Exists(ref.Key()))
	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.Equal(t, []string{"another-device:1"}, members)
}

// 成员来自不同连接、不同槽:走生产的玩家域构造器,直接 MGET 会被守卫以跨槽拒绝,这里必须按 slot 分组读。
func TestPruneStaleFromDeviceSetAcrossSlots(t *testing.T) {
	ctx := context.Background()
	_, mr := newTestRedis(t)
	rdb, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdb.Close() })

	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	live := []string{mustRef(t, oldGateDetails()).DeviceMember(), mustRef(t, restartedGateDetails()).DeviceMember()}
	stale := []string{"gate-gone:4", "gate-gone:5", "gate-gone:6"}
	for _, member := range live {
		require.NoError(t, rdb.Set(ctx, redisslot.LoginSessionKey(member), victimAccount, TTL()).Err())
	}
	for _, member := range append(append([]string{}, live...), stale...) {
		require.NoError(t, rdb.SAdd(ctx, deviceSet, member).Err())
	}
	require.NotEqual(t, redisslot.HashSlot(redisslot.LoginSessionKey(live[0])), redisslot.HashSlot(redisslot.LoginSessionKey(live[1])),
		"夹具的两条活连接必须不同槽,否则用例证明不了按 slot 分组")

	require.Equal(t, len(stale), PruneStaleFromDeviceSet(ctx, rdb, deviceSet))

	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.ElementsMatch(t, live, members)
}

// 读不到登录态不等于登录态不存在:把活连接移出设备集比留一个陈旧成员更糟。
func TestPruneStaleFromDeviceSetKeepsMembersWhenSessionsUnreadable(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	require.NoError(t, rdb.SAdd(ctx, deviceSet, "gate-a:1", "gate-b:2").Err())
	rdb.AddHook(failingPipeline{})

	require.Zero(t, PruneStaleFromDeviceSet(ctx, rdb, deviceSet))

	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"gate-a:1", "gate-b:2"}, members)
}

// failingPipeline 让管道失败,单条命令照常:SMEMBERS 能读到成员,按 slot 读登录态报错。
type failingPipeline struct{}

func (failingPipeline) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failingPipeline) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (failingPipeline) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error {
		return errors.New("injected pipeline failure")
	}
}
