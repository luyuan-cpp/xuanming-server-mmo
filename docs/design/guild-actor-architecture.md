# Guild Actor 化架构设计

> 状态: 草案 / 设计阶段
> 范围: `go/guild` 服务
> 日期: 2026-06-04
> 关联: 已完成的 NodeInfo node_id 全局唯一改造、Snowflake worker id 独立分配改造

---

## 1. 背景与问题

### 1.1 当前 guild 写路径

guild_service 处理写请求(创建公会、申请入会、踢人、捐献、聊天、状态变更)的典型路径:

```go
func (l *GuildLogic) Donate(ctx, req) {
    // 1. 读公会数据(Redis 或 DB)
    guild := l.repo.GetGuild(ctx, req.GuildID)

    // 2. 改内存
    guild.Members[req.PlayerID].Contribution += req.Amount
    guild.Resources += req.Amount

    // 3. 写回(Redis SET / DB UPDATE)
    l.repo.SaveGuild(ctx, guild)
}
```

这个路径在低 QPS / 公会写竞争稀疏时完全够用。问题出在**同一个 guild_id 高并发写**的场景。

### 1.2 50000 人公会场景下的锁竞争

设想最坏场景: **1 个 5000 人的超大公会**,正常活跃 1000 人,每人每秒 1 次公会操作 =
**单 guild_id 1000 QPS**。下面对比三种锁方案的表现。

#### 方案 A: 乐观锁(Redis WATCH / DB version 字段)

```go
guild := repo.Get(...)         // version=42
guild.Resources += 100
repo.SaveIfVersion(..., 42)    // 期望 version=42 才能写
```

1000 个并发请求,只有 1 个能成功写入,**其余 999 个 version mismatch → 重试**。

- p50 延迟还行(运气好的请求一次过)
- **p99 延迟爆炸**(运气差的请求重试几十次)
- DB CPU 100%(都在跑无效的 read-modify-CAS)
- 高竞争下接近**活锁**: 成功率 ≈ 1/N

#### 方案 B: Redis 分布式锁(SETNX / Redlock)

```go
lock := redis.SetNX("guild_lock:" + req.GuildID, "...", 5s)
defer lock.Release()
guild := repo.Get(...)
guild.Resources += 100
repo.Save(...)
```

1000 个并发请求**串行排队**。每个请求耗时拆解:

| 步骤 | 耗时 |
|------|------|
| 获取锁 | ~0.5ms |
| 读公会数据 | ~1ms |
| 计算 + 写回 | ~1ms |
| 释放锁 | ~0.5ms |
| **合计** | **~3ms** |

理论吞吐 ≈ 333 QPS,实际更低。剩余 667 个请求要排队,**p99 延迟可能到几秒**。
而且 Redis 上的 `guild_lock:{超大公会}` 是**单 key 热点**,Redis 单 shard CPU 被它打满。

#### 方案 C: DB 行锁(`SELECT ... FOR UPDATE`)

类似 B,但锁竞争发生在 MySQL 行上。MySQL 单行的 lock wait 队列在高竞争下 p99 几秒起步,
还会拖累整个 InnoDB 实例的事务系统。**比 Redis 锁还差**。

### 1.3 共同瓶颈

不管选 A / B / C, **核心瓶颈都一样**:

> **同一个 guild_id 的所有写操作 = 一次跨进程同步原语 + 多次跨进程 IO,串行执行**

每次操作至少 3-5ms 的"协调成本",且这成本花在和业务逻辑无关的事情上(争锁、读 IO、写 IO)。
真正的业务计算(`Resources += 100`)耗时不到 1 微秒。

**协调成本是业务计算的 3000 倍。**

要把这个比例改回正常水位,**必须把"读-改-写"事务从跨进程改成进程内**。这就是 actor 化要做的事情。

---

## 2. 方案概述

### 2.1 核心思想

```
[原模式]                        [actor 化]
gRPC handler                    gRPC handler
   ↓ Redis lock                    ↓ channel send
   ↓ Redis/DB read                actor goroutine
   ↓ 内存计算                       ↓ 直接改内存
   ↓ Redis/DB write                ↓ channel reply
   ↓ Redis unlock                 (后台异步刷盘)
   ~3ms                            ~50μs
```

把每个 guild_id 映射到一个**常驻 goroutine** + **常驻内存状态**。
所有同 guild_id 的请求**走 channel 串行进入这个 goroutine**,
goroutine 直接操作内存状态,**进程内串行,无 IO,无跨进程协调**。

刷盘改为**后台批量异步**: 每 100ms 或每 N 次修改,把内存状态写回 Redis/DB。
N 次操作合并成 1 次 IO, **DB 写流量降低 99%**。

### 2.2 性能对比

同 guild_id 1000 QPS 写场景:

| 维度 | 锁方案(B) | actor 化 |
|------|------------|---------|
| 单请求延迟 | ~3ms(锁+IO) | <1ms(channel 排队 + 内存) |
| 1000 QPS 总耗时 | 3 秒(串行 IO) | ~1 毫秒(串行内存) |
| DB 写次数 | 1000 次/秒 | 10 次/秒(100ms 合并) |
| Redis 锁压力 | 4000 ops/秒(单 key) | 0 |
| p99 延迟 | 几秒 | <10ms |

