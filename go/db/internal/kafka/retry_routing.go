package kafka

// 重试只交给分区属主(docs/design/db-retry-owner-routing.md,存盘管线 S1 ④)。
//
// 属主判据只有一个:本进程此刻有没有这个分区的 sarama claim(ConsumeClaim 正在跑、claimCtx 未取消)。不用 Setup /
// Cleanup 判属主:Setup 早于 ConsumeClaim 开始,Cleanup 晚于排空结束。ConsumeClaim 进出时在 ownedPartitions 里登记 /
// 摘除一份 claimLease(登记放在 claimCtx 最终派生完成之后、主循环之前),重试协程只对存活的租约认领。
//
// 「claim 活着 ⇒ 本进程是该分区此刻唯一的执行者」只在 coordinator 没有逐出本成员时成立(R7 复核)。sarama v1.43.1 默认
// Range + eager,新属主要等全组退出上一代 claim 才拿到分区 —— 前提是上一代自己参加了再平衡。本成员到 coordinator 的
// 连接半开或 coordinator 卡顿时,coordinator 在 Session.Timeout(10s)后把本成员逐出、完成再平衡,新属主拿到分区并开始
// 执行;本进程这边的心跳请求要等满 Net.ReadTimeout(sarama 缺省 30s)才失败,之后每次重试同样受 30s 约束、最多
// Metadata.Retry.Max+1 次,才取消会话。所以从被逐出到本进程察觉,存在约 Net.ReadTimeout(旧连接半开、重连正常时,
// 重连后的心跳立即回 UnknownMemberId)到 (Metadata.Retry.Max+1)×Net.ReadTimeout(到 coordinator 持续分区时)的双属主
// 窗口:本进程的 claimCtx 与租约仍然存活,继续从 p{P} 认领执行,与新属主并发。再平衡排空超时(drainAfterStop 等满
// drainTimeout)之后仍在跑的重试也是同类重叠。这两种重叠的正确性都只靠排序锁与 applied 游标兜底(与 shared 模式相同;
// 每条收据经 RPOPLPUSH 只会被一个实例认领),两边的执行都计 executed,DbRetryNonOwnerExecutionInOwnerZone 看不见;
// 心跳与位点提交失败经 sarama 的错误通道计 db_consumer_group_errors_total 并限速打 ERROR(consumer_group_errors.go),
// 用来定位窗口。不调小 Net.ReadTimeout:sarama 读 JoinGroup 响应也用它,长再平衡时会超时。
//
// 收据(重试载荷)任一时刻恰在下面一处(派发给 worker 的那份在途任务,收据仍留在认领者的 processing 里):
//   - 共享收件箱 kafka:retry:queue:{topic}:回收、停机归还、旧版实例、人工重投、shared 模式的失败都进这里;
//   - 按原始分区的 ready kafka:retry:queue:{topic}:p{N}:owner 模式的分拣与失败写回的目的地;
//   - 某实例的 processing kafka:retry:processing:{topic}:{instance}(retry_ownership.go);
//   - 旧版共享 processing kafka:retry:processing:{topic}(只回收、不写入);
//   - 死信 kafka:dead:queue:{topic}。
//
// 所有搬移都由单个 Lua 脚本原子完成(分拣、认领、搬回、进死信)。脚本动手之前先核对涉及的键全是 list(或不存在),
// 否则整体报错、一条不动 —— Redis 的脚本没有回滚,不能让 RPOP 之后的 LPUSH 因 WRONGTYPE 失败把收据弄丢。
//
// RetryRouting=owner:每拍先把收件箱按载荷 v2 头里的原始分区分拣进 p{N}(坏载荷进死信),再只对本进程存活的租约从
// p{P} 认领。认领到的任务带上租约的 claimCtx 与在途回执(retryDone = lease.exit),派给分区 P 的 worker,与该分区的
// 新消息在同一个子 shard 串行;handleTask 在入口与 SQL 之前两次复核 claim,撤销则原样搬回 p{origin}、不执行、不耗预算。
// 拿不到分区的实例(热备)只分拣与回收孤儿。重试的 SQL 及其后的 Redis 步骤仍用 w.ctx:已经开始的重试持锁跑完,
// 比半途撤掉更安全。
// RetryRouting=shared:今天的语义,另外每拍兜底排空 p*(从 owner 回滚的路径)。
//
// 已认领进本实例 processing、该搬走却因 Redis 出错没搬成的收据(撤销搬回、misrouted、失败写回、进死信)登记进待重搬表
// (retry_stranded.go),重试协程每拍在分拣与认领之前重搬;搬不动时它们仍留在本实例 processing,停机 release 照旧兜底。
// 本实例续租着、reclaimOnce 跳过自己,没有这张表时它们要等重启才回到收件箱。
//
// 回收、停机归还、旧共享列表一律回共享收件箱,由 owner 实例重新分拣(回收与路由解耦)。回退到不认识 p* 的旧版之前
// 必须先切 shared 并等 p* 归零,或执行 go/db/cmd/retry_requeue(RequeuePartitionedRetries)。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	db_config "db/internal/config"
	"db/internal/metrics"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// retryForwardBatch 是分拣脚本一次调用最多搬的条数(脚本执行期间 Redis 是阻塞的);一拍的总上限仍是
	// min(retryDrainBudgetPerTick, 拍开始时收件箱的长度)。
	retryForwardBatch = 100
	// retryQueueScanCount 是 SCAN p* 每页的 COUNT 提示(只影响往返次数,不影响结果完整性)。
	retryQueueScanCount = 1000
)

// 搬回工具(go/db/cmd/retry_requeue)的 -batch:一次 requeuePartitionScript 调用最多搬几条(脚本执行期间 Redis 是阻塞的)。
// 导出给工具做参数校验与缺省值,范围只在这里定义一次;RequeuePartitionedRetries 自己也按它校验。
const (
	RequeueDefaultBatch = 100
	RequeueMaxBatch     = 1000
)

// ─────────────────────────── 键名 ───────────────────────────
//
// tools/merge_zone/preflight.go 的 dbRetryQueueKey / dbRetryPartitionQueuePrefix / dbRetryPartitionQueuePattern 按同一套
// 键名查(merge_zone 是另一个模块,不能直接引用这里);两边要改一起改,retry_routing_test.go 按字面量钉住。

// retryReadyKey 是共享 ready;owner 模式下它是「未分拣收件箱」。
func retryReadyKey(topic string) string { return "kafka:retry:queue:" + topic }

