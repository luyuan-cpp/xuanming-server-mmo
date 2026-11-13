package data

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 榜维护锁在「拿锁结局未知」时的收拾(docs/design/infra-reconnect-overview.md §4.1 R8)。

// lostSetReplyHook 让第一条带 key 的 SET 照常执行,再把结果换成一个错误:命令在服务端生效了,回包丢了。
// 榜维护锁的句柄不自动重发,调用方看到的就是这个错误。
type lostSetReplyHook struct {
	key   string
	once  sync.Once
	fired bool
}

func (h *lostSetReplyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *lostSetReplyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *lostSetReplyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err != nil || cmd.Name() != "set" || !slices.Contains(cmd.Args(), any(h.key)) {
			return err
		}
		lost := false
		h.once.Do(func() {
			lost = true
			h.fired = true
		})
		if !lost {
			return nil
		}
		lostErr := errors.New("simulated: reply lost after the server applied the command")
		cmd.SetErr(lostErr)
		return lostErr
	}
}

// 拿锁的 SET NX 写进去了、回包丢了:acquireRankLock 报错之前按同一个令牌做条件删除,锁不会空占到 TTL。
// 修复前红:锁里留着这次的令牌,此后 5 分钟重建榜与合服并榜都等到超时。
func TestAcquireRankLock_UnknownOutcomeReleasesItsOwnLock(t *testing.T) {
	repo, mr := newRankRepo(t)
	const zone uint32 = 7
	hook := &lostSetReplyHook{key: guildRankLockKey(zone)}
	repo.rdb.AddHook(hook)

	_, err := repo.acquireRankLock(context.Background(), zone)

	require.Error(t, err)
	require.True(t, hook.fired, "前提不成立:没有拦到拿锁的 SET NX")
	assert.False(t, mr.Exists(guildRankLockKey(zone)), "结局未知时写进去的锁要按令牌删掉")

	release, err := repo.acquireRankLock(context.Background(), zone)
	require.NoError(t, err, "下一次照常拿得到")
	release()
	assert.False(t, mr.Exists(guildRankLockKey(zone)))
}

// 结局未知、而锁其实是别人的(这次 SET NX 没写进去):条件删除按令牌比对,别人的锁原样留着。
func TestAcquireRankLock_UnknownOutcomeLeavesAnotherHoldersLock(t *testing.T) {
	repo, _ := newRankRepo(t)
	ctx := context.Background()
	const zone uint32 = 7
	require.NoError(t, repo.rdb.Set(ctx, guildRankLockKey(zone), "someone-else", time.Minute).Err())
	hook := &lostSetReplyHook{key: guildRankLockKey(zone)}
	repo.rdb.AddHook(hook)

	_, err := repo.acquireRankLock(ctx, zone)

	require.Error(t, err)
	require.True(t, hook.fired, "前提不成立:没有拦到拿锁的 SET NX")
	assert.Equal(t, "someone-else", repo.rdb.Get(ctx, guildRankLockKey(zone)).Val(), "别人的锁不能动")
}
