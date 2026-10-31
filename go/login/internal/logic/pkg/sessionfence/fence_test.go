package sessionfence

// gate 连接会话栅栏的单测(docs/design/client-request-ordering.md §6.2)。重点是「双边握手」的两种顺序:
// 关闭在前 → 绑定看到 closed;绑定在前 → 关闭读到 player。两种都必须收敛到「有人补发断线租约」。

import (
	"context"
	"errors"
	"testing"
	"time"

	login_proto_common "proto/common/base"
	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const testTTL = 6 * time.Minute

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

var testKey = Key{GateInstanceID: "gate-uuid-1", Incarnation: 7}

func TestKeyOfRejectsConnectionWithoutIdentity(t *testing.T) {
	_, ok := KeyOf(&login_proto_common.SessionDetails{GateInstanceId: "g", SessionIncarnation: 0})
	require.False(t, ok, "没有会话代次的连接没有身份")
	_, ok = KeyOf(&login_proto_common.SessionDetails{SessionIncarnation: 3})
	require.False(t, ok, "缺 gate_instance_id 的连接没有身份")
	_, ok = KeyOf(nil)
	require.False(t, ok)
	key, ok := KeyOf(&login_proto_common.SessionDetails{GateInstanceId: "g", SessionIncarnation: 3})
	require.True(t, ok)
	require.Equal(t, Key{GateInstanceID: "g", Incarnation: 3}, key)
}

func TestKeyIsPerGateInstanceAndIncarnation(t *testing.T) {
	// 同一 session_id 在 gate 重启后会被重新发出;栅栏键不含 session_id,上一个进程的关闭标记不得误拒新连接。
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, err := Close(ctx, rdb, Key{GateInstanceID: "gate-old", Incarnation: 1}, testTTL)
	require.NoError(t, err)
	err = CommitLogin(ctx, rdb, Key{GateInstanceID: "gate-new", Incarnation: 1}, "acc", time.Minute)
	require.NoError(t, err, "另一个 gate 进程的关闭标记不影响本连接")
}

func TestKeyStringIsSharedConnectionIdentity(t *testing.T) {
	// login_session 键(loginsession.RefOf)与栅栏键共用这一种连接身份。
	require.Equal(t, "gate-uuid-1:7", testKey.String())
	require.Equal(t, "login:sessfence:{gate-uuid-1:7}", testKey.redisKey())
}

const testSessionKey = "login_session:{gate-uuid-1:7}"

func TestCommitLoginWritesOnlySessionWhenOpen(t *testing.T) {
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))

	got, err := mr.Get(testSessionKey)
	require.NoError(t, err)
	require.Equal(t, "robot_0001", got)
	require.Equal(t, 30*time.Minute, mr.TTL(testSessionKey))
	require.Equal(t, []string{testSessionKey}, mr.Keys(), "设备集在另一个槽,不在这段脚本里;连接没关时也不建栅栏记录")
}

func TestCommitLoginAfterCloseWritesNothing(t *testing.T) {
	// Disconnect 抢跑仍在途的 Login:Login 晚落地时不得留下 login_session 与设备集残留。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	_, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)

	err = CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute)
	require.True(t, errors.Is(err, ErrClosed), "err=%v", err)
	require.False(t, mr.Exists(testSessionKey), "关闭之后不得写 login_session")
}

func TestHandshakeCloseBeforeBind(t *testing.T) {
	// EnterGame 已受理、后台链还没绑定时断线(Disconnect 带 player_id = 0):绑定必须看到 closed,由后台链补发断线租约。
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	outcome, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)

	bound, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Zero(t, bound, "关闭时还没有绑定任何玩家")

	outcome, err = BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeClosed, outcome, "关闭在前,绑定必须看到 closed")
}

func TestHandshakeBindBeforeClose(t *testing.T) {
	// 绑定在前:Disconnect 带 player_id = 0 也必须从栅栏读到玩家,由 Disconnect 补发断线租约。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	require.Equal(t, testTTL, mr.TTL(testKey.redisKey()))

	bound, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound, "绑定在前,关闭必须读到玩家")

	outcome, err = Recheck(ctx, rdb, testKey, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeClosed, outcome, "进场前复查看到已关闭,不再进场")
}

