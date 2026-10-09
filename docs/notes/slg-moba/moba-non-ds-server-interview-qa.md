# MOBA 服务器面试 Q&A — 非 DS（大厅 / 匹配 / 房间 / 战绩 / 段位 / 社交）

**Created:** 2026-05-18

> 范围：MOBA 战斗服（DS）**之外**的所有后端服务——登录、网关、匹配、组队、房间、BP、战绩、段位、好友、聊天、商城、战令、活动等"常驻型"服务。
> 战斗服内部见 [moba-ds-server-interview-qa.md](moba-ds-server-interview-qa.md)。

---

## 一、整体架构

### Q1: MOBA 非战斗后端典型拓扑长什么样？

**A:**

```
                 ┌──────────────────┐
                 │  CDN / OSS       │ 版本资源 / 公告 / 战报包
                 └──────────────────┘
                          ▲
                          │
   ┌──────────┐  HTTPS    ┌──────────────────┐
   │  Client  │ ────────► │  HTTP Portal     │ 登录、服务器列表、商城下单回调
   └────┬─────┘           │  (Java/Go)       │
        │ TCP/UDP         └────────┬─────────┘
        │ 长连接                   │
        ▼                          ▼
   ┌──────────────┐         ┌──────────────┐
   │  Gate (长连)  │ ──────► │  Auth/Login  │ 鉴权、发 token
   └──────┬───────┘         └──────────────┘
          │
          │ 业务路由（gRPC / 消息总线）
          ▼
   ┌────────────────────────────────────────────────────────────┐
   │  业务微服务集群                                              │
   │                                                            │
   │  PlayerProfile  Friend  Chat  Guild  Mail  Inventory       │
   │  Matchmaker  Room  BanPick  Hero/Skin  Shop  BattlePass    │
   │  Stats(战绩)  Rank(段位)  Replay  Reward  Activity         │
   │                                                            │
   │  ↑↓ etcd（服务发现） ↑↓ Redis（缓存/锁） ↑↓ Kafka（事件）  │
   │                                                            │
   │  ↓ 持久化                                                  │
   │  MySQL/TiDB（账号、玩家、战绩）                            │
   │  Redis Cluster（在线、好友状态、匹配池）                   │
   │  ClickHouse / Doris（行为日志、对战分析）                  │
   │  OSS / S3（录像）                                          │
   └────────────────────────────────────────────────────────────┘
                          │
                          ▼
                   ┌──────────────┐
                   │ DS Allocator │ → 战斗 DS（短生命周期）
                   └──────────────┘
```

**关键边界：**
- **HTTP Portal**：无状态，挂 L7 LB（Nginx / Ingress），处理短 API
- **Gate**：有状态（持有玩家长连接），**不挂 L4 LB**，由客户端按 assign-gate 结果直连
- **DS**：短生命周期，由 Allocator 拉起，客户端直连

我们项目同样套路（Java Gateway + cpp gate + go-zero 业务微服务），见 [ARCH.md](../../design/ARCH.md)。

---

### Q2: 为什么不把 Matchmaker / Room 也做成长连接服务？

**A:**

- **匹配是分钟级、低频、可重试**的过程，每个玩家一分钟可能产生 1–2 个 RPC（开始匹配、取消、确认接受）。**走长连接的开销远大于走 gRPC 转发**。
- 长连接服务必须有状态（持玩家连接），扩缩容麻烦；**匹配服无状态可水平扩**，弹性好。
- Gate 是长连接的天然入口，已经持有玩家会话，匹配 RPC 走 Gate→Matchmaker 反而最干净。

**架构原则**：**长连接尽量收口在 Gate，业务服务全做无状态 gRPC**。我们项目 cpp gate + go-zero 微服务就是这个思路。

---

## 二、匹配系统（Matchmaker）

### Q3: MOBA 匹配怎么做？讲下核心算法。

**A:** 经典两段式：**ELO/MMR 筛选 + 多约束打分**。

**核心数据：**
```
Player {
  id, region, mmr, position_pref[], recent_dodge_count,
  party_id (组队), party_avg_mmr, ping_to_zones[]
}
```

