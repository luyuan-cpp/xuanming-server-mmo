package main

// 合服工具把合走标记写进 mapping Redis,并核对路由库与这个 Redis 成对。
//
// 本文件镜像 go/shared/placement 的 Redis 契约(merge_zone 不 import shared/placement,改 shared 那边必须同步改这里,反之亦然):
//   - 按完整值 compare-and-delete 的 RouteReadyCompareAndDeleteScript(镜像删除复用它);
//   - 水位键 mapping:route:watermark 的值格式(只用于「库与 Redis 是否成对」的核对)。
//
// 脚本与常量由 route_mirror_test.go 用 go/parser 读 shared 源码逐字节比对。
//
// 同一连接:go-redis 的 Pipelined 在一条连接上按序发出一批命令,所以 SET 与末尾的 WAITAOF 在同一条连接上 ——
// WAITAOF 只等本连接此前的写。不用 go-redis 的 WaitAOF 方法:它返回 *IntCmd,而 WAITAOF 回两元素数组。
// 份数不足 / 网络错误重发(SET 幂等);错误回复、回复形状不对一律不重试、不伪装成功。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── 镜像的常量与脚本(route_mirror_test.go 与 shared 逐字节比对)─────────────

// routeWatermarkKey 是库回档检测用的水位键(shared/placement.RouteWatermarkKey)。本工具只读不写。
const routeWatermarkKey = "mapping:route:watermark"

// routeWatermarkFormatVersion = shared/placement.RouteWatermarkFormatVersion。
const routeWatermarkFormatVersion = 1

// routeGenerationMaxLen = shared/placement 的 routeReadyGenerationMaxLen(route_meta.generation 是 varchar(32))。
const routeGenerationMaxLen = 32

// routeCompareAndDeleteScript = shared/placement.RouteReadyCompareAndDeleteScript(逐字节):KEYS[1] 当前值等于 ARGV[1] 才 DEL。
// 本工具拿它按完整值删合走标记镜像(脚本与键名无关)。
const routeCompareAndDeleteScript = `-- route ready marker compare-and-delete,docs/design/player-route-mapping-durability-proposal.md §11.2(第 1b 批)
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`

// ── 写镜像的参数 ──────────────────────────────────────────────────────────────

const (
	// routePublishAttempts 是一次写的总尝试次数:首发 + 份数不足 / 网络错误时重发最多 3 次。
	routePublishAttempts = 4
	// routePublishRetryBackoffMin / Max 是重发之前的抖动等待(可被 ctx 取消)。
	routePublishRetryBackoffMin = 10 * time.Millisecond
	routePublishRetryBackoffMax = 50 * time.Millisecond
	// routeWaitAOFMargin:WAITAOF 的等待上限 = min(配置值, ctx 剩余 − 本值),给回包留的余量。
	routeWaitAOFMargin = 100 * time.Millisecond
	// routeWaitAOFTimeoutMaxMs 是 -waitaof-timeout-ms 的上限:本工具的 go-redis 客户端用缺省 ReadTimeout(3s),
	// 一条 pipeline 的读超时覆盖整批回复;WAITAOF 等得比它久,客户端先超时,结论就成了「网络错误」。
	routeWaitAOFTimeoutMaxMs = 2000
)

// 写镜像与成对核对的错误。
var (
	// errRoutePublishNotDurable WAITAOF 在全部尝试里都没确认到要求的落盘份数(或预算已不够发一次 WAITAOF)。
	errRoutePublishNotDurable = errors.New("route publish not confirmed durable by WAITAOF")
	// errUnexpectedRouteWaitAOFReply WAITAOF 的回复不是两个非负整数:确定性错误,不重试。
	errUnexpectedRouteWaitAOFReply = errors.New("unexpected WAITAOF reply")
	// errUnexpectedRouteScriptReply compare-and-delete 脚本的回复形状不对。
	errUnexpectedRouteScriptReply = errors.New("unexpected route script reply")
	// errRouteWatermarkMalformed 水位键的值不合规(镜像 shared 的 ErrMalformed 口径)。
	errRouteWatermarkMalformed = errors.New("malformed route watermark")
	// errRouteWatermarkNewerVersion 水位键是更新格式版本写的(shared ErrRouteWatermarkNewerVersion 的镜像):不信任。
	errRouteWatermarkNewerVersion = errors.New("route watermark has a newer format version")
	// errRoutePairing 全局库与 mapping Redis 不成对,拒绝开写。
	errRoutePairing = errors.New("route DB and mapping Redis are not paired")
)

// routeDurability 是 WAITAOF 的份数与等待(-waitaof-local / -waitaof-replicas / -waitaof-timeout-ms)。
type routeDurability struct {
	NumLocal    int
	NumReplicas int
	Timeout     time.Duration
}

