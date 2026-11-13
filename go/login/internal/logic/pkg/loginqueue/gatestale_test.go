package loginqueue

import (
	"context"
	"testing"
	"time"

	"shared/gatepick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 每台判为停写的 gate 一条标记,值 "1",过期 gatepick.StaleMarkTTL;没判停写的不写。
func TestPublishStaleMarks_WritesOneExpiringMarkPerGate(t *testing.T) {
	rdb, mr := newDrainRedis(t)

	require.NoError(t, PublishStaleMarks(context.Background(), rdb, testMarkZone, []uint32{2, 5}))

	for _, id := range []uint32{2, 5} {
		got, err := mr.Get(gatepick.StaleKey(testMarkZone, id))
		require.NoError(t, err)
		assert.Equal(t, "1", got)
		assert.Equal(t, gatepick.StaleMarkTTL, mr.TTL(gatepick.StaleKey(testMarkZone, id)), "标记必须带过期时间,恢复之后靠它自然消失")
	}
	assert.False(t, mr.Exists(gatepick.StaleKey(testMarkZone, 3)))
}

// 续写把过期时间拉回满值;停止续写之后标记自然过期(从不主动删)。
func TestPublishStaleMarks_RefreshExtendsAndSilenceExpires(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	ctx := context.Background()

	require.NoError(t, PublishStaleMarks(ctx, rdb, testMarkZone, []uint32{2}))
	mr.FastForward(gatepick.ReportPeriod)
	require.NoError(t, PublishStaleMarks(ctx, rdb, testMarkZone, []uint32{2}))
	assert.Equal(t, gatepick.StaleMarkTTL, mr.TTL(gatepick.StaleKey(testMarkZone, 2)))

	mr.FastForward(gatepick.StaleMarkTTL + time.Second)
	assert.False(t, mr.Exists(gatepick.StaleKey(testMarkZone, 2)), "gate 恢复后不再续写,标记过期消失")
}

// 没有要写的、或没有 Redis 时什么都不做,不报错。
func TestPublishStaleMarks_NothingToWrite(t *testing.T) {
	rdb, mr := newDrainRedis(t)

	require.NoError(t, PublishStaleMarks(context.Background(), rdb, testMarkZone, nil))
	require.NoError(t, PublishStaleMarks(context.Background(), nil, testMarkZone, []uint32{1}))
	assert.Empty(t, mr.Keys())
}

// Redis 不可用时返回错误(由调用方记日志),不 panic。
func TestPublishStaleMarks_RedisFailureIsReported(t *testing.T) {
	rdb, mr := newDrainRedis(t)
	mr.Close()

	assert.Error(t, PublishStaleMarks(context.Background(), rdb, testMarkZone, []uint32{1}))
}
