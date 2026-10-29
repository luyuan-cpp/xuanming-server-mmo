package svc

// owner_epoch_mint.go:铸造 owner_epoch 专用的 Redis 通道(docs/design/zone-redis-regression-guard-20261022.md §4.3)。
//
// 两件事收在这里,对 logic 只露一个接口(OwnerEpochMinter):
//   - G3a 墙钟地板的「墙钟」:铸造目标值 = max(观察值 + 1, 当前毫秒),毫秒由这里给出。显式注入,单测可以钉死它;
//   - G3b 铸造后落盘确认:铸造 EVAL 与紧随的 WAITAOF 在同一条连接、同一个 pipeline 里发。WAITAOF 等的是「本连接最后一次
//     写」的复制偏移,换一条连接就覆盖不到这次铸造。AOF 与复制流都是单一有序日志,偏移 X 已落盘意味着 X 之前所有连接的写
//     都已落盘,所以确认同时覆盖了铸造之前的源端冻结存盘与交接标记(设计 §3「三点补充推导」第 2 条)。
//
// 为什么不用 ServiceContext.Redis(go-zero 的 *redis.Redis)而另建 go-redis 客户端(写法同 go/data_service
// internal/routing/publish_redis.go 与 go/db internal/kafka/durable_enqueue.go):
//   - go-zero 按地址只建一个底层客户端、不暴露 Pipelined,EVAL 与 WAITAOF 没法保证落在同一条连接上;
//   - WAITAOF 在 appendfsync everysec 下平均占住连接约 0.5s、最坏约 1s。放进 go-zero 的共享池(10 × GOMAXPROCS),
//     首登尖峰会把 EnterScene 其余的读写与后台循环一起饿住;
//   - go-zero 的熔断器把确定性的脚本错误回复也计成失败,熔断一开所有 Redis 读写都按概率被拒。
//
// 契约:
//   - 连接身份(地址、账号、口令、TLS、DB 0)取 Config.Redis,与 go-zero 对同一份 redis.RedisConf 的换算逐项相同;
//     Type 只许空或 node:铸造脚本是多键脚本,WAITAOF 按单实例连接的写偏移定义;
//   - 池上限固定 ownerEpochMintPoolSize,不预建空闲连接。这个数就是 tools/scripts/k8s_deploy.ps1 Redis 连接总账
//     scene-manager 行的 Fixed,两边要一起改(契约测试钉住);
//   - ReadTimeout = WAITAOF 超时 + 1s,WAITAOF 的服务端超时先于客户端读超时到期;ContextTimeoutEnabled:取连接排队与
//     读写都受调用方 ctx 截止约束,WAITAOF 的预算在发送前按 ctx 剩余算好;
//   - MaxRetries = -1 关掉 go-redis 的整条 pipeline 自动重发。go-redis 在 WAITAOF 读失败时也会把整条 pipeline(含已
//     生效的 EVAL)重发一遍,不凭标记的铸造重发只会撞 CAS 回 0,把「我已生效」误报成并发冲突。重发由 EvalAndConfirm 自己做,
//     **只在 EVAL 本身结果未知(传输错误)时**重发整条 pipeline;EVAL 已有回复之后不再重发,WAITAOF 的失败如实上报;
//   - 不挂熔断器:失败面已由重发次数、退避与 ctx 截止约束。
//
// 生命周期:NewServiceContext 构造、启动探针、Stop 时关闭。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"scene_manager/internal/config"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	// ownerEpochMintPoolSize 是铸造专用客户端的池上限。单副本铸造吞吐上限 ≈ 池大小 / 平均占用(everysec 下约 0.5s)
	// ≈ 2 × 池大小 次/s;超出时排队等连接,等待受请求 ctx 截止约束,超时按 Redis 错误失败(一个字节都没写)。
	// 32 只是起点,以 §8.3 R-b 压测为准再定;改它要同改 k8s_deploy.ps1 Redis 连接总账 scene-manager 行的 Fixed。
	ownerEpochMintPoolSize = 32
	// ownerEpochMintWriteTimeout 是写命令的套接字超时(铸造脚本与 WAITAOF 都很小)。
	ownerEpochMintWriteTimeout = 3 * time.Second
	// ownerEpochMintReadTimeoutSlack:读超时在 WAITAOF 超时之外多给的时间(同 go/db durableClientReadTimeoutSlack)。
	ownerEpochMintReadTimeoutSlack = time.Second
	// ownerEpochMintAttempts 是 EVAL 结果未知(传输错误)时整条 pipeline 的最多发送次数,含首发。与 go-zero 客户端
	// 以前的自动重发同量级;重发能不能认出「首发已生效」由铸造脚本的重放识别决定(只对凭标记的铸造)。
	ownerEpochMintAttempts = 3
	// ownerEpochMintDeadlineMargin:WAITAOF 的等待上限 = min(配置值, ctx 剩余 − 本值),给回包留余量。
	ownerEpochMintDeadlineMargin = 100 * time.Millisecond
	// ownerEpochMintProbeTimeoutSlack:启动探针的 ctx 超时 = WAITAOF 超时 + 本值。
	ownerEpochMintProbeTimeoutSlack = 2 * time.Second
	// ownerEpochMintRetryBackoffMin / Max:重发前的抖动等待,同时失败的请求错开重来。
	ownerEpochMintRetryBackoffMin = 10 * time.Millisecond
	ownerEpochMintRetryBackoffMax = 50 * time.Millisecond
)

