package kafka

// durable_enqueue.go 是「Kafka 位点提交之前、任务唯一副本首次写进 Redis」的入队层(S1 修订稿 §1,
// docs/design/db-retry-owner-routing.md §7)。
//
// 适用范围只有三类入队点,别的写一律不走这里:
//   - deferFailedTask 的 Kafka 分支:Kafka 源任务失败,副本写进重试 ready;
//   - quarantineOrderingConflict 的 Kafka 分支:Kafka 源任务隔离进死信(orderingIncomparable / stale_topic /
//     missing_required 写);
//   - 毒消息(persistPoisonMessage):ConsumeClaim 分发前解码失败,以及 handleTask / processTaskBatch 解码失败。
//
// 这三处之后 Kafka 位点就会提交,Redis 里这一份是任务唯一的副本。只 LPUSH 成功不够:everysec 下 Redis 崩溃最多丢
// 约 1 秒的写,位点却已经前进,存盘就此丢失。所以开启 WAITAOF 时,只有 LPUSH 已按配置落盘(WAITAOF numlocal
// numreplicas 满足)才以 nil 回调,调用方据此 ack;确认不足、超时、出错一律 fail-closed:不 ack、位点不提交,
// claim 失败后由新属主从未确认的 offset 起重投。
//
// 为什么重试源的搬移不走这里(moveRetryReceipt、deadLetterRetry、ackRetryReceipt、分拣与搬回脚本):它们是单脚本
// 原子搬移,前一状态本身就在 Redis 里,脚本作为整体复制;丢了只回到前一状态(收据还在 ready / processing),
// 不是「唯一副本首次落地」。
//
// WAITAOF 不保证什么(不得据此表述为「Redis 崩溃安全」):游标 consumer:applied:*、owner epoch、收据 ack(LREM)、
// 重试源搬移仍是主池上的普通写,everysec 下崩溃最多丢约 1 秒。丢了之后游标可能回退、已 ack 的收据可能重新出现在
// processing,而 MySQL 与 Kafka 位点都已前进,旧 seq 的重放会把 MySQL 覆盖回旧档;这一残余只能由 MySQL 条件写封死。
//
// 重投幂等依据(前提是游标没有随 Redis 一起回退):Kafka 重投与组提交重推产生的多份载荷带相同的
// (seq=offset+1, 原始分区);执行时 applied cursor 判 seq ≤ 已落库即 orderingStale 丢弃并 ack 收据,批内同 seq
// 视为重复投递,owner epoch 只升不降;死信里的重复条目按 taskID 或 (topic, partition, offset) 人工去重。
//
// 为什么用专用客户端 + 组提交 + 异步确认:
//   - 主池只有 10 条连接(GOMAXPROCS=1 × 10),排序锁、游标、落点 MGET 都在上面;WAITAOF 在 everysec 下平均占连接
//     约 0.5s,放进主池会在 MySQL 故障(每条写都走入队)时饿死整条热路径。专用客户端 PoolSize=2:写协程的粘连连接
//     一条,启动探针一条。
//   - 一批 LPUSH 与一次 WAITAOF 走同一条粘连连接的同一个 pipeline:WAITAOF 等的是「本连接」的写偏移,换了连接就
//     覆盖不到。份数不足时在同一连接上只重发 WAITAOF(写偏移不变,不产生重复 LPUSH);网络错误时连接作废重建,
//     未确认的请求在新连接上重推(可能重复,见上面的幂等依据)。
//   - 同步等确认会让失败任务占住子 shard 约 0.5~1s(正常任务约 35ms 的 14~28 倍),峰值时 1% 失败就吃掉剩余余量。
//     所以 submit 只做内存入队就返回,确认在写协程里完成后回调 done,worker 接着处理下一条。位点仍只按连续前缀提交
//     (claimAcker):未确认的 offset 把本分区已提交位点停在它前面,后面完成的任务越不过它;确认后前缀一次推进,
//     未确认则 failKafkaTask,本 claim 不再标记任何位点。确认到达的等待上界 = 排队时间 + 单批预算 5s。
//   - 吞吐上限约为每次 fsync 一批(≤256 条且 ≤8MiB),单实例最坏约 256 条/s;超出时 submit 背压阻塞 worker
//     (待确认总字节 ≤32MiB、通道 ≤1024 条),效果等同同步写法,表现为 Kafka lag,不丢数据。
//
// claim 收尾时等确认的 offset 提交不提交,按收尾路径分(ConsumeClaim):
//   - 停机排空(drainClaimForShutdown)与「消息通道关闭、会话仍在」的排空:入队层仍在,acker 不作废,确认到了照常
//     标记,并在会话释放时提交。停机排空上限 shutdownDrainTimeout 默认取 claimDrainTimeout(10s),大于上面的等待上界。
//   - 再平衡、交接让出、失败收尾(drainAfterStop):第一步 acker.stop 作废未标记的位点。之后仍会等这些确认回调到达
//     (acker.pending,上限 drainTimeout),但到了也不再标记:等确认的延后 offset 及其后已完成的 offset 一律由新属主
//     重投,靠游标与 seq 吸收(见上面的幂等依据)。入队层里那份副本照样落地,新属主重投时若再失败还会再入队一份,
//     重复的重试副本同样由游标与 seq 吸收。
//   - 作废范围是本实例的全部 claim:sarama v1.43.1 任一 ConsumeClaim 返回都会取消整个会话(consumer_group.go 里
//     每个分区 goroutine 都 defer sess.cancel()),一个分区确认失败让 claim 返回 error 后,本实例其余 claim 都会走
//     「session canceled → drainAfterStop」。
//   - 量级:与同步 LPUSH 相比,每个延后 offset 的未标记窗口拉长到约一次 fsync,所以每次再平衡多重投约「每分区一次
//     fsync 间隔的流量」;MySQL 故障期间(每条写都走入队)每次再平衡还会多出同量的重复重试副本。数据不丢。
//   - 要让再平衡也「等确认再提交」,得在 session canceled 与交接让出这两个非失败原因下改成先 waitClaimDrained、
//     再 acker.stop。这会改变「再平衡立即撤 claimCtx」的既有语义,须另立设计,本层不做。
//
// 生命周期:入队层独立于 c.ctx(内部 ctx 只由 close 取消),Stop 的最后一步才 close;停机排空期间 claim 还在等的
// 确认照常完成。

