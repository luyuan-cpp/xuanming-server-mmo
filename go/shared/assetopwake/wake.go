// Package assetopwake 把 scene 的「玩家落地」信号接到资产重投循环上
// (docs/design/landing-recovery-sync-digest.md §5.3)。
//
// scene 在玩家进场完成时向控制集群发布 `SPUBLISH assetop:wake:{aw:<player_id % 64>} <player_id 十进制>`
// (分片 Pub/Sub,docs/design/redis-cluster-all-domains-20261110.md §4.6);guild / trade / data_service 各自订阅,
// 抢到跨副本去重键的那个副本立即对该玩家的 PENDING 行走一遍 Claim + ProcessOne(assetop.Loop.WakePlayer),
// 不再等最长 60 秒的退避或 30 分钟的停放档。
//
// 唤醒**只是提示**:订阅断线、去重键写失败、并发已满时直接丢弃,行照常按原退避重投,只慢不错(设计 L-4)。
// 所以单条唤醒失败都不重试、不阻塞订阅循环;断掉的订阅由订阅循环按退避重新订阅。
//
// 为什么单独成包:assetop 按约定不引 Redis 客户端(它的定位交给 shared/scenenode),
// 订阅与去重是 Redis 的事,放在这里。
//
// 全部代码未编译,待 Codex 验证(AGENTS §10.1)。
package assetopwake

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/assetop"
	"shared/redisslot"
	"shared/safego"
)

// 唤醒频道按 player_id % ShardCount 分片,频道名是 ChannelPrefix 加 {aw:<片号>}(片号十进制、不补零),
// 例如 player 130 发在 "assetop:wake:{aw:2}"。
//
// 发布方是 C++ scene,规则写在 cpp/libs/services/scene/player/system/player_landing.h
// (kAssetWakeChannelPrefix、kAssetWakeShardCount、AssetWakeChannel);两边逐字一致由 wake_test.go 的
// TestChannelMatchesCpp 守住,族前缀 aw 由 shared/redisslot 的 TestControlFamiliesMatchCpp 守住。
//
// 为什么分片:控制集群上普通 PUBLISH 经集群总线广播到全部节点,不随节点数扩展;SPUBLISH 只在频道所在 slot 的那组
// 节点里传播。64 片落在 64 个不同的 slot 上,随 slot 散到各主节点(设计 §4.6)。
//
// 片数与资产通道的工作分片(shared/workshard,片号 = player_id % 64)取同一规则:缺口第 25 条落地后,每个副本只订
// 自己负责的那几片,两边的片号必须一致(wake_test.go 核对 ShardCount == workshard.Shards)。
// 上线后不能改:scene 与订阅方必须同时换,混跑期间只换了一边的唤醒全部落空。
const (
	ShardCount    = 64
	ChannelPrefix = "assetop:wake:"
)

// ShardChannel 返回第 shard 片的唤醒频道。shard 越界是调用方的实现错误,panic。
func ShardChannel(shard uint64) string {
	if shard >= ShardCount {
		panic(fmt.Sprintf("assetopwake: wake shard %d out of range [0, %d)", shard, ShardCount))
	}
	return ChannelPrefix + redisslot.ControlTag(redisslot.ControlFamilyAssetWake, strconv.FormatUint(shard, 10))
}

// ShardChannels 按片号顺序返回全部唤醒频道。
func ShardChannels() []string {
	channels := make([]string, ShardCount)
	for shard := range channels {
		channels[shard] = ShardChannel(uint64(shard))
	}
	return channels
}

// dedupKeyFormat 是跨副本去重键:assetop:wake:{服务名}:{player_id}。
// 每个服务一把:guild 与 trade 各自都要处理这次落地,不能互相去重。
// 它是不带 tag 的单键(SET NX EX),在控制集群上按自己的 slot 落点,与频道没有同槽要求。
const dedupKeyFormat = "assetop:wake:%s:%d"

// DedupKey 返回某服务、某玩家这次唤醒的去重键。
func DedupKey(service string, playerID uint64) string {
	return fmt.Sprintf(dedupKeyFormat, service, playerID)
}