// newRouteDurability 校验 WAITAOF 参数:numlocal 只能是 0 或 1;numreplicas ≥ 0;两者不能都是 0(那等于不确认落盘,
// 本工具不允许);timeout 在 [1, routeWaitAOFTimeoutMaxMs] 毫秒(0 = WAITAOF 无限阻塞,绝不发)。
func newRouteDurability(numLocal, numReplicas, timeoutMs int) (routeDurability, error) {
	switch {
	case numLocal < 0 || numLocal > 1:
		return routeDurability{}, fmt.Errorf("-waitaof-local %d must be 0 or 1", numLocal)
	case numReplicas < 0:
		return routeDurability{}, fmt.Errorf("-waitaof-replicas %d must be >= 0", numReplicas)
	case numLocal == 0 && numReplicas == 0:
		return routeDurability{}, errors.New("-waitaof-local and -waitaof-replicas are both 0: the merge:merged_into mirror must be confirmed by WAITAOF")
	case timeoutMs < 1 || timeoutMs > routeWaitAOFTimeoutMaxMs:
		return routeDurability{}, fmt.Errorf("-waitaof-timeout-ms %d must be in [1, %d] (0 would block forever; above the limit the "+
			"client read timeout fires first)", timeoutMs, routeWaitAOFTimeoutMaxMs)
	}
	return routeDurability{NumLocal: numLocal, NumReplicas: numReplicas, Timeout: time.Duration(timeoutMs) * time.Millisecond}, nil
}

// ── WAITAOF ───────────────────────────────────────────────────────────────────

// checkRouteWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数(data_service checkWaitAOFReply 的镜像)。
// 份数不足回普通错误(调用方重发);形状不对回 errUnexpectedRouteWaitAOFReply(不重试)。
func checkRouteWaitAOFReply(vals []any, numLocal, numReplicas int) error {
	if len(vals) != 2 {
		return fmt.Errorf("%w: %d elements %v, want [numlocal, numreplicas]", errUnexpectedRouteWaitAOFReply, len(vals), vals)
	}
	local, localOK := vals[0].(int64)
	replicas, replicasOK := vals[1].(int64)
	if !localOK || !replicasOK || local < 0 || replicas < 0 {
		return fmt.Errorf("%w: %v", errUnexpectedRouteWaitAOFReply, vals)
	}
	if local < int64(numLocal) || replicas < int64(numReplicas) {
		return fmt.Errorf("%w: WAITAOF acked numlocal=%d numreplicas=%d, want at least %d and %d",
			errRoutePublishNotDurable, local, replicas, numLocal, numReplicas)
	}
	return nil
}

// routeWaitAOFBudgetMs 算本次 WAITAOF 的 timeout(毫秒):min(configured, 截止前剩余 − margin),先截断成整毫秒再判断;
// 小于 1 时 ok=false,调用方不得发送(WAITAOF 的 timeout 为 0 表示无限阻塞)。纯函数,时间由参数注入。
func routeWaitAOFBudgetMs(now, deadline time.Time, hasDeadline bool, configured time.Duration) (int64, bool) {
	budget := configured
	if hasDeadline {
		if remaining := deadline.Sub(now) - routeWaitAOFMargin; remaining < budget {
			budget = remaining
		}
	}
	ms := budget.Milliseconds()
	if ms < 1 {
		return 0, false
	}
	return ms, true
}

// isRouteRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误)。网络错误、ctx 错误都不是。
func isRouteRedisErrorReply(err error) bool {
	var re redis.Error
	return errors.As(err, &re) && !errors.Is(err, redis.Nil)
}

// ── 发布器 ────────────────────────────────────────────────────────────────────

// routePublisher 在 mapping Redis(DB 0)上写 / 删合走标记镜像、读水位键。时钟、睡眠、抖动是单测接缝。
type routePublisher struct {
	rdb *redis.Client
	dur routeDurability
	// now / sleep / backoff:生产 time.Now / sleepCtx / 均匀抖动;单测注入,不依赖墙钟、不真睡。
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	backoff func() time.Duration
}

// newRoutePublisher 装配生产用的发布器。
func newRoutePublisher(rdb *redis.Client, dur routeDurability) *routePublisher {
	return &routePublisher{rdb: rdb, dur: dur, now: time.Now, sleep: sleepCtx, backoff: routePublishRetryBackoff}
}

// routePublishRetryBackoff 在 [Min, Max] 内均匀取一个等待时长。
func routePublishRetryBackoff() time.Duration {
	return routePublishRetryBackoffMin + rand.N(routePublishRetryBackoffMax-routePublishRetryBackoffMin+1)
}