**单实例从 ~333 QPS/公会 提升到 ~30000 QPS/公会**, 一个数量级以上的跳变。

### 2.3 为什么这是"真正"的解决方案

锁方案在 ~3ms 这个量级里找 10%-30% 的改进; actor 化是**协调成本本身的数量级变化**(3ms → 50μs)。
其他方案优化的是常数,actor 化优化的是渐近行为。

---

## 3. 架构图

### 3.1 进程内架构(单实例,本次范围)

```
                      C++ 客户端
                          │
                          │ gRPC: Donate(guild_id=42, player=X, amount=100)
                          ▼
         ┌────────────────────────────────────────┐
         │  guild_service 进程                     │
         │                                        │
         │  ┌──────────────────────────────────┐  │
         │  │ gRPC server (zrpc / grpc-go)     │  │
         │  │   每请求 1 goroutine              │  │
         │  └──────────────────────────────────┘  │
         │                  │                     │
         │                  ▼                     │
         │  ┌──────────────────────────────────┐  │
         │  │ ActorRouter                      │  │
         │  │   actors[42].Send(ctx, cmd)      │  │
         │  │   等 reply chan 返回              │  │
         │  └──────────────────────────────────┘  │
         │                  │                     │
         │                  ▼                     │
         │  ┌─────────────┐  ┌─────────────┐     │
         │  │ Actor #42   │  │ Actor #99   │ …   │
         │  │ goroutine   │  │ goroutine   │     │
         │  │ inbox chan  │  │ inbox chan  │     │
         │  │ state(内存) │  │ state(内存) │     │
         │  │ dirty flag  │  │ dirty flag  │     │
         │  └─────────────┘  └─────────────┘     │
         │         │                              │
         │         ▼ (异步,后台刷盘 goroutine)    │
         │  ┌──────────────────────────────────┐  │
         │  │ FlushScheduler                   │  │
         │  │   每 100ms 扫 dirty actors        │  │
         │  │   批量写 Redis / MySQL           │  │
         │  └──────────────────────────────────┘  │
         └────────────────────────────────────────┘
                  │                  │
                  ▼                  ▼
                Redis              MySQL
```

### 3.2 多实例架构(未来,需要 C++ 端配合)

```
        多个 guild_service 实例                       MySQL
        ┌──────────┐  ┌──────────┐  ┌──────────┐        ▲
        │ inst A   │  │ inst B   │  │ inst C   │        │
        │ guilds:  │  │ guilds:  │  │ guilds:  │ ──────┘ flush
        │ 1,4,7…   │  │ 2,5,8…   │  │ 3,6,9…   │
        └──────────┘  └──────────┘  └──────────┘
              ▲             ▲             ▲
              │             │             │
              │ jump_hash(guild_id) % 3   │
              │             │             │
        ┌─────────────────────────────────────┐
        │  C++ 客户端(gate / scene)           │
        │  watch etcd 拿实例列表,本地 hash    │
        └─────────────────────────────────────┘
```

**本次设计不涉及多实例**, 但本设计的分层和接口要确保**单实例 → 多实例**演进时 actor / 业务代码**零修改**, 只在路由层加 jump hash + 故障转移逻辑。

---

## 4. gRPC 集成详细设计

### 4.1 gRPC + actor 不矛盾,反而是黄金组合

**关键观察**:

- gRPC 是**进程间**协议
- actor 是**进程内**并发模型
- 两者在不同层级,不冲突,反而互补

grpc-go 默认每个请求一个 goroutine,这个 goroutine **阻塞在 channel 上等 actor 回复**, 不浪费——它本来就要阻塞等业务结果。

### 4.2 Handler 写法

```go
// internal/server/guild_server.go
type GuildServer struct {
    pb.UnimplementedGuildServiceServer
    router *actor.Router        // actor 路由
}

func (s *GuildServer) Donate(ctx context.Context, req *pb.DonateRequest) (*pb.DonateResponse, error) {
    // 把请求投递给对应 guild_id 的 actor,等回复
    reply, err := s.router.Send(ctx, req.GuildId, &cmd.DonateCmd{
        PlayerID: req.PlayerId,
        Amount:   req.Amount,
    })
    if err != nil {
        return nil, status.Error(codes.Internal, err.Error())
    }
    // actor 返回的是已构造好的 response,handler 直接返回
    return reply.(*pb.DonateResponse), nil
}

func (s *GuildServer) CreateGuild(ctx context.Context, req *pb.CreateGuildRequest) (*pb.CreateGuildResponse, error) {
    // 创建公会比较特殊: guild_id 还没分配,不能按 guild_id 路由
    // 走"无路由 actor"——专门的 GuildCreator actor 处理所有创建请求
    reply, err := s.router.SendToCreator(ctx, &cmd.CreateGuildCmd{
        Name:     req.Name,
        PlayerID: req.PlayerId,
        ZoneID:   req.ZoneId,
    })
    if err != nil {
        return nil, status.Error(codes.Internal, err.Error())
    }
    return reply.(*pb.CreateGuildResponse), nil
}
```

