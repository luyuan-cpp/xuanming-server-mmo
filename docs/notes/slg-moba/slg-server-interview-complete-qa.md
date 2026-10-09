# 率土之滨 SLG 服务器主程序面试 —— 全面 Q&A

**Updated:** 2026-04-02

> 每题格式: **简答** → **详细解释** → **深挖追问**

---

## 目录

### 一、架构设计 (Q1~Q12)
### 二、大地图与行军 (Q13~Q25)
### 三、战斗系统 (Q26~Q36)
### 四、迷雾与视野 (Q37~Q44)
### 五、数据层与持久化 (Q45~Q55)
### 六、网络与协议 (Q56~Q63)
### 七、并发与性能 (Q64~Q73)
### 八、赛季与运维 (Q74~Q82)
### 九、安全与反作弊 (Q83~Q90)
### 十、联盟与社交 (Q91~Q96)
### 十一、武将与战斗数值 (Q97~Q103)
### 十二、工程能力 (Q104~Q115)
### 十三、系统设计开放题 (Q116~Q120)

---

## 一、架构设计

### Q1: 率土之滨这类 SLG 的服务架构和 MMORPG 有什么根本区别?

**简答**: MMORPG 是多小场景 + 帧同步/状态同步 + 实体级 AOI；SLG 是一张大地图 + 事件驱动 + 格子级 AOI + 离线交互。

**详细解释**:

| 维度 | MMORPG | SLG |
|------|--------|-----|
| 地图模型 | 多小副本，连续坐标 | 单张超大地图，离散格子 |
| 同步模型 | 帧同步/状态同步 (16~60fps) | 事件驱动 + 懒计算 (无 tick) |
| AOI | 实体中心 ("谁在我附近") | 格子中心 ("我能看到哪些格子") |
| 战斗 | 实时动作/技能 | 回合制自动 + 战报回放 |
| 在线性 | 强在线 (离线=消失) | 弱在线 (离线城池仍存在) |
| 场景服扩展 | 开新副本 = 新进程 | 地图不可水平拆分，只能 Region 分区 |
| 核心瓶颈 | 单场景人数上限 | 全地图并发行军/碰撞 |

**深挖追问**:
- "为什么 SLG 地图不能水平拆分？" → 行军跨 Region 会涉及分布式事务（碰撞检测需要原子性读取两个 Region 的数据），引入 2PC 的复杂度远超收益。实践中用 Region 分区 + 锁/Actor 模型解决并发，保持单进程。
- "如果地图真的太大放不进一台机器呢？" → 分服。率土之滨的策略就是赛季服 (5K~10K 人一个服)，一台物理机可以跑多个赛季的 MapService 实例。

---

### Q2: MapService 为什么是有状态的?不能做成无状态微服务吗?

**简答**: 地图是全局共享可变状态 (1.44M 格子 + 万级行军)，每次操作都需要读写地图数据。做成无状态需要所有状态外置到 Redis，但延迟和一致性代价过高。

**详细解释**:

MapService 持有的状态:
- 144 万格子的 owner/building/resource/terrain
- 所有活跃行军的路径、时间表、occupant 注册
- 所有格子的视野观察者表
- 建筑队列、采集状态

如果无状态化:
- 每次行军推进/碰撞检测都要跨网络读写 Redis → 延迟从 ns 级变成 ms 级
- 碰撞检测需要原子读写多个格子的 occupant 数据 → 分布式锁/事务
- 视野推送需要遍历所有 observer → 在 Redis 上做这种遍历极其低效
- 结论: 破坏了性能模型，不值得

正确做法: **单进程有状态 + Region 内并发** + 数据异步持久化到 Redis/MySQL。

**深挖追问**:
- "那 MapService 宕机怎么办？" → 见 Q77 (崩溃恢复)
- "一个进程能撑多少人？" → 见 Q11 (容量规划)

---

### Q3: BattleService 为什么可以无状态?

**简答**: `BattleReport = SimulateBattle(input)` 是纯函数，输入完全来自请求，不依赖任何本地状态。

**详细解释**:
- 一次战斗的所有输入: 攻方快照 + 守方快照 + 随机种子 + 地形
- 这些都在请求里传过来
- BattleService 只做计算，返回战报，不保存任何东西
- 天然的 Worker Pool 模型: N 个实例并行处理战斗请求
- 负载高 → 水平扩容，加更多 BattleService 实例

**深挖追问**:
- "为什么不在 MapService 内直接算?" → 可以 (单战 < 1ms)，独立出来是为了万人同时战斗时不阻塞 Map 主循环，以及方便独立扩容。小项目可以不拆。

---

### Q4: 服务间通信你选 gRPC 还是 Kafka? 各在什么场景?

**简答**: 需要即时返回的用 gRPC (登录验证、数据查询)；不需要即时返回且需要可靠投递的用 Kafka (数据持久化、战斗结果、事件通知)。

**详细解释**:

| 场景 | 选择 | 原因 |
|------|------|------|
| 登录验证 | gRPC | 需要同步返回 token |
| 查询玩家数据 | gRPC | 需要立即拿到数据 |
| 战斗请求 | Kafka/gRPC 均可 | 战斗可以异步处理，用 Kafka 解耦更好 |
| 战斗结果 | Kafka | 异步通知 MapService 应用结果 |
| 数据持久化 | Kafka | 异步写入，可靠重试，不阻塞业务 |
| 推送消息到客户端 | Kafka → Gate | Gate 消费 Kafka 推送给对应连接 |
| 联盟事件广播 | Kafka | 扇出到多个消费者 |

**深挖追问**:
- "Kafka 消息丢失怎么办?" → `acks=all` + `min.insync.replicas=2`，生产者开启幂等。消费端用 manual commit，处理完再 ack。
- "Kafka 消息顺序保证?" → 同一 player_id 作为 partition key → 同玩家消息保证顺序。

---

### Q5: 说说你理解的 SLG 里 "事件驱动" 架构?

**简答**: 所有状态变化都由离散事件触发，而不是逐帧 tick 推进。行军到达、建造完成、战斗结束都是事件。

**详细解释**:

```
传统 MMORPG (tick-based):
  while(running):
    for each entity:
      entity.update(dt)    // 每帧推进所有实体

SLG (event-driven):
  timer_wheel.tick():
    for each expired_event:
      handle(expired_event)  // 只处理到期事件

一万个行军中，每秒只有几个到达目的地 → 只有这几个生成事件。
其余 9990+ 个行军零 CPU 开销（位置可由公式算出）。
```

核心思想: **不做事件没触发时的无用计算**。

事件类型:
- `MarchArrived(army_id, tile_id)` → 触发战斗/驻守/采集
- `BuildComplete(player_id, building_id)` → 更新格子、推送
- `BattleEnded(battle_id, result)` → 应用结果、推送战报
- `VisionEnter(player_id, tile_id)` → 推送格子数据
- `ResourceDepleted(tile_id)` → 通知采集结束

**深挖追问**:
- "事件之间有依赖怎么处理?" → 同一 Region 内事件串行处理，保证因果序。跨 Region 事件通过 EventBus 异步协调。

---

### Q6: MapService 内部用什么并发模型? Actor? Thread Pool? Coroutine?

**简答**: **Region 级 Actor 模型** 最合适 — 每个 Region 是一个 Actor，拥有自己的消息队列，串行处理区域内事件。

**详细解释**:

```
方案对比:
1. 大锁模型: 简单但并发度为 1，不推荐
2. 细粒度锁 (per-tile): 锁竞争严重，死锁风险高
3. Thread Pool + 锁: 锁粒度难把握
4. Region Actor (推荐): 
   - 1444 个 Region，每个 Region 有自己的 mailbox
   - Region 内事件串行处理，无需锁
   - Region 间通信通过异步消息
   - Worker Thread Pool 调度 Region Actor

跨 Region 行军:
  行军从 RegionA 进入 RegionB → 
  RegionA Actor 发消息给 RegionB Actor: "army X entering at tile Y"
  RegionB Actor 收到后处理（碰撞检测、视野更新）
```

**深挖追问**:
- "行军跨 Region 时一致性怎么保证?" → 行军 ownership 在任一时刻只属于一个 Region（当前所在）。进入新 Region 时原子 handoff: 旧 Region 移除 → 消息 → 新 Region 接管。
- "Region 大小怎么定?" → 太小: 跨 Region 消息多; 太大: 并发度低。32×32 是常见选择，1444 个 Region 足够利用 32~64 核 CPU。

---

### Q7: 为什么 SLG 的 AOI 不能用 MMORPG 的 AOI 实现?

**简答**: 问题模型不同。MMORPG AOI 是 "谁在我附近" (连续坐标、实体中心)；SLG AOI 是 "我能看到哪些格子" (离散坐标、格子中心)。

**详细解释**:

```
MMORPG AOI:
  - 连续坐标 (float x, y, z)
  - Grid Hash 或十字链表
  - 实体进入/离开格子 → 通知附近实体
  - 关心的是: "这个NPC/Player 距离我多远?"
  - 更新频率: 极高 (每帧移动)

SLG AOI:
  - 离散格子 (int x, y)
  - Per-tile observer counter
  - 视野来源 (城池/行军/建筑) 在格子上 ++ / --
  - 关心的是: "这个格子对我是否可见?"
  - 更新频率: 低 (行军经过格子时才更新)
  - 观察者是玩家，不是实体

数据结构完全不同 → 无法复用。
```

**深挖追问**:
- "MMORPG 的九宫格 AOI 能不能硬改?" → 勉强可以，但会浪费大量计算。九宫格 AOI 是围绕实体设计的，而 SLG 需要围绕格子维护观察者列表，硬改会丢失"每格子多少人在观察"的核心信息。

---

### Q8: 离线玩家的城池怎么处理?

**简答**: 城池数据常驻内存，离线只意味着 "没有打开观察窗口"，不意味着数据卸载。

**详细解释**:

```
在线 = "打开了客户端连接，在接收推送"
离线 = "没有客户端连接，但所有数据仍在"

MapService 中:
  - 所有玩家的城池、建筑、占领格子始终在内存中
  - 离线玩家的城池可以被任何人侦察、攻击
  - 被攻击 → 正常战斗流程 → 结算 → 战报存 DB → 下次登录推送

  // 不是 "离线玩家数据在 Redis, 在线加载到内存"
  // 而是 "所有地图相关数据始终在 MapService 内存中"

需要区分:
  地图数据 (格子/城池/行军) → 始终在 MapService 内存
  玩家个人数据 (背包/武将详情) → Redis，按需加载
```

**深挖追问**:
- "10 万注册玩家的城池全常驻内存?" → 是的，每个城池占用很少 (几百字节)，10 万个也就几十 MB。
- "5 万离线玩家的武将数据也在内存?" → 不需要。武将详情在 Redis，只有战斗时才加载快照。地图上只存城池位置/等级/归属。

---

### Q9: gate 服务的作用是什么? 和 MMORPG 的 gate 有区别吗?

**简答**: SLG 的 Gate 和 MMORPG 的 Gate 职责相同 — TCP 连接管理、协议路由、心跳、频控。区别在于 SLG 的消息频率低得多。

**详细解释**:

```
Gate 职责:
1. 管理客户端 TCP 长连接
2. 协议解析（消息头 + protobuf 反序列化）
3. 路由: 根据 msg_id 转发到对应后端服务
4. 心跳检测: 30s ping, 90s 超时断开
5. 频控: 按 msg_id 分类限频
6. 推送: 从 Kafka 消费后端服务的推送消息，发给对应客户端
7. 加密/解密 (如果有 beyond-TLS 加密)
8. Session 管理: player_id ↔ connection 映射

和 MMORPG 的区别:
  - MMORPG Gate: 高频消息 (每秒 60+)，对延迟极敏感 (<50ms)
  - SLG Gate: 低频消息 (每秒 1~5)，延迟相对不敏感 (<200ms)
  - SLG Gate 的连接数可能更多 (很多挂机玩家保持连接)
```

**深挖追问**:
- "Gate 怎么知道把消息路由到哪个后端?" → msg_id 范围分段 (1000~1999=Map, 2000~2999=Alliance, ...) 或通过 proto service 定义的 service_id。
- "Gate 宕机了玩家能自动重连吗?" → 客户端实现断线重连，重连后发送 session_token，Gate 恢复 session 映射。

---

### Q10: 给你 1200×1200 的大地图 SLG 项目，从零开始你怎么设计服务架构?

**简答**: 核心大单体 (Map+Build+Army 合一个进程) + 无状态战斗服务 + 辅助微服务 + 标准数据层。

**详细解释**:

```
Phase 1 — 核心 (MVP):
  [Gate] ←TCP→ [Client]
  [Gate] ←gRPC→ [GameService]  // Map + Build + Army + Vision 全在一个进程
  [GameService] → [Redis] + [MySQL]

Phase 2 — 拆分战斗:
  [GameService] ←Kafka→ [BattleService (×N)]

Phase 3 — 拆分辅助:
  [AllianceService] [ChatService] [RankService]
  [DataService] (做数据路由/代理)
  [SchedulerService] (赛季/活动调度)

Phase 4 — 运维:
  [etcd] 服务发现
  [Kafka] 异步消息
  [Prometheus + Grafana] 监控
  K8s 部署

原则: 先做最简单的能跑的版本，有明确瓶颈再拆分。
不要上来就微服务 —— SLG 核心天然是单体。
```

**深挖追问**:
- "一开始就用微服务不行?" → 过早拆分增加开发/调试/部署复杂度，SLG 核心逻辑的耦合度很高 (Map-March-Vision-Battle 强关联)，拆太多反而增加网络延迟和一致性问题。
- "什么时候该拆?" → 有明确的性能证据 (profiling 显示某模块是瓶颈) 或团队规模扩大到需要独立开发/部署时再拆。

---

### Q11: 单台服务器能撑多少人?

**简答**: 单赛季 5K~10K 在线，50K~200K 注册，总内存 < 1GB，CPU 主要被战斗和视野推送占用。

**详细解释**:

```
内存估算:
  地图格子: 1.44M × 128B ≈ 180 MB
  行军 (50K): 50K × 1KB ≈ 50 MB
  视野表: ~100 MB (按需分配)
  建筑队列: ~10 MB
  其他: ~50 MB
  总计: ~400 MB (MapService 进程)

CPU 估算 (10K 在线):
  行军事件: ~1000/s → 视野更新 + 碰撞检查，~5ms total
  战斗: ~100/s × 1ms ≈ 100ms/s → 10% 单核
  视野推送: ~5000 msg/s → 网络 IO 为主
  协议处理: ~50K msg/s → 序列化/反序列化

  32 核机器 → 绑定 16 核给 MapService → 充裕

单台物理机 (64C/128G) 可同时运行 4~8 个赛季实例。
```

**深挖追问**:
- "如果某赛季突然涌入 5 万人呢?" → 硬限制: 排队系统。单赛季地图就 1200×1200，容纳不了那么多城池。开新赛季服分流。

---

### Q12: etcd 在 SLG 架构里做什么?

**简答**: 服务注册与发现 + 分布式配置 + 选主 (MapService 主备切换)。

**详细解释**:

```
1. 服务注册: 每个服务启动时注册到 etcd (地址, 端口, 健康状态)
   key: /services/game/battle-service/instance-1
   value: {"addr": "10.0.0.5:50051", "status": "healthy"}
   TTL: 10s, 续约 lease

2. 服务发现: Gate 启动时 watch /services/game/* → 获取所有后端地址
   后端增删 → 自动更新路由表

3. 配置中心: 
   key: /config/game/season/phase → "mid_season"
   watch 变化 → 动态更新

4. MapService 选主:
   多个 MapService 实例竞争 lease
   获得 lease 的成为 Active
   lease 过期 → Standby 提升
```

---

## 二、大地图与行军

### Q13: 1200×1200 的地图数据结构怎么设计?

**简答**: 二维数组 `Tile tiles[1200][1200]`，每个 Tile 包含地形、归属、建筑、资源等字段，~64-128B/格子，总 ~180MB。

**详细解释**: 见框架文档 §3.1。格子用 tile_id = y*1200+x 一维索引，支持 O(1) 随机访问。

**深挖追问**:
- "为什么不用 HashMap?" → 所有格子都存在且需要随机访问，数组比 HashMap 快且省内存 (无 hash 冲突，无指针开销)。
- "格子里存什么?" → terrain, owner_player_id, building_id, building_level, resource_type, resource_amount, 以及行军 occupant 列表和视野 observer 表 (可能用指针指向独立结构以节省空间)。

---

### Q14: SLG 寻路用什么算法? 为什么?

**简答**: A* + JPS 基础引擎，大地图加 HPA* 分层。不用 NavMesh (离散格子用不着)。

**详细解释**:

```
A*: 经典图搜索，保证最短路径。1200×1200 上最坏 ~250ms。
JPS: A* 的剪枝加速，跳过对称路径，10-30× 加速 (~12ms)。仅适用均匀代价方格图。
HPA*: 分层寻路。地图切 chunk(32×32)，预计算入口点+距离，在线查询先粗搜后精搜，~3ms。

性能对比 (1024×1024, 30% 障碍):
  A*:   ~800K 展开节点, ~250ms
  JPS:  ~15K 展开节点, ~12ms
  HPA*: ~200 粗搜 + ~5K 精搜, ~3ms
```

**深挖追问**:
- "JPS 的限制是什么?" → 不支持不同地形代价 (沼泽=2, 平地=1)。如果需要地形代价差异，用 A* + HPA*。
- "地图动态变化时 HPA* 怎么办?" → 只重算受影响 chunk 的入口点和内部距离，不需全图重算。
- "寻路在哪一端做?" → 服务端（防作弊），客户端只预测显示。

---

### Q15: 行军位置怎么同步?

**简答**: 懒计算 — `current_tile = path[floor((now - start_time) * speed)]`。服务器发送 `{path, start_time, speed}`，客户端自行插值。

**详细解释**: 见框架文档 §4.2。

**深挖追问**:
- "客户端和服务端算出的位置不一致怎么办?" → 服务端是权威。客户端有偏差不影响逻辑，显示上可能有微小跳变。关键操作 (战斗触发) 完全看服务端。
- "speed 变了怎么办?" → 重新下发: 新 start_time = now, 新 path = 剩余路径, 新 speed。客户端收到后重建动画。

---

### Q16: 没有逐帧 tick，怎么检测两支军队在路上相遇?

**简答**: 时空碰撞检测 — 每个格子维护 `occupants[{army_id, enter_time, leave_time}]`，新行军注册时扫描路径格子，检测时间窗口重叠。

**详细解释**: 见框架文档 §4.3。

**深挖追问**:
- "occupant 数据量大不大?" → 平均每格 0~2 个 occupant (大部分格子无行军经过)，峰值每格 <10 个。50K 行军 × 100 格子路径 → 500 万条 occupant 记录，每条 ~24B → ~120MB。
- "碰撞检测性能?" → O(path_length × avg_occupants)，路径平均 100 格、每格平均 1~2 个 occupant → 几百次比较，< 0.1ms。
- "两支友军在格子重叠会怎样?" → 碰撞检测只对敌对关系触发战斗。友军重叠跳过。

---

### Q17: 行军被召回了怎么处理?

**简答**: 1) 用公式算当前位置; 2) 清除剩余路径上的 occupant; 3) 取消已注册定时器; 4) 生成反向路径; 5) 重新注册 occupant + 碰撞检测。

**详细解释**: 见框架文档 §4.4。

**深挖追问**:
- "召回期间能被攻击吗?" → 能。返程也是一次新行军，路径上的 occupant 正常注册，会被检测到碰撞。
- "加速道具怎么处理?" → speed 变化 = 召回 + 新行军 (新速度, 剩余路径)。

---

### Q18: path 很长怎么办? 1200×1200 对角线有 1700 格

**简答**: 游戏设计天然限制了 path 长度 — 率土之滨有 `max_march_range` 上限，初始 ~20 格，满级满科技 ~100-150 格。服务端发起行军时强制校验。