**匹配池：** Redis Sorted Set，按 `(region, mode, mmr)` 分桶；玩家每秒插入并扩散 MMR 容忍区间。

**匹配循环（一秒一轮）：**
```
for bucket in buckets:
    candidates = pool.range(mmr - tolerance, mmr + tolerance)
    while len(candidates) >= 10:
        ten = greedy_pick_10(candidates, constraints={
            position_balanced: 双方都要凑齐 5 个位置,
            mmr_diff <= 200,
            party_split_balanced: 组队拆分到两边,
            ping_diff <= 50ms,
        })
        if ten: create_match(ten)
```

**MMR 容忍随等待时间扩大：**
```
tolerance(t) = base + step * floor(t / 10s)
等 60s 还匹不到 → ±400 MMR，1 分钟后再不行换更远机房
```

**加分点：**
- 国内王者 / LOL 都用**位置匹配**（5 路位置必须齐），未指定位置的扣分但允许
- 组队（2/3/5 黑）匹配时降权，对面也凑组队，避免单排被五排打
- 防"匹配卡死"：开局后短期内放宽容忍，避免凌晨人少时全员转圈

---

### Q4: 匹配池放 Redis 还是放内存？数据量多大？

**A:**

| 方案 | 优点 | 缺点 |
|------|------|------|
| 单进程内存 | 极快、无网络 | 单点、扩不动 |
| Redis 共享 | 多 Matchmaker 实例横向扩 | 网络往返、Lua 脚本写复杂 |
| **本地内存 + Redis 持久化（最优）** | 本地匹配快、Redis 做容灾恢复 | 状态同步要分桶不重叠 |

**数据量**：100 万 DAU 同时在线峰值 ~10 万人，匹配中 ~5–10%，即 5000–10000 玩家在池。每个玩家几百字节，内存 < 10MB，**单进程吃得下**。

**多实例切分**：按 `(region, mode)` 分片，一个 Matchmaker 实例负责若干分片。同一分片只能一个实例处理（用 etcd / Redis 分布式锁选主）。

---

### Q5: 玩家秒接受秒拒绝（dodge），怎么惩罚？

**A:**

接受流程：匹配成功 → **10 秒接受窗口** → 全员点接受 → 进 BP/选英雄 → 全员选完 → 拉 DS。

dodge 定义：
1. 接受窗口超时未点 → 软 dodge
2. 接受了但 BP/选英雄时关游戏 → 硬 dodge

**惩罚：**
- 第 1 次：5 分钟禁匹 + 扣少量积分
- 第 2 次/天：30 分钟禁匹
- 第 3 次/天：3 小时 + 扣大量荣誉积分
- 凌晨重置 dodge 计数

**实现**：Redis `dodge:{playerId}:{date}` 计数 + `banned_until:{playerId}` 时间戳，匹配开始前检查。

**对其他玩家的补偿**：剩余的 9 人重新放回池子最高优先级 + 一次性匹配优先卡。

---

## 三、房间 / BP / 选英雄

### Q6: BP（Ban/Pick）服务器怎么设计？为什么不直接放 DS？

**A:**

**不放 DS 的原因：**
1. **生命周期不同**：BP 30–60 秒，DS 30 分钟，提前拉 DS 浪费资源
2. **状态简单**：BP 只是状态机，不需要游戏引擎
3. **可以分离扩容**：BP 高峰是凌晨开赛季、新版本上线，DS 高峰是周末晚上

**实现**：一个 BP 房 = 一个状态机进程 / 协程实例。
```
BPRoom {
  matchId, mode (solo/duo/competitive),
  players[10], teamA[5], teamB[5],
  phase: WAIT_ACCEPT → BAN_PHASE_1 → PICK_PHASE_1 → BAN_PHASE_2 → PICK_PHASE_2 → CONFIRM
  timer per phase, 超时自动跳过 (Ban) 或随机选 (Pick)
  banPool: [], pickPool: {}
  fsm 转换由玩家操作 + 超时定时器驱动
}
```

