package assetopwake

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shared/assetop"
	"shared/redisslot"
	"shared/workshard"
)

// 本文件用 bus 接口的替身代替 Redis:订阅按组记下每一次订阅,测试往某组当前那次订阅里投消息或错误;
// 去重键按 SET NX 语义记在内存里;唤醒的落点用 fakeWaker 记录。
// 直接调 handle 的用例用 wg.Wait 等后台 goroutine,不靠 sleep。go-redis 适配层(确认、收包、健康检查)在 redis_test.go。

// 编译期契约:生产装配传的就是这两个类型。
var (
	_ Waker   = (*assetop.Loop)(nil)
	_ Counter = (*assetop.Metrics)(nil)
)

// waitTimeout 是等后台 goroutine 的上限;正常几毫秒内就到,超时说明真的卡住了。
const waitTimeout = 5 * time.Second

// ── 替身 ─────────────────────────────────────────────────────

type fakeEvent struct {
	payload string
	err     error
}

// fakeSub 是一次订阅:receive 依次交出 events 里的事件。
type fakeSub struct {
	channels []string
	events   chan fakeEvent
	closes   atomic.Int32
}

func (s *fakeSub) receive(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case ev := <-s.events:
		return ev.payload, ev.err
	}
}

func (s *fakeSub) close() error {
	s.closes.Add(1)
	return nil
}

type fakeBus struct {
	mu sync.Mutex

	// grouping 决定 groups 的结果;nil = 单机形状(全部频道一组)。
	grouping func(channels []string) [][]string

	// 订阅侧:按组键(组里第一个频道)记下每一次成功的订阅;failSubscribes > 0 时先让这么多次订阅失败。
	subs           map[string][]*fakeSub
	failSubscribes int
	subscribeErr   error
	attempts       int
	refreshes      int
	changed        chan struct{}

	// 去重侧:SET NX 语义,记下 TTL 以便断言。
	keys       map[string]time.Duration
	dedupErr   error
	dedupCalls int
}

func newFakeBus() *fakeBus {
	return &fakeBus{
		subs:    map[string][]*fakeSub{},
		changed: make(chan struct{}, 1),
		keys:    map[string]time.Duration{},
	}
}

// oneGroupPerChannel 模拟集群:每个频道一个 slot、一组。
func oneGroupPerChannel(channels []string) [][]string {
	groups := make([][]string, len(channels))
	for i, channel := range channels {
		groups[i] = []string{channel}
	}
	return groups
}

func (b *fakeBus) groups(channels []string) [][]string {
	if b.grouping != nil {
		return b.grouping(channels)
	}
	return [][]string{append([]string(nil), channels...)}
}

func (b *fakeBus) subscribe(_ context.Context, channels []string) (subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	defer b.notify()
	if b.failSubscribes > 0 {
		b.failSubscribes--
		return nil, errors.New("fake: subscribe failed")
	}
	if b.subscribeErr != nil {
		return nil, b.subscribeErr
	}
	sub := &fakeSub{channels: append([]string(nil), channels...), events: make(chan fakeEvent, 16)}
	b.subs[channels[0]] = append(b.subs[channels[0]], sub)
	return sub, nil
}

func (b *fakeBus) refreshTopology(context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshes++
}

// notify 在持锁时调用:非阻塞地发一个「有变化」的信号。
func (b *fakeBus) notify() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func (b *fakeBus) claimDedup(_ context.Context, key string, ttl time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dedupCalls++
	if b.dedupErr != nil {
		return false, b.dedupErr
	}
	if _, ok := b.keys[key]; ok {
		return false, nil
	}
	b.keys[key] = ttl
	return true, nil
}

// waitSubscribes 等组 key 出现第 n 次成功的订阅并返回它。
func (b *fakeBus) waitSubscribes(t *testing.T, key string, n int) *fakeSub {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		b.mu.Lock()
		subs := b.subs[key]
		b.mu.Unlock()
		if len(subs) >= n {
			return subs[n-1]
		}
		select {
		case <-b.changed:
		case <-deadline:
			t.Fatalf("等待 %s 的第 %d 次订阅超时(已有 %d 次)", key, n, len(subs))
		}
	}
}

func (b *fakeBus) subscribeCount(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[key])
}