### 4.3 gRPC ctx 超时与 actor 等待的传播

`Router.Send` 内部:

```go
func (r *Router) Send(ctx context.Context, guildID uint64, command interface{}) (interface{}, error) {
    a := r.getOrCreate(guildID)

    reply := make(chan result, 1)
    env := envelope{ctx: ctx, cmd: command, reply: reply}

    // 1. 投递到 actor inbox; ctx 已取消则立即返回
    select {
    case a.inbox <- env:
    case <-ctx.Done():
        return nil, ctx.Err()
    }

    // 2. 等 actor 回复; ctx 已取消则立即返回
    //    actor 那边也会检查 ctx,处理完发现客户端已断就丢弃 reply
    select {
    case res := <-reply:
        return res.value, res.err
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}
```

actor 主循环里发回复时也要尊重 ctx:

```go
for env := range a.inbox {
    value, err := a.handle(env.ctx, env.cmd)
    select {
    case env.reply <- result{value, err}:
    case <-env.ctx.Done():
        // 调用方已放弃,不阻塞 actor
    default:
        // reply chan 是 buffer=1,正常情况下能立即写入
        // 这分支理论上走不到,留作 defense-in-depth
    }
}
```

**结果**: 客户端断连 → ctx 取消 → handler 立即返回 `Canceled` → actor 处理完丢弃 reply。
actor 不会因为客户端断连而阻塞。

### 4.4 流式 RPC(Server Stream)

公会内有些功能天然是订阅推送(公会聊天、状态变更广播)。actor 模型下这变得**简单**:

```go
type GuildActor struct {
    state       *data.GuildData
    subscribers map[uint64]chan *pb.GuildEvent  // playerID → event chan
}

func (s *GuildServer) SubscribeEvents(req *pb.SubscribeRequest, stream pb.GuildService_SubscribeEventsServer) error {
    eventCh := make(chan *pb.GuildEvent, 16)

    // 让 actor 把 eventCh 加入订阅列表
    if _, err := s.router.Send(stream.Context(), req.GuildId, &cmd.SubscribeCmd{
        PlayerID: req.PlayerId,
        EventCh:  eventCh,
    }); err != nil {
        return err
    }
    defer s.router.Send(context.Background(), req.GuildId, &cmd.UnsubscribeCmd{
        PlayerID: req.PlayerId,
    })

    for {
        select {
        case ev := <-eventCh:
            if err := stream.Send(ev); err != nil {
                return err
            }
        case <-stream.Context().Done():
            return nil
        }
    }
}
```

actor 处理写命令时, 状态变更后**直接广播给所有订阅者**:

```go
func (a *GuildActor) onDonate(cmd *cmd.DonateCmd) (*pb.DonateResponse, error) {
    // 改状态
    a.state.Members[cmd.PlayerID].Contribution += cmd.Amount
    a.state.Resources += cmd.Amount
    a.dirty = true

    // 广播事件——actor 内串行,广播是 goroutine-safe 的
    event := &pb.GuildEvent{Type: "donation", PlayerId: cmd.PlayerID, Amount: cmd.Amount}
    for _, ch := range a.subscribers {
        select {
        case ch <- event:
        default:
            // 订阅者 channel 满,丢弃事件; 客户端处理慢
            // 上策: 监控 metrics,触发剔除慢订阅
        }
    }

    return &pb.DonateResponse{NewTotal: a.state.Resources}, nil
}
```

**注意**: 不要用阻塞 send。一个慢订阅会卡住整个 actor。

---

## 5. Actor 框架选型

### 5.1 候选方案

#### 选项 A: protoactor-go(`asynkron/protoactor-go`)

GitHub ~5k stars, 生产级, Microsoft Orleans 的 Go 移植 + Akka 模式。

**提供**:

- `PID` 抽象(actor 引用,可跨进程)
- Cluster 模式 + 一致性哈希(横向扩展自带)
- Persistence(状态持久化 / 恢复)
- 集群成员管理(基于 etcd / consul)
- gRPC remote actor(actor 在另一台机器上,本地代码透明调用)

**适合**: 想认真做大型分布式有状态服务,需要"actor 不在本进程时也能调用"的场景。

**代价**:

- 学习曲线陡。要理解 PID / Behavior / Receive / 监督树等一整套概念
- 文档不算友好,中文资料少
- 抽象层很厚, debugging 时栈深 10+
- 对 Go 习惯的"channel + goroutine"开发者来说,**写起来像 Java**
- 集群模式启动起来要 etcd / consul,运维负担

#### 选项 B: 自己写 ~200 行

Go 的 channel + goroutine 就是 actor 模型的语言级支持。完整的 actor 框架核心**只需要这几个东西**:

```go
// 1. Actor 接口
type Actor interface {
    HandleCmd(ctx context.Context, cmd interface{}) (interface{}, error)
    Flush(ctx context.Context) error  // 异步刷盘
    OnEvict(ctx context.Context)      // 被 evict 前的清理
}

// 2. Mailbox + 工作 goroutine
type actorRunner struct {
    inbox    chan envelope
    actor    Actor
    idleTime time.Duration
    flushCh  chan struct{}
}

// 3. Router(按 key 路由到 actor)
type Router struct {
    mu     sync.RWMutex
    actors map[uint64]*actorRunner
    spawn  func(uint64) (Actor, error)
}
```

加上 `getOrCreate`、evict、metrics、lazy load,大概 **200 行**。

**优点**:

- 完全掌握每行代码,debug 容易
- 不引入任何依赖
- 性能比 protoactor-go 还好(没有 PID / message routing 的抽象开销)
- 完全契合 Go 习惯,代码读起来像普通 Go

**缺点**:

- 没有 Cluster 模式(但本设计的横向扩展由调用方 jump hash 实现,不需要框架级 Cluster)
- 没有监督树(Go 的 panic �ful 几行代码搞定)
- 没有 actor persistence(自己控制 flush 节奏)

### 5.2 对比表

| 维度 | protoactor-go | 自己写 ~200 行 |
|------|--------------|---------------|
| 代码量 | 0 行业务代码,但要学几千行框架 | ~200 行框架 + 业务代码 |
| 学习曲线 | 高(Akka/Orleans 模型) | 低(就是 Go 习惯) |
| 跨进程 actor | ✅ 内置 | ❌ 需要外部路由(jump hash) |
| 监督树 | ✅ 内置 | ❌ 自己写 panic recover |
| Persistence | ✅ 内置 | ❌ 自己写 Flush |
| Cluster 成员管理 | ✅ etcd / consul 集成 | ❌ 不需要(jump hash 由调用方做) |
| 性能 | 抽象开销,相对慢 | 直接 channel,最快 |
| Debug 友好度 | 栈深 10+,异步消息溯源难 | 普通 Go 栈,gdb / pprof 直接看 |
| 失败时迁回成本 | 高(框架理念绑定深) | 低(自己的代码,接口随便改) |
| 二进制大小 | +数 MB(框架依赖) | +几 KB |
| 升级风险 | 框架版本升级可能 break | 没有外部依赖 |

### 5.3 决策(本轮不下)

**这一轮先把对比留在文档里,下一轮再定**。

倾向: **自己写 ~200 行**,理由:

- 已经有 jump hash 路由方案
- 不需要"跨进程 actor"抽象
- 单实例先做完,看效果再决定是否需要 Cluster
- 自己写,后期迁 protoactor-go 接口可控
- "先做不要的功能 = 浪费时间,事后还得反学"

但若团队对 Akka/Orleans 有经验,protoactor-go 也合理。**等下一轮和团队评审一起决定**。

---

## 6. 数据一致性

### 6.1 内存权威 + 异步刷盘

actor 化下, **内存状态是权威, DB 是落盘备份**。这与"每次写都同步刷 DB"的传统模式不同, 必须正面回答几个问题:

#### Q1: actor 进程崩了, 内存状态丢了怎么办?

**最近一个 flush 周期内的写丢失**(默认 100ms)。

应对策略(三选一,按业务容忍度):

##### 策略 1: 接受丢失(适合多数场景)

100ms 丢失窗口对绝大多数公会操作可接受(捐献掉一秒、聊天少一句)。
配上"重启时从 DB 重新 load"机制,服务恢复透明。

##### 策略 2: WAL(Write-Ahead Log)

```
gRPC handler → actor → ① 写 Kafka WAL(同步) → ② 改内存(同步) → reply
                                                    │
                                                    ▼
                                           后台异步刷盘
```

- 同步写 Kafka(~1ms),内存修改前先 WAL
- 进程崩了 → 重启时回放 Kafka 上未刷盘的 WAL 记录
- 延迟从 50μs 升到 ~1ms,但仍比锁方案的 ~3ms 快

##### 策略 3: 减小 flush 间隔

- 10ms flush 间隔,丢失窗口 10ms
- DB 写压力相对回升, 但仍比锁方案低 90%(原来每次写都刷)

**推荐**: 默认走策略 1; 关键操作(创建公会、解散公会)单独走策略 2 或同步写 DB。

#### Q2: 如何保证 actor 崩了之后,DB 仍能恢复出最新一致状态?

actor 启动时:

1. lazy load: 从 Redis 读公会快照(如果有缓存)
2. cache miss → 从 MySQL load
3. 标记 actor 为 "ready", 开始接收请求

actor 退出时(主动 evict 或进程关闭):

1. 把 dirty 状态 flush 一次
2. 等 flush 成功 ack 后才释放 actor 槽位

进程异常崩溃(SIGKILL 等):

1. 操作系统直接杀,actor 来不及 flush
2. **这里走"接受丢失"或"WAL 重放"的兜底**
3. 重启后新 actor lazy load,从 DB 拿到上一次成功 flush 的状态

#### Q3: flush 成功的语义?

每个 actor 有 `dirtyVersion uint64` 计数器, 每次写命令处理后 `dirtyVersion++`。