存 Redis（短暂、可重建）即可，**不需要落库**。完成后把 `{matchId, players, heroes, items}` 推给 DS Allocator。

---

### Q7: 玩家在 BP 阶段断线/中途离开怎么办？

**A:**

| 场景 | 处理 |
|------|------|
| 网络抖动（< 10s 重连） | 房间状态广播 resend，对方看见 "[reconnecting]" |
| 长时间断线 (> 10s) | 该玩家所有未操作的 ban/pick **由系统按预设/随机执行**，不能拖死全房 |
| 主动退出 | 算 dodge，扣分 + 解散房间，9 人重新进队 |
| 整队 5 个全断 | 房间销毁，全员重排队 |

**关键**：BP 是同步的（5 个 ban / 5 个 pick），一个人卡所有人都得等。**超时强制操作 + 系统兜底**比"等"更好。

---

## 四、玩家数据 / 战绩 / 段位

### Q8: 玩家档案怎么存？热数据 vs 冷数据怎么分？

**A:**

**热数据**（每局都用、几乎每秒都读）：
- 当前在线状态、Gate 位置、好友在线状态、邮件未读数
- 段位、MMR、当前段位胜负场
- **存 Redis**，玩家上线时 warmup，下线 TTL 自动失效

**温数据**（每局结算时写，登录时读）：
- 玩家基础档案（昵称、头像、等级、经验、金币）
- 拥有的英雄/皮肤/物品列表
- 战令进度、活动进度
- **存 MySQL/TiDB，Redis 做缓存**（write-behind 见我们项目 [db_write_behind_dirty_flag_race.md](../../design/db_write_behind_dirty_flag_race.md)）

**冷数据**（战绩、行为日志、分析）：
- 历史对局详情（每局一条，按月分表）
- 行为日志（每个操作）
- **存 ClickHouse / Doris / TiDB 列存**，按时间分区，归档到 OSS

**存储原则**：玩家档案核心字段（金币、物品）走**事务性 KV / SQL**；统计/分析走**列存/OLAP**。混用一定踩坑。

---

### Q9: 段位系统用 ELO 还是 Glicko-2？

**A:**

| 算法 | 优点 | 缺点 |
|------|------|------|
| ELO | 简单、易实现，国际象棋经典 | 不考虑活跃度，长期不玩回归后估值不准 |
| Glicko-2 | 加入"评分偏差 RD"和"波动率"，更准 | 实现复杂，参数难调 |
| **TrueSkill** | 团队场景设计，PS Halo 用过 | 数学复杂，对玩家不透明 |

**MOBA 主流**：
- 大众段位用**改良 ELO**：加入位置、连胜/连败修正、新号保护
- 顶级段位（王者、最强王者）用更精细的 MMR 隐藏分

**核心公式（ELO 简化）：**
```
expected_win_rate(A vs B) = 1 / (1 + 10^((B.mmr - A.mmr) / 400))
delta_mmr_A = K * (actual_result - expected_win_rate)  // K = 16~32
```

**MOBA 特化：**
- MVP / 关键贡献奖励额外 +5 MMR
- 队友送人头不全责（K 值按个人表现微调）
- 连胜超过 5 场加额外 MMR（防卡分）

**段位段位段位**：青铜/白银/...只是 MMR 区间的标签，内部判定一律 MMR。

---

### Q10: 战绩查询怎么扛住高并发？

**A:**

**典型读模式：**
1. 自己最近 20 场 → 高频，必须 < 100ms
2. 好友最近 20 场 → 中频
3. 任意 ID 最近 N 场 → 低频（OB / 大主播）

**架构：**
```
对局结束 →结算服写 MySQL（主表：match_id 主键）
        → 同时写 player_match_index（player_id, match_id, ts）按 player_id 分片
        → 异步同步到 ClickHouse 做分析
玩家请求战绩 → 查 player_match_index（按 player_id 分片，命中 Redis 缓存）
            → 拿 match_id 列表 → 并发查 match 主表
            → 详情合并返回
```

