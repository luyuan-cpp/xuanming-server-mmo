package kafka

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 重试收据的归属(docs/design/no-single-node-horizontal-scaling-20261001.md §3)。
//
// 背景:重试任务从 ready 列表被「认领」到 processing 列表,做完才从 processing 里删掉;进程崩溃时留在
// processing 里的那份就是恢复 at-least-once 的唯一依据。以前 processing 是**全 zone 共用的一个列表**,
// 进程启动时把它整表搬回 ready —— 这只在「同一个 zone 只有一个 db 实例」时才对:第二个实例一启动,就把第一个
// 实例正在处理的收据全部搬走重做(结果是重复执行,不会写坏库,但每次有实例重启都会把全区在途的重试重放一遍,
// 而且「每条收据恰有一个认领者」不成立)。
//
// 现在:
//   - 每个实例认领到**自己的** processing 列表 kafka:retry:processing:{topic}:{instanceID};
//   - 实例在 ZSET kafka:retry:instances:{topic} 里登记租约(member = instanceID,score = 到期时刻的毫秒,
//     一律取 Redis 的 TIME,不用各进程自己的时钟),定期续租;
//   - 租约过期的实例,它名下的收据由任何一个活着的实例搬回 ready(孤儿回收);
//   - 旧版共享列表 kafka:retry:processing:{topic} 只回收、不再写入(滚动升级期间旧版实例还在用它)。
//
// 不变量:**一个实例的 processing 列表只在它持有未过期租约的那一刻才会增长**。认领脚本在同一个原子操作里
// 先续租再认领,所以不存在「已被摘出登记表、却还在往自己列表里认领」的实例 —— 那样的列表没人会来回收。
// 回收脚本只在列表搬空之后才把实例从登记表里摘掉,同样是原子的。
//
// 语义仍是 at-least-once:任一时刻每条未完成的收据至少在 ready、某个实例的 processing、死信三者之一。
// 误回收(实例其实活着,只是租约断了)的后果是重复执行,由写任务的 ordering lock + applied 游标挡住。
//
// 回退到旧版二进制时的注意事项:旧版看不到按实例的列表。正常停机(SIGTERM)时 release 已把收据还回 ready;
// 若新版是崩溃退出,回退前要先起一个新版实例让它回收,或手工把 kafka:retry:processing:{topic}:* 搬回 ready。

const (
	// defaultRetryLeaseSeconds 是 Kafka.RetryLeaseSeconds 未配置(0)时的租约时长;
	// 续租与孤儿回收的周期取它的三分之一,所以崩溃实例的收据最迟约 lease + lease/3 之后回到 ready。
	defaultRetryLeaseSeconds = 30
	// minRetryLeaseSeconds:再短,一次 GC 停顿或 Redis 抖动就会被判成"实例已死"。
	minRetryLeaseSeconds = 10

	// retryReclaimBatch 是回收脚本一次调用最多搬的收据数(脚本执行期间 Redis 是阻塞的,不能一次搬完一个大列表)。
	retryReclaimBatch = 100
	// retryReclaimMaxBatchesPerInstance 限定一拍里替同一个实例最多调几次脚本;搬不完下一拍继续。
	retryReclaimMaxBatchesPerInstance = 100
	// retryLegacyDrainPerTick 限定一拍里从旧版共享列表最多搬多少条。
	retryLegacyDrainPerTick = 500
	// retryExpiredScanLimit 限定一拍里最多处理多少个过期实例。
	retryExpiredScanLimit = 32

	// retryOwnershipTickTimeout 是一拍(续租 + 回收)的时间预算。
	retryOwnershipTickTimeout = 5 * time.Second
	// retryReleaseTimeout 是停机时归还收据的时间预算;超时也无妨,租约过期后别的实例会回收。
	retryReleaseTimeout = 5 * time.Second
)

func retryInstancesKey(topic string) string { return "kafka:retry:instances:" + topic }

// retryLegacyProcessingKey 是旧版(单实例时代)全 zone 共用的 processing 列表。
func retryLegacyProcessingKey(topic string) string { return "kafka:retry:processing:" + topic }

