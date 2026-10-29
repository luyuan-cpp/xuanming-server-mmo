package saveoutbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"shared/savebarrier"
)

// 显式要求时缺任一前置即失败，不把普通单元的跳过计作真实Redis证据。
// 仅操作本次随机玩家的hash/member，不FLUSH、不删除其他member或玩家资产。
func TestRealRedisPendingSaveOutboxSurvivesWorkerRestart(t *testing.T) {
	if os.Getenv("SAVE_OUTBOX_REQUIRE_REDIS") != "1" {
		t.Skip("需显式专用dev Redis；未计作真实Redis验收")
	}
	if os.Getenv("SAVE_OUTBOX_REDIS_MODE") != "dev" || os.Getenv("SAVE_OUTBOX_REDIS_DEDICATED") != "1" {
		t.Fatal("真实补投验收必须显式专用dev Redis，拒绝生产")
	}
	addr := os.Getenv("SAVE_OUTBOX_REDIS_ADDR")
	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err, "需显式本地Redis地址")
	ip := net.ParseIP(host)
	require.True(t, strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback(), "只允许本机dev Redis")
	rc := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("SAVE_OUTBOX_REDIS_PASSWORD"),
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rc.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Fatal("无法连接显式dev Redis")
	}
	var raw [8]byte
	_, err = rand.Read(raw[:])
	require.NoError(t, err)
	playerID := binary.LittleEndian.Uint64(raw[:]) &^ (BucketCount - 1)
	if playerID == 0 {
		playerID = BucketCount
	}
	zone := uint32(900000) + uint32(binary.LittleEndian.Uint16(raw[:2]))%10000
	hash := savebarrier.Key(playerID)
	index := pendingIndexKey(zone, 0)
	exists, err := rc.Exists(ctx, hash, index).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "随机测试身份或索引发生碰撞时拒绝触碰已有状态")
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = rc.Del(clean, hash).Err()
		_ = rc.ZRem(clean, index, strconv.FormatUint(playerID, 10)).Err()
	})
	in := testIntent(t, playerID, 7, 9, zone)
	// 已过首见宽限的意图(首见只登记不补投的行为由 replay_bounds_test.go 覆盖)。
	storeObservedIntent(t, rc, playerID, zone, in)
	pub := &recordingPublisher{failAt: 2}
	cfg := Config{ZoneID: zone, Batch: 16, Interval: 10 * time.Millisecond, Budget: 100 * time.Millisecond, RetryAfter: time.Second}
	r, err := New(rc, rc, pub, nil, openRouteGate(), cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	firstTime := time.Now()
	r.now = func() time.Time { return firstTime }
	r.jitter = func() time.Duration { return 0 }
	n, err := r.Tick(ctx)
	require.Error(t, err)
	require.Equal(t, 1, n)
	// 只有补投worker重建；Redis里的原始两表意图未删除。
	pub2 := &recordingPublisher{}
	r2, err := New(rc, rc, pub2, nil, openRouteGate(), cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	r2.now = func() time.Time { return firstTime.Add(2 * time.Second) }
	r2.jitter = func() time.Duration { return 0 }
	n, err = r2.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, in.first, pub2.tasks[0].body)
	require.Equal(t, in.second, pub2.tasks[1].body)
	require.True(t, rc.HExists(ctx, hash, fieldVersion).Val(), "真实Redis也不能以broker确认代替SQL ACK")
}
