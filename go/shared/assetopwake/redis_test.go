package assetopwake

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"shared/redisslot"
)

// 本文件测 go-redis 适配层:订阅分组、SSUBSCRIBE 确认、收包与健康检查、关连接。
// 收包循环跑在 pubSubConn 替身上,不连 Redis(miniredis v2.35 不支持 SSUBSCRIBE);真集群上的迁槽重订在设计 §9 第 3 项。

// ── 替身 ─────────────────────────────────────────────────────

// fakeTimeoutErr 是读超时(连接空闲)。
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

type fakeReply struct {
	reply interface{}
	err   error
}

// fakeConn 依次交出 replies;交完之后每次都是读超时。
type fakeConn struct {
	mu      sync.Mutex
	replies []fakeReply
	pings   int
	pingErr error
	closes  int
	closed  chan struct{}
}

func newFakeConn(replies ...fakeReply) *fakeConn {
	return &fakeConn{replies: replies, closed: make(chan struct{})}
}

func (c *fakeConn) ReceiveTimeout(context.Context, time.Duration) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.replies) == 0 {
		return nil, fakeTimeoutErr{}
	}
	r := c.replies[0]
	c.replies = c.replies[1:]
	return r.reply, r.err
}

func (c *fakeConn) Ping(context.Context, ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings++
	return c.pingErr
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	if c.closes == 1 {
		close(c.closed)
	}
	return nil
}

func (c *fakeConn) stats() (pings, closes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pings, c.closes
}

func confirmReply(channel string) fakeReply {
	return fakeReply{reply: &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1}}
}

func messageReply(payload string) fakeReply {
	return fakeReply{reply: &redis.Message{Channel: ShardChannel(0), Payload: payload}}
}

func pongReply() fakeReply { return fakeReply{reply: &redis.Pong{}} }

func idleReply() fakeReply { return fakeReply{err: fakeTimeoutErr{}} }

func newTestConnSubscription(t *testing.T, conn *fakeConn) *goRedisSubscription {
	t.Helper()
	sub := newGoRedisSubscription(context.Background(), conn, time.Millisecond)
	t.Cleanup(func() { _ = sub.close() })
	return sub
}

// ── 分组 ─────────────────────────────────────────────────────

// 集群:按 slot 分组,每组的频道同槽,组合起来恰好是全部频道、顺序不变;64 片各自一个 slot,即 64 组。
func TestGroupBySlot(t *testing.T) {
	channels := ShardChannels()
	groups := groupBySlot(channels)
	if len(groups) != ShardCount {
		t.Fatalf("64 个唤醒频道落在 64 个 slot,应分 64 组,实际 %d 组", len(groups))
	}
	var flat []string
	for _, group := range groups {
		slot := redisslot.HashSlot(group[0])
		for _, channel := range group {
			if redisslot.HashSlot(channel) != slot {
				t.Fatalf("组 %v 里的频道不同槽", group)
			}
		}
		flat = append(flat, group...)
	}
	if strings.Join(flat, ",") != strings.Join(channels, ",") {
		t.Fatalf("分组之后应恰好是全部频道且保持顺序,实际 %v", flat)
	}

	// 同槽的频道合在一组(构造两个同 tag 的频道名)。
	same := []string{"x:{aw:1}", "y:{aw:2}", "z:{aw:1}"}
	got := groupBySlot(same)
	if len(got) != 2 || strings.Join(got[0], ",") != "x:{aw:1},z:{aw:1}" || strings.Join(got[1], ",") != "y:{aw:2}" {
		t.Fatalf("同槽的频道应合成一组、按首次出现排序,实际 %v", got)
	}
}

