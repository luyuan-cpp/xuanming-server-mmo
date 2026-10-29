package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// AcquirePlayerLock 拿锁失败时的回滚:SETNX 的结果不明(服务端已执行、客户端没拿到成功回包)时,
// 必须按 token 把可能已落地的锁释放掉,否则该玩家的写要等 PlayerLockTTLSec 过期才恢复;
// 按 token 释放又保证别人持有的锁不受影响。dev 模式下 mapping 锁与数据锁在同一个 miniredis 里,两把都能直接看到;
// mapping 侧的用例另起一个 miniredis 承载 mapping 锁(见 newRouterWithMappingSetNXHook)。
//
// 数据锁被别人持有、mapping 冲突两个用例在修复前的代码上也通过:它们守的是「只删自己的锁」,
// 防止以后有人把回滚改成无条件 DEL,不是红绿回归。

var errSetNXReplyLost = errors.New("connection reset after SETNX was sent")

// lostReplySetNXClient 让数据侧 SETNX 真正写入,再把回包换成错误:模拟「服务端已执行、回包丢失」。
type lostReplySetNXClient struct {
	*goredis.Client
}

func (c *lostReplySetNXClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.BoolCmd {
	if err := c.Client.SetNX(ctx, key, value, expiration).Err(); err != nil {
		return goredis.NewBoolResult(false, err)
	}
	return goredis.NewBoolResult(false, errSetNXReplyLost)
}

// retriedSetNXClient 让数据侧 SETNX 真正写入,再回 false:模拟客户端对断连自动重试,第二次撞上第一次写入的自己的 token。
type retriedSetNXClient struct {
	*goredis.Client
}

func (c *retriedSetNXClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.BoolCmd {
	if err := c.Client.SetNX(ctx, key, value, expiration).Err(); err != nil {
		return goredis.NewBoolResult(false, err)
	}
	return goredis.NewBoolResult(false, nil)
}

// 数据侧 SETNX 已写入却回错误:两把锁都按 token 释放,随后立刻能重新拿到。
func TestAcquirePlayerLock_DataSetNXReplyLostReleasesBothLocks(t *testing.T) {
	r, mr := newTestRouter(t)
	devClient := devClientOf(t, r)

	token, ok, err := r.AcquirePlayerLock(context.Background(), &lostReplySetNXClient{Client: devClient}, 791)
	require.ErrorIs(t, err, errSetNXReplyLost)
	assert.False(t, ok)
	assert.Empty(t, token)

	requireNoPlayerLocks(t, mr, 791)
	_, ok, err = r.AcquirePlayerLock(context.Background(), devClient, 791)
	require.NoError(t, err)
	assert.True(t, ok, "lock must be re-acquirable right away, not after the TTL")
}

// 数据侧 SETNX 回 false,但锁其实是自己这次写入的(重试撞上自己):同样两把都释放。
func TestAcquirePlayerLock_DataSetNXRetryHitsOwnTokenReleasesBothLocks(t *testing.T) {
	r, mr := newTestRouter(t)
	devClient := devClientOf(t, r)

	token, ok, err := r.AcquirePlayerLock(context.Background(), &retriedSetNXClient{Client: devClient}, 792)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, token)

	requireNoPlayerLocks(t, mr, 792)
	_, ok, err = r.AcquirePlayerLock(context.Background(), devClient, 792)
	require.NoError(t, err)
	assert.True(t, ok, "lock must be re-acquirable right away, not after the TTL")
}

// 数据锁真被别人持有:回滚只释放自己的 mapping 锁,别人的数据锁原样保留。
func TestAcquirePlayerLock_DataLockHeldByOtherIsUntouched(t *testing.T) {
	r, mr := newTestRouter(t)
	devClient := devClientOf(t, r)
	require.NoError(t, mr.Set(PlayerDataLockKey(793), "other-owner"))

	token, ok, err := r.AcquirePlayerLock(context.Background(), devClient, 793)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, token)

	got, err := mr.Get(PlayerDataLockKey(793))
	require.NoError(t, err)
	assert.Equal(t, "other-owner", got, "another owner's data lock must survive the rollback")
	assert.False(t, mr.Exists(mappingPlayerLockKey(793)), "our own mapping lock must be rolled back")
}

// mapping 锁冲突:失败方的回滚释放不能动现任持锁者的两把锁。
func TestAcquirePlayerLock_MappingConflictLeavesHolderLocks(t *testing.T) {
	r, mr := newTestRouter(t)
	devClient := devClientOf(t, r)
	holder, ok, err := r.AcquirePlayerLock(context.Background(), devClient, 794)
	require.NoError(t, err)
	require.True(t, ok)

	token, ok, err := r.AcquirePlayerLock(context.Background(), devClient, 794)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, token)

	for _, key := range []string{mappingPlayerLockKey(794), PlayerDataLockKey(794)} {
		got, err := mr.Get(key)
		require.NoError(t, err, key)
		assert.Equal(t, holder, got, "holder's lock %s must survive the loser's rollback", key)
	}
}

// mappingSetNXHook 挂在 mapping 侧的 go-zero 客户端上:SETNX 照常执行,再把回包改掉。
// replyErr 非 nil 时改成这个错误(回包丢失);为 nil 时改成 false(重试撞上自己写入的 token)。
// tamper=false 时原样放行;seen 用于自检钩子确实装在实际使用的客户端上。
type mappingSetNXHook struct {
	replyErr error
	tamper   bool
	seen     bool
}