flush 时:

1. 记录当前 `flushTarget = dirtyVersion`(原子读)
2. 把状态序列化为快照
3. 写 Redis SET / MySQL UPDATE
4. 写成功后 `lastFlushedVersion = flushTarget`

如果 flush 时 actor 仍在接收新写, dirtyVersion 会继续涨——**没关系,下一次 flush 周期再覆盖**。

判断 actor 是否 dirty: `lastFlushedVersion < dirtyVersion`。

---

## 7. Actor 生命周期

### 7.1 状态机

```
   [Spawning] ──load OK──▶ [Ready] ──idle 5min──▶ [Evicting] ──flush OK──▶ [Dead]
       │                       │                       │
       │ load failed            │ HandleCmd 失败但已恢复  │ flush failed
       ▼                       ▼                       ▼
    [Dead]                 [Ready]                  [Retry up to 3x] → [Dead]
```

### 7.2 详细生命周期

```go
// Router.getOrCreate 是 lazy spawn 入口
func (r *Router) getOrCreate(guildID uint64) (*actorRunner, error) {
    r.mu.RLock()
    if a, ok := r.actors[guildID]; ok {
        r.mu.RUnlock()
        return a, nil
    }
    r.mu.RUnlock()

    r.mu.Lock()
    defer r.mu.Unlock()
    // double-check 避免重复创建
    if a, ok := r.actors[guildID]; ok {
        return a, nil
    }

    actor, err := r.spawn(guildID)  // 调用方传入的工厂,内部从 DB load
    if err != nil {
        return nil, err
    }

    runner := &actorRunner{
        inbox:    make(chan envelope, 256),  // buffer 256: 突发流量缓冲
        actor:    actor,
        idleTime: 5 * time.Minute,
    }
    r.actors[guildID] = runner

    go runner.run(r, guildID)
    return runner, nil
}

// actorRunner.run 是 actor 主循环
func (r *actorRunner) run(parent *Router, guildID uint64) {
    defer func() {
        if rec := recover(); rec != nil {
            log.Errorf("actor %d panic: %v", guildID, rec)
            // 不重启,evict; 下次请求来 lazy load
        }
        parent.evict(guildID)
    }()

    idleTimer := time.NewTimer(r.idleTime)
    defer idleTimer.Stop()

    for {
        select {
        case env := <-r.inbox:
            if !idleTimer.Stop() {
                <-idleTimer.C
            }
            value, err := r.actor.HandleCmd(env.ctx, env.cmd)
            select {
            case env.reply <- result{value, err}:
            default:
                // reply buffer=1,正常能写入,这里防漏
            }
            idleTimer.Reset(r.idleTime)

        case <-idleTimer.C:
            // 空闲超时,evict
            ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            if err := r.actor.Flush(ctx); err != nil {
                log.Errorf("actor %d flush before evict failed: %v", guildID, err)
            }
            r.actor.OnEvict(ctx)
            cancel()
            return
        }
    }
}
```

### 7.3 优雅关闭

进程接收 SIGTERM 时:

```go
func (r *Router) Shutdown(ctx context.Context) error {
    r.mu.Lock()
    actors := make(map[uint64]*actorRunner, len(r.actors))
    for k, v := range r.actors {
        actors[k] = v
    }
    r.mu.Unlock()

    var wg sync.WaitGroup
    for guildID, runner := range actors {
        wg.Add(1)
        go func(id uint64, ar *actorRunner) {
            defer wg.Done()
            // 给 actor 发 evict 信号(关 inbox)
            close(ar.inbox)
            // 等 actor goroutine 退出,它会在 evict 时 flush
            <-ar.done
        }(guildID, runner)
    }
    wg.Wait()
    return nil
}
```

guild_service main 函数:

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

s := zrpc.MustNewServer(...)
go s.Start()

<-sigCh
shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