func (b *fakeBus) counts() (attempts, refreshes, dedupCalls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts, b.refreshes, b.dedupCalls
}

type fakeWaker struct {
	mu      sync.Mutex
	calls   []uint64
	budgets []time.Duration

	// block 非 nil 时 WakePlayer 等它关闭或 ctx 结束才返回。
	block    chan struct{}
	called   chan uint64
	returned atomic.Int32
	err      error
}

func newFakeWaker() *fakeWaker { return &fakeWaker{called: make(chan uint64, 64)} }

func (w *fakeWaker) WakePlayer(ctx context.Context, playerID uint64) (int, error) {
	w.mu.Lock()
	w.calls = append(w.calls, playerID)
	if dl, ok := ctx.Deadline(); ok {
		w.budgets = append(w.budgets, time.Until(dl))
	} else {
		w.budgets = append(w.budgets, -1)
	}
	w.mu.Unlock()
	w.called <- playerID
	if w.block != nil {
		select {
		case <-w.block:
		case <-ctx.Done():
		}
	}
	w.returned.Add(1)
	return 1, w.err
}

func (w *fakeWaker) snapshot() ([]uint64, []time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]uint64(nil), w.calls...), append([]time.Duration(nil), w.budgets...)
}

type fakeCounter struct {
	mu sync.Mutex
	n  map[assetop.WakeResult]int
}

func newFakeCounter() *fakeCounter { return &fakeCounter{n: map[assetop.WakeResult]int{}} }

func (c *fakeCounter) IncWake(r assetop.WakeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[r]++
}

func (c *fakeCounter) get(r assetop.WakeResult) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[r]
}

// testRetry 把重订退避缩到毫秒级,用例不用等真实的秒级退避。
var testRetry = retryPolicy{base: 5 * time.Millisecond, max: 20 * time.Millisecond}

func newTestSubscriber(t *testing.T, bus *fakeBus, cfg Config, waker Waker, counter Counter) *Subscriber {
	t.Helper()
	if cfg.Service == "" {
		cfg.Service = "guild"
	}
	s, err := newSubscriber(bus, cfg, waker, counter)
	if err != nil {
		t.Fatalf("newSubscriber 失败: %v", err)
	}
	s.retry = testRetry
	return s
}

// waitSignal 在上限内等一个信号。
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("等待%s超时", what)
	}
}

// waitWake 在上限内等下一次 WakePlayer 被调用,返回它的 player_id。
func waitWake(t *testing.T, w *fakeWaker, what string) uint64 {
	t.Helper()
	select {
	case id := <-w.called:
		return id
	case <-time.After(waitTimeout):
		t.Fatalf("等待%s超时", what)
	}
	return 0
}