func retryInstanceProcessingKey(topic, instanceID string) string {
	return retryLegacyProcessingKey(topic) + ":" + instanceID
}

// resolveRetryInstanceID 决定本实例在登记表里的名字。显式配置优先;否则取 <主机名>:<监听端口>:
// K8s 上主机名就是 Pod 名(Pod 重建即换名,同名只可能是同一个 Pod 的容器重启);本机多开时端口不同。
// 同名意味着"旧进程必然已死",所以启动时可以直接收回同名实例遗留的收据 —— 两个活着的实例配成同名
// 会互相收走对方的在途收据(后果是重复执行)。
func resolveRetryInstanceID(configured, hostname, listenOn string) (string, error) {
	if id := strings.TrimSpace(configured); id != "" {
		return id, nil
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", errors.New("retry instance id: hostname is unavailable and Kafka.RetryInstanceId is not configured")
	}
	port := strings.TrimSpace(listenOn)
	if _, p, err := net.SplitHostPort(port); err == nil {
		port = p
	}
	if port == "" {
		return "", errors.New("retry instance id: ListenOn is empty and Kafka.RetryInstanceId is not configured")
	}
	return hostname + ":" + port, nil
}

// retryLeaseDuration 把配置的秒数折算成租约时长:0(未配置)取默认,过小的值抬到下限。
func retryLeaseDuration(configuredSeconds int) time.Duration {
	seconds := configuredSeconds
	if seconds <= 0 {
		seconds = defaultRetryLeaseSeconds
	}
	if seconds < minRetryLeaseSeconds {
		seconds = minRetryLeaseSeconds
	}
	return time.Duration(seconds) * time.Second
}

// retryLeaseScript 续租(也用于首次登记)。到期时刻取 Redis 的 TIME。
// KEYS[1] = 登记表;ARGV[1] = instanceID,ARGV[2] = 租约毫秒数。
const retryLeaseScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZADD', KEYS[1], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
return 1`

// retryClaimScript 先续租、再把一条收据从 ready 原子地认领到本实例的 processing 列表;ready 为空时返回 nil。
// 续租与认领必须在同一个脚本里(见文件头的不变量)。
// KEYS[1] = ready,KEYS[2] = 本实例的 processing,KEYS[3] = 登记表;ARGV[1] = instanceID,ARGV[2] = 租约毫秒数。
const retryClaimScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZADD', KEYS[3], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
return redis.call('RPOPLPUSH', KEYS[1], KEYS[2])`

// retryReclaimScript 把某个实例名下的收据搬回 ready(一次最多 ARGV[2] 条),列表搬空后把它从登记表里摘掉。
// ARGV[3] = '1' 时先复核租约:对方在此期间续上了就一条都不动,返回 -1。返回值否则是本次搬的条数。
// KEYS[1] = 登记表,KEYS[2] = 该实例的 processing,KEYS[3] = ready;ARGV[1] = 该实例的 instanceID。
const retryReclaimScript = `
if ARGV[3] == '1' then
	local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
	if score then
		local t = redis.call('TIME')
		local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
		if tonumber(score) > now then
			return -1
		end
	end
end
local moved = 0
for i = 1, tonumber(ARGV[2]) do
	local v = redis.call('RPOPLPUSH', KEYS[2], KEYS[3])
	if not v then
		break
	end
	moved = moved + 1
end
if redis.call('LLEN', KEYS[2]) == 0 then
	redis.call('ZREM', KEYS[1], ARGV[1])
end
return moved`

// retryOwnership 管本实例对重试收据的归属:认领、续租、回收孤儿、停机归还。
// 所有状态都在 Redis 里,结构体本身只读,可以被多个 goroutine 同时使用。
type retryOwnership struct {
	rc         redis.Cmdable
	topic      string
	instanceID string
	lease      time.Duration

	readyKey      string
	processingKey string // 本实例的 processing 列表
	legacyKey     string // 旧版共享 processing 列表:只回收,不写入
	instancesKey  string
}