s.Stop()                // 先停 gRPC,不再接新请求
router.Shutdown(shutdownCtx)  // 再 evict 所有 actor,落盘
```

---

## 8. 路由与故障转移

### 8.1 单实例(本次范围)

只有一个 guild_service 实例的情况:

- 所有 gRPC 请求都路由到这个实例(C++ 端只能看到一个)
- 实例内部按 guild_id 路由到 actor
- 实例挂了 → 服务全停, 等 K8s/systemd 拉起,重启时从 DB load
- **不需要 jump hash**,但 Router 的接口已经预留 `guildID` 参数,扩展零修改

### 8.2 多实例(未来,需要 C++ 端配合)

#### 路由策略: jump consistent hash by guild_id

```cpp
// C++ 端伪代码
std::vector<NodeInfo> instances = etcd_watcher.list("GuildNodeService.rpc/...");
int64_t target_idx = jump_consistent_hash(req.guild_id, instances.size());
NodeInfo target = instances[target_idx];
grpc::ChannelArguments args;
auto channel = grpc::CreateChannel(target.endpoint(), creds);
auto stub = guild::GuildService::NewStub(channel);
stub->Donate(...);
```

每个 guild_id 始终路由到同一个实例。实例数变化时, jump hash 保证只有 1/N 的 guild_id 换归属。

#### 故障转移

**场景**: instance B 挂了, 它持有的 guild_id 集合需要迁移到 instance A 和 C。

```
instances = [A, B, C]
B 挂掉, etcd 自动剔除 B
instances = [A, C]
jump_hash(guild_id=42, 2) = 0 → A
```

**问题**: instance B 在崩之前的 100ms 写没刷盘, 现在迁到 A, A 从 DB load 时拿到的是**旧状态**。

**解决方案**:

##### 方案 1: 接受丢失(单 guild 场景下危害不大)

100ms 丢失对捐献、聊天可接受。

##### 方案 2: WAL + 重放

新 actor 在 instance A 启动时:
1. 从 DB load
2. 从 Kafka WAL 找该 guild_id 在 lastFlushedVersion 之后的记录
3. 重放 WAL,recovery 到 B 崩之前的状态

##### 方案 3: 同步写

关键操作(如转账给个人)用同步写,不依赖 flush。

#### 单实例锁(防双主)

instance B 没真挂,只是网络分区。jump_hash 把 guild=42 路由到 A,但 B 仍以为自己是 owner。
**双主写**风险。

防双主:

1. 每个 actor 启动时,在 etcd 拿一个 lease lock: `/guild/owner/{guild_id} = {instance_uuid}`
2. CAS Version==0 才能拿,拿不到说明别人已 own
3. lease 过期 → 锁释放,新 owner 能 CAS 成功
4. 老 owner 要在每次 flush 前**校验** lease 还在: `If Value(lockKey) == myUUID Then OpPut snapshot`

这套机制和 NodeInfo allocator 完全同构, 我们已经验证过。

### 8.3 本次设计的工作量裁剪

| 阶段 | 内容 | 是否本次做 |
|------|------|-----------|
| 阶段 1 | actor 框架(Router + Actor + lazy spawn + evict) | ✅ |
| 阶段 2 | guild_logic 改造成命令式(GuildActor + Cmds) | ✅ |
| 阶段 3 | 异步刷盘(FlushScheduler + dirtyVersion) | ✅ |
| 阶段 4 | 优雅关闭(Shutdown drain) | ✅ |
| 阶段 5 | 单实例压测验证 | ✅ |
| 阶段 6 | jump hash 路由 | ❌ 需要 C++ 配合 |
| 阶段 7 | 故障转移 + WAL | ❌ 需要 C++ 配合 |
| 阶段 8 | 跨实例分布式锁(防双主) | ❌ 需要 C++ 配合 |

**本次只做阶段 1-5**, 单实例 actor 化, 纯 Go 改造, 不依赖 C++ 端。

---

## 9. 改造范围与文件级影响

### 9.1 新增

```
go/guild/internal/actor/
  ├── router.go         # Router + getOrCreate + Shutdown(~80 行)
  ├── runner.go         # actorRunner + run loop + evict(~80 行)
  ├── interface.go      # Actor interface 定义(~30 行)
  └── router_test.go    # 单元测试(~150 行)

go/guild/internal/actor/guild/
  ├── actor.go          # GuildActor 实现(~200 行)
  ├── cmds.go           # 所有命令类型定义(~100 行)
  └── actor_test.go     # 单元测试(~150 行)

go/guild/internal/flusher/
  ├── flusher.go        # FlushScheduler(~80 行)
  └── flusher_test.go   # 单元测试(~100 行)