// 默认值。去重窗口 5 秒对齐 scene 侧「同一实体 5 秒内只发一次」的合并窗口(设计 §5.2):
// 同一次落地的重复发布(重连、gate 补发)落在一个窗口里,每个服务只有一个副本去查库。
//
// 唤醒超时 10 秒的来历:Loop.WakePlayer 只在剩余预算还够一整个 OpBudget(默认 2.5 秒)时才开始下一行,
// 所以真正用来「开始新行」的只有 WakeTimeout − OpBudget。一行常见耗时约 100–150ms(首答多为未落盘,
// 还要等一次重查),10 秒能开始约 50 行,盖得住一个玩家最多 32 行的上限;库或 scene 变慢时剩下的行
// 照常等各自的 next_attempt_ms。原先的 3 秒只够开始 0.5 秒内的几行,而且行会被截在半路。
// 代价是 Stop 的最坏等待随之变长(见 Stop)。
const (
	DefaultConcurrency = 4
	DefaultDedupTTL    = 5 * time.Second
	DefaultWakeTimeout = 10 * time.Second
)

const (
	// dedupTimeout 是写去重键那一次 Redis 往返的上限,与 WakeTimeout 分开计:
	// 去重卡住不该吃掉唤醒本身的预算。
	dedupTimeout = time.Second
	// resubscribeBaseDelay / resubscribeMaxDelay 是订阅断开(没订上、断线、健康检查无回音、服务端退订)之后
	// 重新订阅的退避:第 n 次连续失败等 base·2^(n-1),封顶 max,再加抖动(见 retryPolicy)。
	// 一个副本有多个订阅组(集群上每个 slot 一组),Redis 整体不可用时不能每组每秒重连一次。
	resubscribeBaseDelay = time.Second
	resubscribeMaxDelay  = 30 * time.Second
	// logEveryFailures:同一组连续失败时只在第 1 次与之后每这么多次打一条 ERROR,集群整体不可用时不刷屏。
	logEveryFailures = 10
	// maxLoggedPayload 是非法消息写进日志时保留的最大字节数:负载来自网络,不能原样整段打进日志。
	maxLoggedPayload = 32
)

// errServerUnsubscribed:服务端主动退订了这组频道,连接本身还活着。集群把 slot 迁走,或节点降成别的分片的副本时
// 会推来 sunsubscribe;不重订就再也收不到这几片的唤醒,而且不报任何错。
var errServerUnsubscribed = errors.New("assetopwake: shard channels unsubscribed by the server (slot moved)")

// Waker 是唤醒的落点,生产实现是 *assetop.Loop。
type Waker interface {
	WakePlayer(ctx context.Context, playerID uint64) (int, error)
}

// Counter 记 assetop_wake_total。生产传 *assetop.Metrics,而且必须是 Loop 用的**同一个**实例:
// 同名指标在一个注册表里只能注册一次,订阅端与 Loop 各管一段 result(见 assetop.WakeResult)。
type Counter interface {
	IncWake(assetop.WakeResult)
}

// Config 是订阅端的装配参数。零值字段取默认值。
type Config struct {
	// Service 进去重键,低基数常量(guild / trade),必填。
	Service string
	// Concurrency 是本副本同时在跑的唤醒数上限;满了直接丢(计 dropped)。默认 4。
	// 它只限「同时在查库、在等名额」的唤醒个数;真正压向 scene 的并发仍由 Loop 的 Workers 预算封顶。
	Concurrency int
	// DedupTTL 是去重键的存活时间(SET NX EX)。默认 5 秒。
	DedupTTL time.Duration
	// WakeTimeout 是每次 WakePlayer 的独立超时。默认 10 秒。实际应不小于 2 × Loop 的 OpBudget:
	// WakePlayer 先列行(约一次库往返,≤ 1 秒),之后剩余预算要够一整个 OpBudget 才领下一行。
	// 配得比 OpBudget 小,每次唤醒都回错误;只比 OpBudget 略大,列完行后一行也领不到,静默退化成从不唤醒。
	// 本包只校验非负,下界由装配方的配置校验负责(guild 的 validateLanding 要求 ≥ 2 × OpBudgetMs)。
	WakeTimeout time.Duration
}

func (c Config) withDefaults() (Config, error) {
	if c.Service == "" {
		return c, errors.New("assetopwake: Service 必填(进去重键)")
	}
	if c.Concurrency < 0 || c.DedupTTL < 0 || c.WakeTimeout < 0 {
		return c, fmt.Errorf("assetopwake: 参数不得为负(Concurrency=%d DedupTTL=%v WakeTimeout=%v)",
			c.Concurrency, c.DedupTTL, c.WakeTimeout)
	}
	if c.Concurrency == 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.DedupTTL == 0 {
		c.DedupTTL = DefaultDedupTTL
	}
	if c.WakeTimeout == 0 {
		c.WakeTimeout = DefaultWakeTimeout
	}
	return c, nil
}