import (
	"context"
	db_config "db/internal/config"
	"db/internal/metrics"
	"errors"
	"fmt"
	"shared/drainproof"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// durableEnqueueBudget 是组提交一批的总预算(从开始 flush 算起,含重试与退避);aofAck.budget 为 0 时取它。
	durableEnqueueBudget = 5 * time.Second
	// durableEnqueueQueueSize 是写协程请求通道的容量。
	durableEnqueueQueueSize = 1024
	// durableEnqueueMaxBatch / durableEnqueueMaxBatchBytes 是一批的条数与累计字节上限(取下一条之前先判,
	// 所以一批最多超出字节上限一条;单条超过上限的载荷单独成一批)。
	durableEnqueueMaxBatch      = 256
	durableEnqueueMaxBatchBytes = 8 << 20
	// durableEnqueueMaxPendingBytes 是已交给入队层、尚未完成的载荷总字节上限。db Pod 内存 limit 512Mi,只靠 1024 的
	// 条数上限最坏约 1GiB(玩家行最大 1MiB)。超出时 submit 背压阻塞;pendingBytes==0 时任何单条都放行。
	durableEnqueueMaxPendingBytes = 32 << 20

	// 专用客户端参数(S1 修订稿 §1.4)。ReadTimeout = WaitAOFTimeoutMs + durableClientReadTimeoutSlack,
	// 保证 WAITAOF 的服务端超时先于客户端读超时到期;MaxRetries=-1 关掉 go-redis 对整条 pipeline 的自动重发
	// (它会在超时 / EOF 时把 LPUSH 再发一遍),重试由本层在预算内自己做。
	durableClientPoolSize         = 2
	durableClientWriteTimeout     = 3 * time.Second
	durableClientReadTimeoutSlack = time.Second
	// durableProbeTimeoutSlack:启动探针的 ctx 超时 = WaitAOFTimeoutMs + 它。
	durableProbeTimeoutSlack = 2 * time.Second
	// waitAOFDeadlineMargin:每轮 WAITAOF 的 timeout 至少比批次截止早这么多,给回包与收尾留余量。
	waitAOFDeadlineMargin = 100 * time.Millisecond
)

// durableEnqueueBackoff 是重试间隔:directEnqueue 恰好按它试 4 次(同原 pushPayloadDurably);组提交在批次预算内
// 按它退避,用完后一直取最后一档(1s),直到截止。
var durableEnqueueBackoff = []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond, time.Second}