func newRetryOwnership(rc redis.Cmdable, topic, readyKey, instanceID string, lease time.Duration) *retryOwnership {
	return &retryOwnership{
		rc:            rc,
		topic:         topic,
		instanceID:    instanceID,
		lease:         lease,
		readyKey:      readyKey,
		processingKey: retryInstanceProcessingKey(topic, instanceID),
		legacyKey:     retryLegacyProcessingKey(topic),
		instancesKey:  retryInstancesKey(topic),
	}
}

// renewLease 登记或续租。
func (o *retryOwnership) renewLease(ctx context.Context) error {
	return o.rc.Eval(ctx, retryLeaseScript, []string{o.instancesKey}, o.instanceID, o.lease.Milliseconds()).Err()
}

// claim 认领一条重试收据。ready 为空时返回 redis.Nil。
func (o *retryOwnership) claim(ctx context.Context) ([]byte, error) {
	value, err := o.rc.Eval(ctx, retryClaimScript,
		[]string{o.readyKey, o.processingKey, o.instancesKey}, o.instanceID, o.lease.Milliseconds()).Result()
	if err != nil {
		return nil, err
	}
	payload, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("retry claim script returned %T, want a bulk string", value)
	}
	return []byte(payload), nil
}

// drainInstance 把某个实例名下的收据搬回 ready。requireExpired=true 时由脚本复核租约,
// 对方已续租就停手并返回 leased=true。
func (o *retryOwnership) drainInstance(ctx context.Context, instanceID string, requireExpired bool) (moved int, leased bool, err error) {
	checkLease := "0"
	if requireExpired {
		checkLease = "1"
	}
	keys := []string{o.instancesKey, retryInstanceProcessingKey(o.topic, instanceID), o.readyKey}
	for batch := 0; batch < retryReclaimMaxBatchesPerInstance; batch++ {
		n, err := o.rc.Eval(ctx, retryReclaimScript, keys, instanceID, retryReclaimBatch, checkLease).Int64()
		if err != nil {
			return moved, false, err
		}
		if n < 0 {
			return moved, true, nil
		}
		moved += int(n)
		if n < retryReclaimBatch {
			return moved, false, nil
		}
	}
	return moved, false, nil
}