**优化：**
- `player_match_index` 按 `player_id` 哈希分片，单 player 的战绩全在一个分片
- Redis 缓存"我最近 20 场"，结算后失效一次
- match 详情 hot 期（结算后 1 小时）也缓存 Redis
- 历史战绩冷数据归档到 OSS，按需 lazy load

**容量估算**：1000 万 DAU × 5 局/天 = 5000 万局/天 = ~600 局/秒，单 MySQL 主写无压力。冷数据三个月归档。

---

## 五、社交 / 聊天 / 公会

### Q11: 全局聊天怎么扛万人频道？

**A:** **分级 + 分片 + 限流**。

**频道分级：**
1. **私聊** — 一对一，点对点发，落库
2. **小队/公会** — 几十人，发送时拉取在线名单广播
3. **世界/区域** — 万人级，**绝对不能每条消息广播给所有人**

**世界频道做法：**
```
Producer → Kafka topic "world_chat"
Consumer (每个 Gate 一个) 订阅 → 本地缓存最近 200 条
玩家请求世界频道 → Gate 返回本地缓存（不持久全量历史）
玩家发言 → 走限流（每分钟 5 条）→ 过敏感词 → 投递 Kafka
```

**限流：**
- 单玩家：每分钟 N 条
- 全局：QPS 上限，触发自动降级（只显示 VIP 消息）
- 黑名单：违规用户禁言

**敏感词：** AC 自动机或 Aho-Corasick + 持续更新词库，必经过滤。

我们项目有类似 [world-channel-system.md](../../design/world-channel-system.md)、[chat-sensitive-word-filter.md](../../design/chat-sensitive-word-filter.md)。

---

### Q12: 好友系统怎么存？在线状态怎么实时通知？

**A:**

**好友关系：** MySQL `friendship(user_a, user_b, status, created_at)`，双向写两条避免查询拼接。

**热路径**：
- 玩家上线 → 拉好友 ID 列表（Redis 缓存）→ 查每个好友的 `online_status:{id}` Redis key
- 给所有"我在线"的好友推送"你的好友 X 上线了"

**实时通知做法：**
```
玩家上线 → Login 写 Redis "online:{playerId}" + 推 Kafka "presence" event
反向索引：Redis Hash "friends_of:{playerId}" 存"谁把我加了好友"
Presence Consumer 收到 X 上线 →
    SMEMBERS friends_of:X →
    对每个好友 F → 找 F 的 Gate（player_locator）→ 推送
```

**优化：** 单玩家好友通常 < 100 人，全量推送可承受。万人粉丝场景（主播）走单独"订阅"模型，不走好友通知。

我们项目有 [friend-persistence-architecture.md](../../design/friend-persistence-architecture.md) 和 [guild_friend_service_notes.md](../../design/guild_friend_service_notes.md) 类似实现。

---

### Q13: 公会战 / 战队赛排行榜怎么实时？

**A:**

**排行榜实时性分级：**

| 榜单 | 更新频率 | 实现 |
|------|---------|------|
| 个人天梯 | 实时（每局结算后） | Redis ZSET，ZADD 写入 |
| 周榜 / 月榜 | 每小时刷新 | Redis ZSET，定期重建 |
| 全服排名 | 实时但只查 top 1000 | Redis ZSET，ZREVRANGE |
| 战队/公会积分 | 战斗结束推 Kafka | 异步聚合到 ZSET |

**百万级排行的痛：**
- ZSET 单 key 超 10 万元素时 `ZADD/ZRANGE` 性能下降
- 解决：**分片排行榜**，按 `mmr / 1000` 分桶，每桶一个 ZSET，查询时合并

我们项目有 [guild_ranking_architecture.md](../../design/guild_ranking_architecture.md) 类似设计。

---

## 六、订单 / 商城 / 战令

### Q14: 商城购买和支付回调怎么保证不掉单 / 不重发？

**A:** **本地订单表 + 支付幂等 + 异步发货补偿**。