// 错误哨兵。计数口径见 durableEnqueueResult:canceled 先判,其次 not_durable,其余非 nil 为 error。
var (
	// errEnqueueNotDurable:单批预算内没拿到足够的落盘确认(份数不足、网络错误重试到预算耗尽)。计 not_durable。
	errEnqueueNotDurable = errors.New("enqueue not confirmed durable within budget")
	// errWaitAOFRejected:Redis 对 WAITAOF 回了错误回复(appendonly no、Redis < 7.2 等),确定性错误,不重试。计 error。
	errWaitAOFRejected = errors.New("WAITAOF rejected by redis")
	// errUnexpectedWaitAOFReply:WAITAOF 回复形状不对,不重试。计 error。取值与 data_service 同名哨兵一致。
	errUnexpectedWaitAOFReply = errors.New("unexpected WAITAOF reply")
	// errDurableEnqueueCanceled:调用方 ctx 已取消 / 超时,或入队层已关闭(停机)。计 canceled,不进直方图,
	// 告警不看它;调用方打 INFO、照样不 ack。
	errDurableEnqueueCanceled = errors.New("durable enqueue canceled")
	// errDurableEnqueueNotStarted:aofGroupEnqueue 还没 start 就 submit(编程错误;直接失败而不是排队,
	// 防止单测忘了 start 导致 done 永远不来)。计 error。
	errDurableEnqueueNotStarted = errors.New("durable enqueue used before start")
	// errDurableEnqueueClosed 是 errDurableEnqueueCanceled 的一种:入队层已 close(或正在 close)。
	errDurableEnqueueClosed = fmt.Errorf("%w: enqueue layer closed", errDurableEnqueueCanceled)
)

// durableEnqueue 只用于「Kafka 位点提交之前、任务唯一副本首次写进 Redis」的入队点。
type durableEnqueue interface {
	// submit 把 payload LPUSH 进 queueKey,并按配置确认落盘。done 恰好调用一次:
	//   nil   = 副本已按配置确认(关闭 WAITAOF 时 = LPUSH 成功);
	//   非 nil = 未确认,调用方不得 ack;errors.Is(err, errDurableEnqueueCanceled) 表示停机或 ctx 取消。
	// done 可能在 submit 返回之前同步执行(directEnqueue 一律如此;其它实现在失败分支也可能如此),
	// 也可能在写协程里执行。done 必须快、不得阻塞、不得再调 submit。
	// ctx 只约束「交给入队层」这一步(含背压等待),不约束确认过程。
	submit(ctx context.Context, target, queueKey string, payload []byte, done func(error))
	start(ctx context.Context) error // 启动探针;aofGroupEnqueue 同时起写协程(写协程生命周期只由 close 结束)
	close()                          // 幂等;返回时,所有已 submit 请求的 done 都已调用过
}

var (
	_ durableEnqueue = directEnqueue{}
	_ durableEnqueue = (*aofGroupEnqueue)(nil)
)

// enqueueCanceled 把调用方 ctx 的错误包成 canceled。
func enqueueCanceled(ctxErr error) error {
	return fmt.Errorf("%w: %w", errDurableEnqueueCanceled, ctxErr)
}

// durableEnqueueResult 把 submit 的结果归到 db_durable_enqueue_total 的 result 取值。canceled 必须先判:
// 停机时的取消不得被计成 error(否则每次滚动都会误触 critical 告警 DbDurableEnqueueNotConfirmed)。
func durableEnqueueResult(err error) string {
	switch {
	case err == nil:
		return metrics.DurableOK
	case errors.Is(err, errDurableEnqueueCanceled):
		return metrics.DurableCanceled
	case errors.Is(err, errEnqueueNotDurable):
		return metrics.DurableNotDurable
	default:
		return metrics.DurableError
	}
}

// observeDurableEnqueue 记一次入队结果;d 从 submit 开始计到 done。canceled 不进直方图由 metrics 侧负责。
func observeDurableEnqueue(target string, err error, d time.Duration) {
	metrics.ObserveDurableEnqueue(target, durableEnqueueResult(err), d)
}

// ── directEnqueue ───────────────────────────────────────────────────────────

// directEnqueue 只 LPUSH、不发 WAITAOF:关闭 WAITAOF(两项都显式写 0)时用,也是 c.durable==nil
// (直接拼结构体的单测)时的默认实现。submit 同步执行,done 在 submit 返回前调用 —— 现有单测依赖同步 ack。
type directEnqueue struct {
	rc redis.Cmdable
}

func (d directEnqueue) submit(ctx context.Context, target, queueKey string, payload []byte, done func(error)) {
	started := time.Now()
	err := d.push(ctx, queueKey, payload)
	observeDurableEnqueue(target, err, time.Since(started))
	done(err)
}

// push 是原 pushPayloadDurably 的逻辑:退避 0/100ms/500ms/1s,LPUSH 用调用方 ctx。只要 ctx.Err()!=nil
// (尝试之前、退避等待中、或 LPUSH 失败时)就以 canceled 返回。4 次都失败时返回最后一次的原始错误,计 error
// (组提交路径上同样的网络错误用尽预算计 not_durable,见 metrics.DurableNotDurable)。
func (d directEnqueue) push(ctx context.Context, queueKey string, payload []byte) error {
	var lastErr error
	for _, delay := range durableEnqueueBackoff {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return enqueueCanceled(ctx.Err())
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return enqueueCanceled(err)
		}
		if lastErr = d.rc.LPush(ctx, queueKey, payload).Err(); lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return enqueueCanceled(err)
		}
	}
	return lastErr
}

