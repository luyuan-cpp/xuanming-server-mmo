# SLG Server Architecture Design (率土之滨 Style)

**Created:** 2026-03-30

## 1. 寻路 (Pathfinding)

**地图模型**：格子地图（hex 或 square），不是 NavMesh。

**算法选择**：
- **A\*** 足够。格子数量级 ~10^6，A* 在有良好启发函数时性能完全够用
- 对于长距离寻路，使用 **JPS（Jump Point Search）** 可以在方格图上获得 10-30x 加速
- 如果地图有大面积可通行区域，可预计算 **HPA\*（Hierarchical Pathfinding）**：将地图分为 chunk，先做粗粒度跨 chunk 寻路，再做 chunk 内精细寻路

**服务端实现要点**：
- 寻路在服务端完成（防作弊），客户端只做预测展示
- 地图障碍物变化（建城、拆除）时，只需更新受影响 chunk 的局部连通图
- 路径缓存：相同起终点 + 相同地图版本号 → 命中缓存直接返回
- 行军路径一旦计算完成，存储为 `Vec<TileId>` 序列，服务端按 tick 推进位置

```
March {
    path: [tile_0, tile_1, ..., tile_n],
    current_index: 3,
    speed: tiles_per_second,
    start_time: timestamp,
    // 当前位置 = path[floor((now - start_time) * speed)]
}
```

---

## 2. 战斗 (Combat)

率土之滨的战斗是 **回合制自动战斗**，不同于即时战斗 MMORPG。

**架构**：
- 战斗发生在两支（或多支）部队到达同一格子时触发
- 战斗逻辑是**确定性模拟**（Deterministic Simulation）：相同输入 → 相同结果
- 服务端运行完整战斗模拟，生成**战报（BattleReport）**发给客户端回放

**实现要点**：
- 战斗系统是纯函数：`BattleReport = SimulateBattle(attacker_army, defender_army, random_seed)`
- 不需要实时同步，客户端拿到战报后本地播放动画
- 使用固定 seed 的伪随机数，保证战报可重放验证（防外挂/争议回溯）
- 多队集结攻城时，按到达时间排队，依次或合并处理

**性能**：
- 单场战斗通常 < 1ms（回合数有限，单位数有限）
- 核心瓶颈不在战斗计算，而在并发触发量。用战斗队列 + worker pool 处理突发峰值

---

## 3. 视野 (Vision / Fog of War)

这是 SLG 大地图最核心的系统之一。

**需求**：玩家只能看到自己城池/部队周围一定范围内的信息。

**方案：格子级 interest 计数器**

```
每个格子维护: observer_count map<player_id, int>

当玩家建城/行军经过格子时:
  对视野范围内所有格子: observer_count[player_id]++
当离开时:
  observer_count[player_id]--
  
player 能看到格子 ⟺ observer_count[player_id] > 0
```

**优化**：
- 视野范围通常是固定半径（如 3-5 格），用**预计算的偏移表**（offset table）批量更新
- 行军时，只需做**增量更新**：新进入的前沿格子 +1，离开的尾部格子 -1（类似滑动窗口）
- 不要每 tick 全量重算视野

**大地图分区**：
- 将地图分为 N×N 个 **Region**（如 32×32 格为一个 Region）
- 每个 Region 由一个线程/协程负责，减少锁竞争
- 跨 Region 行军时做 Region 间 handoff

**推送策略**：
- 不要广播全地图变化给所有人
- 每个格子状态变化时，只通知 `observer_count > 0` 的玩家
- 玩家打开大地图滚动时，按视口（viewport）请求格子数据，服务端做**权限过滤**后返回

---

## 4. 行军同步 (March Synchronization)

**核心模型：时间驱动 + 事件驱动混合**

行军不需要每帧 tick，而是基于时间计算的**惰性求值**：

```
current_tile = path[min(floor((now - t_start) * speed), len(path) - 1)]
```

**服务端实现**：
- 行军创建时，计算完整路径和预计到达时间
- 注册一个**定时器事件**：到达目的地时触发（触发战斗/驻扎/采集等）
- 中途不需要 tick 推进位置——任何时刻都能从公式算出当前位置
- 路径上每经过一个格子，注册**视野进入/离开事件**（可预计算所有时间点）

**中途拦截/遭遇 — 时空碰撞检测**：

"时空碰撞"指两支部队在**同一时刻**经过**同一格子**。"同一格子"是空间碰撞，"同一时刻"是时间碰撞，两个条件同时满足才算遭遇。

每个格子维护时间占用表：

```
Tile(x,y).occupants = [{army_id, enter_time, leave_time}, ...]
```

新行军注册时，遍历路径上每个格子，检查时间段重叠：

```
重叠条件: new_enter < existing_leave && existing_enter < new_leave
```

