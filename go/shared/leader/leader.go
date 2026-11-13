// Package leader 提供基于 Redis SetNX 锁的单领导者选举。
//
// 抽取自 go/login/internal/logic/pkg/loginqueue/dispatcher.go 的选主模式
// (scene-manager.yaml 的部署注释里说的"正解"就是这次抽取)。语义与 login
// 侧完全一致:
//   - 竞选:SET key value NX EX ttl,成功即领导者;
//   - 心跳:每 ttl/3 用 Lua 校验属主后 PEXPIRE 续期,失联后容忍 1 个出错拍;
//   - 自我降级:续期报错且距上次**成功续期的发出时刻**达到门槛就主动让位。门槛由
//     FenceAfter(ttl, 心跳间隔, 单次续期最坏耗时) 算出:失联后第 1 个出错拍卡满
//     最坏耗时也不降级,第 2 个出错拍必定降级且在 key 服务端过期前完成。最坏耗时
//     不能只看续期 ctx 超时 —— go-redis 默认不把 ctx 截止时间用到 socket 读写上,
//     socket 读写另受客户端读写超时之和约束(由 Store 经 CallBounder 报告),两段相加
//     见 CallBudget;TTL 太短满足不了时退化为任一续期出错即降级,并记错误日志。不能等"某次成功续期读到 0"
//     才认输 —— Redis 单边不可达时续期只会一直报错、永远读不到 0,而服务端的 key
//     照常过期、被别的副本抢走,那就是双领导。与 shared/snowflakealloc 的租约自
//     fencing、login pkg/locker 的锁心跳同一模式;
//   - 丢锁(key 过期被别的副本抢走):立刻取消领导者上下文,退回候选者
//     循环;其余副本最迟 ~ttl 内接管;
//   - 释放:退出临界区时 Lua 校验属主后 DEL,尽力而为 —— 释放失败也只是
//     让接任者多等一个 TTL,不影响正确性。
//
// 锁的属主凭据是每次竞选新生成的随机值,续期与释放都带属主校验,
// 不存在"续了别人的锁"或"删了别人的锁"。
//
// 这把锁只保证「最终只有一个领导者」,不保证任一时刻只有一个:自我降级靠本地时钟,进程卡顿、
// 或 Redis 主从切换丢了锁时,旧领导者在下一次心跳之前仍以为自己在任。所以领导者独占的写不能只凭
// IsLeader:要么本身幂等、要么带按对象的条件,要么带上 Token 在写脚本里核对锁键(见 Elector.Token)。
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
	// ownedScript 核对锁里是不是这次竞选写进去的值(只读)。见 Run 里 SETNX 回「已存在」之后的核对。
	ownedScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return 1
else
	return 0