func (directEnqueue) start(context.Context) error { return nil }

func (directEnqueue) close() {}

// ── 配置 ─────────────────────────────────────────────────────────────────────

// aofAck 是 WAITAOF 的确认要求。budget 为 0 时取 durableEnqueueBudget(只给单测缩短)。
type aofAck struct {
	numLocal    int
	numReplicas int
	timeout     time.Duration
	budget      time.Duration
}

// enabled 为 false(两项都是 0)时不发 WAITAOF。
func (a aofAck) enabled() bool {
	return a.numLocal > 0 || a.numReplicas > 0
}

func (a aofAck) flushBudget() time.Duration {
	if a.budget > 0 {
		return a.budget
	}
	return durableEnqueueBudget
}

// aofAckFromConfig 从 ServerConfig.RedisClient 取确认要求。Normalize 已把 WaitAOFTimeoutMs==0 填成默认值、
// Validate 拒绝负数;这里对 ≤0 再兜一次默认值,只为没走 Normalize 的调用方(单测)不至于发出 timeout=0 的 WAITAOF
// (0 表示无限阻塞)。
func aofAckFromConfig(cfg db_config.RedisConfig) aofAck {
	timeoutMs := cfg.WaitAOFTimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = db_config.DefaultRedisWaitAOFTimeoutMs
	}
	return aofAck{
		numLocal:    cfg.NumLocal(),
		numReplicas: cfg.NumReplicas(),
		timeout:     time.Duration(timeoutMs) * time.Millisecond,
	}
}

// durableClientOptions 是专用客户端的连接参数(S1 修订稿 §1.4);单测用同一份参数连 miniredis。
func durableClientOptions(addr, password string, db int, ack aofAck) *redis.Options {
	return &redis.Options{
		Addr:             addr,
		Password:         password,
		DB:               db,
		DisableIndentity: true, // 同 svc.NewServiceContext:Redis < 7.2 不认 CLIENT SETINFO
		PoolSize:         durableClientPoolSize,
		MaxRetries:       -1,
		ReadTimeout:      ack.timeout + durableClientReadTimeoutSlack,
		WriteTimeout:     durableClientWriteTimeout,
	}
}

// newDurableEnqueue 按配置选入队实现。两项都写 0 时退回 directEnqueue(只 LPUSH)并打一行 ERROR:
// 这时 Kafka ack 之前的副本没有落盘确认。否则建专用客户端做组提交;调用方必须先 start(探针失败即拒启)。
func newDurableEnqueue(cfg db_config.RedisConfig, main redis.Cmdable) durableEnqueue {
	if !cfg.WaitsForAOF() {
		logx.Errorf("retry enqueue WAITAOF disabled (WaitAOFNumLocal=0 and WaitAOFNumReplicas=0): enqueued copies are not confirmed durable before Kafka ack")
		return directEnqueue{rc: main}
	}
	ack := aofAckFromConfig(cfg)
	return newAOFGroupEnqueue(redis.NewClient(durableClientOptions(cfg.Hosts, cfg.Password, cfg.DB, ack)), ack)
}

// ── aofGroupEnqueue ─────────────────────────────────────────────────────────

// enqueueReq 是一条入队请求。reserved 是它预留的待确认字节:由当前持有请求的一方读写(submit 在交给通道之前,
// 写协程在收到之后),经 channel 交接,无需加锁。
type enqueueReq struct {
	target    string
	queueKey  string
	payload   []byte
	done      func(error)
	submitted time.Time
	reserved  int
}

// aofGroupEnqueue 是开启 WAITAOF 时的组提交实现:submit 只做内存入队,写协程攒批后在一条粘连连接上
// 「LPUSH×k + WAITAOF」,确认后逐条回调 done。
//
// done 恰好一次的论证:请求要么从未进入 reqs,由 submit 自己的失败分支完成;要么被写协程读到,由写协程完成。
// close 的顺序是「置 stopped → cancel 内部 ctx → submitters.Wait() → close(reqs) → 等写协程退出」:置 stopped 之后
// 不再有新的 submitter 登记(登记与置位在同一把锁下),cancel 让卡在背压或通道发送上的 submitter 以 canceled 退出,
// 所以 close(reqs) 一定晚于最后一次发送;写协程读完通道里剩下的请求(以 canceled 完成)才退出。
type aofGroupEnqueue struct {
	rc  *redis.Client // 专用客户端,close 时关闭
	ack aofAck

	reqs            chan *enqueueReq
	maxBatch        int
	maxBatchBytes   int
	maxPendingBytes int

	mu           sync.Mutex
	pendingBytes int
	// spaceFreed 在每次释放字节时 close 并换新,背压中的 submitter 据此重试预留。
	spaceFreed chan struct{}
	stopped    bool
	started    bool

	// submitters 计正在 submit 里(已登记、尚未把请求交出或失败返回)的调用方。Add 只在 mu 下、stopped 为 false 时做,
	// 所以全部 Add 都先于 close 里的 Wait。
	submitters sync.WaitGroup

	// ctx 是入队层自己的生命周期,只由 close 取消,**不从 c.ctx 派生**:停机时 c.ctx 先取消,
	// 而 claim 排空还在等已提交请求的确认。
	ctx     context.Context
	cancel  context.CancelFunc
	runDone chan struct{}

	closeOnce sync.Once

	// conn 是写协程长期持有的粘连连接(WAITAOF 只覆盖本连接的写),网络错误时关闭重建。
	// 只由写协程读写;close 在写协程退出之后才碰它。
	conn *redis.Conn
}