func (h *mappingSetNXHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *mappingSetNXHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		h.seen = true
		err := next(ctx, cmd)
		boolCmd, ok := cmd.(*goredis.BoolCmd)
		if err != nil || !ok || !h.tamper || !isSetNXCmd(cmd) {
			return err
		}
		if h.replyErr != nil {
			boolCmd.SetErr(h.replyErr)
			return h.replyErr
		}
		boolCmd.SetVal(false)
		return nil
	}
}

func (h *mappingSetNXHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// isSetNXCmd 认出 go-redis SetNX 带过期时间发出的 SET key value EX n NX。
func isSetNXCmd(cmd goredis.Cmder) bool {
	args := cmd.Args()
	if cmd.Name() != "set" || len(args) == 0 {
		return false
	}
	last, _ := args[len(args)-1].(string)
	return strings.EqualFold(last, "nx")
}

// newRouterWithMappingSetNXHook 在 newTestRouter 之上把 mapping 客户端换成挂了 h 的 go-zero 客户端,
// mapping 锁落在另起的 mappingMR 里,数据锁仍在 dataMR。
// go-zero 按地址缓存客户端、钩子只在首次创建时装上:新 miniredis 的端口若恰好被本进程用过,拿到的会是旧客户端,
// 所以先 PING 自检钩子确实生效。
func newRouterWithMappingSetNXHook(t *testing.T, h *mappingSetNXHook) (r *Router, dataMR, mappingMR *miniredis.Miniredis) {
	t.Helper()
	r, dataMR = newTestRouter(t)
	mappingMR = miniredis.RunT(t)
	r.mappingRedis = redis.New(mappingMR.Addr(), redis.WithHook(h))
	require.True(t, r.mappingRedis.PingCtx(context.Background()), "mapping miniredis must be reachable")
	require.True(t, h.seen, "测试钩子必须装在实际使用的 mapping 客户端上(go-zero 按地址缓存客户端,端口被复用时会拿到旧客户端)")
	return r, dataMR, mappingMR
}

// mapping 侧 SETNX 已写入却回错误:mapping 锁按 token 释放,数据锁根本没去拿;放开钩子后立刻能重新拿到。
func TestAcquirePlayerLock_MappingSetNXReplyLostReleasesMappingLock(t *testing.T) {
	h := &mappingSetNXHook{replyErr: errSetNXReplyLost, tamper: true}
	r, dataMR, mappingMR := newRouterWithMappingSetNXHook(t, h)
	devClient := devClientOf(t, r)

	token, ok, err := r.AcquirePlayerLock(context.Background(), devClient, 795)
	require.ErrorIs(t, err, errSetNXReplyLost)
	assert.False(t, ok)
	assert.Empty(t, token)

	assert.False(t, mappingMR.Exists(mappingPlayerLockKey(795)), "mapping-side lock written before the reply was lost must be released")
	assert.False(t, dataMR.Exists(PlayerDataLockKey(795)), "data-side lock must not be taken after the mapping lock failed")
	h.tamper = false
	_, ok, err = r.AcquirePlayerLock(context.Background(), devClient, 795)
	require.NoError(t, err)
	assert.True(t, ok, "lock must be re-acquirable right away, not after the TTL")
}

// mapping 侧 SETNX 回 false,但锁其实是自己这次写入的(重试撞上自己):同样按 token 释放。
func TestAcquirePlayerLock_MappingSetNXRetryHitsOwnTokenReleasesMappingLock(t *testing.T) {
	h := &mappingSetNXHook{tamper: true}
	r, dataMR, mappingMR := newRouterWithMappingSetNXHook(t, h)
	devClient := devClientOf(t, r)

	token, ok, err := r.AcquirePlayerLock(context.Background(), devClient, 796)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, token)

	assert.False(t, mappingMR.Exists(mappingPlayerLockKey(796)), "our own mapping-side lock must not be left behind until the TTL")
	assert.False(t, dataMR.Exists(PlayerDataLockKey(796)))
	h.tamper = false
	_, ok, err = r.AcquirePlayerLock(context.Background(), devClient, 796)
	require.NoError(t, err)
	assert.True(t, ok, "lock must be re-acquirable right away, not after the TTL")
}

// lockAcquireFailure 的四种组合:拿锁错误优先;只是没抢到而释放失败时必须回错误(不能当成普通锁冲突)。
func TestLockAcquireFailure(t *testing.T) {
	r, _ := newTestRouter(t)
	errAcquire := errors.New("acquire failed")
	errRelease := errors.New("release failed")

	assert.NoError(t, r.lockAcquireFailure(1, "data", nil, nil), "plain lock conflict")

	err := r.lockAcquireFailure(1, "data", errAcquire, nil)
	assert.ErrorIs(t, err, errAcquire)

	err = r.lockAcquireFailure(1, "mapping", nil, errRelease)
	require.Error(t, err, "conflict whose rollback release failed must not look like a plain conflict")
	assert.ErrorIs(t, err, errRelease)

	err = r.lockAcquireFailure(1, "data", errAcquire, errRelease)
	assert.ErrorIs(t, err, errAcquire)
	assert.NotErrorIs(t, err, errRelease, "the acquire error is returned; the release error is only logged")
}