具体示例：
```
时间轴:     t=0    t=1    t=2    t=3    t=4
部队A路径:  [A0] → [A1] → [A2] → [A3] → [A4]
部队B路径:              [B0] → [B1] → [B2] → [B3]

假设 A2 和 B1 是同一个格子:
  部队A在 t=2 到达, t=3 离开 → [2, 3)
  部队B在 t=2 到达, t=3 离开 → [2, 3)  
  2 < 3 && 2 < 3 → 重叠！在 t=2 触发战斗

如果 B 晚出发 1 秒:
  部队A [2, 3), 部队B [3, 4)
  2 < 4 但 3 < 3 为 false → 没碰上，不触发
```

实现代码思路：
```cpp
for (int i = 0; i < path.size(); i++) {
    double enter_t = start_time + i / speed;
    double leave_t = start_time + (i + 1) / speed;
    Tile& tile = map[path[i]];
    for (auto& existing : tile.occupants) {
        if (IsEnemy(existing.army_id, new_army_id) &&
            enter_t < existing.leave_time && existing.enter_time < leave_t) {
            ScheduleBattle(max(enter_t, existing.enter_time), ...);
            return;
        }
    }
    tile.occupants.push_back({new_army_id, enter_t, leave_t});
}
```

**注册时一次性检测完毕，O(path_length)，零逐帧开销。** 召回/变速时清除旧占用、重新注册。

**客户端同步**：
- 客户端收到行军信息：`{path, start_time, speed}`
- 客户端本地插值动画，不需要服务端每帧推送位置
- 加速/召回等操作：服务端重新计算路径和时间，推送更新事件

---

## 5. 性能优化 (Performance)

| 层面 | 策略 |
|------|------|
| **地图分区** | 将大地图切为 Region，每个 Region 独立处理，减少全局锁 |
| **惰性计算** | 行军位置按需计算，不做每帧 tick |
| **增量视野** | 滑动窗口式更新，不要全量重算 |
| **战斗异步化** | 战斗放入队列由 worker pool 处理，不阻塞主逻辑 |
| **AOI 优化** | 玩家只订阅视口内 Region 的变更事件，减少推送量 |
| **数据冷热分离** | 活跃行军/在线玩家数据在内存；离线玩家/历史战报落库 |
| **时间轮定时器** | 行军到达、建筑完成等大量定时事件，用时间轮（Hashed Timing Wheel）而非优先队列 |
| **协议优化** | 大地图格子数据用差量（delta）推送，不要全量 snapshot |

---

## 6. 数量级参考（单服）

- 同时在线 ~5,000–10,000 人
- 同时行军 ~10,000–50,000 支部队
- 地图格子 ~1.5 × 10^6
- 每格子内存 ~64–128 bytes → 地图总内存 ~100–200 MB，完全放得进内存

---

## 7. 与当前 MMORPG 项目的关系

| 差异点 | MMORPG (当前项目) | SLG (率土之滨) |
|--------|-------------------|----------------|
| AOI | 连续坐标九宫格/十字链表 | 格子级 observer 计数 |
| Scene | 多个小 instance | 一张超大共享地图，Region 分片 |
| 同步模型 | 帧同步/状态同步 | 事件驱动 + 惰性位置计算 |
| 战斗 | 实时技能/伤害同步 | 确定性回合模拟 + 战报回放 |
| Kafka 消息 | 可复用 | 行军事件、战斗结算、跨 Region 通知 |

### 7a. MMORPG AOI vs SLG AOI 深度对比

**MMORPG AOI** 回答的是"**谁在我旁边**"（以实体为中心）：

```
九宫格方案:
┌─────┬─────┬─────┐
│     │     │     │   地图切成大区块（如 100x100）
├─────┼─────┼─────┤   玩家 P 在中间格
│     │ [P] │     │   P 能看到周围 9 格内所有实体
├─────┼─────┼─────┤
│     │     │     │
└─────┴─────┴─────┘

十字链表方案:
X轴链表: ──A──B──P──C──D──  (按X坐标排序)
Y轴链表: ──E──P──F──G──     (按Y坐标排序)
P视野半径=50: 扫描链表中距离<50的节点，取交集
```

**SLG AOI** 回答的是"**我能不能看到那个格子**"（以格子为中心）：

```
玩家在 (5,5) 建城，视野半径=2:
      3   4   5   6   7
  3 [ 1 ][ 1 ][ 1 ][ 1 ][ 1 ]    每个格子有一个计数器
  4 [ 1 ][ 1 ][ 1 ][ 1 ][ 1 ]    observer_count[player_A] = 1
  5 [ 1 ][ 1 ][城][ 1 ][ 1 ]    表示"player_A 能看到这个格子"
  6 [ 1 ][ 1 ][ 1 ][ 1 ][ 1 ]
  7 [ 1 ][ 1 ][ 1 ][ 1 ][ 1 ]

行军时滑动窗口更新:
  t=0: 部队在(10,10) → (9,9)~(11,11) 这9格 count++
  t=1: 部队到(11,10) → 新前沿(12,9)(12,10)(12,11) count++
                         旧尾部(9,9)(9,10)(9,11) count--
  只改变了6个格子，不是全量重算
```