// retryDeadKey 是死信列表(与 NewKeyOrderedKafkaConsumer 里的 retryDeadQueueKey 同一个键)。
func retryDeadKey(topic string) string { return "kafka:dead:queue:" + topic }

// retryPartitionReadyKeyPrefix 是按分区 ready 的公共前缀 kafka:retry:queue:{topic}:p。
// 紧跟 topic 的是 ":p",所以 db_task_zone_1 的前缀不是 db_task_zone_11 / db_task_zone_1_g2 的前缀。
func retryPartitionReadyKeyPrefix(topic string) string { return retryReadyKey(topic) + ":p" }

// retryPartitionReadyKey 是原始分区 partition 的 ready:kafka:retry:queue:{topic}:p{十进制分区号}。
func retryPartitionReadyKey(topic string, partition int32) string {
	return retryPartitionReadyKeyPrefix(topic) + strconv.FormatInt(int64(partition), 10)
}

// retryPartitionReadyKeys 返回 p0..p{n-1}(下标 = 分区号);n ≤ 0 时返回 nil。
func retryPartitionReadyKeys(topic string, n int32) []string {
	if n <= 0 {
		return nil
	}
	keys := make([]string, n)
	for p := int32(0); p < n; p++ {
		keys[p] = retryPartitionReadyKey(topic, p)
	}
	return keys
}

// parseRetryPartitionReadyKey 从 p 键解出分区号。只认 retryPartitionReadyKey 拼得出来的规范形状:前缀逐字相同,
// 后缀是不带符号、不带前导零的十进制 int32。其余(p01、p-1、p1:x、别的 topic)一律 ok=false,由调用方当成多余键处理。
func parseRetryPartitionReadyKey(topic, key string) (int32, bool) {
	suffix, found := strings.CutPrefix(key, retryPartitionReadyKeyPrefix(topic))
	if !found || suffix == "" || len(suffix) > 10 {
		return 0, false
	}
	if len(suffix) > 1 && suffix[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(suffix, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(v), true
}

// retryPartitionReadyKeyPattern 是 SCAN MATCH 用的 glob:前缀先做 glob 转义,再接 "*"。
func retryPartitionReadyKeyPattern(topic string) string {
	return escapeRedisGlob(retryPartitionReadyKeyPrefix(topic)) + "*"
}

// escapeRedisGlob 转义 Redis glob 的元字符 * ? [ ] \,让字面量原样参与 MATCH(同 tools/merge_zone 的同名函数)。
// 现网 topic 名不含这些字符;转义是为了 topic 名里出现元字符时不至于扫到别的 zone 的键或漏扫。
func escapeRedisGlob(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// retryPartitionKeyOf 返回原始分区 partition 的 p 键;越界(或没有 p 键,即直接拼结构体的单测)时返回共享收件箱与 false。
func (c *KeyOrderedKafkaConsumer) retryPartitionKeyOf(partition int32) (string, bool) {
	if partition >= 0 && int(partition) < len(c.retryPartitionKeys) {
		return c.retryPartitionKeys[partition], true
	}
	return c.retryQueueKey, false
}

// ─────────────────────────── 模式 ───────────────────────────

// resolveConsumerRetryRouting 校验消费者构造时拿到的路由模式(已经过 db_config.ResolveRetryRouting,这里再兜一道):
// "" 视同 shared;owner 要求 PartitionCnt > 0(p{N} 键按它生成,没有键就无从分拣)。
func resolveConsumerRetryRouting(routing string, partitionCount int32) (string, error) {
	switch routing {
	case "", db_config.RetryRoutingShared:
		return db_config.RetryRoutingShared, nil
	case db_config.RetryRoutingOwner:
		if partitionCount <= 0 {
			return "", errors.New("RetryRouting=owner needs Kafka.PartitionCnt > 0")
		}
		return db_config.RetryRoutingOwner, nil
	default:
		return "", fmt.Errorf("unknown Kafka.RetryRouting %q (want %q or %q)",
			routing, db_config.RetryRoutingShared, db_config.RetryRoutingOwner)
	}
}

// retryRouteModeOf 把配置值映射成 db_retry_route_total / db_retry_routing_mode 的 mode label。
// 配置取值与指标 label 分属两个契约(yaml 与 stress_summarize / 告警),刻意不共用常量,在这里显式映射。
func retryRouteModeOf(routing string, hasPartitionKeys bool) string {
	if routing == db_config.RetryRoutingOwner && hasPartitionKeys {
		return metrics.RetryModeOwner
	}
	return metrics.RetryModeShared
}

// ownerRouting 报告本消费者是否按 owner 模式路由重试。零值(retryRouting 为空,或没有 p 键 —— 直接拼结构体的单测)
// 一律是 shared,与引入之前的行为相同。
func (c *KeyOrderedKafkaConsumer) ownerRouting() bool {
	return c.retryRouting == db_config.RetryRoutingOwner && len(c.retryPartitionKeys) > 0
}

// retryRouteMode 是本消费者的 mode label。
func (c *KeyOrderedKafkaConsumer) retryRouteMode() string {
	return retryRouteModeOf(c.retryRouting, len(c.retryPartitionKeys) > 0)
}

// retryRouteMode 是本 worker 的 mode label(与所属消费者一致)。
func (w *worker) retryRouteMode() string {
	return retryRouteModeOf(w.retryRouting, len(w.retryPartitionKeys) > 0)
}

// retryReadyKeyFor 是原始分区 partition 的重试写回目的地:owner 模式且分区在契约内 → p{partition},
// 否则 → 共享收件箱。Kafka 源失败入重试、重试再失败搬回、claim 撤销搬回都按它选键。
func (w *worker) retryReadyKeyFor(partition int32) string {
	if w.retryRouting == db_config.RetryRoutingOwner && partition >= 0 && int(partition) < len(w.retryPartitionKeys) {
		return w.retryPartitionKeys[partition]
	}
	return w.retryQueueKey
}

// ─────────────────────────── claim 租约 ───────────────────────────

// claimLease 是一次 ConsumeClaim 调用的属主租约:ctx 是该 claim 最终定稿的 claimCtx,inflight 是已认领、
// 还没走到终态的 owner 重试数。
//
// 属主资格 = !closed && ctx 未取消。closed 只表示「不再接新认领」(每条排空路径的第一步都是 close);ctx 取消表示
// 「属主被撤销」—— handleTask 的两次复核看的是 task.claimCtx,不是 closed。停机排空期间 claimCtx 不随会话取消
// (WithoutCancel 派生),已派发的重试照常跑完,idle 会等到它们。
//
// 为什么不用 sync.WaitGroup:enter(相当于 Add)会与排空的等待并发,WaitGroup 不允许计数为 0 时的 Add 与 Wait 并发;
// 这里用互斥锁 + 计数 + 归零 channel:先 close 再等 idle,close 之后 enter 一律失败,不存在「等待开始之后又加一」。
//
// 全部方法对 nil 接收者安全(ownedPartitions 为 nil 时 acquire 也返回一个可用的未登记租约):enter 返回 false,
// idle 返回已关闭的 channel,revoked 返回 nil(select 里永不就绪),其余为空操作或零值。
type claimLease struct {
	partition int32
	ctx       context.Context

	mu       sync.Mutex
	inflight int
	closed   bool
	idleCh   chan struct{} // inflight 为 0 时是已关闭的 channel
}

// closedSignal 是一个永远关闭的 channel:新租约的初始 idleCh 与 nil 租约的 idle()。只读,从不再关闭。
var closedSignal = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func newClaimLease(partition int32, ctx context.Context) *claimLease {
	return &claimLease{partition: partition, ctx: ctx, idleCh: closedSignal}
}

// enter 登记一条在途重试;租约已关闭或 ctx 已取消时返回 false(调用方不得认领)。
// 必须先 enter 再认领:排空先 close 再等 idle,这个顺序保证排空开始之后不会再有新认领,开始之前的认领一定被等到。
func (l *claimLease) enter() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || (l.ctx != nil && l.ctx.Err() != nil) {
		return false
	}
	if l.inflight == 0 {
		l.idleCh = make(chan struct{})
	}
	l.inflight++
	return true
}

// exit 结束一条在途重试;降到 0 时关闭 idle channel。多调(下溢)是编程错误:记 ERROR 并钳到 0,不 panic。
func (l *claimLease) exit() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight <= 0 {
		logx.Errorf("BUG: claim lease inflight underflow: partition=%d", l.partition)
		l.inflight = 0
		return
	}
	l.inflight--
	if l.inflight == 0 {
		close(l.idleCh)
	}
}

// close 让租约不再接新认领(幂等)。不取消 ctx、不等在途:等待用 idle()。
func (l *claimLease) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// live 报告租约此刻是否仍有属主资格(未关闭且 ctx 未取消)。
func (l *claimLease) live() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.closed && (l.ctx == nil || l.ctx.Err() == nil)
}

