package authority

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	chatpb "proto/chat"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 世界频道近窗快照的单元用例(docs/design/chat-world-channel-fanout.md §3)。不连库:读取函数与单调时钟都是替身。

type fakeMonotonic struct {
	mu sync.Mutex
	at time.Duration
}

func (c *fakeMonotonic) now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeMonotonic) advance(d time.Duration) {
	c.mu.Lock()
	c.at += d
	c.mu.Unlock()
}

func worldRow(t *testing.T, sender uint64, id string, acceptedMs uint64) historyRow {
	t.Helper()
	payload, err := proto.Marshal(&chatpb.ChatMessage{SenderPlayerId: sender, Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "hi " + id, SendTimeMs: int64(acceptedMs)})
	require.NoError(t, err)
	return historyRow{sender: sender, requestID: []byte(id), payload: payload, acceptedMs: acceptedMs}
}

// countingLoader 每次调用返回一份带编号的快照,编号写在 dbNowMs 里,便于断言拿到的是哪一次读取。
type countingLoader struct {
	calls atomic.Int32
}

func (l *countingLoader) load(context.Context) (*worldSnapshot, error) {
	n := l.calls.Add(1)
	return &worldSnapshot{dbNowMs: uint64(n) * 1000}, nil
}

func TestWorldWindowReusesFreshSnapshotAndReloadsWhenExpired(t *testing.T) {
	clock := &fakeMonotonic{}
	loader := &countingLoader{}
	w := newWorldWindow(500*time.Millisecond, clock.now, loader.load)

	first, err := w.get(context.Background())
	require.NoError(t, err)
	clock.advance(499 * time.Millisecond)
	again, err := w.get(context.Background())
	require.NoError(t, err)
	require.Same(t, first, again, "未过期的快照直接复用")
	require.EqualValues(t, 1, loader.calls.Load())

	clock.advance(time.Millisecond)
	reloaded, err := w.get(context.Background())
	require.NoError(t, err)
	require.NotSame(t, first, reloaded, "满 maxAge 就重读")
	require.EqualValues(t, 2, loader.calls.Load())
}

