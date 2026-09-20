package logic

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	pb "proto/guild"
)

// fakeFence 让用例精确控制闸门的三种回答,不必起 Redis。
type fakeFence struct {
	merging  bool
	err      error
	calls    int
	lastZone uint32
}

func (f *fakeFence) MergeInProgress(_ context.Context, zoneID uint32) (bool, error) {
	f.calls++
	f.lastZone = zoneID
	return f.merging, f.err
}

// TestCreateGuild_RefusedWhileZoneIsMerging 是本项修复的核心:
// 合服窗口里建出来的公会会留在一个即将下线的 zone,merge_zone 已经扫过它,
// 于是会长登进目标区看不到自己的公会。
func TestCreateGuild_RefusedWhileZoneIsMerging(t *testing.T) {
	fence := &fakeFence{merging: true}
	// repo/ids 都传 nil:闸门在 CreateGuild 的**第一行**,拒绝时不该碰到它们。
	// 这条断言本身就是"闸门必须在铸号之前"的证明 —— 顺序错了这里会 nil panic。
	l := NewGuildLogic(nil, nil, nil, fence, nil)

	resp, err := l.CreateGuild(context.Background(), &pb.CreateGuildRequest{
		PlayerId: 1001, Name: "merging-zone-guild", ZoneId: 7,
	})
	// 拒绝改回 tip(§11.1):"这个区正在合服,等几分钟"是可预期的运维窗口,
	// 回 gRPC 错误会被 serverbase 记成服务端故障,客户端还会进重连隔离。
	require.NoError(t, err)
	assert.Equal(t, constants.ErrZoneMerging, resp.GetErrorMessage().GetId())
	assert.Nil(t, resp.GetGuild(), "被拒绝的建帮不能回一个公会")
	assert.Equal(t, uint32(7), fence.lastZone, "查的必须是请求里的 zone")
}

// TestCreateGuild_FenceUnreadableFailsClosed:查不到闸门状态 ≠ 没有闸门。
// 与"确实在合服"回同一个码(玩家看到的都是"稍后再试"),两者靠诊断参数区分。
func TestCreateGuild_FenceUnreadableFailsClosed(t *testing.T) {
	fence := &fakeFence{err: errors.New("dial tcp: connection refused")}
	l := NewGuildLogic(nil, nil, nil, fence, nil)

	resp, err := l.CreateGuild(context.Background(), &pb.CreateGuildRequest{
		PlayerId: 1002, Name: "unreadable-fence", ZoneId: 7,
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrZoneMerging, resp.GetErrorMessage().GetId())
	assert.Equal(t, []string{"merge fence unreadable"}, resp.GetErrorMessage().GetParameters(),
		"读不到闸门与真的在合服必须能在日志里分开")
}

// TestMergeFenceTip_SkippedWhenNotConfigured:没配 MergeMarkerRedis 时闸门整体跳过,
// 建帮照常 —— 否则所有还没配这段的环境会因为一个可选加固而建不了公会。
func TestMergeFenceTip_SkippedWhenNotConfigured(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	assert.Nil(t, l.mergeFenceTip(context.Background(), 7))
}

// TestMergeFenceTip_ZoneZeroIsNotChecked:zone 0 没有对应的标记键(内部调用不带 zone),
// 查它没有意义。
func TestMergeFenceTip_ZoneZeroIsNotChecked(t *testing.T) {
	fence := &fakeFence{merging: true}
	l := NewGuildLogic(nil, nil, nil, fence, nil)
	assert.Nil(t, l.mergeFenceTip(context.Background(), 0))
	assert.Zero(t, fence.calls, "zone=0 不该产生一次 Redis 往返")
}

func TestMergeFenceTip_PassesWhenMarkerAbsent(t *testing.T) {
	fence := &fakeFence{merging: false}
	l := NewGuildLogic(nil, nil, nil, fence, nil)
	assert.Nil(t, l.mergeFenceTip(context.Background(), 7))
	assert.Equal(t, 1, fence.calls)
}

// ── RedisMergeFence:与 data_service 共享的键名契约 ──────────────

func TestMergeFenceKeyContract(t *testing.T) {
	// 这串字面量是与 data_service / tools/merge_zone 的契约,
	// 三处必须一字不差(见 merge_fence.go 顶部)。
	assert.Equal(t, "merge:in_progress:42", MergeFenceKey(42))
}

func TestRedisMergeFence_ExistenceIsTheWholeContract(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	fence := NewRedisMergeFence(rdb)
	ctx := context.Background()

	merging, err := fence.MergeInProgress(ctx, 3)
	require.NoError(t, err)
	assert.False(t, merging)

	// 值是 JSON,但闸门从不解析它 —— 键在就是封锁。
	require.NoError(t, mr.Set(MergeFenceKey(3), `{"started_at":1757000000}`))
	merging, err = fence.MergeInProgress(ctx, 3)
	require.NoError(t, err)
	assert.True(t, merging)

	// 值就算是一坨读不懂的东西也照样封锁:判据只有"存在"。
	require.NoError(t, mr.Set(MergeFenceKey(4), "not-json-at-all"))
	merging, err = fence.MergeInProgress(ctx, 4)
	require.NoError(t, err)
	assert.True(t, merging)

	// 工具跑完 DEL(或 TTL 到期)→ 放行。
	mr.Del(MergeFenceKey(3))
	merging, err = fence.MergeInProgress(ctx, 3)
	require.NoError(t, err)
	assert.False(t, merging)
}

func TestRedisMergeFence_RedisDownIsAnError(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	fence := NewRedisMergeFence(rdb)

	mr.Close()
	_, err := fence.MergeInProgress(context.Background(), 3)
	require.Error(t, err, "Redis 不可达必须报错,由调用方 fail-closed;绝不能静默返回 false")
}

// TestNewRedisMergeFence_NilClientYieldsNilFence 钉住 guild.go 的接线前提:
// 没配时构造函数返回 nil 指针,那边据此传一个 nil **接口**给 GuildLogic。
func TestNewRedisMergeFence_NilClientYieldsNilFence(t *testing.T) {
	assert.Nil(t, NewRedisMergeFence(nil))
}