// idle 返回一个在「在途数为 0」时关闭的 channel(此刻已经为 0 时返回已关闭的 channel)。
// 返回的是调用时刻的那一个:之后再 enter 会换新 channel,所以排空必须先 close 再取 idle。
func (l *claimLease) idle() <-chan struct{} {
	if l == nil {
		return closedSignal
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.idleCh
}

// inflightCount 是此刻的在途数(单测与排空超时日志用)。
func (l *claimLease) inflightCount() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight
}

// revoked 返回 ctx 的 Done(属主被撤销时关闭);nil 租约或 nil ctx 返回 nil channel。
func (l *claimLease) revoked() <-chan struct{} {
	if l == nil || l.ctx == nil {
		return nil
	}
	return l.ctx.Done()
}

// ownedPartitions 是本进程的 claim 租约表:分区 → 当前那次 ConsumeClaim 的租约。
//
// 全部方法对 nil 接收者安全(直接拼结构体的单测、KOC 的调用点一律不判空):acquire 返回一个未登记的租约(可正常
// enter / exit / close),不计数、不改指标;release 只 close 租约,不碰表与指标;live / lookup 返回 nil。
type ownedPartitions struct {
	mu     sync.Mutex
	leases map[int32]*claimLease
}

func newOwnedPartitions() *ownedPartitions {
	return &ownedPartitions{leases: make(map[int32]*claimLease)}
}

// acquire 为分区登记一份新租约并返回它(覆盖同分区的旧条目;正常情况下 eager 再平衡不会让同一进程的两代 claim
// 重叠,覆盖只是兜底)。每次调用计一次 db_claim_acquired_total,并更新 db_owned_partitions。
// ctx 必须是 claim 最终定稿的 claimCtx(交接闸的 yieldCtx、停机用的 WithoutCancel 派生都已完成)。
func (o *ownedPartitions) acquire(partition int32, ctx context.Context) *claimLease {
	l := newClaimLease(partition, ctx)
	if o == nil {
		return l
	}
	metrics.CountClaimAcquired()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.leases[partition] = l
	metrics.SetOwnedPartitions(o.liveCountLocked())
	return l
}

// release 关闭租约,并且只在表里登记的仍是它时才摘除(同分区的新一代已经登记时不误删)。l 为 nil 时什么都不做。
func (o *ownedPartitions) release(l *claimLease) {
	if l == nil {
		return
	}
	l.close()
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if cur, ok := o.leases[l.partition]; ok && cur == l {
		delete(o.leases, l.partition)
	}
	metrics.SetOwnedPartitions(o.liveCountLocked())
}

// live 返回此刻存活的租约,按分区升序。
func (o *ownedPartitions) live() []*claimLease {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	out := make([]*claimLease, 0, len(o.leases))
	for _, l := range o.leases {
		if l.live() {
			out = append(out, l)
		}
	}
	o.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].partition < out[j].partition })
	return out
}

// lookup 返回分区的存活租约;没有或已失效时返回 nil。
func (o *ownedPartitions) lookup(partition int32) *claimLease {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if l := o.leases[partition]; l.live() {
		return l
	}
	return nil
}

// liveCountLocked 数存活的租约;调用方持有 o.mu(锁序固定为 o.mu → l.mu,claimLease 的方法从不反向加锁)。
func (o *ownedPartitions) liveCountLocked() int {
	n := 0
	for _, l := range o.leases {
		if l.live() {
			n++
		}
	}
	return n
}

// ─────────────────────────── 认领 ───────────────────────────