func TestWorldWindowConcurrentRequestsShareOneLoad(t *testing.T) {
	clock := &fakeMonotonic{}
	release := make(chan struct{})
	var calls atomic.Int32
	w := newWorldWindow(time.Second, clock.now, func(context.Context) (*worldSnapshot, error) {
		calls.Add(1)
		<-release
		return &worldSnapshot{dbNowMs: 7}, nil
	})

	const waiters = 8
	results := make(chan *worldSnapshot, waiters)
	var started sync.WaitGroup
	for range waiters {
		started.Add(1)
		go func() {
			started.Done()
			snap, err := w.get(context.Background())
			if err != nil {
				results <- nil
				return
			}
			results <- snap
		}()
	}
	started.Wait()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	var first *worldSnapshot
	for range waiters {
		snap := <-results
		require.NotNil(t, snap)
		if first == nil {
			first = snap
		}
		require.Same(t, first, snap, "同一时刻只有一次重读,所有等待方拿同一份结果")
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestWorldWindowDoesNotCacheFailures(t *testing.T) {
	clock := &fakeMonotonic{}
	failure := errors.New("库不可用")
	var calls atomic.Int32
	w := newWorldWindow(time.Minute, clock.now, func(context.Context) (*worldSnapshot, error) {
		if calls.Add(1) == 1 {
			return nil, failure
		}
		return &worldSnapshot{dbNowMs: 9}, nil
	})

	_, err := w.get(context.Background())
	require.ErrorIs(t, err, failure, "读失败按存储故障回,不拿旧快照或空快照顶")
	snap, err := w.get(context.Background())
	require.NoError(t, err, "失败不进缓存,下一次请求立刻重读")
	require.EqualValues(t, 9, snap.dbNowMs)
	require.EqualValues(t, 2, calls.Load())
}

func TestWorldWindowInvalidateKeepsInFlightResultOutOfCache(t *testing.T) {
	clock := &fakeMonotonic{}
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	w := newWorldWindow(time.Minute, clock.now, func(context.Context) (*worldSnapshot, error) {
		n := calls.Add(1)
		if n == 1 {
			<-releaseFirst
		}
		return &worldSnapshot{dbNowMs: uint64(n)}, nil
	})

	early := make(chan *worldSnapshot, 1)
	go func() {
		snap, err := w.get(context.Background())
		if err != nil {
			early <- nil
			return
		}
		early <- snap
	}()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)

	// 本副本世界频道提交成功:在途的第一次读取可能没有这条消息。
	w.invalidate()
	after, err := w.get(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, after.dbNowMs, "作废之后到来的请求发起新的读取,不搭在途的旧读取")

	close(releaseFirst)
	first := <-early
	require.NotNil(t, first)
	require.EqualValues(t, 1, first.dbNowMs, "作废前已经在等的请求照样拿到它等的那次结果")

	cached, err := w.get(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, cached.dbNowMs, "作废前开始的读取结果不进缓存")
	require.EqualValues(t, 2, calls.Load())
}

func TestWorldWindowRequesterCancelDoesNotAbortSharedLoad(t *testing.T) {
	clock := &fakeMonotonic{}
	release := make(chan struct{})
	loadCtxErr := make(chan error, 1)
	w := newWorldWindow(time.Minute, clock.now, func(ctx context.Context) (*worldSnapshot, error) {
		<-release
		loadCtxErr <- ctx.Err()
		return &worldSnapshot{dbNowMs: 3}, nil
	})

	impatient, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := w.get(impatient)
		done <- err
	}()
	patient := make(chan *worldSnapshot, 1)
	go func() {
		snap, _ := w.get(context.Background())
		patient <- snap
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled, "发起者自己的期限到了就先回")
	close(release)
	require.NoError(t, <-loadCtxErr, "读取不受发起者取消的牵连")
	snap := <-patient
	require.NotNil(t, snap)
	require.EqualValues(t, 3, snap.dbNowMs)
}