// newAOFGroupEnqueue 建组提交入队层并接管 rc(close 时关闭)。三个上限取默认常量,单测可在 start 之前改小。
func newAOFGroupEnqueue(rc *redis.Client, ack aofAck) *aofGroupEnqueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &aofGroupEnqueue{
		rc:              rc,
		ack:             ack,
		reqs:            make(chan *enqueueReq, durableEnqueueQueueSize),
		maxBatch:        durableEnqueueMaxBatch,
		maxBatchBytes:   durableEnqueueMaxBatchBytes,
		maxPendingBytes: durableEnqueueMaxPendingBytes,
		spaceFreed:      make(chan struct{}),
		ctx:             ctx,
		cancel:          cancel,
		runDone:         make(chan struct{}),
	}
}

// start 先用专用客户端的池连接(不是粘连连接)发一次启动探针,失败返回 error,Start 据此拒启;成功后起写协程。
// 重复调用是空操作;close 之后调用返回 errDurableEnqueueClosed。
func (q *aofGroupEnqueue) start(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, q.ack.timeout+durableProbeTimeoutSlack)
	defer cancel()
	if err := probeAOFAck(probeCtx, q.rc, q.ack); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return errDurableEnqueueClosed
	}
	if q.started {
		return nil
	}
	q.started = true
	go q.run()
	logx.Infof("retry enqueue WAITAOF enabled: numlocal=%d numreplicas=%d timeout=%s batchBudget=%s",
		q.ack.numLocal, q.ack.numReplicas, q.ack.timeout, q.ack.flushBudget())
	return nil
}

func (q *aofGroupEnqueue) submit(ctx context.Context, target, queueKey string, payload []byte, done func(error)) {
	req := &enqueueReq{target: target, queueKey: queueKey, payload: payload, done: done, submitted: time.Now()}
	if err := ctx.Err(); err != nil {
		q.complete(req, enqueueCanceled(err))
		return
	}

	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		q.complete(req, errDurableEnqueueClosed)
		return
	}
	if !q.started {
		q.mu.Unlock()
		q.complete(req, errDurableEnqueueNotStarted)
		return
	}
	q.submitters.Add(1)
	q.mu.Unlock()
	defer q.submitters.Done()

	if err := q.reserve(ctx, len(payload)); err != nil {
		q.complete(req, err)
		return
	}
	req.reserved = len(payload)

	select {
	case q.reqs <- req:
		// 之后 req 归写协程,这里不再碰它。
	case <-ctx.Done():
		q.complete(req, enqueueCanceled(ctx.Err()))
	case <-q.ctx.Done():
		q.complete(req, errDurableEnqueueClosed)
	}
}

// reserve 预留 n 字节的待确认额度:pendingBytes==0 或加上 n 不超上限时放行,否则等别的请求完成释放额度。
func (q *aofGroupEnqueue) reserve(ctx context.Context, n int) error {
	for {
		q.mu.Lock()
		if q.pendingBytes == 0 || q.pendingBytes+n <= q.maxPendingBytes {
			q.pendingBytes += n
			q.mu.Unlock()
			return nil
		}
		freed := q.spaceFreed
		q.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			return enqueueCanceled(ctx.Err())
		case <-q.ctx.Done():
			return errDurableEnqueueClosed
		}
	}
}

func (q *aofGroupEnqueue) releaseBytes(n int) {
	q.mu.Lock()
	q.pendingBytes -= n
	close(q.spaceFreed)
	q.spaceFreed = make(chan struct{})
	q.mu.Unlock()
}

// complete 结束一条请求,顺序固定:释放字节 → 计数 → 调 done。
func (q *aofGroupEnqueue) complete(r *enqueueReq, err error) {
	if r.reserved > 0 {
		q.releaseBytes(r.reserved)
		r.reserved = 0
	}
	observeDurableEnqueue(r.target, err, time.Since(r.submitted))
	r.done(err)
}