// claimFrom 与 claim 走同一个 retryClaimScript(先续租、再 RPOPLPUSH 进本实例 processing,同一个原子操作,
// 文件头的不变量不变),只是从调用方给出的 ready 列表认领 —— owner 模式下是 p{P}。readyKey 为空时报错,
// 不退回共享键。ready 为空时返回 redis.Nil。
//
// 每次调用带一个新 nonce,go-redis 的内部重发因此是幂等的(retry_ownership.go 文件头「认领的幂等」)。返回 redis.Nil
// 以外的错误时结果未知:记进 unresolved,由下一拍的 resolveUnresolvedClaims 把可能已认领的那条搬回 readyKey。
func (o *retryOwnership) claimFrom(ctx context.Context, readyKey string) ([]byte, error) {
	if readyKey == "" {
		return nil, errors.New("retry claim: ready key is empty")
	}
	nonce, err := newRetryGuardNonce()
	if err != nil {
		return nil, err
	}
	recordKey := retryGuardKey("claim", o.processingKey, nonce)
	value, err := o.rc.Eval(ctx, retryClaimScript,
		[]string{readyKey, o.processingKey, o.instancesKey, recordKey, o.modesKey},
		o.instanceID, o.lease.Milliseconds(), retryGuardTTL.Milliseconds(), o.routingMode).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			o.noteUnresolvedClaim(readyKey, recordKey)
		}
		return nil, err
	}
	payload, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("retry claim script returned %T, want a bulk string", value)
	}
	return []byte(payload), nil
}

// ─────────────────────────── owner 模式:分拣 ───────────────────────────

// forwardRetryScript 把共享收件箱按载荷 v2 头里的原始分区分拣进 p{N}。不经过 processing,不解析 proto、不看 RetryCount
// (那些在执行端判)。载荷格式见 wrapRetryPayload:[magic 1B][seq 8B BE][partition 4B BE][task],Lua 下标从 1 起,
// 分区在第 10..13 字节。
// KEYS[1] = 共享收件箱,KEYS[2] = 死信,KEYS[3..] = p0..p{N-1};ARGV[1] = 本次最多分拣几条。
// 返回 {moved, legacy, invalid, moved_p0, ..., moved_p{N-1}}:legacy = 非 v2 载荷,invalid = v2 但分区越界(含负数)。
const forwardRetryScript = `
for i = 1, #KEYS do
	local t = redis.call('TYPE', KEYS[i]).ok
	if t ~= 'list' and t ~= 'none' then
		return redis.error_reply('WRONGTYPE retry forward refused: ' .. KEYS[i] .. ' is a ' .. t)
	end
end
local n = #KEYS - 2
local counts = {}
for p = 1, n do
	counts[p] = 0
end
local moved, legacy, invalid = 0, 0, 0
for _ = 1, tonumber(ARGV[1]) do
	local v = redis.call('RPOP', KEYS[1])
	if not v then
		break
	end
	if string.len(v) >= 13 and string.byte(v, 1) == 2 then
		local b1, b2, b3, b4 = string.byte(v, 10, 13)
		local p = ((b1 * 256 + b2) * 256 + b3) * 256 + b4
		if p >= 2147483648 then
			p = p - 4294967296
		end
		if p >= 0 and p < n then
			redis.call('LPUSH', KEYS[3 + p], v)
			moved = moved + 1
			counts[p + 1] = counts[p + 1] + 1
		else
			redis.call('LPUSH', KEYS[2], v)
			invalid = invalid + 1
		end
	else
		redis.call('LPUSH', KEYS[2], v)
		legacy = legacy + 1
	end
end
local out = {moved, legacy, invalid}
for p = 1, n do
	out[3 + p] = counts[p]
end
return out`

// forwardSharedRetries 把共享收件箱分拣进 p{N}(owner 模式每拍一次,先于认领;冷却期照做,分拣是纯 Redis)。
// 本拍上限 = min(retryDrainBudgetPerTick, sharedLen)(sharedLen 读失败为 -1 时取前者),每次脚本调用最多
// retryForwardBatch 条。返回每个分区本拍新分拣进来的条数(下标 = 分区号;没有 p 键或什么都没搬时可能为 nil),
// consumeRetryQueueOwner 把它计入该分区本拍的认领额度,让刚回收 / 刚分拣的收据在同一拍里就能执行。
func (c *KeyOrderedKafkaConsumer) forwardSharedRetries(sharedLen int64) []int64 {
	n := len(c.retryPartitionKeys)
	if n == 0 {
		return nil
	}
	limit := int64(retryDrainBudgetPerTick)
	if sharedLen >= 0 && sharedLen < limit {
		limit = sharedLen
	}
	if limit <= 0 {
		return nil
	}
	keys := make([]string, 0, 2+n)
	keys = append(keys, c.retryQueueKey, c.retryDeadQueueKey)
	keys = append(keys, c.retryPartitionKeys...)

	perPartition := make([]int64, n)
	var moved, legacy, invalid int64
	for remaining := limit; remaining > 0; {
		if c.pullStopped() || c.ctx.Err() != nil {
			break
		}
		batch := remaining
		if batch > retryForwardBatch {
			batch = retryForwardBatch
		}
		res, err := c.redisClient.Eval(c.ctx, forwardRetryScript, keys, batch).Int64Slice()
		if err != nil {
			if c.ctx.Err() == nil {
				logx.Errorf("retry forward failed; the inbox stays unsorted until the next tick: topic=%s inbox=%s err=%v",
					c.topic, c.retryQueueKey, err)
			}
			break
		}
		if len(res) != 3+n {
			logx.Errorf("BUG: retry forward script returned %d values, want %d: topic=%s", len(res), 3+n, c.topic)
			break
		}
		moved += res[0]
		legacy += res[1]
		invalid += res[2]
		for p := 0; p < n; p++ {
			perPartition[p] += res[3+p]
		}
		handled := res[0] + res[1] + res[2]
		remaining -= handled
		if handled < batch {
			break // 收件箱空了
		}
	}

	metrics.CountRetryRouteN(metrics.RetryModeOwner, metrics.RetryRouteForwarded, int(moved))
	metrics.CountRetryRouteN(metrics.RetryModeOwner, metrics.RetryRouteDead, int(legacy+invalid))
	metrics.CountTaskDeadLetterN(metrics.DeadLetterLegacyPayload, int(legacy))
	metrics.CountTaskDeadLetterN(metrics.DeadLetterInvalidPartition, int(invalid))
	if legacy+invalid > 0 {
		logx.Errorf("retry forward moved malformed payloads to dead queue: topic=%s legacy=%d invalid_partition=%d deadQueue=%s",
			c.topic, legacy, invalid, c.retryDeadQueueKey)
	}
	return perPartition
}

