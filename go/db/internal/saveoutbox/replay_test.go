package saveoutbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	dbconfig "db/internal/config"
	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
	dbpb "proto/db"
	"shared/placement"
	"shared/savebarrier"
)

type publishedTask struct{ topic, key, body string }

type recordingPublisher struct {
	tasks  []publishedTask
	failAt int
	hook   func()
	// calls 计所有调用(含失败)，用来断言暂停期间没有再碰 broker。
	calls int
}

func (p *recordingPublisher) Publish(ctx context.Context, topic, key string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.calls++
	if p.hook != nil {
		p.hook()
	}
	if p.failAt > 0 && len(p.tasks)+1 == p.failAt {
		return errors.New("注入broker确认失败")
	}
	p.tasks = append(p.tasks, publishedTask{topic, key, string(body)})
	return nil
}

// routeGateStub 是读方就绪闸(RouteGate)的替身：零值关着，open=true 放行。夹具缺省放行(openRouteGate)，
// 只有 r2-rr#5 的用例(replay_route_gate_test.go)拨动它。Tick 在用例的 goroutine 里同步执行，不需要加锁。
type routeGateStub struct{ open bool }

func (g *routeGateStub) RouteGateOpen() bool { return g.open }

func openRouteGate() *routeGateStub { return &routeGateStub{open: true} }

const fixtureID uint64 = 64

func testIntent(t *testing.T, id, epoch, seq uint64, zone uint32) intent {
	t.Helper()
	in := intent{version: savebarrier.Version(epoch, seq), topic: topicFor(zone, 1)}
	for i, row := range []proto.Message{&databasepb.PlayerDatabase{PlayerId: id}, &databasepb.PlayerDatabase_1{PlayerId: id}} {
		body, err := proto.Marshal(row)
		require.NoError(t, err)
		raw, err := proto.Marshal(&dbpb.DBTask{Key: id, Op: dbpb.DBOp_DB_OP_WRITE, MsgType: string(row.ProtoReflect().Descriptor().FullName()),
			Body: body, OwnerEpoch: epoch, SaveSeq: seq, TaskId: "原始任务身份"})
		require.NoError(t, err)
		if i == 0 {
			in.first = string(raw)
		} else {
			in.second = string(raw)
		}
	}
	return in
}

func storeIntent(t *testing.T, rc redis.Cmdable, id uint64, zone uint32, in intent) {
	t.Helper()
	require.NoError(t, rc.HSet(context.Background(), savebarrier.Key(id), "pending", in.version,
		fieldVersion, in.version, fieldTopic, in.topic, fieldFirst, in.first, fieldSecond, in.second).Err())
	require.NoError(t, rc.ZAdd(context.Background(), pendingIndexKey(zone, id%BucketCount), redis.Z{Score: 0, Member: strconv.FormatUint(id, 10)}).Err())
}

// storeObservedIntent 模拟补投器已经首次登记过该版本(过了 FirstReplayAfter 宽限):下一次到期即真正补投。
func storeObservedIntent(t *testing.T, rc redis.Cmdable, id uint64, zone uint32, in intent) {
	t.Helper()
	storeIntent(t, rc, id, zone, in)
	require.NoError(t, rc.HSet(context.Background(), savebarrier.Key(id), fieldAttemptVersion, in.version, fieldAttempts, "0").Err())
}

const fixtureNowMs int64 = 1_700_000_000_000

func testReplay(t *testing.T, rc *redis.Client, p Publisher) *Replay {
	t.Helper()
	r, err := New(rc, rc, p, nil, openRouteGate(), Config{ZoneID: 1, Batch: 2, Budget: 100 * time.Millisecond, RetryAfter: time.Second, Interval: 10 * time.Millisecond}, prometheus.NewRegistry())
	require.NoError(t, err)
	r.now = func() time.Time { return time.UnixMilli(fixtureNowMs) }
	r.jitter = func() time.Duration { return 0 }
	return r
}

func fixtureRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

