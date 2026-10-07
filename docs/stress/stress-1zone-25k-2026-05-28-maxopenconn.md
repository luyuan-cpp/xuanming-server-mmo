# MaxOpenConn 10→30 实测:解了一半,db worker 串行才是真上限

**日期**: 2026-05-28
**前置**: [stress-1zone-25k-2026-05-28-deep-dive.md §1 "MaxOpenConn 是真凶"](./stress-1zone-25k-2026-05-28-deep-dive.md#1-kafka-端真实-lag回答深挖-4)
**结论**: MaxOpenConn 10→30 让成功路径 callback_wait 暴跌 99%(5.14s → 34.6ms),
fail 占比从 46% 降到 29%,但 Kafka backlog 仍 80k —— **db worker 串行才是真正的吞吐天花板,
不是 MySQL 连接池**。深挖文档 §1 的诊断**对了一半**:连接池是 latency 瓶颈,但 throughput
瓶颈在 worker.start 的串行循环里。

> 配套 [ARCH.md §11](../design/ARCH.md#11-关键决策摘要) 第 19 行。
> 续 [Round 5 GC fix 专题](./stress-1zone-25k-2026-05-28-callback-wait.md) 和 [深挖专题](./stress-1zone-25k-2026-05-28-deep-dive.md)。

---

## 1. 改动

```yaml
# go/db/etc/db.yaml
MaxOpenConn: 10 → 30
MaxIdleConn:  3 → 10
```

单文件单 commit,无 schema 变化。

---

## 2. Round 6 25k smoke 数据(20 分钟全跑)

### Robot 视角

| 时刻 | conn | login_fail | login_stuck | enter_fail | recon_fb | max_login | msg/s |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1m | 2,412 | 0 | 0 | 0 | 0 | 151 | 120 |
| 5m | 25,182 | 0 | 0 | 0 | 0 | 153 | 199 |
| 6m | 31,151 | 0 | 0 | 0 | 0 | 153 | 199 |
| **7m** | 37,105 | **3** | **3** | 0 | **1** | **561** ← 首次抬头 | 195 |
| 8-20m | 43k–115k | **3 平稳** | **3 平稳** | **0** | **1 平稳** | 571 | 199 |
| 22m | 124,004 | 3 | 3 | 0 | 1 | 571 | 79 (达 client conn 上限) |

T+7m 出现 3 个 transient 失败 + 1 个 recon_fb,然后**保持平稳 15 分钟**。Round 5 同时刻数据完全一致(也是 T+7m 前后 max_login 抬到 ~570),说明这层是 "Round 5 跟本轮在 robot 端完全等价"。MaxOpenConn 升级对 robot 看到的指标没改变。

### login 后台真实改善

| 指标 | Round 5 (MaxOpenConn=10) | **Round 6 (MaxOpenConn=30)** |
|---|---:|---:|
| entergame_total{success} | 67,700(23 min) | **88,144(20 min)** ← +30% throughput |
| entergame_total{preload_failed} | 57,300 | **36,436** |
| fail / (fail+success) | **46%** | **29%** ← -17pp |
| preload{success} avg | 5,140 ms | **34.6 ms** ← **降 99.3%** |
| preload{preload_failed} avg | 5,490 ms | 5,500 ms ← 没变(TTL=5s 硬上限) |
| dataloader callback_wait{success} avg | 5,140 ms | **34.6 ms** |

**最关键的对比**:`preload{success} avg` 从 5.14 秒掉到 **34.6 毫秒**。这是 MaxOpenConn 真凶猜对的部分 — 当 MySQL 池子够大,真正能跑到 callback 的请求几乎全是亚秒级。

### 但 Kafka backlog 仍 80k

```
Round 5 final lag (20 min after stop): 91,815
Round 6 final lag (still in run):       80,055
```

**问题**:既然 MaxOpenConn=30 让单 task latency 从 5s 砍到 35ms,理论上 throughput 应该 30x,Kafka backlog 应该被吃干。**实测没有**。

---

## 3. 反转:db worker 串行才是真正天花板

### 容量重算

```
dataloader 进慢路径次数(失败 + 成功): 37,674 / 20 min = 31.4/s
每次 dataloader 进 2 个 sub-task: ~63 task/s 进 Kafka

Round 6 db consumer 实际处理:
  Round 6 起跑时 Kafka 余 91,815 backlog
  20 min 后 Kafka 余 80,055 backlog
  净处理 = 91,815 - 80,055 + 新进 (63/s × 1200s) = 87,360 task
  实际 throughput = 87,360 / 1200s ≈ 73/s
```

**db consumer 实际处理 73 task/s**,比 Round 5 的 ~50/s 高一些,但远不到 MaxOpenConn=30 理论容量(30 × 1000/35ms = 857/s)。

### 真正卡在哪里:worker.start 串行

`go/db/internal/kafka/key_ordered_consumer.go:402 func (w *worker) start`

```go
for {
  select {
  case task, ok := <-w.taskCh:
    // batch drain
    batch := []*workerTask{task}
    drainLoop: for { ... }
    w.processTaskBatch(batch, isOfflineExpand)  // ← 串行
  }
}
```

`processTaskBatch` 内是串行循环 `handleTask → processDBTask → MySQL FindOneByWhereClause/Save → Redis LPush + Pub/Sub Publish`。

整个 partition 的 worker 是**单 goroutine**,即使 MySQL 池有 30 个连接,**单 worker 永远只用 1 个连接**。

```
10 partition × 1 worker × 1 MySQL conn per worker = 10 MySQL connections in use
MaxOpenConn=30 → 20 个连接永远闲着!
```

这就是为什么 throughput 增长有限 — MaxOpenConn 给的容量 db consumer 用不到。

### 验证:MaxIdleConn=10 vs in-use 实际数

我没拉 MySQL `SHOW PROCESSLIST` 但推断 in-use connection ≈ 10(每 partition 1 个),空闲 20。

---

## 4. 修复方向(下一轮)

### 方案 A:partition 内多 goroutine 并行 + per-key 顺序

worker.start 改成 dispatcher 模式:
```go
// 单 receiver 把 task 按 key hash 分发给 sub-goroutine
keyShards := make([]chan *workerTask, 4)  // 4 sub-worker per partition
for i := range keyShards { go subWorker(keyShards[i]) }
for {
  task := <-w.taskCh
  subIdx := hash(task.Key) % len(keyShards)
  keyShards[subIdx] <- task
}
```

优点:partition 内并行度 × 4,且同 key 仍串行(per-player 顺序)。
缺点:batch coalescing 逻辑要重写,部分单测要重做。

### 方案 B:partition 数 10 → 20 或 30

简单但 broker / consumer rebalance 成本上升。10 → 20 应该没问题,30 开始要看 Kafka 调优。

### 方案 C:write-back 异步化

Kafka SyncProducer + 串行 worker 之外,把 `LPush task:result` + `Publish notify` 这两步放到独立 channel,worker 不等。问题是引入新的 race(result 可能在 SetSession 之前到达)。

### ROI 排序

按工作量与风险:
1. **B (partition=20)** — 改 yaml + 重启,1 行(login.yaml & db.yaml 同步),理论让 throughput 翻倍
2. **A (sub-shard)** — 30 行代码 + 单测,但效果可能更显著(每 partition 4×,有效并行 40)
3. **C** — 不建议,会引入新 race

**下次先试 B**,简单且可逆。

---

## 5. 三轮全跑对比总览

| 改动 | preload{success} avg | preload{failed} avg | total fail% | Kafka final lag |
|---|---:|---:|---:|---:|
| Round 4 (1+3, dispatcherTaskTTL 30→5s) | 282 ms | 11.5 s | (T+21m 死锁) | n/a |
| Round 5 (+ dispatcher GC 1s) | 5.14 s | 5.49 s | 46% | 91,815 |
| **Round 6 (+ MaxOpenConn 30)** | **34.6 ms** | 5.50 s | **29%** | **80,055** |

注意 Round 5 → Round 6 `preload{success} avg` 5.14s → 0.035s 是**两个数量级的改善**,但 fail% 只从 46% 降到 29%。说明:
- 成功路径上,每条 task 处理时长砍了 99%(MySQL 池子够了)
- 但失败路径仍受 dispatcherTaskTTL=5s 限制 — 这部分 task 永远拿不到 MySQL connection 就被强制 timeout,不是处理慢

**Round 7 目标**:partition=20,期待 fail% 砍到 < 10%。

---

## 6. Run artifacts

- Round 6 snapshots:`robot/logs/stress-1zone-25k-mysqlpool-20260528-095929/prom-snapshots/`
  - t2m_baseline.txt
  - t7m_first-anomaly.txt
  - t20m_final.txt

---

## 7. 给后续 AI 的提醒

如果只看深挖文档 §1 "MaxOpenConn=10 是隐藏瓶颈" 就以为升到 30 万事大吉,会**漏掉 worker 串行才是真天花板**这条更深的发现。永远要**实测之后用 prometheus + Kafka lag 做闭环验证**,不能停在"看起来合理"的诊断上。
