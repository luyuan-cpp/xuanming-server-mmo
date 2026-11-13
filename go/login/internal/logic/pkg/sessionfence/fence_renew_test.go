package sessionfence

// gate 定期续租(Renew)的单测(docs/design/gate-online-identity.md §10.4)。
//
// 续租只续已有的记录、从不新建,而且先查 closed:Disconnect 处理过的连接,迟到的续租不得把登录态续活。

import (
	"context"
	"testing"
	"time"

	"shared/playerredis"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

func TestRenewExtendsLoginSessionAndReportsAccount(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	mr.FastForward(20 * time.Minute)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001"}, renewal)
	require.Equal(t, 30*time.Minute, mr.TTL(testSessionKey), "登录态续满")
	require.Equal(t, []string{testSessionKey}, mr.Keys(), "没绑定角色的连接不建栅栏记录")
}

func TestRenewKeepsBoundRecordAtFullLifetime(t *testing.T) {
	// 在线的连接:登录态与绑定都在。续租把两样都续满,绑定不会因为兜底寿命到期而丢。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 3*time.Hour))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(time.Hour)
	require.Equal(t, boundRecordTTL-time.Hour, mr.TTL(testKey.redisKey()), "夹具前提:绑定的记录在倒计时")

	renewal, err := Renew(ctx, rdb, testKey, 3*time.Hour)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001"}, renewal)
	require.Equal(t, 3*time.Hour, mr.TTL(testSessionKey))
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()), "绑定的记录续满 boundRecordTTL")

	bound, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound, "续租不改绑定本身")
}

func TestRenewWithoutLoginSessionStillKeepsBinding(t *testing.T) {
	// 登录态丢了(login 域 Redis 丢键、续租连续失败超过登录态寿命)但角色还绑在这条连接上:绑定照样续,且不凭空建登录态。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(time.Hour)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true}, renewal, "还有绑定可续,账号为空")
	require.False(t, mr.Exists(testSessionKey), "续租从不新建登录态:登录态只能由 Login 建")
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()))
}

func TestRenewAfterCloseWritesNothing(t *testing.T) {
	// Disconnect 先把栅栏标成 closed、随后才删登录态。迟到的续租落在两步之间时,不得把登录态续活。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	_, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	mr.FastForward(time.Minute)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{}, renewal, "连接已关闭:没有可续的")
	require.Equal(t, 29*time.Minute, mr.TTL(testSessionKey), "登录态寿命没有被续")
	require.Equal(t, testTTL-time.Minute, mr.TTL(testKey.redisKey()), "关闭后的栅栏记录寿命没有被续")
}

func TestRenewWithNothingLeftWritesNothing(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{}, renewal, "从没登录过的连接没有可续的")
	require.Empty(t, mr.Keys(), "续租不建任何键")

	// 离开游戏之后:LeaveGame 解除绑定并删登录态,栅栏记录只剩给在途操作用的短寿命,续租不得把它续长。
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	_, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.NoError(t, rdb.Del(ctx, testSessionKey).Err())
	mr.FastForward(time.Minute)

	renewal, err = Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{}, renewal, "离开游戏之后没有可续的,gate 据此停止续租")
	require.False(t, mr.Exists(testSessionKey))
	require.Equal(t, testTTL-time.Minute, mr.TTL(testKey.redisKey()), "解除绑定后的记录不续")
}

func TestRenewRejectsNonPositiveTTL(t *testing.T) {
	// PEXPIRE 的 0 / 负值会让 Redis 立刻删键:SessionExpireMin 漏配为 0 时续租不得变成清登录态。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	for _, ttl := range []time.Duration{0, -time.Second} {
		_, err := Renew(ctx, rdb, testKey, ttl)
		require.Error(t, err)
	}
	require.Equal(t, 30*time.Minute, mr.TTL(testSessionKey), "登录态原样保留")
}

// 续租脚本碰栅栏与登录态两个键,和 CommitLogin 一样必须同槽;走生产的玩家域构造器,守卫在发送前拒绝跨槽的键。
func TestRenewScriptPassesTheClusterGuard(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdb.Close() })

	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", time.Minute))
	renewal, err := Renew(ctx, rdb, testKey, time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001"}, renewal)
}