func (q *aofGroupEnqueue) completeAll(reqs []*enqueueReq, err error) {
	for _, r := range reqs {
		q.complete(r, err)
	}
}

// run 是写协程:取到第一条后非阻塞地继续攒批,条数达到 maxBatch、累计字节 ≥ maxBatchBytes、或通道暂空即停,
// 然后 flush。通道由 close 关闭后退出;内部 ctx 已取消时读到的请求一律以 canceled 完成。
func (q *aofGroupEnqueue) run() {
	defer close(q.runDone)
	for first := range q.reqs {
		if q.ctx.Err() != nil {
			q.complete(first, errDurableEnqueueClosed)
			continue
		}
		batch := []*enqueueReq{first}
		batchBytes := len(first.payload)
	collect:
		for len(batch) < q.maxBatch && batchBytes < q.maxBatchBytes {
			select {
			case next, ok := <-q.reqs:
				if !ok {
					break collect
				}
				batch = append(batch, next)
				batchBytes += len(next.payload)
			default:
				break collect
			}
		}
		q.flush(batch)
	}
}

// flush 在一条粘连连接上把一批请求推进 Redis 并等落盘确认,批次预算(默认 5s)内按 durableEnqueueBackoff 退避重试:
//   - 每轮 WAITAOF 的 timeout = min(配置值, 截止前剩余 − 100ms),不足 1ms 不发(0 = 无限阻塞),剩余请求以
//     errEnqueueNotDurable 完成 —— 预算耗尽一律是 not_durable,不会被判成 canceled;
//   - needPush 时同一个 pipeline 里先逐条 LPUSH、最后跟 WAITAOF;否则只发 WAITAOF;
//   - LPUSH 的 Redis 错误回复只让那一条失败;网络错误时整条 pipeline 结果未知,连接作废重建,未完成的全部重推;
//   - WAITAOF 错误回复 → errWaitAOFRejected、回复形状错 → errUnexpectedWaitAOFReply,都不重试;网络错误同上重建
//     并重推(新连接的写偏移覆盖不到旧连接的写);份数不足 → 同一连接只重发 WAITAOF;满足 → 全部成功;
//   - 内部 ctx 取消(close)时剩余请求以 canceled 完成。在途的 pipeline 不随 ctx 中断(未开 ContextTimeoutEnabled),
//     最多等专用客户端的 ReadTimeout(WaitAOFTimeoutMs + 1s)。
func (q *aofGroupEnqueue) flush(batch []*enqueueReq) {
	budget := q.ack.flushBudget()
	deadline := time.Now().Add(budget)
	pending := batch
	needPush := true
	var lastErr error
	for attempt := 0; len(pending) > 0; attempt++ {
		if attempt > 0 && !q.backoff(attempt, deadline) {
			q.completeAll(pending, errDurableEnqueueClosed)
			return
		}
		if q.ctx.Err() != nil {
			q.completeAll(pending, errDurableEnqueueClosed)
			return
		}
		waitMs, ok := waitAOFBudgetMs(time.Now(), deadline, true, q.ack.timeout, waitAOFDeadlineMargin)
		if !ok {
			q.completeAll(pending, notDurableError(budget, attempt, lastErr))
			return
		}

		var pushCmds []*redis.IntCmd
		var waitCmd *redis.Cmd
		// 不看 Pipelined 的返回值:它只是第一条失败命令的错误,逐条命令的结果才是判据。
		_, _ = q.stickyConn().Pipelined(q.ctx, func(p redis.Pipeliner) error {
			if needPush {
				pushCmds = make([]*redis.IntCmd, 0, len(pending))
				for _, r := range pending {
					pushCmds = append(pushCmds, p.LPush(q.ctx, r.queueKey, r.payload))
				}
			}
			waitCmd = p.Do(q.ctx, "WAITAOF", q.ack.numLocal, q.ack.numReplicas, waitMs)
			return nil
		})
		if waitCmd == nil {
			// 回调没被调用:一条命令都没发出去。按网络错误处理(连接重建、needPush 保持 true)。
			lastErr = errors.New("pipeline callback was not invoked")
			q.resetConn(lastErr)
			needPush = true
			continue
		}

		if needPush {
			broken := false
			kept := pending[:0]
			for i, r := range pending {
				err := pushCmds[i].Err()
				switch {
				case err == nil:
					kept = append(kept, r)
				case isRedisErrorReply(err):
					q.complete(r, fmt.Errorf("LPUSH %s rejected (deterministic, not retried): %w", r.queueKey, err))
				default:
					broken = true
					lastErr = fmt.Errorf("LPUSH %s: %w", r.queueKey, err)
					kept = append(kept, r)
				}
			}
			pending = kept
			if broken {
				q.resetConn(lastErr)
				continue // needPush 保持 true:新连接上全部重推
			}
			if len(pending) == 0 {
				return
			}
		}

		acks, waitErr := waitCmd.Slice()
		if waitErr != nil {
			if isRedisErrorReply(waitErr) {
				q.completeAll(pending, fmt.Errorf("%w (deterministic, not retried; needs appendonly yes and Redis >= 7.2): %w",
					errWaitAOFRejected, waitErr))
				return
			}
			lastErr = fmt.Errorf("WAITAOF: %w", waitErr)
			q.resetConn(lastErr)
			needPush = true // 新连接的写偏移覆盖不到旧连接上的 LPUSH,只能重推
			continue
		}
		if ackErr := checkWaitAOFReply(acks, q.ack.numLocal, q.ack.numReplicas); ackErr != nil {
			if errors.Is(ackErr, errUnexpectedWaitAOFReply) {
				q.completeAll(pending, fmt.Errorf("WAITAOF (not retried): %w", ackErr))
				return
			}
			lastErr = ackErr
			needPush = false // 同一连接、写偏移不变:只重发 WAITAOF,不重复 LPUSH
			continue
		}
		q.completeAll(pending, nil)
		return
	}
}