// drainLegacy 把旧版共享 processing 列表里的收据搬回 ready(一拍有上限)。
// 旧版实例还活着时,它的在途收据会因此被重复执行 —— 与以前每次滚动发布时的行为相同。
func (o *retryOwnership) drainLegacy(ctx context.Context) (int, error) {
	moved := 0
	for moved < retryLegacyDrainPerTick {
		_, err := o.rc.RPopLPush(ctx, o.legacyKey, o.readyKey).Result()
		if errors.Is(err, redis.Nil) {
			break
		}
		if err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// expiredInstances 列出租约已过期的实例(以 Redis 的时钟为准)。
func (o *retryOwnership) expiredInstances(ctx context.Context) ([]string, error) {
	now, err := o.rc.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	return o.rc.ZRangeByScore(ctx, o.instancesKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(now.UnixMilli(), 10),
		Count: retryExpiredScanLimit,
	}).Result()
}

// retryReclaimResult 是一次回收的结果,只用于日志与测试。
type retryReclaimResult struct {
	legacyMoved   int // 从旧版共享列表搬回的条数
	orphansMoved  int // 从过期实例名下搬回的条数
	instancesDone int // 处理完(未被续租打断)的过期实例数
}

// reclaimOnce 回收一遍:旧版共享列表 + 所有租约过期的其它实例。
// 自己的租约若已过期不在这里处理:下一次续租或认领会续上,自己回收自己只会制造重复执行。
func (o *retryOwnership) reclaimOnce(ctx context.Context) (retryReclaimResult, error) {
	var result retryReclaimResult
	legacy, err := o.drainLegacy(ctx)
	result.legacyMoved = legacy
	if err != nil {
		return result, fmt.Errorf("drain legacy processing list %s: %w", o.legacyKey, err)
	}

	expired, err := o.expiredInstances(ctx)
	if err != nil {
		return result, fmt.Errorf("list expired retry instances in %s: %w", o.instancesKey, err)
	}
	for _, instanceID := range expired {
		if instanceID == o.instanceID {
			continue
		}
		moved, leased, err := o.drainInstance(ctx, instanceID, true)
		result.orphansMoved += moved
		if err != nil {
			return result, fmt.Errorf("reclaim retry receipts of instance %s: %w", instanceID, err)
		}
		if !leased {
			result.instancesDone++
		}
	}
	return result, nil
}

// start 在开始消费之前调用:收回同名实例遗留的收据、登记租约、回收一遍孤儿。
// 任何一步失败都返回错误 —— 与以前"恢复 processing 失败即拒绝启动"同一口径:重试队列不可用时不该开始消费。
func (o *retryOwnership) start(ctx context.Context) error {
	own, _, err := o.drainInstance(ctx, o.instanceID, false)
	if err != nil {
		return fmt.Errorf("recover own retry receipts from %s: %w", o.processingKey, err)
	}
	if err := o.renewLease(ctx); err != nil {
		return fmt.Errorf("register retry instance lease in %s: %w", o.instancesKey, err)
	}
	reclaimed, err := o.reclaimOnce(ctx)
	if err != nil {
		return err
	}
	if own > 0 || reclaimed.legacyMoved > 0 || reclaimed.orphansMoved > 0 {
		logx.Errorf("retry receipts recovered at startup: topic=%s instance=%s own=%d legacy=%d orphans=%d (from %d expired instances)",
			o.topic, o.instanceID, own, reclaimed.legacyMoved, reclaimed.orphansMoved, reclaimed.instancesDone)
	}
	logx.Infof("retry receipt ownership ready: topic=%s instance=%s lease=%s processing=%s",
		o.topic, o.instanceID, o.lease, o.processingKey)
	return nil
}

// run 每收到一个 tick 续租并回收一遍;ctx 取消或 ticks 关闭时返回。
// tick 由调用方给出,测试可以逐拍驱动,不依赖墙钟(与 runPlacementCapabilityHeartbeat 同一做法)。
func (o *retryOwnership) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			o.tick(ctx)
		}
	}
}

func (o *retryOwnership) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, retryOwnershipTickTimeout)
	defer cancel()

	if err := o.renewLease(tickCtx); err != nil {
		if ctx.Err() == nil {
			logx.Errorf("renew retry instance lease failed (other instances will reclaim this instance's receipts once the lease expires): topic=%s instance=%s err=%v",
				o.topic, o.instanceID, err)
		}
		// 自己的租约都没续上,这一拍不去判别人死活。
		return
	}
	result, err := o.reclaimOnce(tickCtx)
	if result.legacyMoved > 0 || result.orphansMoved > 0 {
		logx.Errorf("reclaimed abandoned retry receipts: topic=%s by=%s legacy=%d orphans=%d (from %d expired instances)",
			o.topic, o.instanceID, result.legacyMoved, result.orphansMoved, result.instancesDone)
	}
	if err != nil && ctx.Err() == nil {
		logx.Errorf("reclaim abandoned retry receipts failed (will retry next tick): topic=%s instance=%s err=%v",
			o.topic, o.instanceID, err)
	}
}

// release 在停机时调用:把本实例还没做完的收据还回 ready,并从登记表里摘掉自己。
// 调用前必须已经停掉 worker 与 run 循环(否则它们会再认领 / 再续租)。失败不致命:租约过期后别的实例会回收。
func (o *retryOwnership) release(ctx context.Context) error {
	moved, _, err := o.drainInstance(ctx, o.instanceID, false)
	if err != nil {
		return fmt.Errorf("return retry receipts from %s: %w", o.processingKey, err)
	}
	if moved > 0 {
		logx.Infof("returned %d unfinished retry receipts to the ready queue on shutdown: topic=%s instance=%s",
			moved, o.topic, o.instanceID)
	}
	return nil
}