// waitClosed 在上限内等某次订阅被关闭。
func waitClosed(t *testing.T, sub *fakeSub, what string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for sub.closes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("等待%s超时", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stopWithin 在上限内等 Stop 返回;超时说明有 goroutine 没退出。
func stopWithin(t *testing.T, s *Subscriber) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	waitSignal(t, done, "Stop 返回")
}

// standaloneKey 是单机形状下唯一那一组的组键。
var standaloneKey = ShardChannel(0)

// ── 纯函数与装配 ─────────────────────────────────────────────

func TestParsePlayerID(t *testing.T) {
	maxID := strconv.FormatUint(math.MaxUint64, 10)
	cases := []struct {
		payload string
		want    uint64
		ok      bool
	}{
		{"42", 42, true},
		{maxID, math.MaxUint64, true},
		{"0", 0, false},
		{"", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{" 42", 0, false},
		{"42 ", 0, false},
		{"42\n", 0, false},
		{"0x2a", 0, false},
		{"4_2", 0, false},
		{"abc", 0, false},
		{"18446744073709551616", 0, false}, // uint64 上限 + 1
	}
	for _, c := range cases {
		t.Run(strconv.Quote(c.payload), func(t *testing.T) {
			got, err := ParsePlayerID(c.payload)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("ParsePlayerID(%q) = (%d, %v),期望 (%d, ok=%v)", c.payload, got, err, c.want, c.ok)
			}
		})
	}
}

// 频道名与去重键是跨进程契约:频道由 scene(C++)按同一规则拼出发布,去重键每个服务一把。
// 下面的金样与 C++ 侧 player_landing 的用例取同一组 player_id(130、64、uint64 上限),两边逐字一致。
func TestChannelAndDedupKey(t *testing.T) {
	if ShardCount != 64 || ChannelPrefix != "assetop:wake:" {
		t.Fatalf("分片数应为 64、前缀应为 assetop:wake:,实际 %d / %q", ShardCount, ChannelPrefix)
	}
	// 与资产通道的工作分片同一规则:缺口第 25 条要按 workshard 的片号挑本副本订哪几片。
	if ShardCount != workshard.Shards {
		t.Fatalf("唤醒分片数 %d 必须等于资产通道的工作分片数 workshard.Shards=%d", ShardCount, workshard.Shards)
	}

	goldens := []struct {
		shard uint64
		want  string
	}{
		{0, "assetop:wake:{aw:0}"},
		{2, "assetop:wake:{aw:2}"},
		{63, "assetop:wake:{aw:63}"},
	}
	for _, g := range goldens {
		if got := ShardChannel(g.shard); got != g.want {
			t.Fatalf("ShardChannel(%d) = %q,期望 %q", g.shard, got, g.want)
		}
	}
	players := []struct {
		playerID uint64
		want     string
	}{
		{130, "assetop:wake:{aw:2}"},
		{64, "assetop:wake:{aw:0}"},
		{math.MaxUint64, "assetop:wake:{aw:63}"},
	}
	for _, p := range players {
		if got := ShardChannel(p.playerID % ShardCount); got != p.want {
			t.Fatalf("player %d 的频道 = %q,期望 %q", p.playerID, got, p.want)
		}
	}

	channels := ShardChannels()
	if len(channels) != ShardCount {
		t.Fatalf("ShardChannels 应有 %d 个,实际 %d", ShardCount, len(channels))
	}
	slots := map[uint16]bool{}
	for shard, channel := range channels {
		if channel != ShardChannel(uint64(shard)) {
			t.Fatalf("ShardChannels()[%d] = %q,应按片号排序", shard, channel)
		}
		tag, ok := redisslot.HashTag(channel)
		if !ok || !redisslot.IsControlTag(tag) || !strings.HasPrefix(tag, redisslot.ControlFamilyAssetWake.Prefix()+":") {
			t.Fatalf("频道 %q 的 tag 应是控制集群 aw 族,实际 %q(ok=%v)", channel, tag, ok)
		}
		slots[redisslot.HashSlot(channel)] = true
	}
	// 64 片落在 64 个不同的 slot 上;若有两片同槽,集群上就会合成一组、一条连接,不影响正确性,但热点估算要重算。
	if len(slots) != ShardCount {
		t.Fatalf("64 个唤醒频道应落在 64 个不同的 slot,实际 %d 个", len(slots))
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("越界的片号必须 panic")
			}
		}()
		_ = ShardChannel(ShardCount)
	}()

	if got := DedupKey("guild", 42); got != "assetop:wake:guild:42" {
		t.Fatalf("去重键格式错: %q", got)
	}
	if got := DedupKey("trade", math.MaxUint64); got != "assetop:wake:trade:18446744073709551615" {
		t.Fatalf("去重键应按十进制写完整 uint64,实际 %q", got)
	}
	if redisslot.HasHashTag(DedupKey("guild", 42)) {
		t.Fatal("去重键是不带 tag 的单键,不得带花括号")
	}
}

// cppLandingHeader 是 scene 侧声明唤醒频道规则的头文件,路径相对仓库根。
const cppLandingHeader = "cpp/libs/services/scene/player/system/player_landing.h"

// C++ 侧的三处声明按下面的完整写法查找;改写法要两边同批改。
const (
	cppChannelPrefixDecl = `inline constexpr const char *kAssetWakeChannelPrefix = "`
	cppShardCountDecl    = `inline constexpr uint64_t kAssetWakeShardCount = `
	cppWakeFamilyRef     = `redis_slot::ControlFamily::kAssetWake`
	// cppLegacyChannelDecl 是改分片之前的单频道常量;它还在就说明有人照旧往没人订阅的频道发。
	cppLegacyChannelDecl = `*kAssetWakeChannel = `
)