// notDurableError 是预算耗尽时的错误。lastErr 用 %v 拼进文本而不包进链:结果只能被判成 not_durable。
func notDurableError(budget time.Duration, attempts int, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("%w: budget %s exhausted after %d attempt(s)", errEnqueueNotDurable, budget, attempts)
	}
	return fmt.Errorf("%w: budget %s exhausted after %d attempt(s); last attempt: %v", errEnqueueNotDurable, budget, attempts, lastErr)
}

// backoff 按 durableEnqueueBackoff 等第 attempt 次重试(用完取最后一档),不越过批次截止;
// 内部 ctx 被取消时返回 false。
func (q *aofGroupEnqueue) backoff(attempt int, deadline time.Time) bool {
	delay := durableEnqueueBackoff[min(attempt, len(durableEnqueueBackoff)-1)]
	if remaining := time.Until(deadline); remaining < delay {
		delay = remaining
	}
	if delay <= 0 {
		return q.ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-q.ctx.Done():
		return false
	}
}

func (q *aofGroupEnqueue) stickyConn() *redis.Conn {
	if q.conn == nil {
		q.conn = q.rc.Conn()
	}
	return q.conn
}

// resetConn 作废粘连连接(网络错误后它的状态未知),下一轮取新连接。
func (q *aofGroupEnqueue) resetConn(cause error) {
	if q.conn == nil {
		return
	}
	_ = q.conn.Close()
	q.conn = nil
	if q.ctx.Err() == nil {
		logx.Errorf("durable enqueue connection reset; unconfirmed copies will be pushed again on a new connection: %v", cause)
	}
}

// close 幂等;返回时所有已 submit 请求的 done 都已调用过。顺序见类型注释。
func (q *aofGroupEnqueue) close() {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.stopped = true
		started := q.started
		q.mu.Unlock()

		// 正式排空由外层 drainEnqueue 等待 pending；底层关闭仍须及时中止失败重试。
		q.cancel()
		q.submitters.Wait()
		close(q.reqs)
		if started {
			<-q.runDone
		}
		if q.conn != nil {
			_ = q.conn.Close()
			q.conn = nil
		}
		if err := q.rc.Close(); err != nil {
			logx.Errorf("durable enqueue redis client close failed: %v", err)
		}
	})
}

// ── WAITAOF 探针与纯函数 ─────────────────────────────────────────────────────