**正路：**
```
1. 客户端发起购买 → Order Service 创建 order_id, status=PENDING，落库
2. 调三方支付 → 返回支付 URL
3. 客户端跳转支付 → 三方回调 Order Service
4. Order Service 校验签名 → status=PAID → 投递 Kafka "delivery" event
5. Delivery Consumer 收到 → 给玩家发货（更新背包）→ status=DELIVERED
6. 客户端轮询 / 推送：发货完成
```

**关键：**
- 三方回调必须**幂等**（同一 order_id 重复回调返回相同结果）
- 支付状态查询定时任务：扫 PENDING > 30 分钟的订单，主动调三方查实际状态
- 发货失败重试：Kafka 消费失败回退，定时扫 PAID 但未 DELIVERED 的订单
- **绝对不在支付回调里直接发货**（回调超时 → 三方会重试 → 重复发货）

**对账：** 每日凌晨拉三方流水 vs 本地订单表对账，差异告警人工处理。

---

### Q15: 战令 / 活动进度怎么设计？玩家"差 1 分到 100 级"怎么不出错？

**A:**

**核心：经验值是单调累加的纯整数，等级是经验的纯函数。**

```
struct BattlePassProgress {
  season_id: int
  total_exp: uint64    // 总经验，只增不减
  // current_level / current_progress 都是计算出来的，不存
}

level_of(exp) = upper_bound(exp_table, exp) - 1   // 二分查表
```

**好处：**
- 不存等级，避免"经验和等级对不上"
- 等级表（exp_table）可以热更，玩家经验不变，等级会重新计算
- 多端读不会出现并发条件

**加经验的事务：**
```
old_level = level_of(progress.total_exp)
new_exp = progress.total_exp + delta
new_level = level_of(new_exp)
update progress set total_exp = new_exp
for L in (old_level + 1, new_level]:
    grant_reward(L)  // 幂等：用 (player, season, level) 联合主键去重
```

**幂等的奖励发放**关键：用 `reward_claimed bitset`（参考我们项目奖励系统的设计）。

---

## 七、登录 / 鉴权 / Gate 分配

### Q16: 客户端登录 → 进入大厅，链路是？

**A:**（我们项目就是这么做的，见 [player_login_flow.md](../../design/player_login_flow.md)）

```
1. Client → HTTP /server-list → Portal 返回大区列表
2. Client → HTTP /assign-gate(zoneId) → Portal 调 gRPC AssignGate(go-zero login)
                                       → 返回 {gate_ip, port, HMAC token}
3. Client → TCP/UDP 连 Gate → ClientTokenVerify(token)
4. Gate 本地 HMAC 验签（不查 Redis）→ 通过 → 玩家进入大厅
5. 大厅 RPC（拉取档案、好友、邮件、战令）→ Gate 转发到对应业务服
```

**Token 三层正交：**
- 第三方鉴权（微信/QQ/账密）— 业务层验证一次
- Access Token（2h）— go-zero login 颁发，刷新 Refresh Token
- Gate Token（300s HMAC）— AssignGate 颁发，cpp gate 本地验

详见 [dual-token-authentication.md](../../design/dual-token-authentication.md)。

---

### Q17: 为什么 Gate 不能挂 LVS / Nginx 做负载均衡？

**A:** **Gate 是有状态的**（持有玩家长连接 + 会话路由），LB 后向均衡会破坏几个核心假设：

1. **会话亲和性**：玩家断线重连可能被 LB 路由到不同 Gate，但之前的 Gate 还持有他的 SessionInfo，导致**双连**或**状态不一致**
2. **Kafka 路由**：业务服推送给玩家的消息走 `gate-{gateId}` Kafka topic，LB 让客户端连到的 Gate 和实际订阅 topic 的 Gate 对不上
3. **延迟敏感**：MOBA 操作 RTT < 100ms，LB 一跳额外 5–20ms 是浪费
4. **TCP 半开问题**：LB 检测不到的客户端死连接堆积在 Gate

**正确做法**：客户端拿到 `assign-gate` 返回的 `gate_ip:port`，**直连**到那个 Gate。负载均衡逻辑在 AssignGate 服务里做（选最空闲的 Gate）。

详见 [gate-load-balancing-design.md](../../design/gate-load-balancing-design.md)。

---