// 频道规则两边各写一份。漂移的表现是 guild / trade / data_service 永远收不到唤醒、只剩原退避,一个错都不报,
// 所以逐字比对 C++ 的前缀与分片数,并要求 C++ 用控制集群的 aw 族拼 tag(族前缀本身由 redisslot 的契约核对)。
// 只拷了 go/ 的构建环境里没有头文件时跳过。
func TestChannelMatchesCpp(t *testing.T) {
	// go test 的工作目录是本包目录(go/shared/assetopwake),仓库根在上三级。
	data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(cppLandingHeader)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码,跳过逐字比对")
	}
	if err != nil {
		t.Fatalf("读 %s: %v", cppLandingHeader, err)
	}
	src := string(data)

	start := strings.Index(src, cppChannelPrefixDecl)
	if start < 0 {
		t.Fatalf("%s 里找不到 %q(声明须保持 const char 指针 + 单个字面量的写法)", cppLandingHeader, cppChannelPrefixDecl)
	}
	rest := src[start+len(cppChannelPrefixDecl):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("%s 的 kAssetWakeChannelPrefix 字面量没有闭合", cppLandingHeader)
	}
	if got := rest[:end]; got != ChannelPrefix {
		t.Fatalf("C++ kAssetWakeChannelPrefix = %q,Go ChannelPrefix = %q:两边必须逐字一致", got, ChannelPrefix)
	}

	start = strings.Index(src, cppShardCountDecl)
	if start < 0 {
		t.Fatalf("%s 里找不到 %q(分片数须直接写十进制数字)", cppLandingHeader, cppShardCountDecl)
	}
	rest = src[start+len(cppShardCountDecl):]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	got, err := strconv.ParseUint(rest[:digits], 10, 64)
	if err != nil {
		t.Fatalf("%s 的 kAssetWakeShardCount 不是十进制数字: %q", cppLandingHeader, rest[:min(len(rest), 16)])
	}
	if got != ShardCount {
		t.Fatalf("C++ kAssetWakeShardCount = %d,Go ShardCount = %d:两边必须一致", got, ShardCount)
	}

	if !strings.Contains(src, cppWakeFamilyRef) {
		t.Fatalf("%s 里找不到 %s:频道的 tag 必须用控制集群的 aw 族拼", cppLandingHeader, cppWakeFamilyRef)
	}
	if strings.Contains(src, cppLegacyChannelDecl) {
		t.Fatalf("%s 里还有旧的单频道常量 kAssetWakeChannel:订阅方只订分片频道,往旧频道发的唤醒没人收", cppLandingHeader)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	s := newTestSubscriber(t, newFakeBus(), Config{}, newFakeWaker(), nil)
	if s.cfg.Concurrency != DefaultConcurrency || cap(s.slots) != DefaultConcurrency {
		t.Fatalf("默认并发应为 %d,实际 cfg=%d slots=%d", DefaultConcurrency, s.cfg.Concurrency, cap(s.slots))
	}
	// 唤醒超时必须明显大于 Loop 的默认 OpBudget(2.5s):WakePlayer 只在剩余预算够一整行时才开始新行。
	if s.cfg.DedupTTL != 5*time.Second || s.cfg.WakeTimeout != 10*time.Second {
		t.Fatalf("默认去重窗口 5s、唤醒超时 10s,实际 %v / %v", s.cfg.DedupTTL, s.cfg.WakeTimeout)
	}
	if budget := assetop.DefaultLoopConfig().OpBudget; DefaultWakeTimeout <= budget {
		t.Fatalf("默认唤醒超时 %v 必须大于 Loop 默认 OpBudget %v,否则每次唤醒一行也开始不了", DefaultWakeTimeout, budget)
	}
	// 生产退避的默认值(newTestSubscriber 改小之前)。
	fresh, err := newSubscriber(newFakeBus(), Config{Service: "guild"}, newFakeWaker(), nil)
	if err != nil {
		t.Fatalf("newSubscriber 失败: %v", err)
	}
	if fresh.retry.base != resubscribeBaseDelay || fresh.retry.max != resubscribeMaxDelay {
		t.Fatalf("默认重订退避应为 %v 起、封顶 %v,实际 %+v", resubscribeBaseDelay, resubscribeMaxDelay, fresh.retry)
	}

	bad := []struct {
		name string
		cfg  Config
	}{
		{"缺服务名", Config{}},
		{"并发为负", Config{Service: "guild", Concurrency: -1}},
		{"去重窗口为负", Config{Service: "guild", DedupTTL: -time.Second}},
		{"唤醒超时为负", Config{Service: "guild", WakeTimeout: -time.Second}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newSubscriber(newFakeBus(), c.cfg, newFakeWaker(), nil); err == nil {
				t.Fatal("非法配置必须被拒")
			}
		})
	}
	if _, err := newSubscriber(newFakeBus(), Config{Service: "guild"}, nil, nil); err == nil {
		t.Fatal("缺 Waker 必须被拒")
	}
	if _, err := New(nil, Config{Service: "guild"}, newFakeWaker(), nil); err == nil {
		t.Fatal("缺 Redis 客户端必须被拒")
	}
}

