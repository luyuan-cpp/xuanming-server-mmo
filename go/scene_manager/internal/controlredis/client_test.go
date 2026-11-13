package controlredis

import (
	"context"
	"testing"

	"shared/clusterredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New(clusterredis.Config{Addrs: []string{mr.Addr()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// ctlKey 拼一个 zone 的控制域键(键形同 logic 包的场景键,只用来让守卫认出 tag)。
func ctlKey(zone uint32, suffix string) string {
	return "scene:" + redisslot.ZoneControlTag(redisslot.ControlFamilySceneControl, zone) + ":" + suffix
}

// 缺键的读法与换句柄之前的 go-zero 句柄相同:logic 包的调用点按这些形状区分「不存在」与「读失败」。
func TestMissingKeysReadLikeTheGoZeroHandle(t *testing.T) {
	c, _ := newTestClient(t)
	k := ctlKey(1, "1:node")

	v, err := c.Get(k)
	require.NoError(t, err)
	assert.Equal(t, "", v, "Get 把缺键翻成空串")

	_, err = c.Hget(k, "f")
	assert.ErrorIs(t, err, redis.Nil, "Hget 不翻缺键")
	_, err = c.Zscore(k, "m")
	assert.ErrorIs(t, err, redis.Nil, "Zscore 不翻缺键")

	exists, err := c.Exists(k)
	require.NoError(t, err)
	assert.False(t, exists)

	vals, err := c.Mget(k, ctlKey(1, "2:node"))
	require.NoError(t, err)
	assert.Equal(t, []string{"", ""}, vals, "Mget 的缺项是空串")
	fields, err := c.Hmget(k, "a", "b")
	require.NoError(t, err)
	assert.Equal(t, []string{"", ""}, fields, "Hmget 的缺项是空串")

	members, err := c.Smembers(k)
	require.NoError(t, err)
	assert.Empty(t, members)
}

// 有序集合的分数截成 int64,Zadd 回「是不是新成员」,分页从第 0 页起。
func TestSortedSetShapes(t *testing.T) {
	c, mr := newTestClient(t)
	k := ctlKey(1, "load")
	_, err := mr.ZAdd(k, 2.9, "a")
	require.NoError(t, err)
	_, err = mr.ZAdd(k, 7, "b")
	require.NoError(t, err)

	pairs, err := c.ZrangeWithScores(k, 0, -1)
	require.NoError(t, err)
	assert.Equal(t, []Pair{{Key: "a", Score: 2}, {Key: "b", Score: 7}}, pairs)
	score, err := c.Zscore(k, "a")
	require.NoError(t, err)
	assert.Equal(t, int64(2), score)

	added, err := c.Zadd(k, 10, "c")
	require.NoError(t, err)
	assert.True(t, added, "新成员")
	added, err = c.Zadd(k, 11, "c")
	require.NoError(t, err)
	assert.False(t, added, "只改分数不是新成员")

	page, err := c.ZrangebyscoreWithScoresAndLimit(k, 0, 100, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, []Pair{{Key: "b", Score: 7}}, page, "第 1 页(从 0 起)、每页 1 个")
	none, err := c.ZrangebyscoreWithScoresAndLimit(k, 0, 100, 0, 0)
	require.NoError(t, err)
	assert.Nil(t, none, "size <= 0 什么都不读")
}

// 单键命令可以不带 tag(选主锁这类单键族)。
func TestSingleKeyCommandsMayBeUntagged(t *testing.T) {
	c, mr := newTestClient(t)
	require.NoError(t, mr.Set("scene_manager:single", "v"))
	v, err := c.Get("scene_manager:single")
	require.NoError(t, err)
	assert.Equal(t, "v", v)
}

// 控制句柄不收玩家域的键:键发错句柄当场报错,而不是在开发环境的同一个实例上碰巧读到。
func TestGuardRejectsPlayerDomainKeys(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.Get("player:{42}:location")
	assert.ErrorIs(t, err, clusterredis.ErrWrongDomainTag)
	_, err = c.Eval("return 1", []string{"player:{42}:location"})
	assert.ErrorIs(t, err, clusterredis.ErrWrongDomainTag)
}

// 多键命令的键必须是同一个 tag:两个 zone 的键碰巧同槽也不行,不带 tag 的键也不行。
func TestGuardRejectsMultiKeyCommandsAcrossTags(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.Mget(ctlKey(1, "1:node"), ctlKey(2, "1:node"))
	assert.ErrorIs(t, err, clusterredis.ErrMixedTags)
	_, err = c.Del(ctlKey(1, "1:node"), "untagged")
	assert.ErrorIs(t, err, clusterredis.ErrUntaggedKey)
}

// 管道里有 EVAL 就整批不发(集群客户端会在 EOF / 超时后整批重发管道);同一条 EVAL 单独发照常执行。
func TestPipelineRefusesScriptsButDoSendsThem(t *testing.T) {
	c, mr := newTestClient(t)
	ctx := context.Background()
	k := ctlKey(1, "load")
	err := c.PipelinedCtx(ctx, func(p redis.Pipeliner) error {
		p.ZAdd(ctx, k, redis.Z{Score: 1, Member: "a"})
		p.Eval(ctx, "return redis.call('ZADD', KEYS[1], 1, 'b')", []string{k})
		return nil
	})
	assert.ErrorIs(t, err, clusterredis.ErrNonIdempotentInPipeline)
	assert.False(t, mr.Exists(k), "整批不发:管道里的 ZADD 也没执行")

	got, err := c.Do(ctx, "EVAL", "return redis.call('ZADD', KEYS[1], 1, 'b')", 1, k).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), got)
}

// countingHook 只数 GET:连接建立时的握手命令也会经过钩子,不计。
type countingHook struct{ gets *int }

func (h countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			*h.gets++
		}
		return next(ctx, cmd)
	}
}

func (h countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// AddHook 加的钩子排在守卫之后:守卫拒掉的命令到不了它(单测注入故障的钩子看到的都是会真发出去的命令)。
func TestAddHookRunsAfterTheGuard(t *testing.T) {
	c, _ := newTestClient(t)
	gets := 0
	c.AddHook(countingHook{gets: &gets})
	_, err := c.Get(ctlKey(1, "1:node"))
	require.NoError(t, err)
	_, err = c.Get("player:{1}:location")
	require.Error(t, err)
	assert.Equal(t, 1, gets)
}