docs/design/guild-actor-architecture.md   # 本文档
```

### 9.2 修改

| 文件 | 改动 |
|------|------|
| `go/guild/internal/logic/guild_logic.go` | **整体重构**: 业务逻辑搬到 `internal/actor/guild/actor.go`,`GuildLogic` 退化为薄路由(可能直接删除) |
| `go/guild/internal/server/guild_server.go` | handler 改为 `router.Send(ctx, guild_id, cmd)` |
| `go/guild/guild.go` | 启动时创建 `Router` + `FlushScheduler`; 关闭时 `router.Shutdown` |
| `go/guild/internal/svc/servicecontext.go` | 增加 `Router` 引用 |

### 9.3 不动

- `go/guild/internal/data/` 仓储层不变,actor 内部仍调用 repo.Save / repo.Get
- `proto/guild/` proto 不变
- C++ 客户端无任何改动
- gRPC 接口签名不变(只是 handler 内部实现换)

---

## 10. 命令对照表(GuildLogic 方法 → ActorCmd)

| 当前 GuildLogic 方法 | 新 Cmd 类型 | 备注 |
|--------------------|------------|------|
| `CreateGuild` | `CreateGuildCmd` | 走 GuildCreator(无 guild_id 路由) |
| `JoinGuild` | `JoinGuildCmd` | 路由 by guild_id |
| `LeaveGuild` | `LeaveGuildCmd` | 路由 by guild_id |
| `KickMember` | `KickMemberCmd` | 路由 by guild_id |
| `Donate` | `DonateCmd` | 路由 by guild_id |
| `GetGuildInfo` | `GetGuildInfoCmd` | 读路径,可走快照不进 actor(优化) |
| `ListMembers` | `ListMembersCmd` | 读路径 |
| `SetMemberRole` | `SetMemberRoleCmd` | 路由 by guild_id |
| `DismissGuild` | `DismissGuildCmd` | 路由 by guild_id, 完成后 evict actor |

### 10.1 读请求是否过 actor?

两种选择, 各有取舍:

**方案 A: 读也走 actor(强一致)**

- 所有读都拿到最新内存状态
- 慢查询(列大公会成员)会阻塞 actor 串行队列,降低写吞吐

**方案 B: 读走快照 + cache(高吞吐)**

- actor 每次 flush 时也更新 Redis 快照
- 读直接查 Redis,绕过 actor
- 数据可能滞后 100ms(等于 flush 间隔)
- 写吞吐不受影响

**推荐: 方案 B**, 理由: 公会读操作通常对实时性要求不高, "100ms 前的成员列表"完全够用。
关键写后立即读自己(如转账后立刻看余额) → 让 handler 在写返回时直接拿 actor 内存的最新值塞进 response。

**权限例外**：快照滞后只适用于展示读，不能用于授权。公告、踢人、角色调整等
权限写在落 MySQL 前必须在同一事务内锁定并复核权威 `guild_member.role`；否则
操作者降权/退会后的 Redis 或 actor 旧快照仍会放行写操作。当前
`SetAnnouncement` 已采用 `guild → guild_member` 的固定锁序完成该门禁。

---

## 11. 监控指标

```go
// 必加 metrics
guild_actor_count{state="ready"}              // 当前活跃 actor 数
guild_actor_inbox_pending{guild_id}            // 队列堆积(超阈值告警)
guild_actor_cmd_latency{cmd_type, p50/p99}    // 处理延迟
guild_actor_flush_latency_seconds              // flush 延迟分布
guild_actor_flush_lag{guild_id}               // dirtyVersion - lastFlushedVersion
guild_actor_eviction_total{reason}            // idle / shutdown / panic
guild_actor_load_latency_seconds              // lazy load 耗时
guild_actor_panic_total                        // 异常退出次数
guild_subscriber_drop_total{guild_id}         // 慢订阅丢事件
```

监控关键告警:

- `inbox_pending > 100` 持续 30s → 该 guild 写过载, 上游限流
- `flush_lag > 10000` → flush 跟不上写速度, 调小 flush 间隔或加 WAL
- `panic_total` 任何增长都告警

---

## 12. 风险与回滚

### 12.1 风险清单

| 风险 | 影响 | 缓解 |
|------|------|------|
| 内存状态丢失(进程崩) | 100ms 写丢失 | 1) 接受 2) WAL 3) 减小 flush 间隔 |
| actor 内 panic | 单 guild 不可用直至 lazy reload | recover 后立即 evict, 下次请求 lazy 重启 |
| 大公会内存占用 | 5000 人公会 ~500 KB,5000 公会 ~2.5 GB | OK 范围, 监控告警 |
| flush 失败 → 状态丢 | 写入 Redis/DB 失败 | flush 重试 3x, 失败告警 + 告警时不 evict actor |
| 慢订阅卡 actor | 不会卡(default 分支丢事件) | 监控丢事件率 |
| 启动 lazy load 雪崩 | 500 个公会同时 load → DB 压力 | load 限流(SingleFlight + 并发上限) |
| guild_id 路由不稳定(单实例没问题) | 多实例时 jump hash 漂移 | 阶段 6 故障转移 + 锁机制 |

### 12.2 回滚方案

actor 化是**单向**改造,回滚成本高。但可以做**功能开关**:

```yaml
# guild.yaml
Actor:
  Enabled: false           # 关 → 走旧的 GuildLogic
  FlushIntervalMs: 100
  IdleTimeoutMin: 5
  InboxBufferSize: 256
```

代码层面保留旧 GuildLogic 一段时间, gRPC handler:

```go
func (s *GuildServer) Donate(ctx, req) {
    if config.Actor.Enabled {
        return s.donateViaActor(ctx, req)
    }
    return s.donateViaLegacy(ctx, req)
}
```

压测稳定 1 周后删 legacy 路径。

### 12.3 灰度方案

按 guild_id 段灰度:

```go
if config.Actor.Enabled && req.GuildId % 100 < config.Actor.RolloutPercent {
    // 走 actor
}
```

`RolloutPercent` 从 1 → 10 → 50 → 100, 每档跑 1 天看 metrics。

---

## 13. 测试计划

### 13.1 单元测试

- Router: `getOrCreate`, `Send`, `Shutdown` 各 case
- actorRunner: 命令处理 / panic recover / idle evict / flush 失败
- GuildActor: 每个 cmd 单独测正确性 + 错误路径
- 并发安全: Race 模式跑 1000 个 goroutine 同时 Send 不同 guild_id

### 13.2 集成测试(`//go:build integration`)

