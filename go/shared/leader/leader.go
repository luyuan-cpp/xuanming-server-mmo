// Package leader 提供基于 Redis SetNX 锁的单领导者选举。
//
// 抽取自 go/login/internal/logic/pkg/loginqueue/dispatcher.go 的选主模式
// (scene-manager.yaml 的部署注释里说的"正解"就是这次抽取)。语义与 login
// 侧完全一致:
//   - 竞选:SET key value NX EX ttl,成功即领导者;
//   - 心跳:每 ttl/3 用 Lua 校验属主后 PEXPIRE 续期,给瞬时 Redis 抖动
//     留 2~3 次重试机会;
//   - 自我降级:距上次续期**成功**超过 2/3 ttl 仍未确认到属主,就主动让位。
//     不能等"某次成功续期读到 0"才认输 —— Redis 单边不可达时续期只会一直
//     报错、永远读不到 0,而服务端的 key 照常过期、被别的副本抢走,那就是
//     双领导。2/3 ttl 保证本副本在 key 服务端过期前至少 ttl/3 就退位,
//     与 shared/snowflakealloc 的租约自 fencing 是同一个模式;
//   - 丢锁(key 过期被别的副本抢走):立刻取消领导者上下文,退回候选者
//     循环;其余副本最迟 ~ttl 内接管;
//   - 释放:退出临界区时 Lua 校验属主后 DEL,尽力而为 —— 释放失败也只是
//     让接任者多等一个 TTL,不影响正确性。
//
// 锁的属主凭据是每次竞选新生成的随机值,续期与释放都带属主校验,
// 不存在"续了别人的锁"或"删了别人的锁"。
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 属主校验后续期 / 删除。与 login pkg/locker 的脚本逐字一致,
// 保证 login 迁移到本包时锁语义不变。
const (
	renewScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
else
	return 0
end
`
	releaseScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`
)

// Store 是选主锁需要的最小 Redis 能力。go-zero 与 go-redis 两套客户端
// 各有一个适配器(见 store_gozero.go / store_goredis.go),单测用内存假实现。
type Store interface {
	// SetNX 对应 SET key value NX EX,拿到锁返回 true。
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// EvalInt 执行 Lua 脚本并按整数返回结果。
	EvalInt(ctx context.Context, script string, keys []string, args ...string) (int64, error)
}

// Options 是 New 的可选项。零值可用。
type Options struct {
	// TTL 是锁的存活时间,默认 30s。心跳间隔 = TTL/3,竞选重试间隔同。
	TTL time.Duration
	// ID 是本实例的身份标识(通常传 hostname / pod 名),只用于日志与
	// 锁值前缀,方便排障时 redis-cli GET 一眼看出领导者是谁。
	ID string
	// OnStateChange 在领导权变化时被调用(当选 true / 失去 false)。
	// 在 Run 的 goroutine 里同步调用,不要在里面做慢操作。
	OnStateChange func(isLeader bool)
}

// Elector 是一个可反复竞选的领导者选举器。用法:
//
//	e := leader.New(store, "my_service:leader:lock", leader.Options{ID: host})
//	go e.Run(ctx, nil)                  // 只维持领导权,用 e.IsLeader() 判定
//	go e.Run(ctx, func(lctx context.Context) { ... })  // 当选后跑临界区
type Elector struct {
	store    Store
	key      string
	ttl      time.Duration
	id       string
	onState  func(bool)
	isLeader atomic.Bool

	// stopped/leadCancel 支撑 Stop():优雅退出时主动让位,
	// 而不是把锁留到 TTL 自然过期。
	stopped    atomic.Bool
	mu         sync.Mutex
	leadCancel context.CancelFunc
}

// New 构造一个选举器。key 是全体候选副本共用的锁 key。
func New(store Store, key string, opts Options) *Elector {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	id := opts.ID
	if id == "" {
		id = "unknown"
	}
	return &Elector{
		store:   store,
		key:     key,
		ttl:     ttl,
		id:      id,
		onState: opts.OnStateChange,
	}
}

// IsLeader 报告本实例当前是否持有领导权。跨 goroutine 安全,
// 适合作为业务侧"变更动作闸门"的判定函数。
func (e *Elector) IsLeader() bool {
	return e.isLeader.Load()
}