func TestLeaveDuringEnterChainAbortsBind(t *testing.T) {
	// EnterGame 之后 LeaveGame:后台链在 LeaveGame 之后才绑定时必须中止,不得把会话写回去。
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)

	bound, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Zero(t, bound)

	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeLeft, outcome)

	outcome, err = Recheck(ctx, rdb, testKey, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeLeft, outcome)
}

func TestLeaveUnbindsAndReturnsBoundPlayer(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	_, err = BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)

	bound, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound)

	// 离开之后同一条连接换角:入口拿到新的 gen,新角色可以绑定。
	outcome, gen2, err := CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	require.Equal(t, gen+1, gen2)
	outcome, err = BindPlayer(ctx, rdb, testKey, 1002, gen2, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
}

func TestSecondRoleOnSameConnectionIsRejected(t *testing.T) {
	// 同一条连接连发 EnterGame(p1)、EnterGame(p2):先绑定的赢,后一个在入口或绑定时被拒,不会一会话两角色。
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, gen1, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	_, gen2, err := CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)

	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen1, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	outcome, err = BindPlayer(ctx, rdb, testKey, 1002, gen2, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome)

	outcome, _, err = CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome, "已绑定别的角色,入口直接拒绝")
	outcome, _, err = CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome, "同一角色重进照常放行")
}

func TestRebindSamePlayerIsIdempotent(t *testing.T) {
	ctx := context.Background()
	rdb, _ := newTestRedis(t)
	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
		require.NoError(t, err)
		require.Equal(t, OutcomeOK, outcome)
	}
}

func TestCheckEnterGameRefreshesRecordSoBindSeesEntryGen(t *testing.T) {
	// LeaveGame 在 T0 写 gen = 1;玩家在选角界面停到快过期才 EnterGame。入口必须续期,
	// 否则记录在绑定之前过期,绑定读到 gen = 0 误判「期间已离开」,把合法写入的会话删掉。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	_, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	mr.FastForward(testTTL - time.Second)

	outcome, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	require.Equal(t, uint64(1), gen)
	require.Equal(t, testTTL, mr.TTL(testKey.redisKey()), "入口必须把记录续期到 ttl")

	mr.FastForward(5 * time.Second) // 不续期的话,记录此时已过期
	outcome, err = BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
}

func TestLeaveAfterOriginalExpiryIsStillDetected(t *testing.T) {
	// 反向:入口记下 gen = 1 之后,原记录到期、又来一次 LeaveGame。不续期时 HINCRBY 从空记录重建出 gen = 1,
	// 恰好等于入口的旧值,这次离开被漏掉;续期之后它在原记录上递增到 2,绑定识别出「期间已离开」。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	_, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	mr.FastForward(testTTL - time.Second)

	_, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	mr.FastForward(5 * time.Second)
	_, err = Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)

	outcome, err := BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeLeft, outcome)
}

func TestCommitLoginRejectsNonPositiveTTL(t *testing.T) {
	// SessionExpireMin 漏配为 0 时脚本的 SET ... PX 0 会被 Redis 拒绝;这里在进脚本之前显式报错,且什么都不写。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	for _, ttl := range []time.Duration{0, -time.Second} {
		err := CommitLogin(ctx, rdb, testKey, "robot_0001", ttl)
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrClosed))
	}
	require.False(t, mr.Exists(testSessionKey))
}

// 栅栏的每段脚本都走生产的玩家域构造器:守卫在发送前拒绝不带 tag、跨槽的键。CommitLogin 是唯一的多键脚本,
// 它的两个键必须同槽;其余脚本只碰栅栏这一个键。上了 Cluster 这些才不会收到 CROSSSLOT。
func TestEveryFenceScriptPassesTheClusterGuard(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rdb.Close() })
	require.Equal(t, redisslot.HashSlot(testKey.redisKey()), redisslot.HashSlot(testSessionKey))

	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", time.Minute))
	outcome, gen, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	outcome, err = BindPlayer(ctx, rdb, testKey, 1001, gen, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	outcome, err = Recheck(ctx, rdb, testKey, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	bound, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound)
	_, err = Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.ErrorIs(t, CommitLogin(ctx, rdb, testKey, "robot_0001", time.Minute), ErrClosed)
}