## 八、扩缩容 / 灾备 / 灰度

### Q18: 周五晚高峰流量是凌晨的 10 倍，怎么扩容？

**A:** **分层弹性**：

| 层 | 扩容方式 | 触发指标 |
|----|---------|---------|
| HTTP Portal | K8s HPA，CPU/QPS | CPU > 70% 或 QPS 阈值 |
| Gate | 预先准备机器池，按需启动；新玩家路由到新 Gate | 在线人数接近上限 |
| 业务微服务（Friend / Stats / 等） | K8s HPA，CPU | 同上 |
| Matchmaker | 按分片扩，新分片接管 (region, mode) 子集 | 匹配等待时间上升 |
| DS Pool | Agones / 自研 Operator 维持 N 个 Ready DS | 池水位 < 阈值 |
| MySQL/Redis | 提前按规模规划，平时打 70%，**不指望临时扩** | 容量规划 |

**不能临时扩的：**
- 有状态存储（MySQL 分库分表、Redis Cluster）
- 已开局的 DS 进程
- Gate 已持有的连接（只能引流到新 Gate）

**开服 / 大版本上线**还需要：**登录排队系统**（我们项目用 Bucket4j + AssignGate 真排队，见 [login-queue-2026-05.md](../../design/login-queue-2026-05.md)）。

---

### Q19: 一个大区的 MySQL 主库挂了，怎么办？

**A:**

**预案分级：**

| 程度 | 处理 |
|------|------|
| 短暂抖动（< 30s） | 业务层重试 + write-behind 缓冲，玩家无感 |
| 主库宕机 | MHA / Orchestrator / 自研切换：从库提升为主，DNS / VIP 切换 |
| 整机房断网 | 跨机房热备从库提升为主，玩家会断 1–5 分钟 |
| 数据损坏 | 停服回滚到最近一致备份 + binlog 重放到事件点 |

**关键设计：**
- 玩家档案 write-behind：DB 短暂不可用时 Redis 顶住，Dirty 标记保证恢复后回写（参考 [db_write_behind_dirty_flag_race.md](../../design/db_write_behind_dirty_flag_race.md)）
- 关键操作（充值、商城）必须**强一致**：DB 不可用就拒绝下单，不能让玩家"扣钱了但没东西"
- 跨大区彻底隔离：A 区 DB 挂不影响 B 区（我们项目 [db_zone_isolation.md](../../design/db_zone_isolation.md)）

---

### Q20: 灰度发版怎么不影响在线玩家？

**A:**

**两个维度：**

1. **业务微服务（无状态）**：K8s 滚动更新 + 健康检查，gRPC client 自动重试，几乎无感
2. **Gate（有状态）**：
   - 标记节点为 "draining"，**不再接收新连接**
   - 老连接保留处理，自然消亡（玩家退出 / 一局结束）
   - 等待几分钟到几小时 → 关停
3. **DS**：天然一局一进程，新版本只影响**新开的局**，已开的局跑完为止
4. **数据库**：迁移先走兼容字段（先加字段、再读写、最后删旧），**严禁直接改表结构**

**灰度策略：**
- 按比例：新版本接 10% → 20% → 50% → 100%
- 按用户：内部账号 → 测试服 → 灰度 100 → 灰度 1k → 全量
- 按大区：先小区试水，确认无回归再大区

**回滚预案**：每次发版**必须**有 5 分钟内回滚的能力，发版前演练。

---

## 九、可观测 / 反外挂

### Q21: 监控告警怎么设？哪些指标最重要？

**A:** **金字塔分级，告警别淹没人。**

**P0（5 分钟内响应）：**
- 大区登录失败率 > 5%（持续 1 分钟）
- 匹配排队时间 P95 > 5 分钟
- 支付回调失败率 > 1%
- Gate 在线人数突然下降 30%
- DS 池 Ready 数 < 阈值
- 任何业务服务 5xx > 1%

**P1（1 小时响应）：**
- 慢 RPC P99 > 500ms
- Redis / MySQL 慢查 > 100ms 持续
- Kafka lag > 10000
- 单玩家行为异常（潜在外挂）