// 单机客户端一组订全部(连接总账按每副本 1 条 Pub/Sub 记);集群客户端按 slot 分组。分组不连 Redis。
func TestGoRedisBusGroupsFollowClientType(t *testing.T) {
	standalone := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer standalone.Close()
	groups := goRedisBus{c: standalone}.groups(ShardChannels())
	if len(groups) != 1 || len(groups[0]) != ShardCount {
		t.Fatalf("单机应一组订全部 %d 个频道,实际 %d 组", ShardCount, len(groups))
	}

	cluster := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}})
	defer cluster.Close()
	groups = goRedisBus{c: cluster}.groups(ShardChannels())
	if len(groups) != ShardCount {
		t.Fatalf("集群应按 slot 分 %d 组,实际 %d 组", ShardCount, len(groups))
	}
	// 生产构造(newSubscriber)对两种分组都能通过覆盖核对。
	if err := validateGroups(groups); err != nil {
		t.Fatalf("集群分组应恰好覆盖每片一次: %v", err)
	}
}

// ── 订阅确认 ─────────────────────────────────────────────────

// 每个频道的确认都到了才算订上;顺序不限。
func TestConfirmSubscribedWaitsForEveryChannel(t *testing.T) {
	channels := []string{ShardChannel(0), ShardChannel(1), ShardChannel(2)}
	conn := newFakeConn(confirmReply(channels[2]), confirmReply(channels[0]), confirmReply(channels[1]))
	if err := confirmSubscribed(context.Background(), conn, channels, time.Second); err != nil {
		t.Fatalf("三个确认都到了应返回 nil,实际 %v", err)
	}
	if left := len(conn.replies); left != 0 {
		t.Fatalf("应正好读三个回包,还剩 %d 个", left)
	}
}

// 服务端拒绝(MOVED、CROSSSLOT、未知命令)、读出错、确认之前来了别的回包:都算没订上。
func TestConfirmSubscribedRejects(t *testing.T) {
	channels := []string{ShardChannel(0), ShardChannel(1)}
	cases := []struct {
		name    string
		replies []fakeReply
	}{
		{"服务端回错误", []fakeReply{{err: redis.ErrClosed}}},
		{"只确认了一部分就出错", []fakeReply{confirmReply(channels[0]), {err: errors.New("MOVED 3999 10.0.0.2:7000")}}},
		{"确认之前来了消息", []fakeReply{confirmReply(channels[0]), messageReply("42")}},
		{"确认之前来了退订", []fakeReply{{reply: &redis.Subscription{Kind: "sunsubscribe", Channel: channels[0]}}}},
		{"读超时", []fakeReply{idleReply()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := confirmSubscribed(context.Background(), newFakeConn(c.replies...), channels, time.Second); err == nil {
				t.Fatal("没全部确认必须返回错误")
			}
		})
	}
	// 总预算用完:不再读。
	if err := confirmSubscribed(context.Background(), newFakeConn(confirmReply(channels[0])), channels, 0); err == nil {
		t.Fatal("预算为 0 时必须立即报超时")
	}
}

// ── 收包与健康检查 ───────────────────────────────────────────

// 消息原样交出;PING 的回音、订阅确认跳过。
func TestReceiveReturnsMessagesAndSkipsControlReplies(t *testing.T) {
	conn := newFakeConn(pongReply(), confirmReply(ShardChannel(3)), messageReply("42"), messageReply("43"))
	sub := newTestConnSubscription(t, conn)
	for _, want := range []string{"42", "43"} {
		got, err := sub.receive(context.Background())
		if err != nil || got != want {
			t.Fatalf("receive = (%q, %v),期望 (%q, nil)", got, err, want)
		}
	}
}

// 迁槽:服务端推来 sunsubscribe,收包返回 errServerUnsubscribed,由订阅循环关掉重订。
func TestReceiveReportsServerUnsubscribe(t *testing.T) {
	conn := newFakeConn(fakeReply{reply: &redis.Subscription{Kind: "sunsubscribe", Channel: ShardChannel(17), Count: 0}})
	_, err := newTestConnSubscription(t, conn).receive(context.Background())
	if !errors.Is(err, errServerUnsubscribed) {
		t.Fatalf("sunsubscribe 应报 errServerUnsubscribed,实际 %v", err)
	}
}