// OwnerEpochMinter 是 logic 铸造 owner_epoch 用的执行面。生产实现是 *RedisOwnerEpochMinter;单测通过同一接口注入替身。
type OwnerEpochMinter interface {
	// NowMs 是 G3a 墙钟地板用的当前毫秒(Unix 毫秒)。
	NowMs() uint64
	// EvalAndConfirm 执行铸造脚本,并按配置确认这次写已落盘。返回 error 表示 EVAL 被 Redis 拒绝(确定性错误),
	// 或重发耗尽仍不知道 EVAL 执行了没有;error 为 nil 时 MintEvalResult.Reply 是脚本回复,Durability 是落盘结论。
	EvalAndConfirm(ctx context.Context, script string, keys []string, args ...any) (MintEvalResult, error)
}

// MintDurability 是铸造 EVAL 之后 WAITAOF 的结论。底层类型数的是结论种类数(现 4 种),纯内存使用。
type MintDurability uint8

const (
	// MintDurabilityDisabled:两项份数都配 0,没发 WAITAOF(启动时已打 ERROR)。
	MintDurabilityDisabled MintDurability = iota
	// MintDurabilityConfirmed:WAITAOF 回报的份数满足配置。
	MintDurabilityConfirmed
	// MintDurabilityNotDurable:WAITAOF 正常返回但份数不足(含服务端等满超时)。
	MintDurabilityNotDurable
	// MintDurabilityError:WAITAOF 被拒(appendonly no / Redis < 7.2)、回复形状不对或传输失败,落没落盘不知道。
	MintDurabilityError
)

// MintEvalResult 是一次 EvalAndConfirm 的结果(error 为 nil 时才有意义)。
type MintEvalResult struct {
	// Reply 是铸造脚本的回复原值(go-redis 的 int64)。
	Reply any
	// Durability 与 DurableErr:落盘结论与不满足时的原因(Disabled / Confirmed 时 DurableErr 为 nil)。
	Durability MintDurability
	DurableErr error
	// Attempts 是 pipeline 实际发送的次数(≥ 1)。> 1 说明首发结果未知、重发过,脚本回 0 时首发可能已生效。
	Attempts int
	// Elapsed 是最后一次 pipeline 的往返耗时(含 WAITAOF 的等待),只给指标用。
	Elapsed time.Duration
}

// ErrOwnerEpochMintNotSent:请求剩余时间已不够发一次 WAITAOF(不足 1ms),整条 pipeline 一条都没发,Redis 一个字节都没改。
var ErrOwnerEpochMintNotSent = errors.New("owner epoch mint not sent: deadline budget too short for WAITAOF")

// errUnexpectedWaitAOFReply:WAITAOF 的回复不是两个非负整数。确定性错误,不重试。取值与 go/db、data_service 的同名哨兵一致。
var errUnexpectedWaitAOFReply = errors.New("unexpected WAITAOF reply")

// mintAOFAck 是 WAITAOF 的确认要求,由已 Validate 的 config.OwnerEpochMintConfig 换算。
type mintAOFAck struct {
	numLocal    int
	numReplicas int
	timeout     time.Duration
}