// ─────────────────────────── owner 模式:认领与派发 ───────────────────────────

// consumeRetryQueueOwner 是 owner 模式的一拍:先分拣收件箱;本 zone 主库冷却中则只分拣不认领;没有存活租约(热备)
// 则到此为止。否则在每拍总预算 retryDrainBudgetPerTick 内轮询各租约,每个租约每轮认领一条,直到预算或各自额度用尽、
// 或一整圈没有进展。起点按 retryTick 轮转,免得预算总被分区号小的租约用光。
//
// 每个租约的额度 = 拍开始时 p{P} 的长度 + 本拍分拣进 p{P} 的条数(长度读失败时取总预算):认领失败的收据 LPUSH 回
// 队头、认领从队尾取,额度保证同一条收据一拍里至多被本实例认领一次(L-04 §3.5 的口径)。
// s 由 sampleRetryQueues 给出:s.shared 是收件箱长度,s.partitioned 与 c.retryPartitionKeys 下标对齐(读失败为 -1)。
func (c *KeyOrderedKafkaConsumer) consumeRetryQueueOwner(s retryQueueSample) {
	forwarded := c.forwardSharedRetries(s.shared)
	if c.homeStoreCoolingDown() {
		return
	}
	leases := c.owned.live()
	if len(leases) == 0 {
		return // 热备:分区都在别的实例手里,只分拣、回收孤儿
	}
	c.retryTick++
	start := int(c.retryTick % uint64(len(leases)))
	quota := make([]int64, len(leases))
	for i, l := range leases {
		quota[i] = ownedRetryQuota(s.partitioned, forwarded, l.partition)
	}

	budget := retryDrainBudgetPerTick
	for budget > 0 {
		progressed := false
		for k := 0; k < len(leases) && budget > 0; k++ {
			if c.pullStopped() || c.ctx.Err() != nil {
				return // 停机撤拉取之后不再认领(Stop 第①步)
			}
			if !c.routeGate.opened() {
				return // 读方就绪闸在本拍中途关上:剩下的收据留在 p{N}(nil 闸恒放行,开关关着时不走这里)
			}
			i := (start + k) % len(leases)
			if quota[i] <= 0 {
				continue
			}
			if !c.consumeOneOwnedRetry(leases[i]) {
				quota[i] = 0
				continue
			}
			quota[i]--
			budget--
			progressed = true
		}
		if !progressed {
			break
		}
	}
	if budget == 0 {
		logx.Infof("retry drain budget reached this tick: topic=%s budget=%d mode=owner", c.topic, retryDrainBudgetPerTick)
	}
}

// ownedRetryQuota 是一个租约本拍的认领额度,见 consumeRetryQueueOwner。
func ownedRetryQuota(sampled, forwarded []int64, partition int32) int64 {
	quota := int64(retryDrainBudgetPerTick)
	p := int(partition)
	if p < 0 || p >= len(sampled) || sampled[p] < 0 {
		return quota
	}
	have := sampled[p]
	if p < len(forwarded) {
		have += forwarded[p]
	}
	if have < quota {
		quota = have
	}
	return quota
}

// consumeOneOwnedRetry 从租约所在分区的 p{P} 认领一条重试,派给分区 P 的 worker。返回 false 表示本拍不再对这个租约
// 认领(p{P} 空了、认领失败、租约已关闭或已撤销、停机)。
//
// 认领之后、执行之前的每个出口都把收据原子地搬到终态或留在 processing,并恰好调一次 lease.exit():
//   - 认领为空 / 出错 → exit;
//   - 载荷解析失败、次数用完、无分区、分区越界 → parseClaimedRetry 已原子处理(死信 / 读丢弃)→ exit;
//   - 原始分区不是 P(misrouted)→ 原字节挪回 p{origin}、不执行 → exit;
//   - 派发时租约被撤销 → 原字节挪回 p{P}(requeued_revoked)→ exit;
//   - 派发时停机(c.ctx / pullStop)→ 收据留在本实例 processing,由 Stop 第⑤步 release 归还收件箱 → exit;
//   - 派发成功 → 回执随任务交给 worker(workerTask.retryDone),由 handleTask 等终态路径调 finishRetry。
//
// 派发 select 与 ConsumeClaim、consumeOneRetryFrom 同一写法:撤拉取一路按 c.ctx.Done 处理,不按失败处理。
func (c *KeyOrderedKafkaConsumer) consumeOneOwnedRetry(l *claimLease) bool {
	if l == nil {
		return false
	}
	readyKey, ok := c.retryPartitionKeyOf(l.partition)
	if !ok {
		logx.Errorf("BUG: claim lease partition %d outside the retry partition keys [0,%d): topic=%s",
			l.partition, len(c.retryPartitionKeys), c.topic)
		return false
	}
	if !l.enter() {
		return false
	}
	msg, err := c.claimRetryReceiptFrom(readyKey)
	if err != nil {
		l.exit()
		if !errors.Is(err, redis.Nil) && c.ctx.Err() == nil {
			logx.Errorf("claim partitioned retry failed: ready=%s processing=%s err=%v", readyKey, c.retryProcessingKey, err)
		}
		return false
	}

	task, seq, origin, w, parsed := c.parseClaimedRetry(msg, metrics.RetryModeOwner)
	if !parsed {
		l.exit()
		return true
	}
	if origin != l.partition {
		// 放错了 p 键(人工搬运、旧版本写错):按载荷里的原始分区挪回去,由那个分区的属主执行。
		dest, _ := c.retryPartitionKeyOf(origin)
		c.moveClaimedRetry(msg, dest, metrics.RetryRouteMisrouted)
		l.exit()
		return true
	}

	task.RetryCount++
	retryTask := &workerTask{
		dbTask:             task,
		seq:                seq,
		originPartition:    origin,
		hasOriginPartition: true,
		fromRetry:          true,
		retryReceipt:       append([]byte(nil), msg...),
		claimCtx:           l.ctx,
		retryDone:          l.exit,
		enqueuedAt:         time.Now(),
	}
	c.trackTask(retryTask)
	select {
	case w.taskCh <- retryTask:
		metrics.CountRetryRoute(metrics.RetryModeOwner, metrics.RetryRouteExecuted)
		logx.Debugf("owner retry routed to worker: taskID=%s partition=%d retryCount=%d", task.TaskId, origin, task.RetryCount)
		return true
	case <-l.revoked():
		// claim 在派发前被撤销:原字节挪回 p{P}(RetryCount 未写回,不耗预算),由新属主执行。
		c.moveClaimedRetry(msg, readyKey, metrics.RetryRouteRequeuedRevoked)
		retryTask.finishRetry()
		return false
	case <-c.ctx.Done():
		retryTask.finishRetry()
		return false
	case <-c.pullStop:
		// 停机撤拉取时 worker 通道还满着:不再派发,收据留在本实例 processing,由 Stop 第⑤步原样还回收件箱。
		retryTask.finishRetry()
		return false
	}
}