- 真实 Redis + MySQL
- 同 guild_id 1000 并发 Donate, 校验最终余额正确
- 多 guild_id 并发, 校验互不串流
- 进程 SIGTERM 测试 graceful shutdown 落盘
- 进程 SIGKILL 测试重启后状态(应是 last flushed)

### 13.3 压测对比

按 CLAUDE.md §9 流程:

1. **跑测前**: 跑一轮**当前(锁方案)**guild 压测, 存为 `prev-summary.txt`
2. **改造完**: 跑同一份压测脚本
3. **出对比表**:
   - 单 guild_id 1000 QPS 下 p50/p99
   - 总吞吐
   - DB 写 QPS
   - Redis CPU
4. **复盘文档**: `docs/design/stress-guild-actor-<date>.md`

成功标准:

- 单 guild p99 < 50ms (锁方案 ~几秒)
- DB 写 QPS 降低 ≥ 90%
- 总吞吐 ≥ 锁方案 5x
- 无数据不一致(订单一致性、余额一致性 sanity check)

---

## 14. 工作量估计

| 阶段 | 内容 | 工作量 |
|------|------|------|
| 1 | actor 框架(`internal/actor/`) | 1.5 天 |
| 2 | GuildActor + Cmds 实现 | 1 天 |
| 3 | FlushScheduler | 0.5 天 |
| 4 | gRPC handler 改造 + svcCtx 接入 | 0.5 天 |
| 5 | 单元测试 + 集成测试 | 1.5 天 |
| 6 | 压测脚本 / 对比 / 调优 | 1.5 天 |
| 7 | 文档 / 灰度 / 监控指标 | 0.5 天 |
| **合计** | | **~7 人天** |

---

## 15. 不在本次范围

明确排除的事项:

- ❌ 多实例横向扩展(jump hash 路由)—— 需要 C++ 端配合
- ❌ 故障转移 + WAL —— 需要 C++ 端配合
- ❌ 跨实例分布式锁(防双主)—— 需要 C++ 端配合
- ❌ friend / player_locator actor 化 —— 它们是否有热点需独立评估
- ❌ scene_manager 改造 —— 它已经是 actor-like 模式(每个 scene 独立 goroutine)
- ❌ 公会战 / 公会 BOSS 等大型业务 —— 等基础 actor 化稳定再评估
- ❌ proto.actor / Ergo / 其他框架引入 —— 本轮决策推迟

---

## 16. 决策记录

| # | 决策 | 状态 | 备注 |
|---|------|------|------|
| 1 | 单实例 actor 化(本次范围) | 已定 | 不做横向扩展,等 C++ 方案 |
| 2 | actor 框架选型 | **未定** | 候选: 自己写 / protoactor-go,等下轮决定 |
| 3 | flush 间隔默认 100ms | 已定 | 可配置, 关键操作单独同步写 |
| 4 | 读路径走快照不进 actor | 已定 | 提升读吞吐, 100ms 滞后可接受 |
| 5 | actor evict idle = 5min | 已定 | 平衡内存占用与重 load 成本 |
| 6 | inbox buffer = 256 | 已定 | 突发流量缓冲, 满了上游限流 |
| 7 | 子订阅满直接丢事件 | 已定 | 用 metrics 监控丢事件率 |
| 8 | 提供 `Actor.Enabled` 开关灰度 | 已定 | 按 guild_id 段灰度推进 |

---

## 17. 后续动作

1. 等下一轮评审决定**框架选型**(本文 §5)
2. 决定后启动**阶段 1-5 实施**
3. 实施完成跑**对比压测**, 出复盘文档
4. 稳定 1 周后删除 legacy 路径
5. 评估**第三阶段 C++ 端配合**: 多实例 + jump hash + WAL

---

## 附录 A: 相关已完成改造

- NodeInfo node_id 全局唯一改造(2026-06-04)—— 解决 Snowflake worker id 跨 zone 冲突
- Snowflake worker id 独立分配改造(2026-06-04)—— 解耦 Snowflake seed 与 NodeInfo.NodeId

actor 化改造完成后, guild 服务的核心组件将是:

- NodeInfo allocator → 服务发现 node_id
- snowflakealloc → Snowflake worker id
- ActorRouter → guild_id 进程内路由
- FlushScheduler → 异步刷盘

四套机制各司其职, 边界清晰。

---

## 附录 B: 参考资料

- [Microsoft Orleans 文档](https://learn.microsoft.com/en-us/dotnet/orleans/) — Virtual Actor 模型, 本设计的概念来源
- [Akka Cluster Sharding](https://doc.akka.io/docs/akka/current/typed/cluster-sharding.html) — JVM 上的等价实现
- [protoactor-go](https://github.com/asynkron/protoactor-go) — Go 上的 Akka/Orleans 移植
- [Discord 论文/博客 -- 状态化分片实践](https://discord.com/blog/scaling-elixir-f9b8e1e7c29b) — Erlang 实战
- [Go memory model](https://go.dev/ref/mem) — channel happens-before 语义保证
- [jump consistent hash 论文](https://arxiv.org/abs/1406.2294) — 路由层算法

---

*文档完。框架选型结果出来后, 本文 §5.3 / §16 #2 会更新。*