// enabled 为 false(两项都是 0)时不发 WAITAOF。
func (a mintAOFAck) enabled() bool {
	return a.numLocal > 0 || a.numReplicas > 0
}

// RedisOwnerEpochMinter 是 OwnerEpochMinter 的生产实现:专用 go-redis 客户端 + 注入的墙钟。
type RedisOwnerEpochMinter struct {
	client *goredis.Client
	ack    mintAOFAck
	nowMs  func() uint64
}

var _ OwnerEpochMinter = (*RedisOwnerEpochMinter)(nil)

// NewRedisOwnerEpochMinter 建铸造专用客户端(go-redis 懒连接,构造本身不拨号)。mint 必须已过 Validate;
// nowMs 是墙钟(生产传 WallClockMs,单测传固定值),不得为 nil。
func NewRedisOwnerEpochMinter(conn redis.RedisConf, mint config.OwnerEpochMintConfig, nowMs func() uint64) (*RedisOwnerEpochMinter, error) {
	if nowMs == nil {
		return nil, errors.New("owner epoch mint: wall clock must not be nil")
	}
	if err := mint.Validate(); err != nil {
		return nil, err
	}
	ack := mintAOFAck{
		numLocal:    mint.NumLocal(),
		numReplicas: mint.NumReplicas(),
		timeout:     mint.EffectiveWaitAOFTimeout(),
	}
	opts, err := ownerEpochMintRedisOptions(conn, ack)
	if err != nil {
		return nil, err
	}
	return &RedisOwnerEpochMinter{client: goredis.NewClient(opts), ack: ack, nowMs: nowMs}, nil
}

// ownerEpochMintRedisOptions 把 Config.Redis 换算成铸造专用客户端的参数(纯函数,单测直接测)。
// 连接身份与 go-zero getClient 对同一份 redis.RedisConf 的换算逐项相同;池、超时与重试按本文件头的契约。
func ownerEpochMintRedisOptions(conn redis.RedisConf, ack mintAOFAck) (*goredis.Options, error) {
	if conn.Type != "" && conn.Type != redis.NodeType {
		return nil, fmt.Errorf("owner epoch mint needs a single-instance zone redis (Redis.Type=%q, want %q): "+
			"the mint script is multi-key and WAITAOF confirms one connection's writes on one instance", conn.Type, redis.NodeType)
	}
	if strings.TrimSpace(conn.Host) == "" {
		return nil, errors.New("owner epoch mint: Redis.Host is empty")
	}
	var tlsConfig *tls.Config
	if conn.Tls {
		// 与 go-zero redisclientmanager.go 的 getClient 同口径(Tls=true 时 InsecureSkipVerify),同一份配置两边行为一致。
		tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 与 go-zero 对同一配置项的处理一致
	}
	return &goredis.Options{
		Addr:                  conn.Host,
		Username:              conn.User,
		Password:              conn.Pass,
		DB:                    0,
		PoolSize:              ownerEpochMintPoolSize,
		MinIdleConns:          0,
		MaxRetries:            -1,
		WriteTimeout:          ownerEpochMintWriteTimeout,
		ReadTimeout:           ack.timeout + ownerEpochMintReadTimeoutSlack,
		ContextTimeoutEnabled: true,
		DisableIdentity:       true, // 同 go/db durableClientOptions:不发 CLIENT SETINFO
		TLSConfig:             tlsConfig,
	}, nil
}