| | MMORPG AOI | SLG AOI |
|---|---|---|
| 核心问题 | "谁在我旁边" | "我能不能看到那里" |
| 坐标系 | 连续浮点 (123.45, 678.90) | 离散整数 (32, 58) |
| 数据结构 | 九宫格哈希 / 十字链表 | 每格子 observer 计数器 |
| 更新频率 | 高频（每秒十几次） | 低频（行军每秒 1 格） |
| 推送触发 | 实体变化 → 推给附近玩家 | 格子变化 → 推给 count>0 的玩家 |

**问题模型完全不同，不能复用，需要重写。**

---

## 8. 战斗回放系统 (Battle Replay)

### 8.1 核心原则

确定性模拟 + 事件日志序列化。服务端运行完整战斗，产出 BattleReport（行动序列），客户端从日志回放动画。

### 8.2 确定性引擎约束

- 纯函数：`BattleReport = SimulateBattle(attacker, defender, seed, terrain)`
- 禁止浮点数 — 用定点数或整数百分比（如 `atk * 120 / 100`）
- 禁止依赖系统时间、内存地址、容器遍历顺序等不确定因素
- 随机数统一用一个 seeded PRNG（如 xoshiro256），调用顺序必须固定

### 8.3 BattleReport 数据结构

```protobuf
message BattleReport {
    uint64 battle_id       = 1;
    uint64 random_seed     = 2;
    ArmySnapshot attacker  = 3;
    ArmySnapshot defender  = 4;
    repeated Round rounds  = 5;
    BattleResult result    = 6;
}

message Round {
    uint32 round_number          = 1;
    repeated BattleAction actions = 2;
}

message BattleAction {
    ActionType type        = 1;   // NORMAL_ATTACK, SKILL, BUFF, DEBUFF, HEAL, DEATH
    uint32 source_slot     = 2;   // 发起者位置 (0-5)
    uint32 target_slot     = 3;   // 目标位置
    uint32 skill_id        = 4;   // 触发的战法ID
    int32  damage          = 5;   // 伤害/治疗量
    int32  source_hp_after = 6;   // 行动后血量（自包含，客户端不需要自己算）
    int32  target_hp_after = 7;
    repeated BuffEvent buffs = 8;
    bool   is_critical     = 9;
    bool   is_dodged       = 10;
}

message BuffEvent {
    uint32 buff_id   = 1;
    uint32 target    = 2;
    bool   is_add    = 3;   // true=添加, false=移除
    int32  remaining = 4;   // 剩余回合数
}
```

**设计要点**：
- 每个 Action 自包含 hp_after → 客户端展示永远和服务端一致
- Action 序列线性 → 客户端按顺序逐条播放动画
- 典型大小：~1-5 KB/场（protobuf）

### 8.4 客户端回放流程

```
收到 BattleReport → 进入战斗场景，摆放武将模型
  ↓
for each round:
    for each action:
        播放对应动画 (普攻/技能特效/buff图标/死亡)
        更新血条到 action.target_hp_after
        等待动画结束（快进时跳过）
  ↓
展示结果面板（胜负、战损、经验）
```

**快进/跳过**：1x / 2x / 直接跳到结果。Action 包含状态快照 → 任意位置可 seek。

### 8.5 事件日志 vs 输入重模拟

| | 事件日志（SLG 首选） | 输入重模拟 |
|---|---|---|
| 战报内容 | 完整 Action 序列 | 只存 BattleInput + seed |
| 客户端要求 | 只需动画播放器 | 必须内嵌完整战斗逻辑 |
| 数据量 | ~1-5 KB/场 | ~200 bytes/场 |
| 跨版本兼容 | 旧战报永远可播放 | 逻辑变更后旧 seed 结果不同 |
| 安全性 | 客户端无战斗逻辑可逆向 | 客户端有完整逻辑 |

选择事件日志：长期存储（联盟战史）、跨版本兼容、降低逆向风险。

### 8.6 存储与分发

- 实时推送给双方玩家
- 压缩存储：protobuf serialize → zstd → DB blob（~500B-2KB/条）
- 每人保留最近 100 条 → ~200KB/人
- 全服 10 万玩家 → ~20GB，完全可控
- 超过 30 天归档冷存储
- 历史查看：请求 battle_id → 解压 → 返回

### 8.7 防篡改校验

- CRC32 校验和覆盖 input + 所有 actions（服务端生成）
- 争议时可用 BattleInput 重新模拟，对比 Action 序列
- 更强保证：HMAC 替代 CRC32