**详细解释**:

```
行军距离上限的来源:
├── 基础值: ~20 格 (新号)
├── 科技 "行军距离" 每级 +5 格
├── 武将被动技能加成
├── VIP 等级加成
├── 临时道具/buff
└── 硬上限: ~150-200 格

服务端校验:
  ValidateMarch(player, dest):
    path = find_path(player.city, dest)
    max_range = calc_max_march_range(player)
    if path.size() > max_range:
      return ERROR_MARCH_TOO_FAR
    if not is_in_marchable_area(player, dest):
      return ERROR_OUT_OF_TERRITORY  // 必须从领地边界出发

领地规则 (率土特色):
  - 只能向自己/盟友领地相邻的格子行军
  - 不能跳过空白区直飞远处
  - 实际大部分行军只有 10-30 格
```

**性能意义**:
- 无上限: 50K 行军 × 1700 格 × 4B = **340MB** path 内存
- 有上限: 50K 行军 × 平均 30 格 × 4B = **6MB** path 内存
- 碰撞检测: O(150) vs O(1700), 差 10 倍
- occupant 注册: 每行军 ≤150 条 vs ≤1700 条
- 视野事件: ≤150 个 vs ≤1700 个

这是游戏设计和技术性能的**完美对齐** — 游戏逼你先扩领地再远征，恰好限制了 path 长度。

**深挖追问**:
- "如果玩家 hack 客户端发送超长 path?" → 服务端校验 path.size() <= max_range，拒绝非法请求。寻路本身也在服务端做。
- "返程路径也受限吗?" → 返程不受 march_range 限制（已经在外面了，总得让人回来）。但返程路径长度和去程一样，不会超过原始限制。

---

### Q19: 50K 并发行军的内存开销? path 能优化吗?

**简答**: 原始存法 ~6MB；用方向编码优化到 ~1.7MB。核心思路：格子地图相邻只有 8 方向，每步 1 byte 足够，不需要存完整 tile_id。

**详细解释**:

```
优化方案: 起点 tile_id + uint8_t 方向数组 (代替 uint32_t tile_id 数组)

struct CompactPath {
    uint32_t start_tile;         // 4 B
    std::vector<uint8_t> dirs;   // 每步 1 B (方向 0~7)
};

// 原始: 30步 × 4B = 120B/条
// 优化: 4B + 30步 × 1B = 34B/条  → 节省 71%
// 50K 行军: 6MB → 1.7MB

还原任意位置:
  tile_at(idx): 从 start_tile 沿 dirs[0..idx-1] 逐步推导
  懒计算配合: 缓存 {current_tile, current_idx}, 每次只推 1 步 → O(1)

更激进的方案:
  3-bit 位压缩: 30步 = 12B → 50K = 0.6MB (但位操作复杂)
  RLE 方向:    直线多时 5~15 段 × 2B → 50K = 0.5MB
  JPS 跳点:    只存拐点坐标 → 50K = 0.8MB

推荐 uint8 方向数组 — 节省 70%+ 且代码简单。
更激进的压缩在 ~1.7MB 基础上收益极小，不值得。
```

**深挖追问**:
- "方向数组随机访问不是 O(1) 了?" → 对。但行军只需要"当前位置"，缓存后每步推进 O(1)。碰撞检测/视野注册本来就是顺序遍历，不需要随机访问。
- "网络传输呢？下发给客户端也省流量?" → 是的。方向数组比 tile_id 数组节省 ~70% 带宽。客户端用相同的 neighbor() 函数还原。

---

### Q20: 行军到达目的地后的处理流程?

**简答**: 定时器触发 → 根据 purpose 分发 → 攻击/驻守/采集/侦察/返回。

**详细解释**:

```
MarchArrived(army_id, tile_id):
  switch march.purpose:
    ATTACK:
      // 检查目标格子是否有敌方城池/部队
      if has_enemy(tile_id, march.player_id):
        // 创建 BattleRequest → 发送到 BattleService
        BattleService.RequestBattle(attacker_snapshot, defender_snapshot, seed, terrain)
      else:
        // 目标已被移除/投降 → 占领空地或返回
    REINFORCE:
      // 加入目标格子的驻军列表
      tile.garrison.add(army_id)
    GATHER:
      // 开始采集: 注册采集完成定时器 (duration = resource_amount / gather_speed)
      register_timer(gather_complete_time, GatherComplete(army_id, tile_id))
    SCOUT:
      // 暂时揭露目标区域视野 → 生成侦察报告 → 返回
      reveal_area(tile_id, scout_radius, player_id, duration=30s)
      march.purpose = RETURN; register_return_march(army_id)
    RETURN:
      // 回到城池 → 解散行军 → 恢复兵力
      disband_march(army_id)
```

---

### Q20: 路径中间有人建了新建筑 (障碍变了)，怎么处理?

**简答**: 地图障碍变化时，检查该格子的 passing_armies 列表，对受影响的行军触发重新寻路。

**详细解释**:

```
OnTileObstacleChanged(tile_id):
  for army_id in tile.passing_armies:
    if army hasn't passed this tile yet:
      // 需要重新寻路
      current_pos = compute_current_pos(army_id)
      new_path = find_path(current_pos, army.destination)
      if new_path exists:
        update_march(army_id, new_path)
      else:
        // 无法到达 → 返回
        recall_march(army_id)
```

SLG 地图变化频率极低 (建造一个建筑可能几分钟~小时)，所以 repath 开销完全可接受。

**深挖追问**:
- "repath 期间行军继续走吗?" → 是，行军不暂停。repath 在下一个 tick 生效。如果已经走过了障碍格子，就不需要 repath。

---

### Q21: 行军的碰撞检测是 O(path_length)，路径很长会不会太慢?

**简答**: 1200×1200 对角线最长 ~1700 格，每格检查 1~2 个 occupant，约 3000 次比较，< 0.5ms。完全可接受。

**深挖追问**:
- "能不能优化?" → 可以。用 Region 级粗筛: 先检查路径经过哪些 Region，只查有活跃行军的 Region 的 occupant。

---

### Q22: 同一个格子同时有 100 支军队经过会怎样?

**简答**: 这种情况极端但可能 (热门战略要地)。碰撞检测变成每支新军检查 100 个 occupant (100 次比较)，仍然很快。

**深挖追问**:
- "100v100 的碰撞处理?" → 碰撞检测按时间排序，最早碰撞的先战斗。战斗改变格子状态后，后续碰撞可能变化（比如一方全灭后不再有碰撞）。

---

### Q23: 给行军加 buff (加速/减速) 怎么处理?

**简答**: speed 变化 = 在当前位置截断路径，用新 speed 重新注册后半段。

```
ApplySpeedBuff(army_id, new_speed):
  current_pos = compute_current_pos(army_id)
  remaining_path = path[current_pos_idx:]
  // 清除旧的 occupant + 定时器
  clear_occupants(army_id, remaining_path)
  cancel_timers(army_id)
  // 用新速度重新注册
  army.speed = new_speed
  army.start_time = now
  army.path = remaining_path
  register_occupants(army_id, remaining_path, new_speed)
  register_timers(army_id, remaining_path, new_speed)
  // 重新碰撞检测
  detect_collisions(army_id, remaining_path, new_speed)
  // 通知客户端
  push_march_update(army_id, {remaining_path, now, new_speed})
```

---

### Q24: 行军队列有上限吗?

**简答**: 有。默认每个玩家 3~5 支行军 (通过武将数量/VIP 等级决定)。这是天然的负载控制。

**深挖追问**:
- "为什么不能无限行军?" → 游戏设计限制 + 服务器性能考量。单玩家 100 支行军 × 10K 玩家 = 100 万行军，内存和碰撞检测都扛不住。

---

### Q25: 行军系统最难的技术点是什么?

**简答**: **时空碰撞检测 + 回调/加速后的一致性维护**。

**详细解释**:

碰撞检测本身不难 (就是时间窗口比较)，难的是维护一致性:
1. 行军 A 注册时和 B 碰撞 → 安排战斗
2. A 被召回 → 取消战斗 → 清除 occupant
3. 同时 B 加速 → B 的 occupant 时间窗口变了 → 需要重新检查 B 和所有其他行军的碰撞
4. 同时 C 也经过该格子 → C 的碰撞检测结果可能因 B 速度变化而改变

这些级联操作需要非常仔细的状态管理。建议:
- **每次状态变化 (召回/加速/新注册) 都清除旧状态，完整重建新状态**
- **不要试图增量修补** — 太容易出 bug

---

## 三、战斗系统

### Q26: 战斗确定性怎么保证?

**简答**: 纯函数 + 不用 float + 单 PRNG + 固定迭代顺序 + 不依赖系统状态。

**详细解释**:

```
确定性约束 (检查清单):
□ 不使用 float/double → 用整数百分比 (e.g., atk * 120 / 100)
□ 不使用 time(), rand() → 用传入的 seed 初始化 xoshiro256
□ 不使用 unordered_map/set → 迭代顺序不确定
□ 不使用内存地址做排序
□ PRNG 调用顺序固定 (先攻方技能判定, 后防方)
□ 处理顺序固定 (按 slot 0,1,2 遍历)
□ 数值计算用固定公式 (先乘后除, 避免精度差异)
□ 整除方向固定 (向下取整)
```

**深挖追问**:
- "不同编译器/平台的整数运算会有差异吗?" → 对于有符号整数溢出，C++ 是 UB。解决: 使用 unsigned 或保证不溢出。int32 范围内的乘除不会溢出 (用 int64 中间值)。
- "如果未来要做跨平台战斗验证呢?" → 确定性引擎用 C++ 写，客户端也可内嵌 C++ 验证库。或者用我们选择的 event-log 方案，客户端根本不需要验证。

---

### Q27: 战斗伤害公式怎么设计?

**简答**: `damage = ATK * skill_rate / 100 * 1000 / (1000 + target_DEF) * troop_count / base_troop`

**详细解释**:

```cpp
int32_t CalcDamage(int32_t atk, int32_t def, int32_t skill_rate, 
                    int32_t troop_count, int32_t base_troop,
                    bool is_type_advantage) {
    // 基础伤害 = 攻击力 × 技能倍率(百分比)
    int64_t base = (int64_t)atk * skill_rate / 100;
    // 防御减伤 = 1000 / (1000 + DEF), 这样 DEF=1000 时减伤50%
    base = base * 1000 / (1000 + def);
    // 兵力系数 = 当前兵力 / 基准兵力
    base = base * troop_count / base_troop;
    // 克制加成
    if (is_type_advantage) base = base * 120 / 100;  // +20%
    // 最低伤害 = 1
    return std::max((int32_t)base, 1);
}

// 暴击: damage * 150 / 100 (PRNG 判定暴击率)
// 闪避: damage = 0 (PRNG 判定闪避率)
```

公式特点: 全整数，先乘后除，中间值用 int64 防溢出。

**深挖追问**:
- "为什么防御公式用 1000/(1000+DEF) 而不是线性 DEF/ATK?" → 曲线更平滑，高防也不会 100% 减伤。DEF=0→0%减伤, DEF=1000→50%, DEF=3000→75%。避免堆防御到极端。

---

### Q28: 战报用 event-log 还是 input-resimulation?

**简答**: Event-log (推荐)。包含完整的 action 序列，客户端只回放动画，不需要战斗逻辑。

**详细解释**: 

| | Event-Log (推荐) | Input-Resimulation |
|---|---|---|
| 战报内容 | 完整 action 序列 | 只有 input + seed |
| 客户端要求 | 动画播放器 | 必须内嵌完整战斗逻辑 |
| 数据大小 | 1~5 KB | ~200 B |
| 跨版本兼容 | 旧战报永远可回放 | 逻辑修改 → 旧 seed 不再复现 |
| 安全性 | 客户端无战斗逻辑 | 客户端有完整逻辑可被逆向 |
| 存储 | protobuf + zstd → 500B~2KB | 很小 |

**深挖追问**:
- "event-log 体积比 seed 大 10 倍，能接受吗?" → 10 万玩家 × 100 场战斗 × 2KB = ~2GB，trivial。
- "如果需要验证战斗有没有 bug?" → BattleReport 内含 BattleInput (input + seed)，后端可以随时重跑 SimulateBattle 验证。

---

### Q29: 多支军队攻同一个城怎么处理?

**简答**: SiegeSession 按到达时间排队，逐一与守军战斗。守方不完全恢复 HP → 消耗战特点。

**详细解释**: 见框架文档 §5.5。

**深挖追问**:
- "同时到达的多支军队算一起还是分开?" → 同联盟的合并计算 (集结攻击)；敌对联盟的排队分开打。
- "守方可以在攻城期间增援吗?" → 可以。援军到达后加入防守队列，在当前攻击者之后的轮次中生效。

---

### Q30: 如何防止战斗作弊?

**简答**: 战斗完全服务端运行，客户端不参与任何计算。客户端只收到战报并播放。

**详细解释**:

```
客户端能做的作弊: 无。
  - 不能修改伤害 → 伤害在服务端算
  - 不能跳过战斗 → 战斗在服务端跑
  - 不能修改结果 → 结果在服务端定
  - 不能加速战斗 → 战斗是瞬时的，客户端回放只是动画

唯一的风险: 服务端自身的 bug (如数值溢出)
  → 用 BattleInput 重跑验证
  → GM 工具可以随时重新模拟任何一场战斗
```

---

### Q31: 战斗里的随机数怎么生成?

**简答**: 每场战斗传入 seed，用 xoshiro256 (或 PCG) 生成, 固定调用顺序。

**详细解释**:

```cpp
class BattlePRNG {
    uint64_t state[4]; // xoshiro256** state
public:
    explicit BattlePRNG(uint64_t seed);
    uint64_t next();
    // 范围随机 [0, max)
    uint32_t range(uint32_t max) { return next() % max; } // 简化版
    // 概率判定
    bool chance(uint32_t percent) { return range(100) < percent; }
};

// 调用顺序固定:
// 每个回合，按 slot 0,1,2...N 依次:
//   1. 是否暴击? prng.chance(crit_rate)
//   2. 是否闪避? prng.chance(dodge_rate)
//   3. 技能是否触发? prng.chance(skill_rate)
//   4. 目标选择? prng.range(valid_targets.size())

// 即使某次判定不需要 (比如角色已死), 也要调用 prng 保持序列一致
```

**深挖追问**:
- "为什么不用 std::mt19937?" → mt19937 也行，但状态大 (2.5KB)。xoshiro256 状态只有 32B，速度更快。
- "seed 从哪来?" → MapService 生成: `seed = hash(battle_id, timestamp, server_id)`。battle_id 是全局唯一的 snowflake ID。

---

### Q32: 战斗怎么做技能系统?

**简答**: 每个武将 1 主动技能 + N 被动技能。主动技能有冷却/怒气条件，被动是触发型。

**详细解释**:

```
技能类型:
├── 主动技能: 每 N 回合可用 / 怒气满时释放
│   ├── 单体攻击 (选单目标)
│   ├── AOE (选范围内多个目标)
│   └── 辅助 (加 buff/回血)
├── 被动技能:
│   ├── 触发型: "被攻击时有 30% 概率反击"
│   ├── 光环型: "全队攻击力 +10%"
│   └── 条件型: "HP < 50% 时防御力 +30%"

技能执行:
  ChooseAction(unit, prng):
    if active_skill.is_ready():
      targets = select_targets(active_skill.target_rule, prng)
      execute_skill(active_skill, unit, targets, prng)
      active_skill.start_cooldown()
    else:
      target = select_target(NORMAL_ATTACK_RULE, prng)
      execute_normal_attack(unit, target, prng)
    // 检查被动技能触发
    for passive in unit.passives:
      if passive.trigger_condition_met():
        execute_passive(passive, prng)
```

**深挖追问**:
- "技能描述数据驱动还是代码写死?" → 数据驱动。技能配表 (Excel → protobuf)，定义: 目标选择规则、伤害公式参数、buff 类型和时长。引擎读配表执行。
- "新增技能效果需要改代码吗?" → 如果是已有效果类型的组合 (伤害+眩晕+DOT)，纯配表。如果是全新效果类型 (如"召唤")，需要新增效果处理器。

---

### Q33: 战斗的 buff/debuff 系统怎么设计?

**简答**: Buff 表驱动，每个 buff 有 id/持续回合/叠加规则/效果列表。

```
Buff 数据:
  buff_id: 1001
  name: "燃烧"
  duration_rounds: 3
  stack_rule: REPLACE / STACK / REFRESH_DURATION
  max_stacks: 3
  effects:
    - type: DOT, damage_percent: 10, element: FIRE  // 每回合 10% 火伤
    - type: DEBUFF_DEF, percent: -20                 // 防御 -20%

Buff 管理器:
  AddBuff(target, buff_id, source, prng):
    existing = target.buffs.find(buff_id)
    switch stack_rule:
      REPLACE: remove old, add new
      STACK: if stacks < max, stacks++, else refresh duration
      REFRESH_DURATION: reset remaining_rounds

  TickBuffs(target, round): // 每回合开始/结束
    for buff in target.buffs:
      for effect in buff.effects:
        if effect.type == DOT:
          deal_damage(target, calc_dot_damage(effect))
      buff.remaining_rounds--
      if remaining_rounds <= 0:
        remove_buff(target, buff)
```

---

### Q34: BattleService 的 worker pool 怎么设计?

**简答**: Kafka 消费 battle_request → N 个 worker goroutine/thread → 各自独立执行 SimulateBattle → 结果发回 Kafka。

```
BattleService:
  workers: N (根据CPU核心数)
  input_queue: Kafka topic "battle_request"
  output_topic: Kafka topic "battle_result"

  Worker Loop:
    for msg in consumer:
      req = deserialize(BattleRequest, msg)
      report = SimulateBattle(req.attacker, req.defender, req.seed, req.terrain)
      produce("battle_result", req.battle_id, serialize(report))
      ack(msg)

  流控:
    - back-pressure: consumer lag > threshold → 告警 → 扩容
    - timeout: 单次战斗 > 100ms → 告警 (正常 < 1ms)
```

---

### Q35: 如何处理战斗中 "挟天子以令诸侯" 这类特殊机制?

**简答**: 这是游戏设计层面的特殊 buff/效果，在战斗引擎中作为 SpecialAbility 处理。

**详细解释**: 
率土的特殊机制（如天子系统、特殊武将搭配效果）本质上都是在战斗开始前/中/后触发的特殊 buff 或条件。在引擎中设计 SpecialAbility 钩子:

```
PreBattleHooks:   // 战斗开始前
  - 检查是否有"天子"buff → 全军攻击力 +30%
InBattleHooks:     // 每回合
  - 检查特殊搭配效果 → 触发额外行动
PostBattleHooks:   // 战斗结束后
  - 检查是否有"斩将"效果 → 额外经验
```

---

### Q36: 怎么做战斗平衡性测试?

**简答**: 用确定性引擎批量跑模拟 — 遍历所有武将组合 × 等级 × 装备，统计胜率矩阵。

```
BalanceTest:
  for each attacker_comp in all_compositions:
    for each defender_comp in all_compositions:
      for seed in range(10000):
        report = SimulateBattle(attacker_comp, defender_comp, seed)
        win_rate[attacker_comp][defender_comp] += report.attacker_won
  // 输出胜率矩阵 → 找出异常值 (胜率 > 80% 的组合)
```

确定性引擎的好处: 可以离线批量跑 (百万场/分钟)，不需要服务器。

---

## 四、迷雾与视野

### Q37: 迷雾系统的核心数据结构?

**简答**: 每格子一个 `map<player_id, int16_t> observers`，count > 0 则可见。

**详细解释**: 见框架文档 §6.1。

**深挖追问**:
- "10K 在线每人视野 200 格，observer 表有多大?" → 理论 200 万条 entry，但大量重叠。实际活跃 observer 约 50~100 万条，每条 10B → ~5~10MB。

---

### Q38: 行军时视野怎么增量更新?