// 订阅组必须恰好覆盖每个频道一次:漏掉的片静默收不到唤醒,构造时就拒绝。
func TestSubscriptionGroupsMustCoverEveryShardOnce(t *testing.T) {
	bad := []struct {
		name     string
		grouping func([]string) [][]string
	}{
		{"漏一片", func(ch []string) [][]string { return [][]string{ch[1:]} }},
		{"重复一片", func(ch []string) [][]string { return [][]string{ch, {ch[5]}} }},
		{"空组", func(ch []string) [][]string { return [][]string{ch, {}} }},
		{"未知频道", func(ch []string) [][]string { return [][]string{ch, {"assetop:wake"}} }},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			bus := newFakeBus()
			bus.grouping = c.grouping
			if _, err := newSubscriber(bus, Config{Service: "guild"}, newFakeWaker(), nil); err == nil {
				t.Fatal("订阅组没恰好覆盖每片一次,构造必须失败")
			}
		})
	}

	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	s := newTestSubscriber(t, bus, Config{}, newFakeWaker(), nil)
	if len(s.groups) != ShardCount {
		t.Fatalf("集群形状应有 %d 组,实际 %d", ShardCount, len(s.groups))
	}
}

// 退避:第 n 次连续失败的等待落在 [d/2, d),d = base·2^(n-1) 封顶 max。
func TestRetryDelayBackoffAndJitter(t *testing.T) {
	p := retryPolicy{base: time.Second, max: 30 * time.Second}
	low := func(int64) int64 { return 0 }
	high := func(n int64) int64 { return n - 1 }
	cases := []struct {
		failures int
		d        time.Duration
	}{
		{0, time.Second}, // 不合法的 0 按第 1 次处理
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // 32s 封顶到 30s
		{40, 30 * time.Second},
		{math.MaxInt32, 30 * time.Second}, // 移位不溢出
	}
	for _, c := range cases {
		if got := p.delay(c.failures, low); got != c.d/2 {
			t.Fatalf("第 %d 次失败的最短等待应为 %v,实际 %v", c.failures, c.d/2, got)
		}
		if got := p.delay(c.failures, high); got != c.d-time.Nanosecond {
			t.Fatalf("第 %d 次失败的最长等待应为 %v,实际 %v", c.failures, c.d-time.Nanosecond, got)
		}
	}
}

// ── 单条消息的处理 ───────────────────────────────────────────

// 合法消息:写本服务的去重键(SET NX EX 5)后唤醒;同一窗口内的重复消息被去重。
// 另一个服务有自己的键,不受影响。
func TestHandleWakesOnceAndDedups(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	counter := newFakeCounter()
	s := newTestSubscriber(t, bus, Config{Service: "guild"}, waker, counter)
	ctx := context.Background()

	s.handle(ctx, "42")
	s.wg.Wait()
	s.handle(ctx, "42")
	s.wg.Wait()

	calls, _ := waker.snapshot()
	if len(calls) != 1 || calls[0] != 42 {
		t.Fatalf("同一窗口内只应唤醒一次 player 42,实际 %v", calls)
	}
	bus.mu.Lock()
	ttl, ok := bus.keys["assetop:wake:guild:42"]
	bus.mu.Unlock()
	if !ok || ttl != 5*time.Second {
		t.Fatalf("应写去重键 assetop:wake:guild:42 且 TTL 5s,实际 ok=%v ttl=%v", ok, ttl)
	}
	if counter.get(assetop.WakeReceived) != 2 || counter.get(assetop.WakeDeduped) != 1 {
		t.Fatalf("received 应为 2、deduped 应为 1,实际 %v", counter.n)
	}

	// trade 订阅同一条消息:它有自己的去重键,照样唤醒。
	tradeWaker := newFakeWaker()
	trade := newTestSubscriber(t, bus, Config{Service: "trade"}, tradeWaker, nil)
	trade.handle(ctx, "42")
	trade.wg.Wait()
	if calls, _ := tradeWaker.snapshot(); len(calls) != 1 {
		t.Fatalf("另一个服务不得被 guild 的去重键挡住,实际 %v", calls)
	}
}