// bus 是本包用到的全部 Redis 能力。生产实现是 goRedisBus(redis.go);单测注入替身,
// 便于确定性地驱动订阅、退订、去重与并发满载(要精确控制消息到达时序;miniredis v2.35 也不支持 SSUBSCRIBE)。
type bus interface {
	// groups 把全部唤醒频道分成订阅组:一组走一条订阅连接、一条 SSUBSCRIBE。集群上一条 SSUBSCRIBE 只能订同一个
	// slot 的频道,一组就是一个 slot;单机没有这条限制,全部频道一组、一条连接。结果在构造时取一次,之后不变。
	groups(channels []string) [][]string
	// subscribe 订阅一组频道,每个频道都确认之后才返回这次订阅;没确认就返回错误,不留下要关闭的东西。
	// ctx 结束时这次订阅的连接随之关闭。
	subscribe(ctx context.Context, channels []string) (subscription, error)
	// refreshTopology 在订阅出错或被服务端退订之后调用:集群客户端重读 slot 分布,下一次订阅才连得到新的属主;
	// 单机是空操作。
	refreshTopology(ctx context.Context)
	// claimDedup 即 SET key 1 NX EX ttl:true = 本副本拿到这次唤醒。
	claimDedup(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// subscription 是一组频道的一次订阅,只由创建它的那个订阅循环使用。
type subscription interface {
	// receive 阻塞到下一条唤醒消息,返回它的负载。订阅不再可用时返回非 nil 错误:服务端退订是 errServerUnsubscribed,
	// 断线、健康检查无回音与其余错误原样返回。ctx 结束时尽快返回,返回值不再有意义。
	receive(ctx context.Context) (string, error)
	// close 关闭这次订阅的连接,可重复调用。
	close() error
}

// retryPolicy 是订阅断开之后重新订阅的退避:第 n 次连续失败(n ≥ 1)等 base·2^(n-1),封顶 max,
// 再取 [d/2, d) 里的随机值(equal jitter)。抖动是为了集群整体恢复时,成百上千个副本的订阅不在同一刻重连。
type retryPolicy struct {
	base time.Duration
	max  time.Duration
}

// delay 返回第 failures 次连续失败之后的等待。jitter(n) 返回 [0, n) 里的随机数;生产传 rand.Int64N。
func (p retryPolicy) delay(failures int, jitter func(int64) int64) time.Duration {
	d := p.max
	if failures < 1 {
		failures = 1
	}
	// 左移超过 30 位以后不再算:那时早已封顶,继续移位会溢出成负数。
	if shift := failures - 1; shift < 30 {
		if v := p.base << shift; v > 0 && v < p.max {
			d = v
		}
	}
	half := d / 2
	if d-half <= 0 {
		return d
	}
	return half + time.Duration(jitter(int64(d-half)))
}

// Subscriber 订阅全部唤醒频道并把唤醒交给 Waker。
//
// 生命周期:New → Start(ctx) → Stop()。Start / Stop 都可重复调用;Stop 之后 Start 是空操作。
// 每个订阅组一个订阅循环;所有后台 goroutine(订阅循环与在途唤醒)都记在 wg 里,Stop 返回时它们都已退出。
//
// 缺口第 25 条(资产重投按分片分工)落地之前,每个副本都订全部 64 片,每条唤醒仍投给每个订阅副本;
// 扇出随 zone 数增长的问题见设计 §4.6,本包不解决。
type Subscriber struct {
	bus     bus
	cfg     Config
	waker   Waker
	counter Counter

	// groups 是订阅组(bus.groups 的结果,构造时核过「每个频道恰好一次」),每组一个订阅循环。构造后只读。
	groups [][]string
	// retry 是重订的退避。构造后只读(单测在 Start 之前改小)。
	retry retryPolicy

	// slots 是在途唤醒的名额,容量 = Concurrency;拿不到就丢(不排队:唤醒越排越旧越没用)。
	slots chan struct{}

	// mu 只保护 started / stopped / cancel,让 Start 与 Stop 无论怎么交错都不会在 wg.Wait 之后再 wg.Add。
	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New 组装订阅端。client 是 scene SPUBLISH 唤醒的那一套 Redis(控制集群;本机与尚未切到控制集群的部署是单实例,
// 走同一条路径);clusterredis.New 返回的单机 Client 与 ClusterClient 都行。分片 Pub/Sub 要求 Redis ≥ 7.0。
// 指错实例的表现是永远收不到唤醒、只剩原退避,不会报错 —— 所以装配处要按设计 §2 核对。
// counter 可为 nil(不计数)。
func New(client redis.UniversalClient, cfg Config, waker Waker, counter Counter) (*Subscriber, error) {
	if client == nil {
		return nil, errors.New("assetopwake: 缺少 Redis 客户端")
	}
	return newSubscriber(goRedisBus{c: client}, cfg, waker, counter)
}

func newSubscriber(b bus, cfg Config, waker Waker, counter Counter) (*Subscriber, error) {
	if waker == nil {
		return nil, errors.New("assetopwake: 缺少 Waker(assetop.Loop)")
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if counter == nil {
		counter = noopCounter{}
	}
	groups := b.groups(ShardChannels())
	if err := validateGroups(groups); err != nil {
		return nil, err
	}
	return &Subscriber{
		bus:     b,
		cfg:     cfg,
		waker:   waker,
		counter: counter,
		groups:  groups,
		retry:   retryPolicy{base: resubscribeBaseDelay, max: resubscribeMaxDelay},
		slots:   make(chan struct{}, cfg.Concurrency),
	}, nil
}

// validateGroups 核对订阅组恰好覆盖每个唤醒频道一次、没有空组:漏掉的那几片会静默收不到唤醒,构造时就拒绝。
func validateGroups(groups [][]string) error {
	want := make(map[string]bool, ShardCount)
	for _, channel := range ShardChannels() {
		want[channel] = false
	}
	for _, group := range groups {
		if len(group) == 0 {
			return errors.New("assetopwake: empty subscription group")
		}
		for _, channel := range group {
			seen, known := want[channel]
			if !known {
				return fmt.Errorf("assetopwake: subscription group has unknown channel %q", channel)
			}
			if seen {
				return fmt.Errorf("assetopwake: channel %q appears in more than one subscription group", channel)
			}
			want[channel] = true
		}
	}
	for channel, seen := range want {
		if !seen {
			return fmt.Errorf("assetopwake: channel %q is not in any subscription group", channel)
		}
	}
	return nil
}

// Start 起各订阅组的订阅循环。ctx 取消或 Stop 都会让它们退出;重复调用、Stop 之后再调都是空操作。nil 接收者安全。
func (s *Subscriber) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	for _, group := range s.groups {
		s.wg.Add(1)
		safego.Go("assetopwake.subscribe", func() {
			defer s.wg.Done()
			s.run(runCtx, group)
		})
	}
	logx.Infof("[AssetOpWake] Subscribing to wake shard channels prefix=%s shards=%d subscriptions=%d service=%s concurrency=%d",
		ChannelPrefix, ShardCount, len(s.groups), s.cfg.Service, s.cfg.Concurrency)
}

// Stop 取消订阅并等所有在途唤醒结束。取消会让在途唤醒立刻停止领新行,正在处理的那一行的投递
// 随之结束,只剩 assetop 落库预留的一小段;最坏(某一步不理会取消)也不超过 WakeTimeout 加这一小段。
// 可重复调用;nil 接收者安全。
func (s *Subscriber) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// run 保持一组频道的订阅:断了(没订上、断线、健康检查无回音、服务端退订)就让客户端重读拓扑,
// 按退避等一会儿再订,直到 ctx 结束。
func (s *Subscriber) run(ctx context.Context, group []string) {
	failures := 0 // 连续失败次数;订阅确认之后清零
	for {
		err := s.consume(ctx, group, &failures)
		if ctx.Err() != nil {
			return
		}
		failures++
		s.bus.refreshTopology(ctx)
		delay := s.retry.delay(failures, rand.Int64N)
		logInterruption(group, failures, delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// consume 订阅一次并逐条分发,直到 ctx 结束(返回 nil)或订阅不再可用(返回原因)。订阅确认之后把 *failures 清零。
func (s *Subscriber) consume(ctx context.Context, group []string, failures *int) error {
	sub, err := s.bus.subscribe(ctx, group)
	if err != nil {
		return err
	}
	defer func() { _ = sub.close() }()
	if *failures > 0 {
		logx.Infof("[AssetOpWake] Wake shard subscription restored channels=%s after %d interruption(s)",
			describeGroup(group), *failures)
	}
	*failures = 0

	for {
		payload, err := sub.receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.handle(ctx, payload)
	}
}

// logInterruption 记一次订阅中断。服务端退订(迁槽)是运维的正常动作,记 INFO;其余按连续失败的次数限频记 ERROR。
func logInterruption(group []string, failures int, delay time.Duration, err error) {
	channels := describeGroup(group)
	if errors.Is(err, errServerUnsubscribed) {
		logx.Infof("[AssetOpWake] Wake shard subscription unsubscribed by the server; resubscribing channels=%s in %v",
			channels, delay)
		return
	}
	if failures == 1 || failures%logEveryFailures == 0 {
		logx.Errorf("[AssetOpWake] Wake shard subscription interrupted channels=%s attempt=%d; resubscribing in %v (wakes in between fall back to backoff): %v",
			channels, failures, delay, err)
	}
}

// describeGroup 给日志用:一组只有一个频道时写全名,多个时写第一个加剩余个数(单机一组 64 个)。
func describeGroup(group []string) string {
	switch len(group) {
	case 0:
		return "<none>"
	case 1:
		return group[0]
	default:
		return fmt.Sprintf("%s(+%d more)", group[0], len(group)-1)
	}
}

// handle 处理一条消息。它跑在订阅循环里,必须不阻塞:解析与拿名额都是本地操作,
// 去重与唤醒放进独立 goroutine。各订阅组的循环并发调用它,名额与计数都是并发安全的。
func (s *Subscriber) handle(ctx context.Context, payload string) {
	s.counter.IncWake(assetop.WakeReceived)

	playerID, err := ParsePlayerID(payload)
	if err != nil {
		s.counter.IncWake(assetop.WakeError)
		logx.Errorf("[AssetOpWake] Dropped malformed wake message payload=%q: %v", truncate(payload, maxLoggedPayload), err)
		return
	}

	select {
	case s.slots <- struct{}{}:
	default:
		// 名额满:丢。去重键还没写,别的副本仍可以接下这次唤醒。
		s.counter.IncWake(assetop.WakeDropped)
		return
	}
	// 调用方(订阅循环)自己记在 wg 里且还没退出,这里的 Add 不会与 Stop 的 Wait 撞上「计数为 0 时 Add」。
	s.wg.Add(1)
	safego.Go("assetopwake.wake", func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		s.wake(ctx, playerID)
	})
}

// wake 抢去重键,抢到才唤醒。去重键写失败时不唤醒:那多半是 Redis 本身出了问题,
// 而唤醒丢了只是退回原退避;反过来不去重地唤醒,每次落地每个副本都要查一次库。
func (s *Subscriber) wake(ctx context.Context, playerID uint64) {
	dedupCtx, cancelDedup := context.WithTimeout(ctx, dedupTimeout)
	claimed, err := s.bus.claimDedup(dedupCtx, DedupKey(s.cfg.Service, playerID), s.cfg.DedupTTL)
	cancelDedup()
	if err != nil {
		s.counter.IncWake(assetop.WakeError)
		logx.Errorf("[AssetOpWake] Failed to write wake dedup key; wake skipped player_id=%d: %v", playerID, err)
		return
	}
	if !claimed {
		s.counter.IncWake(assetop.WakeDeduped)
		return
	}

	wakeCtx, cancel := context.WithTimeout(ctx, s.cfg.WakeTimeout)
	defer cancel()
	// woke / none / error 由 Loop.WakePlayer 自己计,这里只留日志。
	if n, err := s.waker.WakePlayer(wakeCtx, playerID); err != nil {
		logx.Errorf("[AssetOpWake] Wake failed; rows fall back to backoff player_id=%d processed=%d: %v", playerID, n, err)
	}
}

// ParsePlayerID 解析唤醒负载:十进制、非零的 uint64,不容忍前后空白与正负号。
// scene 发布的就是 `%llu`,任何别的形状都说明发布方有 bug,丢掉比猜一个 id 安全。
func ParsePlayerID(payload string) (uint64, error) {
	id, err := strconv.ParseUint(payload, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("assetopwake: player_id 不是十进制无符号整数: %w", err)
	}
	if id == 0 {
		return 0, errors.New("assetopwake: player_id 为 0")
	}
	return id, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type noopCounter struct{}

func (noopCounter) IncWake(assetop.WakeResult) {}