**简答**: 滑动窗口 — 前进方向新格子 observer++，后退方向旧格子 observer--。只处理差集。

**详细解释**: 见框架文档 §6.3。

**深挖追问**:
- "增量更新每次涉及多少格子?" → 视野半径 R=3，每步移动一格 → 新增/移除各 ~2R+1=7 格 (扇形区域)。相比全量 ~(2R+1)²=49 格，减少 ~85%。

---

### Q39: 玩家登录时怎么发送初始视野?

**简答**: 登录时只发靠近主城的核心区域（视口内），其余按需请求。

```
OnPlayerLogin(player_id):
  // 1. 发送主城附近的视野数据 (半径 ~50 格)
  core_tiles = get_visible_tiles(player_id, center=home_city, radius=50)
  send(LoginMapData{tiles: core_tiles})
  // 2. 如果有活跃行军 → 发送行军路径附近视野
  for march in player.active_marches:
    march_tiles = get_visible_tiles_along_path(player_id, march)
    send(MarchVisionData{tiles: march_tiles})
  // 3. 地形等静态数据 → 客户端本地缓存, 不重复发
```

**深挖追问**:
- "登录首包会不会很大?" → 核心区域 ~2500 格 × 32B = ~80KB，protobuf + zstd 压缩后 ~20KB，完全可接受。

---

### Q40: 联盟共享视野怎么实现?

**简答**: 盟友的所有视野来源对本玩家也 +1 observer。技术上就是在 observer 表中多一条盟友的引用。

**详细解释**:

```
方案1 (精确但内存大): 每个盟友的视野来源在我的 observer 表中独立计数
  → 50 个盟友 × 200 格子视野 = 10000 条 observer entry per player
  → 内存过大

方案2 (推荐): 联盟视野合并
  alliance_visible[alliance_id] = union of all members' visible tiles
  用 bitset(1.44M bits = 180KB per alliance) 存储
  玩家可见 = 个人视野 OR 联盟视野

  联盟视野更新:
    成员视野变化 → 更新 alliance bitset
    bitset 变化 → 通知该联盟在线成员
  
  优化: alliance bitset 按 Region 分段, 只推送变化的 Region
```

**深挖追问**:
- "联盟 100 人，每人视野变化都更新 alliance bitset?" → 是，但 bitset 操作是 O(1)。合并 bitset 变化后批量推送，不是每次变化都推。

---

### Q41: 侦察技能 (一次性揭露区域) 怎么实现?

**简答**: 在目标区域所有格子上加临时 observer (带过期时间)，过期后自动清除。

```
ScoutReveal(player_id, center_tile, radius, duration_sec):
  for tile in tiles_in_radius(center_tile, radius):
    tile.observers[player_id]++
    // 如果从 0 变 1 → 推送 TileReveal
  register_timer(duration_sec, ExpiresScout(player_id, center_tile, radius))

ExpiresScout(player_id, center_tile, radius):
  for tile in tiles_in_radius(center_tile, radius):
    tile.observers[player_id]--
    // 如果变 0 → 推送 TileFog
```

---

### Q42: 视野推送的带宽优化?

**简答**: 批量合并 (50ms window) + 增量推送 (只发变化) + 压缩 (zstd) + Region 订阅 (只推视口内)。

**详细解释**:

```
优化手段:
1. Batch Window (50ms):
   收集 50ms 内所有 tile 变化 → 合并成一个 TileUpdateBatch
   10K 在线 × 平均 5 次变化/s = 50K/s → 批量后 ~10K msg/s

2. Delta Only:
   只发 {tile_id, changed_fields} 不发完整 Tile
   比如只改了 owner → 只发 {tile_id, new_owner}

3. zstd 压缩:
   protobuf 序列化后 zstd 压缩 → 节省 50-70% 带宽

4. Region 订阅:
   客户端打开地图视口 → 订阅该视口覆盖的 Region
   拖动/缩放 → 更新订阅列表

5. 离线玩家不推:
   observer 在但 session 不在线 → 跳过推送
```

**深挖追问**:
- "50ms 的 batch window 会影响用户体验吗?" → SLG 不是动作游戏，50ms 延迟无感知。

---

### Q43: 客户端缓存策略?

**简答**: 地形数据永久缓存 (不变)；建筑/归属数据按 Region 缓存 (服务器推送更新)；行军数据实时更新 (不缓存)。

---

### Q44: 如何防止 "地图透视" 作弊?

**简答**: 服务器绝不发送玩家视野之外的数据。客户端请求视口时，服务器做 permission filter。

```
HandleViewportRequest(player_id, viewport):
  for region in regions_in_viewport(viewport):
    tiles = region.get_tiles()
    filtered = []
    for tile in tiles:
      if tile.observers[player_id] > 0:
        filtered.append(tile.to_client_data())  // 完整数据
      else:
        filtered.append(tile.to_fog_data())      // 只有地形，无归属/部队
    send(player_id, RegionData{tiles: filtered})
```

即使客户端被 hack，也看不到未揭露的格子数据——因为服务器根本不发。

---

## 五、数据层与持久化

### Q45: Redis 和 MySQL 各存什么?

**简答**: Redis 存热数据 (玩家在线数据, session, 排行榜)；MySQL 存持久化数据 (全量备份, 交易日志, 战报)。

**详细解释**: 见框架文档 §12。

---

### Q46: 数据持久化策略? write-through 还是 write-behind?

**简答**: Write-behind (Kafka → DB Service → MySQL)。关键操作立即写 WAL。

**详细解释**: 

```
正常流程 (write-behind):
  GameLogic 修改数据 → 更新内存 → 发 Kafka msg → DB Service 异步写 MySQL
  延迟: 100ms~1s

关键操作 (write-through):
  战斗结算、充值、交易 → 同步写 MySQL (或至少写 Redis + Kafka)
  延迟: 1~10ms

折中: 先写 Redis (ms 级持久化) + Kafka (异步落 MySQL)
  Redis AOF 保证秒级数据安全
  MySQL 保证最终一致性

脏标记方案的竞态问题: 见框架文档引用的 db-write-behind 分析。
推荐方案: Kafka partition worker 内 batch flush (同 goroutine 串行)。
```

**深挖追问**:
- "Kafka 还没消费完 DB Service 就挂了?" → Kafka 有持久化 + consumer commit。DB Service 重启后从上次 commit offset 继续消费。
- "Redis 挂了呢?" → AOF + Sentinel 自动故障转移。最坏情况丢 1~2 秒数据，可从 MySQL 最近快照 + Kafka binlog 恢复。

---

### Q47: 地图数据怎么持久化?

**简答**: dirty Region 标记 + 定期批量写。

```
TileManager.UpdateTile(tile_id, ...):
  tiles[tile_id] = ...
  dirty_regions.add(tile_id_to_region(tile_id))

PersistenceTick (every 30s):
  for region_id in dirty_regions:
    region_data = serialize_region(region_id)  // 1024 tiles protobuf
    write_to_redis("map_region:{region_id}", region_data)
    send_kafka("db_task", region_data)  // 异步落 MySQL
  dirty_regions.clear()

// Region 粒度写入: 1024 tiles × 128B ≈ 128KB/Region
// 全图 1444 Regions, 但通常每 30s 只有 ~100 个 dirty
// → 每 30s 写入量 ~12.8 MB, 完全可接受
```

---

### Q48: 排行榜怎么实现?

**简答**: Redis Sorted Set，score 为排名指标 (战力/击杀/领地面积)。

```
// 实时排行榜 (Redis ZINCRBY)
ZADD rank:power {power_score} {player_id}
ZREVRANGE rank:power 0 99  // Top 100

// 更新时机:
  - 战力变化 (武将/装备/建筑) → ZADD 更新
  - 击杀 → ZINCRBY
  - 领地变化 → ZADD
  
// 推送: 每 5min 推送一次 Top 100 变化

// 联盟排行榜:
ZADD rank:alliance_power {total_power} {alliance_id}
```

**深挖追问**:
- "百万玩家的排行榜 ZREVRANGE 会慢吗?" → O(log N + M)，N=100K, M=100 → <1ms。
- "需要分页查询呢?" → `ZREVRANGE rank:power offset count`。

---

### Q49: 玩家数据的读写路径?

**简答**: 
- 读: 先查 MapService 内存 (地图数据) → 再查 Redis (个人数据) → 最后查 MySQL (冷数据)
- 写: 修改内存/Redis → Kafka → MySQL

```
读路径:
  地图数据 (格子/行军) → MapService 内存 (最快, ns级)
  个人数据 (武将/背包) → Redis (快, <1ms)
  历史数据 (旧战报)   → MySQL (慢, 1-10ms)
  
写路径:
  地图数据修改 → 内存更新 + dirty flag → 定期 flush Redis → Kafka → MySQL
  个人数据修改 → Redis 更新 → Kafka → MySQL
  战斗结算    → Redis + MySQL 双写 (关键操作)
```

---

### Q50: 怎么保证同一玩家数据不被两个服务同时写?

**简答**: Single-writer-per-player 原则 — 任一时刻一个玩家只在一个 Scene/Map 服务上。

**详细解释**: 见 cross-server-architecture-principle 中的数据一致性设计。

三层防护:
1. **正常路径**: Scene Manager 串行 handoff (释放 → 确认 → 加载)
2. **异常路径**: 分布式锁 (Redis SETNX, TTL 3s)
3. **兜底**: 乐观锁 (version 字段)

---

### Q51: Kafka 消息顺序怎么保证?

**简答**: 同一 player_id 作为 partition key → 保证同 player 消息落到同一 partition → 同 partition 内 FIFO。

**深挖追问**:
- "partition 数量怎么定?" → 通常 = 最大消费者数量。比如 DB Service 有 8 个 worker → 8 个 partition。
- "如果某个 player 消息量特别大 (热点)?" → 单 partition 内仍然串行处理。如果是 KOL 级玩家导致一个 partition 过热，可以在 DB Service 层面做优先级调度。

---

### Q52: Redis 的内存容量估算?

**简答**: 单赛季 ~2~5 GB。