// 非法负载:计 error 后丢弃,不写去重键、不唤醒。
func TestHandleDropsMalformedPayload(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	counter := newFakeCounter()
	s := newTestSubscriber(t, bus, Config{}, waker, counter)

	for _, payload := range []string{"abc", "0", "-7", "42 "} {
		s.handle(context.Background(), payload)
	}
	s.wg.Wait()

	if calls, _ := waker.snapshot(); len(calls) != 0 {
		t.Fatalf("非法负载不得唤醒,实际 %v", calls)
	}
	if _, _, dedup := bus.counts(); dedup != 0 {
		t.Fatalf("非法负载不得写去重键,实际写了 %d 次", dedup)
	}
	if counter.get(assetop.WakeReceived) != 4 || counter.get(assetop.WakeError) != 4 {
		t.Fatalf("received 与 error 都应为 4,实际 %v", counter.n)
	}
}

// 并发已满:直接丢(计 dropped),而且不写去重键 —— 别的副本仍能接下这次唤醒。
// 名额在唤醒结束后归还,之后的消息照常处理。
func TestHandleDropsWhenConcurrencyFull(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	waker.block = make(chan struct{})
	counter := newFakeCounter()
	s := newTestSubscriber(t, bus, Config{Concurrency: 1}, waker, counter)
	ctx := context.Background()

	s.handle(ctx, "1") // 名额在 handle 内同步拿走
	s.handle(ctx, "2") // 名额已满:同步丢弃
	if got := counter.get(assetop.WakeDropped); got != 1 {
		t.Fatalf("并发满时应丢弃并计 dropped=1,实际 %d", got)
	}
	waitWake(t, waker, "player 1 被唤醒")
	close(waker.block)
	s.wg.Wait()

	s.handle(ctx, "3")
	s.wg.Wait()

	calls, _ := waker.snapshot()
	if len(calls) != 2 || calls[0] != 1 || calls[1] != 3 {
		t.Fatalf("应只唤醒 1 与 3,实际 %v", calls)
	}
	bus.mu.Lock()
	_, wroteDropped := bus.keys[DedupKey("guild", 2)]
	bus.mu.Unlock()
	if wroteDropped {
		t.Fatal("被丢弃的唤醒不得写去重键")
	}
}

// 去重键写失败:不唤醒(多半是 Redis 本身有问题,唤醒丢了只是退回原退避)。
func TestHandleDedupErrorSkipsWake(t *testing.T) {
	bus := newFakeBus()
	bus.dedupErr = errors.New("redis 不可用")
	waker := newFakeWaker()
	counter := newFakeCounter()
	s := newTestSubscriber(t, bus, Config{}, waker, counter)

	s.handle(context.Background(), "42")
	s.wg.Wait()

	if calls, _ := waker.snapshot(); len(calls) != 0 {
		t.Fatalf("去重键写失败时不得唤醒,实际 %v", calls)
	}
	if got := counter.get(assetop.WakeError); got != 1 {
		t.Fatalf("应计 error=1,实际 %d", got)
	}
}

// 每次唤醒有自己的超时(默认 10 秒),不继承一个没有截止时间的父 ctx 无限跑下去。
func TestWakeHasOwnTimeout(t *testing.T) {
	waker := newFakeWaker()
	s := newTestSubscriber(t, newFakeBus(), Config{}, waker, nil)

	s.handle(context.Background(), "42")
	s.wg.Wait()

	_, budgets := waker.snapshot()
	if len(budgets) != 1 || budgets[0] <= 0 || budgets[0] > DefaultWakeTimeout {
		t.Fatalf("唤醒的 ctx 应带 (0, %v] 的截止时间,实际 %v", DefaultWakeTimeout, budgets)
	}
}