// durableWrite 是「写 + 同一连接末尾的 WAITAOF」的循环:
//   - write 往 pipeline 里排命令,返回 check;Exec 之后 check 判写的结果:(false, nil) 通过;(true, err) 可重试
//     (网络错误);(false, err) 确定性失败,立刻返回;
//   - 写通过后判 WAITAOF:Redis 错误回复(appendonly 关着、版本不支持)与形状不对是确定性的;网络错误与份数不足重发;
//   - 总尝试次数 routePublishAttempts,每次之前按 ctx 剩余时间重算 WAITAOF 的 timeout,预算不够就不发。
//
// 每次 Pipelined 只取一条连接,SET 与 WAITAOF 在同一条连接上(go-redis 自身的 MaxRetries 也是整条 pipeline 换连接重发)。
func (p *routePublisher) durableWrite(ctx context.Context, what string, write func(pipe redis.Pipeliner) func() (bool, error)) error {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	var lastErr error
	for attempt := 1; attempt <= routePublishAttempts; attempt++ {
		if attempt > 1 {
			if werr := p.sleep(ctx, p.backoff()); werr != nil {
				return fmt.Errorf("%s: %w", what, errors.Join(lastErr, werr))
			}
		}
		waitMs, ok := routeWaitAOFBudgetMs(p.now(), deadline, hasDeadline, p.dur.Timeout)
		if !ok {
			// errors.Join 跳过 nil:第一次尝试之前就没预算时 lastErr 与 ctx.Err() 可能都是 nil。
			return errors.Join(fmt.Errorf("%s: %w: deadline too close to send WAITAOF (attempt %d/%d)",
				what, errRoutePublishNotDurable, attempt, routePublishAttempts), lastErr, ctx.Err())
		}
		var check func() (bool, error)
		var wait *redis.Cmd
		// pipeline 的返回值只是第一条失败命令的错误;逐条命令的结果才是判据。
		_, pipelineErr := p.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			check = write(pipe)
			wait = pipe.Do(ctx, "WAITAOF", p.dur.NumLocal, p.dur.NumReplicas, waitMs)
			return nil
		})
		if pipelineErr != nil {
			holdMaintenanceTool(ctx)
		}
		if check == nil || wait == nil {
			holdMaintenanceTool(ctx)
			return errors.Join(fmt.Errorf("%s: pipeline did not construct its write/WAITAOF result", what), pipelineErr)
		}
		retry, err := check()
		if err != nil {
			holdMaintenanceTool(ctx)
			if !retry {
				return fmt.Errorf("%s: %w", what, err)
			}
			lastErr = err
			continue
		}
		acks, werr := wait.Slice()
		if werr != nil {
			holdMaintenanceTool(ctx)
			if isRouteRedisErrorReply(werr) {
				return fmt.Errorf("%s: WAITAOF %d %d %d rejected (needs appendonly yes and Redis >= 7.2; not retried): %w",
					what, p.dur.NumLocal, p.dur.NumReplicas, waitMs, werr)
			}
			lastErr = fmt.Errorf("WAITAOF: %w", werr)
			continue
		}
		if aerr := checkRouteWaitAOFReply(acks, p.dur.NumLocal, p.dur.NumReplicas); aerr != nil {
			holdMaintenanceTool(ctx)
			if errors.Is(aerr, errUnexpectedRouteWaitAOFReply) {
				return fmt.Errorf("%s: %w", what, aerr)
			}
			lastErr = aerr
			continue
		}
		if pipelineErr != nil {
			return fmt.Errorf("%s: %w", what, pipelineErr)
		}
		return nil
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", what, routePublishAttempts, lastErr)
}

// ── 合走标记镜像 merge:merged_into:{src} ──────────────────────────────────────

// setMergedIntoMirror 在库里写好合走标记之后写镜像(无 TTL)+ WAITAOF(同一 pipeline)。value 必须是 encodeRouteMergedIntoValue 的结果。
func (p *routePublisher) setMergedIntoMirror(ctx context.Context, src uint32, value string) error {
	var v mergedIntoValue
	if err := json.Unmarshal([]byte(value), &v); err != nil || v.Dst == 0 || v.Dst == src || !validRunID(v.RunID) {
		return fmt.Errorf("%w: merged_into value %q for zone %d", errRouteInvalidArgument, value, src)
	}
	key := mergedIntoKey(src)
	return p.durableWrite(ctx, "merged_into mirror "+key, func(pipe redis.Pipeliner) func() (bool, error) {
		cmd := pipe.Set(ctx, key, value, 0)
		return func() (bool, error) {
			if err := cmd.Err(); err != nil {
				return !isRouteRedisErrorReply(err), err
			}
			return false, nil
		}
	})
}