```
估算:
  10K 在线玩家:
    profile: 10K × 1KB = 10MB
    resources: 10K × 100B = 1MB
    generals: 10K × 5KB = 50MB (平均 10 个武将)
    buildings: 10K × 2KB = 20MB
    armies: 10K × 1KB = 10MB
    mails: 10K × 500B = 5MB
  Subtotal: ~100MB

  100K 注册玩家 (含离线):
    100K × 10KB = 1GB

  排行榜: ~50MB
  Session: ~10MB
  地图 Region 缓存: 1444 × 128KB = ~180MB

  总计: ~1.5GB, 留余量 → 分配 4GB
```

---

### Q53: MySQL 表怎么分库分表?

**简答**: 按赛季分库 (每赛季独立 DB)，大表按 player_id Hash 分表。

```
数据库划分:
  db_season_101: 赛季101的所有数据
  db_season_102: 赛季102的所有数据
  → 赛季结束 → 整库归档

分表 (player_id hash):
  players_00 ~ players_15  (16 份)
  generals_00 ~ generals_15
  
地图表不分表 (单地图): map_tiles 按 region_id partition

战报表: battle_reports 按时间 range partition (月级)
```

**深挖追问**:
- "为什么不用 NewSQL (TiDB)?" → 可以，分布式 SQL 简化分表逻辑。但运维复杂度和成本增加。SLG 单赛季数据量不大 (< 50GB)，MySQL 完全够。
- "ORM 怎么用?" → 不推荐重度 ORM。proto → MySQL mapping，用 `REPLACE INTO` 做幂等写入。

---

### Q54: 交易日志 (transaction_log) 怎么设计?

**简答**: 每次资源/物品变更记一条日志，包含 who/what/amount/reason/timestamp。用于审计、回滚、防作弊。

```sql
CREATE TABLE transaction_log (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    player_id BIGINT NOT NULL,
    tx_type TINYINT NOT NULL,        -- 1=currency, 2=item
    item_config_id INT,
    currency_type INT,
    amount BIGINT NOT NULL,
    balance_after BIGINT NOT NULL,
    reason VARCHAR(64) NOT NULL,      -- "battle_loot", "gather", "purchase"
    source_player_id BIGINT,          -- 交易对手 (如有)
    created_at DATETIME NOT NULL,
    INDEX idx_player_time (player_id, created_at)
);
```

**深挖追问**:
- "日志量会不会很大?" → 10K 在线 × 平均 10 笔/分 = 100K/分 = 6M/小时。按月分区，定期归档旧月份。

---

### Q55: 崩溃恢复时数据从哪里恢复?

**简答**: 地图数据从 Redis 最近快照 + dirty Region flush 恢复；行军从 Redis 中的行军记录恢复（懒计算公式重算位置）；玩家数据从 Redis 恢复。

**详细解释**: 见框架文档 §4.6 和 §16。

恢复顺序:
1. 加载地图 Region 数据 (Redis → Memory)
2. 加载行军记录 → 重算位置 → 重建 occupant → 重注册定时器
3. 重跑碰撞检测 (补漏恢复期间错过的)
4. 恢复在线玩家 session → 推送重连消息

---

## 六、网络与协议

### Q56: 用 TCP 还是 UDP?

**简答**: TCP。SLG 消息频率低，对延迟不如 FPS 敏感，TCP 的可靠传输更重要。

**深挖追问**:
- "WebSocket 呢?" → 可以考虑。如果有 Web/H5 客户端需求则用 WebSocket。Native 客户端直接用 TCP 更高效。

---

### Q57: 消息协议怎么设计?

**简答**: 自定义二进制头 (4B length + 2B msg_id + 2B flags) + Protobuf body。

**详细解释**: 见框架文档 §13.1。

**深挖追问**:
- "为什么不直接用 gRPC?" → gRPC 基于 HTTP/2，头部开销大，不适合高并发长连接游戏客户端。自定义协议头部只有 8B。
- "Protobuf 还是 FlatBuffers?" → Protobuf 序列化/反序列化有开销；FlatBuffers 零拷贝读取但使用更复杂。SLG 消息不频繁，Protobuf 足够。

---

### Q58: 频控 (Rate Limiting) 怎么做?

**简答**: 按 msg_id 分类，每类有独立的令牌桶。

```
RateLimiter per player:
  message_limits:
    MARCH_CREATE:  5/sec
    BUILD_START:   3/sec
    CHAT_SEND:     2/sec
    VIEWPORT_REQ:  10/sec
    default:       20/sec
  
  global_limit: 100 msg/sec (防恶意刷包)
  
  exceed_policy:
    soft (< 2x): 丢弃消息, 返回 RATE_LIMITED
    hard (> 5x): 断开连接, 记录异常
    repeat offender: 临时封号
```

---

### Q59: 登录协议流程?

**简答**: Client → Gate → LoginService (验证 token) → 分配 GameService → 加载数据 → 返回登录成功 + 初始数据包。

```
1. Client → Gate: LoginRequest{account, token, platform}
2. Gate → LoginService: VerifyToken{account, token}
3. LoginService → AuthService (Java): 验证 token 合法性
4. LoginService: 查 Redis 获取玩家数据 / 创建新角色
5. LoginService → Gate: LoginResponse{player_id, session_token, game_server_addr}
6. Client → Gate: EnterGame{session_token}
7. Gate → MapService: PlayerLogin{player_id}
8. MapService: 加载玩家地图相关数据, 注册视野 observer
9. MapService → Gate (Kafka): LoginData{tiles, marches, mail_count, ...}
10. Gate → Client: push LoginData
```

---

### Q60: 断线重连怎么处理?

**简答**: 客户端保存 session_token + 最后一条消息的 sequence_number。重连时发送这两个信息，服务器从断点恢复。

```
Reconnect flow:
1. Client → Gate: Reconnect{session_token, last_seq}
2. Gate: 验证 session_token → 恢复 player_id → session 映射
3. Gate → MapService: PlayerReconnect{player_id, last_seq}
4. MapService: 从 last_seq 之后的消息队列重发
5. 如果断线时间 < 30s → 直接恢复
6. 如果断线时间 > 30s → 视为新登录，全量同步
7. 如果断线时间 > session_TTL → 需要重新 login
```

**深挖追问**:
- "消息队列存多久?" → 每个 player 保留最近 100 条或 5 分钟内的推送消息 (ring buffer)。

---

### Q61: 大包问题怎么处理?

**简答**: 分包发送 + 优先级排序。

```
登录首包策略:
  优先级排序:
    P0: 主城位置 + 主界面数据 (先显示)
    P1: 附近地图 (50格范围)
    P2: 行军数据
    P3: 邮件/聊天
    P4: 完整视野范围

  分包:
    每包 < 16KB (避免分 TCP segment)
    按优先级依次发送
    客户端收一包显示一部分 → 渐进式加载
```

---

### Q62: 如何防止重放攻击?

**简答**: 每条消息包含 timestamp + sequence_number + HMAC。

```
消息签名:
  client_msg = MsgHeader + MsgBody + timestamp + seq_number
  hmac = HMAC-SHA256(session_key, client_msg)
  
  服务器验证:
    1. 检查 timestamp 与服务器时间差 < 30s
    2. 检查 seq_number > last_seq (单调递增)
    3. 验证 hmac

  session_key: 登录时协商 (DH 密钥交换 / 服务器下发)
```

---

### Q63: 服务器推送量太大怎么办?

**简答**: 
1. Delta push (只推变化)
2. Batch window (50ms 合并)
3. 优先级丢弃 (非关键消息可丢)
4. Region 订阅 (只推视口)
5. 离线不推
6. 压缩

极端情况 (万人大战) → 降低推送频率，非关键信息延迟推送。

---

## 七、并发与性能

### Q64: MapService 的性能瓶颈在哪?

**简答**: 1) 视野推送 (IO 密集); 2) 碰撞检测 (CPU 密集); 3) 战斗结算应用 (可能触发连锁更新)。

---

### Q65: 10 万个定时器 (行军/建造/buff) 怎么高效管理?

**简答**: Hashed Timing Wheel, O(1) insert/delete/tick。

**详细解释**: 见框架文档 §14.1。

**深挖追问**:
- "和最小堆比呢?" → 堆: insert O(log N), delete O(log N)。10 万个定时器，堆每次操作 ~17 次比较；时间轮 O(1)。
- "精度 100ms 够吗?" → SLG 完全够。最高精度需求来自行军经过格子的 vision event，100ms 偏差不可感知。

---

### Q66: 怎么做性能 profiling?

**简答**: 
- C++: perf / VTune / gprof + 自定义 scope timer
- Go: pprof (CPU + memory + goroutine)
- 关键指标: event_loop 单次迭代时间 (target < 10ms)

---

### Q67: Region 级并发的锁策略?

**简答**: 每个 Region 一个 Actor (mailbox + 串行处理)，不用锁。跨 Region 通过异步消息。

**深挖追问**:
- "跨 Region 操作需要两阶段锁?" → 不需要。用异步消息: RegionA 发 "army entering" → RegionB 收到后处理。如果需要原子性（罕见），用 Region 级临时 merge 或 coordinator pattern。

---

### Q68: C++ 内存管理注意什么?

**简答**: 
1. 地图数据用数组 (连续内存, cache-friendly)
2. 行军/定时器用 object pool (减少 malloc/free)
3. protobuf arena allocation (减少小对象 GC)
4. 用 jemalloc/tcmalloc 替代默认 allocator

---

### Q69: 如何减少 GC 压力 (Go 服务)?

**简答**: 
1. sync.Pool 复用 protobuf 对象
2. 预分配 slice/map
3. 避免大量小对象在堆上
4. Kafka 消费用 batch + 预分配 buffer
5. 热路径用 unsafe.Pointer 减少 interface{} 装箱

---

### Q70: 压测用什么工具?

**简答**: 自研机器人框架 — 一个 goroutine 模拟一个客户端，执行完整游戏流程。