// Waker 出错只记日志,不影响后续消息。
func TestWakeErrorDoesNotStopLaterWakes(t *testing.T) {
	waker := newFakeWaker()
	waker.err = errors.New("列出失败")
	s := newTestSubscriber(t, newFakeBus(), Config{}, waker, nil)

	s.handle(context.Background(), "1")
	s.wg.Wait()
	s.handle(context.Background(), "2")
	s.wg.Wait()

	if calls, _ := waker.snapshot(); len(calls) != 2 {
		t.Fatalf("前一次唤醒出错不该影响下一次,实际 %v", calls)
	}
}

// ── 生命周期 ─────────────────────────────────────────────────

// 单机形状:Start 用一次订阅订全部 64 个分片频道并分发消息;Stop 关闭订阅、等后台 goroutine 全部退出;
// Stop 之后再 Start 是空操作,重复 Stop 安全。
func TestStartStop(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	s := newTestSubscriber(t, bus, Config{}, waker, nil)

	s.Start(context.Background())
	s.Start(context.Background()) // 重复 Start 不再起第二批订阅循环
	sub := bus.waitSubscribes(t, standaloneKey, 1)
	if strings.Join(sub.channels, ",") != strings.Join(ShardChannels(), ",") {
		t.Fatalf("单机形状应一次订全部 %d 个分片频道,实际 %v", ShardCount, sub.channels)
	}
	sub.events <- fakeEvent{payload: "42"}
	if got := waitWake(t, waker, "唤醒"); got != 42 {
		t.Fatalf("应唤醒 player 42,实际 %d", got)
	}

	stopWithin(t, s)
	if attempts, _, _ := bus.counts(); attempts != 1 || sub.closes.Load() != 1 {
		t.Fatalf("应订阅一次并在 Stop 时关闭,实际 attempts=%d closes=%d", attempts, sub.closes.Load())
	}

	s.Start(context.Background())
	stopWithin(t, s)
	if attempts, _, _ := bus.counts(); attempts != 1 {
		t.Fatalf("Stop 之后 Start 应是空操作,实际又订阅了(共 %d 次)", attempts)
	}
}

// 集群形状:每组(每个 slot)一个订阅循环,64 组各订一次、合起来恰好是全部分片频道;任何一片上的消息都会唤醒。
func TestStartSubscribesEveryShardGroup(t *testing.T) {
	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	waker := newFakeWaker()
	s := newTestSubscriber(t, bus, Config{}, waker, nil)
	defer stopWithin(t, s)

	s.Start(context.Background())
	for _, channel := range ShardChannels() {
		sub := bus.waitSubscribes(t, channel, 1)
		if len(sub.channels) != 1 || sub.channels[0] != channel {
			t.Fatalf("组 %s 应只订它自己,实际 %v", channel, sub.channels)
		}
	}

	// player 130 在第 2 片。
	bus.waitSubscribes(t, ShardChannel(130%ShardCount), 1).events <- fakeEvent{payload: "130"}
	if got := waitWake(t, waker, "第 2 片上的唤醒"); got != 130 {
		t.Fatalf("应唤醒 player 130,实际 %d", got)
	}
	if attempts, _, _ := bus.counts(); attempts != ShardCount {
		t.Fatalf("64 组各订一次,实际 %d 次", attempts)
	}
}

// 迁槽:服务端对一组频道推来 sunsubscribe(连接本身还活着)。那一组关掉旧订阅、让客户端重读拓扑、重新订阅,
// 之后的唤醒照常收到;别的组不受影响。
func TestResubscribeAfterServerUnsubscribe(t *testing.T) {
	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	waker := newFakeWaker()
	s := newTestSubscriber(t, bus, Config{}, waker, nil)
	defer stopWithin(t, s)

	s.Start(context.Background())
	for _, channel := range ShardChannels() {
		bus.waitSubscribes(t, channel, 1)
	}
	moved := ShardChannel(17)
	first := bus.waitSubscribes(t, moved, 1)
	first.events <- fakeEvent{err: fmt.Errorf("%w: channel=%s", errServerUnsubscribed, moved)}

	second := bus.waitSubscribes(t, moved, 2)
	waitClosed(t, first, "被退订的那次订阅关闭")
	if _, refreshes, _ := bus.counts(); refreshes < 1 {
		t.Fatal("被服务端退订之后必须先让客户端重读拓扑,再去新属主上重订")
	}
	for _, channel := range ShardChannels() {
		if channel != moved && bus.subscribeCount(channel) != 1 {
			t.Fatalf("没被退订的组 %s 不应重订,实际订了 %d 次", channel, bus.subscribeCount(channel))
		}
	}

	second.events <- fakeEvent{payload: "81"} // 81 % 64 = 17
	if got := waitWake(t, waker, "重订之后的唤醒"); got != 81 {
		t.Fatalf("应唤醒 player 81,实际 %d", got)
	}
}