// deleteMergedIntoMirror 撤销删掉库里的标记之后,按完整值 compare-and-delete 镜像。
func (p *routePublisher) deleteMergedIntoMirror(ctx context.Context, src uint32, value string) (bool, error) {
	return p.compareAndDelete(ctx, mergedIntoKey(src), value)
}

func (p *routePublisher) compareAndDelete(ctx context.Context, key, value string) (bool, error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return false, err
	}
	v, err := p.rdb.Eval(ctx, routeCompareAndDeleteScript, []string{key}, value).Result()
	if err != nil {
		holdMaintenanceTool(ctx)
		return false, fmt.Errorf("compare-and-delete %s: %w", key, err)
	}
	n, ok := v.(int64)
	if !ok || (n != 0 && n != 1) {
		holdMaintenanceTool(ctx)
		return false, fmt.Errorf("%w: compare-and-delete %s replied %T %v", errUnexpectedRouteScriptReply, key, v, v)
	}
	return n == 1, nil
}

// ── 水位键与「库与 Redis 成对」核对 ───────────────────────────────────────────

// routeWatermark 是 mapping:route:watermark 的值(shared RouteWatermark 的镜像)。
type routeWatermark struct {
	V           int    `json:"v"`
	Generation  string `json:"generation"`
	WatermarkMs uint64 `json:"watermark_ms"`
}

// decodeRouteWatermark 解析水位键(shared DecodeRouteWatermark 的镜像,同样分两步解:先只解 v,
// v 大于已知版本回 errRouteWatermarkNewerVersion,再整体解与校验)。
func decodeRouteWatermark(raw string) (routeWatermark, error) {
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(raw), &head); err != nil {
		return routeWatermark{}, fmt.Errorf("%w: not a JSON object with an integer v: %v", errRouteWatermarkMalformed, err)
	}
	if head.V > routeWatermarkFormatVersion {
		return routeWatermark{}, fmt.Errorf("%w: v=%d, this binary knows v<=%d", errRouteWatermarkNewerVersion, head.V, routeWatermarkFormatVersion)
	}
	var w routeWatermark
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return routeWatermark{}, fmt.Errorf("%w: not a JSON object of the expected shape: %v", errRouteWatermarkMalformed, err)
	}
	switch {
	case w.V != routeWatermarkFormatVersion:
		return routeWatermark{}, fmt.Errorf("%w: v=%d, want %d", errRouteWatermarkMalformed, w.V, routeWatermarkFormatVersion)
	case !routeLowerHexOfLen(w.Generation, 1, routeGenerationMaxLen):
		return routeWatermark{}, fmt.Errorf("%w: generation %q must be 1..%d chars of [0-9a-f]", errRouteWatermarkMalformed, w.Generation, routeGenerationMaxLen)
	case w.WatermarkMs == 0:
		return routeWatermark{}, fmt.Errorf("%w: watermark_ms must be > 0", errRouteWatermarkMalformed)
	}
	return w, nil
}

// routeLowerHexOfLen 报告 s 是否由 [minLen, maxLen] 个 [0-9a-f] 组成(shared lowerHexOfLen)。
func routeLowerHexOfLen(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readWatermark 读水位键:不存在回 present=false;不是 string(WRONGTYPE)等错误原样返回(fail-closed)。
func (p *routePublisher) readWatermark(ctx context.Context) (string, bool, error) {
	raw, err := p.rdb.Get(ctx, routeWatermarkKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get %s: %w", routeWatermarkKey, err)
	}
	return raw, true, nil
}

// decideRoutePairing 是「库与 Redis 是否成对」的纯判定:水位键必须存在、可解码、世代等于该库 route_meta.generation ——
// 证明有 data_service 的对账 leader 正在用这份库维护这个 Redis。不存在 = -mapping-redis-addr 指错了实例,或 data_service 还没起来;
// 世代不等 = 这个 Redis 配的是另一份全局库。
func decideRoutePairing(wmRaw string, wmPresent bool, dbGeneration string) error {
	if !wmPresent {
		return fmt.Errorf("%w: %s is missing: no reconcile leader maintains this mapping Redis with the global DB "+
			"(data_service is not up yet, or -mapping-redis-addr points at another instance)", errRoutePairing, routeWatermarkKey)
	}
	wm, err := decodeRouteWatermark(wmRaw)
	if err != nil {
		return fmt.Errorf("%w: %s = %q: %w", errRoutePairing, routeWatermarkKey, wmRaw, err)
	}
	if wm.Generation != dbGeneration {
		return fmt.Errorf("%w: %s generation %q != route_meta.generation %q of -route-db-dsn: this Redis is paired with another global DB",
			errRoutePairing, routeWatermarkKey, wm.Generation, dbGeneration)
	}
	return nil
}