// WallClockMs 是生产用的墙钟:当前 Unix 毫秒。系统时钟早于 1970 时返回 0(地板不起作用,铸造退化为观察值 + 1)。
func WallClockMs() uint64 {
	ms := time.Now().UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

// NowMs 实现 OwnerEpochMinter。
func (m *RedisOwnerEpochMinter) NowMs() uint64 {
	return m.nowMs()
}

// WaitsForAOF 报告铸造之后会不会发 WAITAOF。
func (m *RedisOwnerEpochMinter) WaitsForAOF() bool {
	return m.ack.enabled()
}

// EvalAndConfirm 实现 OwnerEpochMinter:同一个 pipeline 里先 EVAL、再 WAITAOF(开启时)。
//
// 判定:
//   - 发送前按 ctx 剩余算 WAITAOF 的等待上限:min(配置值, 剩余 − 100ms),**先截断成整毫秒**,不足 1ms 一条都不发
//     (WAITAOF 的 timeout 0 是无限阻塞)—— 首发前就不够时回 ErrOwnerEpochMintNotSent;
//   - EVAL 是 Redis 错误回复(脚本 error_reply、WRONGTYPE、OOM 等):确定性的,不重发,返回 error;
//   - EVAL 传输错误(含取连接失败、ctx 截止):结果未知,退避后在新连接上重发整条 pipeline,最多 ownerEpochMintAttempts 次。
//     首发若已生效,凭标记的铸造会被脚本的重放识别认出来;脚本在识别分支里把 owner_epoch 原值写回一次,所以新连接上的
//     WAITAOF 仍覆盖首发那次写。不凭标记的铸造认不出,重发回 0(已登记的残余,调用方据 Attempts 打日志);
//   - EVAL 有回复之后不再重发:WAITAOF 的结论原样写进 Durability,由调用方按「推送失败」处理不满足的情形。
func (m *RedisOwnerEpochMinter) EvalAndConfirm(ctx context.Context, script string, keys []string, args ...any) (MintEvalResult, error) {
	var lastErr error
	for attempt := 1; attempt <= ownerEpochMintAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, mintRetryBackoff()); err != nil {
				return MintEvalResult{Attempts: attempt - 1},
					fmt.Errorf("owner epoch mint EVAL result unknown after %d attempt(s) (retry wait: %v): %w", attempt-1, err, lastErr)
			}
		}
		var waitMs int64
		if m.ack.enabled() {
			deadline, hasDeadline := ctx.Deadline()
			var ok bool
			waitMs, ok = waitAOFBudgetMs(time.Now(), deadline, hasDeadline, m.ack.timeout, ownerEpochMintDeadlineMargin)
			if !ok {
				if lastErr == nil {
					return MintEvalResult{}, ErrOwnerEpochMintNotSent
				}
				return MintEvalResult{Attempts: attempt - 1},
					fmt.Errorf("owner epoch mint EVAL result unknown after %d attempt(s), no budget left to resend: %w", attempt-1, lastErr)
			}
		}

		started := time.Now()
		var evalCmd, waitCmd *goredis.Cmd
		// 不看 Pipelined 的返回值:它只是第一条失败命令的错误,逐条命令的结果才是判据。
		_, _ = m.client.Pipelined(ctx, func(p goredis.Pipeliner) error {
			evalCmd = p.Eval(ctx, script, keys, args...)
			if m.ack.enabled() {
				waitCmd = p.Do(ctx, "WAITAOF", m.ack.numLocal, m.ack.numReplicas, waitMs)
			}
			return nil
		})
		elapsed := time.Since(started)
		if evalCmd == nil {
			lastErr = errors.New("pipeline callback was not invoked")
			continue
		}
		if evalErr := evalCmd.Err(); evalErr != nil {
			if isRedisErrorReply(evalErr) {
				return MintEvalResult{Attempts: attempt},
					fmt.Errorf("owner epoch mint script rejected by redis (deterministic, not resent): %w", evalErr)
			}
			lastErr = evalErr
			continue
		}
		durability, durableErr := m.judgeWaitAOF(waitCmd)
		return MintEvalResult{
			Reply:      evalCmd.Val(),
			Durability: durability,
			DurableErr: durableErr,
			Attempts:   attempt,
			Elapsed:    elapsed,
		}, nil
	}
	return MintEvalResult{Attempts: ownerEpochMintAttempts},
		fmt.Errorf("owner epoch mint EVAL result unknown after %d attempt(s): %w", ownerEpochMintAttempts, lastErr)
}

// judgeWaitAOF 把 WAITAOF 那条命令的结果翻成结论。waitCmd 为 nil = 没发(两项都配 0)。
func (m *RedisOwnerEpochMinter) judgeWaitAOF(waitCmd *goredis.Cmd) (MintDurability, error) {
	if waitCmd == nil {
		return MintDurabilityDisabled, nil
	}
	return judgeWaitAOFReply(waitCmd, m.ack.numLocal, m.ack.numReplicas)
}

