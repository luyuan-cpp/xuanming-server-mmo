# Dispatcher subscriber 串行 LPop → 异步:解 95% callback_wait{failed}

**日期**: 2026-05-29
**前置**: [stress-1zone-25k-2026-05-28-subshard.md §5 17.8% 失败率的下一步](./stress-1zone-25k-2026-05-28-subshard.md#5-178-失败率的下一步)
> Round 7 的累计 fail% 17.8% 我曾解释为 ramp 期 cold-start 残留。今天再读 prom 数据,发现真相是 **dispatcher Pub/Sub subscriber goroutine 同步 LPop**,根本不是失败率"早就过去了"。

**结论**: 把 `dispatch()` 里的 LPop+Unmarshal+cb 全部移到独立 goroutine,subscriber 主循环只做 take()(微秒级 mu lock + map delete)。预期 callback_wait{success} 占比从 5% 升到 ~95%,fail% 从 17.8% 跌到 ~0。

> 这是一份**仅靠读数据**找到的瓶颈,没有再跑一次压测就开修。Round 8 用来验证。

---

## 1. Round 7 的反直觉数字

stress_summarize 给的 `cb_wait{ok} 2,289 ms` 是个误读 —— 那只是 **success 桶里命中的少数样本**的平均。把 prom 的 _count 字段也读进来:

```
dataloader_preload_kafka_send_seconds_count{result="success"}   = 46,881
dataloader_preload_callback_wait_seconds_count{result="success"} =  2,437   ← 5%
dataloader_preload_callback_wait_seconds_count{result="failed"}  = 44,444   ← 95%
```

`2437 + 44444 = 46881`,完美对应 kafka_send 成功数。也就是说:**Kafka 把消息送进 broker 之后,只有 5% 的 callback 在 5s TTL 内被 dispatcher dispatch 到**。

但同时:
- Kafka backlog 仅 4,233(partition 跟得上)
- db worker 实测 throughput ~190/s(已经把 input 200/s 吃干净)
- robot 视角 0 失败(scene-side NIL retry 兜底)

矛盾点:既然 db 侧吃完了,task:result Redis LPush + Publish 都执行了,login 侧为什么 95% 看不到回调?

---

## 2. Root cause:subscriber goroutine 单线程同步 LPop

`go/login/internal/dispatcher/task_result_dispatcher.go` 旧实现:

```go
ch := psub.Channel()
for {
    select {
    case msg, ok := <-ch:
        d.dispatch(ctx, msg.Payload)   // ← 同步执行,包含 LPop
    }
}

func (d *TaskResultDispatcher) dispatch(ctx context.Context, taskID string) {
    cb := d.take(taskID)
    if cb == nil { return }
    bytesVal, err := d.rc.LPop(...)    // ← 5-10ms Redis round-trip
    proto.Unmarshal(...)
    go cb(...)
}
```

每条 Pub/Sub 通知都让 subscriber 同步执行一次 Redis LPop。本机环境 LPop ~5ms,稳态 200/s task 意味着 subscriber 必须每秒做 200 次 LPop = 1000ms / 1000ms 已经满负载。

go-redis v9.16.0 `psub.Channel()` 的内部 ringbuf 默认 100,send timeout 60s。channel 不会立刻 drop,但消息在 channel 里**排队**:第 100 条比第 1 条晚到 ~500ms,第 1000 条晚到 ~5s,第 5000 条晚到 ~25s。

subscriber 轮到第 N 条时调 `take(taskID)`:这时 GC ticker(1s 一跳,见 568efa576)早就把 5s TTL 的 entry sweep 走了 → take 返回 nil → 静默丢弃 → caller 端在 5s 时被 sweepExpired 触发 `task result wait timed out`。

**所以 95% "失败" 的本质是:数据已经在 Redis,但 dispatcher 来不及 dispatch**。

---

## 3. 修复

```go
func (d *TaskResultDispatcher) dispatch(ctx context.Context, taskID string) {
    cb := d.take(taskID)        // ← 仍在 subscriber 主循环,微秒级
    if cb == nil { return }

    go func() {                 // ← LPop + Unmarshal + cb 全部下沉
        bytesVal, err := d.rc.LPop(ctx, resultKey).Bytes()
        ...
        cb(res, nil)
    }()
}
```

变化:
- subscriber 每条消息只做 `take()`,~µs 级,200/s 不可能积压
- LPop 并发数 = 飞行中 task 数,redis pool 默认 10 conn 够用(LPop 5ms × 10 = 500ms 容量,2000 task/s 才会饱和)
- take() 之后才 LPop = 同 taskID 不会被 dispatch 两次

---

## 4. 这个 bug 为什么活到了 Round 7

| 阶段 | dispatcherTaskTTL | GC tick | 实际 SLO | 表现 |
|---|---|---|---|---|
| pre-Round 5 | 30s | 15s | 30-45s | callback 慢但不 timeout(5ms × 200/s 在 30s 窗口内能消化掉积压尾) |
| Round 5+ (568efa576) | 5s | 1s | ~5s | 一旦稳态超过 ~100/s 就开始 5% 收到 + 95% 超时 |
| Round 7 (subshard) | 5s | 1s | ~5s | input 250/s,问题更显眼,但被 robot fail=0 + 累计 fail% 看起来稳态遮蔽 |

也就是说:Round 5 把 TTL 砍成 5s 是"正确的修",但暴露了 dispatcher 本身就跟不上。subshard 让 db 侧追上来,**input 排到 dispatcher 这一关**,瓶颈位置换了一次手。

---

## 5. 预期影响

- `dataloader_preload_callback_wait_seconds_count{success}` 占比 **5% → ~95%**
- `entergame_total_count{result="preload_failed"}` 累计速率从 ~30/s 降到 ~1/s 以内
- robot 表观无变化(早就 0 失败)
- scene-side `kMaxLoadRetries` 命中数应该从满血状态退回到 ~0
- Kafka backlog 不变(瓶颈不在 Kafka)
- preload{success} avg 可能略升(目前 29ms 是 5% 早到样本,真实 P50 应该是 ~50-100ms,真正长尾会被记进来)

不预期的副作用:
- **goroutine 数飙升**:稳态 200 task/s × 50ms LPop = ~10 个并发 goroutine,可忽略
- redis pool 压力:LPop 是单 round-trip 命令,go-redis pool 默认 10 conn,200/s × 5ms = 1 conn 满载,有冗余

---

## 6. Round 8 验收标准

跑 60 分钟 1zone-25k:

| 指标 | Round 7 | Round 8 目标 |
|---|---:|---:|
| robot fail | 0 | 0 |
| max_login | 209ms | < 500ms |
| entergame fail% | 17.8% | **< 2%** |
| callback_wait{success} 占比 | 5% | **> 95%** |
| Kafka backlog | 4,233 | < 5,000 |

如果 fail% 仍然 > 5%,说明 dispatcher 不是唯一瓶颈,要再深挖 ChannelSize / pool size / db_rpc 端 LPush 顺序。

---

## 7. 给下一个 AI 的提醒

这次教训:
- **不要只读 stress_summarize 的 avg 数字**,看 `_count` 才能识别 success vs failed 比例
- Pub/Sub `psub.Channel()` 的内部 buffer 是隐藏成本 — 任何 dispatcher 模式都要看 subscriber 主循环每条消息要花多久
- LPop / DEL / EXPIRE 这种"看起来很快"的 Redis 命令在串行 hot path 上要算 5-10ms × 入流量 才能下结论