// 空闲一个间隔就 PING;再空闲一个间隔还没有任何回包,当连接已死。
func TestReceiveHealthCheckGivesUpWithoutReply(t *testing.T) {
	conn := newFakeConn() // 只有读超时
	_, err := newTestConnSubscription(t, conn).receive(context.Background())
	if !errors.Is(err, errHealthCheckTimeout) {
		t.Fatalf("PING 没有回音应报 errHealthCheckTimeout,实际 %v", err)
	}
	if pings, _ := conn.stats(); pings != 1 {
		t.Fatalf("放弃之前应只 PING 一次,实际 %d 次", pings)
	}
}

// 任何回包(PING 的回音或消息)都把「等回音」清掉,之后的空闲重新计。
func TestReceiveHealthCheckResetsOnAnyReply(t *testing.T) {
	conn := newFakeConn(idleReply(), pongReply(), idleReply(), messageReply("7"))
	got, err := newTestConnSubscription(t, conn).receive(context.Background())
	if err != nil || got != "7" {
		t.Fatalf("receive = (%q, %v),期望 (\"7\", nil)", got, err)
	}
	if pings, _ := conn.stats(); pings != 2 {
		t.Fatalf("两段空闲各 PING 一次,实际 %d 次", pings)
	}
}

// PING 发不出去、读出非超时的错误、来了不认识的回包:都原样交给订阅循环重订。
func TestReceiveReturnsConnectionErrors(t *testing.T) {
	pingFails := newFakeConn()
	pingFails.pingErr = errors.New("broken pipe")
	if _, err := newTestConnSubscription(t, pingFails).receive(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("PING 写失败应报出原因,实际 %v", err)
	}

	readFails := newFakeConn(fakeReply{err: errors.New("connection reset by peer")})
	if _, err := newTestConnSubscription(t, readFails).receive(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("读出错应原样返回,实际 %v", err)
	}

	unknown := newFakeConn(fakeReply{reply: "not a pubsub reply"})
	if _, err := newTestConnSubscription(t, unknown).receive(context.Background()); err == nil {
		t.Fatal("不认识的回包必须报错")
	}
}

// ctx 结束会关掉连接(让阻塞的读立刻返回);close 之后撤掉这个登记,ctx 再结束也不会重复关。
func TestSubscriptionClosesConnectionOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := newFakeConn()
	newGoRedisSubscription(ctx, conn, time.Second)
	cancel()
	select {
	case <-conn.closed:
	case <-time.After(waitTimeout):
		t.Fatal("ctx 结束之后连接应被关闭")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	conn2 := newFakeConn()
	sub := newGoRedisSubscription(ctx2, conn2, time.Second)
	if err := sub.close(); err != nil {
		t.Fatalf("close 失败: %v", err)
	}
	cancel2()
	// AfterFunc 若没撤掉,会在另一个 goroutine 里再关一次;给它一点时间暴露出来。
	time.Sleep(10 * time.Millisecond)
	if _, closes := conn2.stats(); closes != 1 {
		t.Fatalf("close 之后 ctx 结束不应再关一次,实际关了 %d 次", closes)
	}
}

// 回包分类:只有 *redis.Message 是唤醒;sunsubscribe 是订阅失效;其余可忽略。
func TestClassifyReply(t *testing.T) {
	if payload, isMessage, err := classifyReply(&redis.Message{Payload: "42"}); err != nil || !isMessage || payload != "42" {
		t.Fatalf("消息应原样交出,实际 (%q, %v, %v)", payload, isMessage, err)
	}
	for _, ignorable := range []interface{}{&redis.Pong{}, &redis.Pong{Payload: "PONG"}, &redis.Subscription{Kind: "ssubscribe"}} {
		if _, isMessage, err := classifyReply(ignorable); err != nil || isMessage {
			t.Fatalf("%T 应被忽略,实际 isMessage=%v err=%v", ignorable, isMessage, err)
		}
	}
	if _, _, err := classifyReply(&redis.Subscription{Kind: "sunsubscribe", Channel: ShardChannel(1)}); !errors.Is(err, errServerUnsubscribed) {
		t.Fatalf("sunsubscribe 应报 errServerUnsubscribed,实际 %v", err)
	}
	if !isTimeout(fakeTimeoutErr{}) || isTimeout(errors.New("x")) || isTimeout(context.Canceled) {
		t.Fatal("isTimeout 只认读超时")
	}
}