```
Robot 设计:
  - 每个 robot = 1 goroutine + 1 TCP connection
  - 模拟行为: 登录 → 建造 → 征兵 → 行军 → 战斗 → 采集
  - 可配置: 行为权重, 操作间隔, 并发数
  - 监控: 成功率, 延迟分布, 吞吐量

压测指标:
  - P50/P95/P99 消息延迟
  - 战斗 QPS
  - 视野推送 throughput
  - MapService 内存/CPU
```

---

### Q71: 怎么做热更新 (不停服更新)?

**简答**: 
- 配表热更: 重新加载 protobuf 配表到内存 (不重启)
- 逻辑热更: 重启 + 快速恢复 (< 2 分钟停服)
- 无状态服务: 滚动更新 (K8s rolling update)
- MapService: 主备切换 (备机用新代码, 主机 drain → 备机提升)

---

### Q72: CPU 绑核有必要吗?

**简答**: 有帮助。MapService 主线程绑定单核减少 context switch; Region worker 线程绑定各自核心。

---

### Q73: 网络 IO 用什么模型?

**简答**: Linux: epoll (event-driven, non-blocking IO); Windows: IOCP。封装为 EventLoop 抽象。

SLG 的网络 IO 不是瓶颈 (消息频率低)。瓶颈在逻辑计算。

---

## 八、赛季与运维

### Q74: 赛季开始时的 "开服洪峰" 怎么处理?

**简答**: 排队机制 + 预热 + 限流。

```
开服策略:
1. 排队: 超过 N 人同时登录 → 排队等待
2. 预热: 提前 10 分钟启动所有服务 + 加载地图
3. 限流: 每秒放行 M 个玩家登录
4. 防重复请求: 登录请求幂等 (token 去重)
5. 降级: 非核心功能暂时关闭 (聊天/排行榜)
```

---

### Q75: 赛季结束数据归档怎么做?

**简答**: 见框架文档 §10.2。冻结 → 结算 → 归档 (热/冷分离) → 释放资源。

---

### Q76: K8s 部署 SLG 有什么特殊考虑?

**简答**: MapService 用 StatefulSet (有状态, 稳定网络标识); BattleService 用 Deployment + HPA (无状态, 自动扩缩)。

```
特殊考虑:
  - MapService: StatefulSet, replicas=1, persistent volume
    不能用 Deployment (随机 pod 名)
    需要固定标识 (etcd 注册用)
    PV 存日志和临时快照
  - Gate: Deployment, 需要稳定外部 IP (LoadBalancer / NodePort)
  - Kafka: 独立命名空间, StatefulSet
  - 每个赛季一个 namespace: mmorpg-zone-{season_name}
```

---

### Q77: MapService 宕机了怎么恢复?

**简答**: 快速重启 → 从 Redis 加载地图/行军数据 → 重算行军位置 → 重建定时器/碰撞。目标 < 2 分钟。

**详细解释**: 见框架文档 §4.6。

核心优势: 懒计算模型意味着不需要恢复中间状态 — 公式给出任意时刻的正确位置。

**深挖追问**:
- "2 分钟停服可以接受吗?" → SLG 对停服容忍度远高于 MMORPG (行军还在继续, 只是看不到)。公告 + 快速恢复即可。热备方案可以做到 < 30 秒。

---

### Q78: 如何做灰度发布?

**简答**: 按赛季灰度 — 先更新低优先级赛季，观察无异常后推广到全部。

```
灰度策略:
  Phase 1: 更新测试服 (内部测试)
  Phase 2: 更新 1~2 个低活跃赛季 (灰度观察 24h)
  Phase 3: 更新所有赛季 (滚动)
  
  无状态服务 (Battle, Chat): K8s canary deployment (新旧版本共存, 按比例分流)
  有状态服务 (MapService): 主备切换 (新代码启动备机 → 验证 → 切主)
```

---

### Q79: 日志系统怎么设计?

**简答**: 结构化日志 (JSON/logfmt) + 集中收集 (ELK/Loki) + 分级 (debug/info/warn/error)。

关键日志:
- 每次战斗: battle_id, participants, result
- 每次行军: army_id, path, start_time
- 每次资源变更: transaction_log
- 异常: 碰撞检测异常、数据不一致

---

### Q80: 监控告警设置?

**简答**: 见框架文档 §16.4。核心指标 + Prometheus + Grafana + AlertManager。

关键告警:
- MapService event_loop > 50ms → P1
- BattleService queue_depth > 1000 → P2
- Kafka consumer lag > 10000 → P1
- Redis memory > 80% → P2
- MySQL replication lag > 10s → P1

---

### Q81: 维护窗口怎么操作?

**简答**: 
1. 公告 (提前 30min)
2. 关闭新登录
3. 倒计时踢人
4. 停止 MapService + flush 所有数据
5. 执行维护操作 (更新/修复/迁移)
6. 验证
7. 开放登录

---

### Q82: 赛季间的停服时间多久?

**简答**: 目标 < 30 分钟。主要时间花在数据归档和新赛季初始化。

---

## 九、安全与反作弊

### Q83~Q90: 见框架文档 §15 + exploit-loss-prevention-systems。

### Q83: SLG 最常见的作弊手段?

**简答**: 多开/脚本 > 地图透视 > 加速 > 协议篡改。

---

### Q84: 怎么检测脚本/外挂?

**简答**: 
- 操作频率分析 (过于规律 = 脚本)
- 操作时间分布 (人类操作间隔有随机性, 脚本趋于固定)
- 行为序列分析 (脚本总是同一套动作)
- 设备指纹 (同设备多账号)
- 举报机制

---

### Q85: 怎么处理利用 bug 获取的非法资源?

**简答**: 三级响应:
1. 封堵 (BlockGlobal/BlockItem) → 立即止血
2. 追溯 (QueryTransactionLog) → 查明影响范围
3. 回收 (BatchRecallItems) → 扣除非法获取

详见 exploit-loss-prevention-systems。

---

### Q86: DDoS 防护?

**简答**: CDN/WAF 前置 + SYN Cookie + 连接数限制 + 云厂商 DDoS 防护服务。

---

### Q87: 加密通信?

**简答**: TLS 1.3 (传输层加密)。可选: 应用层 AES-GCM 二次加密 (增加破解难度)。

---

### Q88: 如何防止内部 GM 滥权?

**简答**: GM 操作审计日志 + 权限分级 + 高危操作双人审核 + 操作回放。

---

### Q89: 充值相关的安全措施?

**简答**: 
- 服务端验证所有支付渠道的回调签名
- 幂等处理 (同一 order_id 不重复发货)
- 对账系统 (定期比对渠道流水 vs 发货记录)
- 金额异常检测 (突然大额 → 人工审核)

---

### Q90: 数据泄露防护?

**简答**: 
- DB 访问权限最小化
- 敏感数据 (密码) bcrypt 哈希
- 个人信息 AES 加密存储
- 员工不能直接访问生产 DB (通过审批工具)
- 定期安全审计

---

## 十、联盟与社交

### Q91: 联盟系统的技术难点?

**简答**: 
1. 联盟领地连通性检测 (BFS/Union-Find)
2. 集结攻城的多人协调
3. 联盟聊天的扇出推送
4. 联盟排名的实时计算

---

### Q92: 联盟领地连通性检测怎么做?

**简答**: Union-Find (并查集)，每次领地变化时增量更新。

```
OnTileOwnerChanged(tile_id, new_alliance_id):
  if new_alliance_id == this_alliance:
    // 新增领地 → 检查四周是否有同联盟格子 → union
    for neighbor in tile_neighbors(tile_id):
      if tile_owner(neighbor) == this_alliance:
        union_find.merge(tile_id, neighbor)
  else:
    // 失去领地 → 需要重建受影响区域的连通性
    // 重建范围: 只重建 tile_id 所在 Region 的同联盟格子
    rebuild_uf_for_region(tile_id, old_alliance_id)
```

**深挖追问**:
- "删除格子后 Union-Find 怎么处理?" → Union-Find 不支持高效删除。采用: 局部重建 (只重建受影响的连通分量)。

---

### Q93: 集结攻城的倒计时中如果发起者掉线?

**简答**: 集结一旦发起就是服务器状态，不依赖发起者在线。倒计时到 → 自动出发。发起者离线不影响已参与的盟友。

---

### Q94: 联盟聊天 1000 人在线怎么推送?

**简答**: 分批 push — 1000 人 × 1 条消息 = 1000 次 push。用 Kafka 扇出到各 Gate。

```
ChatMessage → ChatService:
  1. 存储消息 (Redis list, 保留最近 500 条)
  2. 查询联盟在线成员列表
  3. 按 gate_node_id 分组
  4. 每组发一条 Kafka 消息到对应 gate topic
  5. Gate 消费后推送给各自管理的连接

优化: 如果 1000 人都在同一个 Gate → 一条 Kafka 消息搞定
```

---

### Q95: 外交关系 (同盟/敌对) 存在哪?

**简答**: Redis Hash — `alliance:{id}:diplomacy` → `{target_alliance_id: relation_type}`

变化不频繁 (几天一次)，可缓存在 MapService 内存中。

---

### Q96: 联盟科技怎么做?

**简答**: 联盟成员捐献资源 → 科技经验 → 升级 → 全联盟 buff。

```
AllianceTech:
  tech_id: 201 (攻击力研究)
  level: 3
  current_exp: 5000 / 10000
  effect: alliance_atk_bonus = level * 2%  // 全联盟攻击力 +6%

  捐献: member donates 1000 wood → tech.current_exp += 1000
  升级: current_exp >= required_exp → level++ → 广播全盟
```

---

## 十一、武将与战斗数值

### Q97: 数值框架怎么设计?

**简答**: 基础属性 + 等级成长 + 品质系数 + 装备加成 + buff 加成 = 最终属性。

```
final_atk = (base_atk + level * growth_rate) * quality_coeff
          + equipment_atk
          + alliance_tech_bonus
          + buff_bonus

所有加成按 "加法类" 和 "乘法类" 分开:
  加法类: 装备固定值, buff 固定值 → 先加
  乘法类: 质量系数, 百分比 buff → 后乘
  → 避免乘法连乘导致数值爆炸
```

---

### Q98: 武将品质/稀有度怎么影响战力?

**简答**: 高品质 = 更高的基础属性 + 更强的专属技能 + 更高的成长率。