end
`
)

// Store 是选主锁需要的最小 Redis 能力。go-redis 的单机客户端与 UniversalClient 各有一个适配器
// (见 store_goredis.go / store_universal.go),单测用内存假实现。go-zero 句柄的适配器 2026-11-12 随最后一个调用方
// (scene_manager 换控制集群句柄)删除。
type Store interface {
	// SetNX 对应 SET key value NX EX,拿到锁返回 true。
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// EvalInt 执行 Lua 脚本并按整数返回结果。
	EvalInt(ctx context.Context, script string, keys []string, args ...string) (int64, error)
}

// CallBounder 是 Store 的可选能力:报告单次调用 socket 写 + 读这一段的上界,不是整次调用的最坏耗时 ——
// 写之前的连接池排队 / 重试退避 / 拨号受调用方 ctx 截止时间约束,心跳用 CallBudget 把续期 ctx 超时
// 加上去再算降级门槛;新建连接时的 HELLO / AUTH 握手读写仍不在其内。
// 返回 0 表示客户端认 ctx 截止时间,单次调用由调用方的 ctx 超时封顶;返回 UnboundedCall
// 表示可能无限期阻塞。不实现时按"认 ctx 截止时间"处理。
type CallBounder interface {
	MaxCallDuration() time.Duration
}

// DefaultRetryPeriod 是跟随者两次竞选之间的缺省间隔,取 client-go leaderelection 的缺省值(RetryPeriod 2s)。
//
// 它与心跳间隔(TTL/3)分开:心跳间隔要给瞬时抖动留出 TTL 内 2~3 次续期机会,竞选间隔只决定「锁空出来之后
// 多久有人接任」。两者绑在一起时缺省 TTL 30s 的竞选间隔是 10s:每次滚动重启,现任主动让位之后要白等最多 10s
// 才有新领导者,这段时间选主任务(scene_manager 的排空迁移与死节点接管、login 的排队放行等)全部停摆
// (docs/design/scene-node-planned-drain.md §9「选主交接」)。竞选间隔只影响 SETNX 的频率,不影响锁的安全性。
const DefaultRetryPeriod = 2 * time.Second

// Options 是 New 的可选项。零值可用。
type Options struct {
	// TTL 是锁的存活时间,默认 30s。心跳间隔 = TTL/3。
	TTL time.Duration
	// RetryPeriod 是没当选时两次竞选之间的间隔。<= 0 取 DefaultRetryPeriod;不超过心跳间隔(TTL/3),
	// 超过时按心跳间隔算。
	RetryPeriod time.Duration
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
	store       Store
	key         string
	ttl         time.Duration
	retryPeriod time.Duration
	id          string
	onState     func(bool)
	isLeader    atomic.Bool

	// stopped/leadCancel 支撑 Stop():优雅退出时主动让位,
	// 而不是把锁留到 TTL 自然过期。
	stopped    atomic.Bool
	mu         sync.Mutex
	leadCancel context.CancelFunc
	// token 是本任期写进锁键的值,不在任时为空串(见 Token)。由 mu 保护。
	token string
}

// New 构造一个选举器。key 是全体候选副本共用的锁 key。
func New(store Store, key string, opts Options) *Elector {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	retryPeriod := opts.RetryPeriod
	if retryPeriod <= 0 {
		retryPeriod = DefaultRetryPeriod
	}
	id := opts.ID
	if id == "" {
		id = "unknown"
	}
	return &Elector{
		store:       store,
		key:         key,
		ttl:         ttl,
		retryPeriod: retryPeriod,
		id:          id,
		onState:     opts.OnStateChange,
	}
}

// heartbeatInterval 是续期心跳的间隔 = TTL/3:TTL 内有 2~3 次续期机会兜住瞬时 Redis 抖动。
// 下限只做除零保护 —— 生产 TTL 是秒级(默认 30s),毫秒级 TTL 仅用于单测。
func heartbeatInterval(ttl time.Duration) time.Duration {
	return max(ttl/3, 10*time.Millisecond)
}

// CampaignInterval 是锁空出来之后跟随者最迟多久再竞选一次:竞选间隔 retryPeriod(<= 0 取 DefaultRetryPeriod)与
// 心跳间隔(TTL/3)取小。Run 按它等待;别处要按选主参数推算「接任最迟多久」(例如 scene_manager 分线目录的 TTL)时
// 调它,不要自己再写一遍。ttl 与 New 的口径相同:<= 0 按缺省 30s。
func CampaignInterval(ttl, retryPeriod time.Duration) time.Duration {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if retryPeriod <= 0 {
		retryPeriod = DefaultRetryPeriod
	}
	return min(retryPeriod, heartbeatInterval(ttl))
}

// IsLeader 报告本实例当前是否持有领导权。跨 goroutine 安全,
// 适合作为业务侧"变更动作闸门"的判定函数。
func (e *Elector) IsLeader() bool {
	return e.isLeader.Load()
}

// Token 返回本任期写进锁键的值,不在任时返回空串。跨 goroutine 安全。
//
// 它给领导者独占的写做围栏:写脚本在同一段里先核对锁键的值仍等于它,再写数据(锁键要和数据键同槽)。
// IsLeader 是本进程对自己的判断,会过时 —— 进程卡顿到锁过期,或 Redis 主从切换丢了锁之后,在下一次心跳之前它仍为真。
// 带着 Token 的写这时会被拒:锁键里已经是别人的值,或者空了。由存锁的那台 Redis 在写的那一刻判定谁是属主,
// 不依赖任何一方的时钟。只凭 IsLeader 做的写没有这层保证,要么本身幂等,要么自带按对象的条件。
func (e *Elector) Token() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.token
}

func (e *Elector) setToken(token string) {
	e.mu.Lock()
	e.token = token
	e.mu.Unlock()
}

// Run 阻塞运行候选者循环,直到 ctx 结束;通常 `go e.Run(...)`。
//
// 每次当选后调用 whileLeader(lctx):lctx 在丢锁或 ctx 结束时取消;
// whileLeader 返回后释放锁、重新竞选。传 nil 等价于"持有领导权直到丢失"。
// whileLeader 主动提前返回视为让位:释放锁并回到候选者循环。
func (e *Elector) Run(ctx context.Context, whileLeader func(ctx context.Context)) {
	interval := heartbeatInterval(e.ttl)
	retry := CampaignInterval(e.ttl, e.retryPeriod)
	if whileLeader == nil {
		whileLeader = func(lctx context.Context) { <-lctx.Done() }
	}

	for {
		if ctx.Err() != nil || e.stopped.Load() {
			return
		}

		val := e.id + ":" + randomToken()
		// SETNX 发出时刻:服务端写入不早于它,key 最早在它 +TTL 过期,
		// 本任期心跳的自我降级从这里起算。
		campaignAt := time.Now()
		ok, err := e.store.SetNX(ctx, e.key, val, e.ttl)
		if err == nil && !ok {
			// 回「已存在」不一定是别人在任:go-redis / go-zero 的 MaxRetries 缺省 3,首发 SETNX 已经写入、回包丢了时,
			// 客户端会原样重发,重发看到的正是自己这一次的值。不核对的话这把锁没人续期、空占到 TTL 过期,期间没有领导者
			// (docs/design/infra-reconnect-overview.md §4 R8)。写入发生在这次 SETNX 发出之后,campaignAt 仍是安全的起算点。
			// 核对失败按「别人在任」处理,下一拍再竞选。
			if owned, checkErr := e.store.EvalInt(ctx, ownedScript, []string{e.key}, val); checkErr == nil && owned == 1 {
				logx.Infof("[leader] campaign setnx key=%s id=%s was replayed after its first attempt won; the lock is ours", e.key, e.id)
				ok = true
			}
		}
		if err != nil {
			// 竞选失败必须出声:Redis ACL/网络长期不通时,否则所有副本都
			// 静默当跟随者,变更类循环无人执行且没有任何日志线索。
			logx.Errorf("[leader] campaign setnx key=%s id=%s failed: %v", e.key, e.id, err)
		}
		if err != nil || !ok {
			// 已有领导者,或 Redis 抖动 —— 等一个竞选间隔再竞选。
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
				continue
			}
		}

		logx.Infof("[leader] elected key=%s id=%s ttl=%s", e.key, e.id, e.ttl)
		// 先有围栏值、后有领导权:IsLeader 为真时 Token 一定已是本任期的值。
		e.setToken(val)
		e.isLeader.Store(true)
		e.notify(true)

		leadCtx, leadCancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.leadCancel = leadCancel
		e.mu.Unlock()
		stopHB := e.startHeartbeat(val, interval, campaignAt, func(err error) {
			logx.Errorf("[leader] lost leadership key=%s id=%s: %v", e.key, e.id, err)
			leadCancel()
		})

		whileLeader(leadCtx)

		stopHB()
		e.isLeader.Store(false)
		e.setToken("")
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
//   - 续期持续报错:距上次成功续期达到 FenceAfter 门槛就自我降级。
//     只把错误当瞬时抖动无限重试是不行的 —— Redis 单边不可达时本副本
//     永远读不到 0,而 key 在服务端照常过期、被人抢走,即双领导。
//
// acquiredAt 是本任期 SETNX 的发出时刻,作为"上次成功"的初值,续期节拍也从它排起。
// stop 返回后 onLost 不会再被调用,在途的那次续期回来也不会。定时器到点后在发出续期前、
// 续期返回后各查一次 stop:select 在 stopCh 与定时器同时就绪时随机选,必须让 stop 胜出 ——
// 否则可有可无的最后一次续期会把 key 再延长一个 TTL,拖慢 Run 收尾时带属主校验的释放与交接。
func (e *Elector) startHeartbeat(val string, interval time.Duration, acquiredAt time.Time, onLost func(error)) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	// 门槛都是为了赶在服务端 key 过期之前退位:
	//   - lastOK 记续期请求**发出**的时刻:PEXPIRE 不早于它执行,key 最早在
	//     lastOK+TTL 过期。记回包时刻会晚一个成功往返;出错回包又比成功快
	//     (如连接被拒)时,第 2 个出错拍的"距上次成功"够不到门槛,拖到第 3 拍
	//     ≈ TTL 才降级,和 key 过期赛跑。
	//   - renewTimeout 只是续期 ctx 的超时,封不住一次调用:客户端不认 ctx 截止时间时
	//     (go-redis 默认、go-zero Redis),socket 读写另受它自己的读写超时之和约束
	//     (CallBounder 报告的值),写之前的排队 / 重试退避 / 拨号又先吃掉最多 renewTimeout。
	//     所以 maxCall 取两段之和(CallBudget),门槛公式与三条约束见 FenceAfter;
	//     新建连接时的 HELLO / AUTH 握手读写仍不在其内。
	//   - 满足不了约束(TTL/3 不大于 maxCall)时 FenceAfter 退化为任一续期出错即降级,
	//     这时慢调用仍可能拖过 key 过期,只能靠调大 TTL,每个任期记一次错误日志。
	renewTimeout := min(2*time.Second, e.ttl/8)
	maxCall := renewTimeout
	if b, ok := e.store.(CallBounder); ok {
		maxCall = CallBudget(renewTimeout, b.MaxCallDuration())
	}
	fenceAfter, guaranteed := FenceAfter(e.ttl, interval, maxCall)
	if !guaranteed {
		logx.Errorf("[leader] heartbeat key=%s ttl=%s interval=%s: ttl too short for client worst-case call %s, "+
			"cannot guarantee self-fencing before lock expiry; demoting on any renew error; set ttl > 3x%s with margin",
			e.key, e.ttl, interval, maxCall, maxCall)
	}

	go func() {
		defer close(doneCh)
		lastOK := acquiredAt
		// 按 lastOK 排拍,不按本 goroutine 起动时刻起 ticker:SETNX 往返与同步的当选回调都在
		// 心跳起动之前,错相位的节拍会让第 2 个出错拍拖过 key 过期(见 FenceAfter 前提)。
		next := lastOK.Add(interval)
		timer := time.NewTimer(time.Until(next))
		defer timer.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-timer.C:
				// select 在 stopCh 与定时器同时就绪时随机选;先让 stop 生效,不再发出多余的续期。
				select {
				case <-stopCh:
					return
				default:
				}
				sentAt := time.Now()
				renewCtx, cancel := context.WithTimeout(context.Background(), renewTimeout)
				res, err := e.store.EvalInt(renewCtx, renewScript, []string{e.key},
					val, fmt.Sprintf("%d", e.ttl.Milliseconds()))
				cancel()
				// 续期在途时 stop 可能已被调用:Run 已在收尾,此后不得再 onLost。
				select {
				case <-stopCh:
					return
				default:
				}
				if err != nil {
					logx.Errorf("[leader] heartbeat renew error key=%s: %v", e.key, err)
					if since := time.Since(lastOK); since >= fenceAfter {
						onLost(fmt.Errorf("no successful renew for %s (>= %s, ttl %s), self-fencing: last err: %w",
							since, fenceAfter, e.ttl, err))
						return
					}
					next = next.Add(interval)
					timer.Reset(time.Until(next))
					continue
				}
				if res == 0 {
					onLost(fmt.Errorf("lock %s no longer owned by %s", e.key, e.id))
					return
				}
				lastOK = sentAt
				next = sentAt.Add(interval)
				timer.Reset(time.Until(next))
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