// Run 阻塞运行候选者循环,直到 ctx 结束;通常 `go e.Run(...)`。
//
// 每次当选后调用 whileLeader(lctx):lctx 在丢锁或 ctx 结束时取消;
// whileLeader 返回后释放锁、重新竞选。传 nil 等价于"持有领导权直到丢失"。
// whileLeader 主动提前返回视为让位:释放锁并回到候选者循环。
func (e *Elector) Run(ctx context.Context, whileLeader func(ctx context.Context)) {
	// 心跳/竞选间隔 = TTL/3:TTL 内有 2~3 次续期机会兜住瞬时 Redis 抖动。
	// 下限只做除零保护 —— 生产 TTL 是秒级(默认 30s),毫秒级 TTL 仅用于单测。
	interval := e.ttl / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if whileLeader == nil {
		whileLeader = func(lctx context.Context) { <-lctx.Done() }
	}

	for {
		if ctx.Err() != nil || e.stopped.Load() {
			return
		}

		val := e.id + ":" + randomToken()
		ok, err := e.store.SetNX(ctx, e.key, val, e.ttl)
		if err != nil {
			// 竞选失败必须出声:Redis ACL/网络长期不通时,否则所有副本都
			// 静默当跟随者,变更类循环无人执行且没有任何日志线索。
			logx.Errorf("[leader] campaign setnx key=%s id=%s failed: %v", e.key, e.id, err)
		}
		if err != nil || !ok {
			// 已有领导者,或 Redis 抖动 —— 等一拍再竞选。
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				continue
			}
		}

		logx.Infof("[leader] elected key=%s id=%s ttl=%s", e.key, e.id, e.ttl)
		e.isLeader.Store(true)
		e.notify(true)

		leadCtx, leadCancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.leadCancel = leadCancel
		e.mu.Unlock()
		stopHB := e.startHeartbeat(val, interval, func(err error) {
			logx.Errorf("[leader] lost leadership key=%s id=%s: %v", e.key, e.id, err)
			leadCancel()
		})

		whileLeader(leadCtx)

		stopHB()
		e.isLeader.Store(false)
		e.notify(false)

		// 尽力释放:丢锁场景下 key 已易主,属主校验会拒绝删除,无害。
		relCtx, relCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := e.store.EvalInt(relCtx, releaseScript, []string{e.key}, val); err != nil {
			logx.Errorf("[leader] release lock key=%s failed: %v", e.key, err)
		}
		relCancel()
		e.mu.Lock()
		e.leadCancel = nil
		e.mu.Unlock()
		leadCancel()
	}
}

// Stop 停止竞选并(若在任)主动让位:取消领导者上下文,Run 的清理路径会用
// 属主校验脚本释放锁并退出候选循环。幂等,跨 goroutine 安全。
//
// 用途:优雅退出。go-zero 的 SIGTERM 处理走 os.Exit,main 的 defer 不会
// 执行 —— 不主动让位的话锁要等 TTL 自然过期,每次滚动更新都会多出一段
// 最长 ~TTL 的无领导窗口。把它挂到 proc.AddShutdownListener 即可。
func (e *Elector) Stop() {
	e.stopped.Store(true)
	e.mu.Lock()
	cancel := e.leadCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startHeartbeat 起一个续期 goroutine,返回停止函数。两条降级路径:
//   - 续期成功但返回 0:属主已换人,立刻 onLost;
//   - 续期持续报错:距上次成功超过 2/3 TTL 就自我降级(见包注释)。
//     只把错误当瞬时抖动无限重试是不行的 —— Redis 单边不可达时本副本
//     永远读不到 0,而 key 在服务端照常过期、被人抢走,即双领导。
func (e *Elector) startHeartbeat(val string, interval time.Duration, onLost func(error)) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		fenceAfter := e.ttl * 2 / 3
		lastOK := time.Now()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				res, err := e.store.EvalInt(renewCtx, renewScript, []string{e.key},
					val, fmt.Sprintf("%d", e.ttl.Milliseconds()))
				cancel()
				if err != nil {
					logx.Errorf("[leader] heartbeat renew error key=%s: %v", e.key, err)
					if since := time.Since(lastOK); since >= fenceAfter {
						// 自我降级要赶在 key 服务端过期(TTL)之前,留出
						// ≥TTL/3 的余量给在途动作干净收尾。
						onLost(fmt.Errorf("no successful renew for %s (>= %s), self-fencing: last err: %w",
							since, fenceAfter, err))
						return
					}
					continue
				}
				if res == 0 {
					onLost(fmt.Errorf("lock %s no longer owned by %s", e.key, e.id))
					return
				}
				lastOK = time.Now()
			}
		}
	}()

	return func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
		<-doneCh
	}
}

func (e *Elector) notify(isLeader bool) {
	if e.onState != nil {
		e.onState(isLeader)
	}
}

// randomToken 生成 16 字节随机十六进制串作为锁属主凭据。
// 用 crypto/rand 而不是引入 uuid 依赖 —— shared 模块保持零新增外部依赖。
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败几乎只发生在系统熵源坏掉时;退化为纳秒时间戳,
		// 碰撞概率仍然可忽略(还有 id 前缀兜底)。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