```
品质系数: 白=1.0, 绿=1.2, 蓝=1.5, 紫=2.0, 橙=2.5, 红=3.0
SSR 武将和 R 武将即使同等级, 最终属性差 3 倍
但 R 武将更容易满星 (碎片获取容易) → 缩小差距
```

---

### Q99: Gacha (抽卡) 系统怎么做?

**简答**: 概率表 + 保底机制。

```
GachaPull(player_id, banner_id):
  pity_count = player.pity_counters[banner_id]
  
  if pity_count >= PITY_THRESHOLD:  // 保底
    result = guaranteed_ssr()
    player.pity_counters[banner_id] = 0
  else:
    roll = prng.range(10000)  // 万分比
    if roll < ssr_rate:       // 1.6%
      result = random_ssr(banner)
      player.pity_counters[banner_id] = 0
    elif roll < ssr_rate + sr_rate:  // 13%
      result = random_sr(banner)
      pity_count++
    else:
      result = random_r(banner)
      pity_count++
  
  // 记录交易日志（审计 + 概率验证）
  log_gacha(player_id, result, roll)
  return result

// 法律合规: 公示概率, 保底机制, 未成年人限制
```

**深挖追问**:
- "概率在服务端还是客户端?" → 必须在服务端。客户端只发 "我要抽卡" 请求。

---

### Q100: 兵种克制系统怎么实现?

**简答**: 配表驱动 — 克制矩阵 `type_advantage[attacker_type][defender_type] = bonus`。

```
const int32_t TYPE_ADVANTAGE[3][3] = {
  //       枪  骑  弓
  /* 枪 */ {0, 20, -20},
  /* 骑 */ {-20, 0, 20},
  /* 弓 */ {20, -20, 0}
};
// 正数 = 伤害加成 %, 负数 = 伤害减免 %
```

---

### Q101: 战法 (技能搭配) 系统怎么做?

**简答**: 每个武将有若干技能槽，玩家自由搭配技能卡。技能之间有协同效果 (配表定义)。

---

### Q102: 武将经验和等级上限?

**简答**: 等级上限 = 主城等级 × 系数。经验来源: 战斗、任务、道具。经验变更服务端计算，防作弊。

---

### Q103: 战力计算公式?

**简答**: `power = f(atk, def, hp, spd, skills) × level_coeff × star_coeff`

```
用于排行榜和匹配的参考值，不参与实际战斗计算。
公式需要定期调平 (alpha/beta test 后根据实际战斗数据调整权重)。
```

---

## 十二、工程能力

### Q104: 你怎么做代码规范?

**简答**: 
- C++: Google Style + clang-tidy + 项目 .clang-tidy 配置
- Go: gofmt + golangci-lint
- Proto: 统一 naming (snake_case message, PascalCase service)
- Code Review: PR required, 至少 1 人 approve

---

### Q105: 怎么做单元测试?

**简答**: 
- 战斗引擎: 输入固定 seed → 验证输出 (确定性!)
- 碰撞检测: 构造已知碰撞/不碰撞案例
- 视野系统: 添加 observer → 验证 count
- 寻路: 已知地图 → 验证路径最优性

```
// 战斗测试示例
TEST(BattleTest, DeterministicOutput) {
    auto input = MakeTestInput(seed=12345);
    auto report1 = SimulateBattle(input);
    auto report2 = SimulateBattle(input);
    EXPECT_EQ(report1, report2);  // 确定性保证
}
```

---

### Q106: CI/CD 流水线?

**简答**: 
```
PR → lint + unit test → build → integration test → merge
Release → build images → staging deploy → smoke test → production deploy
```

---

### Q107: 怎么做配表管理?

**简答**: Excel → 导表工具 → Protobuf binary → 服务端加载。版本控制在 Git。

```
流程:
  策划编辑 Excel → 导表工具 (protogen) → 生成 .pb 文件 + 校验
  校验: 引用完整性 (技能引用的 buff_id 存在)、数值范围、重复 ID 检查
  热更: GM 命令 → 重新加载配表 → 不重启
```

---

### Q108: 怎么做性能回归测试?

**简答**: 每次 release 跑标准 benchmark → 对比历史数据 → 性能下降 > 10% → 阻塞发布。

---

### Q109: 团队分工怎么安排?

**简答**: 
```
服务端主程序 (1): 架构设计, 核心模块 (Map/March/Vision)
战斗程序 (1-2): 战斗引擎, 技能系统, 数值框架
后端程序 (2-3): 联盟/社交/任务/活动/商城
基础架构 (1): 网络层, 数据层, 部署, 监控
客户端 (3-5): ...
策划 (2-3): 配表, 数值, 玩法设计
QA (2-3): 测试, 压测
```

---

### Q110: 怎么做 Code Review?

**简答**: 
- 每次 PR 必须 review
- Review checklist: 正确性 > 可读性 > 性能 > 风格
- 特别关注: 数据一致性、并发安全、确定性约束
- 战斗引擎修改: 必须跑确定性回归测试

---

### Q111: 如何设计可扩展的游戏系统?

**简答**: **数据驱动 + 配表 + 事件系统**。新增玩法 = 新增配表 + 注册新的事件处理器，不修改核心代码。

```
示例: 新增 "天气系统" 影响行军速度
  1. 配表: weather_config.xlsx (天气类型, 影响范围, 速度系数)
  2. SchedulerService: 按时间表切换天气
  3. MapService: weather_changed event → 
     受影响区域的行军 apply speed modifier → 重注册
  4. 客户端: 天气动画
  
核心系统 (March/Vision/Battle) 完全不改。
```

---

### Q112: SLG 项目的技术风险?

**简答**: 
1. MapService 单点故障 (最大风险)
2. 大规模攻城性能问题
3. 赛季末期数据膨胀
4. 配表复杂度导致 bug
5. 确定性战斗跨平台一致性

---

### Q113: 你做的最复杂的技术系统是什么?

**模板答案**: (结合自身经历回答, 以下为示例)

"行军碰撞检测系统。挑战在于:
1. 没有逐帧 tick，需要在注册时预计算所有碰撞
2. 召回/加速会导致级联更新
3. 需要保证高并发下的一致性
4. 测试难度高 (时间相关的边界条件多)

我的方案: 时空碰撞检测 + 每次变更完全重建状态 (不增量修补)。
性能: 单次注册 < 0.5ms, 支持 50K 并发行军。"

---

### Q114: 遇到过最难调的 bug 是什么?

**模板答案**: (示例)

"战斗确定性失效 — 同一输入偶尔产生不同结果。
排查过程:
1. 首先怀疑浮点 → 确认全部是整数运算
2. 然后怀疑 PRNG → 换 seed 还是不一致
3. 最终发现: unordered_set 的迭代顺序在不同运行中不同
4. 修复: 改用 sorted vector，固定迭代顺序
教训: 确定性系统必须审查所有容器的迭代顺序。"

---

### Q115: 你认为 SLG 服务端最重要的设计原则是什么?

**简答**: **事件驱动 + 懒计算 + 单一权威 + 数据驱动配表**

1. 事件驱动: 不做无用计算
2. 懒计算: 任何时刻都能从公式算出正确状态
3. 单一权威: 服务器是唯一真相来源
4. 数据驱动: 新玩法不改核心代码

---

## 十三、系统设计开放题

### Q116: 如果让你从头设计率土之滨的服务器，你第一步做什么?

**简答**: 
1. 定义 proto 协议 (所有消息的通信契约)
2. 实现最小可行 MapService (地图加载 + 单行军 + 视野)
3. 接 Gate + 简单客户端验证
4. 加入战斗系统
5. 逐步扩展 (联盟/建造/采集/赛季)

原则: 核心系统先行 (Map → March → Vision → Battle)，辅助系统后加。

---

### Q117: 如果在线人数从 1 万要支撑到 10 万怎么做?

**简答**: 不可能在一张地图上。10 万人 = 10 个赛季服 × 1 万人。每个赛季服独立 MapService。

如果是跨服场景: 10 万人观战 → 用推流 (类似直播)，不是每个人都有实时交互。

---

### Q118: 如果要加入 "天下大势图" (全局地图概览，数千人同时看)，怎么做?

**简答**: 
- 低频更新的静态快照 (每分钟生成一次全局地图缩略图)
- 客户端加载缩略图，不实时同步
- 只有详细视图才走实时推送

---

### Q119: 如果要做国际版 (全球多区域部署)，架构怎么改?

**简答**: 
- 每个大区独立部署 (NA/EU/Asia)
- 大区内多赛季服复用基础设施
- 跨大区不互通 (延迟太高)
- 跨大区赛季冠军赛: 用专门的短期服务器，快照数据过去

---

### Q120: 如果面试官说 "我们用的是 Erlang/Elixir + BEAM 虚拟机做 SLG"，你怎么看?

**简答**: 合理选择。BEAM 的 Actor 模型天然适合 Region 级并发，每个 Region 是一个 Erlang process。容错性极好 ("let it crash")。缺点: CPU 密集计算 (战斗模拟) 性能不如 C++。可以用 NIF 或外部 BattleService 补足。

---

## 面试准备 Checklist

- [ ] 能画出完整架构图 (Gate, MapService, BattleService, DataLayer)
- [ ] 能推导行军懒计算公式 + 碰撞检测算法
- [ ] 能设计 BattleReport protobuf 结构
- [ ] 能回答 Region Actor 并发模型
- [ ] 能估算内存/CPU/带宽
- [ ] 能解释 Event-Log vs Input-Resimulation 取舍
- [ ] 能描述崩溃恢复流程
- [ ] 能设计赛季生命周期管理
- [ ] 能回答反作弊措施
- [ ] 能讲清 SLG AOI 和 MMORPG AOI 的区别
- [ ] 能写出确定性战斗引擎的伪代码
- [ ] 能解释 Hashed Timing Wheel vs 最小堆
- [ ] 能描述一个完整的 debug 案例 (如确定性失效排查)
- [ ] 能回答跨服/合服的技术方案
- [ ] 能估算 Redis/MySQL/Kafka 的容量需求