// moveClaimedRetry 把认领到本实例 processing 的收据原字节搬到 dest(misrouted / 派发前撤销),成功计
// db_retry_route_total{mode=当前模式, action}。Redis 出错时登记进待重搬表(retry_stranded.go),下一拍重搬、搬成再计数;
// 没有这张表(直接拼结构体的单测)或收据已不在 processing 时只打 ERROR,收据留在本实例 processing 直到停机 release。
func (c *KeyOrderedKafkaConsumer) moveClaimedRetry(receipt []byte, dest, action string) bool {
	mode := c.retryRouteMode()
	moved, err := moveRetryReceipt(c.ctx, c.redisClient, c.retryProcessingKey, dest, receipt, receipt)
	if err == nil && moved {
		metrics.CountRetryRoute(mode, action)
		return true
	}
	if c.ctx.Err() != nil {
		// 停机:消费者已被取消,搬不动是预期的;收据在本实例 processing,Stop 第⑤步归还收件箱。
		logx.Infof("shutdown: claimed retry left in processing for shutdown release: dest=%s action=%s", dest, action)
		return false
	}
	if err != nil && c.stranded.add(strandedRetry{
		processingKey: c.retryProcessingKey, dest: dest, receipt: receipt, replacement: receipt, what: action,
		onMoved: func() { metrics.CountRetryRoute(mode, action) },
	}) {
		logx.Errorf("retry receipt move failed; queued for a re-move on the next retry tick: dest=%s action=%s err=%v",
			dest, action, err)
		return false
	}
	logx.Errorf("retry receipt move failed; receipt stays in this instance's processing list until shutdown release: dest=%s action=%s moved=%v err=%v",
		dest, action, moved, err)
	return false
}

// requeueRevokedRetry 把 claim 已撤销的重试收据原样(原字节,RetryCount 未写回)搬回 p{origin}:不执行、不耗预算。
// handleTask 在入口与 SQL 之前复核 claim 时调用;排序锁若已拿到,由 handleTask 既有的 defer 释放。
// Redis 出错时登记进待重搬表,由重试协程下一拍重搬(retry_stranded.go);没有这张表时收据留在本实例 processing,
// 直到停机 release 归还收件箱。
func (w *worker) requeueRevokedRetry(task *workerTask, reason string) {
	dest := w.retryReadyKeyFor(task.originPartition)
	mode := w.retryRouteMode()
	moved, err := moveRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, dest, task.retryReceipt, task.retryReceipt)
	if err != nil || !moved {
		if w.ctx.Err() != nil {
			// 停机:worker 已被取消,搬不动是预期的;收据在本实例 processing,Stop 第⑤步归还。
			logx.Infof("shutdown: revoked retry left in processing for shutdown release: taskID=%s partition=%d reason=%s",
				task.dbTask.GetTaskId(), task.originPartition, reason)
			return
		}
		if err != nil && w.stranded.add(strandedRetry{
			processingKey: w.retryProcessingKey, dest: dest, receipt: task.retryReceipt, replacement: task.retryReceipt,
			what:    fmt.Sprintf("%s taskID=%s", metrics.RetryRouteRequeuedRevoked, task.dbTask.GetTaskId()),
			onMoved: func() { metrics.CountRetryRoute(mode, metrics.RetryRouteRequeuedRevoked) },
		}) {
			logx.Errorf("requeue revoked retry failed; queued for a re-move on the next retry tick: taskID=%s partition=%d dest=%s reason=%s err=%v",
				task.dbTask.GetTaskId(), task.originPartition, dest, reason, err)
			return
		}
		logx.Errorf("requeue revoked retry failed; receipt stays in this instance's processing list until shutdown release: taskID=%s partition=%d dest=%s reason=%s moved=%v err=%v",
			task.dbTask.GetTaskId(), task.originPartition, dest, reason, moved, err)
		return
	}
	metrics.CountRetryRoute(mode, metrics.RetryRouteRequeuedRevoked)
	logx.Debugf("revoked retry requeued untouched: taskID=%s partition=%d dest=%s reason=%s",
		task.dbTask.GetTaskId(), task.originPartition, dest, reason)
}

// discardQueuedRetries 在 worker 退出时(子 shard 全部停下之后)非阻塞地排空 taskCh 与各子 shard 通道,对其中的任务
// 调 finishRetry:重试的收据留在本实例 processing,由 Stop 第⑤步 release 归还收件箱;不调的话租约的在途数永不归零,
// claim 排空要等满时限。Kafka 任务的 finishRetry 是空操作,原样丢弃(没标记的位点由下一任属主重放)。
func (w *worker) discardQueuedRetries() {
	drainQueuedTasks(w.taskCh)
	for _, ch := range w.subShardChans {
		drainQueuedTasks(ch)
	}
}

func drainQueuedTasks(ch chan *workerTask) {
	for {
		select {
		case task, ok := <-ch:
			if !ok {
				return
			}
			if task != nil {
				task.finishRetry()
			}
		default:
			return
		}
	}
}

// ─────────────────────────── 搬回工具(回滚用) ───────────────────────────

// requeuePartitionScript 把一个 p 键里的收据搬回共享收件箱:v2 载荷原字节 LPUSH 收件箱,其余进死信。
// 可重入:搬过的不会再出现在 p 键里。KEYS[1] = p 键,KEYS[2] = 共享收件箱,KEYS[3] = 死信;ARGV[1] = 本次上限。
// 返回 {moved, dead}。
const requeuePartitionScript = `
for i = 1, #KEYS do
	local t = redis.call('TYPE', KEYS[i]).ok
	if t ~= 'list' and t ~= 'none' then
		return redis.error_reply('WRONGTYPE retry requeue refused: ' .. KEYS[i] .. ' is a ' .. t)
	end
end
local moved, dead = 0, 0
for _ = 1, tonumber(ARGV[1]) do
	local v = redis.call('RPOP', KEYS[1])
	if not v then
		break
	end
	if string.len(v) >= 13 and string.byte(v, 1) == 2 then
		redis.call('LPUSH', KEYS[2], v)
		moved = moved + 1
	else
		redis.call('LPUSH', KEYS[3], v)
		dead = dead + 1
	end
end
return {moved, dead}`