func TestReplaySurvivesPublisherRestartAndRequiresBothSQLAcks(t *testing.T) {
	rc := fixtureRedis(t)
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	n, err := r.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, []publishedTask{{in.topic, "64", in.first}, {in.topic, "64", in.second}}, p.tasks, "原bytes/key/E/S一字不改")
	require.True(t, rc.HExists(context.Background(), savebarrier.Key(fixtureID), fieldVersion).Val(), "broker确认不能清意图")
	// 进程在broker确认后、SQL提交前退出。新实例重新读同一持久意图。
	p2 := &recordingPublisher{}
	r2 := testReplay(t, rc, p2)
	r2.now = func() time.Time { return r.now().Add(2 * time.Second) }
	require.NoError(t, rc.HSet(context.Background(), savebarrier.Key(fixtureID), "player_database", in.version).Err())
	n, err = r2.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n, "只有一表ACK仍保留并重投")
	require.Equal(t, p.tasks, p2.tasks)
	require.NoError(t, rc.HSet(context.Background(), savebarrier.Key(fixtureID), "player_database_1", in.version).Err())
	r2.bucket = 0
	r2.now = func() time.Time { return r.now().Add(4 * time.Second) }
	n, err = r2.Tick(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	require.False(t, rc.HExists(context.Background(), savebarrier.Key(fixtureID), fieldVersion).Val())
	require.Zero(t, rc.ZCard(context.Background(), pendingIndexKey(1, 0)).Val())
	require.Equal(t, in.version, rc.HGet(context.Background(), savebarrier.Key(fixtureID), "pending").Val())
	require.Equal(t, in.version, rc.HGet(context.Background(), savebarrier.Key(fixtureID), "player_database_1").Val())
	require.Equal(t, int64(3), rc.HLen(context.Background(), savebarrier.Key(fixtureID)).Val(), "回收后只剩 pending 与两表 ACK,补投状态字段一并清掉")
}

func TestPartialPublishKeepsOriginalPairForReplay(t *testing.T) {
	rc := fixtureRedis(t)
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeObservedIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{failAt: 2}
	r := testReplay(t, rc, p)
	n, err := r.Tick(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, in.second, rc.HGet(context.Background(), savebarrier.Key(fixtureID), fieldSecond).Val())
	p2 := &recordingPublisher{}
	r2 := testReplay(t, rc, p2)
	r2.now = func() time.Time { return r.now().Add(2 * time.Second) }
	n, err = r2.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, in.first, p2.tasks[0].body)
	require.Equal(t, in.second, p2.tasks[1].body)
}

func TestOldPublishedPairCannotClearNewPending(t *testing.T) {
	rc := fixtureRedis(t)
	old := testIntent(t, fixtureID, 7, 9, 1)
	newer := testIntent(t, fixtureID, 8, 1, 1)
	storeObservedIntent(t, rc, fixtureID, 1, old)
	p := &recordingPublisher{}
	p.hook = func() {
		storeObservedIntent(t, rc, fixtureID, 1, newer)
		require.NoError(t, rc.HSet(context.Background(), savebarrier.Key(fixtureID), "player_database", old.version, "player_database_1", old.version).Err())
	}
	r := testReplay(t, rc, p)
	_, err := r.Tick(context.Background())
	require.NoError(t, err)
	p.hook = nil
	r.bucket = 0
	_, err = r.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, newer.version, rc.HGet(context.Background(), savebarrier.Key(fixtureID), fieldVersion).Val())
	require.Equal(t, newer.first, p.tasks[2].body)
}

func TestPoisonIntentDoesNotStarveLaterPlayers(t *testing.T) {
	rc := fixtureRedis(t)
	poison := testIntent(t, fixtureID, 7, 9, 1)
	poison.first = "损坏的DBTask"
	storeObservedIntent(t, rc, fixtureID, 1, poison)
	require.NoError(t, rc.ZAdd(context.Background(), pendingIndexKey(1, 0), redis.Z{Score: -1, Member: "64"}).Err())
	good := testIntent(t, 128, 8, 1, 1)
	storeObservedIntent(t, rc, 128, 1, good)
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	r.cfg.Batch = 1
	_, err := r.Tick(context.Background())
	require.NoError(t, err)
	require.Empty(t, p.tasks)
	r.bucket = 0
	_, err = r.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, p.tasks, 2)
	require.Equal(t, "128", p.tasks[0].key)
	require.True(t, rc.HExists(context.Background(), savebarrier.Key(fixtureID), fieldFirst).Val(), "不删坏意图")
}

