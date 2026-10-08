# 压测复盘: ephemeral port 扩容前后对比 (2026-05)

**Date:** 2026-05-08
**场景:** 单 zone 15k/zone × 3 zone,机器人短连压测登录
**目标:** 定位 login 长尾(max 25s)的根因

---

## 一、对比表(实测)

| 指标 | 优化前 | 优化后 | 改善 |
|---|---|---|---|
| login_fail | 409-511 (~3%) | 0 | 100% |
| avg_login | 414-453ms | 77-79ms | 5.5x |
| max_login | 18-25s | 0.9-1.2s | ~20x |
| TCP SynSent 排队 | 1540 | 0 | 完全消除 |
| Bound (端口排队) | 15470 | 2704 | 5.7x 缓解 |

---

## 二、根因

### 优化前内核状态

```
Bound      = 15470  → connect() 已发但 ephemeral port 还没分到 / 排队中
SynSent    = 1540   → SYN 已发,等回包,堆积
TIME_WAIT  ≈ 几万   → 端口被占住 60s 不可复用
```

**症状解读:**
1. ephemeral port 默认范围 `32768 60999` 只有 ~28K 个端口
2. 短连接每次 close 进 TIME_WAIT 60s,持续高 QPS 时端口被吃光
3. 新 connect() 在内核里排队等端口 → 等不到 / 等很久 → 长尾 25s
4. `Bound + SynSent` 就是这种"端口耗尽 + connect 排队"的教科书签名

### 优化动作

```ini
net.ipv4.ip_local_port_range = 1024 65535   # 28K → 64K 个端口
net.ipv4.tcp_tw_reuse        = 1            # TIME_WAIT 端口可被新 connect 复用
net.ipv4.tcp_max_tw_buckets  = 1048576      # 桶上限拉大
```

效果立竿见影,见 §一。

---

## 三、判断"是被测机极限"还是"压测机自己极限"的方法

撞墙后,**先回答"端口耗尽出现在哪台机器"**:

```bash
ss -s | head
ss -tan state time-wait | awk '{print $4}' | sort | uniq -c | sort -rn | head
```

| TIME_WAIT 集中在 | 解释 |
|---|---|
| 压测机上几万 | 测的是**压测机的极限**,gate 没事 |
| gate 机器上,集中在 login IP | 是 **gate→login 的短连接**(见缺口 #13) |
| gate 机器上,集中在 gate 监听端口 | 是**客户端**短连进来(改客户端 keep-alive) |

本次压测的 `Bound=15470` 出现在哪台机器,需要在缺口 #13 核查时复测确认。

---

## 四、剩余可疑信号

优化后仍有 `Bound=2704`,说明**仍有 5.7K 量级的 connect 在排队**。可能性:

1. gate→login gRPC 仍未完全长连复用(任务 #13 核查)
2. 压测机仍在短连(检查机器人客户端 keep-alive)
3. 单 IP 对单后端的 64K 上限被另一种流量(如 DB / Redis)分掉

→ 在缺口 #13 排查清楚源头之前,不再盲目调内核参数。

---

## 五、下一阶梯压测计划

### 阶梯
```
45k → 60k → 80k → 100k → 120k
每档 5 分钟
```

### 每档要采集的指标
- QPS / avg / p99 / max(login)
- gate 上 `ss -s` 全状态
- gate CPU% 拆分(us / sy / si)
- gate `nstat | grep -i drop`
- login_server CPU + accept queue
- player_locker 锁等待时间(go-zero metric)
- Kafka 端到端延迟

### 拐点判定(三选一)
| 现象 | 结论 | 下一步 |
|---|---|---|
| Bound + SynSent 又起来 | 端口又撞 | 多源 IP / 多 gate 实例 |
| accept queue 溢出 + 延迟陡升 + CPU 没满 | gate 内部瓶颈 | profile 业务热点 |
| CPU 100% | 算力极限 | 横向扩 gate |

---

## 六、容量初判

按 78ms avg 反推:

- 单 ephemeral port 在 60s TIME_WAIT 下理论 1.3K req/s
- 64K 端口 × 多源 IP 3 个 ≈ 250K req/s 理论上限
- 业务真实瓶颈大概率早于此,要看下一阶梯压测拐点

---

## 关联文档
- [docs/ops/gate-kernel-tuning-runbook.md](../ops/gate-kernel-tuning-runbook.md) sysctl 完整配置 + 故障 SOP
- [docs/design/ARCH.md](../design/ARCH.md) §9 调优总览
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](../design/architecture-current-state-vs-gaps-2026-05.md) 缺口 #13 / #2
- [docs/stress/stress-test-progress.md](./stress-test-progress.md) 历史压测记录