// RequeueReport 是 RequeuePartitionedRetries 的结果。
type RequeueReport struct {
	// Moved 是 p0..p{N-1} 各自搬回共享收件箱的 v2 条数(每个分区都有一项,没有可搬的为 0)。
	Moved map[int32]int64
	// Stray 是 SCAN 到的越界 / 非规范 p 键(例如 PartitionCnt 之外的 p12)→ 搬回共享收件箱的 v2 条数。
	Stray map[string]int64
	// Dead 是非 v2 载荷进死信的总条数。
	Dead int64
}

// RequeuePartitionedRetries 把 kafka:retry:queue:{topic}:p* 里的收据全部搬回共享收件箱 kafka:retry:queue:{topic}
// (回滚到不认识 p* 的旧版本之前 / 之后用,见 go/db/cmd/retry_requeue)。先处理 p0..p{N-1},再 SCAN p* 处理越界与多余的键;
// 每个键分批调用 requeuePartitionScript,直到一批不满。可重入:中途失败或被打断后重跑即可,已搬的不会重复。
//
// 前置条件(调用方负责):该 zone 没有 RetryRouting=owner 的实例在跑 —— owner 实例会把搬回收件箱的收据再分拣回 p*。
// 为了不和它来回搬个没完,每个键的批数上限按开始时的长度计(长度 / batch + 2),搬不完的残留由调用方随后的盘点报出来。
// 任何 Redis 错误(含键类型不对)立即返回 error,已搬的部分记在返回的报告里。
func RequeuePartitionedRetries(ctx context.Context, rc redis.Cmdable, topic string, partitionCount int32, batch int) (RequeueReport, error) {
	report := RequeueReport{Moved: map[int32]int64{}, Stray: map[string]int64{}}
	if err := validateRetryQueueTarget(topic, partitionCount); err != nil {
		return report, err
	}
	if err := ValidateRequeueBatch(batch); err != nil {
		return report, err
	}
	inbox, dead := retryReadyKey(topic), retryDeadKey(topic)

	for p := int32(0); p < partitionCount; p++ {
		moved, deadN, err := requeueOneKey(ctx, rc, retryPartitionReadyKey(topic, p), inbox, dead, batch)
		report.Moved[p] += moved
		report.Dead += deadN
		if err != nil {
			return report, err
		}
	}

	keys, err := scanRetryPartitionKeys(ctx, rc, topic)
	if err != nil {
		return report, err
	}
	for _, key := range keys {
		if p, ok := parseRetryPartitionReadyKey(topic, key); ok && p < partitionCount {
			continue // 上面已经处理过
		}
		moved, deadN, err := requeueOneKey(ctx, rc, key, inbox, dead, batch)
		report.Stray[key] += moved
		report.Dead += deadN
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// requeueOneKey 分批把一个 p 键搬空(批数上限见 RequeuePartitionedRetries)。
func requeueOneKey(ctx context.Context, rc redis.Cmdable, key, inbox, dead string, batch int) (moved, deadN int64, err error) {
	length, err := rc.LLen(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("retry requeue: llen %s: %w", key, err)
	}
	rounds := length/int64(batch) + 2
	for i := int64(0); i < rounds; i++ {
		if err := ctx.Err(); err != nil {
			return moved, deadN, fmt.Errorf("retry requeue: %s: %w", key, err)
		}
		res, err := rc.Eval(ctx, requeuePartitionScript, []string{key, inbox, dead}, batch).Int64Slice()
		if err != nil {
			return moved, deadN, fmt.Errorf("retry requeue: %s: %w", key, err)
		}
		if len(res) != 2 {
			return moved, deadN, fmt.Errorf("retry requeue: %s: script returned %d values, want 2", key, len(res))
		}
		moved += res[0]
		deadN += res[1]
		if res[0]+res[1] < int64(batch) {
			return moved, deadN, nil
		}
	}
	return moved, deadN, nil
}

// scanRetryPartitionKeys 用 SCAN MATCH 列出 topic 的全部 p 键(迭代到 cursor 回到 0,去重、排序)。
// 任一页出错整体返回 error(fail-closed):不能把「没扫完」当成「没有」。
func scanRetryPartitionKeys(ctx context.Context, rc redis.Cmdable, topic string) ([]string, error) {
	pattern := retryPartitionReadyKeyPattern(topic)
	seen := map[string]struct{}{}
	var keys []string
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", pattern, err)
		}
		page, next, err := rc.Scan(ctx, cursor, pattern, retryQueueScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("scan %s (cursor %d): %w", pattern, cursor, err)
		}
		for _, k := range page {
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Strings(keys)
	return keys, nil
}

// RetryQueueDepthReport 是一个 topic 重试链路各列表的长度(只读盘点)。
type RetryQueueDepthReport struct {
	// OrphanProcessing来自完整processing前缀SCAN，不能只从registry反查而漏掉孤儿。
	OrphanProcessing  map[string]int64
	ModesWithoutLease []string
	// Shared 是共享收件箱 kafka:retry:queue:{topic} 的长度。
	Shared int64
	// Partitioned 是 p 键 → 长度:p0..p{N-1} 全部列出(不存在为 0),另加 SCAN 到的越界 / 多余 p 键。
	Partitioned map[string]int64
	// Dead 是死信 kafka:dead:queue:{topic} 的长度。
	Dead int64
	// LegacyProcessing 是旧版共享 processing kafka:retry:processing:{topic} 的长度。
	LegacyProcessing int64
	// Instances 是登记表 kafka:retry:instances:{topic} 里的实例 → 它的 processing 长度。
	Instances map[string]int64
	// InstanceModes 是登记表里每个实例 → 它登记的生效模式(metrics.RetryModeOwner / RetryModeShared,模式 HASH
	// kafka:retry:instance_mode:{topic});没登记(本版本之前的镜像)为空串。键集合与 Instances 相同。
	InstanceModes map[string]string
}

// PartitionedTotal 是全部 p 键长度之和(回滚前置条件「p* 全 0」看它)。
func (r RetryQueueDepthReport) PartitionedTotal() int64 {
	var total int64
	for _, n := range r.Partitioned {
		total += n
	}
	return total
}

// OwnerInstances 是登记表里登记为 owner 模式的实例,按名字排序。回滚前置条件「该 zone 没有 owner 实例」看它;
// 只看得见本版本起的实例(旧镜像不登记模式,见 InstanceModes)。登记表里租约已过期、还没被回收的实例也算:
// 它可能只是续租失败、仍在分拣,宁可拦下。
func (r RetryQueueDepthReport) OwnerInstances() []string {
	var owners []string
	for id, mode := range r.InstanceModes {
		if mode == metrics.RetryModeOwner {
			owners = append(owners, id)
		}
	}
	sort.Strings(owners)
	return owners
}

// LooksUnused 报告盘点的目标看起来是否根本没在用:登记表为空,且共享收件箱、死信、旧共享 processing 与全部 p* 都是 0。
// 正常回滚前 shared 实例在线、紧急回滚后旧镜像实例也在续租,登记表都不为空;全为空多半是 topic / Redis / DB / zone 指错了
// (所有键都不存在时盘点照样全 0、不报错)。只有该 zone 全部 go/db 都已正常停机时才会真的全空。
func (r RetryQueueDepthReport) LooksUnused() bool {
	return len(r.Instances) == 0 && len(r.OrphanProcessing) == 0 && len(r.ModesWithoutLease) == 0 && r.Shared == 0 && r.Dead == 0 && r.LegacyProcessing == 0 && r.PartitionedTotal() == 0
}

// RetryQueueDepths 盘点 topic 的重试链路:各列表 LLEN、ZRANGE 登记表、HMGET 模式 HASH、SCAN p*。只读;任一读失败返回 error。
func RetryQueueDepths(ctx context.Context, rc redis.Cmdable, topic string, partitionCount int32) (RetryQueueDepthReport, error) {
	report := RetryQueueDepthReport{OrphanProcessing: map[string]int64{}, Partitioned: map[string]int64{}, Instances: map[string]int64{}, InstanceModes: map[string]string{}}
	if err := validateRetryQueueTarget(topic, partitionCount); err != nil {
		return report, err
	}
	llen := func(key string) (int64, error) {
		n, err := rc.LLen(ctx, key).Result()
		if err != nil {
			return 0, fmt.Errorf("retry queue depths: llen %s: %w", key, err)
		}
		return n, nil
	}
	var err error
	if report.Shared, err = llen(retryReadyKey(topic)); err != nil {
		return report, err
	}
	if report.Dead, err = llen(retryDeadKey(topic)); err != nil {
		return report, err
	}
	if report.LegacyProcessing, err = llen(retryLegacyProcessingKey(topic)); err != nil {
		return report, err
	}
	for _, key := range retryPartitionReadyKeys(topic, partitionCount) {
		if report.Partitioned[key], err = llen(key); err != nil {
			return report, err
		}
	}
	keys, err := scanRetryPartitionKeys(ctx, rc, topic)
	if err != nil {
		return report, err
	}
	for _, key := range keys {
		if _, listed := report.Partitioned[key]; listed {
			continue
		}
		if report.Partitioned[key], err = llen(key); err != nil {
			return report, err
		}
	}
	instances, err := rc.ZRange(ctx, retryInstancesKey(topic), 0, -1).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return report, fmt.Errorf("retry queue depths: zrange %s: %w", retryInstancesKey(topic), err)
	}
	for _, id := range instances {
		if report.Instances[id], err = llen(retryInstanceProcessingKey(topic, id)); err != nil {
			return report, err
		}
	}

	registered := map[string]bool{}
	for _, id := range instances {
		registered[id] = true
	}
	cursor := uint64(0)
	seenProcessing := map[string]bool{}
	for {
		keys, next, scanErr := rc.Scan(ctx, cursor, escapeRedisGlob(retryLegacyProcessingKey(topic))+":*", 256).Result()
		if scanErr != nil {
			return report, scanErr
		}
		for _, key := range keys {
			if seenProcessing[key] {
				continue
			}
			seenProcessing[key] = true
			if len(seenProcessing) > 16384 {
				return report, errors.New("retry processing扫描超过16K预算")
			}
			id := strings.TrimPrefix(key, retryLegacyProcessingKey(topic)+":")
			if id == "" {
				return report, errors.New("retry processing实例名为空")
			}
			if !registered[id] {
				n, err := llen(key)
				if err != nil {
					return report, err
				}
				report.OrphanProcessing[key] = n
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	allModes, err := rc.HGetAll(ctx, retryInstanceModesKey(topic)).Result()
	if err != nil {
		return report, err
	}
	for id := range allModes {
		if !registered[id] {
			report.ModesWithoutLease = append(report.ModesWithoutLease, id)
		}
	}
	sort.Strings(report.ModesWithoutLease)
	if len(instances) == 0 {
		return report, nil
	}

	modes, err := rc.HMGet(ctx, retryInstanceModesKey(topic), instances...).Result()
	if err != nil {
		return report, fmt.Errorf("retry queue depths: hmget %s: %w", retryInstanceModesKey(topic), err)
	}
	if len(modes) != len(instances) {
		return report, fmt.Errorf("retry queue depths: hmget %s returned %d values for %d instances",
			retryInstanceModesKey(topic), len(modes), len(instances))
	}
	for i, id := range instances {
		mode, _ := modes[i].(string) // nil = 没登记
		report.InstanceModes[id] = mode
	}
	return report, nil
}

func validateRetryQueueTarget(topic string, partitionCount int32) error {
	if topic == "" {
		return errors.New("retry queue: topic is empty")
	}
	if partitionCount <= 0 {
		return fmt.Errorf("retry queue: partition count must be > 0, got %d", partitionCount)
	}
	return nil
}

// ValidateRequeueBatch 校验搬回工具的 -batch 是否落在 [1, RequeueMaxBatch]。工具在做任何读写之前先调它,
// RequeuePartitionedRetries 动手之前也调一次。
func ValidateRequeueBatch(batch int) error {
	if batch < 1 || batch > RequeueMaxBatch {
		return fmt.Errorf("retry requeue: batch must be in [1, %d], got %d", RequeueMaxBatch, batch)
	}
	return nil
}