func TestHomeZoneIndexMigrationRequiresAuthoritativeMapping(t *testing.T) {
	for _, matched := range []bool{false, true} {
		t.Run(fmt.Sprint(matched), func(t *testing.T) {
			rc := fixtureRedis(t)
			in := testIntent(t, fixtureID, 7, 9, 2)
			storeObservedIntent(t, rc, fixtureID, 1, in)
			if matched {
				require.NoError(t, rc.Set(context.Background(), placement.HomeZoneKey(fixtureID), "2", 0).Err())
			}
			p := &recordingPublisher{}
			r := testReplay(t, rc, p)
			_, err := r.Tick(context.Background())
			require.NoError(t, err)
			require.Empty(t, p.tasks, "旧zone只迁索引，不向其他zone发任务")
			if matched {
				require.Zero(t, rc.ZCard(context.Background(), pendingIndexKey(1, 0)).Val())
				require.Equal(t, int64(1), rc.ZCard(context.Background(), pendingIndexKey(2, 0)).Val())
			} else {
				require.Equal(t, int64(1), rc.ZCard(context.Background(), pendingIndexKey(1, 0)).Val())
			}
			require.Equal(t, in.first, rc.HGet(context.Background(), savebarrier.Key(fixtureID), fieldFirst).Val())
		})
	}
}

func TestIntentRejectsWrongEpochKeyTypeAndTopicGeneration(t *testing.T) {
	for _, kind := range []string{"epoch", "key", "row", "table", "generation", "index_type"} {
		t.Run(kind, func(t *testing.T) {
			rc := fixtureRedis(t)
			in := testIntent(t, fixtureID, 7, 9, 1)
			task := &dbpb.DBTask{}
			require.NoError(t, proto.Unmarshal([]byte(in.first), task))
			switch kind {
			case "epoch":
				task.OwnerEpoch++
			case "key":
				task.Key++
			case "row":
				task.Body, _ = proto.Marshal(&databasepb.PlayerDatabase{PlayerId: fixtureID + 1})
			case "table":
				task.MsgType = "任意其他表"
			case "generation":
				in.topic = topicFor(1, 2)
			}
			raw, err := proto.Marshal(task)
			require.NoError(t, err)
			in.first = string(raw)
			storeObservedIntent(t, rc, fixtureID, 1, in)
			if kind == "index_type" {
				require.NoError(t, rc.Del(context.Background(), pendingIndexKey(1, 0)).Err())
				require.NoError(t, rc.Set(context.Background(), pendingIndexKey(1, 0), "错误类型", 0).Err())
			}
			p := &recordingPublisher{}
			r := testReplay(t, rc, p)
			_, err = r.Tick(context.Background())
			if kind == "index_type" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, p.tasks)
			require.Equal(t, in.first, rc.HGet(context.Background(), savebarrier.Key(fixtureID), fieldFirst).Val())
		})
	}
}

func TestCanceledReplayRetainsIntent(t *testing.T) {
	rc := fixtureRedis(t)
	in := testIntent(t, fixtureID, 7, 9, 1)
	storeIntent(t, rc, fixtureID, 1, in)
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Tick(ctx)
	require.Error(t, err)
	require.Empty(t, p.tasks)
	require.Equal(t, in.version, rc.HGet(context.Background(), savebarrier.Key(fixtureID), fieldVersion).Val())
}

func TestCompletedIndexCleanupRequiresExactBothAcks(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			rc := fixtureRedis(t)
			ctx := context.Background()
			in := testIntent(t, fixtureID, 7, 9, 1)
			storeIntent(t, rc, fixtureID, 1, in)
			require.NoError(t, rc.HDel(ctx, savebarrier.Key(fixtureID), fieldVersion, fieldTopic, fieldFirst, fieldSecond).Err())
			second := savebarrier.Version(7, 8)
			if complete {
				second = in.version
			}
			require.NoError(t, rc.HSet(ctx, savebarrier.Key(fixtureID), "player_database", in.version, "player_database_1", second).Err())
			p := &recordingPublisher{}
			_, err := testReplay(t, rc, p).Tick(ctx)
			require.NoError(t, err)
			require.Empty(t, p.tasks)
			want := int64(1)
			if complete {
				want = 0
			}
			require.Equal(t, want, rc.ZCard(ctx, pendingIndexKey(1, 0)).Val(), "缺意图残留也不能在仅一表已提交时清掉")
			require.Equal(t, in.version, rc.HGet(ctx, savebarrier.Key(fixtureID), "pending").Val())
		})
	}
}