// 订阅断开(断线、健康检查无回音)后按退避重新订阅,新订阅上的消息照常处理。
func TestResubscribeAfterSubscriptionDrops(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	s := newTestSubscriber(t, bus, Config{}, waker, nil)
	defer stopWithin(t, s)

	s.Start(context.Background())
	first := bus.waitSubscribes(t, standaloneKey, 1)
	first.events <- fakeEvent{err: errHealthCheckTimeout}

	second := bus.waitSubscribes(t, standaloneKey, 2)
	waitClosed(t, first, "断开的那次订阅关闭")
	second.events <- fakeEvent{payload: "9"}
	if got := waitWake(t, waker, "重订阅后的唤醒"); got != 9 {
		t.Fatalf("应唤醒 player 9,实际 %d", got)
	}
}

// 订阅一直订不上(Redis 不可用、属主回 MOVED):每次失败都让客户端重读拓扑,按退避一直重试,恢复后照常收消息。
func TestResubscribeRetriesUntilSubscribeSucceeds(t *testing.T) {
	bus := newFakeBus()
	bus.failSubscribes = 3
	waker := newFakeWaker()
	s := newTestSubscriber(t, bus, Config{}, waker, nil)
	defer stopWithin(t, s)

	s.Start(context.Background())
	sub := bus.waitSubscribes(t, standaloneKey, 1)
	attempts, refreshes, _ := bus.counts()
	if attempts != 4 || refreshes != 3 {
		t.Fatalf("失败 3 次后第 4 次订上,每次失败重读一次拓扑;实际 attempts=%d refreshes=%d", attempts, refreshes)
	}
	sub.events <- fakeEvent{payload: "5"}
	if got := waitWake(t, waker, "恢复后的唤醒"); got != 5 {
		t.Fatalf("应唤醒 player 5,实际 %d", got)
	}
}

// Stop 要等在途唤醒结束:取消会传到唤醒的 ctx,唤醒返回之后 Stop 才返回。
func TestStopWaitsInFlightWake(t *testing.T) {
	bus := newFakeBus()
	waker := newFakeWaker()
	waker.block = make(chan struct{}) // 永不放行:只能靠 ctx 取消返回
	s := newTestSubscriber(t, bus, Config{}, waker, nil)

	s.Start(context.Background())
	bus.waitSubscribes(t, standaloneKey, 1).events <- fakeEvent{payload: "7"}
	waitWake(t, waker, "唤醒开始")

	stopWithin(t, s)
	if waker.returned.Load() != 1 {
		t.Fatal("Stop 返回时在途唤醒必须已经结束")
	}
}

// 父 ctx 结束也会让各订阅循环退出,之后 Stop 立即返回。
func TestStartStopsOnParentCancel(t *testing.T) {
	bus := newFakeBus()
	bus.grouping = oneGroupPerChannel
	s := newTestSubscriber(t, bus, Config{}, newFakeWaker(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	subs := make([]*fakeSub, 0, ShardCount)
	for _, channel := range ShardChannels() {
		subs = append(subs, bus.waitSubscribes(t, channel, 1))
	}
	cancel()
	stopWithin(t, s)
	for i, sub := range subs {
		if sub.closes.Load() != 1 {
			t.Fatalf("退出时每组的订阅都应关闭,第 %d 组 closes=%d", i, sub.closes.Load())
		}
	}
}

// 没 Start 就 Stop、nil 接收者:都不 panic、不阻塞。
func TestStopWithoutStartAndNilReceiver(t *testing.T) {
	s := newTestSubscriber(t, newFakeBus(), Config{}, newFakeWaker(), nil)
	stopWithin(t, s)

	var nilSub *Subscriber
	nilSub.Start(context.Background())
	nilSub.Stop()
}
