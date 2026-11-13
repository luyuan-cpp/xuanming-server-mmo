package loginsession

// gate 为活着的连接续租登录态(Renew)的单测(docs/design/gate-online-identity.md §10.4)。
//
// 续租之前的问题:登录态与设备集是绝对寿命(Node.SessionExpireMin),在线超过它的连接从设备集里消失,
// 账号的设备数上限(Account.MaxDevicesPerAccount)对它不再计数。

import (
	"context"
	"testing"
	"time"

	"login/internal/logic/pkg/sessionfence"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// loginForRenewTest 走登录提交的两步,让这条连接以 victimAccount 登录。
func loginForRenewTest(t *testing.T, ctx context.Context, rdb *redis.Client, ref Ref) {
	t.Helper()
	require.NoError(t, sessionfence.CommitLogin(ctx, rdb, ref.Fence(), victimAccount, TTL()))
	require.NoError(t, RegisterDevice(ctx, rdb, victimAccount, ref, TTL()))
}

func TestRenewKeepsSessionAndDeviceCountedPastTheOriginalTTL(t *testing.T) {
	// 回归场景:登录后一直在线。每隔寿命的三分之一续一次,过了最初的寿命,登录态与设备集成员都还在,
	// 再登录时这条连接仍被计入设备数(PruneStaleFromDeviceSet 不剔它)。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	loginForRenewTest(t, ctx, rdb, ref)

	for elapsed := time.Duration(0); elapsed < 2*TTL(); elapsed += RenewAfter(TTL()) {
		mr.FastForward(RenewAfter(TTL()))
		kept, err := Renew(ctx, rdb, ref, TTL())
		require.NoError(t, err)
		require.True(t, kept)
	}

	require.Equal(t, TTL(), mr.TTL(ref.Key()), "登录态续满")
	require.Equal(t, TTL(), mr.TTL(deviceSet), "设备集续满")
	require.Zero(t, PruneStaleFromDeviceSet(ctx, rdb, deviceSet), "在线的连接不是陈旧成员")
	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.Equal(t, []string{ref.DeviceMember()}, members)
}

func TestWithoutRenewTheSessionExpiresAndDropsOutOfTheDeviceSet(t *testing.T) {
	// 对照:不续租时,过了寿命登录态与设备集一起到期,这条连接不再被计数。这就是续租要补的缺口,也是 gate 进程崩溃后的回收路径。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	loginForRenewTest(t, ctx, rdb, ref)

	mr.FastForward(TTL() + time.Second)

	require.False(t, mr.Exists(ref.Key()))
	require.False(t, mr.Exists(redisslot.AccountDeviceSetKey(victimAccount)))
	kept, err := Renew(ctx, rdb, ref, TTL())
	require.NoError(t, err)
	require.False(t, kept, "到期之后没有可续的,续租从不新建登录态")
	require.Empty(t, mr.Keys())
}

func TestRenewRestoresAMissingDeviceMember(t *testing.T) {
	// 登录态还在、设备集成员丢了(设备集被清掉):续租把成员补回,在线的连接一定被计数。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	loginForRenewTest(t, ctx, rdb, ref)
	require.NoError(t, rdb.Del(ctx, deviceSet).Err())

	kept, err := Renew(ctx, rdb, ref, TTL())
	require.NoError(t, err)
	require.True(t, kept)
	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.Equal(t, []string{ref.DeviceMember()}, members)
	require.Equal(t, TTL(), mr.TTL(deviceSet))
}

func TestRenewAfterCleanupWritesNothing(t *testing.T) {
	// 离开游戏 / 断线清理之后迟到的续租:没有可续的,不得把登录态或设备集成员写回去。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	loginForRenewTest(t, ctx, rdb, ref)
	Cleanup(ctx, rdb, ref, "test")

	kept, err := Renew(ctx, rdb, ref, TTL())
	require.NoError(t, err)
	require.False(t, kept)
	require.False(t, mr.Exists(ref.Key()), "清理之后续租不重建登录态")
	isMember, err := rdb.SIsMember(ctx, redisslot.AccountDeviceSetKey(victimAccount), ref.DeviceMember()).Result()
	require.NoError(t, err)
	require.False(t, isMember, "清理之后续租不把连接登记回设备集")
}

func TestRenewAfterConnectionClosedDoesNotReviveTheSession(t *testing.T) {
	// Disconnect 先关栅栏、再清登录态。续租落在两步之间时登录态还在,但连接已关闭:不续、不重新登记设备。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	loginForRenewTest(t, ctx, rdb, ref)
	_, err := sessionfence.Close(ctx, rdb, ref.Fence(), 6*time.Minute)
	require.NoError(t, err)
	mr.FastForward(time.Minute)

	kept, err := Renew(ctx, rdb, ref, TTL())
	require.NoError(t, err)
	require.False(t, kept)
	require.Equal(t, TTL()-time.Minute, mr.TTL(ref.Key()), "登录态寿命没有被续")
	require.Equal(t, TTL()-time.Minute, mr.TTL(deviceSet), "设备集寿命没有被续")
}

func TestRenewDoesNotTouchAnotherConnectionOfTheSameAccount(t *testing.T) {
	// 同一账号两条连接:只续发来续租的那一条;另一条不续租就照常到期,被下一次登录的自愈剔出设备集。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	live, silent := mustRef(t, oldGateDetails()), mustRef(t, restartedGateDetails())
	deviceSet := redisslot.AccountDeviceSetKey(victimAccount)
	loginForRenewTest(t, ctx, rdb, live)
	loginForRenewTest(t, ctx, rdb, silent)

	for i := 0; i < 4; i++ {
		mr.FastForward(RenewAfter(TTL()))
		kept, err := Renew(ctx, rdb, live, TTL())
		require.NoError(t, err)
		require.True(t, kept)
	}

	require.True(t, mr.Exists(live.Key()))
	require.False(t, mr.Exists(silent.Key()), "没续租的连接照常到期")
	require.Equal(t, 1, PruneStaleFromDeviceSet(ctx, rdb, deviceSet), "到期连接的成员由自愈剔除")
	members, err := mr.Members(deviceSet)
	require.NoError(t, err)
	require.Equal(t, []string{live.DeviceMember()}, members)
}

func TestRenewReportsRedisFailure(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	ref := mustRef(t, oldGateDetails())
	loginForRenewTest(t, ctx, rdb, ref)
	mr.SetError("ERR injected redis failure")
	t.Cleanup(func() { mr.SetError("") })

	kept, err := Renew(ctx, rdb, ref, TTL())
	require.Error(t, err, "Redis 故障必须报给调用方,gate 退避之后重试")
	require.False(t, kept)
}

func TestRenewAfterIsOneThirdOfTheLifetime(t *testing.T) {
	require.Equal(t, 10*time.Minute, RenewAfter(30*time.Minute))
}