**P2（日报）：**
- 资源水位、容量预测、错误日志聚合

**业务指标 vs 系统指标**：DAU、PCU、人均对局数、平均匹配时间、流失率这些**业务指标**比 CPU/内存重要得多——CPU 100% 不一定影响玩家，匹配排队 10 分钟一定爆炸。

---

### Q22: 业务层反作弊怎么做？（非战斗内的）

**A:**

**典型作弊场景与对策：**

| 场景 | 检测 | 处罚 |
|------|------|------|
| 脚本刷战令 / 活动 | 行为模式（每天精确同时间、操作完全规律） | 进度回退 + 警告 → 封号 |
| 卖小号 | 多账号同 IP / 同设备指纹 + MMR 异常上升 | 限制段位上分 |
| 工作室刷分 | 大量低级账号集中互送人头 | 整批封 + 关联设备封 |
| 演员（故意输） | 单方面表现极差（人头/伤害/输出曲线异常） | 系统检测 + 玩家举报双重 |
| 充值欺诈（盗刷、退款撸物） | 风控规则 + 三方风控 API | 冻结账号 + 追回物品 |

**核心基础设施：**
- **设备指纹**（IDFV / 自研 SDK 上报多维特征）
- **行为日志全链路**（每个操作打到 Kafka → ClickHouse）
- **风控规则引擎**（规则 + ML 模型双轨）
- **客户端代码加固**（VMP / 自研壳）

**封号策略**：阶梯式（警告 → 1天 → 7天 → 30天 → 永久）；金币/物品**冻结**而非删除（避免误判）。

---

## 十、特殊话题

### Q23: 自走棋的非战斗服需要什么特殊设计？

**A:** 和 MOBA 大体一样，差异在：

1. **匹配** 8 人对局，匹配池更宽松（MMR 容忍更大）
2. **赛季奖励** 更频繁（每周小赛季 + 月赛季 + 季赛季），奖励发放系统压力大
3. **羁绊 / 棋子表** 数据驱动且热更频繁，配表系统必须支持热加载 + 灰度
4. **战绩** 8 个人的排名而非胜负，结算复杂度高
5. **观战 / 训练模式** 需要单独的房间服，能让 1 个观众看 8 个棋盘

---

### Q24: MOBA 服务器和 FPS 服务器（如 Valorant、Apex）后端有什么主要差异？

**A:**

| 维度 | MOBA | FPS |
|------|------|-----|
| 战斗服模型 | 帧同步 30Hz | 状态同步 60–128Hz |
| 客户端预测 | 弱（指令级） | 强（位置/瞄准） |
| 反作弊重点 | 地图透视（FOW） | 自动瞄准、穿墙 |
| DS 资源 | 0.1 核 / 局 | 1–2 核 / 局 |
| 一局玩家数 | 10 | 10–60（吃鸡 100） |
| 匹配复杂度 | 位置 + MMR | MMR + 武器熟练度 |
| 段位频率 | 每局结算 | 每局结算 |
| 录像 | 指令流 KB 级 | 状态流 MB–GB 级（FPS 录像贵） |

**非战斗外围服务（匹配 / 战绩 / 段位 / 社交 / 商城）几乎完全相同**，可以复用同一套微服务架构。

---

### Q25: 假设你要从零搭一套 MOBA 后端，先做哪 5 件事？

**A:** 按依赖顺序：

1. **登录 / Gate / 鉴权** — 没这套客户端连不进来，所有联调都做不了
2. **玩家档案 + 持久化（MySQL + Redis 缓存）** — 数据基础
3. **匹配 + BP + DS Allocator** — 能让玩家进局
4. **DS 战斗服 + 结算回调** — 闭环
5. **战绩 + 段位 + 排行榜** — 留存关键

**先不做的：** 公会、聊天频道、商城、战令、活动 —— 都是有了核心循环后的运营层，可以后续加。

**踩坑预防：** 一开始就把**确定性战斗、幂等结算、write-behind、Kafka 分区、跨区隔离**这几个核心原则定死，后期改架构成本高 10 倍。