func TestMigrationCASAndTargetTypePreserveOriginalIntent(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			rc := fixtureRedis(t)
			ctx := context.Background()
			old := testIntent(t, fixtureID, 7, 9, 2)
			storeIntent(t, rc, fixtureID, 1, old)
			want := old
			if changed {
				want = testIntent(t, fixtureID, 8, 1, 2)
				storeIntent(t, rc, fixtureID, 1, want)
			} else {
				require.NoError(t, rc.Set(ctx, pendingIndexKey(2, 0), "不可覆盖的其他类型", 0).Err())
			}
			n, err := migrateScript.Run(ctx, rc, []string{savebarrier.Key(fixtureID), pendingIndexKey(1, 0), pendingIndexKey(2, 0)}, "64", old.version, old.topic).Int64()
			if changed {
				require.NoError(t, err)
				require.Zero(t, n)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, int64(1), rc.ZCard(ctx, pendingIndexKey(1, 0)).Val())
			require.Equal(t, want.version, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldVersion).Val())
			require.Equal(t, want.first, rc.HGet(ctx, savebarrier.Key(fixtureID), fieldFirst).Val())
		})
	}
}

func TestMalformedIndexMemberIsDeferredWithoutDeletingEvidence(t *testing.T) {
	rc := fixtureRedis(t)
	ctx := context.Background()
	index := pendingIndexKey(1, 0)
	require.NoError(t, rc.ZAdd(ctx, index, redis.Z{Score: -1, Member: "00064"}).Err())
	storeObservedIntent(t, rc, fixtureID, 1, testIntent(t, fixtureID, 7, 9, 1))
	p := &recordingPublisher{}
	r := testReplay(t, rc, p)
	r.cfg.Batch = 1
	_, err := r.Tick(ctx)
	require.NoError(t, err)
	require.Empty(t, p.tasks)
	score, err := rc.ZScore(ctx, index, "00064").Result()
	require.NoError(t, err)
	require.Greater(t, score, float64(r.now().UnixMilli()))
	r.bucket = 0
	_, err = r.Tick(ctx)
	require.NoError(t, err)
	require.Len(t, p.tasks, 2)
}

func TestReplayRedisHonorsBudgetWithoutMutatingSharedOptions(t *testing.T) {
	source := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DB: 4, PoolSize: 9})
	t.Cleanup(func() { _ = source.Close() })
	copy := replayRedis(source)
	t.Cleanup(func() { _ = copy.Close() })
	require.Equal(t, source.Options().Addr, copy.Options().Addr)
	require.Equal(t, source.Options().DB, copy.Options().DB)
	require.False(t, source.Options().ContextTimeoutEnabled)
	require.Equal(t, 9, source.Options().PoolSize)
	require.True(t, copy.Options().ContextTimeoutEnabled)
	require.Zero(t, copy.Options().MaxRetries, "go-redis将输入-1归一为零次重试")
	require.Equal(t, 2, copy.Options().MaxActiveConns)
	require.Equal(t, time.Second, copy.Options().ReadTimeout)
}

func TestStartRejectsSplitOwnerRedisBeforeStartingWorker(t *testing.T) {
	app := dbconfig.Config{}
	app.ServerConfig.Fencing.Mode = dbconfig.FencingModeDual
	app.ServerConfig.RedisClient.Hosts = "127.0.0.1:1"
	app.ServerConfig.OwnerEpochRedis = &dbconfig.PlacementRedisConfig{Hosts: "127.0.0.1:2"}
	s, err := Start(context.Background(), app, nil, nil, nil, openRouteGate())
	require.Error(t, err)
	require.Nil(t, s)
}
