package sessionfence

// gate 定期续租(Renew)的单测(docs/design/gate-online-identity.md §10.4)。
//
// 续租先查 closed:Disconnect 处理过的连接,迟到的续租不得把登录态续活。它从不新建绑定;登录态只在一种情况下重建:
// 连接还绑着角色、登录态却过期了(续租曾连续失败超过寿命),按绑定记录里 login 自己留的账号副本重建。

import (
	"context"
	"testing"
	"time"

	"shared/playerredis"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

func TestRenewWithoutAccountCopyKeepsBindingButDoesNotInventALoginSession(t *testing.T) {
	// 绑定那一刻就没有登录态,记录里没有账号副本:绑定照样续,但登录态无从重建,续租不凭空造一个。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(time.Hour)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true}, renewal, "还有绑定可续,账号为空")
	require.False(t, mr.Exists(testSessionKey), "没有 login 自己留的账号副本就不建登录态")
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()))
}

func TestRenewRestoresExpiredLoginSessionOfABoundConnection(t *testing.T) {
	// 续租连续失败超过登录态寿命(login 或登录域 Redis 长时间不可用):登录态过期了,连接还在游戏里。
	// 恢复后的第一次续租按绑定记录里的账号副本重建登录态,并把账号报给调用方去重新登记设备。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(31 * time.Minute)
	require.False(t, mr.Exists(testSessionKey), "夹具前提:登录态已过期")

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001", Restored: true}, renewal)
	account, err := mr.Get(testSessionKey)
	require.NoError(t, err)
	require.Equal(t, "robot_0001", account, "重建出来的登录态与过期前是同一个账号")
	require.Equal(t, 30*time.Minute, mr.TTL(testSessionKey))
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()))

	// 重建之后就是普通的续租,不再报重建。
	renewal, err = Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001"}, renewal)
}

func TestRenewRestoresTheAccountOfTheLatestLoginOnTheConnection(t *testing.T) {
	// 绑定之后在同一条连接上重新 Login:登录态换成这次的账号,记录里的副本跟着改,重建出来的与过期前一致。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0002", 30*time.Minute))
	mr.FastForward(31 * time.Minute)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0002", Restored: true}, renewal)
}

func TestRenewDoesNotRestoreOnceTheBindingIsGone(t *testing.T) {
	// 重建只给还绑着角色、没关闭的连接。离开游戏连同账号副本一起解除;断线标 closed。两种之后登录态没了都无从重建。
	for name, unbind := range map[string]func(context.Context, *redis.Client) error{
		"离开游戏": func(ctx context.Context, rdb *redis.Client) error {
			_, err := Leave(ctx, rdb, testKey, testTTL)
			return err
		},
		"断线": func(ctx context.Context, rdb *redis.Client) error {
			_, err := Close(ctx, rdb, testKey, testTTL)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rdb, mr := newTestRedis(t)
			require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
			bindForLifetimeTest(t, ctx, rdb, 1001)
			require.NoError(t, unbind(ctx, rdb))
			require.NoError(t, rdb.Del(ctx, testSessionKey).Err(), "LeaveGame / Disconnect 的清理会删登录态")

			renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
			require.NoError(t, err)
			require.Equal(t, Renewal{}, renewal)
			require.False(t, mr.Exists(testSessionKey), "解除绑定或关闭之后续租不重建登录态")
		})
	}
}

func TestRenewDoesNotRestoreForAConnectionThatNeverEnteredTheGame(t *testing.T) {
	// 没进游戏的连接没有绑定记录,也就没有账号副本:登录态过期就是没有可续的,客户端下一步操作会被要求重新登录。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	mr.FastForward(31 * time.Minute)

	renewal, err := Renew(ctx, rdb, testKey, 30*time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{}, renewal)
	require.Empty(t, mr.Keys(), "续租不建任何键")
}

func TestLeaveDropsTheAccountCopySoALaterBindingCannotInheritIt(t *testing.T) {
	// 账号副本跟着绑定走:离开游戏之后再绑定时没有登录态,记录里不能还留着上一次绑定的账号。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	bindForLifetimeTest(t, ctx, rdb, 1001)
	require.Equal(t, "robot_0001", mr.HGet(testKey.redisKey(), "account"), "夹具前提:绑定时留了副本")
	_, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Empty(t, mr.HGet(testKey.redisKey(), "account"), "解除绑定连同副本一起删")

	require.NoError(t, rdb.Del(ctx, testSessionKey).Err())
	bindForLifetimeTest(t, ctx, rdb, 1002)
	require.Empty(t, mr.HGet(testKey.redisKey(), "account"), "绑定那一刻没有登录态就不留副本")
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

	// 绑定脚本现在也读登录态(抄账号副本),重建登录态的分支也在续租脚本里:两条路径同样要过守卫。
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	mr.FastForward(2 * time.Minute)
	renewal, err = Renew(ctx, rdb, testKey, time.Minute)
	require.NoError(t, err)
	require.Equal(t, Renewal{Kept: true, Account: "robot_0001", Restored: true}, renewal)
}