// judgeWaitAOFReply 是 judgeWaitAOF 的纯函数部分(单测用 goredis.NewCmd + SetVal / SetErr 构造回复)。
func judgeWaitAOFReply(waitCmd *goredis.Cmd, numLocal, numReplicas int) (MintDurability, error) {
	acks, err := waitCmd.Slice()
	if err != nil {
		if isRedisErrorReply(err) {
			return MintDurabilityError, fmt.Errorf("WAITAOF rejected (needs appendonly yes and Redis >= 7.2): %w", err)
		}
		return MintDurabilityError, fmt.Errorf("WAITAOF: %w", err)
	}
	if err := checkWaitAOFReply(acks, numLocal, numReplicas); err != nil {
		if errors.Is(err, errUnexpectedWaitAOFReply) {
			return MintDurabilityError, err
		}
		return MintDurabilityNotDurable, err
	}
	return MintDurabilityConfirmed, nil
}

// Probe 是启动期的 WAITAOF 能力探针:按配置发一次 WAITAOF(timeout = 配置值),报错或份数不足返回 error,调用方据此拒启。
// 未开启时什么都不发,返回 nil。
//
// 为什么在启动期挡:WAITAOF 的配置错误(appendonly no 回 ERR、Redis < 7.2 回 unknown command、NumReplicas 写了 1 而拓扑里
// 没有副本)是确定性的,留到运行期就是每一次铸造都被当成未落盘回滚,所有换节点 / 首登都失败。探针用的连接上没写过东西,
// 本地确认会立即返回;timeout 仍取配置值,免得赶上 fsync 间隙误判。
func (m *RedisOwnerEpochMinter) Probe(ctx context.Context) error {
	if !m.ack.enabled() {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.ack.timeout+ownerEpochMintProbeTimeoutSlack)
	defer cancel()
	timeoutMs := m.ack.timeout.Milliseconds()
	acks, err := m.client.Do(probeCtx, "WAITAOF", m.ack.numLocal, m.ack.numReplicas, timeoutMs).Slice()
	if err != nil {
		if !isRedisErrorReply(err) {
			// 没执行(连不上、ctx 已结束)单独报,否则会报成「needs appendonly yes」,把运维引错方向。
			return fmt.Errorf("owner epoch mint WAITAOF probe not executed (connection / context): %w", err)
		}
		return fmt.Errorf("owner epoch mint WAITAOF %d %d %d failed (needs appendonly yes and Redis >= 7.2; "+
			"set OwnerEpochMint.WaitAOFNumLocal/WaitAOFNumReplicas to 0 only where no AOF exists): %w",
			m.ack.numLocal, m.ack.numReplicas, timeoutMs, err)
	}
	if err := checkWaitAOFReply(acks, m.ack.numLocal, m.ack.numReplicas); err != nil {
		return fmt.Errorf("owner epoch mint WAITAOF probe (check OwnerEpochMint.WaitAOFNumLocal / WaitAOFNumReplicas against the topology): %w", err)
	}
	return nil
}

// Close 关闭专用客户端。
func (m *RedisOwnerEpochMinter) Close() error {
	return m.client.Close()
}

// 以下纯函数与 go/data_service/internal/routing/route_publish.go、go/db/internal/kafka/durable_enqueue.go 的同名函数语义
// 逐字一致(各模块一份:不为几行代码开公共包,同 go/db 那边的取舍),**几处要改一起改**。

// isRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误)。网络错误、ctx 错误都不是。
func isRedisErrorReply(err error) bool {
	var re goredis.Error
	return errors.As(err, &re)
}

// checkWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数。
// 份数不足回普通错误;形状不对回 errUnexpectedWaitAOFReply。
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

// waitAOFBudgetMs 算本次 WAITAOF 的 timeout(毫秒):min(configured, 截止前剩余 − margin),**先截断成整毫秒再判断**。
// 结果小于 1 时 ok=false,调用方不得发送 —— WAITAOF 的 timeout 为 0 表示无限阻塞。没有截止时间时取 configured。
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

// mintRetryBackoff 在 [Min, Max] 内均匀取一个等待时长(同 data_service publishRetryBackoff)。
func mintRetryBackoff() time.Duration {
	return ownerEpochMintRetryBackoffMin + rand.N(ownerEpochMintRetryBackoffMax-ownerEpochMintRetryBackoffMin+1)
}

// sleepCtx 可取消的等待:ctx 先结束就立即返回 ctx.Err()。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
