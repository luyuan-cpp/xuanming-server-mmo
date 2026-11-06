package sessionfence

// 绑定了玩家之后的记录寿命(docs/design/data-overwrite-rollback-audit-20261018.md §4 login 第 2 条,
// docs/design/client-request-ordering.md §6.2「TTL」):绑定只由 Leave / Close 解除,不随在途操作的 ttl 到期;
// 登录提交(登录态续期)、进游戏入口、绑定都把记录续满 boundRecordTTL,Leave / Close 把它收回到 ttl。
//
// 改之前绑定与其余字段共用 ttl(6 分钟):绑定后 6 分钟内没有 LeaveGame / Disconnect,player 随记录一起过期,
// 同一连接再 EnterGame 就能绑上第二个角色;断线时 Close 也读不到绑定的玩家,带 player_id = 0 的 Disconnect 挂不上断线租约。

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// bindForLifetimeTest 走一遍入口与绑定点,让这条连接绑定 playerID(与后台链成功绑定时同样的两步)。
func bindForLifetimeTest(t *testing.T, ctx context.Context, rdb *redis.Client, playerID uint64) {
	t.Helper()
	outcome, gen, err := CheckEnterGame(ctx, rdb, testKey, playerID, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
	outcome, err = BindPlayer(ctx, rdb, testKey, playerID, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome)
}

func TestBindingOutlivesOperationTTL(t *testing.T) {
	// 绑定之后过了在途操作的 ttl,连接还在、没有 LeaveGame:换绑第二个角色在入口与绑定点都必须被拒,断线仍读得到绑定的玩家。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(testTTL + time.Minute)

	outcome, gen, err := CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome, "绑定不随 ttl 过期,同一连接不能换绑第二个角色")
	outcome, err = BindPlayer(ctx, rdb, testKey, 1002, gen)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome)

	bound, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	require.Equal(t, uint64(1001), bound, "断线时要读到绑定的玩家,由 Disconnect 补挂断线租约")
}

func TestEntryCheckKeepsBindingLifetime(t *testing.T) {
	// 同一角色重进、被拒的换角都会走入口续期:续期不得把绑定的记录压回 ttl。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(time.Hour)

	outcome, _, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOK, outcome, "同一角色重进照常放行")
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()), "入口把绑定的记录续满,不压回 ttl")

	outcome, _, err = CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome)
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()), "被拒的换角同样不压回 ttl")

	mr.FastForward(testTTL + time.Minute)
	outcome, _, err = CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome)
}

func TestEntryCheckDoesNotExtendClosedRecord(t *testing.T) {
	// 断线之后晚到的 EnterGame:入口照旧拒绝,也不能把已关闭的记录续成绑定的寿命(连接已经不在,记录只剩在途操作会读)。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	_, err := Close(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)

	outcome, _, err := CheckEnterGame(ctx, rdb, testKey, 1001, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeClosed, outcome)
	require.Equal(t, testTTL, mr.TTL(testKey.redisKey()))
}

func TestCommitLoginRenewsBoundRecord(t *testing.T) {
	// 登录态续期时同步续栅栏:游戏里在同一条连接上重新 Login,绑定了玩家的记录续满 boundRecordTTL。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(24 * time.Hour)
	require.Less(t, mr.TTL(testKey.redisKey()), boundRecordTTL)

	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	require.Equal(t, boundRecordTTL, mr.TTL(testKey.redisKey()), "登录态续上了,绑定的栅栏也要一起续上")
	require.Equal(t, 30*time.Minute, mr.TTL(testSessionKey))

	outcome, _, err := CheckEnterGame(ctx, rdb, testKey, 1002, testTTL)
	require.NoError(t, err)
	require.Equal(t, OutcomeOtherPlayer, outcome)
}

func TestCommitLoginLeavesUnboundRecordAlone(t *testing.T) {
	// 没绑定玩家时登录提交不改栅栏的寿命:这样的记录只服务在途操作,ttl 已经够(没有记录时也不建,见
	// TestCommitLoginWritesOnlySessionWhenOpen)。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	_, err := Leave(ctx, rdb, testKey, testTTL)
	require.NoError(t, err)
	mr.FastForward(time.Minute)

	require.NoError(t, CommitLogin(ctx, rdb, testKey, "robot_0001", 30*time.Minute))
	require.Equal(t, testTTL-time.Minute, mr.TTL(testKey.redisKey()))
}

func TestLeaveAndCloseReturnRecordToOperationTTL(t *testing.T) {
	// 绑定只由 Leave / Close 解除:两者都把记录收回到 ttl,之后按 ttl 正常过期,不留长寿记录。
	for _, tc := range []struct {
		name    string
		release func(ctx context.Context, rdb *redis.Client) (uint64, error)
	}{
		{"leave", func(ctx context.Context, rdb *redis.Client) (uint64, error) { return Leave(ctx, rdb, testKey, testTTL) }},
		{"close", func(ctx context.Context, rdb *redis.Client) (uint64, error) { return Close(ctx, rdb, testKey, testTTL) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rdb, mr := newTestRedis(t)
			bindForLifetimeTest(t, ctx, rdb, 1001)
			mr.FastForward(time.Hour)

			bound, err := tc.release(ctx, rdb)
			require.NoError(t, err)
			require.Equal(t, uint64(1001), bound)
			require.Equal(t, testTTL, mr.TTL(testKey.redisKey()))

			mr.FastForward(testTTL + time.Second)
			require.False(t, mr.Exists(testKey.redisKey()))
		})
	}
}

func TestBoundRecordOfLostDisconnectIsReclaimed(t *testing.T) {
	// gate 进程崩溃、Disconnect 没送到:绑定了玩家的记录按 boundRecordTTL 回收,不会永久留在登录域。
	ctx := context.Background()
	rdb, mr := newTestRedis(t)
	bindForLifetimeTest(t, ctx, rdb, 1001)
	mr.FastForward(boundRecordTTL - time.Second)
	require.True(t, mr.Exists(testKey.redisKey()))

	mr.FastForward(2 * time.Second)
	require.False(t, mr.Exists(testKey.redisKey()))
}