// probeAOFAck 是启动期的 WAITAOF 能力探针:按配置发一次 WAITAOF(timeout = WaitAOFTimeoutMs),报错或确认份数不足
// 就返回 error,Start 据此拒启。未开启时什么都不发,直接返回 nil。
//
// 为什么在启动期挡:WAITAOF 的配置错误(appendonly no 回 ERR、Redis < 7.2 回 unknown command、NumReplicas 写了 1
// 而拓扑里没有副本)是确定性的,留到运行期就是每一次入队都 fail-closed、对应分区反复停滞。
// 探针用的连接上没有写过东西,本地确认会立即返回;timeout 仍取配置值而不是 1ms,免得池里某条刚写过的连接
// 赶上 everysec 的 fsync 间隙被误判。
func probeAOFAck(ctx context.Context, rc redis.Cmdable, ack aofAck) error {
	if !ack.enabled() {
		return nil
	}
	timeoutMs := ack.timeout.Milliseconds()
	if timeoutMs < 1 {
		// WAITAOF 的 timeout 为 0 表示无限阻塞,不能发。
		return fmt.Errorf("WAITAOF probe: timeout %s is below 1ms (ServerConfig.RedisClient.WaitAOFTimeoutMs)", ack.timeout)
	}
	var waitCmd *redis.Cmd
	_, pipeErr := rc.Pipelined(ctx, func(p redis.Pipeliner) error {
		waitCmd = p.Do(ctx, "WAITAOF", ack.numLocal, ack.numReplicas, timeoutMs)
		return nil
	})
	if waitCmd == nil {
		if pipeErr == nil {
			pipeErr = errors.New("pipeline callback was not invoked")
		}
		return fmt.Errorf("WAITAOF probe not sent: %w", pipeErr)
	}
	acks, err := waitCmd.Slice()
	if err != nil {
		return fmt.Errorf("WAITAOF %d %d %d failed (needs appendonly yes and Redis >= 7.2; set ServerConfig.RedisClient.WaitAOFNumLocal/WaitAOFNumReplicas to 0 only where no AOF exists): %w",
			ack.numLocal, ack.numReplicas, timeoutMs, err)
	}
	if err := checkWaitAOFReply(acks, ack.numLocal, ack.numReplicas); err != nil {
		return fmt.Errorf("WAITAOF probe (check ServerConfig.RedisClient.WaitAOFNumLocal / WaitAOFNumReplicas against the topology): %w", err)
	}
	return nil
}

// 以下三个纯函数与 go/data_service/internal/routing/route_publish.go 的同名函数语义逐字一致(那边服务 player_route
// 发布的 WAITAOF),**两处要改一起改**。不为两段小函数新开 go/shared 包,也不动 data_service 的代码。

// isRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误)。网络错误、ctx 错误、熔断拒绝都不是。
func isRedisErrorReply(err error) bool {
	var re redis.Error
	return errors.As(err, &re)
}

// checkWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数。
// 份数不足回普通错误(调用方按 errEnqueueNotDurable 重试);形状不对回 errUnexpectedWaitAOFReply(不重试)。
// 纯函数,单测直接测。
func checkWaitAOFReply(vals []any, numLocal, numReplicas int) error {
	if len(vals) != 2 {
		return fmt.Errorf("%w: %d elements %v, want [numlocal, numreplicas]", errUnexpectedWaitAOFReply, len(vals), vals)
	}
	local, localOK := vals[0].(int64)
	replicas, replicasOK := vals[1].(int64)
	if !localOK || !replicasOK || local < 0 || replicas < 0 {
		return fmt.Errorf("%w: %v", errUnexpectedWaitAOFReply, vals)
	}
	if local < int64(numLocal) || replicas < int64(numReplicas) {
		return fmt.Errorf("WAITAOF acked numlocal=%d numreplicas=%d, want at least %d and %d", local, replicas, numLocal, numReplicas)
	}
	return nil
}

// waitAOFBudgetMs 算本次 WAITAOF 的 timeout(毫秒):min(configured, 截止前剩余 − margin),
// **先截断成整毫秒再判断**。结果小于 1 时 ok=false,调用方不得发送 —— WAITAOF 的 timeout 为 0 表示无限阻塞。
// 没有截止时间时取 configured。时间由参数注入,纯函数。
func waitAOFBudgetMs(now, deadline time.Time, hasDeadline bool, configured, margin time.Duration) (int64, bool) {
	budget := configured
	if hasDeadline {
		if remaining := deadline.Sub(now) - margin; remaining < budget {
			budget = remaining
		}
	}
	ms := budget.Milliseconds()
	if ms < 1 {
		return 0, false
	}
	return ms, true
}

// drainEnqueue把done本身纳入join。回调全部返回以后才允许retry owner归还或报告QUIESCED。
type drainEnqueue struct {
	inner   durableEnqueue
	pending *drainproof.Session
	mu      sync.Mutex
	err     error
}

func (q *drainEnqueue) start(ctx context.Context) error { return q.inner.start(ctx) }
func (q *drainEnqueue) submit(ctx context.Context, target, key string, body []byte, callback func(error)) {
	done, err := q.pending.Enter()
	if err != nil {
		callback(err)
		return
	}
	q.inner.submit(ctx, target, key, body, func(err error) {
		defer done()
		if err != nil && q.pending.Closed() {
			q.mu.Lock()
			q.err = errors.Join(q.err, err)
			q.mu.Unlock()
		}
		callback(err)
	})
}
func (q *drainEnqueue) close() {
	q.pending.CloseAdmission()
	_ = q.pending.Wait(context.Background())
	q.inner.close()
}
func (q *drainEnqueue) drainError() error { q.mu.Lock(); defer q.mu.Unlock(); return q.err }