func TestWorldWindowLoadPanicReleasesWaiters(t *testing.T) {
	clock := &fakeMonotonic{}
	var calls atomic.Int32
	w := newWorldWindow(time.Minute, clock.now, func(context.Context) (*worldSnapshot, error) {
		if calls.Add(1) == 1 {
			panic("读取函数的缺陷")
		}
		return &worldSnapshot{dbNowMs: 5}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := w.get(ctx)
	require.ErrorIs(t, err, errWorldWindowLoadAborted, "读取 panic 时等待方拿到错误,不挂到超时")
	snap, err := w.get(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 5, snap.dbNowMs)
}

func TestWorldWindowNilSnapshotIsAnError(t *testing.T) {
	clock := &fakeMonotonic{}
	w := newWorldWindow(time.Minute, clock.now, func(context.Context) (*worldSnapshot, error) { return nil, nil })
	_, err := w.get(context.Background())
	require.ErrorIs(t, err, errWorldWindowLoadAborted)
}

func TestWorldSnapshotNowAdvancesDatabaseTimeByMonotonicAge(t *testing.T) {
	snap := &worldSnapshot{dbNowMs: 1000, takenAt: 2 * time.Second}
	require.EqualValues(t, 1250, snap.nowMs(2250*time.Millisecond))
	require.EqualValues(t, 1000, snap.nowMs(time.Second), "单调时钟不会倒退;防御性地按 0 计")
}

func TestVisibleHistoryWindowSemantics(t *testing.T) {
	world := chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD
	const window = 10000
	a := worldRow(t, 11, "a", 100000)
	b := worldRow(t, 12, "b", 99000)
	c := worldRow(t, 13, "c", 88000) // 与 b 间隔 11000,达到 TTL:窗口在 b 之后截断
	corrupt := historyRow{sender: 14, requestID: []byte("d"), payload: []byte("不是 PB"), acceptedMs: 87000}

	got, err := visibleHistory([]historyRow{a, b, c, corrupt}, 101000, window, world, 11, 0)
	require.NoError(t, err, "截断之后的行即使损坏也不报,与直读遍历到才报一致")
	require.Len(t, got, 2)
	require.Equal(t, "hi a", got[0].Content)
	require.Equal(t, "hi b", got[1].Content)

	got, err = visibleHistory([]historyRow{a, b}, 110000, window, world, 11, 0)
	require.NoError(t, err)
	require.Empty(t, got, "最新一条距今达到 TTL,整窗不可见")

	_, err = visibleHistory([]historyRow{a, {sender: 14, requestID: []byte("d"), payload: []byte("不是 PB"), acceptedMs: 99500}}, 101000, window, world, 11, 0)
	require.Error(t, err, "遍历到的损坏原件照样报错")

	targeted := worldRow(t, 15, "e", 99800)
	message := &chatpb.ChatMessage{}
	require.NoError(t, proto.Unmarshal(targeted.payload, message))
	message.TargetPlayerId = 16
	targeted.payload, err = proto.Marshal(message)
	require.NoError(t, err)
	_, err = visibleHistory([]historyRow{a, targeted}, 101000, window, world, 11, 0)
	require.Error(t, err, "世界频道原件不能带私聊对象")

	_, err = visibleHistory([]historyRow{{sender: 11, payload: a.payload, acceptedMs: a.acceptedMs}}, 101000, window, world, 11, 0)
	require.Error(t, err, "缺请求号的行是损坏原件")
}

// windowStore 组一个只走世界频道快照分支的 Store:不连库,身份校验用目录替身。
func windowStore(t *testing.T, load func(context.Context) (*worldSnapshot, error)) (*Store, *fakeMonotonic) {
	t.Helper()
	clock := &fakeMonotonic{}
	return &Store{
		states: newFixtureRoleStates(),
		rules:  fixtureRules(),
		actors: newActorVerifyCache(0, actorVerifyMaxEntries, monotonicSinceStart()),
		world:  newWorldWindow(time.Minute, clock.now, load),
	}, clock
}

func TestPullWorldReadsSnapshotWithRequestLimitAndDatabaseClock(t *testing.T) {
	rows := make([]historyRow, 0, 30)
	for i := range 30 {
		rows = append(rows, worldRow(t, uint64(100+i), fmt.Sprintf("r%d", i), uint64(200000-i*1000)))
	}
	var calls atomic.Int32
	s, clock := windowStore(t, func(context.Context) (*worldSnapshot, error) {
		calls.Add(1)
		return &worldSnapshot{rows: rows, dbNowMs: 200500}, nil
	})
	ctx := original(t, 11)
	world := &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD}

	got, err := s.Pull(ctx, world, 5)
	require.NoError(t, err)
	require.Len(t, got, 5, "快照存的是整窗,应答仍按本次请求的条数截断")
	require.EqualValues(t, rows[0].acceptedMs, got[0].SendTimeMs)

	got, err = s.Pull(ctx, world, 50)
	require.NoError(t, err)
	require.Len(t, got, 30)
	require.EqualValues(t, 1, calls.Load(), "同一份快照服务多次拉取")

	// TTL 用读取时的数据库时间加快照年龄:fixtureRules 的 TTL 是 604800 秒,这里推进到最新一条刚好过期。
	clock.advance(30 * time.Second) // 只推进单调时钟,快照 maxAge 是一分钟,仍复用
	s.rules.HistoryTTLSeconds = 30
	got, err = s.Pull(ctx, world, 50)
	require.NoError(t, err)
	require.Empty(t, got, "最新一条 200000 距推算的数据库时间 230500 已满 30 秒")

	_, err = s.Pull(context.Background(), world, 5)
	require.ErrorIs(t, err, ErrOriginal, "快照分支之前照样校验身份")
}
